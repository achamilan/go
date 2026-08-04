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
//
// Uses CreateFileW with a UTF-16 converted path so non-ASCII (e.g. Chinese)
// directory names work; CreateFileA would mangle UTF-8 bytes as ANSI.
func writeDeadTraceToFile(path string, buf []byte) {
	if len(buf) == 0 {
		return
	}

	// Convert Go string (UTF-8) to null-terminated UTF-16.
	const surrLow = (surrogateMin + surrogateMax + 1) / 2 // 0xDC00
	var nameUTF16 [_MAX_PATH]uint16
	w := 0
	for _, r := range path {
		if w >= len(nameUTF16)-2 {
			break // leave room for a surrogate pair + NUL
		}
		if r < 0x10000 {
			nameUTF16[w] = uint16(r)
			w++
		} else {
			r -= 0x10000
			nameUTF16[w] = surrogateMin + uint16(r>>10)&0x3ff
			nameUTF16[w+1] = surrLow + uint16(r)&0x3ff
			w += 2
		}
	}
	nameUTF16[w] = 0

	// CreateFileW(lpFileName, dwDesiredAccess, dwShareMode, lpSecurityAttributes,
	//             dwCreationDisposition, dwFlagsAndAttributes, hTemplateFile)
	handle := stdcall(_CreateFileW,
		uintptr(unsafe.Pointer(&nameUTF16[0])),
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
