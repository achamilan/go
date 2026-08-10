// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// mpChunk is a 4MB slab carved into same-class blocks (bump allocation).
// A chunk serves a single size class so that a fully idle chunk can be
// reclaimed without scanning other classes' free lists:
//
//   - total:     blocks ever carved from this chunk (monotonic)
//   - inCentral: blocks currently sitting in the central free list
//
// When inCentral == total, every block is in the central list — none in
// user hands, none in any per-P cache — and the chunk can be dropped:
// its blocks are purged from the free list and the chunk is unregistered,
// letting the GC (and eventually the scavenger) return the memory.
//
// The chunk memory itself is a noscan heap object (mallocgc with a nil
// type). Only plain integers live in there: headers and user data.
// Storing Go pointers in chunk memory is forbidden — noscan memory is
// not scanned by the GC, on any build.
type mpChunk struct {
	base      unsafe.Pointer
	offset    int
	cls       int32
	total     int32
	inCentral int32
}

const mpChunkSize = 4 << 20 // 4MB

// mpArena manages all chunks: an address-ordered registry (binary search
// maps a block back to its chunk), one active bump-allocation chunk per
// class, and the large-block bucket cache.
type mpArena struct {
	lock   mutex
	chunks []*mpChunk             // sorted by base
	cur    [mpMaxClasses]*mpChunk // active chunk per class
	large  mpLargeCache
}

// allocBlocks carves n blocks of bSize bytes from the class's active chunk.
// Returns block bases (header start).
func (a *mpArena) allocBlocks(bSize uintptr, n, cls int) []unsafe.Pointer {
	out := make([]unsafe.Pointer, 0, n)
	lockWithRank(&a.lock, lockRankMpArena)
	defer unlock(&a.lock)
	for len(out) < n {
		cur := a.cur[cls]
		if cur == nil || uintptr(cur.offset)+bSize > mpChunkSize {
			base := mallocgc(mpChunkSize, nil, true) // noscan, zeroed
			cur = &mpChunk{base: base, cls: int32(cls)}
			a.insertLocked(cur)
			a.cur[cls] = cur
		}
		out = append(out, unsafe.Add(cur.base, cur.offset))
		cur.offset += int(bSize)
		cur.total++
	}
	return out
}

// insertLocked inserts c into the address-ordered registry.
func (a *mpArena) insertLocked(c *mpChunk) {
	base := uintptr(c.base)
	i := 0
	for i < len(a.chunks) && uintptr(a.chunks[i].base) < base {
		i++
	}
	a.chunks = append(a.chunks, nil)
	copy(a.chunks[i+1:], a.chunks[i:])
	a.chunks[i] = c
}

