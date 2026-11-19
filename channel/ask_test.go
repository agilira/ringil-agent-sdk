// ask_test.go: several questions open at once
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"testing"

	"github.com/agilira/ringil-agent-sdk/serve"
)

func TestSeveralOpenWantTheButtons(t *testing.T) {
	r := newRig(t, true)
	b, err := NewBridge(Config{Channel: r.app, Server: r.srv, Allow: []string{me}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b.chats[room] = &chat{run: "r1"}
	b.ask(ctx, room, "r1", serve.Approval{ID: "a1", Tool: "read"})
	b.ask(ctx, room, "r1", serve.Approval{ID: "a2", Tool: "rm", Worker: "cleaner 1"})
	r.app.next(t)
	if q := r.app.next(t).Text; q[:16] != "Worker cleaner 1" {
		t.Fatalf("asker: %q", q)
	}
	b.handle(ctx, Message{Chat: room, From: me, Text: "no"})
	if got := r.app.next(t).Text; got != "Several questions are open: answer each with its buttons." {
		t.Fatalf("written answer with two open: %q", got)
	}
	if n := len(b.chats[room].open); n != 2 {
		t.Fatalf("%d open", n)
	}
}
