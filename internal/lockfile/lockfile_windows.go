// lockfile_windows.go: locks and private directories on Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package lockfile

import (
	"os"

	"golang.org/x/sys/windows"
)

// TryLock takes an exclusive, non-blocking lock on the first byte of f.
func TryLock(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return err == nil, err
}

// Unlock releases a lock taken on f.
func Unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// NoFollow: Windows has no O_NOFOLLOW; the stores refuse links with Lstat
// before opening, and os.Root keeps any path inside the directory.
const NoFollow = 0

// CheckPrivate: directory permissions on Windows are ACLs, inherited from
// the user profile for the default locations; not checked here.
func CheckPrivate(os.FileInfo) error { return nil }
