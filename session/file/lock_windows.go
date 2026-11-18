// lock_windows.go: exclusive session locks on Windows
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package file

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive, non-blocking lock on the first byte of f.
func tryLock(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return err == nil, err
}

// openFlags: Windows has no O_NOFOLLOW. The Lstat check before opening
// refuses links, and os.Root keeps any path inside the directory.
const openFlags = 0

// checkPrivate: directory permissions on Windows are ACLs, inherited from
// the user profile for the default location; not checked here.
func checkPrivate(os.FileInfo) error { return nil }
