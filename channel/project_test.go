// project_test.go: /project, an owner makes a project from a chat
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

// /project starts a run labelled Project, with what was said; a plain
// message about a project is not one.
func TestProjectRunsAreLabelled(t *testing.T) {
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
	b, err := NewBridge(Config{Ephemeral: true, Channel: app, Server: srv, Owners: []string{me}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	say(b, me, me, "/project the Q1 plan")
	if s := <-sources; s != Project {
		t.Fatalf("source %q", s)
	}
	app.nextIn(t, me)
	say(b, me, me, "this is a big project")
	if s := <-sources; s != "" {
		t.Fatalf("a plain message labelled %q", s)
	}
}

// Only an owner, in their own chat, makes a project; /project alone says
// how.
func TestOnlyAnOwnerMakesProjects(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team"})
	say(b, worker, worker, "/project x")
	if got := app.nextIn(t, worker).Text; !strings.Contains(got, "Only an owner") {
		t.Fatalf("%q", got)
	}
}
