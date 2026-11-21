// later_test.go: check_later in the terminal and in serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/later"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// A run stays for the checks it set: a check with nothing new shows a
// note, not its [quiet]; the last one says where things stand.
func TestARunWaitsForItsChecks(t *testing.T) {
	h := newHarness(t, "",
		fake.Call("check_later", `{"what":"the download","every":"20ms","for":"1s","times":2}`),
		fake.Reply("I will check the download."),
		fake.Reply("[quiet]"),
		fake.Reply("The download is complete."))
	h.c.laterLimits = later.Limits{MinEvery: time.Millisecond}
	if code := h.run("run", "download it and keep an eye on it"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	out, notes := h.out.String(), h.err.String()
	if strings.Contains(out, "[quiet]") || !strings.Contains(notes, "nothing new") {
		t.Fatalf("a quiet check showed:\nout: %q\nnotes: %q", out, notes)
	}
	if !strings.Contains(out, "The download is complete.") || !strings.Contains(notes, "check 2 of 2") {
		t.Fatalf("the last check:\nout: %q\nnotes: %q", out, notes)
	}
}

func TestTheChatPromptTakesACheckComeDue(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	c := newCLI(pr, &bytes.Buffer{}, &bytes.Buffer{})
	c.due = make(chan later.Check, 1)
	c.due <- later.Check{ID: 3}
	if _, due, ok := c.promptLine(); !ok || due == nil || due.ID != 3 {
		t.Fatalf("%v %v", due, ok)
	}
	go func() { _, _ = pw.Write([]byte("hello\n")) }()
	if line, due, ok := c.promptLine(); !ok || due != nil || line != "hello" {
		t.Fatalf("%q %v %v", line, due, ok)
	}
}

// Checks come back where someone is: not in serve's API runs, nor in a
// trigger's runs.
func TestChecksOnlyWhereTheyComeBack(t *testing.T) {
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	if c.laterTools() != nil {
		t.Fatal("offered with no board")
	}
	c.board = later.NewBoard(later.Limits{}, func(later.Check) bool { return true })
	defer c.board.Close()
	if len(c.laterTools()) != 2 || c.checkConversation(context.Background()) != terminalChecks {
		t.Fatal("not offered in the terminal")
	}
	c.inTrigger = true
	if c.laterTools() != nil {
		t.Fatal("offered in a trigger's run")
	}
	c.inTrigger = false
	c.server = &serve.Server{}
	c.runConversation = "api-1"
	if c.laterTools() != nil {
		t.Fatal("offered in an API run")
	}
	c.runConversation = "telegram-5"
	if len(c.laterTools()) != 2 || c.checkConversation(context.Background()) != "" {
		t.Fatal("a chat run with no bridge running: offered, but no conversation to come back to")
	}
}

// Audit 20/11: a check come due during a turn piled up in a buffer and
// ran stale (also after /reset). Now it is handed over only while the
// conversation waits, and counted only then.
func TestATerminalCheckWaitsUncountedForTheConversation(t *testing.T) {
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.laterLimits = later.Limits{MinEvery: time.Millisecond}
	c.terminalBoard()
	defer c.shutdown()
	if _, err := c.board.Add(terminalChecks, "x", 5*time.Millisecond, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // a turn: nobody takes it
	select {
	case ch := <-c.due:
		if ch.Done != 1 {
			t.Fatalf("checks offered during the turn were counted: %+v", ch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never handed over")
	}
	c.board.StopAll(terminalChecks)
	select {
	case ch := <-c.due:
		t.Fatalf("a stopped check came: %+v", ch)
	case <-time.After(50 * time.Millisecond):
	}
}

// Audit 20/11: a check's approval read the line the user was typing for
// the chat. In the terminal a check's calls that would ask are refused.
func TestATerminalCheckDoesNotAsk(t *testing.T) {
	h := newHarness(t, "y\n",
		fake.Call("check_later", `{"what":"the notes","every":"10ms","for":"1s","times":1}`),
		fake.Reply("I will look."),
		fake.Call("notes", `{"command":"delete","path":"/project/INDEX.md"}`),
		fake.Reply("Could not delete the notes."))
	h.c.laterLimits = later.Limits{MinEvery: time.Millisecond}
	if code := h.run("run", "watch"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if notes := h.err.String(); !strings.Contains(notes, "a check may not ask in the terminal: notes refused") || strings.Contains(notes, "wants to run notes") {
		t.Fatalf("%q", notes)
	}
}
