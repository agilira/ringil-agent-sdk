// audit_test.go: regressions for the skills audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

// Finding 1: one damaged skill stopped the whole catalog.
func TestBrokenSkillIsLeftOut(t *testing.T) {
	_, cat := installed(t)
	if err := os.Mkdir(filepath.Join(cat, "junk"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := Open(cat)
	if err != nil || len(c.List()) != 1 || !errors.HasCode(c.Broken()["junk"], ErrTampered) {
		t.Fatalf("list %v broken %v err %v", c.List(), c.Broken(), err)
	}
}

// Finding 6: fences, duplicate keys, quoting, characters not bytes.
func TestParserStrictness(t *testing.T) {
	bad := []string{
		"---\nname: a\ndescription: d\n---trailing\nbody",
		"---\nname: a\nname: b\ndescription: d\n---\n",
	}
	for _, b := range bad {
		if _, _, err := parse(b); !errors.HasCode(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", b, err)
		}
	}
	info, body, err := parse("---\nname: a\ndescription: 'it''s \"fine\"'\n---\nuno\n---\ndue")
	if err != nil || info.Description != `it's "fine"` || body != "uno\n---\ndue" {
		t.Fatalf("%+v %q %v", info, body, err)
	}
	cjk := "---\nname: a\ndescription: " + strings.Repeat("技", 600) + "\n---\n"
	if _, _, err := parse(cjk); err != nil {
		t.Fatalf("600 characters refused: %v", err)
	}
	if info, _, err := parse("---\rname: a\rdescription: d\r---\rb"); err != nil || info.Name != "a" {
		t.Fatalf("CR endings: %v", err)
	}
}

// Remove renames first: an interrupted removal leaves nothing loadable.
func TestRemoveLeavesNothing(t *testing.T) {
	_, cat := installed(t)
	if err := Remove(cat, "review"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cat)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left %v %v", entries, err)
	}
}
