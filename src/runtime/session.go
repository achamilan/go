// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Session memory: bump-allocated memory buckets with immediate release.
//
// A Session is a handle to a collection of sessionBuckets. Objects allocated
// within a session are bump-allocated inside sessionBuckets, each backed by
// an mspan. When Close() is called, all bucket memory is immediately returned
// to the heap — no GC cycle is required.
//
// Session memory reduces allocation overhead via bump allocation and
// amortizes free cost at the bucket level. The caller is responsible for
// calling Close() to release memory; there is no GC-driven automatic cleanup.

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

const (
	// sessionBucketBytes is the default size of a sessionBucket.
	// Must be a multiple of pageSize.
	sessionBucketBytes = 64 << 10 // 64 KB

	// sessionBucketPages is the number of pages in a session bucket.
	sessionBucketPages = sessionBucketBytes / pageSize
)

func init() {
	if sessionBucketBytes%pageSize != 0 {
		throw("sessionBucketBytes must be a multiple of pageSize")
	}
}

// sessionBucket is a block of pre-allocated memory used for session allocations.
// Each bucket maps to a single mspan with state mSpanSession.
type sessionBucket struct {
	_    sys.NotInHeap
	next *sessionBucket // next bucket in session's full-bucket list
	span *mspan         // underlying runtime span
	base uintptr        // start of usable memory
	bump uintptr        // bump allocation pointer
	end  uintptr        // end of usable memory
	refs atomic.Int32   // number of allocated objects in this bucket
}

// sessionBucketPool holds recycled bucket metadata ready for reuse.
var sessionBucketPool struct {
	mu    mutex
	free  *sessionBucket // free list (zeroed, ready for use)
	ready *sessionBucket // buckets awaiting zeroing before reuse
}

var sessionBucketAlloc struct {
	mu  mutex
	fix fixalloc
}

func init() {
	lockInit(&sessionBucketPool.mu, lockRankLeafRank)
	lockInit(&sessionBucketAlloc.mu, lockRankLeafRank)
	sessionBucketAlloc.fix.init(unsafe.Sizeof(sessionBucket{}), nil, nil, &memstats.other_sys)
}

// activeSessionSpans tracks all live session bucket spans so the GC
// can conservatively scan them for session→heap pointers during marking.
var activeSessionSpans struct {
	mu    mutex
	spans []*mspan
}

func init() {
	lockInit(&activeSessionSpans.mu, lockRankLeafRank)
}

// addActiveSpan registers a session bucket span for GC root scanning.
func addActiveSpan(s *mspan) {
	lock(&activeSessionSpans.mu)
	activeSessionSpans.spans = append(activeSessionSpans.spans, s)
	unlock(&activeSessionSpans.mu)
}

// removeActiveSpan removes a span from the GC root scan list.
func removeActiveSpan(s *mspan) {
	lock(&activeSessionSpans.mu)
	spans := activeSessionSpans.spans
	for i, sp := range spans {
		if sp == s {
			// Swap-remove for O(1) deletion.
			spans[i] = spans[len(spans)-1]
			spans[len(spans)-1] = nil
			activeSessionSpans.spans = spans[:len(spans)-1]
			break
		}
	}
	unlock(&activeSessionSpans.mu)
}

// allocBucket allocates or reuses a sessionBucket.
// It atomically reaps ready buckets from the pool and pops from the free list.
func allocBucket(s *mspan) *sessionBucket {
	lock(&sessionBucketPool.mu)
	// Reap ready buckets into the free list first.
	for sessionBucketPool.ready != nil {
		b := sessionBucketPool.ready
		sessionBucketPool.ready = b.next
		b.next = sessionBucketPool.free
		sessionBucketPool.free = b
	}
	// Pop from the free list, or return nil to trigger a new allocation.
	var b *sessionBucket
	if sessionBucketPool.free != nil {
		b = sessionBucketPool.free
		sessionBucketPool.free = b.next
	}
	unlock(&sessionBucketPool.mu)

	if b == nil {
		lock(&sessionBucketAlloc.mu)
		b = (*sessionBucket)(sessionBucketAlloc.fix.alloc())
		unlock(&sessionBucketAlloc.mu)
	} else {
		b.next = nil
	}
	b.span = s
	b.base = s.base()
	b.bump = b.base
	b.end = s.base() + s.npages*pageSize
	b.refs.Store(0)
	return b
}

// freeBucket returns a bucket metadata struct to the free pool.
func freeBucket(b *sessionBucket) {
	b.span = nil
	b.next = nil
	lock(&sessionBucketPool.mu)
	b.next = sessionBucketPool.ready
	sessionBucketPool.ready = b
	unlock(&sessionBucketPool.mu)
}

// Session is a handle for session-scoped memory allocation.
// All memory allocated through a Session is immediately freed
// when Close() is called. There is no GC-driven cleanup;
// the caller must call Close() to release memory.
type Session struct {
	buckets     *sessionBucket // active bucket for bump allocation
	fullBuckets *sessionBucket // list of full buckets
	mu          mutex
	closed      atomic.Bool // guards against use-after-close
}

