// automate_test.go: /automate
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

func TestAutomation(t *testing.T) {
	for in, want := range map[string]string{
		"/automate every day at 8 brief me":  "every day at 8 brief me",
		"/automate@MyBot\nwhen a file comes": "when a file comes",
		"/AUTOMATE":                          "",
	} {
		if got, ok := automation(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"/automated", "automate this", "please /automate x"} {
		if _, ok := automation(in); ok {
			t.Errorf("%q read as the command", in)
		}
	}
}

func TestAutomateRunsAreLabelled(t *testing.T) {
	sources := make(chan string, 2)
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Reply("made"), fake.Reply("ok"))})
	if err != nil {
		t.Fatal(err)
	}
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := serve.New(serve.Config{Token: token, Store: store, Profiles: []serve.Profile{{Name: "default", New: func(ctx context.Context) (serve.Instance, error) {
		sources <- serve.SourceOf(ctx)
		return serve.Instance{Agent: a}, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	app := newApp()
	b, err := NewBridge(Config{Channel: app, Server: srv, Owners: []string{me}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	say(b, me, me, "/automate every day at 8 brief me")
	if s := <-sources; s != Automate {
		t.Fatalf("source %q", s)
	}
	app.nextIn(t, me)
	say(b, me, me, "check the download every 5 minutes")
	if s := <-sources; s != "" {
		t.Fatalf("a plain message labelled %q", s)
	}
}

func TestOnlyAnOwnerAutomatesInTheirChat(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team"})
	say(b, worker, worker, "/automate every day")
	if got := app.nextIn(t, worker).Text; !strings.Contains(got, "Only an owner") {
		t.Fatalf("%q", got)
	}
	b.handle(context.Background(), Message{Chat: office, From: me, Group: true, Addressed: true, Text: "/automate every day"})
	if got := app.nextIn(t, office).Text; !strings.Contains(got, "Only an owner") {
		t.Fatalf("in a group: %q", got)
	}
	say(b, me, me, "/automate")
	if got := app.nextIn(t, me).Text; !strings.Contains(got, "Say what to automate") {
		t.Fatalf("%q", got)
	}
}
