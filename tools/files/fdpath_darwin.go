// fdpath_darwin.go: the path of an open file on macOS
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package files

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fdPath returns where the open file really is, through fcntl(F_GETPATH).
func fdPath(f *os.File) (string, error) {
	buf := make([]byte, unix.PathMax)
	// #nosec G103 -- F_GETPATH writes at most PathMax bytes into buf
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}
