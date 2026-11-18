// atomicfile.go: replacing a file so that readers see old or new, never half
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package atomicfile writes a file inside an os.Root atomically: a new
// temporary file, synced, renamed over the old one, then the directory
// synced so the rename itself survives a crash.
package atomicfile

import (
	"crypto/rand"
	"os"
	"strings"

	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// Write replaces name in root with data, mode 0600.
func Write(root *os.Root, name string, data []byte) error {
	tmp := "." + name + "-" + strings.ToLower(rand.Text()[:12]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|lockfile.NoFollow, 0o600)
	if err != nil {
		return err
	}
	if err := fill(f, data); err != nil {
		return discard(err, root, tmp)
	}
	if err := root.Rename(tmp, name); err != nil {
		return discard(err, root, tmp)
	}
	return syncDir(root)
}

// discard removes the temporary file, keeping the first failure.
func discard(err error, root *os.Root, tmp string) error {
	if rerr := root.Remove(tmp); rerr != nil {
		return &cleanupError{err: err, cleanup: rerr}
	}
	return err
}

type cleanupError struct{ err, cleanup error }

func (e *cleanupError) Error() string { return e.err.Error() + " (cleanup: " + e.cleanup.Error() + ")" }
func (e *cleanupError) Unwrap() error { return e.err }

func fill(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