// chunkOfLocked finds the chunk containing p, or nil. Caller holds a.lock.
func (a *mpArena) chunkOfLocked(p unsafe.Pointer) *mpChunk {
	addr := uintptr(p)
	lo, hi := 0, len(a.chunks) // first chunk with base > addr
	for lo < hi {
		mid := (lo + hi) / 2
		if uintptr(a.chunks[mid].base) <= addr {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return nil
	}
	c := a.chunks[lo-1]
	if addr < uintptr(c.base)+mpChunkSize {
		return c
	}
	return nil
}

// noteTaken records blocks leaving the central free list.
func (a *mpArena) noteTaken(blocks []unsafe.Pointer) {
	lockWithRank(&a.lock, lockRankMpArena)
	defer unlock(&a.lock)
	for _, p := range blocks {
		if c := a.chunkOfLocked(p); c != nil {
			c.inCentral--
		}
	}
}

// noteFreed records blocks entering the central free list and returns any
// chunks that became fully idle. The caller must hold the corresponding
// central lock, so no take can interleave between the fullness check and
// the subsequent purge.
func (a *mpArena) noteFreed(blocks []unsafe.Pointer) (full []*mpChunk) {
	lockWithRank(&a.lock, lockRankMpArena)
	defer unlock(&a.lock)
	for _, p := range blocks {
		c := a.chunkOfLocked(p)
		if c == nil {
			continue
		}
		c.inCentral++
		if c.inCentral == c.total {
			full = append(full, c)
		}
	}
	return full
}

// release unregisters fully idle chunks. Their blocks were already purged
// from the central list by the caller, so nothing references the chunk
// memory anymore and the GC can collect it.
func (a *mpArena) release(full []*mpChunk) {
	lockWithRank(&a.lock, lockRankMpArena)
	defer unlock(&a.lock)
	for _, c := range full {
		if c.inCentral != c.total { // raced with reuse; skip
			continue
		}
		for i, x := range a.chunks {
			if x == c {
				a.chunks[i] = a.chunks[len(a.chunks)-1]
				a.chunks[len(a.chunks)-1] = nil
				a.chunks = a.chunks[:len(a.chunks)-1]
				// Registry order broke; restore it. Churn is rare.
				a.sortLocked()
				break
			}
		}
		if a.cur[c.cls] == c {
			a.cur[c.cls] = nil
		}
	}
}

// sortLocked restores base ordering after a removal. Insertion sort is
// fine: the registry is small and removals are rare.
func (a *mpArena) sortLocked() {
	for i := 1; i < len(a.chunks); i++ {
		for j := i; j > 0 && uintptr(a.chunks[j].base) < uintptr(a.chunks[j-1].base); j-- {
			a.chunks[j], a.chunks[j-1] = a.chunks[j-1], a.chunks[j]
		}
	}
}

// contains reports whether p lies in any of the given chunks.
// It reads only immutable chunk fields and needs no lock.
func (a *mpArena) contains(full []*mpChunk, p unsafe.Pointer) bool {
	for _, c := range full {
		if uintptr(p) >= uintptr(c.base) && uintptr(p) < uintptr(c.base)+mpChunkSize {
			return true
		}
	}
	return false
}

// chunkCount reports the number of registered chunks (for tests).
func (a *mpArena) chunkCount() int {
	lockWithRank(&a.lock, lockRankMpArena)
	defer unlock(&a.lock)
	return len(a.chunks)
}

// Large blocks (> maxSmallSize): power-of-two buckets from 64KB to 4MB.
// Freed buffers go back to their bucket for reuse instead of becoming GC
// work. Retention is bounded: at most mpLargeKeepPerBucket buffers per
// bucket (~64MB worst case). Blocks above 4MB are not cached.

const (
	mpLargeBucketBase    = 16 // smallest bucket is 1<<16 = 64KB
	mpLargeBucketCount   = 7  // 64KB .. 4MB
	mpLargeKeepPerBucket = 8
)

type mpLargeCache struct {
	lock mutex // never nested with other mpool locks; mpArena rank is fine
	free [mpLargeBucketCount][]unsafe.Pointer
}

func mpLargeBucketOf(size uintptr) (idx int, ok bool) {
	if size > 1<<(mpLargeBucketBase+mpLargeBucketCount-1) {
		return 0, false
	}
	for n := uintptr(1 << mpLargeBucketBase); n < size; n <<= 1 {
		idx++
	}
	return idx, true
}

func mpLargeBucketBytes(idx int) uintptr { return 1 << (mpLargeBucketBase + idx) }

func (lc *mpLargeCache) take(idx int) unsafe.Pointer {
	lockWithRank(&lc.lock, lockRankMpArena)
	defer unlock(&lc.lock)
	lst := lc.free[idx]
	if len(lst) == 0 {
		return nil
	}
	p := lst[len(lst)-1]
	lst[len(lst)-1] = nil
	lc.free[idx] = lst[:len(lst)-1]
	return p
}

func (lc *mpLargeCache) give(idx int, base unsafe.Pointer) {
	lockWithRank(&lc.lock, lockRankMpArena)
	defer unlock(&lc.lock)
	if len(lc.free[idx]) < mpLargeKeepPerBucket {
		lc.free[idx] = append(lc.free[idx], base)
	}
}

// allocLarge allocates a large block and returns its base (the user
// pointer is computed by mpDebugMallocLarge).
func (a *mpArena) allocLarge(size uintptr) unsafe.Pointer {
	if idx, ok := mpLargeBucketOf(size); ok {
		if base := a.large.take(idx); base != nil {
			mpWriteLargeMeta(base, size, idx)
			return base
		}
		base := mallocgc(mpLargeHeader+mpLargeBucketBytes(idx), nil, true)
		mpWriteLargeMeta(base, size, idx)
		return base
	}
	base := mallocgc(mpLargeHeader+size, nil, true)
	mpWriteLargeMeta(base, size, -1)
	return base
}

// freeLarge returns a large block to its bucket. p is the user pointer.
// Blocks with bucket < 0 (above 4MB) are simply dropped for the GC.
func (a *mpArena) freeLarge(p unsafe.Pointer) {
	bucket := *(*int)(unsafe.Add(p, -mpLargeHeader))
	if bucket < 0 {
		return
	}
	a.large.give(bucket, unsafe.Add(p, -mpLargeHeader))
}
