// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

func init() {
	register("GCFairness", GCFairness)
	register("GCFairness2", GCFairness2)
	register("GCSys", GCSys)
	register("GCPhys", GCPhys)
	register("DeferLiveness", DeferLiveness)
	register("GCZombie", GCZombie)
	register("GCMemoryLimit", GCMemoryLimit)
	register("GCMemoryLimitNoGCPercent", GCMemoryLimitNoGCPercent)
	register("GCDeadTrace", GCDeadTrace)
	register("GCDeadTraceComplex", GCDeadTraceComplex)
	register("GCDeadTraceSession", GCDeadTraceSession)
	register("GCDeadTraceFullyDead", GCDeadTraceFullyDead)
	register("GCDeadTraceMultiSession", GCDeadTraceMultiSession)
	register("GCDeadTraceBucketOvercount", GCDeadTraceBucketOvercount)
	register("GCDeadTraceBucketOvercountConcurrent", GCDeadTraceBucketOvercountConcurrent)
}

func GCSys() {
	runtime.GOMAXPROCS(1)
	memstats := new(runtime.MemStats)
	runtime.GC()
	runtime.ReadMemStats(memstats)
	sys := memstats.Sys

	runtime.MemProfileRate = 0 // disable profiler

	itercount := 100000
	for i := 0; i < itercount; i++ {
		workthegc()
	}

	// Should only be using a few MB.
	// We allocated 100 MB or (if not short) 1 GB.
	runtime.ReadMemStats(memstats)
	if sys > memstats.Sys {
		sys = 0
	} else {
		sys = memstats.Sys - sys
	}
	if sys > 16<<20 {
		fmt.Printf("using too much memory: %d bytes\n", sys)
		return
	}
	fmt.Printf("OK\n")
}

var sink []byte

func workthegc() []byte {
	sink = make([]byte, 1029)
	return sink
}

