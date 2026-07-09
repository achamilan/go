package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// Actor Framework
// ============================================================

// Message types for inter-actor communication.
type MsgType int

const (
	MsgStart  MsgType = iota // begin work
	MsgStop                   // shutdown
	MsgQuery                  // request stats
	MsgReport                 // report stats
	MsgTick                   // periodic tick
)

// Message exchanged between actors.
type Message struct {
	Type    MsgType
	From    string
	Payload interface{}
}

// Actor is the interface for all actors.
type Actor interface {
	Start()
	Stop()
	Send(msg Message)
	Name() string
}

// baseActor provides shared actor infrastructure.
type baseActor struct {
	name    string
	mailbox chan Message
	done    chan struct{}
	wg      *sync.WaitGroup
}

func newBaseActor(name string, wg *sync.WaitGroup, mailboxSize int) baseActor {
	return baseActor{
		name:    name,
		mailbox: make(chan Message, mailboxSize),
		done:    make(chan struct{}),
		wg:      wg,
	}
}

func (a *baseActor) Name() string { return a.name }
func (a *baseActor) Send(msg Message) {
	select {
	case a.mailbox <- msg:
	case <-a.done:
	}
}

// ============================================================
// WorkerActor — simulates gcdeadtrace allocation patterns
// ============================================================

// AllocPattern controls what allocation pattern a worker uses.
type AllocPattern int

const (
	PatternLoop AllocPattern = iota // GCDeadTrace-like: loop allocate, all die
	PatternMixed                    // GCDeadTraceComplex-like: some live, some die
	PatternSession                  // GCDeadTraceSession-like: session tracked
	PatternFullyDead                // GCDeadTraceFullyDead-like: per-site tracking
)

func (p AllocPattern) String() string {
	switch p {
	case PatternLoop:
		return "loop"
	case PatternMixed:
		return "mixed"
	case PatternSession:
		return "session"
	case PatternFullyDead:
		return "fullydead"
	default:
		return "unknown"
	}
}

// AllocStats tracks allocation activity inside a worker.
type AllocStats struct {
	TotalAllocs     int64
	TotalBytes      int64
	DeadAllocs      int64
	DeadBytes       int64
	AliveAllocs     int64
	AliveBytes      int64
	GCCycles        int64
	SessionAllocs   int64
	SessionBytes    int64
	SessionFreed    int64
	SessionFreedByt int64
}

// WorkerActor simulates GCDeadTrace allocation patterns.
type WorkerActor struct {
	baseActor

	pattern  AllocPattern
	interval time.Duration // between allocation bursts

	// Persistent references (kept alive across GC cycles)
	aliveRefs [][]byte

	// Scratch reference (overwritten each burst → becomes dead)
	scratch  []byte
	scratch2 []byte

	// Tracking
	stats   AllocStats
	burstID atomic.Int64

	// For session pattern
	sessionActive bool
}

// NewWorkerActor creates a worker with the given allocation pattern.
func NewWorkerActor(name string, pattern AllocPattern, wg *sync.WaitGroup) *WorkerActor {
	return &WorkerActor{
		baseActor: newBaseActor(name, wg, 16),
		pattern:   pattern,
		interval:  500 * time.Millisecond,
	}
}

func (w *WorkerActor) Start() {
	w.wg.Add(1)
	go w.run()
}

func (w *WorkerActor) Stop() {
	close(w.done)
}

// run is the actor's main event loop.
func (w *WorkerActor) run() {
	defer w.wg.Done()
	log.Printf("[%s] started (pattern=%s)", w.name, w.pattern)

	// Start a ticker for periodic allocation bursts.
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Enable profiling at allocation level.
	runtime.MemProfileRate = 1

	for {
		select {
		case <-w.done:
			log.Printf("[%s] stopping", w.name)
			return

		case <-ticker.C:
			w.doAllocBurst()

		case msg := <-w.mailbox:
			w.handleMessage(msg)
		}
	}
}

