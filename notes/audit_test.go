// audit_test.go: regressions for the notes audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package notes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Finding 1: notes in a folder could be created by rename but never
// written.
func TestNestedNotesAreWritable(t *testing.T) {
	n, _, _ := newNotes(t)
	must(t, n, input{Command: "create", Path: "/scratchpad/a/b/plan.md", FileText: "uno\n"})
	must(t, n, input{Command: "str_replace", Path: "/scratchpad/a/b/plan.md", OldStr: "uno", NewStr: "due"})
	if got := must(t, n, input{Command: "view", Path: "/scratchpad/a/b/plan.md"}); got != "1\tdue\n" {
		t.Fatalf("view %q", got)
	}
}

// Finding 4: inserting at the end added a phantom line; CRLF was mixed.
func TestInsertAtEnd(t *testing.T) {
	cases := []struct{ text, add, want string }{
		{"", "A", "A\n"},
		{"a\nb\n", "c", "a\nb\nc\n"},
		{"a\nb", "c", "a\nb\nc"},
		{"a\r\nb\r\n", "c\nd", "a\r\nb\r\nc\r\nd\r\n"},
	}
	for _, c := range cases {
		lines, _, _ := splitLines(c.text)
		got, err := insertAt(c.text, len(lines), c.add)
		if err != nil || got != c.want {
			t.Errorf("%q + %q = %q %v, want %q", c.text, c.add, got, err, c.want)
		}
	}
}

// Finding 3: escaping grew the index past its bound.
func TestIndexBoundAfterEscaping(t *testing.T) {
	n, _, project := newNotes(t)
	if err := os.WriteFile(filepath.Join(project, IndexFile), []byte(strings.Repeat("<", 8000)), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := n.Context(bg)
	if err != nil || len(text) > maxIndex+512 {
		t.Fatalf("context of %d bytes %v", len(text), err)
	}
}

// Finding 2: changes to lasting notes, and any delete, need approval.
func TestNeedsApproval(t *testing.T) {
	dir := t.TempDir()
	n, err := New(Scope{Name: "scratchpad", Dir: filepath.Join(dir, "s")}, Scope{Name: "project", Dir: filepath.Join(dir, "p"), ConfirmChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(in input) bool {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return n.NeedsApproval(agent.ToolCall{Name: DefaultName, Input: raw})
	}
	if ask(input{Command: "create", Path: "/scratchpad/x.md"}) || ask(input{Command: "view", Path: "/project"}) {
		t.Fatal("asked for a scratch write or a read")
	}
	if !ask(input{Command: "create", Path: "/project/INDEX.md"}) || !ask(input{Command: "delete", Path: "/scratchpad/x.md"}) {
		t.Fatal("did not ask for a lasting change or a delete")
	}
}

// Finding 5: folders were not counted against the limit.
func TestFoldersCount(t *testing.T) {
	n, scratch, _ := newNotes(t)
	for i := range maxFiles {
		if err := os.Mkdir(filepath.Join(scratch, "d"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26))), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := call(t, n, input{Command: "create", Path: "/scratchpad/new.md", FileText: "x"}); err == nil {
		t.Fatal("limit ignored folders")
	}
}

// 18/11: Nemotron 4B inserted at line 0 of an INDEX.md that did not exist
// and gave up; that insert creates the note, other edits say to create.
func TestInsertCreatesAMissingNote(t *testing.T) {
	n, scratch, _ := newNotes(t)
	if out := must(t, n, input{Command: "insert", Path: "/scratchpad/INDEX.md", InsertLine: 0, InsertText: "PostgreSQL 17"}); !strings.Contains(out, "Saved") {
		t.Fatalf("%q", out)
	}
	if data, err := os.ReadFile(filepath.Join(scratch, "INDEX.md")); err != nil || string(data) != "PostgreSQL 17" {
		t.Fatalf("%q %v", data, err)
	}
	_, err := call(t, n, input{Command: "str_replace", Path: "/scratchpad/other.md", OldStr: "a", NewStr: "b"})
	if !strings.Contains(agent.ModelMessage(err), "does not exist yet; to start it, use command create") {
		t.Fatalf("%v", err)
	}
}

// 18/11: Qwen 3B sent new_path instead of path for create, every run; it
// is the path when the command is not rename. No path at all is named.
func TestMissingPathIsNamed(t *testing.T) {
	n, scratch, _ := newNotes(t)
	if out := must(t, n, input{Command: "create", NewPath: "/scratchpad/INDEX.md", FileText: "x"}); !strings.Contains(out, "Saved") {
		t.Fatalf("%q", out)
	}
	if _, err := os.Stat(filepath.Join(scratch, "INDEX.md")); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, n, input{Command: "create", FileText: "x"})
	if !strings.Contains(agent.ModelMessage(err), "path is missing") {
		t.Fatalf("%v", err)
	}
}
