// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Malloc profiling.
// Patterned after tcmalloc's algorithms; shorter code.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/profilerecord"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// NOTE(rsc): Everything here could use cas if contention became an issue.
var (
	// profInsertLock protects changes to the start of all *bucket linked lists
	profInsertLock mutex
	// profBlockLock protects the contents of every blockRecord struct
	profBlockLock mutex
	// profMemActiveLock protects the active field of every memRecord struct
	profMemActiveLock mutex
	// profMemFutureLock is a set of locks that protect the respective elements
	// of the future array of every memRecord struct
	profMemFutureLock [len(memRecord{}.future)]mutex
)

// All memory allocations are local and do not escape outside of the profiler.
// The profiler is forbidden from referring to garbage-collected memory.

const (
	// profile types
	memProfile bucketType = 1 + iota
	blockProfile
	mutexProfile

	// size of bucket hash table
	buckHashSize = 179999

	// maxSkip is to account for deferred inline expansion
	// when using frame pointer unwinding. We record the stack
	// with "physical" frame pointers but handle skipping "logical"
	// frames at some point after collecting the stack. So
	// we need extra space in order to avoid getting fewer than the
	// desired maximum number of frames after expansion.
	// This should be at least as large as the largest skip value
	// used for profiling; otherwise stacks may be truncated inconsistently
	maxSkip = 6

	// maxProfStackDepth is the highest valid value for debug.profstackdepth.
	// It's used for the bucket.stk func.
	// TODO(fg): can we get rid of this?
	maxProfStackDepth = 1024
)

type bucketType int

// A bucket holds per-call-stack profiling information.
// The representation is a bit sleazy, inherited from C.
// This struct defines the bucket header. It is followed in
// memory by the stack words and then the actual record
// data, either a memRecord or a blockRecord.
//
// Per-call-stack profiling information.
// Lookup by hashing call stack into a linked-list hash table.
//
// None of the fields in this bucket header are modified after
// creation, including its next and allnext links.
//
// No heap pointers.
type bucket struct {
	_       sys.NotInHeap
	next    *bucket
	allnext *bucket
	typ     bucketType // memBucket or blockBucket (includes mutexProfile)
	hash    uintptr
	size    uintptr
	nstk    uintptr
}

// A memRecord is the bucket data for a bucket of type memProfile,
// part of the memory profile.
type memRecord struct {
	// The following complex 3-stage scheme of stats accumulation
	// is required to obtain a consistent picture of mallocs and frees
	// for some point in time.
	// The problem is that mallocs come in real time, while frees
	// come only after a GC during concurrent sweeping. So if we would
	// naively count them, we would get a skew toward mallocs.
	//
	// Hence, we delay information to get consistent snapshots as
	// of mark termination. Allocations count toward the next mark
	// termination's snapshot, while sweep frees count toward the
	// previous mark termination's snapshot:
	//
	//              MT          MT          MT          MT
	//             .·|         .·|         .·|         .·|
	//          .·˙  |      .·˙  |      .·˙  |      .·˙  |
	//       .·˙     |   .·˙     |   .·˙     |   .·˙     |
	//    .·˙        |.·˙        |.·˙        |.·˙        |
	//
	//       alloc → ▲ ← free
	//               ┠┅┅┅┅┅┅┅┅┅┅┅P
	//       C+2     →    C+1    →  C
	//
	//                   alloc → ▲ ← free
	//                           ┠┅┅┅┅┅┅┅┅┅┅┅P
	//                   C+2     →    C+1    →  C
	//
	// Since we can't publish a consistent snapshot until all of
	// the sweep frees are accounted for, we wait until the next
	// mark termination ("MT" above) to publish the previous mark
	// termination's snapshot ("P" above). To do this, allocation
	// and free events are accounted to *future* heap profile
	// cycles ("C+n" above) and we only publish a cycle once all
	// of the events from that cycle must be done. Specifically:
	//
	// Mallocs are accounted to cycle C+2.
	// Explicit frees are accounted to cycle C+2.
	// GC frees (done during sweeping) are accounted to cycle C+1.
	//
	// After mark termination, we increment the global heap
	// profile cycle counter and accumulate the stats from cycle C
	// into the active profile.

	// active is the currently published profile. A profiling
	// cycle can be accumulated into active once its complete.
	active memRecordCycle

	// future records the profile events we're counting for cycles
	// that have not yet been published. This is ring buffer
	// indexed by the global heap profile cycle C and stores
	// cycles C, C+1, and C+2. Unlike active, these counts are
	// only for a single cycle; they are not cumulative across
	// cycles.
	//
	// We store cycle C here because there's a window between when
	// C becomes the active cycle and when we've flushed it to
	// active.
	future [3]memRecordCycle

	// gcDeadFrees and gcDeadFreeBytes accumulate frees for GODEBUG=gcdeadtrace.
	// Read-and-reset atomically at GC end.
	gcDeadFrees     uintptr
	gcDeadFreeBytes uintptr

	// Session allocation tracking is now done entirely via the session
	// table (gcDeadSessionTable) and per-object specials. Bucket-level
	// heuristic counters have been removed to avoid false positives
	// when non-session allocations share the same bucket.
}

// memRecordCycle
type memRecordCycle struct {
	allocs, frees           uintptr
	alloc_bytes, free_bytes uintptr
}

// add accumulates b into a. It does not zero b.
func (a *memRecordCycle) add(b *memRecordCycle) {
	a.allocs += b.allocs
	a.frees += b.frees
	a.alloc_bytes += b.alloc_bytes
	a.free_bytes += b.free_bytes
}

// A blockRecord is the bucket data for a bucket of type blockProfile,
// which is used in blocking and mutex profiles.
type blockRecord struct {
	count  float64
	cycles int64
}

var (
	mbuckets atomic.UnsafePointer // *bucket, memory profile buckets
	bbuckets atomic.UnsafePointer // *bucket, blocking profile buckets
	xbuckets atomic.UnsafePointer // *bucket, mutex profile buckets
	buckhash atomic.UnsafePointer // *buckhashArray

	mProfCycle mProfCycleHolder
)

type buckhashArray [buckHashSize]atomic.UnsafePointer // *bucket

const mProfCycleWrap = uint32(len(memRecord{}.future)) * (2 << 24)

// mProfCycleHolder holds the global heap profile cycle number (wrapped at
// mProfCycleWrap, stored starting at bit 1), and a flag (stored at bit 0) to
// indicate whether future[cycle] in all buckets has been queued to flush into
// the active profile.
type mProfCycleHolder struct {
	value atomic.Uint32
}

// read returns the current cycle count.
func (c *mProfCycleHolder) read() (cycle uint32) {
	v := c.value.Load()
	cycle = v >> 1
	return cycle
}

// setFlushed sets the flushed flag. It returns the current cycle count and the
// previous value of the flushed flag.
func (c *mProfCycleHolder) setFlushed() (cycle uint32, alreadyFlushed bool) {
	for {
		prev := c.value.Load()
		cycle = prev >> 1
		alreadyFlushed = (prev & 0x1) != 0
		next := prev | 0x1
		if c.value.CompareAndSwap(prev, next) {
			return cycle, alreadyFlushed
		}
	}
}

// increment increases the cycle count by one, wrapping the value at
// mProfCycleWrap. It clears the flushed flag.
func (c *mProfCycleHolder) increment() {
	// We explicitly wrap mProfCycle rather than depending on
	// uint wraparound because the memRecord.future ring does not
	// itself wrap at a power of two.
	for {
		prev := c.value.Load()
		cycle := prev >> 1
		cycle = (cycle + 1) % mProfCycleWrap
		next := cycle << 1
		if c.value.CompareAndSwap(prev, next) {
			break
		}
	}
}

// newBucket allocates a bucket with the given type and number of stack entries.
func newBucket(typ bucketType, nstk int) *bucket {
	size := unsafe.Sizeof(bucket{}) + uintptr(nstk)*unsafe.Sizeof(uintptr(0))
	switch typ {
	default:
		throw("invalid profile bucket type")
	case memProfile:
		size += unsafe.Sizeof(memRecord{})
	case blockProfile, mutexProfile:
		size += unsafe.Sizeof(blockRecord{})
	}

	b := (*bucket)(persistentalloc(size, 0, &memstats.buckhash_sys))
	b.typ = typ
	b.nstk = uintptr(nstk)
	return b
}

// stk returns the slice in b holding the stack. The caller can assume that the
// backing array is immutable.
func (b *bucket) stk() []uintptr {
	stk := (*[maxProfStackDepth]uintptr)(add(unsafe.Pointer(b), unsafe.Sizeof(*b)))
	if b.nstk > maxProfStackDepth {
		// prove that slicing works; otherwise a failure requires a P
		throw("bad profile stack count")
	}
	return stk[:b.nstk:b.nstk]
}

// mp returns the memRecord associated with the memProfile bucket b.
func (b *bucket) mp() *memRecord {
	if b.typ != memProfile {
		throw("bad use of bucket.mp")
	}
	data := add(unsafe.Pointer(b), unsafe.Sizeof(*b)+b.nstk*unsafe.Sizeof(uintptr(0)))
	return (*memRecord)(data)
}

// bp returns the blockRecord associated with the blockProfile bucket b.
func (b *bucket) bp() *blockRecord {
	if b.typ != blockProfile && b.typ != mutexProfile {
		throw("bad use of bucket.bp")
	}
	data := add(unsafe.Pointer(b), unsafe.Sizeof(*b)+b.nstk*unsafe.Sizeof(uintptr(0)))
	return (*blockRecord)(data)
}

// Return the bucket for stk[0:nstk], allocating new bucket if needed.
func stkbucket(typ bucketType, size uintptr, stk []uintptr, alloc bool) *bucket {
	bh := (*buckhashArray)(buckhash.Load())
	if bh == nil {
		lock(&profInsertLock)
		// check again under the lock
		bh = (*buckhashArray)(buckhash.Load())
		if bh == nil {
			bh = (*buckhashArray)(sysAlloc(unsafe.Sizeof(buckhashArray{}), &memstats.buckhash_sys, "profiler hash buckets"))
			if bh == nil {
				throw("runtime: cannot allocate memory")
			}
			buckhash.StoreNoWB(unsafe.Pointer(bh))
		}
		unlock(&profInsertLock)
	}

	// Hash stack.
	var h uintptr
	for _, pc := range stk {
		h += pc
		h += h << 10
		h ^= h >> 6
	}
	// hash in size
	h += size
	h += h << 10
	h ^= h >> 6
	// finalize
	h += h << 3
	h ^= h >> 11

	i := int(h % buckHashSize)
	// first check optimistically, without the lock
	for b := (*bucket)(bh[i].Load()); b != nil; b = b.next {
		if b.typ == typ && b.hash == h && b.size == size && eqslice(b.stk(), stk) {
			return b
		}
	}

	if !alloc {
		return nil
	}

	lock(&profInsertLock)
	// check again under the insertion lock
	for b := (*bucket)(bh[i].Load()); b != nil; b = b.next {
		if b.typ == typ && b.hash == h && b.size == size && eqslice(b.stk(), stk) {
			unlock(&profInsertLock)
			return b
		}
	}

	// Create new bucket.
	b := newBucket(typ, len(stk))
	copy(b.stk(), stk)
	b.hash = h
	b.size = size

	var allnext *atomic.UnsafePointer
	if typ == memProfile {
		allnext = &mbuckets
	} else if typ == mutexProfile {
		allnext = &xbuckets
	} else {
		allnext = &bbuckets
	}

	b.next = (*bucket)(bh[i].Load())
	b.allnext = (*bucket)(allnext.Load())

	bh[i].StoreNoWB(unsafe.Pointer(b))
	allnext.StoreNoWB(unsafe.Pointer(b))

	unlock(&profInsertLock)
	return b
}

func eqslice(x, y []uintptr) bool {
	if len(x) != len(y) {
		return false
	}
	for i, xi := range x {
		if xi != y[i] {
			return false
		}
	}
	return true
}

// mProf_NextCycle publishes the next heap profile cycle and creates a
// fresh heap profile cycle. This operation is fast and can be done
// during STW. The caller must call mProf_Flush before calling
// mProf_NextCycle again.
//
// This is called by mark termination during STW so allocations and
// frees after the world is started again count towards a new heap
// profiling cycle.
func mProf_NextCycle() {
	mProfCycle.increment()
}

