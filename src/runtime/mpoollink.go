// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Linkname wrappers exposing mpool and span allocation to package mempool.

//go:linkname mempool_Malloc mempool.runtime_mempool_Malloc
func mempool_Malloc(size uintptr) unsafe.Pointer { return mpMalloc(size) }

//go:linkname mempool_Calloc mempool.runtime_mempool_Calloc
func mempool_Calloc(n, size uintptr) unsafe.Pointer { return mpCalloc(n, size) }

//go:linkname mempool_Realloc mempool.runtime_mempool_Realloc
func mempool_Realloc(p unsafe.Pointer, size uintptr) unsafe.Pointer { return mpRealloc(p, size) }

//go:linkname mempool_Free mempool.runtime_mempool_Free
func mempool_Free(p unsafe.Pointer) { mpFree(p) }

//go:linkname mempool_UsableSize mempool.runtime_mempool_UsableSize
func mempool_UsableSize(p unsafe.Pointer) uintptr { return mpUsableSize(p) }

//go:linkname mempool_AllocSpan mempool.runtime_mempool_AllocSpan
func mempool_AllocSpan(size uintptr) unsafe.Pointer { return mpAllocLargeNoscan(size) }

//go:linkname mempool_FreeSpan mempool.runtime_mempool_FreeSpan
func mempool_FreeSpan(p unsafe.Pointer) { mpFreeLargeNoscan(p) }

//go:linkname mempool_TypedAllocLarge mempool.runtime_mempool_TypedAllocLarge
func mempool_TypedAllocLarge(t *_type, size uintptr) unsafe.Pointer {
	return mpTypedAllocLarge(t, size)
}
