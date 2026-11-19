// channels_test.go: each chat's documents to its own runs only
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInboxOf(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "telegram-5"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "telegram-6")); err != nil {
		t.Fatal(err)
	}
	c := &cli{inboxRoot: root}
	if got := c.inboxOf("telegram-5"); got != filepath.Join(root, "telegram-5") {
		t.Fatalf("own inbox %q", got)
	}
	for _, conv := range []string{"", "telegram-7", "telegram-6", "../x", "."} {
		if got := c.inboxOf(conv); got != "" {
			t.Errorf("%q granted %q", conv, got)
		}
	}
	if got := (&cli{}).inboxOf("telegram-5"); got != "" {
		t.Fatalf("without channels: %q", got)
	}
}

func TestNewAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{"24h": 24 * time.Hour, "90m": 90 * time.Minute, "never": -1} {
		if got, err := newAfter(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "30s", "-1h", "soon"} {
		if _, err := newAfter(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
