// roles_test.go: owners, members, the public, groups
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

const (
	worker   = "200"
	outsider = "300"
	office   = "-50" // a group chat
)

// profiled is a server whose profiles answer with their own name, or,
// when the message says "delete", ask to delete a file first.
type profiled struct {
	srv *serve.Server
	ran atomic.Bool
}

// echo is a provider that answers with name, or calls delete_file first.
type echo struct{ name string }

func (e echo) Info() agent.ModelInfo { return agent.ModelInfo{Name: e.name} }

func (e echo) Generate(_ context.Context, req *agent.Request, _ agent.TextSink) (*agent.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == agent.RoleUser && strings.Contains(last.Text(), "delete") {
		return &agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "delete_file", Input: []byte(`{}`)}}}, Stop: agent.StopToolUse}, nil
	}
	return &agent.Response{Message: agent.AssistantText(e.name), Stop: agent.StopEnd}, nil
}

func newProfiled(t *testing.T) *profiled {
	t.Helper()
	p := &profiled{}
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var profiles []serve.Profile
	for _, name := range []string{"default", "team", "public"} {
		tool := agent.NewTool("delete_file", "deletes", func(context.Context, struct{}) (string, error) {
			p.ran.Store(true)
			return "deleted", nil
		}, agent.Destructive())
		reg, err := agent.NewRegistry(tool)
		if err != nil {
			t.Fatal(err)
		}
		a, err := agent.New(agent.Config{Provider: echo{name}, Tools: reg, Approve: func(ctx context.Context, c agent.ToolCall, sp agent.ToolSpec) (agent.Decision, error) {
			return p.srv.Approve(ctx, c, sp)
		}})
		if err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, serve.Profile{Name: name, New: serve.Fixed(a)})
	}
	if p.srv, err = serve.New(serve.Config{Token: token, Profiles: profiles, Store: store}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.srv.Close)
	return p
}

func teamBridge(t *testing.T, cfg Config) (*Bridge, *chatApp, *profiled, chan Event) {
	t.Helper()
	p, app, events := newProfiled(t), newApp(), make(chan Event, 16)
	cfg.Channel, cfg.Server = app, p.srv
	cfg.Owners = []string{me}
	cfg.OnEvent = func(e Event) { events <- e }
	b, err := NewBridge(ephemeral(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	return b, app, p, events
}

func say(b *Bridge, chat, from, text string) {
	b.handle(context.Background(), Message{Chat: chat, From: from, Text: text})
}

func TestEachRoleItsProfile(t *testing.T) {
	b, app, _, events := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team"})
	say(b, me, me, "hi")
	if got := app.nextIn(t, me).Text; got != "default" {
		t.Fatalf("owner: %q", got)
	}
	say(b, worker, worker, "hi")
	if got := app.nextIn(t, worker).Text; got != "team" {
		t.Fatalf("member: %q", got)
	}
	say(b, outsider, outsider, "hi")
	if e := <-events; e.Kind != Refused || e.From != outsider {
		t.Fatalf("outsider: %+v", e)
	}
	app.quiet(t)
}

func TestPublicBot(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{PublicProfile: "public"})
	say(b, outsider, outsider, "hi")
	if got := app.nextIn(t, outsider).Text; got != "public" {
		t.Fatalf("public: %q", got)
	}
}

func TestMembersNeedTheirProfile(t *testing.T) {
	p := newProfiled(t)
	for _, cfg := range []Config{
		{Channel: newApp(), Server: p.srv, Members: []string{worker}},
		{Channel: newApp(), Server: p.srv, IsMember: func(context.Context, string) (bool, error) { return true, nil }},
	} {
		if _, err := NewBridge(ephemeral(cfg)); err == nil {
			t.Error("members without a profile")
		}
	}
}

func TestIsMemberIsAskedAndKept(t *testing.T) {
	var asked atomic.Int32
	fail := atomic.Bool{}
	b, app, _, events := teamBridge(t, Config{MemberProfile: "team", IsMember: func(_ context.Context, from string) (bool, error) {
		asked.Add(1)
		if fail.Load() {
			return false, errors.New("telegram down")
		}
		return from == worker, nil
	}})
	clock := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	say(b, worker, worker, "hi")
	app.nextIn(t, worker)
	say(b, worker, worker, "again")
	app.nextIn(t, worker)
	if asked.Load() != 1 {
		t.Fatalf("asked %d times", asked.Load())
	}
	clock = clock.Add(11 * time.Minute)
	fail.Store(true)
	say(b, worker, worker, "later")
	if e := <-events; e.Kind != MemberCheckFailed {
		t.Fatalf("event %+v", e)
	}
	app.quiet(t)
}

func TestGroups(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team"})
	ctx := context.Background()
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Text: "lunch?"})
	app.quiet(t)
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Addressed: true, Text: "hi bot"})
	if got := app.nextIn(t, office).Text; got != "team" {
		t.Fatalf("member in a group: %q", got)
	}
	b.handle(ctx, Message{Chat: office, From: me, Group: true, Addressed: true, Text: "hi bot"})
	if got := app.nextIn(t, office).Text; got != "team" {
		t.Fatalf("an owner in a group talks with the members' profile: %q", got)
	}
}

