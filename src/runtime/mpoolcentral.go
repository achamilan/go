// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// mpCentral is the per-class central cache of the manual memory pool.
// free holds block bases (header start). Blocks are exchanged with
// per-P caches in batches of mpBatchSize.
type mpCentral struct {
	lock mutex
	free []unsafe.Pointer
}

// take pops up to n blocks.
func (c *mpCentral) take(n int) []unsafe.Pointer {
	lockWithRank(&c.lock, lockRankMpCentral)
	defer unlock(&c.lock)
	if len(c.free) == 0 {
		return nil
	}
	if n > len(c.free) {
		n = len(c.free)
	}
	out := c.free[len(c.free)-n:]
	c.free = c.free[:len(c.free)-n]
	mpGlobal.arena.noteTaken(out)
	return out
}

// give returns a batch of blocks. If a chunk becomes fully idle as a
// result (all of its blocks are back in the central list), its blocks
// are purged and the chunk is unregistered, releasing the memory to the GC.
func (c *mpCentral) give(blocks []unsafe.Pointer) {
	lockWithRank(&c.lock, lockRankMpCentral)
	defer unlock(&c.lock)
	c.free = append(c.free, blocks...)
	full := mpGlobal.arena.noteFreed(blocks)
	if len(full) == 0 {
		return
	}
	// Holding c.lock, no take can interleave: purging is safe.
	kept := c.free[:0]
	for _, p := range c.free {
		if !mpGlobal.arena.contains(full, p) {
			kept = append(kept, p)
		}
	}
	for i := len(kept); i < len(c.free); i++ {
		c.free[i] = nil
	}
	c.free = kept
	mpGlobal.arena.release(full)
}

// mpThreadCache is the per-P cache of free blocks: one stack per class,
// no locking (accessed only while the owning P is pinned via acquirem).
// Unlike a sync.Pool-backed cache it is never cleared by GC; residual
// blocks are flushed to central at the start of every GC cycle
// (see mpFlushAll), mirroring mcache's flushallmcaches.
type mpThreadCache struct {
	lists [mpMaxClasses][]unsafe.Pointer
}

// mpWriteSmallHeader stamps a freshly carved block's flag word:
// (cls << 1) | 1. The low bit 1 marks a small block; large blocks store
// an even flag (size << 2). Written once at carve time; reused blocks
// keep their flag. The flag sits 8 bytes before the user pointer.
func mpWriteSmallHeader(base unsafe.Pointer, cls int) {
	*(*uintptr)(unsafe.Add(base, mpUserOffset-8)) = uintptr(cls)<<1 | 1
}

// mpRefill moves a batch of blocks of class cls into the current P's
// cache. Called without m locks held: locks may park the goroutine.
func mpRefill(cls int) {
	c := &mpGlobal.centrals[cls]
	blocks := c.take(mpBatchSize)
	if len(blocks) == 0 {
		blocks = mpGlobal.arena.allocBlocks(mpBlockSize(cls), mpBatchSize, cls)
		for _, b := range blocks {
			mpWriteSmallHeader(b, cls)
		}
	}
	mp := acquirem()
	pp := mp.p.ptr()
	if pp == nil {
		// Lost the P between the fast path and here: hand blocks back.
		releasem(mp)
		c.give(blocks)
		return
	}
	tc := &pp.mpcache
	tc.lists[cls] = append(tc.lists[cls], blocks...)
	releasem(mp)
}

// mpFlushAll moves every per-P cached block back to the central caches.
// Called with the world stopped (from gcStart, next to flushallmcaches),
// so no other goroutine can be mutating a P cache concurrently.
// This is what lets fully-idle chunks be reclaimed even though per-P
// caches would otherwise hold residual blocks forever.
func mpFlushAll() {
	for _, pp := range allp {
		tc := &pp.mpcache
		for cls := 0; cls < mpClassTab.n; cls++ {
			if len(tc.lists[cls]) == 0 {
				continue
			}
			blocks := tc.lists[cls]
			tc.lists[cls] = nil
			mpGlobal.centrals[cls].give(blocks)
		}
	}
}