// mProf_Flush flushes the events from the current heap profiling
// cycle into the active profile. After this it is safe to start a new
// heap profiling cycle with mProf_NextCycle.
//
// This is called by GC after mark termination starts the world. In
// contrast with mProf_NextCycle, this is somewhat expensive, but safe
// to do concurrently.
func mProf_Flush() {
	cycle, alreadyFlushed := mProfCycle.setFlushed()
	if alreadyFlushed {
		return
	}

	index := cycle % uint32(len(memRecord{}.future))
	lock(&profMemActiveLock)
	lock(&profMemFutureLock[index])
	mProf_FlushLocked(index)
	unlock(&profMemFutureLock[index])
	unlock(&profMemActiveLock)
}

// mProf_FlushLocked flushes the events from the heap profiling cycle at index
// into the active profile. The caller must hold the lock for the active profile
// (profMemActiveLock) and for the profiling cycle at index
// (profMemFutureLock[index]).
func mProf_FlushLocked(index uint32) {
	assertLockHeld(&profMemActiveLock)
	assertLockHeld(&profMemFutureLock[index])
	head := (*bucket)(mbuckets.Load())
	for b := head; b != nil; b = b.allnext {
		mp := b.mp()

		// Flush cycle C into the published profile and clear
		// it for reuse.
		mpc := &mp.future[index]
		mp.active.add(mpc)
		*mpc = memRecordCycle{}
	}
}

// mProf_PostSweep records that all sweep frees for this GC cycle have
// completed. This has the effect of publishing the heap profile
// snapshot as of the last mark termination without advancing the heap
// profile cycle.
func mProf_PostSweep() {
	// Flush cycle C+1 to the active profile so everything as of
	// the last mark termination becomes visible. *Don't* advance
	// the cycle, since we're still accumulating allocs in cycle
	// C+2, which have to become C+1 in the next mark termination
	// and so on.
	cycle := mProfCycle.read() + 1

	index := cycle % uint32(len(memRecord{}.future))
	lock(&profMemActiveLock)
	lock(&profMemFutureLock[index])
	mProf_FlushLocked(index)
	unlock(&profMemFutureLock[index])
	unlock(&profMemActiveLock)
}

// Called by malloc to record a profiled block.
func mProf_Malloc(mp *m, p unsafe.Pointer, size uintptr, typ *_type) {
	if mp.profStack == nil {
		// mp.profStack is nil if we happen to sample an allocation during the
		// initialization of mp. This case is rare, so we just ignore such
		// allocations. Change MemProfileRate to 1 if you need to reproduce such
		// cases for testing purposes.
		return
	}
	// Only use the part of mp.profStack we need and ignore the extra space
	// reserved for delayed inline expansion with frame pointer unwinding.
	nstk := callers(3, mp.profStack[:debug.profstackdepth+2])
	index := (mProfCycle.read() + 2) % uint32(len(memRecord{}.future))

	b := stkbucket(memProfile, size, mp.profStack[:nstk], true)
	mr := b.mp()
	mpc := &mr.future[index]

	lock(&profMemFutureLock[index])
	mpc.allocs++
	mpc.alloc_bytes += size
	unlock(&profMemFutureLock[index])

	if debug.gcdeadtrace > 0 {
		gp := mp.curg
		if gp != nil && gp.gcDeadSessionActive {
			// Record in the session table for per-session tracking.
			sid := gp.gcDeadSessionID
			idx := sid % gcDeadMaxSessions
			e := &gcDeadSessionTable[idx]
			if e.id == sid && !e.ended {
				atomic.Xadduintptr(&e.allocs, 1)
				atomic.Xadduintptr(&e.allocBytes, size)
				atomic.Xadduintptr(&e.cumAllocs, 1)
				atomic.Xadduintptr(&e.cumAllocBytes, size)

				// Resolve type name for alive output.
				tn := ""
				if typ != nil {
					tn = toRType(typ).string()
				}

				// Record alloc bucket ref for alive session attribution.
				bp := unsafe.Pointer(b)
				for j := range e.allocBucketRefs {
					if e.allocBucketRefs[j].bucket == bp {
						atomic.Xadduintptr(&e.allocBucketRefs[j].frees, 1)
						atomic.Xadduintptr(&e.allocBucketRefs[j].bytes, size)
						goto done
					}
				}
				for j := range e.allocBucketRefs {
					if e.allocBucketRefs[j].bucket == nil {
						e.allocBucketRefs[j].bucket = bp
						e.allocBucketRefs[j].frees = 1
						e.allocBucketRefs[j].bytes = size
						e.allocBucketRefs[j].typeName = tn
						goto done
					}
				}
				// All slots full: overwrite first (LRU-approximate).
				e.allocBucketRefs[0].bucket = bp
				e.allocBucketRefs[0].frees = 1
				e.allocBucketRefs[0].bytes = size
				e.allocBucketRefs[0].typeName = tn
			done:
			}
		}
	}

	// Setprofilebucket locks a bunch of other mutexes, so we call it outside of
	// the profiler locks. This reduces potential contention and chances of
	// deadlocks. Since the object must be alive during the call to
	// mProf_Malloc, it's fine to do this non-atomically.
	systemstack(func() {
		setprofilebucket(p, b)
	})

	// Add a gcdeadtrace session special to attribute freed objects
	// to the correct session. This enables per-session output in
	// gcDeadTracePrint.
	if debug.gcdeadtrace > 0 {
		gp := mp.curg
		if gp != nil && gp.gcDeadSessionActive {
			sid := gp.gcDeadSessionID
			idx := sid % gcDeadMaxSessions
			e := &gcDeadSessionTable[idx]
			if e.id == sid && !e.ended {
				lock(&mheap_.speciallock)
				ss := (*specialGcDeadSession)(mheap_.specialGcDeadSessionAlloc.alloc())
				unlock(&mheap_.speciallock)
				ss.special.kind = _KindSpecialGcDeadSession
				ss.sessionID = gp.gcDeadSessionID
				ss.b = b // store bucket for per-site session attribution in freed output
				ss.typ = typ // store type for type name in freed output
				if !addspecial(p, &ss.special, false) {
					// Already has this special — free the unused allocation.
					lock(&mheap_.speciallock)
					mheap_.specialGcDeadSessionAlloc.free(unsafe.Pointer(ss))
					unlock(&mheap_.speciallock)
				}
			}
		}
	}
}

// Called when freeing a profiled block.
func mProf_Free(b *bucket, size uintptr) {
	index := (mProfCycle.read() + 1) % uint32(len(memRecord{}.future))

	mp := b.mp()
	mpc := &mp.future[index]

	lock(&profMemFutureLock[index])
	mpc.frees++
	mpc.free_bytes += size
	if debug.gcdeadtrace > 0 {
		atomic.Xadduintptr(&mp.gcDeadFrees, 1)
		atomic.Xadduintptr(&mp.gcDeadFreeBytes, size)
	}
	unlock(&profMemFutureLock[index])
}

// gcDeadTraceMaxSites is the maximum number of distinct allocation sites
// reported per GC cycle. For complex sessions with many allocation points,
// this can be increased. Arrays are heap-allocated via persistentalloc
// on first use, so this consumes ~2 MB of persistent memory at runtime
// only when gcdeadtrace is enabled.
const gcDeadTraceMaxSites = 4096

// gcDeadTraceMaxFrames is the maximum number of user frames to
// distinguish call sites. Increasing this helps separate objects
// allocated by the same function called from different places.
const gcDeadTraceMaxFrames = 3

// gcDeadTraceBufSize is the output buffer size (1 MB).
const gcDeadTraceBufSize = 1 << 20

// gcDeadTrace storage types, defined at package level so persistentalloc
// can compute their size.
type gcDeadRawEntry struct {
	pcs              [gcDeadTraceMaxFrames]uintptr
	nframes          int
	frees            uintptr
	bytes            uintptr

	// Per-session freed attribution, cross-referenced from session table.
	sessionRefs     [gcDeadSessionRefSlots]gcDeadSessionRef
	numSessionRefs int

	// Per-session alive attribution, cross-referenced from session table.
	aliveSessionRefs     [gcDeadSessionRefSlots]gcDeadSessionRef
	numAliveSessionRefs int
}

type gcDeadSite struct {
	funcs [gcDeadTraceMaxFrames]struct {
		name string
		file string
		line int32
	}
	nframes int
	frees   uintptr
	bytes   uintptr

	// Per-session freed attribution.
	sessionRefs     [gcDeadSessionRefSlots]gcDeadSessionRef
	numSessionRefs int

	// Per-session alive attribution.
	aliveSessionRefs     [gcDeadSessionRefSlots]gcDeadSessionRef
	numAliveSessionRefs int
}

// Persistent storage for gcDeadTracePrint, allocated once on first use.
var (
	gcDeadRawData   *[gcDeadTraceMaxSites]gcDeadRawEntry
	gcDeadSitesData *[gcDeadTraceMaxSites]gcDeadSite
	gcDeadBufData   *[gcDeadTraceBufSize]byte
)

// gcDeadMaxSessions is the maximum number of concurrent gcdeadtrace sessions.
const gcDeadMaxSessions = 4096

// Number of distinct allocation sites tracked per session for per-site
// session attribution in gcdeadsession:freed output.
const gcDeadPerSessionSites = 256

// gcDeadSessionBucketRef tracks freed counts per (session, bucket) pair,
// enabling per-site output lines to show which session freed the objects.
type gcDeadSessionBucketRef struct {
	bucket       unsafe.Pointer // *bucket
	frees        uintptr        // per-cycle alloc/free ref count
	bytes        uintptr        // per-cycle bytes
	cumFrees     uintptr        // cumulative frees at this bucket from this session
	cumFreeBytes uintptr
	typeName     string         // type name (e.g. "[]byte"), set at allocation time
}

const gcDeadSessionRefSlots = 16

// Session attribution for a single site line in gcDeadTracePrint output.
type gcDeadSessionRef struct {
	sessionID uint64
	objs      uintptr // count of objects (freed or alive depending on context)
	bytes     uintptr
	typeName  string
}

// gcDeadSessionInfo tracks per-session allocation and free data.
// Entries persist after the session ends so that freed objects can still
// be attributed to the correct session.
type gcDeadSessionInfo struct {
	id         uint64
	goid       uint64   // first goroutine ID to join this session (reference only)
	startPC    uintptr  // PC of the first GcDeadSessionStart caller
	endPC      uintptr  // PC of the GcDeadSessionEnd caller (0 if session hasn't ended)
	printed    uint32   // 0=unprinted, 1=printed without end (re-print when end arrives), 2=printed with end
	ended      bool     // End has been called — no more allocs accepted
	joinCount  int32    // number of goroutines that have joined this session
	allocs     uintptr  // session allocs this GC cycle
	allocBytes uintptr
	frees      uintptr  // freed this GC cycle (from specials)
	freeBytes  uintptr
	cumAllocs  uintptr  // cumulative
	cumAllocBytes uintptr
	cumFrees   uintptr
	cumFreeBytes uintptr

	// Per-bucket freed tracking, populated by gcDeadRecordFree.
	// Used in gcDeadTracePrint to append session info to site lines.
	bucketRefs [gcDeadPerSessionSites]gcDeadSessionBucketRef

	// Per-bucket alloc tracking, populated by mProf_Malloc.
	// Used in gcDeadTracePrint to append session info to alive site lines.
	allocBucketRefs [gcDeadPerSessionSites]gcDeadSessionBucketRef
}

// Session tracking table. Entries are indexed by session ID % gcDeadMaxSessions.
var gcDeadSessionTable [gcDeadMaxSessions]gcDeadSessionInfo

