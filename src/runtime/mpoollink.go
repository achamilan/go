// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Linkname wrappers exposing noscan span allocation to package mempool.

//go:linkname mempool_AllocSpan mempool.runtime_mempool_AllocSpan
func mempool_AllocSpan(size uintptr) unsafe.Pointer { return mpAllocLargeNoscan(size) }

//go:linkname mempool_FreeSpan mempool.runtime_mempool_FreeSpan
func mempool_FreeSpan(p unsafe.Pointer) { mpFreeLargeNoscan(p) }
