// lock_unix.go: exclusive session locks on Unix
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package file

import (
	"os"
	"syscall"

	"github.com/agilira/go-errors"
	"golang.org/x/sys/unix"

	agent "github.com/agilira/ringil-agent-sdk"
)

// tryLock takes an exclusive, non-blocking lock on f. flock locks belong to
// the open file description, so a second Open in the same process conflicts
// too.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK {
		return false, nil
	}
	return err == nil, err
}

// openFlags refuses to follow a symlink planted where a session file goes.
const openFlags = unix.O_NOFOLLOW | unix.O_CLOEXEC

// checkPrivate refuses a directory owned by someone else or writable by
// others. Group write is allowed: with the usual per-user groups (umask 002)
// it gives nobody else access.
func checkPrivate(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return errors.New(agent.ErrSessionIO, "session directory is not owned by the current user")
	}
	if info.Mode().Perm()&0o002 != 0 {
		return errors.New(agent.ErrSessionIO, "session directory is writable by others; chmod 700 it")
	}
	return nil
}
