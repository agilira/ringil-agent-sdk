// fdpath_windows.go: the path of an open file on Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package files

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// fdPath returns the file's final path, with links, junctions and 8.3
// short names resolved.
func fdPath(f *os.File) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	if int(n) > len(buf) {
		return "", windows.ERROR_INSUFFICIENT_BUFFER
	}
	p := windows.UTF16ToString(buf[:n])
	p = strings.TrimPrefix(p, `\\?\UNC\`)
	return strings.TrimPrefix(p, `\\?\`), nil
}
