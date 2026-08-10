// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/abi"
	"math/rand"
	. "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// mpTypedNode is a pointer-containing type large enough to take the
// one-span-per-object path.
type mpTypedNode struct {
	next  *mpTypedNode
	value int64
	pad   [64 << 10]byte
}

var mpTypedNodeType = abi.TypeOf(mpTypedNode{})
var mpTypedNodeSize = unsafe.Sizeof(mpTypedNode{})

func TestMPTypedLargeBasic(t *testing.T) {
	p := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
	if p == nil {
		t.Fatal("MPTypedAllocLarge returned nil")
	}
	n := (*mpTypedNode)(p)
	if n.value != 0 || n.next != nil { // must be zeroed
		t.Fatal("fresh allocation not zeroed")
	}
	n.value = 42
	MPTypedFreeLarge(p)
}

// TestMPTypedLargeGCScan is the core correctness test: an object that is
// reachable ONLY through a pointer stored inside a typed large allocation
// must survive GC. This validates that the bitmap written at allocation
// time is actually honored by the scanner.
func TestMPTypedLargeGCScan(t *testing.T) {
	p := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
	n := (*mpTypedNode)(p)
	n.next = &mpTypedNode{value: 0xDEAD}
	GC()
	GC()
	if n.next == nil || n.next.value != 0xDEAD {
		t.Fatal("pointer target collected: typed large span not scanned")
	}
	MPTypedFreeLarge(p)
}

func TestMPTypedLargeReuse(t *testing.T) {
	p := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
	MPTypedFreeLarge(p)
	// Same-size spans should be recycled via the ready list after the
	// sweeper moves them off quarantine. Allocation must succeed and be
	// usable; exact-address reuse is timing-dependent, so only smoke it.
	for i := 0; i < 64; i++ {
		q := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
		(*mpTypedNode)(q).value = int64(i)
		MPTypedFreeLarge(q)
	}
	GC() // drive sweep of quarantined spans
	for i := 0; i < 64; i++ {
		q := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
		MPTypedFreeLarge(q)
	}
}

func TestMPTypedLargeSizes(t *testing.T) {
	// Various sizes around page boundaries.
	sizes := []uintptr{4096, 8192, 100 << 10, 1 << 20, 3<<20 + 123}
	for _, sz := range sizes {
		p := MPTypedAllocLarge(mpTypedNodeType, max(mpTypedNodeSize, sz))
		*(*byte)(p) = 1
		MPTypedFreeLarge(p)
	}
}

func TestMPTypedLargeConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				p := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
				n := (*mpTypedNode)(p)
				n.next = &mpTypedNode{value: int64(i)}
				n.value = int64(i)
				if n.next.value != int64(i) {
					t.Error("corrupted")
				}
				MPTypedFreeLarge(p)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkMPTypedLarge(b *testing.B) {
	for b.Loop() {
		p := MPTypedAllocLarge(mpTypedNodeType, mpTypedNodeSize)
		MPTypedFreeLarge(p)
	}
}

// mpLargeStress drives a high-concurrency, high-churn workload of large
// allocations: each worker keeps a small ring of live objects, evicting
// (freeing) the oldest on every iteration, and touches every page to
// force physical commit. alloc/free are supplied by the caller so the
// same workload can compare mpTypedAllocLarge against make.
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

func TestMPTypedLargeStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const workers = 32
	const dur = 2 * time.Second

	ops, peak, released := mpLargeStress(t, workers, dur,
		func(sz uintptr) unsafe.Pointer {
			return MPTypedAllocLarge(mpTypedNodeType, max(mpTypedNodeSize, sz))
		},
		mpStressWrapFree(MPTypedFreeLarge))
	t.Logf("typed:   %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops, float64(ops)/dur.Seconds(), float64(peak)/(1<<20), float64(released)/(1<<20))

	ops2, peak2, released2 := mpLargeStress(t, workers, dur,
		func(sz uintptr) unsafe.Pointer {
			b := make([]byte, sz)
			return unsafe.Pointer(unsafe.SliceData(b))
		},
		func(unsafe.Pointer, uintptr) {})
	t.Logf("make:    %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops2, float64(ops2)/dur.Seconds(), float64(peak2)/(1<<20), float64(released2)/(1<<20))

	ops4, peak4, released4 := mpLargeStress(t, workers, dur, mpStressPoolAlloc, mpStressPoolFree)
	t.Logf("sync.Pool: %d ops (%.0f ops/s), peak HeapInuse %.1fMB, HeapReleased after GC %.1fMB",
		ops4, float64(ops4)/dur.Seconds(), float64(peak4)/(1<<20), float64(released4)/(1<<20))
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
