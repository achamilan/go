// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mempool_test

import (
	"mempool"
	"runtime"
	"sync"
	"testing"
	"unsafe"
)

func TestSpan(t *testing.T) {
	for _, sz := range []uintptr{1025, 4096, 100 << 10, 2 << 20} {
		p := mempool.AllocSpan(sz)
		if p == nil {
			t.Fatalf("size %d: nil", sz)
		}
		b := unsafe.Slice((*byte)(p), sz)
		if b[0] != 0 || b[sz-1] != 0 {
			t.Fatalf("size %d: span not zeroed", sz)
		}
		b[0], b[sz-1] = 1, 1
		mempool.FreeSpan(p)
	}
}

func TestSpanGCSurvival(t *testing.T) {
	p := mempool.AllocSpan(64 << 10)
	for i := range unsafe.Slice((*byte)(p), 64<<10) {
		unsafe.Slice((*byte)(p), 64<<10)[i] = byte(i)
	}
	runtime.GC()
	runtime.GC()
	for i := 0; i < 64<<10; i += 4096 {
		if v := *(*byte)(unsafe.Add(p, i)); v != byte(i) {
			t.Fatalf("data corrupted after GC at %d", i)
		}
	}
	mempool.FreeSpan(p)
}

func TestConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				sz := uintptr(4096 + (g*100+i)%200*4096)
				p := mempool.AllocSpan(sz)
				*(*byte)(p) = byte(g)
				*(*byte)(unsafe.Add(p, sz-1)) = byte(i)
				mempool.FreeSpan(p)
			}
		}(g)
	}
	wg.Wait()
}

type ptrlessObj struct {
	magic uint32
	val   int64
	pad   [64 << 10]byte
}

type withPtr struct {
	next *withPtr
	pad  [64 << 10]byte
}

func TestAllocObject(t *testing.T) {
	o := mempool.AllocObject[ptrlessObj]()
	if o == nil {
		t.Fatal("AllocObject returned nil")
	}
	if o.magic != 0 || o.val != 0 {
		t.Fatal("not zeroed")
	}
	o.magic = 0xDEADBEEF
	runtime.GC()
	if o.magic != 0xDEADBEEF {
		t.Fatal("data corrupted after GC")
	}
	mempool.FreeObject(o)
}

func TestAllocObjectRejectsPointers(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for pointer-containing type")
		}
	}()
	mempool.AllocObject[withPtr]()
}
