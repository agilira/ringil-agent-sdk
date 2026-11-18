// audit_test.go: regressions for the session audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package file

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Finding 2: an Append that would make the session unloadable is refused.
func TestInvalidAppendIsRefused(t *testing.T) {
	s, _ := newStore(t)
	sess := open(t, s, "v")
	appendOK(t, sess, agent.UserText("x"), agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "t", Input: json.RawMessage(`{}`)}}})
	if err := sess.Append(bg, agent.UserText("too early")); !errors.HasCode(err, agent.ErrSessionCorrupt) {
		t.Fatalf("append %v", err)
	}
	appendOK(t, sess, agent.ToolMessage(agent.ToolResult{CallID: "c1", Content: "ok"}))
	closeOK(t, sess)
	again := open(t, s, "v")
	defer closeOK(t, again)
	if len(again.Messages()) != 3 {
		t.Fatalf("messages %d", len(again.Messages()))
	}
}

// Finding 6: a verified last line that lost its newline is kept.
func TestUnterminatedLastLineIsKept(t *testing.T) {
	s, dir := newStore(t)
	sess := open(t, s, "u")
	appendOK(t, sess, turn()...)
	closeOK(t, sess)
	damage(t, dir, "u", func(d []byte) []byte { return bytes.TrimSuffix(d, []byte("\n")) })
	again := open(t, s, "u")
	if len(again.Messages()) != 4 {
		t.Fatalf("messages %d", len(again.Messages()))
	}
	appendOK(t, again, agent.UserText("next"))
	closeOK(t, again)
	final := open(t, s, "u")
	defer closeOK(t, final)
	if len(final.Messages()) != 5 {
		t.Fatalf("after append %d", len(final.Messages()))
	}
}

// Plausible finding: a foreign file is never overwritten.
func TestForeignFileIsNotOverwritten(t *testing.T) {
	s, dir := newStore(t)
	path := filepath.Join(dir, "notes.jsonl")
	if err := os.WriteFile(path, []byte("my notes, no newline"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(bg, "notes"); !errors.HasCode(err, agent.ErrSessionCorrupt) {
		t.Fatalf("open %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "my notes, no newline" {
		t.Fatalf("file changed: %q %v", data, err)
	}
}

func TestHeaderCutByCrashIsRewritten(t *testing.T) {
	s, dir := newStore(t)
	if err := os.WriteFile(filepath.Join(dir, "h.jsonl"), headerLine()[:10], 0o600); err != nil {
		t.Fatal(err)
	}
	sess := open(t, s, "h")
	appendOK(t, sess, agent.UserText("x"))
	closeOK(t, sess)
	closeOK(t, open(t, s, "h"))
}

// Plausible finding: a directory others can write to is refused.
func TestSharedDirectoryRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("New %v", err)
	}
}

// Finding 1, store side: one run at a time per open session.
func TestSessionAdmitsOneRun(t *testing.T) {
	s, _ := newStore(t)
	sess := open(t, s, "r")
	defer closeOK(t, sess)
	guard := sess.(agent.RunGuard)
	end, err := guard.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.BeginRun(); !errors.HasCode(err, agent.ErrSessionBusy) {
		t.Fatalf("second run %v", err)
	}
	end()
	end2, err := guard.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	end2()
}
