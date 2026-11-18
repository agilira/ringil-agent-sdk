// links_unix.go: refusing files with several names
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package rules

import (
	"os"
	"syscall"
)

func singleName(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || st.Nlink <= 1
}
