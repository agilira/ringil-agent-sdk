// open.go: opening a lock file shared by several processes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package lockfile

import "os"

// Open opens the lock file name in root, creating it only when missing:
// a plain O_CREATE open by many processes at once can fail transiently on
// macOS, opening first and creating exclusively avoids that race.
func Open(root *os.Root, name string) (*os.File, error) {
	for range 3 {
		f, err := root.OpenFile(name, os.O_RDWR|NoFollow, 0)
		if !os.IsNotExist(err) {
			return f, err
		}
		f, err = root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|NoFollow, 0o600)
		if !os.IsExist(err) {
			return f, err
		}
	}
	return nil, os.ErrExist
}
