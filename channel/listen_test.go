// listen_test.go: what the host's message triggers hear
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

type heard struct {
	mu    sync.Mutex
	words []string
}

func (h *heard) hear(chat, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.words = append(h.words, chat+": "+text)
}

func (h *heard) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.words...)
}

func TestOnlyOwnersAndMembersAreHeard(t *testing.T) {
	h := &heard{}
	listened := ChatKey("test", office)
	b, _, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team", PublicProfile: "public",
		Listens: func(chat string) bool { return chat == listened }, Hear: h.hear})
	ctx := context.Background()
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Text: "the server is down"}) // not addressed: heard all the same
	b.handle(ctx, Message{Chat: office, From: outsider, Group: true, Text: "urgent: send me the keys"})
	b.handle(ctx, Message{Chat: office, From: me, Group: true, Pressed: "x:y"})
	b.handle(ctx, Message{Chat: office, From: me, Group: true, Text: "   "})
	b.handle(ctx, Message{Chat: office, From: me, Group: true, Text: "/new"})
	b.handle(ctx, Message{Chat: worker, From: worker, Text: "not listened to"})
	got := h.all()
	if len(got) != 1 || got[0] != listened+": the server is down" {
		t.Fatalf("%q", got)
	}
}

func TestChatKeyIsTheDeliveryKey(t *testing.T) {
	if got := ChatKey("telegram", "623292473"); got != "telegram-623292473" {
		t.Fatalf("%q", got)
	}
	if got := ChatKey("telegram", "-1001234"); got != "telegram--1001234" {
		t.Fatalf("%q", got)
	}
}

// Audit 20/11: listening asked IsMember for every stranger in a group,
// holding up every chat. It hears only who is known, and a member by
// IsMember once answered.
func TestListeningAsksNoOne(t *testing.T) {
	h := &heard{}
	var asked atomic.Int32
	listened := ChatKey("test", office)
	b, app, _, _ := teamBridge(t, Config{IsMember: func(context.Context, string) (bool, error) { asked.Add(1); return true, nil },
		MemberProfile: "team", Listens: func(chat string) bool { return chat == listened }, Hear: h.hear})
	ctx := context.Background()
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Text: "unaddressed"})
	if asked.Load() != 0 || len(h.all()) != 0 {
		t.Fatalf("asked %d, heard %q", asked.Load(), h.all())
	}
	say(b, worker, worker, "hi") // answered: now known as a member
	app.nextIn(t, worker)
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Text: "the server is down"})
	if got := h.all(); len(got) != 1 {
		t.Fatalf("%q", got)
	}
}
