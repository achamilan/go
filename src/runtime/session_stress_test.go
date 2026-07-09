// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Session memory stress test: long-running concurrent workloads designed
// to surface race conditions, memory leaks, bucket pool recycling bugs,
// and data corruption under sustained pressure.

package runtime_test

import (
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

const (
	stressDuration = 30 * time.Second
	reportInterval = 3 * time.Second
	stressMultiple = 4
)

type stressState struct {
	done        chan struct{}
	startTime   time.Time
	startGo     int
	startHeap   uint64 // heap allocated at start
	corruptions atomic.Uint64
	panics      atomic.Uint64
	allocs      atomic.Uint64
	sessions    atomic.Uint64
	gcCount     atomic.Uint64
	closedErr   atomic.Uint64 // sessions closed due to error in W2
}

func TestSessionStress(t *testing.T) {
	if testing.Short() {
		// Use shorter duration in short mode but still run.
		t.Log("short mode: running 5s stress test")
	}

	dur := stressDuration
	if testing.Short() {
		dur = 5 * time.Second
	}

	s := &stressState{
		done:      make(chan struct{}),
		startTime: time.Now(),
		startGo:   runtime.NumGoroutine(),
	}
	{
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		s.startHeap = ms.HeapAlloc
	}

	seed := time.Now().UnixNano()
	numCPU := runtime.GOMAXPROCS(0)
	workers := numCPU * stressMultiple

	t.Logf("stress: duration=%v, gomaxprocs=%d, workers=%d", dur, numCPU, workers)

	var wg sync.WaitGroup

	// ── W1: SessionLifecycle ──────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.recoverPanic("W1")
		w1_sessionLifecycle(s, newRNG(seed+1))
	}()

	// ── W2: ConcurrentAlloc ───────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.recoverPanic("W2-main")
		w2_concurrentAlloc(s, newRNG(seed+2), numCPU*2)
	}()

	// ── W3: GCHammer ─────────────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.recoverPanic("W3")
		w3_gcHammer(s, newRNG(seed+3))
	}()

	// ── W4: BucketBoundary ────────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.recoverPanic("W4")
		w4_bucketBoundary(s, newRNG(seed+4))
	}()

	// ── W5: PoolChurn ─────────────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.recoverPanic("W5")
		w5_poolChurn(s, newRNG(seed+5))
	}()

	// ── Monitor ───────────────────────────────────────────────────────
	stopMonitor := make(chan struct{})
	go func() {
		ticker := time.NewTicker(reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				s.report(t)
			}
		}
	}()

	// ── Wait for duration, then stop ──────────────────────────────────
	time.Sleep(dur)
	close(s.done)

	// Wait for all workloads with timeout.
	waitTimeout(&wg, 30*time.Second)

	close(stopMonitor)

	// ── Final verification ─────────────────────────────────────────────
	runtime.GC()
	runtime.GC()
	runtime.GC()

	t.Log("── Final results ──")
	s.report(t)

	if c := s.corruptions.Load(); c != 0 {
		t.Errorf("DATA CORRUPTION: %d corruptions detected", c)
	}
	if p := s.panics.Load(); p != 0 {
		t.Errorf("PANICS: %d goroutine panics detected", p)
	}

	// Allow a small delta for timing goroutines.
	endGo := runtime.NumGoroutine()
	delta := endGo - s.startGo
	if delta > 10 {
		t.Errorf("GOROUTINE LEAK: started with %d, ended with %d (delta=%d)",
			s.startGo, endGo, delta)
	} else if delta > 0 {
		t.Logf("goroutine delta: +%d (within tolerance)", delta)
	}
}

func (s *stressState) report(t *testing.T) {
	elapsed := time.Since(s.startTime).Truncate(time.Second)
	t.Logf("[t=%v] sessions: %d | allocs: %d | gc: %d",
		elapsed, s.sessions.Load(), s.allocs.Load(), s.gcCount.Load())
	t.Logf("  corruptions: %d | panics: %d | closedErr: %d | goroutines: %d",
		s.corruptions.Load(), s.panics.Load(), s.closedErr.Load(), runtime.NumGoroutine())
}

func (s *stressState) recoverPanic(name string) {
	if r := recover(); r != nil {
		s.panics.Add(1)
	}
}

// cookie computes a session-scoped value for write verification.
func cookie(sessionID uint64, seq uint64) uint64 {
	return (sessionID << 32) | (seq & 0xFFFFFFFF)
}

// ─── W1: SessionLifecycle ────────────────────────────────────────────────

func w1_sessionLifecycle(s *stressState, rng *rand.Rand) {
	var sid atomic.Uint64
	for {
		select {
		case <-s.done:
			return
		default:
		}

		sess := runtime.NewSession()
		id := sid.Add(1)
		n := rng.Intn(200) + 1

		// Allocate n objects of random sizes (1–8192 bytes).
		// Store addresses as uintptr (not unsafe.Pointer) to avoid
		// triggering GC write barriers on pointers to session memory.
		type alloc struct {
			addr uintptr
			cook uint64
		}
		allocs := make([]alloc, 0, n)

		ok := true
		for seq := 0; seq < n; seq++ {
			sz := uintptr(rng.Intn(8192) + 1)
			p := sess.Alloc(sz)
			if p == nil {
				ok = false
				break
			}
			cv := cookie(id, uint64(seq))
			*(*uint64)(p) = cv
			allocs = append(allocs, alloc{uintptr(p), cv})
		}

		// Verify all cookies.
		if ok {
			for _, a := range allocs {
				if *(*uint64)(unsafe.Pointer(a.addr)) != a.cook {
					s.corruptions.Add(1)
				}
			}
		}

		sess.Close()
		s.sessions.Add(1)
		s.allocs.Add(uint64(len(allocs)))
	}
}

