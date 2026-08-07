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

func TestMallocFree(t *testing.T) {
	p := mempool.Malloc(100)
	if p == nil {
		t.Fatal("Malloc returned nil")
	}
	if mempool.UsableSize(p) < 100 {
		t.Fatalf("UsableSize = %d, want >= 100", mempool.UsableSize(p))
	}
	b := unsafe.Slice((*byte)(p), 100)
	for i := range b {
		b[i] = byte(i)
	}
	mempool.Free(p)

	q := mempool.Calloc(1, 100)
	for i, v := range unsafe.Slice((*byte)(q), 100) {
		if v != 0 {
			t.Fatalf("Calloc byte %d = %d, want 0", i, v)
		}
	}
	mempool.Free(q)
}

func TestRealloc(t *testing.T) {
	p := mempool.Malloc(32)
	for i := range unsafe.Slice((*byte)(p), 32) {
		unsafe.Slice((*byte)(p), 32)[i] = byte(i + 1)
	}
	q := mempool.Realloc(p, 1000)
	for i := 0; i < 32; i++ {
		if v := *(*byte)(unsafe.Add(q, i)); v != byte(i+1) {
			t.Fatalf("Realloc lost data at %d", i)
		}
	}
	mempool.Free(q)
}

func TestSpan(t *testing.T) {
	p := mempool.AllocSpan(100 << 10)
	b := unsafe.Slice((*byte)(p), 100<<10)
	if b[0] != 0 || b[len(b)-1] != 0 {
		t.Fatal("span not zeroed")
	}
	b[0], b[len(b)-1] = 1, 1
	mempool.FreeSpan(p)
}

type node struct {
	next *node
	val  int64
	pad  [64 << 10]byte
}

func TestSpanTyped(t *testing.T) {
	n := mempool.AllocSpanTyped[node]()
	if n == nil {
		t.Fatal("AllocSpanTyped returned nil")
	}
	if n.next != nil || n.val != 0 {
		t.Fatal("typed span not zeroed")
	}
	n.next = &node{val: 7}
	// The object is GC-scanned: the pointer target must survive GC.
	runtime.GC()
	if n.next == nil || n.next.val != 7 {
		t.Fatal("pointer target collected: typed span not scanned")
	}
	mempool.FreeSpanTyped(n)
}

func TestConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				p := mempool.Malloc(1 + uintptr(i%512))
				*(*byte)(p) = 1
				mempool.Free(p)
			}
			for i := 0; i < 50; i++ {
				p := mempool.AllocSpan(64 << 10)
				*(*byte)(p) = 1
				mempool.FreeSpan(p)
			}
		}()
	}
	wg.Wait()
}

type ptrless struct {
	val int64
	pad [64 << 10]byte
}

func TestSpanTypedPtrless(t *testing.T) {
	// 无指针类型应自动走 noscan 路径，功能等价。
	n := mempool.AllocSpanTyped[ptrless]()
	if n == nil {
		t.Fatal("AllocSpanTyped returned nil")
	}
	if n.val != 0 {
		t.Fatal("not zeroed")
	}
	n.val = 99
	runtime.GC()
	if n.val != 99 {
		t.Fatal("data corrupted after GC")
	}
	mempool.FreeSpanTyped(n)
}
