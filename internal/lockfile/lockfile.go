// lockfile.go: exclusive file locks and private directories
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package lockfile holds the platform pieces shared by the SDK's on-disk
// stores: exclusive locks on a file, the open flag that refuses symlinks,
// and the check that a directory is private to the user.
package lockfile

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrNotPrivate is returned by CheckPrivate for a directory another user
// owns or can write to.
var ErrNotPrivate = errors.New("directory is not private to the current user")

// Lock waits for an exclusive lock on f until ctx ends. Polling keeps it
// portable and cancellable; the stores hold their locks briefly.
func Lock(ctx context.Context, f *os.File) error {
	for {
		ok, err := TryLock(f)
		if err != nil || ok {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
