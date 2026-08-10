// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Size classes for the manual memory pool (mpool).
//
// The table starts at 8 bytes and grows each class by roughly 25%
// (aligned to 8 bytes), up to maxSmallSize. Classes cover user sizes;
// each block additionally carries an mpHeaderSize-byte header.

const (
	mpHeaderSize = 8  // bytes of header before each block
	mpBatchSize  = 32 // blocks exchanged between per-P cache and central
	mpMaxClasses = 40 // upper bound on the number of classes
)

// mpClassTable holds the generated size class table.
// Fixed-size arrays only: it is built during package initialization,
// when allocation is not yet safe.
type mpClassTable struct {
	sizes [mpMaxClasses]uintptr // user-visible bytes per class
	small [1024/8 + 1]int32    // size (<=1024, 8-byte granule) -> class
	n     int
}

var mpClassTab = mpMakeClassTable()

func mpMakeClassTable() (t mpClassTable) {
	step := uintptr(8)
	for size := uintptr(8); ; size += step {
		t.sizes[t.n] = size
		t.n++
		if size >= maxSmallSize { // largest class must cover maxSmallSize
			break
		}
		if s := alignUp(size>>2, 8); s > step { // ~25% growth per class
			step = s
		}
	}
	// Direct lookup table for sizes <= 1024. Note that the smallest class
	// covering a size may itself be > 1024 (e.g. 945..1024 map to a larger
	// class), so clamp the fill range instead of stopping at the first
	// oversized class.
	for i := 0; i < t.n; i++ {
		var prev uintptr
		if i > 0 {
			prev = t.sizes[i-1]
		}
		from := prev + 8
		if from > 1024 {
			break
		}
		to := min(t.sizes[i], 1024)
		for s := from; s <= to; s += 8 {
			t.small[s/8] = int32(i)
		}
	}
	return t
}

// mpSizeToClass returns the smallest class holding size.
// Callers must ensure size <= maxSmallSize.
func mpSizeToClass(size uintptr) int {
	if size <= 1024 {
		return int(mpClassTab.small[alignUp(size, 8)/8])
	}
	// Hand-rolled binary search (runtime cannot import sort).
	lo, hi := 0, mpClassTab.n
	for lo < hi {
		mid := (lo + hi) / 2
		if mpClassTab.sizes[mid] < size {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// mpBlockSize returns the per-block footprint of a class, header included.
func mpBlockSize(cls int) uintptr {
	return alignUp(mpHeaderSize+mpClassTab.sizes[cls], 8)
}
