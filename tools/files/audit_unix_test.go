// audit_unix_test.go: FIFO regression, Unix only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package files

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Finding 2: grep on a FIFO blocked forever.
func TestFifoDoesNotHang(t *testing.T) {
	dir := tree(t, map[string]string{})
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink(t, "pipe", filepath.Join(dir, "lnk"))
	r := open(t, Config{Dirs: []string{dir}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range []string{"pipe", "lnk"} {
			failCall(t, r, "grep", map[string]any{"pattern": "x", "path": p}, ErrInput)
			failCall(t, r, "read_file", map[string]any{"path": p}, ErrInput)
		}
		mustCall(t, r, "grep", map[string]any{"pattern": "x"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a tool blocked on a FIFO")
	}
}