// NewSession creates a new Session for scoped memory allocation.
// The caller MUST call Close() to release the allocated memory.
//
//go:nosplit
func NewSession() *Session {
	s := new(Session)
	lockInit(&s.mu, lockRankLeafRank)
	return s
}

// allocBucketSpan allocates a new mspan for a session bucket.
func (s *Session) allocBucketSpan() *mspan {
	var span *mspan
	systemstack(func() {
		span = mheap_.allocManual(sessionBucketPages, spanAllocSession)
	})
	if span != nil {
		addActiveSpan(span)
	}
	return span
}

// refill gets a new active bucket, moving the current one to the full list.
// Must be called with s.mu held.
func (s *Session) refill() *sessionBucket {
	if s.buckets != nil && s.buckets.bump > s.buckets.base {
		s.buckets.next = s.fullBuckets
		s.fullBuckets = s.buckets
	}

	span := s.allocBucketSpan()
	if span == nil {
		throw("session: failed to allocate bucket span")
	}

	b := allocBucket(span)
	s.buckets = b
	return b
}

// Alloc allocates size bytes of zero-initialized memory from the session.
// The returned pointer is valid until Close() is called.
// Returns nil if size is 0 or the session has been closed.
func (s *Session) Alloc(size uintptr) unsafe.Pointer {
	if size == 0 || s.closed.Load() {
		return nil
	}

	lock(&s.mu)
	if s.closed.Load() {
		unlock(&s.mu)
		return nil
	}

	b := s.buckets
	if b == nil || b.bump+size > b.end {
		b = s.refill()
	}

	x := b.bump
	b.bump = alignUp(x+size, 8)
	b.refs.Add(1)
	unlock(&s.mu)

	memclrNoHeapPointers(unsafe.Pointer(x), size)
	return unsafe.Pointer(x)
}

// Close immediately releases all memory allocated through this session.
// After Close, any pointers returned by Alloc are invalid.
// Close is safe to call multiple times.
func (s *Session) Close() {
	lock(&s.mu)
	if s.closed.Load() {
		unlock(&s.mu)
		return // already closed
	}
	s.closed.Store(true)
	s.freeAllBucketsLocked()
	unlock(&s.mu)
}

// freeAllBucketsLocked releases all bucket spans and metadata.
// Must be called with s.mu held.
func (s *Session) freeAllBucketsLocked() {
	if s.buckets != nil {
		removeActiveSpan(s.buckets.span)
		freeBucketSpan(s.buckets.span)
		freeBucket(s.buckets)
		s.buckets = nil
	}

	for b := s.fullBuckets; b != nil; b = b.next {
		removeActiveSpan(b.span)
		freeBucketSpan(b.span)
	}
	// Free bucket metadata for full buckets in a second pass
	// after spans are freed.
	for s.fullBuckets != nil {
		b := s.fullBuckets
		s.fullBuckets = b.next
		freeBucket(b)
	}
}

// freeBucketSpan returns a bucket's mspan to the heap.
func freeBucketSpan(span *mspan) {
	systemstack(func() {
		if span != nil {
			mheap_.freeManual(span, spanAllocSession)
		}
	})
}

// markrootSessionSpans conservatively scans all active session bucket spans
// as GC roots. This ensures that session→heap pointers are followed during
// garbage collection marking.
//
// This is called from markroot during the concurrent mark phase.
func markrootSessionSpans(gcw *gcWork) {
	lock(&activeSessionSpans.mu)
	for _, s := range activeSessionSpans.spans {
		if s == nil {
			continue
		}
		// Conservative scan: every pointer-aligned word in the span's
		// used portion is treated as a potential heap pointer.
		base := s.base()
		limit := s.limit
		for p := base; p < limit; p += goarch.PtrSize {
			ptr := *(*uintptr)(unsafe.Pointer(p))
			if ptr == 0 {
				continue
			}
			// Pre-check span state+bounds before calling findObject.
			// Conservative scanning may encounter cookie values that
			// look like pointers to dead spans or out-of-bounds regions.
			// findObject panics on such pointers when
			// debug.invalidptr != 0 (e.g. in tests).
			ts := spanOf(ptr)
			if ts == nil {
				continue
			}
			if ts.state.get() != mSpanInUse || ptr < ts.base() || ptr >= ts.limit {
				continue
			}
			if obj, span, objIndex := findObject(ptr, p, 0); obj != 0 {
				// Skip free (unallocated) objects to avoid
				// marking zombies. False pointer matches in
				// conservative scanning can point to free slots.
				if !span.isFree(objIndex) {
					greyobject(obj, p, 0, span, gcw, objIndex)
				}
			}
		}
	}
	unlock(&activeSessionSpans.mu)
}