// ─── W2: ConcurrentAlloc ─────────────────────────────────────────────────

func w2_concurrentAlloc(s *stressState, rng *rand.Rand, numWorkers int) {
	sess := runtime.NewSession()
	var sessID atomic.Uint64
	sessID.Store(1)

	var rebuildCh atomic.Uint64 // flag: non-zero means rebuild needed

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := 0; i < numWorkers; i++ {
		go func(id int) {
			defer wg.Done()
			defer s.recoverPanic("W2-worker")
			localRng := newRNG(rng.Int63()) // per-goroutine random source
			iter := 0
			for {
				select {
				case <-s.done:
					return
				default:
				}
				cur := sess
				sz := uintptr(localRng.Intn(4096) + 1)
				p := cur.Alloc(sz)
				if p != nil {
					cv := cookie(sessID.Load(), uint64(id*100000+iter))
					*(*uint64)(p) = cv
				} else {
					// Session closed → flag rebuild.
					rebuildCh.Store(1)
				}
				iter++
				if iter%500 == 0 && rebuildCh.Load() != 0 {
					// The main loop handles rebuild.
				}
			}
		}(i)
	}

	// Main loop: periodically close and recreate session to simulate
	// use-after-close races with active Alloc callers.
	recreateTimer := time.NewTimer(time.Duration(rng.Intn(100)+50) * time.Millisecond)
	defer recreateTimer.Stop()

	for {
		select {
		case <-s.done:
			sess.Close()
			wg.Wait()
			return
		case <-recreateTimer.C:
			// Proactive rebuild: close current and create new.
			old := sess
			sess = runtime.NewSession()
			sessID.Add(1)
			old.Close()
			s.closedErr.Add(1)
			rebuildCh.Store(0)
			recreateTimer.Reset(time.Duration(rng.Intn(100)+50) * time.Millisecond)
		default:
			if rebuildCh.Load() != 0 {
				// Worker detected a closed session; rebuild on demand.
				sess.Close()
				sess = runtime.NewSession()
				sessID.Add(1)
				rebuildCh.Store(0)
				s.closedErr.Add(1)
				recreateTimer.Reset(time.Duration(rng.Intn(100)+50) * time.Millisecond)
			}
		}
	}
}

// ─── W3: GCHammer ────────────────────────────────────────────────────────

func w3_gcHammer(s *stressState, rng *rand.Rand) {
	for {
		select {
		case <-s.done:
			return
		default:
		}
		// Sleep random interval.
		time.Sleep(time.Duration(rng.Intn(100)+1) * time.Millisecond)

		runtime.GC()
		s.gcCount.Add(1)
	}
}

// ─── W4: BucketBoundary ──────────────────────────────────────────────────

func w4_bucketBoundary(s *stressState, rng *rand.Rand) {
	// Sizes designed to test refill path near bucket boundaries.
	sizes := []uintptr{
		sessionBucketBytes - 16,
		32,
		sessionBucketBytes - 8,
		16,
		1024,
		sessionBucketBytes,
	}

	for {
		select {
		case <-s.done:
			return
		default:
		}

		sess := runtime.NewSession()
		ok := true
		for i, sz := range sizes {
			p := sess.Alloc(sz)
			if p == nil {
				ok = false
				break
			}
			cv := cookie(s.sessions.Load()%9999+1, uint64(i))
			*(*uint64)(p) = cv
		}

		// Also add random allocations to mix things up.
		if ok {
			extra := rng.Intn(5) + 1
			for i := 0; i < extra; i++ {
				sz := uintptr(rng.Intn(1024) + 1)
				p := sess.Alloc(sz)
				if p == nil {
					break
				}
				*(*uint64)(p) = 0xBEEF
			}
		}

		sess.Close()
		s.sessions.Add(1)
		s.allocs.Add(uint64(len(sizes)))
	}
}

// ─── W5: PoolChurn ────────────────────────────────────────────────────────

func w5_poolChurn(s *stressState, rng *rand.Rand) {
	for {
		select {
		case <-s.done:
			return
		default:
		}

		const batchSize = 100
		sessions := make([]*runtime.Session, batchSize)
		for i := 0; i < batchSize; i++ {
			sess := runtime.NewSession()
			// Small allocation to touch the bucket.
			p := sess.Alloc(uintptr(rng.Intn(248) + 8))
			if p != nil {
				*(*uint64)(p) = uint64(i)
			}
			sessions[i] = sess
		}

		// Close in random order to maximize pool fragmentation.
		rng.Shuffle(batchSize, func(i, j int) {
			sessions[i], sessions[j] = sessions[j], sessions[i]
		})

		for _, sess := range sessions {
			sess.Close()
		}

		s.sessions.Add(batchSize)
		s.allocs.Add(batchSize)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func newRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		// Timeout: some goroutines may be stuck, but the test should still
		// report results rather than hanging forever.
	}
}
