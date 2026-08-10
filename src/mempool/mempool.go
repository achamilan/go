// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mempool provides C-style manual memory management:
// deterministic malloc/free with eager memory return, layered by
// use case.
//
// There are two families of operations:
//
//   - Malloc/Calloc/Realloc/Free/UsableSize: a size-class allocator for
//     small blocks (<=32KB) with per-P caches and chunk reclamation.
//     The memory is NOT scanned by the garbage collector: it must never
//     contain Go pointers.
//   - AllocSpan/FreeSpan (+ AllocSpanTyped/FreeSpanTyped): one object
//     per span for large blocks (>1KB), freed back to the OS
//     immediately. AllocSpan memory is also collector-invisible
//     (no Go pointers); AllocSpanTyped memory is fully scanned and
//     may hold Go pointers.
//
// As in C, violating the contracts is undefined behavior:
//   - Malloc returns uninitialized memory (Calloc zeroes it).
//   - Using a pointer after Free, freeing twice, or freeing a pointer
//     not produced by this package is undefined behavior. For spans,
//     use-after-free faults deterministically (the pages are unmapped).
package mempool

import (
	"internal/abi"
	"unsafe"
)

// Malloc allocates size bytes, like C malloc: the memory is uninitialized
// (reused blocks keep their previous contents), and size == 0 allocates 1
// byte. The result is 8-byte aligned. The memory is not scanned by the
// garbage collector and must not contain Go pointers.
func Malloc(size uintptr) unsafe.Pointer { return runtime_mempool_Malloc(size) }

// Calloc allocates n*size bytes of zeroed memory, like C calloc.
// The memory is not scanned by the garbage collector.
func Calloc(n, size uintptr) unsafe.Pointer { return runtime_mempool_Calloc(n, size) }

// Realloc resizes p to size bytes, preserving min(old, new) contents,
// like C realloc. Realloc(nil, size) is equivalent to Malloc(size).
func Realloc(p unsafe.Pointer, size uintptr) unsafe.Pointer { return runtime_mempool_Realloc(p, size) }

// Free releases p, like C free. Free(nil) is a no-op.
func Free(p unsafe.Pointer) { runtime_mempool_Free(p) }

// UsableSize returns the usable size of an allocation made by Malloc,
// Calloc, or Realloc, like C malloc_usable_size.
func UsableSize(p unsafe.Pointer) uintptr { return runtime_mempool_UsableSize(p) }

// AllocSpan allocates size bytes on its own span, intended for large
// objects (>1KB). The memory is zeroed, is not scanned by the garbage
// collector (it must not contain Go pointers), and is returned to the
// operating system immediately when freed — accessing it after FreeSpan
// faults deterministically.
func AllocSpan(size uintptr) unsafe.Pointer { return runtime_mempool_AllocSpan(size) }

// FreeSpan frees a span allocated by AllocSpan or AllocSpanTyped.
// FreeSpan(nil) is a no-op.
func FreeSpan(p unsafe.Pointer) { runtime_mempool_FreeSpan(p) }

// AllocSpanTyped allocates one object of type T on its own span.
// If T contains pointers, the memory carries full GC type information
// and is scanned (the pointers keep their targets alive); if T contains
// no pointers it is equivalent to AllocSpan. The memory is zeroed.
// Free with FreeSpan or FreeSpanTyped: freeing returns the span's pages
// to the OS immediately, so use-after-free faults deterministically.
func AllocSpanTyped[T any]() *T {
	t := abi.TypeOf((*T)(nil)).Elem()
	if !t.Pointers() {
		return (*T)(AllocSpan(t.Size_))
	}
	return (*T)(runtime_mempool_TypedAllocLarge(t, t.Size_))
}

// FreeSpanTyped frees an object allocated by AllocSpanTyped.
func FreeSpanTyped[T any](p *T) {
	FreeSpan(unsafe.Pointer(p))
}

//go:linkname runtime_mempool_Malloc
func runtime_mempool_Malloc(size uintptr) unsafe.Pointer

//go:linkname runtime_mempool_Calloc
func runtime_mempool_Calloc(n, size uintptr) unsafe.Pointer

//go:linkname runtime_mempool_Realloc
func runtime_mempool_Realloc(p unsafe.Pointer, size uintptr) unsafe.Pointer

//go:linkname runtime_mempool_Free
func runtime_mempool_Free(p unsafe.Pointer)

//go:linkname runtime_mempool_UsableSize
func runtime_mempool_UsableSize(p unsafe.Pointer) uintptr

//go:linkname runtime_mempool_AllocSpan
func runtime_mempool_AllocSpan(size uintptr) unsafe.Pointer

//go:linkname runtime_mempool_FreeSpan
func runtime_mempool_FreeSpan(p unsafe.Pointer)

//go:linkname runtime_mempool_TypedAllocLarge
func runtime_mempool_TypedAllocLarge(t *abi.Type, size uintptr) unsafe.Pointer
