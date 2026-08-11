// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Noscan large-object allocation: one object per span, immediate release.
//
// Each object lives on its own user-arena-style span. The memory is NOT
// scanned by the garbage collector (noscan span class): it must never
// contain Go pointers. Freeing returns the span through the user-arena
// fault/quarantine machinery (freeUserArenaChunk): pages are sysFault'ed
// as soon as the GC phase permits, giving immediate physical release,
// and the address space is recycled for same-size spans via the ready
// list.
//
// Intended for objects larger than 1KB where per-object page-granularity
// overhead is acceptable.

// Deferred-fault span cache: freed spans are kept mapped (skipping
// sysFault/sysMap/memclr on the next same-size alloc) and only really
// released at the start of the next GC cycle.
//
// Correctness: a cached span is still mSpanInUse and unreferenced by the
// application, so the sweeper would want to recycle it — but the cache
// holds the span's base pointer in a scanned Go container, so the GC
// marks it every cycle and the sweeper preserves it (nalloc > 0), exactly
// like userArena's refs slice. mpSpanCacheFlush drains the cache from
// gcStart, before any sweep of the new cycle can observe such a span.
const mpSpanCacheCap = 32

type mpCachedSpan struct {
	s *mspan
	x unsafe.Pointer
}

var mpSpanCache struct {
	lock mutex
	m    map[uintptr][]mpCachedSpan // key: npages
	n    int
}

func mpSpanCacheGet(npages uintptr) (s *mspan, x unsafe.Pointer) {
	lockWithRank(&mpSpanCache.lock, lockRankMpArena)
	if lst := mpSpanCache.m[npages]; len(lst) > 0 {
		c := lst[len(lst)-1]
		lst[len(lst)-1] = mpCachedSpan{}
		mpSpanCache.m[npages] = lst[:len(lst)-1]
		mpSpanCache.n--
		s, x = c.s, c.x
	}
	unlock(&mpSpanCache.lock)
	return s, x
}

func mpSpanCachePut(s *mspan, x unsafe.Pointer) bool {
	lockWithRank(&mpSpanCache.lock, lockRankMpArena)
	defer unlock(&mpSpanCache.lock)
	if mpSpanCache.n >= mpSpanCacheCap {
		return false
	}
	if mpSpanCache.m == nil {
		mpSpanCache.m = make(map[uintptr][]mpCachedSpan)
	}
	mpSpanCache.m[s.npages] = append(mpSpanCache.m[s.npages], mpCachedSpan{s, x})
	mpSpanCache.n++
	return true
}

// mpSpanCacheFlush drains the deferred-fault cache, really releasing
// every cached span. Called from gcStart with the world stopped.
func mpSpanCacheFlush() {
	lockWithRank(&mpSpanCache.lock, lockRankMpArena)
	m := mpSpanCache.m
	mpSpanCache.m = nil
	mpSpanCache.n = 0
	unlock(&mpSpanCache.lock)
	// freeUserArenaChunk takes userArenaState/mheap locks; do it after
	// releasing the cache lock to keep lock nesting simple.
	for _, lst := range m {
		for _, c := range lst {
			freeUserArenaChunk(c.s, c.x)
		}
	}
}

// mpAllocLargeNoscan allocates a single object of size bytes on its own
// noscan span. The memory is zeroed and is NOT scanned by the GC:
// storing Go pointers in it is forbidden (pointed-to objects may be
// collected).
func mpAllocLargeNoscan(size uintptr) unsafe.Pointer {
	if size == 0 {
		size = 1
	}
	if size > maxAlloc {
		panic(plainError("runtime: allocation size out of range"))
	}
	// No bitmap reserve: the whole span is usable.
	npages := alignUp(size, pageSize) / pageSize
	spanBytes := npages * pageSize

	if gcphase == _GCmarktermination {
		throw("mpAllocLargeNoscan called with gcphase == _GCmarktermination")
	}
	if gcBlackenEnabled != 0 {
		deductAssistCredit(spanBytes)
	}

	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	// Fast path: reuse a deferred-fault span; no page operations.
	span, x := mpSpanCacheGet(npages)
	fresh := span == nil
	if fresh {
		systemstack(func() {
			span = mheap_.allocUserArenaSpan(npages)
		})
		if span == nil {
			throw("out of memory")
		}
		x = unsafe.Pointer(span.base())
	} else {
		memclrNoHeapPointers(x, span.elemsize)
	}

	if gcphase != _GCoff {
		gcmarknewobject(span, span.base())
	}

	if raceenabled {
		racemalloc(x, span.elemsize)
	}
	if msanenabled {
		msanmalloc(x, span.elemsize)
	}
	if asanenabled {
		rzStart := span.base() + span.elemsize
		asanpoison(unsafe.Pointer(rzStart), span.limit-rzStart)
		asanunpoison(x, span.elemsize)
	}

	if rate := MemProfileRate; rate > 0 && fresh {
		// Cached spans keep their original profile bucket; profiling
		// again would collide. (Cache-hit allocations are invisible to
		// the memory profiler, a documented approximation.)
		c := getMCache(mp)
		if c == nil {
			throw("mpAllocLargeNoscan called without a P or outside bootstrapping")
		}
		if rate != 1 && int64(spanBytes) < c.nextSample {
			c.nextSample -= int64(spanBytes)
		} else {
			profilealloc(mp, x, spanBytes)
		}
	}
	mp.mallocing = 0
	releasem(mp)

	if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
		gcStart(t)
	}

	return x
}

