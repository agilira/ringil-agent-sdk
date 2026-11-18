// group_unix_test.go: a plain server's children stop with it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package mcp

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Audit 18/11 finding 8: children of a plain server outlived Close.
func TestPlainServerChildrenStop(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	c, err := Connect(bg, Server{Name: "p", Command: []string{self}, Env: map[string]string{"RINGIL_MCP_TEST_SERVER": "1", "RINGIL_MCP_TEST_CHILD": pidFile}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Log(err)
	}
	for range 50 {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Log(err)
	}
	t.Fatal("the server's child outlived Close")
}
