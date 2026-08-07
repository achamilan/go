// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	. "runtime"
	"sync"
	"testing"
	"unsafe"
)

func TestMPMallocFree(t *testing.T) {
	p := MPMalloc(100)
	if p == nil {
		t.Fatal("MPMalloc returned nil")
	}
	if MPUsableSize(p) < 100 {
		t.Fatalf("MPUsableSize = %d, want >= 100", MPUsableSize(p))
	}
	b := unsafe.Slice((*byte)(p), 100)
	for i := range b {
		b[i] = byte(i)
	}
	MPFree(p)

	// Same class should reuse the same block (per-P LIFO).
	p2 := MPMalloc(100)
	if p2 != p {
		t.Logf("note: reused block not identical (p=%p p2=%p)", p, p2)
	}
	MPFree(p2)
}

func TestMPCallocZeroed(t *testing.T) {
	p := MPMalloc(64)
	for _, i := range unsafe.Slice((*byte)(p), 64) {
		_ = i
	}
	unsafe.Slice((*byte)(p), 64)[10] = 0xAB
	MPFree(p)
	q := MPCalloc(1, 64)
	for i, v := range unsafe.Slice((*byte)(q), 64) {
		if v != 0 {
			t.Fatalf("byte %d = %#x, want 0", i, v)
		}
	}
	MPFree(q)
}

func TestMPRealloc(t *testing.T) {
	p := MPMalloc(32)
	for i := range unsafe.Slice((*byte)(p), 32) {
		unsafe.Slice((*byte)(p), 32)[i] = byte(i + 1)
	}
	q := MPRealloc(p, 1000)
	for i := 0; i < 32; i++ {
		if v := *(*byte)(unsafe.Add(q, i)); v != byte(i+1) {
			t.Fatalf("grow lost data at %d: got %d want %d", i, v, i+1)
		}
	}
	r := MPRealloc(q, 16)
	for i := 0; i < 16; i++ {
		if v := *(*byte)(unsafe.Add(r, i)); v != byte(i+1) {
			t.Fatalf("shrink lost data at %d", i)
		}
	}
	MPFree(r)

	s := MPRealloc(nil, 48)
	if s == nil || MPUsableSize(s) < 48 {
		t.Fatal("MPRealloc(nil) failed")
	}
	MPFree(s)
}

func TestMPLarge(t *testing.T) {
	p := MPMalloc(100 << 10) // 128KB bucket
	MPFree(p)
	p2 := MPMalloc(100 << 10)
	if p2 != p {
		t.Fatal("large block not reused from bucket cache")
	}
	MPFree(p2)

	q := MPMalloc(8 << 20) // above 4MB: uncached, dropped on free
	unsafe.Slice((*byte)(q), 8<<20)[(8<<20)-1] = 1
	MPFree(q)
}

func TestMPFreeNil(t *testing.T) {
	MPFree(nil)
}

func TestMPConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				p := MPMalloc(1 + uintptr(i%512))
				*(*byte)(p) = 1
				MPFree(p)
			}
		}()
	}
	wg.Wait()
}

func TestMPChunkScavenge(t *testing.T) {
	const n = 128
	ptrs := make([]unsafe.Pointer, n)
	for i := range ptrs {
		ptrs[i] = MPMalloc(100)
	}
	before := MPChunkCount()
	if before == 0 {
		t.Fatal("expected at least one chunk")
	}
	for _, p := range ptrs {
		MPFree(p)
	}
	// Per-P caches hold residual blocks; gcStart flushes them via
	// mpFlushAll, after which fully idle chunks are reclaimed.
	GC()
	if after := MPChunkCount(); after >= before {
		t.Fatalf("chunk not reclaimed: before=%d after=%d", before, after)
	}
}

func TestMPGCSurvival(t *testing.T) {
	// Blocks must stay alive and intact across GC while referenced.
	p := MPMalloc(256)
	for i := range unsafe.Slice((*byte)(p), 256) {
		unsafe.Slice((*byte)(p), 256)[i] = byte(i)
	}
	GC()
	GC()
	for i := 0; i < 256; i++ {
		if v := *(*byte)(unsafe.Add(p, i)); v != byte(i) {
			t.Fatalf("block corrupted after GC at %d: got %d", i, v)
		}
	}
	MPFree(p)
}

func BenchmarkMPMallocFree(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p := MPMalloc(128)
			MPFree(p)
		}
	})
}

func BenchmarkMPMallocFreeSizes(b *testing.B) {
	sizes := []uintptr{16, 64, 256, 1024, 4096, 16384}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			p := MPMalloc(sizes[i%len(sizes)])
			*(*byte)(p) = 1
			MPFree(p)
			i++
		}
	})
}
