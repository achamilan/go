// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

// Typed large-object allocation with immediate release ("one object, one span").
//
// Each object lives on its own user-arena-style span with a real GC bitmap
// (written from typ at allocation time), so the object may contain Go
// pointers — the GC scans it like any other heap object. Freeing returns
// the span through the user-arena fault/quarantine machinery
// (freeUserArenaChunk): the pages are sysFault'ed as soon as the GC
// permits, giving immediate physical release, and the address space is
// recycled for same-size spans via the ready list.
//
// Unlike mpool (mpool.go), which is noscan bytes only, this path trades
// raw speed for pointer safety and prompt memory return.

// userArenaSpanReserveBytes is the amount of tail space reserved in a
// variable-size user arena span for the pointer/scalar bitmap and the
// dummy _type describing it. Mirrors userArenaChunkReserveBytes.
func userArenaSpanReserveBytes(spanBytes uintptr) uintptr {
	return spanBytes/goarch.PtrSize/8 + unsafe.Sizeof(_type{})
}

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
	m    map[uintptr][]mpCachedSpan // key: npages<<1 | noscan
	n    int
}

func mpSpanCacheGet(npages uintptr, noscan bool) (s *mspan, x unsafe.Pointer) {
	key := npages<<1 | uintptr(b2i(noscan))
	lockWithRank(&mpSpanCache.lock, lockRankMpArena)
	if lst := mpSpanCache.m[key]; len(lst) > 0 {
		c := lst[len(lst)-1]
		lst[len(lst)-1] = mpCachedSpan{}
		mpSpanCache.m[key] = lst[:len(lst)-1]
		mpSpanCache.n--
		s, x = c.s, c.x
	}
	unlock(&mpSpanCache.lock)
	return s, x
}

func mpSpanCachePut(s *mspan, x unsafe.Pointer) bool {
	key := s.npages<<1 | uintptr(b2i(s.spanclass.noscan()))
	lockWithRank(&mpSpanCache.lock, lockRankMpArena)
	defer unlock(&mpSpanCache.lock)
	if mpSpanCache.n >= mpSpanCacheCap {
		return false
	}
	if mpSpanCache.m == nil {
		mpSpanCache.m = make(map[uintptr][]mpCachedSpan)
	}
	mpSpanCache.m[key] = append(mpSpanCache.m[key], mpCachedSpan{s, x})
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

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mpSpanCacheReinit prepares a cached span for reuse by a new allocation:
// clears the bitmap (scan spans) and the object area. No page operations
// and no heap-size accounting: the span never left the heap while cached.
// GC marking is handled by the caller's common path.
func mpSpanCacheReinit(s *mspan, x unsafe.Pointer, noscan bool) {
	if !noscan {
		s.initHeapBits()
	}
	memclrNoHeapPointers(x, s.elemsize)
}

// mpTypedAllocLarge allocates one object of type typ with size bytes on
// its own span. size must be at least typ.Size_. The object is zeroed.
func mpTypedAllocLarge(typ *_type, size uintptr) unsafe.Pointer {
	if typ == nil {
		throw("mpTypedAllocLarge: nil type")
	}
	if size < typ.Size_ {
		throw("mpTypedAllocLarge: size smaller than type")
	}
	if size > maxAlloc {
		panic(plainError("runtime: allocation size out of range"))
	}
	dataSize := alignUp(size, goarch.PtrSize)
	// The span tail reserves 1 bit per pointer slot (computed on the whole
	// span, see userArenaSpanReserveBytes) plus a dummy _type. Solve
	// spanBytes - spanBytes/64 - sizeof(_type) >= dataSize by allocating
	// need*64/63 bytes for the data area.
	need := dataSize + unsafe.Sizeof(_type{})
	npages := alignUp(need+need/63+1, pageSize) / pageSize
	spanBytes := npages * pageSize

	if gcphase == _GCmarktermination {
		throw("mpTypedAllocLarge called with gcphase == _GCmarktermination")
	}

	// The span counts toward heapLive, so assist the GC proportionally.
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
	span, x := mpSpanCacheGet(npages, false)
	fresh := span == nil
	if fresh {
		systemstack(func() {
			span = mheap_.allocUserArenaSpan(npages, false)
		})
		if span == nil {
			throw("out of memory")
		}
		x = unsafe.Pointer(span.base())
	} else {
		mpSpanCacheReinit(span, x, false)
	}

	// Allocate black during GC. The object is all zeroed, so no scanning
	// is needed yet. This may race with GC marking, so do it atomically.
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

	// Write the GC bitmap for the object. After this point the GC scans
	// the object according to typ.
	userArenaHeapBitsSetType(typ, x, span)

	if rate := MemProfileRate; rate > 0 && fresh {
		// Cached spans keep their original profile bucket; profiling
		// again would collide. (Cache-hit allocations are invisible to
		// the memory profiler, a documented approximation.)
		c := getMCache(mp)
		if c == nil {
			throw("mpTypedAllocLarge called without a P or outside bootstrapping")
		}
		if rate != 1 && int64(spanBytes) < c.nextSample {
			c.nextSample -= int64(spanBytes)
		} else {
			profilealloc(mp, x, spanBytes)
		}
	}
	mp.mallocing = 0
	releasem(mp)

	// The span counts toward heapLive; potentially trigger a GC.
	if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
		gcStart(t)
	}

	return x
}

