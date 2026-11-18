// audit_unix_test.go: FIFO regression, Unix only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package local

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Finding 5: a FIFO at the memory's name hung every operation.
func TestFifoMemoryFileDoesNotHang(t *testing.T) {
	m, dir := newStore(t)
	if err := syscall.Mkfifo(filepath.Join(dir, dataFile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.Search(ctx, "x", 1)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.HasCode(err, agent.ErrMemoryIO) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("search blocked on a FIFO")
	}
}