// gcDeadRecordFree attributes a freed object to its session via sessionID and
// to its allocation site via bucket pointer.
// Called from freeSpecial when a _KindSpecialGcDeadSession special is freed.
func gcDeadRecordFree(sessionID uint64, size uintptr, b *bucket, typ *_type) {
	idx := sessionID % gcDeadMaxSessions
	e := &gcDeadSessionTable[idx]
	if e.id != sessionID {
		return
	}
	atomic.Xadduintptr(&e.frees, 1)
	atomic.Xadduintptr(&e.freeBytes, size)
	atomic.Xadduintptr(&e.cumFrees, 1)
	atomic.Xadduintptr(&e.cumFreeBytes, size)

	// Resolve type name for freed output.
	tn := ""
	if typ != nil {
		tn = toRType(typ).string()
	}

	// Track per-(session, bucket) freed counts for site attribution.
	if b != nil {
		bp := unsafe.Pointer(b)
		for j := range e.bucketRefs {
			// Fast path: matching bucket pointer.
			if e.bucketRefs[j].bucket == bp {
				atomic.Xadduintptr(&e.bucketRefs[j].frees, 1)
				atomic.Xadduintptr(&e.bucketRefs[j].bytes, size)
				goto updateAllocCumFrees
			}
		}
		// Slow path: find an empty slot or the first slot (LRU-evict).
		for j := range e.bucketRefs {
			if e.bucketRefs[j].bucket == nil {
				// Store the bucket pointer with release store; subsequent
				// loads in gcDeadTracePrint will observe it.
				e.bucketRefs[j].bucket = bp
				e.bucketRefs[j].frees = 1
				e.bucketRefs[j].bytes = size
				e.bucketRefs[j].typeName = tn
				goto updateAllocCumFrees
			}
		}
		// All slots full: write into first slot (approximate fallback).
		e.bucketRefs[0].bucket = bp
		e.bucketRefs[0].frees = 1
		e.bucketRefs[0].bytes = size
		e.bucketRefs[0].typeName = tn

	updateAllocCumFrees:
		// Also update allocBucketRefs.cumFrees for accurate per-site alive tracking.
		for j := range e.allocBucketRefs {
			if e.allocBucketRefs[j].bucket == bp {
				atomic.Xadduintptr(&e.allocBucketRefs[j].cumFrees, 1)
				atomic.Xadduintptr(&e.allocBucketRefs[j].cumFreeBytes, size)
				break
			}
		}
	}
}

