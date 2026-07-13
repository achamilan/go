// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !unix && !windows

package runtime

const canCreateFile = false

func create(name *byte, perm int32) int32 {
	throw("unimplemented")
	return -1
}

// writeDeadTraceToFile is a no-op on platforms that don't support file creation.
func writeDeadTraceToFile(path string, buf []byte) {
	if !gcDeadTraceFileCreated {
		println("runtime: gcdeadtracefile: unsupported platform, cannot create", path)
		gcDeadTraceFileCreated = true
	}
}
