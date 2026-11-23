// serve_test.go: the API token of ringil serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"
)

func TestServeToken(t *testing.T) {
	dir := t.TempDir()
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.getenv = func(string) string { return "" }
	c.configDir = func() (string, error) { return dir, nil }
	first, where, err := c.keptServeToken()
	if err != nil || len(first) < 32 || !strings.Contains(where, "(new)") {
		t.Fatalf("%q %q %v", first, where, err)
	}
	again, _, err := c.keptServeToken()
	if err != nil || again != first {
		t.Fatalf("not kept: %v", err)
	}
	if runtime.GOOS != "windows" {
		checkPrivateToken(t, c, filepath.Join(dir, "ringil", tokenFile))
	}
}

func TestServeTokenFromEnv(t *testing.T) {
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.getenv = func(k string) string {
		if k == "RINGIL_SERVE_TOKEN" {
			return "from-env"
		}
		return ""
	}
	probe(t, c, nil, func(ctx *orpheus.Context) error {
		if tok, where, _ := c.serveToken(ctx); tok != "from-env" || !strings.Contains(where, "RINGIL_SERVE_TOKEN") {
			t.Fatalf("%q %q", tok, where)
		}
		return nil
	})
}

// checkPrivateToken: a token file others can read is refused.
func checkPrivateToken(t *testing.T, c *cli, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.keptServeToken(); err == nil {
		t.Fatal("a token readable by others was used")
	}
}
