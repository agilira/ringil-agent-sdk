// audit_test.go: what the audit of 26/11 found in imports
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agilira/go-errors"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fileState is a state of one file source and one directory source.
func fileState(t *testing.T) map[string]Source {
	t.Helper()
	dir := t.TempDir()
	agents := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "notes")
	if err := os.Mkdir(notes, 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]Source{"agents": {Path: agents, File: true}, "notes": {Path: notes}}
}

// Archives whose manifest lies about its sources are refused before
// anything moves: the state as it was, nothing beside it.
func TestManifestSourcesChecked(t *testing.T) {
	for name, archive := range map[string][]byte{
		"named twice":       forged(t, map[string]string{"agents": "new"}, `{"version":1,"sources":["agents","agents"],"files":{"agents":"`+sum("new")+`"}}`),
		"file not there":    forged(t, map[string]string{}, `{"version":1,"sources":["agents"],"files":{}}`),
		"file as directory": forged(t, map[string]string{"agents/x": "new"}, `{"version":1,"sources":["agents"],"files":{"agents/x":"`+sum("new")+`"}}`),
		"directory as file": forged(t, map[string]string{"notes": "new"}, `{"version":1,"sources":["notes"],"files":{"notes":"`+sum("new")+`"}}`),
		"not named":         forged(t, map[string]string{"notes/a": "new"}, `{"version":1,"sources":[],"files":{"notes/a":"`+sum("new")+`"}}`),
	} {
		sources := fileState(t)
		if _, err := Import(bg, bytes.NewReader(archive), Options{Sources: sources, LockDir: t.TempDir()}); err == nil {
			t.Errorf("%s: imported", name)
		}
		if b, _ := os.ReadFile(sources["agents"].Path); string(b) != "mine" {
			t.Errorf("%s: AGENTS.md is %q", name, b)
		}
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(sources["agents"].Path), ".*")); len(left) != 0 {
			t.Errorf("%s: left beside %v", name, left)
		}
	}
}

// An import cut short mid-swap is undone by the next process that uses
// the state.
func TestCutShortImportUndone(t *testing.T) {
	sources := fileState(t)
	lockDir := t.TempDir()
	agents := sources["agents"].Path
	old := filepath.Join(filepath.Dir(agents), ".AGENTS.md.old-x")
	staged := filepath.Join(filepath.Dir(agents), ".AGENTS.md.import-x")
	// As a crash leaves it: the old state moved aside, the new one in place.
	if err := os.Rename(agents, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agents, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(journal{Moves: []move{{Target: agents, Old: old, HadOld: true, Staged: staged}}})
	if err := os.WriteFile(filepath.Join(lockDir, journalName), j, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := Use(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if b, _ := os.ReadFile(agents); string(b) != "mine" {
		t.Fatalf("AGENTS.md is %q", b)
	}
	if _, err := os.Stat(filepath.Join(lockDir, journalName)); !os.IsNotExist(err) {
		t.Fatal("the journal stayed")
	}
}

// Before runs with the state held, and its error stops the import.
func TestBeforeUnderTheLock(t *testing.T) {
	sources := fileState(t)
	var buf bytes.Buffer
	if _, err := Create(bg, &buf, sources, nil); err != nil {
		t.Fatal(err)
	}
	lockDir := t.TempDir()
	stop := errors.New("COPY", "no copy")
	_, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: sources, LockDir: lockDir, Before: func() error {
		if _, err := Use(lockDir); !errors.HasCode(err, ErrBusy) {
			t.Errorf("the state was not held during Before: %v", err)
		}
		return stop
	}})
	if !errors.HasCode(err, "COPY") {
		t.Fatalf("%v", err)
	}
}

// An import finds one cut short before it: undone first, its old state
// back, and what it left beside swept.
func TestImportUndoesOneCutShort(t *testing.T) {
	sources := fileState(t)
	var buf bytes.Buffer
	if _, err := Create(bg, &buf, sources, nil); err != nil {
		t.Fatal(err)
	}
	lockDir := t.TempDir()
	agents := sources["agents"].Path
	old := filepath.Join(filepath.Dir(agents), ".AGENTS.md.old-x")
	if err := os.Rename(agents, old); err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(journal{Moves: []move{{Target: agents, Old: old, HadOld: true, Staged: agents + ".gone"}}})
	if err := os.WriteFile(filepath.Join(lockDir, journalName), j, 0o600); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(filepath.Dir(agents), ".notes.import-y")
	if err := os.Mkdir(left, 0o700); err != nil {
		t.Fatal(err)
	}
	var copied string
	if _, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: sources, LockDir: lockDir, Before: func() error {
		b, _ := os.ReadFile(agents)
		copied = string(b)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if copied != "mine" {
		t.Fatalf("the copy before saw %q, not the state put back", copied)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatal("a leftover of an extraction stayed")
	}
}

// A source that is a link is replaced where it points, the link kept.
func TestImportThroughALink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	link := linkedFile(t, real, filepath.Join(dir, "AGENTS.md"))
	sources := map[string]Source{"agents": {Path: link, File: true}}
	var buf bytes.Buffer
	if _, err := Create(bg, &buf, sources, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: sources, LockDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	if b, _ := os.ReadFile(real); string(b) != "v1" {
		t.Fatalf("through the link: %q", b)
	}
}

// linkedFile writes real ("v1") and a link to it.
func linkedFile(t *testing.T, real, link string) string {
	t.Helper()
	if err := os.WriteFile(real, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks")
	}
	return link
}

func TestImportCreatesMissingParents(t *testing.T) {
	sources := fileState(t)
	if err := os.WriteFile(filepath.Join(sources["notes"].Path, "a.md"), []byte("note"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Create(bg, &buf, sources, nil); err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	dst := map[string]Source{
		"agents": {Path: filepath.Join(fresh, "sub", "cfg", "AGENTS.md"), File: true},
		"notes":  {Path: filepath.Join(fresh, "sub", "data", "notes")},
	}
	if _, err := Import(bg, bytes.NewReader(buf.Bytes()), Options{Sources: dst, LockDir: filepath.Join(fresh, "locks")}); err != nil {
		t.Fatalf("import into missing parent dirs: %v", err)
	}
	if b, err := os.ReadFile(dst["agents"].Path); err != nil || string(b) != "mine" {
		t.Fatalf("agents=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst["notes"].Path, "a.md")); err != nil || string(b) != "note" {
		t.Fatalf("note=%q err=%v", b, err)
	}
}
