// file_test.go: tests for the file session store
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

var bg = context.Background()

func newStore(t *testing.T) (agent.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func open(t *testing.T, s agent.Store, id string) agent.Session {
	t.Helper()
	sess, err := s.Open(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func closeOK(t *testing.T, sess agent.Session) {
	t.Helper()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendOK(t *testing.T, sess agent.Session, msgs ...agent.Message) {
	t.Helper()
	if err := sess.Append(bg, msgs...); err != nil {
		t.Fatal(err)
	}
}

func turn() []agent.Message {
	return []agent.Message{
		agent.UserText("quanto fa 2+2?"),
		{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "calc", Input: json.RawMessage(`{"a":2}`)}}},
		agent.ToolMessage(agent.ToolResult{CallID: "c1", Content: "4"}),
		agent.AssistantText("4"),
	}
}

func TestRoundTripAcrossOpens(t *testing.T) {
	s, _ := newStore(t)
	sess := open(t, s, "chat")
	if sess.ID() != "chat" || len(sess.Messages()) != 0 {
		t.Fatalf("new session %q %d", sess.ID(), len(sess.Messages()))
	}
	appendOK(t, sess, turn()[:2]...)
	appendOK(t, sess, turn()[2:]...)
	closeOK(t, sess)
	again := open(t, s, "chat")
	defer closeOK(t, again)
	got := again.Messages()
	if len(got) != 4 || got[3].Text() != "4" || string(got[1].Calls[0].Input) != `{"a":2}` {
		t.Fatalf("reloaded %+v", got)
	}
	appendOK(t, again, agent.UserText("e 3+3?"))
	if len(again.Messages()) != 5 {
		t.Fatal("append after reload")
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	s, dir := newStore(t)
	closeOK(t, open(t, s, "p"))
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "p.jsonl"): 0o600, filepath.Join(dir, "p.lock"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v (%v)", path, info.Mode().Perm(), want, err)
		}
	}
}

func TestSecondOpenIsBusy(t *testing.T) {
	s, _ := newStore(t)
	first := open(t, s, "busy")
	if _, err := s.Open(bg, "busy"); !errors.HasCode(err, agent.ErrSessionBusy) {
		t.Fatalf("second open: %v", err)
	}
	if err := s.Delete(bg, "busy"); !errors.HasCode(err, agent.ErrSessionBusy) {
		t.Fatalf("delete while open: %v", err)
	}
	closeOK(t, first)
	closeOK(t, open(t, s, "busy"))
}

func TestInvalidIDs(t *testing.T) {
	s, dir := newStore(t)
	for _, id := range []string{"../escape", "a/b", "", "x.jsonl"} {
		if _, err := s.Open(bg, id); !errors.HasCode(err, agent.ErrSessionInput) {
			t.Errorf("open %q: %v", id, err)
		}
		if err := s.Delete(bg, id); !errors.HasCode(err, agent.ErrSessionInput) {
			t.Errorf("delete %q: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a file was created outside the store")
	}
}

func TestSymlinkRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, dir := newStore(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(bg, "link"); !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("symlinked session opened: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "x" {
		t.Fatalf("symlink target was written: %q %v", data, err)
	}
	closeOK(t, open(t, s, "fine")) // the lock was released after the refusal
}

func TestCrashLeavesPartialLineThatIsCut(t *testing.T) {
	s, dir := newStore(t)
	sess := open(t, s, "crash")
	appendOK(t, sess, turn()[:2]...)
	closeOK(t, sess)
	path := filepath.Join(dir, "crash.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"msg":{"role":"to`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	again := open(t, s, "crash")
	if len(again.Messages()) != 2 {
		t.Fatalf("messages %d", len(again.Messages()))
	}
	appendOK(t, again, turn()[2])
	closeOK(t, again)
	final := open(t, s, "crash")
	defer closeOK(t, final)
	if len(final.Messages()) != 3 {
		t.Fatalf("after recovery %d", len(final.Messages()))
	}
}

func damage(t *testing.T, dir, id string, edit func([]byte) []byte) {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edit(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDamageIsDetected(t *testing.T) {
	edits := map[string]func([]byte) []byte{
		"edited message": func(d []byte) []byte { return bytes.Replace(d, []byte(`"4"`), []byte(`"5"`), 1) },
		"removed line": func(d []byte) []byte {
			lines := bytes.SplitAfter(d, []byte("\n"))
			return bytes.Join(append(lines[:2:2], lines[3:]...), nil)
		},
		"swapped lines": func(d []byte) []byte {
			lines := bytes.SplitAfter(d, []byte("\n"))
			lines[1], lines[2] = lines[2], lines[1]
			return bytes.Join(lines, nil)
		},
		"bad header":     func(d []byte) []byte { return bytes.Replace(d, []byte("ringil-session"), []byte("other"), 1) },
		"future version": func(d []byte) []byte { return bytes.Replace(d, []byte(`"version":1`), []byte(`"version":9`), 1) },
		"extra field":    func(d []byte) []byte { return bytes.Replace(d, []byte(`{"seq":1,`), []byte(`{"x":1,"seq":1,`), 1) },
		"garbage line":   func(d []byte) []byte { return append(d, []byte("not json\n")...) },
	}
	for name, edit := range edits {
		s, dir := newStore(t)
		sess := open(t, s, "d")
		appendOK(t, sess, turn()...)
		closeOK(t, sess)
		damage(t, dir, "d", edit)
		if _, err := s.Open(bg, "d"); !errors.HasCode(err, agent.ErrSessionCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStructurallyInvalidHistoryIsCorrupt(t *testing.T) {
	data, _, _, err := encode([]agent.Message{agent.ToolMessage(agent.ToolResult{CallID: "x"})}, 0, genesis)
	if err != nil {
		t.Fatal(err)
	}
	s, dir := newStore(t)
	if err := os.WriteFile(filepath.Join(dir, "orphan.jsonl"), append(headerLine(), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(bg, "orphan"); !errors.HasCode(err, agent.ErrSessionCorrupt) {
		t.Fatalf("orphan result accepted: %v", err)
	}
}

func seed(t *testing.T, s agent.Store, dir string) {
	t.Helper()
	for _, id := range []string{"a", "b"} {
		sess := open(t, s, id)
		appendOK(t, sess, agent.UserText(id))
		closeOK(t, sess)
	}
	for _, junk := range []string{"bad name.jsonl", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, junk), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func checkOnly(t *testing.T, s agent.Store, id string) {
	t.Helper()
	if list, err := s.List(bg); err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestListAndDelete(t *testing.T) {
	s, dir := newStore(t)
	seed(t, s, dir)
	list, err := s.List(bg)
	if err != nil || len(list) != 2 || list[0].Size == 0 {
		t.Fatalf("list %+v %v", list, err)
	}
	if err := s.Delete(bg, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(bg, "never-existed"); err != nil {
		t.Fatalf("deleting a missing session: %v", err)
	}
	checkOnly(t, s, "b")
	fresh := open(t, s, "a")
	defer closeOK(t, fresh)
	if len(fresh.Messages()) != 0 {
		t.Fatal("deleted session came back")
	}
}

func TestClosedSession(t *testing.T) {
	s, _ := newStore(t)
	sess := open(t, s, "c")
	closeOK(t, sess)
	closeOK(t, sess) // idempotent
	if err := sess.Append(bg, agent.UserText("x")); !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("append after close: %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	s, _ := newStore(t)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := s.Open(ctx, "x"); err == nil {
		t.Fatal("open")
	}
	if _, err := s.List(ctx); err == nil {
		t.Fatal("list")
	}
	if err := s.Delete(ctx, "x"); err == nil {
		t.Fatal("delete")
	}
	sess := open(t, s, "x")
	defer closeOK(t, sess)
	if err := sess.Append(ctx, agent.UserText("x")); err == nil {
		t.Fatal("append")
	}
}

func TestNewRefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("err %v", err)
	}
}

func TestMessagesAreCopies(t *testing.T) {
	s, _ := newStore(t)
	sess := open(t, s, "m")
	defer closeOK(t, sess)
	msg := agent.UserText("orig")
	appendOK(t, sess, msg)
	msg.Parts[0].Text = "changed"
	got := sess.Messages()
	got[0].Parts[0].Text = "changed too"
	if sess.Messages()[0].Text() != "orig" {
		t.Fatal("session shares memory")
	}
}

// FuzzDecode feeds arbitrary file contents to the loader: it must never
// panic, and whatever it accepts must be a valid history.
func FuzzDecode(f *testing.F) {
	good, _, _, err := encode(turn(), 0, genesis)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(append(headerLine(), good...))
	f.Add(headerLine())
	f.Add([]byte(`{"format":"ringil-session","version":1}` + "\n" + `{"seq":1,"msg":{},"hash":"00"}` + "\n"))
	f.Add([]byte("partial"))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := decode(data)
		if err != nil {
			return
		}
		if verr := agent.ValidateHistory(d.msgs); verr != nil || d.valid > int64(len(data)) || int64(len(d.msgs)) != d.seq {
			t.Fatalf("accepted bad state: %v valid=%d seq=%d msgs=%d", verr, d.valid, d.seq, len(d.msgs))
		}
		if d.valid > 0 && !d.unterminated && !strings.HasSuffix(string(data[:d.valid]), "\n") {
			t.Fatal("valid prefix does not end at a line")
		}
	})
}

// A session file larger than maxSession is refused on Open rather than read
// into memory (regression: readSession had no size bound).
func TestSessionFileTooLarge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big"+dataExt)
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxSession + 1); err != nil { // sparse: no real blocks
		_ = f.Close()
		t.Skipf("cannot make a sparse oversized file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(bg, "big"); !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("want ErrSessionIO for an oversized session, got %v", err)
	}
}

func TestReadBoundedRejectsPastLimit(t *testing.T) {
	dir := t.TempDir()
	okPath := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(okPath, []byte(strings.Repeat("a", 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(okPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readBounded(f, 16)
	_ = f.Close()
	if err != nil || len(data) != 16 {
		t.Fatalf("exact limit failed: len=%d err=%v", len(data), err)
	}
	bigPath := filepath.Join(dir, "big.jsonl")
	if err := os.WriteFile(bigPath, []byte(strings.Repeat("a", 17)), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := os.Open(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readBounded(g, 16)
	_ = g.Close()
	if !errors.HasCode(err, agent.ErrSessionIO) {
		t.Fatalf("want ErrSessionIO past limit, got %v", err)
	}
}

// titled makes a store with one session, "a".
func titled(t *testing.T) (agent.TitleStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Open(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(context.Background(), agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()
	return st.(agent.TitleStore), dir
}

// A title is kept beside the history and listed without opening it.
func TestTitles(t *testing.T) {
	ts, _ := titled(t)
	ctx := context.Background()
	if err := ts.SetTitle(ctx, "a", "  My\ttitle  "); err != nil {
		t.Fatal(err)
	}
	if list, _ := ts.List(ctx); len(list) != 1 || list[0].Title != "My title" {
		t.Fatalf("%+v", list)
	}
	if err := ts.SetTitle(ctx, "../x", "t"); err == nil {
		t.Fatal("a bad ID was accepted")
	}
}

// A title written by hand is cleaned when read; it goes with its session.
func TestTitleByHandAndDelete(t *testing.T) {
	ts, dir := titled(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "a.title"), []byte("bad\x1b[2Jtitle\n"+strings.Repeat("x", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, _ := ts.List(ctx); len(list[0].Title) > agent.MaxTitle || strings.ContainsRune(list[0].Title, 0x1b) {
		t.Fatalf("%q", list[0].Title)
	}
	if err := ts.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.title")); !os.IsNotExist(err) {
		t.Fatal("the title outlived its session")
	}
}

func TestTitleConcurrentWrites(t *testing.T) {
	ts, _ := titled(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 4 {
				if err := ts.SetTitle(bg, "a", fmt.Sprintf("title-%d-%d", g, i)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
