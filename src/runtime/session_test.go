// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestSessionAlloc(t *testing.T) {
	sess := runtime.NewSession()
	if sess == nil {
		t.Fatal("NewSession returned nil")
	}

	p := sess.Alloc(128)
	if p == nil {
		t.Fatal("Session.Alloc returned nil")
	}
	if uintptr(p)%8 != 0 {
		t.Error("Session.Alloc returned unaligned pointer")
	}

	*(*uint64)(p) = 0xDEADBEEF
	if *(*uint64)(p) != 0xDEADBEEF {
		t.Error("Session memory readback failed")
	}

	sess.Close()
}

func TestSessionAllocMultipleBuckets(t *testing.T) {
	sess := runtime.NewSession()
	if sess == nil {
		t.Fatal("NewSession returned nil")
	}

	const allocSize = 8192
	const numAllocs = 32 // 256KB across multiple 64KB buckets

	var ptrs []uintptr
	for i := 0; i < numAllocs; i++ {
		p := sess.Alloc(allocSize)
		if p == nil {
			t.Fatalf("Session.Alloc returned nil at iteration %d", i)
		}
		*(*uint64)(p) = uint64(i)
		ptrs = append(ptrs, uintptr(p))
	}

	for i, up := range ptrs {
		if *(*uint64)(unsafe.Pointer(up)) != uint64(i) {
			t.Errorf("Session memory corruption at index %d: got %d, want %d",
				i, *(*uint64)(unsafe.Pointer(up)), i)
		}
	}

	sess.Close()
}

func TestSessionClose(t *testing.T) {
	sess := runtime.NewSession()
	if sess == nil {
		t.Fatal("NewSession returned nil")
	}

	p := sess.Alloc(64)
	*(*uint64)(p) = 42

	sess.Close()
	// No crash = pass.
}

func TestSessionDoubleClose(t *testing.T) {
	sess := runtime.NewSession()
	sess.Alloc(64)
	sess.Close()
	sess.Close() // second close should be safe (no-op)
}

func TestSessionAllocAfterClose(t *testing.T) {
	sess := runtime.NewSession()
	sess.Close()

	// Alloc after Close should return nil.
	p := sess.Alloc(64)
	if p != nil {
		t.Error("Session.Alloc after Close should return nil")
	}
}

func TestSessionZeroAlloc(t *testing.T) {
	sess := runtime.NewSession()
	if sess == nil {
		t.Fatal("NewSession returned nil")
	}

	p := sess.Alloc(0)
	if p != nil {
		t.Error("Session.Alloc(0) should return nil")
	}

	sess.Close()
}

func TestSessionAllocSizes(t *testing.T) {
	sizes := []uintptr{
		1, 2, 3, 4, 7, 8, 9, 15, 16, 17,
		31, 32, 33, 63, 64, 65, 127, 128, 129,
		255, 256, 257, 511, 512, 1023, 1024,
		4095, 4096, 8191, 8192,
	}

	for _, size := range sizes {
		sess := runtime.NewSession()
		p := sess.Alloc(size)
		if p == nil {
			t.Fatalf("Session.Alloc(%d) returned nil", size)
		}
		if uintptr(p)%8 != 0 {
			t.Errorf("Session.Alloc(%d) returned unaligned pointer %p", size, p)
		}
		// Write full size to verify memory is fully writable.
		for i := uintptr(0); i < size; i++ {
			*(*byte)(unsafe.Pointer(uintptr(p) + i)) = byte(i & 0xFF)
		}
		sess.Close()
	}
}

func TestSessionAllocAlignment(t *testing.T) {
	sess := runtime.NewSession()
	defer sess.Close()

	// 1 byte then 8 bytes → 8-byte gap (padding)
	p1 := sess.Alloc(1)
	p2 := sess.Alloc(8)
	diff := uintptr(p2) - uintptr(p1)
	if diff != 8 {
		t.Errorf("Expected 8 byte gap after 1-byte alloc, got %d", diff)
	}
	if uintptr(p2)%8 != 0 {
		t.Error("Second alloc not 8-byte aligned")
	}

	// 3 bytes then 8 bytes → 8-byte gap
	_ = sess.Alloc(3)
	p4 := sess.Alloc(8)
	if uintptr(p4)%8 != 0 {
		t.Error("Alloc after odd size not aligned")
	}

	// 9 bytes → 16 byte increment
	p5 := sess.Alloc(9)
	p6 := sess.Alloc(8)
	diff2 := uintptr(p6) - uintptr(p5)
	if diff2 != 16 {
		t.Errorf("Expected 16 byte gap after 9-byte alloc, got %d", diff2)
	}
}

