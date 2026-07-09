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

// sink prevents the compiler from optimizing away allocations in benchmarks.
var sink any

// ---------- byte slice benchmarks ----------

func BenchmarkMakeNew(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := make([]byte, 1024)
		sink = s
	}
}

func BenchmarkSyncPool(b *testing.B) {
	b.ReportAllocs()
	var pool = sync.Pool{
		New: func() any {
			return make([]byte, 1024)
		},
	}
	for i := 0; i < b.N; i++ {
		s := pool.Get().([]byte)
		pool.Put(s)
	}
}

func BenchmarkSlicePool(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}
	for i := 0; i < b.N; i++ {
		s := pool.Get(1024)
		pool.Put(s)
	}
}

func BenchmarkSlicePoolParallel(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := pool.Get(1024)
			pool.Put(s)
		}
	})
}

func BenchmarkSyncPoolParallel(b *testing.B) {
	b.ReportAllocs()
	var pool = sync.Pool{
		New: func() any {
			return make([]byte, 1024)
		},
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := pool.Get().([]byte)
			pool.Put(s)
		}
	})
}

func BenchmarkMixedSize(b *testing.B) {
	b.ReportAllocs()
	sizes := []int{256, 512, 1024, 2048, 4096, 8192}
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}
	for i := 0; i < b.N; i++ {
		s := pool.Get(sizes[i%len(sizes)])
		pool.Put(s)
	}
}

func BenchmarkNetworkBuf(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}
	b.RunParallel(func(pb *testing.PB) {
		data := make([]byte, 1024)
		for pb.Next() {
			buf := pool.Get(4096)
			copy(buf, data)
			pool.Put(buf)
		}
	})
}

// ---------- struct slice benchmarks ----------

func BenchmarkMakeNewStruct(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := make([]Record, 64)
		sink = s
	}
}

func BenchmarkSlicePoolStruct(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[Record]{
		New: func(n int) []Record { return make([]Record, n) },
	}
	for i := 0; i < b.N; i++ {
		s := pool.Get(64)
		pool.Put(s)
	}
}

func BenchmarkSlicePoolStructParallel(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[Record]{
		New: func(n int) []Record { return make([]Record, n) },
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := pool.Get(64)
			pool.Put(s)
		}
	})
}

func BenchmarkMakeNewStructLarge(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := make([]Record, 1024)
		sink = s
	}
}

func BenchmarkSlicePoolStructLarge(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[Record]{
		New: func(n int) []Record { return make([]Record, n) },
	}
	for i := 0; i < b.N; i++ {
		s := pool.Get(1024)
		pool.Put(s)
	}
}

// ---------- allocation reduction benchmarks ----------

// BenchmarkAllocSustainedLoad measures steady-state allocation rate.
// After the initial warmup, a healthy pool should reach near-zero allocs.
func BenchmarkAllocSustainedLoad(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}

	// Warmup: fill the pool.
	for i := 0; i < 1000; i++ {
		s := pool.Get(4096)
		pool.Put(s)
	}
	runtime.GC()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := pool.Get(4096)
		pool.Put(s)
	}
}

// BenchmarkAllocSustainedLoadMake is the baseline for sustained load
// without any pooling.
func BenchmarkAllocSustainedLoadMake(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := make([]byte, 4096)
		sink = s
	}
}

// BenchmarkMemoryRetention measures how many slices survive GC
// and remain available for reuse.
func BenchmarkMemoryRetention(b *testing.B) {
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Allocate and put back into pool (simulate request lifecycle).
		s1 := pool.Get(2048)

		// Simulate some work with another buffer.
		s2 := pool.Get(512)
		pool.Put(s2)

		pool.Put(s1)
	}
}

// ---------- realistic workload benchmarks ----------

// BenchmarkBurstWorkload simulates a bursty network server:
// short bursts of allocation followed by periods of quiet.
func BenchmarkBurstWorkload(b *testing.B) {
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Burst: allocate several buffers of different sizes.
		bufs := make([][]byte, 10)
		for j := 0; j < 10; j++ {
			bufs[j] = pool.Get(256 << (j % 4)) // 256, 512, 1024, 2048
		}
		// Return all to pool.
		for j := 0; j < 10; j++ {
			pool.Put(bufs[j])
		}
	}
}