// gcDeadTracePrint prints a summary of freed profiled objects grouped by
// allocation site. Called at the end of each GC cycle when GODEBUG=gcdeadtrace>0.
//
// Grouping uses up to gcDeadTraceMaxFrames non-runtime stack frames, so
// objects allocated by the same function from different call sites
// (e.g. B = A() vs C = A() at different lines) are distinguishable.
//
// Only session-specific output is printed. The gcdead: global section is
// not included; use GODEBUG=gcdeadtracefile to capture full output.
//
//	gcdeadsession:freed: N session objs (M bytes) freed from K sites
//	  funcName (file:line): N session objs, M session bytes
//	gcdeadsession:alive: N session objs (M bytes) still alive from K sites
//	  funcName (file:line): N session objs, M session bytes
//
// GODEBUG options:
//
//	GODEBUG=gcdeadtrace=1              enable gcdeadtrace (output to stderr)
//	GODEBUG=gcdeadtracefile=<path>     also append output to the given file
//	                                   (file is created if it doesn't exist)
func gcDeadTracePrint() {
	// Allocate persistent storage on first call.
	if gcDeadRawData == nil {
		rawSize := unsafe.Sizeof([gcDeadTraceMaxSites]gcDeadRawEntry{})
		p := persistentalloc(rawSize, 0, &memstats.other_sys)
		gcDeadRawData = (*[gcDeadTraceMaxSites]gcDeadRawEntry)(p)

		sitesSize := unsafe.Sizeof([gcDeadTraceMaxSites]gcDeadSite{})
		p2 := persistentalloc(sitesSize, 0, &memstats.other_sys)
		gcDeadSitesData = (*[gcDeadTraceMaxSites]gcDeadSite)(p2)

		p3 := persistentalloc(gcDeadTraceBufSize, 0, &memstats.other_sys)
		gcDeadBufData = (*[gcDeadTraceBufSize]byte)(p3)
	}

	raw := gcDeadRawData[:]
	sites := gcDeadSitesData[:]
	buf := gcDeadBufData[:]

	// Phase 1: collect raw bucket data under lock (no symbol lookup).
	// Session stats are now computed entirely from the session table,
	// not from bucket-level heuristic counters. See Phase 2 and Phase 4.
	rawCount := 0
	totalSessionFrees := uintptr(0)
	totalSessionBytes := uintptr(0)
	totalSessionAlive := uintptr(0)
	totalSessionAliveBytes := uintptr(0)

	lock(&profMemActiveLock)
	for b := (*bucket)(mbuckets.Load()); b != nil; b = b.allnext {
		mp := b.mp()
		f := atomic.Xchguintptr(&mp.gcDeadFrees, 0)
		b2 := atomic.Xchguintptr(&mp.gcDeadFreeBytes, 0)

		if f == 0 {
			continue
		}

		// Walk the stack to find up to gcDeadTraceMaxFrames non-runtime frames.
		stk := b.stk()
		var pcs [gcDeadTraceMaxFrames]uintptr
		nframes := 0
		for _, p := range stk {
			callPC := p
			if callPC > 1 {
				callPC--
			}
			fi := findfunc(callPC)
			if fi.valid() {
				name := funcname(fi)
				if len(name) >= 8 && name[:8] == "runtime." {
					continue
				}
				pcs[nframes] = callPC
				nframes++
				if nframes >= gcDeadTraceMaxFrames {
					break
				}
			}
		}
		if nframes == 0 && len(stk) > 0 {
			pcs[0] = stk[len(stk)-1]
			nframes = 1
		}

		// Merge with existing entry by the PC tuple.
		idx := -1
		for i := 0; i < rawCount; i++ {
			if raw[i].nframes != nframes {
				continue
			}
			match := true
			for j := 0; j < nframes; j++ {
				if raw[i].pcs[j] != pcs[j] {
					match = false
					break
				}
			}
			if match {
				idx = i
				break
			}
		}
		if idx >= 0 {
			raw[idx].frees += f
			raw[idx].bytes += b2
		} else if rawCount < gcDeadTraceMaxSites {
			raw[rawCount] = gcDeadRawEntry{
				pcs: pcs, nframes: nframes,
				frees: f, bytes: b2,
			}
			rawCount++
		}
	}
	unlock(&profMemActiveLock)

	// Cross-reference session table bucketRefs with raw entries.
	// This populates raw[].sessionRefs for per-session attribution in site lines.
	// If a bucket referenced by the session table has no raw entry, create one.
	for si := range gcDeadSessionTable {
		se := &gcDeadSessionTable[si]
		if se.id == 0 {
			continue
		}
	}
	for si := range gcDeadSessionTable {
		se := &gcDeadSessionTable[si]
		if se.id == 0 {
			continue
		}
		for _, br := range se.bucketRefs {
			if br.bucket == nil || br.frees == 0 {
				continue
			}
			bp := (*bucket)(br.bucket)
			// Extract non-runtime PCs from this bucket's stack.
			stk := bp.stk()
			var bpcs [gcDeadTraceMaxFrames]uintptr
			bNframes := 0
			for _, p := range stk {
				callPC := p
				if callPC > 1 {
					callPC--
				}
				fi := findfunc(callPC)
				if fi.valid() {
					name := funcname(fi)
					if len(name) >= 8 && name[:8] == "runtime." {
						continue
					}
					bpcs[bNframes] = callPC
					bNframes++
					if bNframes >= gcDeadTraceMaxFrames {
						break
					}
				}
			}
			if bNframes == 0 && len(stk) > 0 {
				bpcs[0] = stk[len(stk)-1]
				bNframes = 1
			}
			// Find matching raw entry by PC tuple. Create one if no match.
			found := false
			for ri := 0; ri < rawCount; ri++ {
				if raw[ri].nframes != bNframes {
					continue
				}
				match := true
				for j := 0; j < bNframes; j++ {
					if raw[ri].pcs[j] != bpcs[j] {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				// Found matching site. Add session attribution.
				r := &raw[ri]
				if r.numSessionRefs < len(r.sessionRefs) {
					ns := r.numSessionRefs
					r.sessionRefs[ns].sessionID = se.id
					r.sessionRefs[ns].objs = br.frees
					r.sessionRefs[ns].bytes = br.bytes
					r.sessionRefs[ns].typeName = br.typeName
					r.numSessionRefs++
				}
				found = true
				break
			}
			if !found && rawCount < gcDeadTraceMaxSites {
				// Create a new raw entry for this previously unseen bucket.
				raw[rawCount] = gcDeadRawEntry{
					pcs: bpcs, nframes: bNframes,
				}
				r := &raw[rawCount]
				r.sessionRefs[0].sessionID = se.id
				r.sessionRefs[0].objs = br.frees
				r.sessionRefs[0].bytes = br.bytes
				r.sessionRefs[0].typeName = br.typeName
				r.numSessionRefs = 1
				rawCount++
			}
		}
	}

	// Cross-reference session table allocBucketRefs with raw entries.
	// This populates raw[].aliveSessionRefs for alive session attribution.
	// Alive count = allocs (br.frees) - cumulative frees (br.cumFrees).
	for si := range gcDeadSessionTable {
		se := &gcDeadSessionTable[si]
		if se.id == 0 {
			continue
		}
		for _, br := range se.allocBucketRefs {
			if br.bucket == nil || br.frees == 0 {
				continue
			}
			bp := (*bucket)(br.bucket)
			stk := bp.stk()
			var bpcs [gcDeadTraceMaxFrames]uintptr
			bNframes := 0
			for _, p := range stk {
				callPC := p
				if callPC > 1 {
					callPC--
				}
				fi := findfunc(callPC)
				if fi.valid() {
					name := funcname(fi)
					if len(name) >= 8 && name[:8] == "runtime." {
						continue
					}
					bpcs[bNframes] = callPC
					bNframes++
					if bNframes >= gcDeadTraceMaxFrames {
						break
					}
				}
			}
			if bNframes == 0 && len(stk) > 0 {
				bpcs[0] = stk[len(stk)-1]
				bNframes = 1
			}
			found := false
			for ri := 0; ri < rawCount; ri++ {
				if raw[ri].nframes != bNframes {
					continue
				}
				match := true
				for j := 0; j < bNframes; j++ {
					if raw[ri].pcs[j] != bpcs[j] {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				// Found matching site. Add alive session attribution.
			// Alive = allocs (br.frees) - cumulative frees (br.cumFrees).
				aliveObjs := br.frees
				aliveBytes := br.bytes
				if br.cumFrees > 0 {
					if br.cumFrees < br.frees {
						aliveObjs = br.frees - br.cumFrees
						aliveBytes = br.bytes - br.cumFreeBytes
					} else {
						aliveObjs = 0
						aliveBytes = 0
					}
				}
				if aliveObjs > 0 {
					r := &raw[ri]
					if r.numAliveSessionRefs < len(r.aliveSessionRefs) {
						ns := r.numAliveSessionRefs
						r.aliveSessionRefs[ns].sessionID = se.id
						r.aliveSessionRefs[ns].objs = aliveObjs
						r.aliveSessionRefs[ns].bytes = aliveBytes
						r.aliveSessionRefs[ns].typeName = br.typeName
						r.numAliveSessionRefs++
					}
				}
				found = true
				break
			}
			if !found && rawCount < gcDeadTraceMaxSites {
				aliveObjs := br.frees
				aliveBytes := br.bytes
				if br.cumFrees > 0 {
					if br.cumFrees < br.frees {
						aliveObjs = br.frees - br.cumFrees
						aliveBytes = br.bytes - br.cumFreeBytes
					} else {
						aliveObjs = 0
						aliveBytes = 0
					}
				}
				if aliveObjs > 0 {
					// Create a new raw entry for this previously unseen bucket.
					raw[rawCount] = gcDeadRawEntry{
						pcs: bpcs, nframes: bNframes,
					}
					r := &raw[rawCount]
					r.aliveSessionRefs[0].sessionID = se.id
					r.aliveSessionRefs[0].objs = aliveObjs
					r.aliveSessionRefs[0].bytes = aliveBytes
					r.aliveSessionRefs[0].typeName = br.typeName
					r.numAliveSessionRefs = 1
					rawCount++
				}
			}
		}
	}

	// Clear per-cycle bucketRefs and allocBucketRefs between GC cycles.
	for si := range gcDeadSessionTable {
		se := &gcDeadSessionTable[si]
		if se.id == 0 {
			continue
		}
		for j := range se.bucketRefs {
			if se.bucketRefs[j].bucket != nil {
				if atomic.Loaduintptr(&se.bucketRefs[j].frees) > 0 {
					atomic.Storeuintptr(&se.bucketRefs[j].frees, 0)
					atomic.Storeuintptr(&se.bucketRefs[j].bytes, 0)
				}
			}
		}
		for j := range se.allocBucketRefs {
			if se.allocBucketRefs[j].bucket != nil {
				atomic.Storeuintptr(&se.allocBucketRefs[j].frees, 0)
				atomic.Storeuintptr(&se.allocBucketRefs[j].bytes, 0)
			}
		}
	}

	// Phase 2: resolve PCs and merge by the full site key.
	siteCount := 0

	for i := 0; i < rawCount; i++ {
		r := &raw[i]
		key := [gcDeadTraceMaxFrames]struct {
			name string
			file string
			line int32
		}{}
		for j := 0; j < r.nframes; j++ {
			key[j].name = "?"
			key[j].file = "?"
			if r.pcs[j] != 0 {
				fi := findfunc(r.pcs[j])
				if fi.valid() {
					key[j].name = funcname(fi)
					key[j].file, key[j].line = funcline(fi, r.pcs[j])
				}
			}
		}

		idx := -1
		for j := 0; j < siteCount; j++ {
			if sites[j].nframes != r.nframes {
				continue
			}
			match := true
			for k := 0; k < r.nframes; k++ {
				if sites[j].funcs[k].name != key[k].name ||
					sites[j].funcs[k].file != key[k].file ||
					sites[j].funcs[k].line != key[k].line {
					match = false
					break
				}
			}
			if match {
				idx = j
				break
			}
		}
		if idx >= 0 {
			sites[idx].frees += r.frees
			sites[idx].bytes += r.bytes
			// Merge session refs.
			for ri := 0; ri < r.numSessionRefs; ri++ {
				rr := &r.sessionRefs[ri]
				found := false
				for si := 0; si < sites[idx].numSessionRefs; si++ {
					if sites[idx].sessionRefs[si].sessionID == rr.sessionID {
						sites[idx].sessionRefs[si].objs += rr.objs
						sites[idx].sessionRefs[si].bytes += rr.bytes
						found = true
						break
					}
				}
				if !found && sites[idx].numSessionRefs < len(sites[idx].sessionRefs) {
					ns := sites[idx].numSessionRefs
					sites[idx].sessionRefs[ns] = *rr
					sites[idx].numSessionRefs++
				}
			}
			// Merge alive session refs.
			for ri := 0; ri < r.numAliveSessionRefs; ri++ {
				rr := &r.aliveSessionRefs[ri]
				found := false
				for si := 0; si < sites[idx].numAliveSessionRefs; si++ {
					if sites[idx].aliveSessionRefs[si].sessionID == rr.sessionID {
						sites[idx].aliveSessionRefs[si].objs += rr.objs
						sites[idx].aliveSessionRefs[si].bytes += rr.bytes
						found = true
						break
					}
				}
				if !found && sites[idx].numAliveSessionRefs < len(sites[idx].aliveSessionRefs) {
					ns := sites[idx].numAliveSessionRefs
					sites[idx].aliveSessionRefs[ns] = *rr
					sites[idx].numAliveSessionRefs++
				}
			}
		} else if siteCount < gcDeadTraceMaxSites {
			sites[siteCount] = gcDeadSite{
				funcs:               key,
				nframes:             r.nframes,
				frees:               r.frees,
				bytes:               r.bytes,
				numSessionRefs:      r.numSessionRefs,
				numAliveSessionRefs: r.numAliveSessionRefs,
			}
			for ri := 0; ri < r.numSessionRefs; ri++ {
				sites[siteCount].sessionRefs[ri] = r.sessionRefs[ri]
			}
			for ri := 0; ri < r.numAliveSessionRefs; ri++ {
				sites[siteCount].aliveSessionRefs[ri] = r.aliveSessionRefs[ri]
			}
			siteCount++
		}
	}

	// Phase 3: sort by bytes freed, descending (insertion sort).
	for i := 1; i < siteCount; i++ {
		tmp := sites[i]
		j := i
		for j > 0 && sites[j-1].bytes < tmp.bytes {
			sites[j] = sites[j-1]
			j--
		}
		sites[j] = tmp
	}

	// Phase 4: build output into a buffer.
	n := 0

	appendStr := func(s string) {
		m := copy(buf[n:], s)
		n += m
	}

	appendUintptr := func(v uintptr) {
		if v == 0 {
			buf[n] = '0'
			n++
			return
		}
		var tmp [20]byte
		b := itoa(tmp[:], uint64(v))
		m := copy(buf[n:], b)
		n += m
	}

	appendPCLoc := func(pc uintptr) {
		fi := findfunc(pc)
		if fi.valid() {
			file, line := funcline(fi, pc)
			var tmp [20]byte
			b := itoa(tmp[:], uint64(line))
			n += copy(buf[n:], file)
			buf[n] = ':'
			n++
			n += copy(buf[n:], b)
		} else {
			n += copy(buf[n:], "?:?")
		}
	}

	// First pass: accumulate totals from the session table so we know
	// whether there is any session data to output. The totals are also
	// used for the GC separator and freed/alive summary lines.
	for i := range gcDeadSessionTable {
		e := &gcDeadSessionTable[i]
		if e.id == 0 {
			continue
		}
		frees := atomic.Loaduintptr(&e.frees)
		freeBytes := atomic.Loaduintptr(&e.freeBytes)
		cumAllocs := atomic.Loaduintptr(&e.cumAllocs)
		cumAllocBytes := atomic.Loaduintptr(&e.cumAllocBytes)
		cumFrees := atomic.Loaduintptr(&e.cumFrees)
		cumFreeBytes := atomic.Loaduintptr(&e.cumFreeBytes)

		alive := uintptr(0)
		aliveBytes := uintptr(0)
		if cumAllocs > cumFrees {
			alive = cumAllocs - cumFrees
			aliveBytes = cumAllocBytes - cumFreeBytes
		}

		// Accumulate totals.
		totalSessionFrees += frees
		totalSessionBytes += freeBytes
		totalSessionAlive += alive
		totalSessionAliveBytes += aliveBytes
	}

	// Add a separator with GC cycle number so outputs are distinguishable.
	if totalSessionFrees > 0 || totalSessionAlive > 0 {
		appendStr("=== GC #")
		appendUintptr(uintptr(memstats.numgc))
		appendStr(" ===\n")
	}

	// Second pass: per-session breakdown output. Reads from the session table,
	// which tracks each session independently via per-object specials.
	hasSessionData := false
	for i := range gcDeadSessionTable {
		e := &gcDeadSessionTable[i]
		if e.id == 0 {
			continue
		}
		allocs := atomic.Loaduintptr(&e.allocs)
		allocBytes := atomic.Loaduintptr(&e.allocBytes)
		frees := atomic.Loaduintptr(&e.frees)
		freeBytes := atomic.Loaduintptr(&e.freeBytes)
		cumAllocs := atomic.Loaduintptr(&e.cumAllocs)
		cumAllocBytes := atomic.Loaduintptr(&e.cumAllocBytes)
		cumFrees := atomic.Loaduintptr(&e.cumFrees)
		cumFreeBytes := atomic.Loaduintptr(&e.cumFreeBytes)

		alive := uintptr(0)
		aliveBytes := uintptr(0)
		if cumAllocs > cumFrees {
			alive = cumAllocs - cumFrees
			aliveBytes = cumAllocBytes - cumFreeBytes
		}

		// Skip sessions that were already printed in a previous GC cycle
		// and have no new per-cycle allocation activity.
		// Re-print a session if it was previously printed without an end
		// (printed==1) and now has an end (endPC!=0).
		printed := atomic.Load(&e.printed)
		endPC := e.endPC
		if allocs > 0 || (alive > 0 && printed == 0) || (endPC != 0 && printed == 1) {
			if !hasSessionData {
				appendStr("gcdeadsession by session:\n")
				hasSessionData = true
			}
			// If session has an end, mark as fully printed (2) so it won't
			// be re-printed. Otherwise mark as printed-without-end (1) so it
			// gets re-printed in a future cycle when endPC becomes set.
			if endPC != 0 {
				atomic.Store(&e.printed, 2)
			} else {
				atomic.Store(&e.printed, 1)
			}
			appendStr("  session #")
			var tmp [20]byte
			b := itoa(tmp[:], e.id)
			m := copy(buf[n:], b)
			n += m
			appendStr(": ")
			appendUintptr(allocs)
			appendStr(" allocs (")
			appendUintptr(allocBytes)
			appendStr(" bytes), ")
			appendUintptr(frees)
			appendStr(" freed (")
			appendUintptr(freeBytes)
			appendStr(" bytes), ")
			appendUintptr(alive)
			appendStr(" alive (")
			appendUintptr(aliveBytes)
			appendStr(" bytes)")
			// Append start/end location if available.
			if e.startPC != 0 {
				appendStr(" [start: ")
				appendPCLoc(e.startPC)
				if e.endPC != 0 {
					appendStr(", end: ")
					appendPCLoc(e.endPC)
				}
				appendStr("]")
			}
			appendStr("\n")
		}

		// Reset per-cycle counters; keep cumulative counters.
		if allocs > 0 || frees > 0 {
			atomic.Storeuintptr(&e.allocs, 0)
			atomic.Storeuintptr(&e.allocBytes, 0)
			atomic.Storeuintptr(&e.frees, 0)
			atomic.Storeuintptr(&e.freeBytes, 0)
		}
	}

	if totalSessionFrees == 0 && totalSessionAlive == 0 {
		return
	}

	// Session freed report: objects allocated in session that have been freed.
	if totalSessionFrees > 0 {
		sessionSiteCount := uintptr(0)
		for i := 0; i < siteCount; i++ {
			if sites[i].numSessionRefs > 0 {
				sessionSiteCount++
			}
		}

		appendStr("gcdeadsession:freed: ")
		appendUintptr(totalSessionFrees)
		appendStr(" session objs (")
		appendUintptr(totalSessionBytes)
		appendStr(" bytes) freed from ")
		appendUintptr(sessionSiteCount)
		appendStr(" sites\n")

		for i := 0; i < siteCount; i++ {
			s := &sites[i]
			if s.numSessionRefs == 0 {
				continue
			}
			// Compute per-site freed count from sessionRefs.
			siteFrees := uintptr(0)
			siteBytes := uintptr(0)
			for ri := 0; ri < s.numSessionRefs; ri++ {
				siteFrees += s.sessionRefs[ri].objs
				siteBytes += s.sessionRefs[ri].bytes
			}
			var linetmp [20]byte

			appendStr("  ")
			for j := 0; j < s.nframes; j++ {
				if j > 0 {
					appendStr(" < ")
				}
				lb := itoa(linetmp[:], uint64(s.funcs[j].line))
				appendStr(s.funcs[j].name)
				appendStr(" (")
				appendStr(s.funcs[j].file)
				appendStr(":")
				appendStr(string(lb))
				appendStr(")")
			}

			appendStr(": ")
			appendUintptr(siteFrees)
			appendStr(" session objs, ")
			appendUintptr(siteBytes)
			appendStr(" session bytes")
			// Append per-session attribution.
			for ri := 0; ri < s.numSessionRefs; ri++ {
				r := &s.sessionRefs[ri]
				appendStr(" [session #")
				var tmp2 [20]byte
				b := itoa(tmp2[:], r.sessionID)
				m := copy(buf[n:], b)
				n += m
				appendStr(": ")
				appendUintptr(r.objs)
				appendStr(" objs, ")
				appendUintptr(r.bytes)
				appendStr(" bytes")
				if r.typeName != "" {
					appendStr(" @")
					appendStr(r.typeName)
				}
				appendStr("]")
			}
			appendStr("\n")
		}
	}

	// Session alive report: objects allocated in session that are still alive.
	if totalSessionAlive > 0 {
		sessionSiteCount := uintptr(0)
		for i := 0; i < siteCount; i++ {
			if sites[i].numAliveSessionRefs > 0 {
				sessionSiteCount++
			}
		}

		appendStr("gcdeadsession:alive: ")
		appendUintptr(totalSessionAlive)
		appendStr(" session objs (")
		appendUintptr(totalSessionAliveBytes)
		appendStr(" bytes) still alive from ")
		appendUintptr(sessionSiteCount)
		appendStr(" sites\n")

		for i := 0; i < siteCount; i++ {
			s := &sites[i]
			sAlive := uintptr(0)
			sAliveBytes := uintptr(0)
			// Compute per-site alive count from aliveSessionRefs.
			for ri := 0; ri < s.numAliveSessionRefs; ri++ {
				sAlive += s.aliveSessionRefs[ri].objs
				sAliveBytes += s.aliveSessionRefs[ri].bytes
			}
			if sAlive == 0 {
				continue
			}
			var linetmp [20]byte

			appendStr("  ")
			for j := 0; j < s.nframes; j++ {
				if j > 0 {
					appendStr(" < ")
				}
				lb := itoa(linetmp[:], uint64(s.funcs[j].line))
				appendStr(s.funcs[j].name)
				appendStr(" (")
				appendStr(s.funcs[j].file)
				appendStr(":")
				appendStr(string(lb))
				appendStr(")")
			}

			appendStr(": ")
			appendUintptr(sAlive)
			appendStr(" session objs, ")
			appendUintptr(sAliveBytes)
			appendStr(" session bytes")
			// Append per-session alive attribution.
			for ri := 0; ri < s.numAliveSessionRefs; ri++ {
				r := &s.aliveSessionRefs[ri]
				appendStr(" [session #")
				var tmp2 [20]byte
				b := itoa(tmp2[:], r.sessionID)
				m := copy(buf[n:], b)
				n += m
				appendStr(": ")
				appendUintptr(r.objs)
				appendStr(" objs, ")
				appendUintptr(r.bytes)
				appendStr(" bytes")
				if r.typeName != "" {
					appendStr(" @")
					appendStr(r.typeName)
				}
				appendStr("]")
			}
			appendStr("\n")
		}
	}

	// Phase 5: write output.
	printlock()
	write(2, unsafe.Pointer(&buf[0]), int32(n))
	printunlock()

	if debug.gcdeadtracefile != "" {
		writeDeadTraceToFile(debug.gcdeadtracefile, buf[:n])
	}
}

// GcDeadSessionStart begins a gcdeadtrace session for the calling goroutine.
// While in a session, all allocations made by this goroutine are tracked
// and reported separately as gcdeadsession: output at the end of each GC cycle.
// Has no effect if GODEBUG=gcdeadtrace=0.
//
// MemProfileRate is automatically set to 1 when the first session starts,
// ensuring all session allocations are tracked. It is restored to the
// original value when the last session ends.
//
// Multiple simultaneous sessions are tracked independently. Each session
// is assigned a unique ID and freed objects are attributed to the correct
// session via per-object metadata.
//
// Example:
//
//	runtime.GcDeadSessionStart()
//	obj := allocate()   // tracked as session allocation
//	runtime.GcDeadSessionEnd()
//
// Use GODEBUG=gcdeadtracefile=<path> to save output to a file.
func GcDeadSessionStart(id uint64) {
	if debug.gcdeadtrace == 0 {
		return
	}
	gp := getg().m.curg
	if gp == nil || gp.gcDeadSessionActive {
		return
	}

	idx := id % gcDeadMaxSessions
	e := &gcDeadSessionTable[idx]

	// If session has already ended, don't join.
	if e.ended {
		return
	}

	// If this is the first time this sessionId is being used, initialize entry.
	if e.id != id {
		e.id = id
		e.goid = gp.goid
		e.startPC = sys.GetCallerPC()
		e.endPC = 0
		e.ended = false
		e.printed = 0
		e.joinCount = 0
		e.allocs = 0
		e.allocBytes = 0
		e.frees = 0
		e.freeBytes = 0
		e.cumAllocs = 0
		e.cumAllocBytes = 0
		e.cumFrees = 0
		e.cumFreeBytes = 0
		// Clear bucket refs from any previous session.
		for j := range e.bucketRefs {
			e.bucketRefs[j] = gcDeadSessionBucketRef{}
		}
		for j := range e.allocBucketRefs {
			e.allocBucketRefs[j] = gcDeadSessionBucketRef{}
		}
	}

	gp.gcDeadSessionActive = true
	gp.gcDeadSessionID = id
	atomic.Xaddint32(&e.joinCount, 1)

	// Ensure all allocations are profiled for accurate session tracking.
	// MemProfileRate is set to 1 on first session start so that every
	// allocation reaches mProf_Malloc and session counters are updated.
	if gcDeadSessionCount.Add(1) == 1 {
		gcDeadSavedRate = MemProfileRate
		MemProfileRate = 1
	}
}

// GcDeadSessionEnd marks a gcdeadtrace session as ended by sessionId.
// After this call, no more allocations can be attributed to this session.
// Has no effect if the session is already ended or if GODEBUG=gcdeadtrace=0.
// Can only be called once per sessionId — subsequent calls are no-ops.
func GcDeadSessionEnd(id uint64) {
	if debug.gcdeadtrace == 0 {
		return
	}
	gp := getg().m.curg
	if gp == nil || !gp.gcDeadSessionActive || gp.gcDeadSessionID != id {
		return
	}

	idx := id % gcDeadMaxSessions
	e := &gcDeadSessionTable[idx]
	if e.id != id || e.ended {
		return
	}

	// Record the caller's PC for end location output.
	e.endPC = sys.GetCallerPC()
	e.ended = true

	// Clear the calling goroutine's session state.
	gp.gcDeadSessionActive = false
	gp.gcDeadSessionID = 0

	// Adjust global session count by all goroutines that joined this session.
	jc := atomic.Xchgint32(&e.joinCount, 0)
	if gcDeadSessionCount.Add(-jc) <= 0 {
		MemProfileRate = gcDeadSavedRate
	}
}

// gcDeadSessionCount tracks the number of active gcdeadtrace sessions.
// Used to manage MemProfileRate automatically.
var gcDeadSessionCount atomic.Int32

// gcDeadSavedRate holds the MemProfileRate value before the first session
// started, so it can be restored when all sessions end.
var gcDeadSavedRate int

// gcDeadTraceFileCreated tracks whether the gcdeadtracefile creation success
// message has been printed, so we don't spam stderr on every GC cycle.
var gcDeadTraceFileCreated bool

var blockprofilerate uint64 // in CPU ticks

// SetBlockProfileRate controls the fraction of goroutine blocking events
// that are reported in the blocking profile. The profiler aims to sample
// an average of one blocking event per rate nanoseconds spent blocked.
//
// To include every blocking event in the profile, pass rate = 1.
// To turn off profiling entirely, pass rate <= 0.
func SetBlockProfileRate(rate int) {
	var r int64
	if rate <= 0 {
		r = 0 // disable profiling
	} else if rate == 1 {
		r = 1 // profile everything
	} else {
		// convert ns to cycles, use float64 to prevent overflow during multiplication
		r = int64(float64(rate) * float64(ticksPerSecond()) / (1000 * 1000 * 1000))
		if r == 0 {
			r = 1
		}
	}

	atomic.Store64(&blockprofilerate, uint64(r))
}

func blockevent(cycles int64, skip int) {
	if cycles <= 0 {
		cycles = 1
	}

	rate := int64(atomic.Load64(&blockprofilerate))
	if blocksampled(cycles, rate) {
		saveblockevent(cycles, rate, skip+1, blockProfile)
	}
}

// blocksampled returns true for all events where cycles >= rate. Shorter
// events have a cycles/rate random chance of returning true.
func blocksampled(cycles, rate int64) bool {
	if rate <= 0 || (rate > cycles && cheaprand64()%rate > cycles) {
		return false
	}
	return true
}

// saveblockevent records a profile event of the type specified by which.
// cycles is the quantity associated with this event and rate is the sampling rate,
// used to adjust the cycles value in the manner determined by the profile type.
// skip is the number of frames to omit from the traceback associated with the event.
// The traceback will be recorded from the stack of the goroutine associated with the current m.
// skip should be positive if this event is recorded from the current stack
// (e.g. when this is not called from a system stack)
func saveblockevent(cycles, rate int64, skip int, which bucketType) {
	if debug.profstackdepth == 0 {
		// profstackdepth is set to 0 by the user, so mp.profStack is nil and we
		// can't record a stack trace.
		return
	}
	if skip > maxSkip {
		print("requested skip=", skip)
		throw("invalid skip value")
	}
	gp := getg()
	mp := acquirem() // we must not be preempted while accessing profstack

	var nstk int
	if tracefpunwindoff() || gp.m.hasCgoOnStack() {
		if gp.m.curg == nil || gp.m.curg == gp {
			nstk = callers(skip, mp.profStack)
		} else {
			nstk = gcallers(gp.m.curg, skip, mp.profStack)
		}
	} else {
		if gp.m.curg == nil || gp.m.curg == gp {
			if skip > 0 {
				// We skip one fewer frame than the provided value for frame
				// pointer unwinding because the skip value includes the current
				// frame, whereas the saved frame pointer will give us the
				// caller's return address first (so, not including
				// saveblockevent)
				skip -= 1
			}
			nstk = fpTracebackPartialExpand(skip, unsafe.Pointer(getfp()), mp.profStack)
		} else {
			mp.profStack[0] = gp.m.curg.sched.pc
			nstk = 1 + fpTracebackPartialExpand(skip, unsafe.Pointer(gp.m.curg.sched.bp), mp.profStack[1:])
		}
	}

	saveBlockEventStack(cycles, rate, mp.profStack[:nstk], which)
	releasem(mp)
}

// fpTracebackPartialExpand records a call stack obtained starting from fp.
// This function will skip the given number of frames, properly accounting for
// inlining, and save remaining frames as "physical" return addresses. The
// consumer should later use CallersFrames or similar to expand inline frames.
func fpTracebackPartialExpand(skip int, fp unsafe.Pointer, pcBuf []uintptr) int {
	var n int
	lastFuncID := abi.FuncIDNormal
	skipOrAdd := func(retPC uintptr) bool {
		if skip > 0 {
			skip--
		} else if n < len(pcBuf) {
			pcBuf[n] = retPC
			n++
		}
		return n < len(pcBuf)
	}
	for n < len(pcBuf) && fp != nil {
		// return addr sits one word above the frame pointer
		pc := *(*uintptr)(unsafe.Pointer(uintptr(fp) + goarch.PtrSize))

		if skip > 0 {
			callPC := pc - 1
			fi := findfunc(callPC)
			u, uf := newInlineUnwinder(fi, callPC)
			for ; uf.valid(); uf = u.next(uf) {
				sf := u.srcFunc(uf)
				if sf.funcID == abi.FuncIDWrapper && elideWrapperCalling(lastFuncID) {
					// ignore wrappers
				} else if more := skipOrAdd(uf.pc + 1); !more {
					return n
				}
				lastFuncID = sf.funcID
			}
		} else {
			// We've skipped the desired number of frames, so no need
			// to perform further inline expansion now.
			pcBuf[n] = pc
			n++
		}

		// follow the frame pointer to the next one
		fp = unsafe.Pointer(*(*uintptr)(fp))
	}
	return n
}

// mLockProfile holds information about the runtime-internal lock contention
// experienced and caused by this M, to report in metrics and profiles.
//
// These measurements are subject to some notable constraints: First, the fast
// path for lock and unlock must remain very fast, with a minimal critical
// section. Second, the critical section during contention has to remain small
// too, so low levels of contention are less likely to snowball into large ones.
// The reporting code cannot acquire new locks until the M has released all
// other locks, which means no memory allocations and encourages use of
// (temporary) M-local storage.
//
// The M has space for storing one call stack that caused contention, and the
// magnitude of that contention. It also has space to store the magnitude of
// additional contention the M caused, since it might encounter several
// contention events before it releases all of its locks and is thus able to
// transfer the locally buffered call stack and magnitude into the profile.
//
// The M collects the call stack when it unlocks the contended lock. The
// traceback takes place outside of the lock's critical section.
//
// The profile for contention on sync.Mutex blames the caller of Unlock for the
// amount of contention experienced by the callers of Lock which had to wait.
// When there are several critical sections, this allows identifying which of
// them is responsible. We must match that reporting behavior for contention on
// runtime-internal locks.
//
// When the M unlocks its last mutex, it transfers the locally buffered call
// stack and magnitude into the profile. As part of that step, it also transfers
// any "additional contention" time to the profile. Any lock contention that it
// experiences while adding samples to the profile will be recorded later as
// "additional contention" and not include a call stack, to avoid an echo.
type mLockProfile struct {
	waitTime   atomic.Int64 // (nanotime) total time this M has spent waiting in runtime.lockWithRank. Read by runtime/metrics.
	stack      []uintptr    // call stack at the point of this M's unlock call, when other Ms had to wait
	cycles     int64        // (cputicks) cycles attributable to "stack"
	cyclesLost int64        // (cputicks) contention for which we weren't able to record a call stack
	haveStack  bool         // stack and cycles are to be added to the mutex profile (even if cycles is 0)
	disabled   bool         // attribute all time to "lost"
}

func (prof *mLockProfile) start() int64 {
	if cheaprandn(gTrackingPeriod) == 0 {
		return nanotime()
	}
	return 0
}

func (prof *mLockProfile) end(start int64) {
	if start != 0 {
		prof.waitTime.Add((nanotime() - start) * gTrackingPeriod)
	}
}

// recordUnlock prepares data for later addition to the mutex contention
// profile. The M may hold arbitrary locks during this call.
//
// From unlock2, we might not be holding a p in this code.
//
//go:nowritebarrierrec
func (prof *mLockProfile) recordUnlock(cycles int64) {
	if cycles < 0 {
		cycles = 0
	}

	if prof.disabled {
		// We're experiencing contention while attempting to report contention.
		// Make a note of its magnitude, but don't allow it to be the sole cause
		// of another contention report.
		prof.cyclesLost += cycles
		return
	}

	if prev := prof.cycles; prev > 0 {
		// We can only store one call stack for runtime-internal lock contention
		// on this M, and we've already got one. Decide which should stay, and
		// add the other to the report for runtime._LostContendedRuntimeLock.
		if cycles == 0 {
			return
		}
		prevScore := cheaprandu64() % uint64(prev)
		thisScore := cheaprandu64() % uint64(cycles)
		if prevScore > thisScore {
			prof.cyclesLost += cycles
			return
		} else {
			prof.cyclesLost += prev
		}
	}
	prof.captureStack()
	prof.cycles = cycles
}

func (prof *mLockProfile) captureStack() {
	if debug.profstackdepth == 0 {
		// profstackdepth is set to 0 by the user, so mp.profStack is nil and we
		// can't record a stack trace.
		return
	}

	skip := 4 // runtime.(*mLockProfile).recordUnlock runtime.unlock2Wake runtime.unlock2 runtime.unlockWithRank
	if staticLockRanking {
		// When static lock ranking is enabled, we'll always be on the system
		// stack at this point. There will be a runtime.unlockWithRank.func1
		// frame, and if the call to runtime.unlock took place on a user stack
		// then there'll also be a runtime.systemstack frame. To keep stack
		// traces somewhat consistent whether or not static lock ranking is
		// enabled, we'd like to skip those. But it's hard to tell how long
		// we've been on the system stack so accept an extra frame in that case,
		// with a leaf of "runtime.unlockWithRank runtime.unlock" instead of
		// "runtime.unlock".
		skip += 1 // runtime.unlockWithRank.func1
	}
	prof.haveStack = true

	var nstk int
	gp := getg()
	sp := sys.GetCallerSP()
	pc := sys.GetCallerPC()
	systemstack(func() {
		var u unwinder
		u.initAt(pc, sp, 0, gp, unwindSilentErrors|unwindJumpStack)
		nstk = tracebackPCs(&u, skip, prof.stack)
	})
	if nstk < len(prof.stack) {
		prof.stack[nstk] = 0
	}
}

// store adds the M's local record to the mutex contention profile.
//
// From unlock2, we might not be holding a p in this code.
//
//go:nowritebarrierrec
func (prof *mLockProfile) store() {
	if gp := getg(); gp.m.locks == 1 && gp.m.mLockProfile.haveStack {
		prof.storeSlow()
	}
}

func (prof *mLockProfile) storeSlow() {
	// Report any contention we experience within this function as "lost"; it's
	// important that the act of reporting a contention event not lead to a
	// reportable contention event. This also means we can use prof.stack
	// without copying, since it won't change during this function.
	mp := acquirem()
	prof.disabled = true

	nstk := int(debug.profstackdepth)
	for i := 0; i < nstk; i++ {
		if pc := prof.stack[i]; pc == 0 {
			nstk = i
			break
		}
	}

	cycles, lost := prof.cycles, prof.cyclesLost
	prof.cycles, prof.cyclesLost = 0, 0
	prof.haveStack = false

	rate := int64(atomic.Load64(&mutexprofilerate))
	saveBlockEventStack(cycles, rate, prof.stack[:nstk], mutexProfile)
	if lost > 0 {
		lostStk := [...]uintptr{
			abi.FuncPCABIInternal(_LostContendedRuntimeLock) + sys.PCQuantum,
		}
		saveBlockEventStack(lost, rate, lostStk[:], mutexProfile)
	}

	prof.disabled = false
	releasem(mp)
}

func saveBlockEventStack(cycles, rate int64, stk []uintptr, which bucketType) {
	b := stkbucket(which, 0, stk, true)
	bp := b.bp()

	lock(&profBlockLock)
	// We want to up-scale the count and cycles according to the
	// probability that the event was sampled. For block profile events,
	// the sample probability is 1 if cycles >= rate, and cycles / rate
	// otherwise. For mutex profile events, the sample probability is 1 / rate.
	// We scale the events by 1 / (probability the event was sampled).
	if which == blockProfile && cycles < rate {
		// Remove sampling bias, see discussion on http://golang.org/cl/299991.
		bp.count += float64(rate) / float64(cycles)
		bp.cycles += rate
	} else if which == mutexProfile {
		bp.count += float64(rate)
		bp.cycles += rate * cycles
	} else {
		bp.count++
		bp.cycles += cycles
	}
	unlock(&profBlockLock)
}

var mutexprofilerate uint64 // fraction sampled

// SetMutexProfileFraction controls the fraction of mutex contention events
// that are reported in the mutex profile. On average 1/rate events are
// reported. The previous rate is returned.
//
// To turn off profiling entirely, pass rate 0.
// To just read the current rate, pass rate < 0.
// (For n>1 the details of sampling may change.)
func SetMutexProfileFraction(rate int) int {
	if rate < 0 {
		return int(mutexprofilerate)
	}
	old := mutexprofilerate
	atomic.Store64(&mutexprofilerate, uint64(rate))
	return int(old)
}

func mutexevent(cycles int64, skip int) {
	if cycles < 0 {
		cycles = 0
	}
	rate := int64(atomic.Load64(&mutexprofilerate))
	if rate > 0 && cheaprand64()%rate == 0 {
		saveblockevent(cycles, rate, skip+1, mutexProfile)
	}
}

// Go interface to profile data.

// A StackRecord describes a single execution stack.
type StackRecord struct {
	Stack0 [32]uintptr // stack trace for this record; ends at first 0 entry
}

// Stack returns the stack trace associated with the record,
// a prefix of r.Stack0.
func (r *StackRecord) Stack() []uintptr {
	for i, v := range r.Stack0 {
		if v == 0 {
			return r.Stack0[0:i]
		}
	}
	return r.Stack0[0:]
}

// MemProfileRate controls the fraction of memory allocations
// that are recorded and reported in the memory profile.
// The profiler aims to sample an average of
// one allocation per MemProfileRate bytes allocated.
//
// To include every allocated block in the profile, set MemProfileRate to 1.
// To turn off profiling entirely, set MemProfileRate to 0.
//
// The tools that process the memory profiles assume that the
// profile rate is constant across the lifetime of the program
// and equal to the current value. Programs that change the
// memory profiling rate should do so just once, as early as
// possible in the execution of the program (for example,
// at the beginning of main).
var MemProfileRate int = 512 * 1024

// disableMemoryProfiling is set by the linker if memory profiling
// is not used and the link type guarantees nobody else could use it
// elsewhere.
// We check if the runtime.memProfileInternal symbol is present.
var disableMemoryProfiling bool

// A MemProfileRecord describes the live objects allocated
// by a particular call sequence (stack trace).
type MemProfileRecord struct {
	AllocBytes, FreeBytes     int64       // number of bytes allocated, freed
	AllocObjects, FreeObjects int64       // number of objects allocated, freed
	Stack0                    [32]uintptr // stack trace for this record; ends at first 0 entry
}

// InUseBytes returns the number of bytes in use (AllocBytes - FreeBytes).
func (r *MemProfileRecord) InUseBytes() int64 { return r.AllocBytes - r.FreeBytes }

// InUseObjects returns the number of objects in use (AllocObjects - FreeObjects).
func (r *MemProfileRecord) InUseObjects() int64 {
	return r.AllocObjects - r.FreeObjects
}

// Stack returns the stack trace associated with the record,
// a prefix of r.Stack0.
func (r *MemProfileRecord) Stack() []uintptr {
	for i, v := range r.Stack0 {
		if v == 0 {
			return r.Stack0[0:i]
		}
	}
	return r.Stack0[0:]
}

// MemProfile returns a profile of memory allocated and freed per allocation
// site.
//
// MemProfile returns n, the number of records in the current memory profile.
// If len(p) >= n, MemProfile copies the profile into p and returns n, true.
// If len(p) < n, MemProfile does not change p and returns n, false.
//
// If inuseZero is true, the profile includes allocation records
// where r.AllocBytes > 0 but r.AllocBytes == r.FreeBytes.
// These are sites where memory was allocated, but it has all
// been released back to the runtime.
//
// The returned profile may be up to two garbage collection cycles old.
// This is to avoid skewing the profile toward allocations; because
// allocations happen in real time but frees are delayed until the garbage
// collector performs sweeping, the profile only accounts for allocations
// that have had a chance to be freed by the garbage collector.
//
// Most clients should use the runtime/pprof package or
// the testing package's -test.memprofile flag instead
// of calling MemProfile directly.
func MemProfile(p []MemProfileRecord, inuseZero bool) (n int, ok bool) {
	return memProfileInternal(len(p), inuseZero, func(r profilerecord.MemProfileRecord) {
		copyMemProfileRecord(&p[0], r)
		p = p[1:]
	})
}

// memProfileInternal returns the number of records n in the profile. If there
// are less than size records, copyFn is invoked for each record, and ok returns
// true.
//
// The linker set disableMemoryProfiling to true to disable memory profiling
// if this function is not reachable. Mark it noinline to ensure the symbol exists.
// (This function is big and normally not inlined anyway.)
// See also disableMemoryProfiling above and cmd/link/internal/ld/lib.go:linksetup.
//
//go:noinline
func memProfileInternal(size int, inuseZero bool, copyFn func(profilerecord.MemProfileRecord)) (n int, ok bool) {
	cycle := mProfCycle.read()
	// If we're between mProf_NextCycle and mProf_Flush, take care
	// of flushing to the active profile so we only have to look
	// at the active profile below.
	index := cycle % uint32(len(memRecord{}.future))
	lock(&profMemActiveLock)
	lock(&profMemFutureLock[index])
	mProf_FlushLocked(index)
	unlock(&profMemFutureLock[index])
	clear := true
	head := (*bucket)(mbuckets.Load())
	for b := head; b != nil; b = b.allnext {
		mp := b.mp()
		if inuseZero || mp.active.alloc_bytes != mp.active.free_bytes {
			n++
		}
		if mp.active.allocs != 0 || mp.active.frees != 0 {
			clear = false
		}
	}
	if clear {
		// Absolutely no data, suggesting that a garbage collection
		// has not yet happened. In order to allow profiling when
		// garbage collection is disabled from the beginning of execution,
		// accumulate all of the cycles, and recount buckets.
		n = 0
		for b := head; b != nil; b = b.allnext {
			mp := b.mp()
			for c := range mp.future {
				lock(&profMemFutureLock[c])
				mp.active.add(&mp.future[c])
				mp.future[c] = memRecordCycle{}
				unlock(&profMemFutureLock[c])
			}
			if inuseZero || mp.active.alloc_bytes != mp.active.free_bytes {
				n++
			}
		}
	}
	if n <= size {
		ok = true
		for b := head; b != nil; b = b.allnext {
			mp := b.mp()
			if inuseZero || mp.active.alloc_bytes != mp.active.free_bytes {
				r := profilerecord.MemProfileRecord{
					AllocBytes:   int64(mp.active.alloc_bytes),
					FreeBytes:    int64(mp.active.free_bytes),
					AllocObjects: int64(mp.active.allocs),
					FreeObjects:  int64(mp.active.frees),
					Stack:        b.stk(),
				}
				copyFn(r)
			}
		}
	}
	unlock(&profMemActiveLock)
	return
}

func copyMemProfileRecord(dst *MemProfileRecord, src profilerecord.MemProfileRecord) {
	dst.AllocBytes = src.AllocBytes
	dst.FreeBytes = src.FreeBytes
	dst.AllocObjects = src.AllocObjects
	dst.FreeObjects = src.FreeObjects
	if raceenabled {
		racewriterangepc(unsafe.Pointer(&dst.Stack0[0]), unsafe.Sizeof(dst.Stack0), sys.GetCallerPC(), abi.FuncPCABIInternal(MemProfile))
	}
	if msanenabled {
		msanwrite(unsafe.Pointer(&dst.Stack0[0]), unsafe.Sizeof(dst.Stack0))
	}
	if asanenabled {
		asanwrite(unsafe.Pointer(&dst.Stack0[0]), unsafe.Sizeof(dst.Stack0))
	}
	i := copy(dst.Stack0[:], src.Stack)
	clear(dst.Stack0[i:])
}

//go:linkname pprof_memProfileInternal
func pprof_memProfileInternal(p []profilerecord.MemProfileRecord, inuseZero bool) (n int, ok bool) {
	return memProfileInternal(len(p), inuseZero, func(r profilerecord.MemProfileRecord) {
		p[0] = r
		p = p[1:]
	})
}

func iterate_memprof(fn func(*bucket, uintptr, *uintptr, uintptr, uintptr, uintptr)) {
	lock(&profMemActiveLock)
	head := (*bucket)(mbuckets.Load())
	for b := head; b != nil; b = b.allnext {
		mp := b.mp()
		fn(b, b.nstk, &b.stk()[0], b.size, mp.active.allocs, mp.active.frees)
	}
	unlock(&profMemActiveLock)
}

// BlockProfileRecord describes blocking events originated
// at a particular call sequence (stack trace).
type BlockProfileRecord struct {
	Count  int64
	Cycles int64
	StackRecord
}

// BlockProfile returns n, the number of records in the current blocking profile.
// If len(p) >= n, BlockProfile copies the profile into p and returns n, true.
// If len(p) < n, BlockProfile does not change p and returns n, false.
//
// Most clients should use the [runtime/pprof] package or
// the [testing] package's -test.blockprofile flag instead
// of calling BlockProfile directly.
func BlockProfile(p []BlockProfileRecord) (n int, ok bool) {
	var m int
	n, ok = blockProfileInternal(len(p), func(r profilerecord.BlockProfileRecord) {
		copyBlockProfileRecord(&p[m], r)
		m++
	})
	if ok {
		expandFrames(p[:n])
	}
	return
}

func expandFrames(p []BlockProfileRecord) {
	expandedStack := makeProfStack()
	for i := range p {
		cf := CallersFrames(p[i].Stack())
		j := 0
		for j < len(expandedStack) {
			f, more := cf.Next()
			// f.PC is a "call PC", but later consumers will expect
			// "return PCs"
			expandedStack[j] = f.PC + 1
			j++
			if !more {
				break
			}
		}
		k := copy(p[i].Stack0[:], expandedStack[:j])
		clear(p[i].Stack0[k:])
	}
}

// blockProfileInternal returns the number of records n in the profile. If there
// are less than size records, copyFn is invoked for each record, and ok returns
// true.
func blockProfileInternal(size int, copyFn func(profilerecord.BlockProfileRecord)) (n int, ok bool) {
	lock(&profBlockLock)
	head := (*bucket)(bbuckets.Load())
	for b := head; b != nil; b = b.allnext {
		n++
	}
	if n <= size {
		ok = true
		for b := head; b != nil; b = b.allnext {
			bp := b.bp()
			r := profilerecord.BlockProfileRecord{
				Count:  int64(bp.count),
				Cycles: bp.cycles,
				Stack:  b.stk(),
			}
			// Prevent callers from having to worry about division by zero errors.
			// See discussion on http://golang.org/cl/299991.
			if r.Count == 0 {
				r.Count = 1
			}
			copyFn(r)
		}
	}
	unlock(&profBlockLock)
	return
}

// copyBlockProfileRecord copies the sample values and call stack from src to dst.
// The call stack is copied as-is. The caller is responsible for handling inline
// expansion, needed when the call stack was collected with frame pointer unwinding.
func copyBlockProfileRecord(dst *BlockProfileRecord, src profilerecord.BlockProfileRecord) {
	dst.Count = src.Count
	dst.Cycles = src.Cycles
	if raceenabled {
		racewriterangepc(unsafe.Pointer(&dst.Stack0[0]), unsafe.Sizeof(dst.Stack0), sys.GetCallerPC(), abi.FuncPCABIInternal(BlockProfile))
	}
	if msanenabled {
		msanwrite(unsafe.Pointer(&dst.Stack0[0]), unsafe.Sizeof(dst.Stack0))
	}
	if asanenabled {
		asanwrite(unsafe.Pointer(&dst.Stack0[0]), unsafe.Sizeof(dst.Stack0))
	}
	// We just copy the stack here without inline expansion
	// (needed if frame pointer unwinding is used)
	// since this function is called under the profile lock,
	// and doing something that might allocate can violate lock ordering.
	i := copy(dst.Stack0[:], src.Stack)
	clear(dst.Stack0[i:])
}

//go:linkname pprof_blockProfileInternal
func pprof_blockProfileInternal(p []profilerecord.BlockProfileRecord) (n int, ok bool) {
	return blockProfileInternal(len(p), func(r profilerecord.BlockProfileRecord) {
		p[0] = r
		p = p[1:]
	})
}

// MutexProfile returns n, the number of records in the current mutex profile.
// If len(p) >= n, MutexProfile copies the profile into p and returns n, true.
// Otherwise, MutexProfile does not change p, and returns n, false.
//
// Most clients should use the [runtime/pprof] package
// instead of calling MutexProfile directly.
func MutexProfile(p []BlockProfileRecord) (n int, ok bool) {
	var m int
	n, ok = mutexProfileInternal(len(p), func(r profilerecord.BlockProfileRecord) {
		copyBlockProfileRecord(&p[m], r)
		m++
	})
	if ok {
		expandFrames(p[:n])
	}
	return
}

// mutexProfileInternal returns the number of records n in the profile. If there
// are less than size records, copyFn is invoked for each record, and ok returns
// true.
func mutexProfileInternal(size int, copyFn func(profilerecord.BlockProfileRecord)) (n int, ok bool) {
	lock(&profBlockLock)
	head := (*bucket)(xbuckets.Load())
	for b := head; b != nil; b = b.allnext {
		n++
	}
	if n <= size {
		ok = true
		for b := head; b != nil; b = b.allnext {
			bp := b.bp()
			r := profilerecord.BlockProfileRecord{
				Count:  int64(bp.count),
				Cycles: bp.cycles,
				Stack:  b.stk(),
			}
			copyFn(r)
		}
	}
	unlock(&profBlockLock)
	return
}

//go:linkname pprof_mutexProfileInternal
func pprof_mutexProfileInternal(p []profilerecord.BlockProfileRecord) (n int, ok bool) {
	return mutexProfileInternal(len(p), func(r profilerecord.BlockProfileRecord) {
		p[0] = r
		p = p[1:]
	})
}

// ThreadCreateProfile returns n, the number of records in the thread creation profile.
// If len(p) >= n, ThreadCreateProfile copies the profile into p and returns n, true.
// If len(p) < n, ThreadCreateProfile does not change p and returns n, false.
//
// Most clients should use the runtime/pprof package instead
// of calling ThreadCreateProfile directly.
func ThreadCreateProfile(p []StackRecord) (n int, ok bool) {
	return threadCreateProfileInternal(len(p), func(r profilerecord.StackRecord) {
		i := copy(p[0].Stack0[:], r.Stack)
		clear(p[0].Stack0[i:])
		p = p[1:]
	})
}

// threadCreateProfileInternal returns the number of records n in the profile.
// If there are less than size records, copyFn is invoked for each record, and
// ok returns true.
func threadCreateProfileInternal(size int, copyFn func(profilerecord.StackRecord)) (n int, ok bool) {
	first := (*m)(atomic.Loadp(unsafe.Pointer(&allm)))
	for mp := first; mp != nil; mp = mp.alllink {
		n++
	}
	if n <= size {
		ok = true
		for mp := first; mp != nil; mp = mp.alllink {
			r := profilerecord.StackRecord{Stack: mp.createstack[:]}
			copyFn(r)
		}
	}
	return
}

//go:linkname pprof_threadCreateInternal
func pprof_threadCreateInternal(p []profilerecord.StackRecord) (n int, ok bool) {
	return threadCreateProfileInternal(len(p), func(r profilerecord.StackRecord) {
		p[0] = r
		p = p[1:]
	})
}

//go:linkname pprof_goroutineProfileWithLabels
func pprof_goroutineProfileWithLabels(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	return goroutineProfileWithLabels(p, labels)
}

// labels may be nil. If labels is non-nil, it must have the same length as p.
func goroutineProfileWithLabels(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	if labels != nil && len(labels) != len(p) {
		labels = nil
	}

	return goroutineProfileWithLabelsConcurrent(p, labels)
}

//go:linkname pprof_goroutineLeakProfileWithLabels
func pprof_goroutineLeakProfileWithLabels(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	return goroutineLeakProfileWithLabels(p, labels)
}

// labels may be nil. If labels is non-nil, it must have the same length as p.
func goroutineLeakProfileWithLabels(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	if labels != nil && len(labels) != len(p) {
		labels = nil
	}

	return goroutineLeakProfileWithLabelsConcurrent(p, labels)
}

var goroutineProfile = struct {
	sema    uint32
	active  bool
	offset  atomic.Int64
	records []profilerecord.StackRecord
	labels  []unsafe.Pointer
}{
	sema: 1,
}

// goroutineProfileState indicates the status of a goroutine's stack for the
// current in-progress goroutine profile. Goroutines' stacks are initially
// "Absent" from the profile, and end up "Satisfied" by the time the profile is
// complete. While a goroutine's stack is being captured, its
// goroutineProfileState will be "InProgress" and it will not be able to run
// until the capture completes and the state moves to "Satisfied".
//
// Some goroutines (the finalizer goroutine, which at various times can be
// either a "system" or a "user" goroutine, and the goroutine that is
// coordinating the profile, any goroutines created during the profile) move
// directly to the "Satisfied" state.
type goroutineProfileState uint32

const (
	goroutineProfileAbsent goroutineProfileState = iota
	goroutineProfileInProgress
	goroutineProfileSatisfied
)

type goroutineProfileStateHolder atomic.Uint32

func (p *goroutineProfileStateHolder) Load() goroutineProfileState {
	return goroutineProfileState((*atomic.Uint32)(p).Load())
}

func (p *goroutineProfileStateHolder) Store(value goroutineProfileState) {
	(*atomic.Uint32)(p).Store(uint32(value))
}

func (p *goroutineProfileStateHolder) CompareAndSwap(old, new goroutineProfileState) bool {
	return (*atomic.Uint32)(p).CompareAndSwap(uint32(old), uint32(new))
}

func goroutineLeakProfileWithLabelsConcurrent(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	if len(p) == 0 {
		// An empty slice is obviously too small. Return a rough
		// allocation estimate.
		return work.goroutineLeak.count, false
	}

	pcbuf := makeProfStack() // see saveg() for explanation

	// Prepare a profile large enough to store all leaked goroutines.
	n = work.goroutineLeak.count

	if n > len(p) {
		// There's not enough space in p to store the whole profile, so
		// we're not allowed to write to p at all and must return n, false.
		return n, false
	}

	// Visit each leaked goroutine and try to record its stack.
	var offset int
	forEachGRace(func(gp1 *g) {
		if readgstatus(gp1)&^_Gscan == _Gleaked {
			systemstack(func() { saveg(^uintptr(0), ^uintptr(0), gp1, &p[offset], pcbuf) })
			if labels != nil {
				labels[offset] = gp1.labels
			}
			offset++
		}
	})

	if raceenabled {
		raceacquire(unsafe.Pointer(&labelSync))
	}

	return n, true
}

func goroutineProfileWithLabelsConcurrent(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	if len(p) == 0 {
		// An empty slice is obviously too small. Return a rough
		// allocation estimate without bothering to STW. As long as
		// this is close, then we'll only need to STW once (on the next
		// call).
		return int(gcount(false)), false
	}

	semacquire(&goroutineProfile.sema)

	ourg := getg()

	pcbuf := makeProfStack() // see saveg() for explanation
	stw := stopTheWorld(stwGoroutineProfile)
	// Using gcount while the world is stopped should give us a consistent view
	// of the number of live goroutines, minus the number of goroutines that are
	// alive and permanently marked as "system". But to make this count agree
	// with what we'd get from isSystemGoroutine, we need special handling for
	// goroutines that can vary between user and system to ensure that the count
	// doesn't change during the collection. So, check the finalizer goroutine
	// and cleanup goroutines in particular.
	n = int(gcount(false))
	if fingStatus.Load()&fingRunningFinalizer != 0 {
		n++
	}
	n += int(gcCleanups.running.Load())

	if n > len(p) {
		// There's not enough space in p to store the whole profile, so (per the
		// contract of runtime.GoroutineProfile) we're not allowed to write to p
		// at all and must return n, false.
		startTheWorld(stw)
		semrelease(&goroutineProfile.sema)
		return n, false
	}

	// Save current goroutine.
	sp := sys.GetCallerSP()
	pc := sys.GetCallerPC()
	systemstack(func() {
		saveg(pc, sp, ourg, &p[0], pcbuf)
	})
	if labels != nil {
		labels[0] = ourg.labels
	}
	ourg.goroutineProfiled.Store(goroutineProfileSatisfied)
	goroutineProfile.offset.Store(1)

	// Prepare for all other goroutines to enter the profile. Aside from ourg,
	// every goroutine struct in the allgs list has its goroutineProfiled field
	// cleared. Any goroutine created from this point on (while
	// goroutineProfile.active is set) will start with its goroutineProfiled
	// field set to goroutineProfileSatisfied.
	goroutineProfile.active = true
	goroutineProfile.records = p
	goroutineProfile.labels = labels
	startTheWorld(stw)

	// Visit each goroutine that existed as of the startTheWorld call above.
	//
	// New goroutines may not be in this list, but we didn't want to know about
	// them anyway. If they do appear in this list (via reusing a dead goroutine
	// struct, or racing to launch between the world restarting and us getting
	// the list), they will already have their goroutineProfiled field set to
	// goroutineProfileSatisfied before their state transitions out of _Gdead.
	//
	// Any goroutine that the scheduler tries to execute concurrently with this
	// call will start by adding itself to the profile (before the act of
	// executing can cause any changes in its stack).
	forEachGRace(func(gp1 *g) {
		tryRecordGoroutineProfile(gp1, pcbuf, Gosched)
	})

	stw = stopTheWorld(stwGoroutineProfileCleanup)
	endOffset := goroutineProfile.offset.Swap(0)
	goroutineProfile.active = false
	goroutineProfile.records = nil
	goroutineProfile.labels = nil
	startTheWorld(stw)

	// Restore the invariant that every goroutine struct in allgs has its
	// goroutineProfiled field cleared.
	forEachGRace(func(gp1 *g) {
		gp1.goroutineProfiled.Store(goroutineProfileAbsent)
	})

	if raceenabled {
		raceacquire(unsafe.Pointer(&labelSync))
	}

	if n != int(endOffset) {
		// It's a big surprise that the number of goroutines changed while we
		// were collecting the profile. But probably better to return a
		// truncated profile than to crash the whole process.
		//
		// For instance, needm moves a goroutine out of the _Gdeadextra state and so
		// might be able to change the goroutine count without interacting with
		// the scheduler. For code like that, the race windows are small and the
		// combination of features is uncommon, so it's hard to be (and remain)
		// sure we've caught them all.
	}

	semrelease(&goroutineProfile.sema)
	return n, true
}

// tryRecordGoroutineProfileWB asserts that write barriers are allowed and calls
// tryRecordGoroutineProfile.
//
//go:yeswritebarrierrec
func tryRecordGoroutineProfileWB(gp1 *g) {
	if getg().m.p.ptr() == nil {
		throw("no P available, write barriers are forbidden")
	}
	tryRecordGoroutineProfile(gp1, nil, osyield)
}

// tryRecordGoroutineProfile ensures that gp1 has the appropriate representation
// in the current goroutine profile: either that it should not be profiled, or
// that a snapshot of its call stack and labels are now in the profile.
func tryRecordGoroutineProfile(gp1 *g, pcbuf []uintptr, yield func()) {
	if status := readgstatus(gp1); status == _Gdead || status == _Gdeadextra {
		// Dead goroutines should not appear in the profile. Goroutines that
		// start while profile collection is active will get goroutineProfiled
		// set to goroutineProfileSatisfied before transitioning out of _Gdead,
		// so here we check _Gdead first.
		return
	}

	for {
		prev := gp1.goroutineProfiled.Load()
		if prev == goroutineProfileSatisfied {
			// This goroutine is already in the profile (or is new since the
			// start of collection, so shouldn't appear in the profile).
			break
		}
		if prev == goroutineProfileInProgress {
			// Something else is adding gp1 to the goroutine profile right now.
			// Give that a moment to finish.
			yield()
			continue
		}

		// While we have gp1.goroutineProfiled set to
		// goroutineProfileInProgress, gp1 may appear _Grunnable but will not
		// actually be able to run. Disable preemption for ourselves, to make
		// sure we finish profiling gp1 right away instead of leaving it stuck
		// in this limbo.
		mp := acquirem()
		if gp1.goroutineProfiled.CompareAndSwap(goroutineProfileAbsent, goroutineProfileInProgress) {
			doRecordGoroutineProfile(gp1, pcbuf)
			gp1.goroutineProfiled.Store(goroutineProfileSatisfied)
		}
		releasem(mp)
	}
}

// doRecordGoroutineProfile writes gp1's call stack and labels to an in-progress
// goroutine profile. Preemption is disabled.
//
// This may be called via tryRecordGoroutineProfile in two ways: by the
// goroutine that is coordinating the goroutine profile (running on its own
// stack), or from the scheduler in preparation to execute gp1 (running on the
// system stack).
func doRecordGoroutineProfile(gp1 *g, pcbuf []uintptr) {
	if isSystemGoroutine(gp1, false) {
		// System goroutines should not appear in the profile.
		// Check this here and not in tryRecordGoroutineProfile because isSystemGoroutine
		// may change on a goroutine while it is executing, so while the scheduler might
		// see a system goroutine, goroutineProfileWithLabelsConcurrent might not, and
		// this inconsistency could cause invariants to be violated, such as trying to
		// record the stack of a running goroutine below. In short, we still want system
		// goroutines to participate in the same state machine on gp1.goroutineProfiled as
		// everything else, we just don't record the stack in the profile.
		return
	}
	// Double-check that we didn't make a grave mistake. If the G is running then in
	// general, we cannot safely read its stack.
	//
	// However, there is one case where it's OK. There's a small window of time in
	// exitsyscall where a goroutine could be in _Grunning as it's exiting a syscall.
	// This is OK because goroutine will not exit the syscall until it passes through
	// a call to tryRecordGoroutineProfile. (An explicit one on the fast path, an
	// implicit one via the scheduler on the slow path.)
	//
	// This is also why it's safe to check syscallsp here. The syscall path mutates
	// syscallsp only after passing through tryRecordGoroutineProfile.
	if readgstatus(gp1) == _Grunning && gp1.syscallsp == 0 {
		print("doRecordGoroutineProfile gp1=", gp1.goid, "\n")
		throw("cannot read stack of running goroutine")
	}

	offset := int(goroutineProfile.offset.Add(1)) - 1

	if offset >= len(goroutineProfile.records) {
		// Should be impossible, but better to return a truncated profile than
		// to crash the entire process at this point. Instead, deal with it in
		// goroutineProfileWithLabelsConcurrent where we have more context.
		return
	}

	// saveg calls gentraceback, which may call cgo traceback functions. When
	// called from the scheduler, this is on the system stack already so
	// traceback.go:cgoContextPCs will avoid calling back into the scheduler.
	//
	// When called from the goroutine coordinating the profile, we still have
	// set gp1.goroutineProfiled to goroutineProfileInProgress and so are still
	// preventing it from being truly _Grunnable. So we'll use the system stack
	// to avoid schedule delays.
	systemstack(func() { saveg(^uintptr(0), ^uintptr(0), gp1, &goroutineProfile.records[offset], pcbuf) })

	if goroutineProfile.labels != nil {
		goroutineProfile.labels[offset] = gp1.labels
	}
}

func goroutineProfileWithLabelsSync(p []profilerecord.StackRecord, labels []unsafe.Pointer) (n int, ok bool) {
	gp := getg()

	isOK := func(gp1 *g) bool {
		// Checking isSystemGoroutine here makes GoroutineProfile
		// consistent with both NumGoroutine and Stack.
		if gp1 == gp {
			return false
		}
		if status := readgstatus(gp1); status == _Gdead || status == _Gdeadextra {
			return false
		}
		if isSystemGoroutine(gp1, false) {
			return false
		}
		return true
	}

	pcbuf := makeProfStack() // see saveg() for explanation
	stw := stopTheWorld(stwGoroutineProfile)

	// World is stopped, no locking required.
	n = 1
	forEachGRace(func(gp1 *g) {
		if isOK(gp1) {
			n++
		}
	})

	if n <= len(p) {
		ok = true
		r, lbl := p, labels

		// Save current goroutine.
		sp := sys.GetCallerSP()
		pc := sys.GetCallerPC()
		systemstack(func() {
			saveg(pc, sp, gp, &r[0], pcbuf)
		})
		r = r[1:]

		// If we have a place to put our goroutine labelmap, insert it there.
		if labels != nil {
			lbl[0] = gp.labels
			lbl = lbl[1:]
		}

		// Save other goroutines.
		forEachGRace(func(gp1 *g) {
			if !isOK(gp1) {
				return
			}

			if len(r) == 0 {
				// Should be impossible, but better to return a
				// truncated profile than to crash the entire process.
				return
			}
			// saveg calls gentraceback, which may call cgo traceback functions.
			// The world is stopped, so it cannot use cgocall (which will be
			// blocked at exitsyscall). Do it on the system stack so it won't
			// call into the schedular (see traceback.go:cgoContextPCs).
			systemstack(func() { saveg(^uintptr(0), ^uintptr(0), gp1, &r[0], pcbuf) })
			if labels != nil {
				lbl[0] = gp1.labels
				lbl = lbl[1:]
			}
			r = r[1:]
		})
	}

	if raceenabled {
		raceacquire(unsafe.Pointer(&labelSync))
	}

	startTheWorld(stw)
	return n, ok
}

// GoroutineProfile returns n, the number of records in the active goroutine stack profile.
// If len(p) >= n, GoroutineProfile copies the profile into p and returns n, true.
// If len(p) < n, GoroutineProfile does not change p and returns n, false.
//
// Most clients should use the [runtime/pprof] package instead
// of calling GoroutineProfile directly.
func GoroutineProfile(p []StackRecord) (n int, ok bool) {
	records := make([]profilerecord.StackRecord, len(p))
	n, ok = goroutineProfileInternal(records)
	if !ok {
		return
	}
	for i, mr := range records[0:n] {
		l := copy(p[i].Stack0[:], mr.Stack)
		clear(p[i].Stack0[l:])
	}
	return
}

func goroutineProfileInternal(p []profilerecord.StackRecord) (n int, ok bool) {
	return goroutineProfileWithLabels(p, nil)
}

func saveg(pc, sp uintptr, gp *g, r *profilerecord.StackRecord, pcbuf []uintptr) {
	// To reduce memory usage, we want to allocate a r.Stack that is just big
	// enough to hold gp's stack trace. Naively we might achieve this by
	// recording our stack trace into mp.profStack, and then allocating a
	// r.Stack of the right size. However, mp.profStack is also used for
	// allocation profiling, so it could get overwritten if the slice allocation
	// gets profiled. So instead we record the stack trace into a temporary
	// pcbuf which is usually given to us by our caller. When it's not, we have
	// to allocate one here. This will only happen for goroutines that were in a
	// syscall when the goroutine profile started or for goroutines that manage
	// to execute before we finish iterating over all the goroutines.
	if pcbuf == nil {
		pcbuf = makeProfStack()
	}

	var u unwinder
	u.initAt(pc, sp, 0, gp, unwindSilentErrors)
	n := tracebackPCs(&u, 0, pcbuf)
	r.Stack = make([]uintptr, n)
	copy(r.Stack, pcbuf)
}

// Stack formats a stack trace of the calling goroutine into buf
// and returns the number of bytes written to buf.
// If all is true, Stack formats stack traces of all other goroutines
// into buf after the trace for the current goroutine.
func Stack(buf []byte, all bool) int {
	var stw worldStop
	if all {
		stw = stopTheWorld(stwAllGoroutinesStack)
	}

	n := 0
	if len(buf) > 0 {
		gp := getg()
		sp := sys.GetCallerSP()
		pc := sys.GetCallerPC()
		systemstack(func() {
			g0 := getg()
			// Force traceback=1 to override GOTRACEBACK setting,
			// so that Stack's results are consistent.
			// GOTRACEBACK is only about crash dumps.
			g0.m.traceback = 1
			g0.writebuf = buf[0:0:len(buf)]
			goroutineheader(gp)
			traceback(pc, sp, 0, gp)
			if all {
				tracebackothers(gp)
			}
			g0.m.traceback = 0
			n = len(g0.writebuf)
			g0.writebuf = nil
		})
	}

	if all {
		startTheWorld(stw)
	}
	return n
}