func GCFairness() {
	runtime.GOMAXPROCS(1)
	f, err := os.Open("/dev/null")
	if os.IsNotExist(err) {
		// This test tests what it is intended to test only if writes are fast.
		// If there is no /dev/null, we just don't execute the test.
		fmt.Println("OK")
		return
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	for i := 0; i < 2; i++ {
		go func() {
			for {
				f.Write([]byte("."))
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	fmt.Println("OK")
}

func GCFairness2() {
	// Make sure user code can't exploit the GC's high priority
	// scheduling to make scheduling of user code unfair. See
	// issue #15706.
	runtime.GOMAXPROCS(1)
	debug.SetGCPercent(1)
	var count [3]int64
	var sink [3]any
	for i := range count {
		go func(i int) {
			for {
				sink[i] = make([]byte, 1024)
				atomic.AddInt64(&count[i], 1)
			}
		}(i)
	}
	// Note: If the unfairness is really bad, it may not even get
	// past the sleep.
	//
	// If the scheduling rules change, this may not be enough time
	// to let all goroutines run, but for now we cycle through
	// them rapidly.
	//
	// OpenBSD's scheduler makes every usleep() take at least
	// 20ms, so we need a long time to ensure all goroutines have
	// run. If they haven't run after 30ms, give it another 1000ms
	// and check again.
	time.Sleep(30 * time.Millisecond)
	var fail bool
	for i := range count {
		if atomic.LoadInt64(&count[i]) == 0 {
			fail = true
		}
	}
	if fail {
		time.Sleep(1 * time.Second)
		for i := range count {
			if atomic.LoadInt64(&count[i]) == 0 {
				fmt.Printf("goroutine %d did not run\n", i)
				return
			}
		}
	}
	fmt.Println("OK")
}

func GCPhys() {
	// This test ensures that heap-growth scavenging is working as intended.
	//
	// It attempts to construct a sizeable "swiss cheese" heap, with many
	// allocChunk-sized holes. Then, it triggers a heap growth by trying to
	// allocate as much memory as would fit in those holes.
	//
	// The heap growth should cause a large number of those holes to be
	// returned to the OS.

	const (
		// The total amount of memory we're willing to allocate.
		allocTotal = 32 << 20

		// The page cache could hide 64 8-KiB pages from the scavenger today.
		maxPageCache = (8 << 10) * 64
	)

	// How big the allocations are needs to depend on the page size.
	// If the page size is too big and the allocations are too small,
	// they might not be aligned to the physical page size, so the scavenger
	// will gloss over them.
	pageSize := os.Getpagesize()
	var allocChunk int
	if pageSize <= 8<<10 {
		allocChunk = 64 << 10
	} else {
		allocChunk = 512 << 10
	}
	allocs := allocTotal / allocChunk

	// Set GC percent just so this test is a little more consistent in the
	// face of varying environments.
	debug.SetGCPercent(100)

	// Set GOMAXPROCS to 1 to minimize the amount of memory held in the page cache,
	// and to reduce the chance that the background scavenger gets scheduled.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	// Allocate allocTotal bytes of memory in allocChunk byte chunks.
	// Alternate between whether the chunk will be held live or will be
	// condemned to GC to create holes in the heap.
	saved := make([][]byte, allocs/2+1)
	condemned := make([][]byte, allocs/2)
	for i := 0; i < allocs; i++ {
		b := make([]byte, allocChunk)
		if i%2 == 0 {
			saved = append(saved, b)
		} else {
			condemned = append(condemned, b)
		}
	}

	// Run a GC cycle just so we're at a consistent state.
	runtime.GC()

	// Drop the only reference to all the condemned memory.
	condemned = nil

	// Clear the condemned memory.
	runtime.GC()

	// At this point, the background scavenger is likely running
	// and could pick up the work, so the next line of code doesn't
	// end up doing anything. That's fine. What's important is that
	// this test fails somewhat regularly if the runtime doesn't
	// scavenge on heap growth, and doesn't fail at all otherwise.

	// Make a large allocation that in theory could fit, but won't
	// because we turned the heap into swiss cheese.
	saved = append(saved, make([]byte, allocTotal/2))

	// heapBacked is an estimate of the amount of physical memory used by
	// this test. HeapSys is an estimate of the size of the mapped virtual
	// address space (which may or may not be backed by physical pages)
	// whereas HeapReleased is an estimate of the amount of bytes returned
	// to the OS. Their difference then roughly corresponds to the amount
	// of virtual address space that is backed by physical pages.
	//
	// heapBacked also subtracts out maxPageCache bytes of memory because
	// this is memory that may be hidden from the scavenger per-P. Since
	// GOMAXPROCS=1 here, subtracting it out once is fine.
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	heapBacked := stats.HeapSys - stats.HeapReleased - maxPageCache
	// If heapBacked does not exceed the heap goal by more than retainExtraPercent
	// then the scavenger is working as expected; the newly-created holes have been
	// scavenged immediately as part of the allocations which cannot fit in the holes.
	//
	// Since the runtime should scavenge the entirety of the remaining holes,
	// theoretically there should be no more free and unscavenged memory. However due
	// to other allocations that happen during this test we may still see some physical
	// memory over-use.
	overuse := (float64(heapBacked) - float64(stats.HeapAlloc)) / float64(stats.HeapAlloc)
	// Check against our overuse threshold, which is what the scavenger always reserves
	// to encourage allocation of memory that doesn't need to be faulted in.
	//
	// Add additional slack in case the page size is large and the scavenger
	// can't reach that memory because it doesn't constitute a complete aligned
	// physical page. Assume the worst case: a full physical page out of each
	// allocation.
	threshold := 0.1 + float64(pageSize)/float64(allocChunk)
	if overuse <= threshold {
		fmt.Println("OK")
		return
	}
	// Physical memory utilization exceeds the threshold, so heap-growth scavenging
	// did not operate as expected.
	//
	// In the context of this test, this indicates a large amount of
	// fragmentation with physical pages that are otherwise unused but not
	// returned to the OS.
	fmt.Printf("exceeded physical memory overuse threshold of %3.2f%%: %3.2f%%\n"+
		"(alloc: %d, goal: %d, sys: %d, rel: %d, objs: %d)\n", threshold*100, overuse*100,
		stats.HeapAlloc, stats.NextGC, stats.HeapSys, stats.HeapReleased, len(saved))
	runtime.KeepAlive(saved)
	runtime.KeepAlive(condemned)
}

// Test that defer closure is correctly scanned when the stack is scanned.
func DeferLiveness() {
	var x [10]int
	escape(&x)
	fn := func() {
		if x[0] != 42 {
			panic("FAIL")
		}
	}
	defer fn()

	x[0] = 42
	runtime.GC()
	runtime.GC()
	runtime.GC()
}

//go:noinline
func escape(x any) { sink2 = x; sink2 = nil }

var sink2 any

// Test zombie object detection and reporting.
func GCZombie() {
	// Allocate several objects of unusual size (so free slots are
	// unlikely to all be re-allocated by the runtime).
	const size = 190
	const count = 8192 / size
	keep := make([]*byte, 0, (count+1)/2)
	free := make([]uintptr, 0, (count+1)/2)
	zombies := make([]*byte, 0, len(free))
	for i := 0; i < count; i++ {
		obj := make([]byte, size)
		p := &obj[0]
		if i%2 == 0 {
			keep = append(keep, p)
		} else {
			free = append(free, uintptr(unsafe.Pointer(p)))
		}
	}

	// Free the unreferenced objects.
	runtime.GC()

	// Bring the free objects back to life.
	for _, p := range free {
		zombies = append(zombies, (*byte)(unsafe.Pointer(p)))
	}

	// GC should detect the zombie objects.
	runtime.GC()
	println("failed")
	runtime.KeepAlive(keep)
	runtime.KeepAlive(zombies)
}

func GCMemoryLimit() {
	gcMemoryLimit(100)
}

func GCMemoryLimitNoGCPercent() {
	gcMemoryLimit(-1)
}

var deadSink []byte

// gcDeadSession sinks for GCDeadTraceSession test.
// Package-level to ensure heap allocation via escape analysis.
var (
	gcDeadPreSink     []byte // alloc on main goroutine, NOT in session, will die
	gcDeadSessionSink []byte // alloc on goroutine A, WITH session, will die
	gcDeadOtherSink   []byte // alloc on goroutine B, NOT in session, will die
	gcDeadAliveSink   []byte // alloc on goroutine A, WITH session, stays alive

	// gcDeadSessionMulti sinks for GCDeadTraceMultiSession test.
	gcDeadMultiA1 []byte // session A alloc, will die
	gcDeadMultiA2 []byte // session A alloc, stays alive
	gcDeadMultiB1 []byte // session B alloc, will die
	gcDeadMultiB2 []byte // session B alloc, stays alive

	// gcDeadBucketOvercountConcurrent sinks for GCDeadTraceBucketOvercountConcurrent test.
	gcDeadConcSessA1 []byte // session A alloc, will die (same bucket as non-session)
	gcDeadConcSessA2 []byte // session A alloc, stays alive
	gcDeadConcSessB1 []byte // session B alloc, will die (same bucket as non-session)
)

func GCDeadTrace() {
	runtime.MemProfileRate = 1 // profile every allocation

	runtime.GcDeadSessionStart(1)

	// Allocate garbage that escapes to the heap via the global deadSink.
	// Previous iteration's value becomes unreferenced and dies.
	for i := 0; i < 500; i++ {
		deadSink = make([]byte, 256)
	}
	deadSink = nil // last iteration's allocation also dies

	runtime.GcDeadSessionEnd(1)

	live := make([]byte, 1024) // keep one allocation live
	runtime.GC()               // force GC (sync sweep)
	_ = live

	fmt.Println("OK")
}

// --- Realistic allocation test with actual object usage ---

// DataBlock is a linked-list node with a byte buffer payload.
// Its fields are actually read and written during processing.
type DataBlock struct {
	id   uint64
	_    [4]byte // padding
	size int
	buf  []byte
	next *DataBlock
}

var globalLiveBlocks *DataBlock
var globalLiveCache []byte

//go:noinline
func createDataBlock(id uint64, size int) *DataBlock {
	b := new(DataBlock)
	b.id = id
	b.size = size
	b.buf = make([]byte, size)
	// Write data into the buffer — actual use of the allocation.
	for i := range b.buf {
		b.buf[i] = byte(id) ^ byte(i)
	}
	return b
}

//go:noinline
func buildBlockList(n int, size int) *DataBlock {
	var head *DataBlock
	for i := 0; i < n; i++ {
		b := createDataBlock(uint64(i), size)
		b.next = head
		head = b
	}
	return head // returns list to caller (escape)
}

//go:noinline
func checksumBlockList(head *DataBlock) uint64 {
	var sum uint64
	for b := head; b != nil; b = b.next {
		// Read from the buffer — actual use of allocated memory.
		for _, v := range b.buf {
			sum += uint64(v)
		}
	}
	return sum
}

//go:noinline
func processTempBuffer(size int) uint64 {
	buf := make([]byte, size)
	// Fill with data.
	for i := range buf {
		buf[i] = byte(i*7 + 3)
	}
	// Read back and compute checksum.
	var sum uint64
	for _, v := range buf {
		sum += uint64(v)
	}
	return sum
}

// Helper functions that call buildBlockList from different callers,
// so gcdeadtrace can distinguish which caller produced the dead objects.

//go:noinline
func makeDeadSmallBlocks() *DataBlock {
	return buildBlockList(150, 64) // 150 nodes × 64-byte buffers → dies
}

//go:noinline
func makeDeadLargeBlocks() *DataBlock {
	return buildBlockList(80, 512) // 80 nodes × 512-byte buffers → dies
}

func GCDeadTraceComplex() {
	runtime.MemProfileRate = 1

	runtime.GcDeadSessionStart(2)

	// Phase 1: Live blocks — stored in global, will appear in gcdeadsession:alive.
	// Same allocation function (buildBlockList), different caller.
	globalLiveBlocks = buildBlockList(30, 256) // 30 nodes × 256-byte buffers → alive

	// Phase 2: Dead blocks from two different callers.
	// Both call buildBlockList → createDataBlock, but gcdeadtrace can
	// distinguish them by the caller context.
	deadSmall := makeDeadSmallBlocks() // caller: makeDeadSmallBlocks → will die
	deadLarge := makeDeadLargeBlocks() // caller: makeDeadLargeBlocks → will die

	// Phase 3: Actually use the allocated data.
	cs1 := checksumBlockList(deadSmall)
	cs2 := checksumBlockList(deadLarge)
	_ = cs1
	_ = cs2

	// Phase 4: Temp buffer used and released → will die.
	_ = processTempBuffer(4096)

	// Phase 5: Global cache kept alive → will appear in gcdeadsession:alive.
	globalLiveCache = make([]byte, 1024)
	for i := range globalLiveCache {
		globalLiveCache[i] = 0xAB
	}

	runtime.GcDeadSessionEnd(2)

	// Phase 6: Release dead objects.
	deadSmall = nil
	deadLarge = nil

	runtime.GC()

	fmt.Println("OK")
}

func GCDeadTraceSession() {
	runtime.MemProfileRate = 1

	var wg sync.WaitGroup

	// Phase 1: Main goroutine allocates (NOT in session → NOT in gcdeadsession).
	gcDeadPreSink = make([]byte, 128)

	// Phase 2: Session goroutine A:
	//   - allocates 256B that will die (nil'd before GC)
	//   - allocates 1024B that will stay alive (kept via gcDeadAliveSink)
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.GcDeadSessionStart(101)
		gcDeadSessionSink = make([]byte, 256)  // will die
		gcDeadAliveSink = make([]byte, 1024)   // will stay alive
		runtime.GcDeadSessionEnd(101)
	}()
	wg.Wait()

	// Phase 3: Non-session goroutine B (NOT in gcdeadsession).
	wg.Add(1)
	go func() {
		defer wg.Done()
		gcDeadOtherSink = make([]byte, 512)
	}()
	wg.Wait()

	// Drop dying references. gcDeadAliveSink is kept alive.
	gcDeadPreSink = nil
	gcDeadSessionSink = nil
	gcDeadOtherSink = nil

	runtime.GC()
	fmt.Println("OK")
}

// GCDeadTraceMultiSession tests per-session object tagging with two
// concurrent sessions that allocate from the same call site.
// The per-session breakdown should attribute frees to the correct session
// even when both sessions share the same bucket (same allocation site + size).
//
// Session A: 3 × 256B (freed) + 1 × 1024B (alive)
// Session B: 2 × 256B (freed) + 1 × 512B (alive)
// Main goroutine: 1 × 128B (non-session, not tracked)
func GCDeadTraceMultiSession() {
	runtime.MemProfileRate = 1

	// Non-session allocation on main goroutine.
	gcDeadPreSink = make([]byte, 128)

	var wg sync.WaitGroup

	// Session goroutine A: 3 × 256B freed, 1 × 1024B alive.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.GcDeadSessionStart(201)
		gcDeadMultiA1 = make([]byte, 256)  // will die
		gcDeadMultiA1 = make([]byte, 256)  // will die (overwritten)
		gcDeadMultiA1 = make([]byte, 256)  // will die (overwritten)
		gcDeadMultiA2 = make([]byte, 1024) // will stay alive
		runtime.GcDeadSessionEnd(201)
	}()

	// Session goroutine B: 2 × 256B freed, 1 × 512B alive.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.GcDeadSessionStart(202)
		gcDeadMultiB1 = make([]byte, 256)  // will die
		gcDeadMultiB1 = make([]byte, 256)  // will die (overwritten)
		gcDeadMultiB2 = make([]byte, 512)  // will stay alive
		runtime.GcDeadSessionEnd(202)
	}()

	wg.Wait()

	// Drop dying references; keep alive references stay.
	gcDeadMultiA1 = nil
	gcDeadMultiB1 = nil

	runtime.GC()
	fmt.Println("OK")
}

// --- Fully dead site test helpers ---
// Each function is a distinct call site in the profiler stack trace.

//go:noinline
func makeFullyDead64() []byte {
	return make([]byte, 64)
}

//go:noinline
func makeFullyDead128() []byte {
	return make([]byte, 128)
}

//go:noinline
func makeMixed256() []byte {
	return make([]byte, 256)
}

//go:noinline
func makeAllAlive1024() []byte {
	return make([]byte, 1024)
}

var (
	fullyDeadTemp []byte   // overwritten repeatedly, last ref nil'd → all die
	fullyDeadLive []byte   // keeps some mixed allocs alive
)

func GCDeadTraceFullyDead() {
	runtime.MemProfileRate = 1

	runtime.GcDeadSessionStart(3)

	// Call site A: 5 × 64B, ALL die (overwritten + nil'd before GC).
	for i := 0; i < 5; i++ {
		fullyDeadTemp = makeFullyDead64()
	}

	// Call site B: 3 × 128B, ALL die (overwritten + nil'd before GC).
	for i := 0; i < 3; i++ {
		fullyDeadTemp = makeFullyDead128()
	}

	// Call site C: 4 × 256B, MIXED — 2 survive, 2 die.
	for i := 0; i < 4; i++ {
		b := makeMixed256()
		if i < 2 {
			fullyDeadLive = b // keep 2 alive via global (b escapes)
		}
		// other 2: b goes out of scope → die
	}

	// Call site D: 1 × 1024B, ALL survive (kept via global).
	globalLiveCache = makeAllAlive1024()

	runtime.GcDeadSessionEnd(3)

	// Drop the last overwritten local reference → remaining all-dead objects die.
	fullyDeadTemp = nil

	runtime.GC()
	fmt.Println("OK")
}

// go:noinline ensures allocSameBucket uses same bucket (same call stack + size).
//
//go:noinline
func allocSameBucket() []byte {
	return make([]byte, 256)
}

// GCDeadTraceBucketOvercount verifies that a non-session allocation at the
// same call site as a session allocation is NOT counted as a session freed object.
//
// Test layout:
//  1. Goroutine A: start session → allocSameBucket() [will die] → end session
//  2. Same goroutine A: allocSameBucket() [will die, NOT in session] → same bucket!
//  3. Nil the dying references
//  4. GC
//  5. Print "OK"
//
// Expected: gcdeadsession:freed shows 1 session obj (256 bytes), not 2.
// The non-session allocSameBucket at the same call site should not be counted.
func GCDeadTraceBucketOvercount() {
	runtime.MemProfileRate = 1

	// Step 1: Session goroutine allocates 256B that will die.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.GcDeadSessionStart(301)
		deadSink = allocSameBucket() // session alloc, 256B, will die
		runtime.GcDeadSessionEnd(301)
	}()
	wg.Wait()

	// Step 2: Same goroutine (main) allocates from the same call site,
	// but NOT in a session. This should NOT be counted as session freed.
	deadSink = allocSameBucket() // non-session alloc, same bucket, will die

	// Step 3: Nil the dying reference.
	deadSink = nil

	// Step 4: Force GC.
	runtime.GC()

	// Step 5: Done.
	fmt.Println("OK")
}

