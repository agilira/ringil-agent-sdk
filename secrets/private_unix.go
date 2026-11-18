// private_unix.go: ownership checks for secret files on Unix
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package secrets

import (
	"os"
	"syscall"

	"github.com/agilira/go-errors"
)

// noFollow refuses a symlink in the final component when opening.
const noFollow = syscall.O_NOFOLLOW

func checkOwner(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return errors.New(ErrSource, "the env file must belong to the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New(ErrSource, "the env file must not be accessible to group or others; chmod 600 it")
	}
	return nil
}
