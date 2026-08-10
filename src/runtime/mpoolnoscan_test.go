// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	. "runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestMPNoscanLargeBasic(t *testing.T) {
	for _, sz := range []uintptr{1025, 4096, 100 << 10, 2 << 20, 8 << 20} {
		p := MPAllocLargeNoscan(sz)
		if p == nil {
			t.Fatalf("size %d: nil", sz)
		}
		b := unsafe.Slice((*byte)(p), sz)
		if b[0] != 0 || b[sz-1] != 0 { // must be zeroed
			t.Fatalf("size %d: not zeroed", sz)
		}
		b[0], b[sz-1] = 1, 1
		MPFreeLargeNoscan(p)
	}
}

func TestMPNoscanLargeGCSurvival(t *testing.T) {
	p := MPAllocLargeNoscan(64 << 10)
	for i := range unsafe.Slice((*byte)(p), 64<<10) {
		unsafe.Slice((*byte)(p), 64<<10)[i] = byte(i)
	}
	GC()
	GC()
	for i := 0; i < 64<<10; i += 4096 {
		if v := *(*byte)(unsafe.Add(p, i)); v != byte(i) {
			t.Fatalf("data corrupted after GC at %d: got %d", i, v)
		}
	}
	MPFreeLargeNoscan(p)
}

func TestMPNoscanLargeReuse(t *testing.T) {
	p := MPAllocLargeNoscan(128 << 10)
	MPFreeLargeNoscan(p)
	for i := 0; i < 64; i++ {
		q := MPAllocLargeNoscan(128 << 10)
		*(*byte)(q) = 1
		MPFreeLargeNoscan(q)
	}
	GC()
	for i := 0; i < 64; i++ {
		q := MPAllocLargeNoscan(128 << 10)
		MPFreeLargeNoscan(q)
	}
}

func TestMPNoscanLargeConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				sz := uintptr(4096 + (g*300+i)%500*4096)
				p := MPAllocLargeNoscan(sz)
				*(*byte)(p) = byte(g)
				*(*byte)(unsafe.Add(p, sz-1)) = byte(i)
				MPFreeLargeNoscan(p)
			}
		}(g)
	}
	wg.Wait()
}

func TestMPNoscanLargeStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const workers = 32
	const dur = 2 * time.Second

	ops, peak, released := mpLargeStress(t, workers, dur, MPAllocLargeNoscan, mpStressWrapFree(MPFreeLargeNoscan))
	t.Logf("noscan: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops, float64(ops)/dur.Seconds(), float64(peak)/(1<<20), float64(released)/(1<<20))

	ops3, peak3, released3 := mpLargeStress(t, workers, dur,
		func(sz uintptr) unsafe.Pointer {
			return MPTypedAllocLarge(mpTypedNodeType, max(mpTypedNodeSize, sz))
		},
		mpStressWrapFree(MPTypedFreeLarge))
	t.Logf("typed:  %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops3, float64(ops3)/dur.Seconds(), float64(peak3)/(1<<20), float64(released3)/(1<<20))

	ops2, peak2, released2 := mpLargeStress(t, workers, dur,
		func(sz uintptr) unsafe.Pointer {
			b := make([]byte, sz)
			return unsafe.Pointer(unsafe.SliceData(b))
		},
		func(unsafe.Pointer, uintptr) {})
	t.Logf("make:   %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops2, float64(ops2)/dur.Seconds(), float64(peak2)/(1<<20), float64(released2)/(1<<20))

	ops4, peak4, released4 := mpLargeStress(t, workers, dur, mpStressPoolAlloc, mpStressPoolFree)
	t.Logf("sync.Pool: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops4, float64(ops4)/dur.Seconds(), float64(peak4)/(1<<20), float64(released4)/(1<<20))
}

// TestMPSpanStressFixedSizes uses a small discrete size set (realistic
// buffer-pool shape) where the deferred-fault cache can actually hit.
func TestMPSpanStressFixedSizes(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const workers = 32
	const dur = 2 * time.Second
	sizes := []uintptr{64 << 10, 128 << 10, 256 << 10, 512 << 10}

	ops, peak, released := mpLargeStressSizes(t, workers, dur, sizes, MPAllocLargeNoscan, mpStressWrapFree(MPFreeLargeNoscan))
	t.Logf("noscan: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops, float64(ops)/dur.Seconds(), float64(peak)/(1<<20), float64(released)/(1<<20))

	ops3, peak3, released3 := mpLargeStressSizes(t, workers, dur, sizes,
		func(sz uintptr) unsafe.Pointer {
			return MPTypedAllocLarge(mpTypedNodeType, max(mpTypedNodeSize, sz))
		},
		mpStressWrapFree(MPTypedFreeLarge))
	t.Logf("typed:  %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops3, float64(ops3)/dur.Seconds(), float64(peak3)/(1<<20), float64(released3)/(1<<20))

	ops2, peak2, released2 := mpLargeStressSizes(t, workers, dur, sizes,
		func(sz uintptr) unsafe.Pointer {
			b := make([]byte, sz)
			return unsafe.Pointer(unsafe.SliceData(b))
		},
		func(unsafe.Pointer, uintptr) {})
	t.Logf("make:   %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops2, float64(ops2)/dur.Seconds(), float64(peak2)/(1<<20), float64(released2)/(1<<20))

	ops4, peak4, released4 := mpLargeStressSizes(t, workers, dur, sizes, mpStressPoolAlloc, mpStressPoolFree)
	t.Logf("sync.Pool: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops4, float64(ops4)/dur.Seconds(), float64(peak4)/(1<<20), float64(released4)/(1<<20))
}
