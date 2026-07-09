// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package slicepool provides a generic, type-safe pool for variable-sized slices.
//
// Typical usage:
//
//	pool := slicepool.SlicePool[byte]{
//		New: func(n int) []byte { return make([]byte, n) },
//	}
//	buf := pool.Get(1024)   // returns []byte with cap >= 1024
//	defer pool.Put(buf)     // returns the slice to the pool
package slicepool

import (
	"math/bits"
	"sync"
)

const (
	// minCap is the minimum capacity eligible for pooling.
	minCap = 16
	// numClasses is the number of size classes.
	numClasses = 16
)

// SlicePool is a generic pool for variable-sized slices.
//
// A SlicePool is safe for concurrent use by multiple goroutines.
type SlicePool[E any] struct {
	// New, if non-nil, must return a new slice of the specified capacity.
	New     func(int) []E
	classes [numClasses]sync.Pool
}

// Get returns a slice of capacity at least n from the pool, if possible.
// If not possible and New is not nil, Get returns the value returned by New(m),
// with m greater or equal to n.
// Otherwise it returns nil.
//
// Callers must not assume any relationship between individual slices returned by Get
// and slices previously passed as arguments to Put.
// Callers can assume that the length and contents of the returned slice are either as
// set by New, or as they were when the slice was passed to Put.
func (p *SlicePool[E]) Get(n int) []E {
	if n <= 0 {
		n = 1
	}

	class := sizeClass(n)
	if class < 0 {
		class = 0 // n < minCap
	}

	// Top class: no upper bound, so capacity-insensitive pooling
	// makes little sense. Allocate directly.
	if class == numClasses-1 {
		if p.New != nil {
			return p.New(n)
		}
		return nil
	}

	if v := p.classes[class].Get(); v != nil {
		s := v.([]E)
		if cap(s) >= n {
			return s
		}
		// cap too small to satisfy this request
		// (e.g., externally-created slice Put into this class).
		// Discard and allocate fresh below.
	}
	if p.New != nil {
		return p.New(classMax(class))
	}
	return nil
}

// Put adds the slice s to the pool.
// The slice s should not be used after it has been passed to Put.
//
// Put accepts any slice whose capacity falls within a valid size class.
// Slices smaller than the minimum capacity or in the top (unbounded) class
// are silently discarded.
//
// Put is best effort, in that it may silently drop the slice in case it
// detects it would not be beneficial to add it to the pool.
func (p *SlicePool[E]) Put(s []E) {
	c := cap(s)
	class := sizeClass(c)
	if class < 0 {
		return // capacity < minCap, discard
	}
	if class == numClasses-1 {
		return // top class: not pooled
	}
	p.classes[class].Put(s)
}

// sizeClass returns the size class index for capacity c.
// Returns -1 if c < minCap.
func sizeClass(c int) int {
	if c < minCap {
		return -1
	}
	// class = floor(log2(c / minCap))
	class := bits.Len64(uint64(c/minCap)) - 1
	if class >= numClasses {
		return numClasses - 1
	}
	return class
}

// classMax returns the recommended allocation capacity for a size class:
// the maximum capacity that still falls within that class.
// New is called with this value so that the returned slice's cap
// maps back to the same class.
func classMax(class int) int {
	if class+1 < numClasses {
		return minCap<<(class+1) - 1
	}
	// Top class: no upper bound, use the class min as a reasonable default.
	return minCap << class
}