// GCDeadTraceBucketOvercountConcurrent verifies same-bucket overcounting in a
// concurrent session scenario. Two concurrent session goroutines allocate from
// the same call site (allocSameBucket), plus a non-session allocation from the
// same call site. The non-session alloc must not be counted as session freed.
//
// Goroutine A: session → allocSameBucket() [will die] → alloc256() [stays alive]
// Goroutine B: session → allocSameBucket() [will die]
// Main goroutine: allocSameBucket() [non-session, same bucket, will die]
//
// Expected: gcdeadsession:freed = 2 session objs (512 bytes) — NOT 3.
// gcdeadsession:alive = 1 session obj (256 bytes).
//
//go:noinline
func alloc256Live() []byte {
	return make([]byte, 256)
}

func GCDeadTraceBucketOvercountConcurrent() {
	runtime.MemProfileRate = 1

	var wg sync.WaitGroup
	var startWg sync.WaitGroup
	startWg.Add(2)

	// Goroutine A: session with one dying and one alive alloc.
	// allocSameBucket() shares the bucket with non-session alloc.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.GcDeadSessionStart(401)
		startWg.Done()
		startWg.Wait()
		gcDeadConcSessA1 = allocSameBucket()  // 256B, will die
		gcDeadConcSessA2 = alloc256Live()     // 256B, stays alive
	}()

	// Goroutine B: joins the same session 401 from a different call site.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.GcDeadSessionStart(401)
		startWg.Done()
		startWg.Wait()
		gcDeadConcSessB1 = allocSameBucket()  // 256B, will die
		runtime.GcDeadSessionEnd(401)
	}()

	wg.Wait()

	// Non-session alloc at the same bucket — should NOT be session-freed.
	deadSink = allocSameBucket() // 256B, non-session, will die

	// Nil all dying references.
	gcDeadConcSessA1 = nil
	gcDeadConcSessB1 = nil
	deadSink = nil

	runtime.GC()
	fmt.Println("OK")
}

