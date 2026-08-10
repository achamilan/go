// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// mpool is a manual memory allocator with C-style malloc/free semantics:
// deterministic reuse, size-class segregation, and eager reclamation of
// fully idle chunks. Three-level structure:
//
//	per-P cache (lock-free fast path, pinned via acquirem)
//	-> per-class central cache (one lock per class, batched exchange)
//	-> arena (4MB chunks, one class each, address-ordered registry)
//
// Hard constraint: chunk and large-block memory is noscan. Headers are
// plain integers and user data must never contain Go pointers — the GC
// does not scan noscan memory on any build.

var mpGlobal mpAllocator

type mpAllocator struct {
	arena    mpArena
	centrals [mpMaxClasses]mpCentral
}

// mpMalloc allocates size bytes, like C malloc: reused blocks are NOT
// zeroed; size == 0 allocates 1 byte. The result is 8-byte aligned.
func mpMalloc(size uintptr) unsafe.Pointer {
	if size == 0 {
		size = 1
	}
	allocSize := size + mpDebugOverhead
	if allocSize > maxSmallSize {
		return mpDebugMallocLarge(mpGlobal.arena.allocLarge(allocSize), size)
	}
	cls := mpSizeToClass(allocSize)
	for {
		mp := acquirem()
		if mp.gsignal == getg() {
			throw("mpMalloc during signal")
		}
		if pp := mp.p.ptr(); pp != nil {
			tc := &pp.mpcache
			if lst := tc.lists[cls]; len(lst) > 0 {
				base := lst[len(lst)-1]
				lst[len(lst)-1] = nil
				tc.lists[cls] = lst[:len(lst)-1]
				releasem(mp)
				return mpDebugMalloc(base, size)
			}
		}
		releasem(mp)
		mpRefill(cls) // slow path: may take locks; not under acquirem
	}
}

// mpCalloc allocates n*size zeroed bytes, like C calloc.
func mpCalloc(n, size uintptr) unsafe.Pointer {
	total := n * size
	p := mpMalloc(total)
	memclrNoHeapPointers(p, total)
	return p
}

// mpFree releases p, like C free: mpFree(nil) is a no-op. Using p after
// freeing, or freeing it twice, is undefined behavior (and throws in
// debug builds).
func mpFree(p unsafe.Pointer) {
	if p == nil {
		return
	}
	// Debug validation runs before the header read: an invalid pointer
	// may not even be readable.
	mpDebugFree(p)
	// The flag word sits 8 bytes before the user pointer in both layouts:
	// low bit 1 = small (cls<<1|1), low bits 00 = large (size<<2).
	w := *(*uintptr)(unsafe.Add(p, -8))
	if w&1 == 0 {
		mpGlobal.arena.freeLarge(p)
		return
	}
	cls := int(w >> 1)
	base := unsafe.Add(p, -mpUserOffset)
	for {
		mp := acquirem()
		if pp := mp.p.ptr(); pp != nil {
			tc := &pp.mpcache
			tc.lists[cls] = append(tc.lists[cls], base)
			if len(tc.lists[cls]) < 2*mpBatchSize {
				releasem(mp)
				return
			}
			// Cache is full: hand half back to central, after dropping
			// the m lock (central's lock may park the goroutine).
			half := tc.lists[cls][mpBatchSize:]
			tc.lists[cls] = tc.lists[cls][:mpBatchSize]
			releasem(mp)
			mpGlobal.centrals[cls].give(half)
			return
		}
		releasem(mp)
		// No P (bootstrap paths): go straight to central.
		mpGlobal.centrals[cls].give([]unsafe.Pointer{base})
		return
	}
}

// mpRealloc resizes p to size bytes, preserving min(old, new) contents,
// like C realloc. mpRealloc(nil, size) is mpMalloc(size).
func mpRealloc(p unsafe.Pointer, size uintptr) unsafe.Pointer {
	if p == nil {
		return mpMalloc(size)
	}
	if size == 0 {
		size = 1
	}
	old := mpUsableSize(p)
	// In-place if the new size lands in the same class, or for a large
	// block that shrinks by at most half. Skipped in debug builds so a
	// stale access to the old block is caught.
	if !mpDebug && size <= old && (old <= maxSmallSize && mpSizeToClass(size) == mpSizeToClass(old) ||
		old > maxSmallSize && size > maxSmallSize && size >= old/2) {
		return p
	}
	np := mpMalloc(size)
	memmove(np, p, min(old, size))
	mpFree(p)
	return np
}

// mpUsableSize returns the usable size of an allocation, like C
// malloc_usable_size (class size, or the requested size for large blocks;
// the requested size in debug builds).
func mpUsableSize(p unsafe.Pointer) uintptr {
	if p == nil {
		return 0
	}
	if mpDebug {
		return *(*uintptr)(unsafe.Add(p, -16))
	}
	w := *(*uintptr)(unsafe.Add(p, -8))
	if w&1 == 0 {
		return w >> 2
	}
	return mpClassTab.sizes[int(w>>1)]
}
