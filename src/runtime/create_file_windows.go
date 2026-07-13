// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package runtime

import "unsafe"

const canCreateFile = true

// Windows CreateFileA dwDesiredAccess constants.
const (
	_GENERIC_WRITE    = 0x40000000
	_FILE_APPEND_DATA = 0x00000004
)

// Windows CreateFileA dwShareMode constants.
const (
	_FILE_SHARE_READ  = 0x00000001
	_FILE_SHARE_WRITE = 0x00000002
)

// Windows CreateFileA dwCreationDisposition constants.
const (
	_CREATE_ALWAYS = 2
	_OPEN_ALWAYS   = 4
)

// Windows CreateFileA dwFlagsAndAttributes constants.
const (
	_FILE_ATTRIBUTE_NORMAL = 0x00000080
)

// writeDeadTraceToFile appends the gcdeadtrace output to a file using Windows API.
// Used for GODEBUG=gcdeadtracefile=<path>. Opens the file with FILE_APPEND_DATA
// access and OPEN_ALWAYS disposition, so multiple GC cycles accumulate.
func writeDeadTraceToFile(path string, buf []byte) {
	if len(buf) == 0 {
		return
	}

	// Convert Go string to null-terminated ANSI byte slice.
	var nameBytes [_MAX_PATH]byte
	plen := len(path)
	if plen > _MAX_PATH-1 {
		plen = _MAX_PATH - 1
	}
	copy(nameBytes[:plen], path)
	nameBytes[plen] = 0

	// CreateFileA(lpFileName, dwDesiredAccess, dwShareMode, lpSecurityAttributes,
	//             dwCreationDisposition, dwFlagsAndAttributes, hTemplateFile)
	handle := stdcall(_CreateFileA,
		uintptr(unsafe.Pointer(&nameBytes[0])),
		_FILE_APPEND_DATA,
		_FILE_SHARE_READ|_FILE_SHARE_WRITE,
		0, // lpSecurityAttributes (NULL)
		_OPEN_ALWAYS,
		_FILE_ATTRIBUTE_NORMAL,
		0, // hTemplateFile (NULL)
	)
	if handle == ^uintptr(0) { // INVALID_HANDLE_VALUE
		println("runtime: gcdeadtracefile: failed to open", path)
		return
	}
	if !gcDeadTraceFileCreated {
		println("runtime: gcdeadtracefile: created", path)
		gcDeadTraceFileCreated = true
	}

	var written uint32
	stdcall(_WriteFile,
		handle,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&written)),
		0, // lpOverlapped (NULL)
	)

	stdcall(_CloseHandle, handle)
}
