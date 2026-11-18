// lockfile_test.go: tests for locks and private directories
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package lockfile

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func openLock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func TestTryLockExcludesSecondOpener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, b := openLock(t, path), openLock(t, path)
	if ok, err := TryLock(a); !ok || err != nil {
		t.Fatalf("first %v %v", ok, err)
	}
	if ok, err := TryLock(b); ok || err != nil {
		t.Fatalf("second %v %v", ok, err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLock(b); !ok || err != nil {
		t.Fatalf("after unlock %v %v", ok, err)
	}
}

func TestLockWaitsAndHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, b := openLock(t, path), openLock(t, path)
	if err := Lock(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Lock(ctx, b); err != context.DeadlineExceeded {
		t.Fatalf("err %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		released <- Unlock(a)
	}()
	if err := Lock(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

func TestCheckPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ACLs")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || CheckPrivate(info) != nil {
		t.Fatalf("private dir refused: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(dir); err != nil || CheckPrivate(info) != ErrNotPrivate {
		t.Fatalf("shared dir accepted: %v", err)
	}
}