// Test SetMemoryLimit functionality.
//
// This test lives here instead of runtime/debug because the entire
// implementation is in the runtime, and testprog gives us a more
// consistent testing environment to help avoid flakiness.
func gcMemoryLimit(gcPercent int) {
	if oldProcs := runtime.GOMAXPROCS(4); oldProcs < 4 {
		// Fail if the default GOMAXPROCS isn't at least 4.
		// Whatever invokes this should check and do a proper t.Skip.
		println("insufficient CPUs")
		return
	}
	debug.SetGCPercent(gcPercent)

	const myLimit = 256 << 20
	if limit := debug.SetMemoryLimit(-1); limit != math.MaxInt64 {
		print("expected MaxInt64 limit, got ", limit, " bytes instead\n")
		return
	}
	if limit := debug.SetMemoryLimit(myLimit); limit != math.MaxInt64 {
		print("expected MaxInt64 limit, got ", limit, " bytes instead\n")
		return
	}
	if limit := debug.SetMemoryLimit(-1); limit != myLimit {
		print("expected a ", myLimit, "-byte limit, got ", limit, " bytes instead\n")
		return
	}

	target := make(chan int64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		sinkSize := int(<-target / memLimitUnit)
		for {
			if len(memLimitSink) != sinkSize {
				memLimitSink = make([]*[memLimitUnit]byte, sinkSize)
			}
			for i := 0; i < len(memLimitSink); i++ {
				memLimitSink[i] = new([memLimitUnit]byte)
				// Write to this memory to slow down the allocator, otherwise
				// we get flaky behavior. See #52433.
				for j := range memLimitSink[i] {
					memLimitSink[i][j] = 9
				}
			}
			// Again, Gosched to slow down the allocator.
			runtime.Gosched()
			select {
			case newTarget := <-target:
				if newTarget == math.MaxInt64 {
					return
				}
				sinkSize = int(newTarget / memLimitUnit)
			default:
			}
		}
	}()
	var m [2]metrics.Sample
	m[0].Name = "/memory/classes/total:bytes"
	m[1].Name = "/memory/classes/heap/released:bytes"

	// Don't set this too high, because this is a *live heap* target which
	// is not directly comparable to a total memory limit.
	maxTarget := int64((myLimit / 10) * 8)
	increment := int64((myLimit / 10) * 1)
	for i := increment; i < maxTarget; i += increment {
		target <- i

		// Check to make sure the memory limit is maintained.
		// We're just sampling here so if it transiently goes over we might miss it.
		// The internal accounting is inconsistent anyway, so going over by a few
		// pages is certainly possible. Just make sure we're within some bound.
		// Note that to avoid flakiness due to #52433 (especially since we're allocating
		// somewhat heavily here) this bound is kept loose. In practice the Go runtime
		// should do considerably better than this bound.
		bound := int64(myLimit + 16<<20)
		if runtime.GOOS == "darwin" {
			bound += 24 << 20 // Be more lax on Darwin, see issue 73136.
		}
		start := time.Now()
		for time.Since(start) < 200*time.Millisecond {
			metrics.Read(m[:])
			retained := int64(m[0].Value.Uint64() - m[1].Value.Uint64())
			if retained > bound {
				print("retained=", retained, " limit=", myLimit, " bound=", bound, "\n")
				panic("exceeded memory limit by more than bound allows")
			}
			runtime.Gosched()
		}
	}

	if limit := debug.SetMemoryLimit(math.MaxInt64); limit != myLimit {
		print("expected a ", myLimit, "-byte limit, got ", limit, " bytes instead\n")
		return
	}
	println("OK")
}

// Pick a value close to the page size. We want to m
const memLimitUnit = 8000

var memLimitSink []*[memLimitUnit]byte