// mpFreeLargeNoscan frees an object allocated by mpAllocLargeNoscan.
// The span goes to the deferred-fault cache when it has room (no page
// operations; released at the next gcStart), otherwise pages are faulted
// as soon as the GC phase permits.
func mpFreeLargeNoscan(x unsafe.Pointer) {
	if x == nil {
		return
	}
	s := spanOf(uintptr(x))
	if s == nil || !s.isUserArenaChunk {
		throw("mpFreeLargeNoscan: pointer not in an arena span")
	}
	if mpSpanCachePut(s, x) {
		return
	}
	freeUserArenaChunk(s, x)
}

// allocUserArenaSpan allocates a user-arena-style noscan span of exactly
// npages pages, reusing a same-size span from the ready list when
// possible. Generalized from allocUserArenaChunk (fixed size, scan).
//
// Must be in a non-preemptible state to ensure the consistency of
// statistics. Acquires the heap lock; must run on the system stack.
//
//go:systemstack
func (h *mheap) allocUserArenaSpan(npages uintptr) *mspan {
	spanBytes := npages * pageSize
	var s *mspan
	var base uintptr

	lock(&h.lock)
	// Reuse an exact-size faulted span if there is one.
	for c := h.userArena.readyList.first; c != nil; c = c.next {
		if c.npages == npages {
			h.userArena.readyList.remove(c)
			s = c
			base = c.base()
			break
		}
	}
	if base == 0 {
		hintList := &h.userArena.arenaHints
		if raceenabled {
			hintList = &h.arenaHints
		}
		v, size := h.sysAlloc(spanBytes, hintList, false)
		base = uintptr(v)
		if base == 0 {
			unlock(&h.lock)
			return nil
		}
		// sysAlloc may return more than asked for (alignment). Put the tail
		// on the ready list as its own span rather than wasting it.
		if size > spanBytes {
			extra := h.allocMSpanLocked()
			extra.init(base+spanBytes, (size-spanBytes)/pageSize)
			h.userArena.readyList.insertBack(extra)
			size = spanBytes
		}
		s = h.allocMSpanLocked()
	}
	unlock(&h.lock)

	// Reused spans are faulted (Reserved) and fresh spans are Reserved;
	// transition to Prepared and then Ready.
	sysMap(unsafe.Pointer(base), spanBytes, &gcController.heapReleased)
	sysUsed(unsafe.Pointer(base), spanBytes, spanBytes)

	// Model the span as a heap span for a large noscan object.
	spc := makeSpanClass(0, true)
	h.initSpan(s, spanAllocHeap, spc, base, npages)
	s.isUserArenaChunk = true
	s.freeindex = 1
	s.allocCount = 1
	s.limit = s.base() + s.elemsize

	if asanenabled {
		s.elemsize -= redZoneSize(s.elemsize)
	}

	// Account for this span's memory.
	gcController.heapInUse.add(int64(spanBytes))
	gcController.heapReleased.add(-int64(spanBytes))

	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.inHeap, int64(spanBytes))
	atomic.Xaddint64(&stats.committed, int64(spanBytes))
	atomic.Xadd64(&stats.largeAlloc, int64(s.elemsize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	gcController.totalAlloc.Add(int64(s.elemsize))
	gcController.update(int64(s.elemsize), 0)

	// Clear the span; fresh and faulted-reused memory are both zero-filled
	// by the kernel, but this is also the critical THP signal on Linux.
	memclrNoHeapPointers(unsafe.Pointer(s.base()), s.elemsize)

	s.needzero = 0

	s.freeIndexForScan = 1

	// Set up the range for allocation.
	s.userArenaChunkFree = makeAddrRange(base, base+s.elemsize)

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	h.central[spc].mcentral.fullSwept(h.sweepgen).push(s)

	// Noscan: no bitmap reserve and no dummy type (they live in the tail
	// reserve). Clear any stale pointer recycled from a previous scan span.
	*(*uintptr)(unsafe.Pointer(&s.largeType)) = 0

	return s
}
