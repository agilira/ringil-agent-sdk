// private_unix.go: files only the user owns and writes, on Unix
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package main

import (
	"os"
	"syscall"
)

// noFollow refuses to open a symlink planted where a file of ours goes.
const noFollow = syscall.O_NOFOLLOW | syscall.O_CLOEXEC

// private: the user owns info and nobody else can write it. Group write is
// allowed only for a per-user group (gid equal to uid, the usual umask 002
// setup), which gives nobody else access.
func private(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o002 != 0 {
		return false
	}
	return info.Mode().Perm()&0o020 == 0 || st.Gid == st.Uid
}
