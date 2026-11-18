// audit_unix_test.go: FIFO regression, Unix only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package vector

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Finding 7: a FIFO at vectors.json hung every search.
func TestFifoVectorsDoNotHang(t *testing.T) {
	m, dir := newMemory(t, &conceptEmbedder{model: "m"})
	put(t, m, "Riunione con Acme")
	if err := syscall.Unlink(filepath.Join(dir, vectorFile)); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, vectorFile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.Search(ctx, "meeting", 1)
		done <- err
	}()
	select {
	case err := <-done:
		t.Log(err) // a partial or I/O error is fine; returning is the point
	case <-time.After(3 * time.Second):
		t.Fatal("search blocked on a FIFO")
	}
}
