// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package slicepool_test

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"slicepool"
)

func newBytePool() *slicepool.SlicePool[byte] {
	return &slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}
}

// ---------- byte slice tests ----------

func TestBasic(t *testing.T) {
	pool := newBytePool()

	s1 := pool.Get(1024)
	if cap(s1) < 1024 {
		t.Fatalf("Get(1024) returned cap=%d, want >= 1024", cap(s1))
	}
	pool.Put(s1)

	s2 := pool.Get(1024)
	if cap(s2) < 1024 {
		t.Fatalf("second Get(1024) returned cap=%d, want >= 1024", cap(s2))
	}
	pool.Put(s2)
}

func TestPutGetSameClass(t *testing.T) {
	pool := newBytePool()

	orig := pool.Get(1024)
	pool.Put(orig)

	got := pool.Get(1024)
	if cap(got) != cap(orig) {
		t.Fatalf("expected cap=%d, got cap=%d", cap(orig), cap(got))
	}
}

func TestNewNil(t *testing.T) {
	pool := &slicepool.SlicePool[byte]{}

	s := pool.Get(1024)
	if s != nil {
		t.Fatal("Get with nil New should return nil when pool is empty")
	}
}

func TestConcurrent(t *testing.T) {
	pool := newBytePool()

	var wg sync.WaitGroup
	n := 100
	errCh := make(chan error, n*1000)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				req := j%4096 + 16
				s := pool.Get(req)
				if cap(s) < req {
					errCh <- fmt.Errorf("Get(%d) returned cap=%d", req, cap(s))
					return
				}
				pool.Put(s)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestPutNil(t *testing.T) {
	pool := newBytePool()
	pool.Put(nil) // should not panic
}

func TestGetZero(t *testing.T) {
	pool := newBytePool()
	s := pool.Get(0)
	if cap(s) < 1 {
		t.Fatal("Get(0) should return cap >= 1")
	}
}

func TestReuseAcrossSizes(t *testing.T) {
	pool := newBytePool()

	for _, sz := range []int{16, 32, 64, 128, 256, 512, 1024, 2048} {
		s := pool.Get(sz)
		if cap(s) < sz {
			t.Fatalf("Get(%d) returned cap=%d", sz, cap(s))
		}
		pool.Put(s)
	}
}

func TestPutSmallDiscarded(t *testing.T) {
	pool := newBytePool()
	pool.Put(make([]byte, 8)) // cap < 16, silently discarded
}

func TestPutExternalSliceAccepted(t *testing.T) {
	// Externally-created slices (not via Get/New) should be accepted
	// and reused for matching-size requests.
	var called int
	pool := &slicepool.SlicePool[byte]{
		New: func(n int) []byte {
			called++
			return make([]byte, n)
		},
	}

	// Put an externally-created slice: cap=1024, class=6.
	pool.Put(make([]byte, 1024))

	// Get(1024) should find it in the pool; New should not be called.
	s := pool.Get(1024)
	if cap(s) < 1024 {
		t.Fatalf("Get(1024) returned cap=%d", cap(s))
	}
	if called != 0 {
		t.Fatalf("New called %d times, want 0 (external slice should be reused)", called)
	}
	pool.Put(s)
}

func TestPutExternalSliceTooSmallDiscarded(t *testing.T) {
	// An externally-created slice with a cap too small for the request
	// is discarded from the pool and New is called instead.
	var called int
	pool := &slicepool.SlicePool[byte]{
		New: func(n int) []byte {
			called++
			return make([]byte, n)
		},
	}

	// Put a slice with cap=100 (class=2, range 64-127).
	pool.Put(make([]byte, 100))

	// Get(200) maps to class=3 (128-255). The cap=100 slice is in class=2,
	// so it won't be found; New will be called.
	s := pool.Get(200)
	if cap(s) < 200 {
		t.Fatalf("Get(200) returned cap=%d", cap(s))
	}
	if called != 1 {
		t.Fatalf("New called %d times, want 1 (small slice in wrong class)", called)
	}
}

func TestGC(t *testing.T) {
	pool := newBytePool()

	// Put slices into the pool (any valid-class cap is accepted).
	for i := 0; i < 100; i++ {
		pool.Put(make([]byte, 1024))
	}

	runtime.GC()

	s := pool.Get(1024)
	if cap(s) < 1024 {
		t.Fatal("Get after GC should still work")
	}
}

func TestPutGetRepeatability(t *testing.T) {
	pool := newBytePool()

	// Get(500) allocates classMax(4) = 511. Put+Get should cycle in class 4.
	for i := 0; i < 10; i++ {
		s := pool.Get(500)
		if cap(s) < 500 {
			t.Fatalf("iteration %d: Get(500) returned cap=%d", i, cap(s))
		}
		pool.Put(s)
	}
}

func TestGetNegative(t *testing.T) {
	pool := newBytePool()
	s := pool.Get(-5)
	if cap(s) < 1 {
		t.Fatal("Get(-5) should return cap >= 1")
	}
}

func TestAllClassesPooling(t *testing.T) {
	// Verify each size class can Get/Put correctly.
	// For class 0: Get(16) → allocates classMax(0)=31 → Put back with cap=31.
	// Since classMax(i) maps back to the same class i, Get/Put should cycle.
	pool := newBytePool()
	for class := 0; class < 15; class++ {
		req := classMaxForTest(class)
		_ = req // not needed; Get(1..cap) will allocate classMax(class)
		s := pool.Get(classMaxForTest(class) - 1) // request one less than max
		if cap(s) < classMaxForTest(class)-1 {
			t.Fatalf("class %d: Get(%d) returned cap=%d", class, classMaxForTest(class)-1, cap(s))
		}
		if cap(s) != classMaxForTest(class) {
			t.Fatalf("class %d: expected cap=%d, got %d", class, classMaxForTest(class), cap(s))
		}
		pool.Put(s)

		s2 := pool.Get(classMaxForTest(class) - 1)
		if cap(s2) != classMaxForTest(class) {
			t.Fatalf("class %d: second Get expected cap=%d, got %d", class, classMaxForTest(class), cap(s2))
		}
		pool.Put(s2)
	}
}

// classMaxForTest returns the class max for testing using the same
// formula as the slicepool package.
func classMaxForTest(class int) int {
	const minCap = 16
	const numClasses = 16
	if class+1 < numClasses {
		return minCap<<(class+1) - 1
	}
	return minCap << class
}

// ---------- struct slice tests ----------

// Record is a moderate-size struct for testing SlicePool with non-byte types.
type Record struct {
	ID    int64
	Hash  [32]byte
	Value float64
	Count uint64
}

func newRecordPool() *slicepool.SlicePool[Record] {
	return &slicepool.SlicePool[Record]{
		New: func(n int) []Record { return make([]Record, n) },
	}
}

func TestStructSlice(t *testing.T) {
	pool := newRecordPool()

	s1 := pool.Get(64)
	if cap(s1) < 64 {
		t.Fatalf("Get(64) returned cap=%d, want >= 64", cap(s1))
	}
	pool.Put(s1)

	s2 := pool.Get(64)
	if cap(s2) < 64 {
		t.Fatalf("second Get(64) returned cap=%d, want >= 64", cap(s2))
	}
	pool.Put(s2)
}

func TestStructSliceConcurrent(t *testing.T) {
	pool := newRecordPool()

	var wg sync.WaitGroup
	n := 50
	errCh := make(chan error, n*200)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				req := j%512 + 16
				s := pool.Get(req)
				if cap(s) < req {
					errCh <- fmt.Errorf("Get(%d) returned cap=%d", req, cap(s))
					return
				}
				pool.Put(s)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestStructSliceDataPreserved(t *testing.T) {
	pool := &slicepool.SlicePool[Record]{
		New: func(n int) []Record { return make([]Record, n) },
	}

	// Get a slice, write data, Put, Get again — data should be preserved.
	s1 := pool.Get(16)
	s1 = s1[:1]
	s1[0] = Record{ID: 42, Hash: [32]byte{1, 2, 3}, Value: 3.14, Count: 99}
	pool.Put(s1)

	s2 := pool.Get(16)
	if s2[0].ID != 42 || s2[0].Count != 99 {
		t.Fatalf("data not preserved: got ID=%d Count=%d, want ID=42 Count=99", s2[0].ID, s2[0].Count)
	}
}

func TestStructSliceCustomNew(t *testing.T) {
	pool := &slicepool.SlicePool[Record]{
		New: func(n int) []Record {
			s := make([]Record, n)
			for i := range s {
				s[i].ID = -1 // sentinel: uninitialized
			}
			return s
		},
	}

	s := pool.Get(16)
	if len(s) > 0 && s[0].ID != -1 {
		t.Fatal("custom New did not initialize fields")
	}
	pool.Put(s)
}

func TestStructSlicePutExternalAccepted(t *testing.T) {
	// Externally-created struct slices should be accepted and reused.
	var called int
	pool := &slicepool.SlicePool[Record]{
		New: func(n int) []Record {
			called++
			return make([]Record, n)
		},
	}

	// Put an externally-created slice: cap=50, class=1 (32-63).
	pool.Put(make([]Record, 50))

	// Get(50) should find it in the pool.
	s := pool.Get(50)
	if cap(s) < 50 {
		t.Fatalf("Get(50) returned cap=%d", cap(s))
	}
	if called != 0 {
		t.Fatalf("New called %d times, want 0 (external slice should be reused)", called)
	}
}

func TestStructSliceTopClassNotPooled(t *testing.T) {
	// Top class (class 15, >=256K elements * sizeof(Record)=56 → >=14MB)
	// Since sizeof(Record)=56, getting 262145 elements is class 15 for the struct pool.
	// Actually let's test with byte pool where the boundary is clearer.
	pool := newBytePool()

	// Get with n that maps to top class (>=262144 for bytes).
	// This should allocate directly, not panic.
	s := pool.Get(262144)
	if cap(s) < 262144 {
		t.Fatalf("Get(262144) returned cap=%d", cap(s))
	}

	// Put should silently discard top class slices.
	pool.Put(s)

	// Another Get should again allocate (not find the discarded slice).
	s2 := pool.Get(262144)
	if cap(s2) < 262144 {
		t.Fatalf("second Get(262144) returned cap=%d", cap(s2))
	}
	pool.Put(s2)
}

func TestStructSliceAllClasses(t *testing.T) {
	pool := newRecordPool()
	// sizeof(Record) = 56 bytes.
	// For each non-top class, verify Get/Put round-trips correctly.
	for class := 0; class < 15; class++ {
		classMax := classMaxForTest(class)
		req := classMax - 1 // request just below class max
		s := pool.Get(req)
		if cap(s) != classMax {
			t.Fatalf("class %d: Get(%d) returned cap=%d, want %d", class, req, cap(s), classMax)
		}
		pool.Put(s)

		s2 := pool.Get(req)
		if cap(s2) != classMax {
			t.Fatalf("class %d: second Get(%d) returned cap=%d, want %d", class, req, cap(s2), classMax)
		}
		pool.Put(s2)
	}
}

func TestStructSliceMixedCaps(t *testing.T) {
	// When the pool returns a slice with cap < n, Get discards it and
	// allocates a new one via New.
	var called int
	pool := &slicepool.SlicePool[Record]{
		New: func(n int) []Record {
			called++
			return make([]Record, n)
		},
	}

	// Get(20): class 0, pool empty → New(31)
	s1 := pool.Get(20) // cap=31, called=1
	if cap(s1) < 20 {
		t.Fatalf("Get(20) returned cap=%d", cap(s1))
	}
	pool.Put(s1)
	if called != 1 {
		t.Fatalf("New called %d times after first Get, want 1", called)
	}

	// Put an external slice with cap=50 (class=1, 32-63).
	pool.Put(make([]Record, 50))

	// Get(55): class=1. Pool has cap=50. 50 < 55 → discard, allocate New(63).
	s2 := pool.Get(55)
	if cap(s2) < 55 {
		t.Fatalf("Get(55) returned cap=%d, want >= 55", cap(s2))
	}
	if called != 2 {
		t.Fatalf("New called %d times, want 2 (too-small slice discarded)", called)
	}
	pool.Put(s2)
}

func TestStructSliceNilNew(t *testing.T) {
	pool := &slicepool.SlicePool[Record]{}

	s := pool.Get(64)
	if s != nil {
		t.Fatal("Get with nil New should return nil when pool is empty")
	}
}
