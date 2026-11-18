// fdpath_linux.go: the path of an open file on Linux
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package files

import (
	"os"
	"strconv"
	"strings"
)

// fdPath returns where the open file really is, whatever path or links
// led to it.
func fdPath(f *os.File) (string, error) {
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(p, " (deleted)") {
		return "", os.ErrNotExist
	}
	return p, nil
}
