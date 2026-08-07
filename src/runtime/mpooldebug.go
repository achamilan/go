// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Debug guardrails for mpool, gated by a const pair so the compiler
// dead-code-eliminates everything in production builds (zero cost).
// To enable, flip both consts to true/1 and rebuild the runtime.
//
// When enabled:
//  1. double-free / invalid-free detection: a global registry of live
//     pointers; freeing an unregistered pointer throws.
//  2. use-after-free poisoning: user area is filled with 0xDD on free
//     and 0xCD on allocation (MSVC CRT style).
//  3. boundary canary: 8 magic bytes past the user area, checked on free.
//
// Block layouts (the flag word always sits 8 bytes before the user pointer,
// and the requested size 16 bytes before it, in both modes):
//
//	small: [reqsize][flag=(cls<<1)|1][user size][canary]
//	large: [bucket][reqsize][flag=size<<2][user size][canary]
//
// Allocation picks the class/bucket by size+mpDebugOverhead, so the canary
// never eats user space or breaks alignment.

const (
	mpDebug    = false // master switch
	mpDebugExt = 0     // 1 when mpDebug is set; keep in sync!
)

const (
	mpDebugOverhead = mpDebugExt * 16        // reqsize slot + canary
	mpUserOffset    = mpHeaderSize + mpDebugExt*8
	mpLargeHeader   = 16 + mpDebugExt*8
	mpCanaryWord    = 0xDEADBEEFC0FFEE11
)

// mpDbg is the live-pointer registry (debug mode only).
// Linear scans are fine for a debug build.
var mpDbg struct {
	lock mutex // never nested with other mpool locks; mpArena rank is fine
	live []mpDbgEntry
}

type mpDbgEntry struct {
	p    unsafe.Pointer
	size uintptr
}

func mpDbgAdd(p unsafe.Pointer, size uintptr) {
	lockWithRank(&mpDbg.lock, lockRankMpArena)
	mpDbg.live = append(mpDbg.live, mpDbgEntry{p, size})
	unlock(&mpDbg.lock)
}

func mpDbgRemove(p unsafe.Pointer) (uintptr, bool) {
	lockWithRank(&mpDbg.lock, lockRankMpArena)
	for i, e := range mpDbg.live {
		if e.p == p {
			mpDbg.live[i] = mpDbg.live[len(mpDbg.live)-1]
			mpDbg.live[len(mpDbg.live)-1] = mpDbgEntry{}
			mpDbg.live = mpDbg.live[:len(mpDbg.live)-1]
			unlock(&mpDbg.lock)
			return e.size, true
		}
	}
	unlock(&mpDbg.lock)
	return 0, false
}

// mpDebugMalloc finishes a small-block allocation: it stamps the requested
// size, the trailing canary, poisons the user area, and registers p.
// The flag word (at base+mpUserOffset-8) was written when the block was carved.
func mpDebugMalloc(base unsafe.Pointer, size uintptr) unsafe.Pointer {
	if !mpDebug {
		return unsafe.Add(base, mpHeaderSize)
	}
	*(*uintptr)(base) = size
	p := unsafe.Add(base, mpUserOffset)
	*(*uintptr)(unsafe.Add(p, size)) = mpCanaryWord
	mpFill(p, size, 0xCD)
	mpDbgAdd(p, size)
	return p
}

// mpDebugMallocLarge is mpDebugMalloc for large blocks; bucket and flag
// were written by mpWriteLargeMeta.
func mpDebugMallocLarge(base unsafe.Pointer, size uintptr) unsafe.Pointer {
	if !mpDebug {
		return unsafe.Add(base, mpLargeHeader)
	}
	*(*uintptr)(unsafe.Add(base, 8)) = size
	p := unsafe.Add(base, mpLargeHeader)
	*(*uintptr)(unsafe.Add(p, size)) = mpCanaryWord
	mpFill(p, size, 0xCD)
	mpDbgAdd(p, size)
	return p
}

// mpDebugFree validates a free: registration, canary, then poison.
func mpDebugFree(p unsafe.Pointer) {
	if !mpDebug {
		return
	}
	size, ok := mpDbgRemove(p)
	if !ok {
		throw("mpool: double free or free of untracked pointer")
	}
	if *(*uintptr)(unsafe.Add(p, size)) != mpCanaryWord {
		throw("mpool: canary corrupted (buffer overflow past allocation)")
	}
	mpFill(p, size, 0xDD)
}

// mpWriteLargeMeta writes the large-block header: bucket at the block base,
// flag=size<<2 at the flag offset (8 bytes before the user pointer).
func mpWriteLargeMeta(base unsafe.Pointer, size uintptr, bucket int) {
	*(*int)(base) = bucket
	*(*uintptr)(unsafe.Add(base, mpLargeHeader-8)) = size << 2
}

func mpFill(p unsafe.Pointer, n uintptr, v byte) {
	for i := uintptr(0); i < n; i++ {
		*(*byte)(unsafe.Add(p, i)) = v
	}
}
