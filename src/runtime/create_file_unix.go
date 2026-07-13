// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package runtime

import "unsafe"

const canCreateFile = true

// create returns an fd to a write-only file.
func create(name *byte, perm int32) int32 {
	return open(name, _O_CREAT|_O_WRONLY|_O_TRUNC, perm)
}

// writeDeadTraceToFile appends the given buffer to the specified file.
// Used for GODEBUG=gcdeadtracefile=. Opens the file in append mode
// (O_CREAT|O_APPEND), so multiple GC cycles accumulate.
func writeDeadTraceToFile(path string, buf []byte) {
	var nameBytes [512]byte
	plen := len(path)
	if plen > 511 {
		plen = 511
	}
	copy(nameBytes[:plen], path)
	nameBytes[plen] = 0

	fd := open(&nameBytes[0], _O_CREAT|_O_WRONLY|_O_APPEND, 0666)
	if fd < 0 {
		println("runtime: gcdeadtracefile: failed to open", path)
		return
	}
	if !gcDeadTraceFileCreated {
		println("runtime: gcdeadtracefile: created", path)
		gcDeadTraceFileCreated = true
	}
	write(uintptr(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
	closefd(fd)
}