// mpTypedFreeLarge frees an object allocated by mpTypedAllocLarge. The
// span goes to the deferred-fault cache when it has room (no page
// operations; released at the next gcStart), otherwise it is returned to
// the runtime immediately: pages are faulted as soon as the GC phase
// permits and the address space becomes reusable.
func mpTypedFreeLarge(x unsafe.Pointer) {
	if x == nil {
		return
	}
	s := spanOf(uintptr(x))
	if s == nil || !s.isUserArenaChunk {
		throw("mpTypedFreeLarge: pointer not in a typed arena span")
	}
	if mpSpanCachePut(s, x) {
		return
	}
	freeUserArenaChunk(s, x)
}

// allocUserArenaSpan allocates a user-arena-style span of exactly npages
// pages, reusing a same-size span from the ready list when possible.
// Generalized from allocUserArenaChunk (which is fixed-size).
//
// If noscan is false, the span tail reserves room for the GC bitmap and
// the dummy type (caller must write the bitmap before publishing the
// object). If noscan is true, no space is reserved and the whole span is
// usable — the contents must not contain Go pointers.
//
// Must be in a non-preemptible state to ensure the consistency of
// statistics. Acquires the heap lock; must run on the system stack.
//
//go:systemstack
func (h *mheap) allocUserArenaSpan(npages uintptr, noscan bool) *mspan {
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
		v, size := h.sysAlloc(spanBytes, hintList, &mheap_.userArenaArenas)
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
	sysMap(unsafe.Pointer(base), spanBytes, &gcController.heapReleased, "user arena span")
	sysUsed(unsafe.Pointer(base), spanBytes, spanBytes)

	// Model the span as a heap span for a large object.
	spc := makeSpanClass(0, noscan)
	h.initSpan(s, spanAllocHeap, spc, base, npages, spanBytes)
	s.isUserArenaChunk = true
	if !noscan {
		s.elemsize -= userArenaSpanReserveBytes(spanBytes)
	}
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

	if !noscan {
		// Clear the heap bitmap so it's safe to allocate noscan data
		// without writing anything out. (For noscan spans the bitmap
		// area is zero-sized and this would write past the span.)
		s.initHeapBits()
	}

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

	// Set up an allocation header. Avoid write barriers here because this type
	// is not a real type, and it exists in an invalid location. The dummy type
	// lives in the span tail reserve, so it only exists for scan spans.
	if !noscan {
		*(*uintptr)(unsafe.Pointer(&s.largeType)) = uintptr(unsafe.Pointer(s.limit))
		*(*uintptr)(unsafe.Pointer(&s.largeType.GCData)) = s.limit + unsafe.Sizeof(_type{})
		s.largeType.PtrBytes = 0
		s.largeType.Size_ = s.elemsize
	} else {
		// mspan structs are recycled; don't leave a stale dummy type
		// pointer from a previous scan span.
		*(*uintptr)(unsafe.Pointer(&s.largeType)) = 0
	}

	return s
}

// mpAllocLargeNoscan allocates a single object of size bytes on its own
// span, with immediate release on free (same machinery as
// mpTypedAllocLarge, minus the GC bitmap). Intended for objects larger
// than 1KB where per-object page-granularity overhead is acceptable.
//
// The memory is NOT scanned by the GC: storing Go pointers in it is
// forbidden (pointed-to objects may be collected). The memory is zeroed.
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
	span, x := mpSpanCacheGet(npages, true)
	if span == nil {
		systemstack(func() {
			span = mheap_.allocUserArenaSpan(npages, true)
		})
		if span == nil {
			throw("out of memory")
		}
		x = unsafe.Pointer(span.base())
	} else {
		mpSpanCacheReinit(span, x, true)
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

	mp.mallocing = 0
	releasem(mp)

	if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
		gcStart(t)
	}

	return x
}

// mpFreeLargeNoscan frees an object allocated by mpAllocLargeNoscan.
// Pages are faulted (returned to the OS) as soon as the GC phase permits.
func mpFreeLargeNoscan(x unsafe.Pointer) {
	mpTypedFreeLarge(x) // same span shape; identical release path
}
