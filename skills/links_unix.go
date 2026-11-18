// links_unix.go: refusing hard-linked files at installation
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package skills

import (
	"os"
	"syscall"
)

// linked reports a file with more than one name: a hard link could bring
// content from outside the skill's directory into the installed copy.
func linked(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
