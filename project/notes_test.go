// notes_test.go: a project's notes for a host
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Notes are written, listed, read and removed by path, each recorded;
// what is not a note's path is refused.
func TestNotes(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "n"}, "user"))(t)
	if err := s.WriteNote(p.ID, "decisions/tone.md", "formal", "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteNote(p.ID, "INDEX.md", "- tone", "agent:writer"); err != nil {
		t.Fatal(err)
	}
	if got := get(s.Notes(p.ID))(t); strings.Join(got, ",") != "INDEX.md,decisions/tone.md" {
		t.Fatalf("%v", got)
	}
	if got := get(s.Note(p.ID, "decisions/tone.md"))(t); got != "formal" {
		t.Fatalf("%q", got)
	}
	for _, bad := range []string{"../x.md", "/abs.md", "a/../b.md", "x.txt", "a//b.md", ".hidden.md", "a/./b.md", ""} {
		wantCode(t, s.WriteNote(p.ID, bad, "x", "user"), ErrInput, bad)
		_, err := s.Note(p.ID, bad)
		wantCode(t, err, ErrInput, bad)
	}
	if err := s.RemoveNote(p.ID, "INDEX.md", "user"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.RemoveNote(p.ID, "INDEX.md", "user"), ErrNotFound, "twice")
	evs, _, _ := s.Timeline(p.ID, 0, 10)
	if kindsOf(evs) != "project_created note note note" {
		t.Fatalf("%s", kindsOf(evs))
	}
}

// A link in the notes' folder is not followed out of it.
func TestNoteLinkNotFollowed(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "n"}, "user"))(t)
	secret := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(secret, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := get(s.NotesDir(p.ID))(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.md")); err != nil {
		t.Skip("no links here")
	}
	if got, err := s.Note(p.ID, "leak.md"); err == nil {
		t.Fatalf("read through a link: %q", got)
	}
	if got := get(s.Notes(p.ID))(t); len(got) != 0 {
		t.Fatalf("a link listed: %v", got)
	}
}
