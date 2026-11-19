// typing_test.go: typing while working, not while waiting for the user
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type typingApp struct {
	*chatApp
	typed atomic.Int32
}

func (a *typingApp) Typing(context.Context, string) error {
	a.typed.Add(1)
	return nil
}

func TestTypingWhileWorkingOnly(t *testing.T) {
	old := typingEvery
	typingEvery = 5 * time.Millisecond
	t.Cleanup(func() { typingEvery = old })
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("finished"))
	app := &typingApp{chatApp: newApp()} // not the rig's: its bridge reads that one
	b, err := NewBridge(Config{Channel: app, Server: r.srv, Owners: []string{me}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = b.Run(ctx) }()
	defer func() { cancel(); <-done }()
	app.in <- Message{Chat: room, From: me, Text: "clean up"}
	q := app.next(t) // the question: the run waits
	for deadline := time.Now().Add(time.Second); app.typed.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never typing")
		}
	}
	time.Sleep(20 * time.Millisecond) // a tick in flight may still land
	before := app.typed.Load()
	time.Sleep(50 * time.Millisecond)
	if app.typed.Load() != before {
		t.Fatal("typing while waiting for the user")
	}
	app.in <- Message{Chat: room, From: me, Pressed: q.Buttons[1].ID}
	app.next(t)
	app.next(t)
	end := app.typed.Load()
	time.Sleep(50 * time.Millisecond)
	if app.typed.Load() != end {
		t.Fatal("typing after the run ended")
	}
}
