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

// Package-level sink variables ensure heap escape for gcdeadtrace tests.
var (
	concurrentSinkA1 []byte
	concurrentSinkA2 []byte
	concurrentSinkB1 []byte
	concurrentSinkB2 []byte
)

// Custom type definitions for gcdeadtrace type tracking demo.
type ListNode struct {
	next *ListNode
	data []byte
}

type StringContainer struct {
	name   string
	labels []string
}

type DataProcessor interface {
	Process() uint64
}

type intProcessor struct {
	id   uint64
	data []byte
}

func (p *intProcessor) Process() uint64 {
	return p.id
}

// Package-level sink variables for custom types.
var (
	ctListHead    *ListNode
	ctMapSink     map[uint64][]byte
	ctProcessor   DataProcessor
	ctBuiltString string
	ctScratchMap  map[uint64][]byte
	ctScratchStr  string
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
	PatternConcurrent               // Multi-session concurrent tracking
	PatternCustomTypes              // Custom types: struct, map, interface, string
	PatternRePrint                  // Session re-print on end verification
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
	case PatternConcurrent:
		return "concurrent"
	case PatternCustomTypes:
		return "customtypes"
	case PatternRePrint:
		return "reprint"
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

	// MemProfileRate is automatically set to 1 by GcDeadSessionStart,
	// so no explicit rate setting is needed here.

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
	case PatternConcurrent:
		w.patternConcurrent()
	case PatternCustomTypes:
		w.patternCustomTypes()
	case PatternRePrint:
		w.patternRePrint()
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
// Uses a unique session ID per burst so each burst is independently tracked.
func (w *WorkerActor) patternSession() {
	sessionID := 1000 + uint64(w.burstID.Load())+1
	runtime.GcDeadSessionStart(sessionID)

	// Session allocation: 256B that will die.
	w.scratch = make([]byte, 256)
	atomic.AddInt64(&w.stats.SessionAllocs, 1)
	atomic.AddInt64(&w.stats.SessionBytes, 256)

	// Session allocation: 1024B kept alive.
	w.aliveRefs = append(w.aliveRefs, make([]byte, 1024))
	atomic.AddInt64(&w.stats.SessionAllocs, 1)
	atomic.AddInt64(&w.stats.SessionBytes, 1024)

	// End session — only session allocations are tracked.
	runtime.GcDeadSessionEnd(sessionID)
	w.sessionActive = false

	// Non-session allocation (not in gcdeadsession output).
	w.scratch2 = make([]byte, 512)

	// Let the session 256B die.
	w.scratch = nil

	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

// patternRePrint: verify session re-print when endPC becomes set.
// Flow: GC while active → GC while active (no re-print) → end → GC (re-print with end).
func (w *WorkerActor) patternRePrint() {
	if !w.sessionActive {
		runtime.GcDeadSessionStart(1002)
		w.sessionActive = true
	}

	// Dying allocation (256B) — will be freed.
	w.scratch = make([]byte, 256)
	// Alive allocation (1024B) — kept referenced.
	w.aliveRefs = append(w.aliveRefs, make([]byte, 1024))

	// GC 1: session has no end → first print (printed=1, no "end:").
	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)

	// Nil the dying ref so GC 2 sees frees.
	w.scratch = nil

	// GC 2: session still active, printed=1, endPC=0 → NOT re-printed.
	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)

	// End session — endPC becomes set.
	runtime.GcDeadSessionEnd(1002)
	w.sessionActive = false

	// GC 3: endPC != 0, printed=1 → re-printed with "end:".
	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

// patternFullyDead: TestGcDeadTraceFullyDead-like — per-site fully dead detection.
// Uses a unique session ID per burst so each burst is independently tracked.
func (w *WorkerActor) patternFullyDead() {
	sessionID := 2000 + uint64(w.burstID.Load())+1
	runtime.GcDeadSessionStart(sessionID)

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

	runtime.GcDeadSessionEnd(sessionID)

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

// concurrentAllocWorker is a shared function so both goroutines have the exact
// same call stack when calling allocSiteC — they map to the same raw entry and
// their distinct goids appear as separate [session #3001: ... (gid=N)] lines.
func concurrentAllocWorker(w *WorkerActor, a1 *[]byte, a2 *[]byte, size2 int, startBarrier, allocBarrier *sync.WaitGroup, wg *sync.WaitGroup) {
	defer wg.Done()
	runtime.GcDeadSessionStart(3001)
	startBarrier.Done()
	startBarrier.Wait() // wait for both goroutines to start the session
	*a1 = w.allocSiteC()                    // same site, same call stack for both goroutines
	*a2 = make([]byte, size2)               // alive, different size per goroutine
	allocBarrier.Done()
	allocBarrier.Wait() // wait for both goroutines to finish allocating
	runtime.GcDeadSessionEnd(3001)
}

// patternConcurrent: single session 3001 shared by two goroutines via a shared
// allocation function. Tests that the same session+site with different goids
// produces separate [session #3001: ... (gid=N)] [session #3001: ... (gid=M)] lines
// under a single site heading.
//
// Session 3001:
//   gid-A: 1 × allocSiteC (freed) + 1 × 1024B (alive)
//   gid-B: 1 × allocSiteC (freed) + 1 × 512B  (alive)
//   → allocSiteC line shows [session #3001: ... (gid=A)] [session #3001: ... (gid=B)]
func (w *WorkerActor) patternConcurrent() {
	// Reset sinks from previous burst so prior allocations die.
	concurrentSinkA1 = nil
	concurrentSinkA2 = nil
	concurrentSinkB1 = nil
	concurrentSinkB2 = nil

	var wg sync.WaitGroup
	var startBarrier sync.WaitGroup
	var allocBarrier sync.WaitGroup
	startBarrier.Add(2)
	allocBarrier.Add(2)

	wg.Add(2)
	go concurrentAllocWorker(w, &concurrentSinkA1, &concurrentSinkA2, 1024, &startBarrier, &allocBarrier, &wg)
	go concurrentAllocWorker(w, &concurrentSinkB1, &concurrentSinkB2, 512, &startBarrier, &allocBarrier, &wg)

	wg.Wait()

	// Drop dying references; alive refs (A2, B2) stay assigned.
	concurrentSinkA1 = nil
	concurrentSinkB1 = nil

	atomic.AddInt64(&w.stats.TotalAllocs, 4)
	atomic.AddInt64(&w.stats.TotalBytes, 256+1024+256+512)
	atomic.AddInt64(&w.stats.DeadAllocs, 2)
	atomic.AddInt64(&w.stats.DeadBytes, 256+256)

	// Trigger GC to see per-session breakdown output.
	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

// patternCustomTypes: tests gcdeadtrace tracking of various Go types.
// Allocations: struct (new), slice (make), map (make), interface (&struct), string (conversion).
// Uses a unique session ID per burst so each burst is independently tracked.
func (w *WorkerActor) patternCustomTypes() {
	sessionID := 4000 + uint64(w.burstID.Load())+1
	runtime.GcDeadSessionStart(sessionID)

	// 1. Struct allocation — ListNode via new(), nested make([]byte, size)
	ctListHead = ctAllocListNode(1, 128) // alive
	_ = ctAllocListNode(2, 64)           // dies (no reference kept)

	// 2. String conversion
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	ctBuiltString = string(raw) // alive
	ctScratchStr = string(raw)  // dies (overwritten to "" below)

	// 3. Map allocations (map with nested byte slices)
	ctMapSink = ctAllocMap(4)    // alive
	ctScratchMap = ctAllocMap(2) // dies

	// 4. Interface allocation via &intProcessor{}
	ctProcessor = ctAllocProcessor(42) // alive

	runtime.GcDeadSessionEnd(sessionID)

	// Drop dead references before GC
	ctScratchStr = ""
	ctScratchMap = nil

	runtime.GC()
	atomic.AddInt64(&w.stats.GCCycles, 1)
}

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
// Custom Type Helper Functions
// ============================================================

//go:noinline
func ctAllocListNode(id uint64, size int) *ListNode {
	return &ListNode{
		data: make([]byte, size),
	}
}

//go:noinline
func ctAllocByteSlice(size int) []byte {
	return make([]byte, size)
}

//go:noinline
func ctAllocMap(n int) map[uint64][]byte {
	m := make(map[uint64][]byte)
	for i := 0; i < n; i++ {
		m[uint64(i)] = make([]byte, 32)
	}
	return m
}

//go:noinline
func ctAllocProcessor(id uint64) DataProcessor {
	return &intProcessor{
		id:   id,
		data: make([]byte, 64),
	}
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
	mode := flag.String("mode", "all", "worker mode: all, loop, mixed, session, fullydead, concurrent, customtypes")
	flag.Parse()

	fmt.Println("========================================")
	fmt.Println("  Go Actor Pattern + gcdeadtrace Demo")
	fmt.Println("========================================")
	fmt.Printf("  Runtime: %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("  Mode: %s\n", *mode)
	fmt.Println("========================================")
	fmt.Println()

	if *mode == "all" {
		fmt.Println("  Workers:")
		fmt.Println("    worker-loop        — loop alloc, all die")
		fmt.Println("    worker-mixed       — multi-site, some live/some die")
		fmt.Println("    worker-session     — single session tracking")
		fmt.Println("    worker-fullydead   — per-site fully-dead detection")
		fmt.Println("    worker-concurrent  — two concurrent sessions, same site")
		fmt.Println("    worker-customtypes — struct, map, interface, string types")
		fmt.Println("  (gcdeadtrace session output appears on stderr via GODEBUG=gcdeadtrace=1)")
		fmt.Println()
	}

	// Build the actor system.
	system := NewActorSystem()

	// Create worker actors with different allocation patterns.
	switch *mode {
	case "all":
		_ = system.AddWorker("worker-loop", PatternLoop)
		_ = system.AddWorker("worker-mixed", PatternMixed)
		_ = system.AddWorker("worker-session", PatternSession)
		_ = system.AddWorker("worker-fullydead", PatternFullyDead)
		_ = system.AddWorker("worker-concurrent", PatternConcurrent)
		_ = system.AddWorker("worker-customtypes", PatternCustomTypes)
	case "loop":
		_ = system.AddWorker("worker-loop", PatternLoop)
	case "mixed":
		_ = system.AddWorker("worker-mixed", PatternMixed)
	case "session":
		_ = system.AddWorker("worker-session", PatternSession)
	case "fullydead":
		_ = system.AddWorker("worker-fullydead", PatternFullyDead)
	case "concurrent":
		_ = system.AddWorker("worker-concurrent", PatternConcurrent)
	case "customtypes":
		_ = system.AddWorker("worker-customtypes", PatternCustomTypes)
	case "reprint":
		_ = system.AddWorker("worker-reprint", PatternRePrint)
	default:
		log.Fatalf("unknown mode: %s (valid: all, loop, mixed, session, fullydead, concurrent, customtypes, reprint)", *mode)
	}

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
