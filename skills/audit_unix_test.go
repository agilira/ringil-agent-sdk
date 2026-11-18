// audit_unix_test.go: regressions that need Unix links and modes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

// Finding 4: a hard link brought outside content into the skill.
func TestHardLinkRefused(t *testing.T) {
	src := newSkill(t, map[string]string{"SKILL.md": reviewSkill})
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(src, "notes.md")); err != nil {
		t.Skip("no hard links here")
	}
	// Finding 5: the reason reaches the user, with its own code.
	_, err := Install(bg, src, filepath.Join(t.TempDir(), "s"))
	if !errors.HasCode(err, ErrInvalid) || !strings.Contains(err.Error(), "notes.md") {
		t.Fatalf("hard link: %v", err)
	}
}

// Plausible: a catalog writable by others could be planted.
func TestCatalogMustBePrivate(t *testing.T) {
	_, cat := installed(t)
	if err := os.Chmod(cat, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cat); !errors.HasCode(err, ErrIO) {
		t.Fatalf("world-writable catalog opened: %v", err)
	}
}