func TestSessionEdgeBucket(t *testing.T) {
	sess := runtime.NewSession()
	defer sess.Close()

	// Allocate nearly a full bucket, then push over the edge.
	const bucketSize = 64 << 10 // 64KB, matches sessionBucketBytes
	p1 := sess.Alloc(bucketSize - 16)
	if p1 == nil {
		t.Fatal("Failed to allocate near-bucket-limit")
	}

	// This should trigger a new bucket.
	p2 := sess.Alloc(32)
	if p2 == nil {
		t.Fatal("Failed to allocate across bucket boundary")
	}

	// Both allocations should be valid in different buckets.
	*(*uint64)(p1) = 111
	*(*uint64)(p2) = 222
	if *(*uint64)(p1) != 111 || *(*uint64)(p2) != 222 {
		t.Error("Data corruption across bucket boundary")
	}
}

func TestSessionConcurrentAlloc(t *testing.T) {
	sess := runtime.NewSession()
	defer sess.Close()

	const numG = 8
	const numAllocs = 1000

	var wg sync.WaitGroup
	wg.Add(numG)
	for g := 0; g < numG; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < numAllocs; i++ {
				p := sess.Alloc(128)
				if p == nil {
					t.Errorf("goroutine %d: Alloc returned nil at iter %d", id, i)
					return
				}
				*(*uint64)(p) = uint64(id*numAllocs + i)
			}
		}(g)
	}
	wg.Wait()
}

func TestSessionConcurrentMultiSessions(t *testing.T) {
	const numSessions = 10

	var wg sync.WaitGroup
	wg.Add(numSessions)
	for i := 0; i < numSessions; i++ {
		go func(id int) {
			defer wg.Done()
			sess := runtime.NewSession()
			for j := 0; j < 100; j++ {
				p := sess.Alloc(64)
				*(*uint64)(p) = uint64(id*100 + j)
			}
			sess.Close()
		}(i)
	}
	wg.Wait()
}

func TestSessionGCInteraction(t *testing.T) {
	// Verify that GC doesn't crash or corrupt active session memory.
	sess := runtime.NewSession()
	defer sess.Close()

	p := sess.Alloc(128)
	*(*uint64)(p) = 0xBEEFCAFE

	// Run GC while session is active.
	runtime.GC()
	runtime.GC()

	if *(*uint64)(p) != 0xBEEFCAFE {
		t.Error("Active session memory corrupted by GC")
	}
}

func TestSessionCloseReleasesImmediately(t *testing.T) {
	sess := runtime.NewSession()
	sess.Alloc(sessionBucketBytes) // force span allocation
	sess.Close()
	// After Close, run GC to verify the freed span doesn't cause issues.
	runtime.GC()
	runtime.GC()
	// No crash = pass.
}

// TestSessionSessionToHeapPointer verifies that GC conservative root scanning
// correctly tracks session→heap pointers (session keeps heap objects alive),
// and that Close + GC doesn't crash (span freed correctly).
//
// Phase 1 verification: the conservative scan path and Close path must not
// cause GC panics, data corruption, or double-free. Verifying finalizer
// timing after Close requires deeper runtime integration (Phase 2).
func TestSessionSessionToHeapPointer(t *testing.T) {
	type testObj struct {
		value int64
		next  *testObj
	}

	// Create a chain of heap objects.
	obj := &testObj{value: 42}
	obj.next = &testObj{value: 43}
	obj.next.next = &testObj{value: 44}

	// Set finalizer on the first object to detect premature collection.
	finalizedEarly := make(chan struct{}, 1)
	runtime.SetFinalizer(obj.next.next, func(o *testObj) {
		finalizedEarly <- struct{}{}
	})

	sess := runtime.NewSession()
	// Allocate slot in session and store a pointer to the heap object chain.
	p := sess.Alloc(unsafe.Sizeof(unsafe.Pointer(nil)))
	if p == nil {
		t.Fatal("Failed to allocate slot for heap pointer")
	}
	*(*unsafe.Pointer)(p) = unsafe.Pointer(obj)

	// Drop the direct reference. Only session bucket points to the chain.
	obj = nil

	// GC while session is active: conservative scan must find session→obj.
	runtime.GC()
	runtime.GC()
	runtime.Gosched()

	select {
	case <-finalizedEarly:
		t.Error("Heap object collected while session is active — conservative root scan failed")
	default:
		// Expected: session bucket conservative scan keeps objects alive.
	}

	// Close the session. Span is freed via freeManual.
	sess.Close()

	// GC after close must not crash, double-free, or access freed span.
	runtime.GC()
	runtime.GC()
	runtime.Gosched()

	// No crash = pass. This verifies:
	// 1. markrootSessionSpans correctly scans active session spans.
	// 2. removeActiveSpan + freeManual works without leaving dangling refs.
	// 3. GC doesn't access freed mSpanSession spans.
}

