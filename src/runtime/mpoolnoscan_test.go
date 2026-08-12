// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"fmt"
	"math/rand"
	"os"
	. "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// mpStressLogFile, when the MPSTRESS_LOG env var names a file, receives
// every stress-test result line (in addition to t.Logf output).
var mpStressLogFile = sync.OnceValues(func() (*os.File, error) {
	path := os.Getenv("MPSTRESS_LOG")
	if path == "" {
		return nil, nil
	}
	return os.Create(path)
})

func mpStressLog(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	if f, _ := mpStressLogFile(); f != nil {
		fmt.Fprintf(f, t.Name()+": "+format+"\n", args...)
	}
}

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

// mpLargeStress drives a high-concurrency, high-churn workload of large
// allocations: each worker keeps a small ring of live objects, evicting
// (freeing) the oldest on every iteration, and touches every page to
// force physical commit. alloc/free are supplied by the caller so the
// same workload can compare different allocation paths.
func mpLargeStress(t *testing.T, workers int, dur time.Duration, alloc func(uintptr) unsafe.Pointer, free func(unsafe.Pointer, uintptr)) (ops int64, peakInuse, endReleased uint64) {
	return mpLargeStressSizes(t, workers, dur, nil, alloc, free)
}

// mpLargeStressSizes is mpLargeStress with an explicit size set; a nil
// sizes slice means the default uniform 64KB..2MB random distribution.
func mpLargeStressSizes(t *testing.T, workers int, dur time.Duration, sizes []uintptr, alloc func(uintptr) unsafe.Pointer, free func(unsafe.Pointer, uintptr)) (ops int64, peakInuse, endReleased uint64) {
	t.Helper()
	var total atomic.Int64
	var peak atomic.Uint64
	stop := make(chan struct{})

	// Peak HeapInuse sampler.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			var ms MemStats
			ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			const ringSize = 4
			ring := make([]unsafe.Pointer, ringSize)
			ringSz := make([]uintptr, ringSize)
			i := 0
			for {
				select {
				case <-stop:
					for j := range ring {
						if ring[j] != nil {
							free(ring[j], ringSz[j])
						}
					}
					return
				default:
				}
				// 64KB .. 2MB, uniform; or from the explicit size set.
				var sz uintptr
				if len(sizes) > 0 {
					sz = sizes[r.Intn(len(sizes))]
				} else {
					sz = uintptr(64<<10) + uintptr(r.Intn(1984<<10))
				}
				p := alloc(sz)
				// Touch every page to force commit.
				for off := uintptr(0); off < sz; off += 4096 {
					*(*byte)(unsafe.Add(p, off)) = 1
				}
				if ring[i] != nil {
					free(ring[i], ringSz[i])
				}
				ring[i], ringSz[i] = p, sz
				i = (i + 1) % ringSize
				total.Add(1)
			}
		}(int64(w)*7919 + 1)
	}
	time.Sleep(dur)
	close(stop)
	wg.Wait()

	GC()
	GC()
	var ms MemStats
	ReadMemStats(&ms)
	return total.Load(), peak.Load(), ms.HeapReleased
}

// sync.Pool mode for the stress workload: power-of-two bucketed []byte pools.
var mpStressPools [16]sync.Pool // bucket i holds 64KB<<i buffers

func mpStressPoolBucket(size uintptr) (int, uintptr) {
	n := uintptr(64 << 10)
	for i := 0; ; i++ {
		if n >= size {
			return i, n
		}
		n <<= 1
	}
}

func mpStressPoolAlloc(size uintptr) unsafe.Pointer {
	i, n := mpStressPoolBucket(size)
	b, _ := mpStressPools[i].Get().([]byte)
	if cap(b) < int(n) {
		b = make([]byte, n)
	}
	b = b[:n]
	return unsafe.Pointer(unsafe.SliceData(b))
}

func mpStressPoolFree(p unsafe.Pointer, size uintptr) {
	i, n := mpStressPoolBucket(size)
	mpStressPools[i].Put(unsafe.Slice((*byte)(p), n))
}

func mpStressWrapFree(f func(unsafe.Pointer)) func(unsafe.Pointer, uintptr) {
	return func(p unsafe.Pointer, _ uintptr) { f(p) }
}

func mpStressMakeAlloc(sz uintptr) unsafe.Pointer {
	b := make([]byte, sz)
	return unsafe.Pointer(unsafe.SliceData(b))
}

func mpStressNoFree(unsafe.Pointer, uintptr) {}

func TestMPNoscanLargeStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const workers = 32
	const dur = 2 * time.Second

	ops, peak, released := mpLargeStress(t, workers, dur, MPAllocLargeNoscan, mpStressWrapFree(MPFreeLargeNoscan))
	mpStressLog(t, "noscan: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops, float64(ops)/dur.Seconds(), float64(peak)/(1<<20), float64(released)/(1<<20))

	ops2, peak2, released2 := mpLargeStress(t, workers, dur, mpStressMakeAlloc, mpStressNoFree)
	mpStressLog(t, "make:   %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops2, float64(ops2)/dur.Seconds(), float64(peak2)/(1<<20), float64(released2)/(1<<20))

	ops4, peak4, released4 := mpLargeStress(t, workers, dur, mpStressPoolAlloc, mpStressPoolFree)
	mpStressLog(t, "sync.Pool: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
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
	mpStressLog(t, "noscan: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops, float64(ops)/dur.Seconds(), float64(peak)/(1<<20), float64(released)/(1<<20))

	ops2, peak2, released2 := mpLargeStressSizes(t, workers, dur, sizes, mpStressMakeAlloc, mpStressNoFree)
	mpStressLog(t, "make:   %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops2, float64(ops2)/dur.Seconds(), float64(peak2)/(1<<20), float64(released2)/(1<<20))

	ops4, peak4, released4 := mpLargeStressSizes(t, workers, dur, sizes, mpStressPoolAlloc, mpStressPoolFree)
	mpStressLog(t, "sync.Pool: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops4, float64(ops4)/dur.Seconds(), float64(peak4)/(1<<20), float64(released4)/(1<<20))
}
