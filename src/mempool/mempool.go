// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mempool provides C-style manual memory management for large
// objects: one object per span with immediate return of the memory to
// the operating system on free.
//
// The memory is NOT scanned by the garbage collector: it must never
// contain Go pointers. Intended for large (>1KB) pointer-free data such
// as I/O buffers, where per-object page-granularity overhead is
// acceptable in exchange for prompt memory release.
//
// As in C, violating the contracts is undefined behavior:
//   - Using a pointer after FreeSpan faults deterministically (the pages
//     are unmapped).
//   - Freeing twice, or freeing a pointer not produced by AllocSpan, is
//     undefined behavior.
package mempool

import "unsafe"

// AllocSpan allocates size bytes on its own span. The memory is zeroed,
// is not scanned by the garbage collector (it must not contain Go
// pointers), and is returned to the operating system when freed —
// accessing it after FreeSpan faults deterministically.
func AllocSpan(size uintptr) unsafe.Pointer { return runtime_mempool_AllocSpan(size) }

// FreeSpan frees a span allocated by AllocSpan. FreeSpan(nil) is a no-op.
func FreeSpan(p unsafe.Pointer) { runtime_mempool_FreeSpan(p) }

//go:linkname runtime_mempool_AllocSpan
func runtime_mempool_AllocSpan(size uintptr) unsafe.Pointer

//go:linkname runtime_mempool_FreeSpan
func runtime_mempool_FreeSpan(p unsafe.Pointer)
