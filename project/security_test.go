// security_test.go: the store's names and files at their bounds
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// No id reaches a file outside the store, and nothing panics.
func FuzzIDs(f *testing.F) {
	for _, s := range []string{"q4", "../x", "a/b", "..", ".", "C:x", "a\x00b", "notes"} {
		f.Add(s, "a title")
	}
	f.Fuzz(func(t *testing.T, id, title string) {
		base := t.TempDir()
		if err := os.WriteFile(filepath.Join(base, "secret"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(filepath.Join(base, "store"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		p, err := s.Create(Project{ID: id, Name: "n"}, "user")
		if err == nil && !validID.MatchString(p.ID) {
			t.Fatalf("made %q", p.ID)
		}
		_, _ = s.Get(id)
		_, _, _ = s.AddCard(id, "user", Card{Title: title})
		_, _, _ = s.Timeline(id, 0, 10)
		_ = s.RemoveCard(id, title, "user")
		if !validID.MatchString(id) {
			if err := s.Remove(id); err == nil {
				t.Fatalf("removed %q", id)
			}
		}
		if _, err := os.Stat(filepath.Join(base, "secret")); err != nil {
			t.Fatal("a file outside the store went")
		}
	})
}

// A directory others may write is not a store; a damaged file is said.
func TestStoreBounds(t *testing.T) {
	if runtime.GOOS != "windows" {
		dir := filepath.Join(t.TempDir(), "open")
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(dir, 0o777)
		_, err := Open(dir)
		wantCode(t, err, ErrStore, "open to others")
	}
	s := open(t)
	p := get(s.Create(Project{Name: "x"}, "user"))(t)
	if err := os.WriteFile(filepath.Join(s.dir, p.ID, cardsFile), []byte(`[{"id":"c1","admin":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Cards(p.ID)
	wantCode(t, err, ErrStore, "a damaged board")
	_, err = s.Create(Project{Name: "x", Columns: make([]Column, 13)}, "user")
	wantCode(t, err, ErrInput, "13 columns")
	_, err = s.Create(Project{Name: "x", Folder: "relative"}, "user")
	wantCode(t, err, ErrInput, "a relative folder")
}
