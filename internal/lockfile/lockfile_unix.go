// lockfile_unix.go: locks and private directories on Unix
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package lockfile

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// TryLock takes an exclusive, non-blocking lock on f. flock locks belong to
// the open file description, so a second opener in the same process
// conflicts too.
func TryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK {
		return false, nil
	}
	return err == nil, err
}

// Unlock releases a lock taken on f.
func Unlock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

// NoFollow refuses to follow a symlink planted where a store's file goes.
const NoFollow = unix.O_NOFOLLOW | unix.O_CLOEXEC

// CheckPrivate refuses a directory owned by someone else, writable by
// others, or writable by a group other than the user's own. Group write is
// allowed only for a per-user group (gid equal to uid, the usual umask
// 002 setup), which gives nobody else access.
func CheckPrivate(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o002 != 0 {
		return ErrNotPrivate
	}
	if info.Mode().Perm()&0o020 != 0 && st.Gid != st.Uid {
		return ErrNotPrivate
	}
	return nil
}
