// mcp_test.go: the MCP configuration of the terminal agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

func TestLoadMCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if got, err := loadMCP(path, false); err != nil || got != nil {
		t.Fatalf("missing default: %v %v", got, err)
	}
	if _, err := loadMCP(path, true); err == nil {
		t.Fatal("missing explicit file accepted")
	}
	checkMCPFiles(t, path)
}

func checkMCPFiles(t *testing.T, path string) {
	t.Helper()
	write := func(body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"mcpServers":{"a":{"command":"x","args":["y"]},"b":{"type":"http","url":"https://h/mcp","headers":{"K":"${KEY}"}}}}`, 0o600)
	got, err := loadMCP(path, false)
	if err != nil || len(got) != 2 || got["b"].Headers["K"] != "${KEY}" {
		t.Fatalf("load %v %v", got, err)
	}
	write(`{"mcpServers":{},"extra":1}`, 0o600)
	if _, err := loadMCP(path, false); !errors.HasCode(err, ErrUsage) {
		t.Fatalf("unknown field accepted: %v", err)
	}
	if runtime.GOOS != "windows" {
		write(`{"mcpServers":{}}`, 0o666)
		if _, err := loadMCP(path, false); err == nil {
			t.Fatal("world-writable configuration accepted")
		}
	}
}

func TestHeaderReferencesResolvePerRequest(t *testing.T) {
	vals := map[string]string{"KEY": "k1"}
	res := secrets.Map(vals)
	h := headerFunc(map[string]string{"X-Api-Key": "${KEY}", "Authorization": "Bearer ${KEY}"}, res)
	got, err := h(context.Background())
	if err != nil || got.Get("X-Api-Key") != "k1" || got.Get("Authorization") != "Bearer k1" {
		t.Fatalf("headers %v %v", got, err)
	}
	vals["KEY"] = "k2"
	if got, _ := h(context.Background()); got.Get("X-Api-Key") != "k2" {
		t.Fatalf("rotated key not used: %v", got)
	}
	if _, err := headerFunc(map[string]string{"K": "${MISSING}"}, res)(context.Background()); err == nil {
		t.Fatal("missing secret accepted")
	}
}