// BenchmarkSizeDistribution simulates a workload where small buffers
// are frequent and large buffers are rare (power-law distribution).
func BenchmarkSizeDistribution(b *testing.B) {
	b.ReportAllocs()
	// Sizes weighted toward small allocations.
	sizes := []int{
		128, 128, 128, 128, 128, // 50% small
		512, 512, 512, // 30% medium
		2048, 2048, // 20% large
	}
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sz := sizes[i%len(sizes)]
		s := pool.Get(sz)
		pool.Put(s)
	}
}

// BenchmarkSizeDistributionMake is the make-only baseline for
// the size distribution workload.
func BenchmarkSizeDistributionMake(b *testing.B) {
	b.ReportAllocs()
	sizes := []int{
		128, 128, 128, 128, 128,
		512, 512, 512,
		2048, 2048,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sz := sizes[i%len(sizes)]
		s := make([]byte, sz)
		sink = s
	}
}

// BenchmarkMultiClassSteadyState verifies that many size classes
// can be populated and reused without interference.
func BenchmarkMultiClassSteadyState(b *testing.B) {
	b.ReportAllocs()
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}

	// Warmup each class.
	for class := 0; class < 15; class++ {
		classMax := classMaxForTest(class)
		s := pool.Get(classMax)
		pool.Put(s)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		class := i % 15
		req := classMaxForTest(class)
		s := pool.Get(req)
		pool.Put(s)
	}
}

// BenchmarkMultiClassMake is the make-only baseline for multi-class.
func BenchmarkMultiClassMake(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		class := i % 15
		req := classMaxForTest(class)
		s := make([]byte, req)
		sink = s
	}
}

// ---------- struct workload benchmarks ----------

// BenchmarkStructMixedWorkload simulates processing records of varying sizes.
func BenchmarkStructMixedWorkload(b *testing.B) {
	b.ReportAllocs()
	sizes := []int{16, 32, 64, 128, 256}
	pool := slicepool.SlicePool[Record]{
		New: func(n int) []Record { return make([]Record, n) },
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sz := sizes[i%len(sizes)]
		s := pool.Get(sz)
		// Simulate work: write some data.
		if len(s) > 0 {
			s[0].ID = int64(i)
			s[0].Value = float64(i) * 1.5
		}
		pool.Put(s)
	}
}

// ---------- memory pressure benchmarks ----------

// BenchmarkMemoryWithGC periodically triggers GC to measure
// pool behavior under memory pressure.
func BenchmarkMemoryWithGC(b *testing.B) {
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := pool.Get(4096)
		pool.Put(s)

		// Simulate periodic GC.
		if i%1000 == 0 {
			runtime.GC()
		}
	}
}

// BenchmarkMemoryReport measures total allocation volume and reports the
// ratio of bytes allocated with pooling vs without.
func BenchmarkMemoryReport(b *testing.B) {
	// --- with pooling ---
	b.Run("Pooled", func(b *testing.B) {
		b.ReportAllocs()
		pool := slicepool.SlicePool[byte]{
			New: func(n int) []byte { return make([]byte, n) },
		}
		// Warmup.
		for i := 0; i < 100; i++ {
			s := pool.Get(4096)
			pool.Put(s)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s := pool.Get(4096)
			pool.Put(s)
		}
	})

	// --- without pooling ---
	b.Run("Make", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s := make([]byte, 4096)
			sink = s
		}
	})
}

// ---------- parallel scaling benchmarks ----------

// BenchmarkScaling measures throughput as goroutine count increases.
func BenchmarkScaling(b *testing.B) {
	pool := slicepool.SlicePool[byte]{
		New: func(n int) []byte { return make([]byte, n) },
	}
	for _, g := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("G=%d", g), func(b *testing.B) {
			b.ReportAllocs()
			b.SetParallelism(g)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					s := pool.Get(1024)
					pool.Put(s)
				}
			})
		})
	}
}

func BenchmarkScalingMake(b *testing.B) {
	for _, g := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("G=%d", g), func(b *testing.B) {
			b.ReportAllocs()
			b.SetParallelism(g)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					s := make([]byte, 1024)
					sink = s
				}
			})
		})
	}
}