// TestSessionConcurrentAllocClose stresses the race between Alloc and Close
// on the same Session. This verifies the mutex + closed double-check pattern
// correctly prevents use-after-close and double-free.
func TestSessionConcurrentAllocClose(t *testing.T) {
	const numWriters = 4
	const allocSize = 128

	var wg sync.WaitGroup
	var started sync.WaitGroup
	started.Add(numWriters)

	done := make(chan struct{})
	var allocsOk atomic.Uint64
	var allocsNil atomic.Uint64
	var closePanicked atomic.Bool
	var writerPanicked atomic.Bool

	sess := runtime.NewSession()

	wg.Add(numWriters)
	for i := 0; i < numWriters; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					writerPanicked.Store(true)
				}
			}()
			started.Done()
			for {
				select {
				case <-done:
					return
				default:
					p := sess.Alloc(allocSize)
					if p != nil {
						allocsOk.Add(1)
						*(*uint64)(p) = 0xCAFE
					} else {
						allocsNil.Add(1)
					}
				}
			}
		}()
	}

	// Wait for all writers to start allocating.
	started.Wait()

	// Give writers a head start to allocate some objects.
	time.Sleep(time.Millisecond)

	// Close while writers are still allocating.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				closePanicked.Store(true)
			}
		}()
		sess.Close()
	}()

	// Let Close race with allocations briefly.
	time.Sleep(10 * time.Millisecond)

	// Stop writers.
	close(done)
	wg.Wait()

	// Verify no panics in any goroutine.
	if closePanicked.Load() {
		t.Error("Close() panicked during concurrent access")
	}
	if writerPanicked.Load() {
		t.Error("Alloc() panicked during concurrent access")
	}

	// Verify some allocations succeeded.
	// Nil allocs are expected after Close wins the race.
	ok := allocsOk.Load()
	nilCount := allocsNil.Load()
	t.Logf("allocs: ok=%d nil=%d", ok, nilCount)
	if ok == 0 {
		t.Error("No allocations succeeded")
	}
}

// TestSessionManySessions stresses session creation and destruction
// to ensure no resource leaks over many cycles.
func TestSessionManySessions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	const numSessions = 10000
	const allocSize = 128

	for i := 0; i < numSessions; i++ {
		sess := runtime.NewSession()
		p := sess.Alloc(allocSize)
		if p == nil {
			t.Fatalf("Alloc returned nil at session %d", i)
		}
		*(*uint64)(p) = uint64(i)
		sess.Close()
	}

	// Run GC to verify no crash after releasing many sessions.
	runtime.GC()
	runtime.GC()
}

// TestSessionAllocHugeSize verifies that allocating more than one bucket size
// does not cause a crash, infinite loop, or memory corruption.
// Phase 1 does not support per-object sizes > bucket size, but it should
// fail gracefully (either return nil or the allocation works in a new bucket).
func TestSessionAllocHugeSize(t *testing.T) {
	sess := runtime.NewSession()
	defer sess.Close()

	// Allocate more than one bucket — this should be handled gracefully.
	hugeSize := uintptr(sessionBucketBytes + 1024)
	p := sess.Alloc(hugeSize)
	if p == nil {
		// nil is acceptable: Phase 1 doesn't support > bucket size
		t.Log("Alloc(>bucket) returned nil (expected Phase 1 behavior)")
		return
	}

	// If it returned non-nil, verify alignment and writability.
	if uintptr(p)%8 != 0 {
		t.Error("Huge alloc returned unaligned pointer")
	}
	// Write at start and end to verify memory is valid.
	*(*uint64)(p) = 0xABCD
	*(*uint64)(unsafe.Pointer(uintptr(p) + hugeSize - 8)) = 0xDCBA
}

// sessionBucketBytes mirrors the constant in runtime/session.go.
const sessionBucketBytes = 64 << 10