func TestOnlyOwnersApprove(t *testing.T) {
	b, app, p, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team"})
	say(b, worker, worker, "delete it")
	if got := app.nextIn(t, worker).Text; !strings.Contains(got, "only an owner") {
		t.Fatalf("member's own chat: %q", got)
	}
	app.nextIn(t, worker) // the run's answer
	if p.ran.Load() {
		t.Fatal("the tool ran without an owner")
	}
	ctx := context.Background()
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Addressed: true, Text: "delete it"})
	q := app.nextIn(t, office)
	if !strings.Contains(q.Text, "an owner answers") {
		t.Fatalf("group ask: %q", q.Text)
	}
	b.handle(ctx, Message{Chat: office, From: worker, Group: true, Pressed: q.Buttons[0].ID})
	if got := app.nextIn(t, office).Text; !strings.Contains(got, "Only an owner") {
		t.Fatalf("member pressed: %q", got)
	}
	b.handle(ctx, Message{Chat: office, From: me, Group: true, Pressed: q.Buttons[0].ID})
	if got := app.nextIn(t, office).Text; got != "Allowed." {
		t.Fatalf("owner pressed: %q", got)
	}
	app.nextIn(t, office)
	if !p.ran.Load() {
		t.Fatal("the owner's yes did not run the tool")
	}
}

func TestMessagesPerHour(t *testing.T) {
	b, app, _, events := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team", MaxPerHour: 2})
	clock := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	for range 2 {
		say(b, worker, worker, "hi")
		app.nextIn(t, worker)
	}
	say(b, worker, worker, "hi")
	if got := app.nextIn(t, worker).Text; !strings.Contains(got, "many messages") {
		t.Fatalf("third: %q", got)
	}
	if e := <-events; e.Kind != Limited {
		t.Fatalf("event %+v", e)
	}
	for range 3 {
		say(b, me, me, "hi")
		app.nextIn(t, me)
	}
	clock = clock.Add(time.Hour)
	say(b, worker, worker, "hi")
	if got := app.nextIn(t, worker).Text; got != "team" {
		t.Fatalf("an hour later: %q", got)
	}
}

func TestAGroupTalksWithTheLeastTrusted(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team", PublicProfile: "public"})
	b.handle(context.Background(), Message{Chat: office, From: me, Group: true, Addressed: true, Text: "hi"})
	if got := app.nextIn(t, office).Text; got != "public" {
		t.Fatalf("owner in a group with a public bot: %q", got)
	}
	// With no members' or public profile (25/11, Slack: a channel worked
	// only after setting one), the owners talk in a group as in their own
	// chats, and no one else is heard.
	owners, oapp, _, _ := teamBridge(t, Config{})
	owners.handle(context.Background(), Message{Chat: office, From: me, Group: true, Addressed: true, Text: "hi"})
	if got := oapp.nextIn(t, office).Text; got != "default" {
		t.Fatalf("owners only: %q", got)
	}
	owners.handle(context.Background(), Message{Chat: office, From: outsider, Group: true, Addressed: true, Text: "hi"})
	oapp.quiet(t) // someone else is not answered
}

func TestGroupCommands(t *testing.T) {
	b, app, _, _ := teamBridge(t, Config{Members: []string{worker}, MemberProfile: "team", PublicProfile: "public", MaxPerHour: 2})
	ctx := context.Background()
	group := func(from, text string) string {
		b.handle(ctx, Message{Chat: office, From: from, Group: true, Addressed: true, Text: text})
		return app.nextIn(t, office).Text
	}
	if got := group(outsider, "/new"); !strings.Contains(got, "Only an owner") {
		t.Fatalf("public /new: %q", got)
	}
	if got := group(worker, "/new"); !strings.Contains(got, "New conversation") {
		t.Fatalf("member /new: %q", got)
	}
	q := group(me, "delete it") // an owner's run, waiting
	if got := group(worker, "/stop"); !strings.Contains(got, "Only an owner") {
		t.Fatalf("member stopping an owner's run: %q", got)
	}
	if got := group(me, "/stop"); got != "Stopped." {
		t.Fatalf("owner /stop: %q (asked %q)", got, q)
	}
	if got := group(outsider, "/voice"); got == "" {
		t.Fatal("no answer")
	}
	if got := group(outsider, "/text"); !strings.Contains(got, "many messages") {
		t.Fatalf("commands past the bound: %q", got)
	}
}

// The host can say who everyone is (its directory, an SSO) in place of
// the lists (21/11: the people model was the Bridge's own). Its roles get
// their profiles; an error is a stranger.
func TestAuthorize(t *testing.T) {
	p, app, events := newProfiled(t), newApp(), make(chan Event, 16)
	asked := 0
	cfg := Config{Channel: app, Server: p.srv, MemberProfile: "team", OnEvent: func(e Event) { events <- e },
		Authorize: func(_ context.Context, from string) (Role, error) {
			asked++
			switch from {
			case me:
				return Owner, nil
			case worker:
				return Member, nil
			}
			return Stranger, errors.New("directory down")
		}}
	b, err := NewBridge(ephemeral(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	eachHostRole(t, b, app)
	if asked != 2 {
		t.Fatalf("asked %d times: the answers are not kept", asked)
	}
	say(b, outsider, outsider, "hi")
	for e := range events {
		if e.Kind == Refused {
			break
		}
	}
	if _, err := NewBridge(Config{Ephemeral: true, Channel: app, Server: p.srv, Owners: []string{me}, Authorize: cfg.Authorize}); err == nil {
		t.Fatal("Authorize and Owners together")
	}
}

// eachHostRole: the owner and the member Authorize names talk with their
// profiles; the member's second message asks no one again.
func eachHostRole(t *testing.T, b *Bridge, app *chatApp) {
	t.Helper()
	say(b, me, me, "hi")
	if got := app.nextIn(t, me).Text; got != "default" {
		t.Fatalf("owner: %q", got)
	}
	say(b, worker, worker, "hi")
	if got := app.nextIn(t, worker).Text; got != "team" {
		t.Fatalf("member: %q", got)
	}
	say(b, worker, worker, "again")
	app.nextIn(t, worker)
}
