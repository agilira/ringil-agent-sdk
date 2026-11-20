// later_test.go: checks that come back to their chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	stderrors "errors"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func codeOf(err error) errors.ErrorCode {
	var e *errors.Error
	if stderrors.As(err, &e) {
		return e.Code
	}
	return ""
}

// comeBack retries while the chat finishes what it was doing.
func comeBack(t *testing.T, b *Bridge, conv string) {
	t.Helper()
	for range 500 {
		err := b.ComeBack(conv, "<check-later>look</check-later>")
		if err == nil {
			return
		}
		if codeOf(err) != ErrBusy {
			t.Fatalf("come back: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the chat stayed busy")
}

func TestACheckComesBackToItsPrivateChat(t *testing.T) {
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("cleaned"), fake.Reply("the download is done"), fake.Reply(" [quiet]\n"))
	r.app.say("clean up")
	r.app.next(t) // the question: the chat is in a run
	conv := r.bridge.current(room)
	if !r.bridge.Private(conv) {
		t.Fatal("a private chat's run is not private")
	}
	if err := r.bridge.ComeBack(conv, "look"); codeOf(err) != ErrBusy {
		t.Fatalf("came back into a run: %v", err)
	}
	r.app.say("no")
	for r.app.next(t).Text != "cleaned" {
	}
	comeBack(t, r.bridge, conv)
	if got := r.app.next(t).Text; got != "the download is done" {
		t.Fatalf("%q", got)
	}
	comeBack(t, r.bridge, conv)
	r.app.quiet(t) // nothing new: nothing sent
	if r.ran.Load() {
		t.Fatal("the tool ran")
	}
}

func TestACheckEndsWithItsConversation(t *testing.T) {
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("cleaned"))
	r.app.say("clean up")
	r.app.next(t) // Run runs
	if err := r.bridge.ComeBack("test-9", "look"); codeOf(err) != ErrGone {
		t.Fatalf("a conversation never seen: %v", err)
	}
	conv := r.bridge.current(room)
	r.bridge.Private(conv)
	r.app.say("no")
	for r.app.next(t).Text != "cleaned" {
	}
	r.app.say("/new")
	r.app.next(t)
	if err := r.bridge.ComeBack(conv, "look"); codeOf(err) != ErrGone {
		t.Fatalf("came back to a conversation the chat left: %v", err)
	}
}

func TestAGroupAndThePublicHaveNoChecks(t *testing.T) {
	b, _, _, _ := teamBridge(t, Config{})
	b.mu.Lock()
	b.chats["g"] = &chat{conv: "test-g", group: true, from: me, role: owner}
	b.chats["p"] = &chat{conv: "test-p", from: "999", role: public}
	b.mu.Unlock()
	if b.Private("test-g") || b.Private("test-p") || b.Private("test-none") {
		t.Fatal("private")
	}
}

func TestBacksAreBounded(t *testing.T) {
	b, _, _, _ := teamBridge(t, Config{})
	for i := range maxBacks + 5 {
		conv := "test-" + string(rune('a'+i%26)) + time.Duration(i).String()
		b.mu.Lock()
		b.chats["c"] = &chat{conv: conv, from: me, role: owner}
		b.mu.Unlock()
		b.Private(conv)
		b.Private(conv) // seen again: not counted twice
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.chats, "c")
	if len(b.backs) != maxBacks || len(b.backOrder) != maxBacks {
		t.Fatalf("%d backs, %d in order", len(b.backs), len(b.backOrder))
	}
}

// A check's run asks in its chat as any other run does.
func TestACheckAsksInItsChat(t *testing.T) {
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("cleaned"), fake.Call("delete_file", `{}`), fake.Reply("not now"))
	r.app.say("clean up")
	r.app.next(t)
	conv := r.bridge.current(room)
	r.bridge.Private(conv)
	r.app.say("no")
	for r.app.next(t).Text != "cleaned" {
	}
	comeBack(t, r.bridge, conv)
	if q := r.app.next(t); len(q.Buttons) == 0 {
		t.Fatalf("the check's call was not asked: %+v", q)
	}
	r.app.say("no")
	for r.app.next(t).Text != "not now" {
	}
	if r.ran.Load() {
		t.Fatal("the tool ran unasked")
	}
}