// handleMessage processes incoming messages.
func (w *WorkerActor) handleMessage(msg Message) {
	switch msg.Type {
	case MsgQuery:
		// Report current stats back to sender.
		stats := w.stats
		stats.AliveAllocs = int64(len(w.aliveRefs))
		for _, b := range w.aliveRefs {
			stats.AliveBytes += int64(len(b))
		}
		reply := Message{
			Type:    MsgReport,
			From:    w.name,
			Payload: stats,
		}
		// Send report to whoever asked.
		// In this demo we use a known system mailbox.
		if ch, ok := msg.Payload.(chan Message); ok {
			select {
			case ch <- reply:
			default:
			}
		}

	case MsgStop:
		w.scratch = nil
		w.scratch2 = nil
		w.aliveRefs = nil
		runtime.GC()
		log.Printf("[%s] cleaned up and stopped", w.name)
	}
}

// doAllocBurst runs one allocation burst matching the configured pattern.
func (w *WorkerActor) doAllocBurst() {
	w.burstID.Add(1)

	switch w.pattern {
	case PatternLoop:
		w.patternLoop()
	case PatternMixed:
		w.patternMixed()
	case PatternSession:
		w.patternSession()
	case PatternFullyDead:
		w.patternFullyDead()
	}
}

// patternLoop: TestGcDeadTrace-like — allocate in loop, all die.
//   - 500 iterations of make([]byte, 256), overwriting each time
//   - last reference set to nil before GC
//   - one persistent allocation kept alive
func (w *WorkerActor) patternLoop() {
	// Allocate garbage — each iteration's previous value dies.
	for i := 0; i < 500; i++ {
		w.scratch = make([]byte, 256)
		atomic.AddInt64(&w.stats.TotalAllocs, 1)
		atomic.AddInt64(&w.stats.TotalBytes, 256)
	}
	// Previous reference dies.
	w.scratch = nil
	atomic.AddInt64(&w.stats.DeadAllocs, 500)
	atomic.AddInt64(&w.stats.DeadBytes, 500*256)

	// Keep one alive.
	w.aliveRefs = append(w.aliveRefs, make([]byte, 1024))
	atomic.AddInt64(&w.stats.TotalAllocs, 1)
	atomic.AddInt64(&w.stats.TotalBytes, 1024)

	// Trigger GC to see gcdeadtrace output.
	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

// patternMixed: TestGcDeadTraceComplex-like — multiple callers, session tracked.
func (w *WorkerActor) patternMixed() {
	// Allocate blocks from different "sites" (logically different callers).
	site1 := w.allocSmallBlocks(30, 256) // 30 × 256B — dies
	site2 := w.allocLargeBlocks(5, 1024) // 5 × 1KB — dies

	// Use the data to prevent dead code elimination.
	_ = checksum(site1)
	_ = checksum(site2)

	// Keep a global-like live reference.
	w.aliveRefs = append(w.aliveRefs, make([]byte, 4096))
	atomic.AddInt64(&w.stats.TotalAllocs, 1)
	atomic.AddInt64(&w.stats.TotalBytes, 4096)

	// site1, site2 go out of scope — they die.
	atomic.AddInt64(&w.stats.DeadAllocs, 35)
	atomic.AddInt64(&w.stats.DeadBytes, 30*256+5*1024)

	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

//go:noinline
func (w *WorkerActor) allocSmallBlocks(n, size int) [][]byte {
	blocks := make([][]byte, n)
	for i := range blocks {
		blocks[i] = make([]byte, size)
	}
	return blocks
}

//go:noinline
func (w *WorkerActor) allocLargeBlocks(n, size int) [][]byte {
	blocks := make([][]byte, n)
	for i := range blocks {
		blocks[i] = make([]byte, size)
	}
	return blocks
}

// patternSession: TestGcDeadTraceSession-like — uses GcDeadSession tracking.
func (w *WorkerActor) patternSession() {
	if !w.sessionActive {
		runtime.GcDeadSessionStart()
		w.sessionActive = true
	}

	// Session allocation: 256B that will die.
	w.scratch = make([]byte, 256)
	atomic.AddInt64(&w.stats.SessionAllocs, 1)
	atomic.AddInt64(&w.stats.SessionBytes, 256)

	// Session allocation: 1024B kept alive.
	w.aliveRefs = append(w.aliveRefs, make([]byte, 1024))
	atomic.AddInt64(&w.stats.SessionAllocs, 1)
	atomic.AddInt64(&w.stats.SessionBytes, 1024)

	// End session — only session allocations are tracked.
	runtime.GcDeadSessionEnd()
	w.sessionActive = false

	// Non-session allocation (not in gcdeadsession output).
	w.scratch2 = make([]byte, 512)

	// Let the session 256B die.
	w.scratch = nil

	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

// patternFullyDead: TestGcDeadTraceFullyDead-like — per-site fully dead detection.
func (w *WorkerActor) patternFullyDead() {
	runtime.GcDeadSessionStart()

	// Site A: 5 × 64B, ALL die.
	for i := 0; i < 5; i++ {
		w.scratch = w.allocSiteA()
	}
	atomic.AddInt64(&w.stats.DeadAllocs, 5)
	atomic.AddInt64(&w.stats.DeadBytes, 5*64)

	// Site B: 3 × 128B, ALL die.
	for i := 0; i < 3; i++ {
		w.scratch = w.allocSiteB()
	}
	atomic.AddInt64(&w.stats.DeadAllocs, 3)
	atomic.AddInt64(&w.stats.DeadBytes, 3*128)

	// Site C: 4 × 256B, 2 survive (stored) + 2 die.
	for i := 0; i < 4; i++ {
		b := w.allocSiteC()
		if i < 2 {
			w.aliveRefs = append(w.aliveRefs, b) // keep alive
		}
	}
	atomic.AddInt64(&w.stats.DeadAllocs, 2)
	atomic.AddInt64(&w.stats.DeadBytes, 2*256)

	runtime.GcDeadSessionEnd()

	w.scratch = nil
	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

//go:noinline
func (w *WorkerActor) allocSiteA() []byte { return make([]byte, 64) }

//go:noinline
func (w *WorkerActor) allocSiteB() []byte { return make([]byte, 128) }

//go:noinline
func (w *WorkerActor) allocSiteC() []byte { return make([]byte, 256) }

// ============================================================
// MonitorActor — monitors memory usage across actors
// ============================================================

// MonitorSnapshot is a point-in-time memory snapshot.
type MonitorSnapshot struct {
	Timestamp     time.Time
	Mem           runtime.MemStats
	NumGoroutines int
	Workers       map[string]AllocStats
}

// MonitorActor periodically reads memory stats and queries workers.
type MonitorActor struct {
	baseActor

	interval     time.Duration
	workers      map[string]*WorkerActor
	queryMailbox chan Message
	snapshots    []MonitorSnapshot
	maxSnapshots int
}

// NewMonitorActor creates a memory monitor.
func NewMonitorActor(name string, interval time.Duration, workers map[string]*WorkerActor, wg *sync.WaitGroup) *MonitorActor {
	return &MonitorActor{
		baseActor:    newBaseActor(name, wg, 32),
		interval:     interval,
		workers:      workers,
		queryMailbox: make(chan Message, 16),
		maxSnapshots: 10,
	}
}

func (m *MonitorActor) Start() {
	m.wg.Add(1)
	go m.run()
}

func (m *MonitorActor) Stop() {
	close(m.done)
}

func (m *MonitorActor) run() {
	defer m.wg.Done()
	log.Printf("[%s] monitor started (interval=%v)", m.name, m.interval)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// First snapshot immediately.
	m.takeSnapshot()

	for {
		select {
		case <-m.done:
			m.printSummary()
			log.Printf("[%s] stopping", m.name)
			return

		case <-ticker.C:
			m.takeSnapshot()

		case msg := <-m.mailbox:
			if msg.Type == MsgReport {
				m.handleWorkerReport(msg)
			}
		}
	}
}

// takeSnapshot reads system memory stats and queries workers.
func (m *MonitorActor) takeSnapshot() {
	var snap MonitorSnapshot
	snap.Timestamp = time.Now()
	runtime.ReadMemStats(&snap.Mem)
	snap.NumGoroutines = runtime.NumGoroutine()

	// Query each worker for their stats.
	snap.Workers = make(map[string]AllocStats)
	for name, worker := range m.workers {
		worker.Send(Message{
			Type:    MsgQuery,
			From:    m.name,
			Payload: m.queryMailbox,
		})

		// Collect reply with short timeout.
		select {
		case reply := <-m.queryMailbox:
			if stats, ok := reply.Payload.(AllocStats); ok {
				snap.Workers[name] = stats
			}
		case <-time.After(100 * time.Millisecond):
			// worker may be busy, skip this cycle.
		}
	}

	m.snapshots = append(m.snapshots, snap)
	if len(m.snapshots) > m.maxSnapshots {
		m.snapshots = m.snapshots[1:]
	}

	m.printSnapshot(snap)
}

func (m *MonitorActor) handleWorkerReport(msg Message) {
	// Reports can be logged or aggregated here.
	if stats, ok := msg.Payload.(AllocStats); ok {
		log.Printf("[%s] report from %s: allocs=%d bytes=%d",
			m.name, msg.From, stats.TotalAllocs, stats.TotalBytes)
	}
}

func (m *MonitorActor) printSnapshot(snap MonitorSnapshot) {
	fmt.Println()
	fmt.Println("========================================")
	fmt.Printf("  Memory Snapshot at %s\n", snap.Timestamp.Format("15:04:05.000"))
	fmt.Println("========================================")
	fmt.Printf("  Alloc       = %6.1f MB\n", float64(snap.Mem.Alloc)/1024/1024)
	fmt.Printf("  TotalAlloc  = %6.1f MB\n", float64(snap.Mem.TotalAlloc)/1024/1024)
	fmt.Printf("  Sys         = %6.1f MB\n", float64(snap.Mem.Sys)/1024/1024)
	fmt.Printf("  NumGC       = %d\n", snap.Mem.NumGC)
	fmt.Printf("  Goroutines  = %d\n", snap.NumGoroutines)
	fmt.Printf("  PauseTotal  = %6.1f ms\n", float64(snap.Mem.PauseTotalNs)/1e6)
	fmt.Println("----------------------------------------")

	for name, stats := range snap.Workers {
		fmt.Printf("  [%s]\n", name)
		fmt.Printf("    TotalAllocs  = %d\n", stats.TotalAllocs)
		fmt.Printf("    Total bytes  = %d\n", stats.TotalBytes)
		fmt.Printf("    Dead allocs  = %d (%d bytes)\n", stats.DeadAllocs, stats.DeadBytes)
		fmt.Printf("    Alive refs   = %d (%d bytes)\n", stats.AliveAllocs, stats.AliveBytes)
		fmt.Printf("    Session      = %d allocs, %d bytes\n", stats.SessionAllocs, stats.SessionBytes)
		fmt.Printf("    GC cycles    = %d\n", stats.GCCycles)
	}
	fmt.Println("========================================")
	fmt.Println()
}

func (m *MonitorActor) printSummary() {
	if len(m.snapshots) == 0 {
		return
	}
	first := m.snapshots[0]
	last := m.snapshots[len(m.snapshots)-1]
	elapsed := last.Timestamp.Sub(first.Timestamp)

	fmt.Println()
	fmt.Println(">>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>")
	fmt.Println("  MONITOR SUMMARY")
	fmt.Println(">>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>")
	fmt.Printf("  Duration     = %v\n", elapsed)
	fmt.Printf("  Start Alloc  = %6.1f MB\n", float64(first.Mem.Alloc)/1024/1024)
	fmt.Printf("  End Alloc    = %6.1f MB\n", float64(last.Mem.Alloc)/1024/1024)
	fmt.Printf("  TotalAlloc   = %6.1f MB (cumulative)\n", float64(last.Mem.TotalAlloc)/1024/1024)
	fmt.Printf("  GC cycles    = %d\n", last.Mem.NumGC-first.Mem.NumGC)
	fmt.Printf("  Goroutines   = %d\n", last.NumGoroutines)
	fmt.Println("<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<")
	fmt.Println()
}

// ============================================================
// ActorSystem — manages lifecycle of all actors
// ============================================================

type ActorSystem struct {
	workers  map[string]*WorkerActor
	monitors []*MonitorActor
	wg       sync.WaitGroup
}

func NewActorSystem() *ActorSystem {
	return &ActorSystem{
		workers:  make(map[string]*WorkerActor),
		monitors: nil,
	}
}

func (as *ActorSystem) AddWorker(name string, pattern AllocPattern) *WorkerActor {
	w := NewWorkerActor(name, pattern, &as.wg)
	as.workers[name] = w
	return w
}

func (as *ActorSystem) AddMonitor(name string, interval time.Duration) *MonitorActor {
	m := NewMonitorActor(name, interval, as.workers, &as.wg)
	as.monitors = append(as.monitors, m)
	return m
}

func (as *ActorSystem) Start() {
	log.Println("[system] starting all actors...")
	for _, m := range as.monitors {
		m.Start()
	}
	for _, w := range as.workers {
		w.Start()
	}
	log.Println("[system] all actors started")
}

func (as *ActorSystem) Stop() {
	log.Println("[system] stopping all actors...")
	for _, w := range as.workers {
		w.Stop()
	}
	for _, m := range as.monitors {
		m.Stop()
	}
	as.wg.Wait()
	log.Println("[system] all actors stopped")
}

// ============================================================
// Helpers
// ============================================================

//go:noinline
func checksum(blocks [][]byte) uint64 {
	var h uint64
	for _, b := range blocks {
		for i, v := range b {
			h ^= uint64(v) << (uint64(i%8) * 8)
		}
	}
	return h
}

// ============================================================
// Main
// ============================================================

func main() {
	duration := flag.Duration("duration", 10*time.Second, "total run duration")
	flag.Parse()

	fmt.Println("========================================")
	fmt.Println("  Go Actor Pattern + gcdeadtrace Demo")
	fmt.Println("========================================")
	fmt.Printf("  Runtime: %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
	fmt.Println("========================================")
	fmt.Println()

	// Check if gcdeadtrace is available in the runtime.
	// The demo works with or without it, but session tracking
	// requires the modified runtime.

	// Build the actor system.
	system := NewActorSystem()

	// Create worker actors with different allocation patterns.
	_ = system.AddWorker("worker-loop", PatternLoop)       // GCDeadTrace-like
	mixed := system.AddWorker("worker-mixed", PatternMixed) // GCDeadTraceComplex-like
	_ = system.AddWorker("worker-session", PatternSession) // GCDeadTraceSession-like
	_ = system.AddWorker("worker-fullydead", PatternFullyDead) // GCDeadTraceFullyDead-like

	// Create monitor actors.
	// monitor-general watches all workers every 2 seconds.
	_ = system.AddMonitor("monitor-general", 2*time.Second)
	// monitor-worker watches specific worker (mixed) every 1 second.
	_ = system.AddMonitor("monitor-mixed", 1*time.Second)

	// Register the second monitor to only watch the mixed worker.
	// (Reuse mixed worker reference from above)
	_ = mixed

	// Start all actors.
	system.Start()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	select {
	case <-sigCh:
		log.Println("received interrupt, shutting down...")
	case <-time.After(*duration):
		log.Printf("run duration reached (%v), shutting down...", *duration)
	}

	system.Stop()
	fmt.Println("Demo finished.")
}
