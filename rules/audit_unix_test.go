// audit_unix_test.go: hard links, Unix only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Finding 3: a hard link brought a private key in under a rules name.
func TestHardLinkRefused(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	key := filepath.Join(other, "id_ed25519")
	write(t, key, "PRIVATE KEY")
	if err := os.Link(key, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skip("no hard links")
	}
	if text := Render(Load(Config{Dir: root})); strings.Contains(text, "PRIVATE") {
		t.Fatalf("render %q", text)
	}
}
