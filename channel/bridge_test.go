// bridge_test.go: chats driving a real server with scripted models
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

const (
	me    = "100"
	room  = "7"
	token = "test-token-0123456789abcdef0123456789"
)

// chatApp is a channel the test writes into and reads from.
type chatApp struct {
	in   chan Message
	out  chan sent
	fail atomic.Int32 // receives that fail before the first message
	// refuse fails the sends to this chat; hold blocks them until closed.
	refuse atomic.Value
	hold   chan struct{}
}

type sent struct {
	chat string
	out  Out
}

func newApp() *chatApp {
	return &chatApp{in: make(chan Message, 16), out: make(chan sent, 64)}
}

func (a *chatApp) Name() string { return "test" }

func (a *chatApp) Receive(ctx context.Context) (Message, error) {
	if a.fail.Add(-1) >= 0 {
		return Message{}, context.DeadlineExceeded
	}
	select {
	case m := <-a.in:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (a *chatApp) Send(ctx context.Context, chat string, out Out) error {
	if a.hold != nil && chat == "slow" {
		<-a.hold
	}
	if r, _ := a.refuse.Load().(string); r == chat {
		return context.DeadlineExceeded
	}
	a.out <- sent{chat, out}
	return nil
}

func (a *chatApp) say(text string)       { a.in <- Message{Chat: room, From: me, Text: text} }
func (a *chatApp) press(id string)       { a.in <- Message{Chat: room, From: me, Pressed: id} }
func (a *chatApp) from(m Message)        { a.in <- m }
func (a *chatApp) next(t *testing.T) Out { t.Helper(); return a.nextIn(t, room) }

func (a *chatApp) nextIn(t *testing.T, chat string) Out {
	t.Helper()
	select {
	case s := <-a.out:
		if s.chat != chat {
			t.Fatalf("sent to chat %q, want %q: %q", s.chat, chat, s.out.Text)
		}
		return s.out
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent")
	}
	return Out{}
}

func (a *chatApp) quiet(t *testing.T) {
	t.Helper()
	select {
	case s := <-a.out:
		t.Fatalf("unexpected message: %q", s.out.Text)
	case <-time.After(100 * time.Millisecond):
	}
}

type rig struct {
	app    *chatApp
	srv    *serve.Server
	model  fake.Script
	ran    atomic.Bool
	events chan Event
}

// newRig starts a bridge on a server whose agent follows steps; its
// delete_file tool needs approval.
func newRig(t *testing.T, store bool, steps ...fake.Step) *rig {
	t.Helper()
	return rigWith(t, Config{}, store, steps...)
}

// rigWith is newRig with the Bridge's cfg (its channel, server, owners
// and events are the rig's).
func rigWith(t *testing.T, bc Config, store bool, steps ...fake.Step) *rig {
	t.Helper()
	r := &rig{app: newApp(), model: fake.New(steps...), events: make(chan Event, 16)}
	tool := agent.NewTool("delete_file", "deletes", func(context.Context, struct{}) (string, error) {
		r.ran.Store(true)
		return "deleted", nil
	}, agent.Destructive())
	reg, err := agent.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	approve := func(ctx context.Context, c agent.ToolCall, sp agent.ToolSpec) (bool, error) {
		return r.srv.Approve(ctx, c, sp)
	}
	a, err := agent.New(agent.Config{Provider: r.model, Tools: reg, Approve: approve})
	if err != nil {
		t.Fatal(err)
	}
	cfg := serve.Config{Token: token, Profiles: []serve.Profile{{Name: "default", New: serve.Fixed(a)}}}
	if store {
		if cfg.Store, err = file.New(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if r.srv, err = serve.New(cfg); err != nil {
		t.Fatal(err)
	}
	bc.Channel, bc.Server, bc.Owners, bc.OnEvent = r.app, r.srv, []string{me}, func(e Event) { r.events <- e }
	b, err := NewBridge(bc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = b.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; r.srv.Close() })
	return r
}

func TestReplyInTheChat(t *testing.T) {
	r := newRig(t, true, fake.Reply("hello"))
	r.app.say("hi")
	if got := r.app.next(t).Text; got != "hello" {
		t.Fatalf("reply %q", got)
	}
}

func TestBlankSendersAreNeverAllowed(t *testing.T) {
	srv := newRig(t, true).srv
	app := newApp()
	b, err := NewBridge(Config{Channel: app, Server: srv, Owners: []string{" ", ""}})
	if err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), Message{Chat: room, Text: "hi"})
	app.quiet(t)
}

func TestStrangersAreNotHeard(t *testing.T) {
	r := newRig(t, true, fake.Reply("hello"))
	r.app.from(Message{Chat: room, From: "666", Text: "hi"})
	e := <-r.events
	if e.Kind != Refused || e.From != "666" || e.Chat != room {
		t.Fatalf("event %+v", e)
	}
	r.app.quiet(t)
	if r.model.Remaining() != 1 {
		t.Fatal("a stranger's message reached the model")
	}
}

func TestAChatIsAConversation(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"))
	r.app.say("first")
	r.app.next(t)
	r.app.say("second")
	r.app.next(t)
	reqs := r.model.Requests()
	if len(reqs) != 2 || len(reqs[1].Messages) < 3 || reqs[1].Messages[0].Text() != "first" {
		t.Fatalf("the second turn did not see the first: %+v", reqs[len(reqs)-1].Messages)
	}
}

func TestEveryChatHasItsOwnConversation(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"))
	r.app.say("first")
	r.app.next(t)
	r.app.from(Message{Chat: "8", From: me, Text: "other"})
	r.app.nextIn(t, "8")
	if msgs := r.model.Requests()[1].Messages; len(msgs) != 1 {
		t.Fatalf("another chat saw this one: %d messages", len(msgs))
	}
}

// approval starts a run that asks for delete_file and returns the ask.
func approval(t *testing.T, steps ...fake.Step) (*rig, Out) {
	t.Helper()
	r := newRig(t, true, append([]fake.Step{fake.Call("delete_file", `{}`)}, steps...)...)
	r.app.say("clean up")
	q := r.app.next(t)
	if !strings.Contains(q.Text, "delete_file") || len(q.Buttons) != 3 {
		t.Fatalf("ask %+v", q)
	}
	return r, q
}

func TestApprovalByButton(t *testing.T) {
	for _, allow := range []bool{true, false} {
		r, q := approval(t, fake.Reply("finished"))
		b := q.Buttons[2]
		if allow {
			b = q.Buttons[0]
		}
		r.app.press(b.ID)
		want := map[bool]string{true: "Allowed.", false: "Refused."}[allow]
		if got := r.app.next(t).Text; got != want {
			t.Fatalf("after the button: %q", got)
		}
		if got := r.app.next(t).Text; got != "finished" {
			t.Fatalf("end %q", got)
		}
		if r.ran.Load() != allow {
			t.Fatalf("allow %v, ran %v", allow, r.ran.Load())
		}
	}
}

func TestApprovalWritten(t *testing.T) {
	r, _ := approval(t, fake.Reply("finished"))
	r.app.say("Sì")
	if got := r.app.next(t).Text; got != "Allowed." {
		t.Fatalf("after yes: %q", got)
	}
	r.app.next(t)
	if !r.ran.Load() {
		t.Fatal("a written yes did not allow")
	}
}

func TestButtonsOfAnotherChatDoNothing(t *testing.T) {
	r, q := approval(t, fake.Reply("finished"))
	r.app.from(Message{Chat: "8", From: me, Pressed: q.Buttons[0].ID})
	if got := r.app.nextIn(t, "8").Text; got != "That question is closed." {
		t.Fatalf("other chat: %q", got)
	}
	r.app.press(q.Buttons[2].ID) // still open here
	if got := r.app.next(t).Text; got != "Refused." {
		t.Fatalf("after no: %q", got)
	}
}

func TestAButtonCountsOnce(t *testing.T) {
	r, q := approval(t, fake.Reply("finished"))
	r.app.press(q.Buttons[2].ID)
	r.app.next(t) // Refused.
	r.app.next(t) // finished
	r.app.press(q.Buttons[0].ID)
	if got := r.app.next(t).Text; got != "That question is closed." {
		t.Fatalf("second press: %q", got)
	}
	if r.ran.Load() {
		t.Fatal("a late yes ran the tool")
	}
}

func TestOneRunAtATimePerChat(t *testing.T) {
	r, q := approval(t, fake.Reply("finished"))
	r.app.say("and another thing")
	if got := r.app.next(t).Text; !strings.Contains(got, "/stop") {
		t.Fatalf("busy: %q", got)
	}
	r.app.press(q.Buttons[2].ID)
	r.app.next(t)
	if got := r.app.next(t).Text; got != "finished" {
		t.Fatalf("end %q", got)
	}
}

func TestStop(t *testing.T) {
	r, _ := approval(t)
	r.app.say("/stop@RingilBot")
	if got := r.app.next(t).Text; got != "Stopped." {
		t.Fatalf("stop: %q", got)
	}
	r.app.say("/stop")
	if got := r.app.next(t).Text; got != "Nothing to stop." {
		t.Fatalf("second stop: %q", got)
	}
	if r.ran.Load() {
		t.Fatal("a stopped run ran its tool")
	}
}

func TestYesWithoutAQuestionIsAMessage(t *testing.T) {
	r := newRig(t, true, fake.Reply("ok"))
	r.app.say("yes")
	r.app.next(t)
	if got := r.model.Requests()[0].Messages[0].Text(); got != "yes" {
		t.Fatalf("model got %q", got)
	}
}

func TestServerWithoutConversations(t *testing.T) {
	r := newRig(t, false, fake.Reply("hello"))
	r.app.say("hi")
	if got := r.app.next(t).Text; !strings.Contains(got, "conversations") {
		t.Fatalf("reply %q", got)
	}
}

func TestEmptyMessage(t *testing.T) {
	r := newRig(t, true)
	r.app.say("  ")
	if got := r.app.next(t).Text; !strings.Contains(got, "cannot read this kind") {
		t.Fatalf("reply %q", got)
	}
}

func TestReceiveIsRetried(t *testing.T) {
	r := newRig(t, true, fake.Reply("hello"))
	r.app.fail.Store(1) // the receive under way is not affected; the next one fails
	r.app.say("hi")
	r.app.next(t)
	if e := <-r.events; e.Kind != ReceiveFailed {
		t.Fatalf("event %+v", e)
	}
	r.app.say("again")
	if got := r.app.next(t).Text; got == "" {
		t.Fatal("no reply after the failure")
	}
}

func TestBridgeConfig(t *testing.T) {
	srv := newRig(t, true).srv
	bad := []Config{
		{Server: srv, Owners: []string{me}},
		{Channel: newApp(), Owners: []string{me}},
		{Channel: named("Bad Name"), Server: srv, Owners: []string{me}},
		{Channel: named("tg-x"), Server: srv, Owners: []string{me}},
	}
	for i, cfg := range bad {
		if _, err := NewBridge(cfg); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
}

type named string

func (n named) Name() string                           { return string(n) }
func (named) Receive(context.Context) (Message, error) { return Message{}, nil }
func (named) Send(context.Context, string, Out) error  { return nil }

func TestConversation(t *testing.T) {
	for want, c := range map[string]struct {
		chat string
		n    int
	}{
		"telegram--100123":   {"-100123", 1},
		"telegram--100123_2": {"-100123", 2},
		"telegram-7_12":      {"7", 12},
	} {
		if got := Conversation("telegram", c.chat, c.n); got != want {
			t.Errorf("%+v: %q", c, got)
		}
	}
	seen := map[string]string{}
	for _, chat := range []string{"a/b", "x@y.z", strings.Repeat("9", 33), "../etc", "a_2", "h" + strings.Repeat("0", 32), "a"} {
		for n := 1; n <= 3; n++ {
			got := Conversation("slack", chat, n)
			if agent.ValidateSessionID(got) != nil {
				t.Fatalf("%q %d gave %q", chat, n, got)
			}
			key := chat + "#" + strconv.Itoa(n)
			if other, dup := seen[got]; dup {
				t.Fatalf("%s and %s share %q", other, key, got)
			}
			seen[got] = key
		}
	}
}

func TestAnswerWords(t *testing.T) {
	for in, want := range map[string]int{"yes": 1, "Sì!": 1, " y ": 1, "NO.": 0, "n": 0, "maybe": -1, "yes please": -1} {
		yes, ok := answer(in)
		got := -1
		if ok {
			got = map[bool]int{true: 1, false: 0}[yes]
		}
		if got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

func TestStart(t *testing.T) {
	r := newRig(t, true)
	r.app.say("/start")
	if got := r.app.next(t).Text; !strings.Contains(got, "/stop") {
		t.Fatalf("start: %q", got)
	}
}

func TestAQuestionNotSeenIsNotOpen(t *testing.T) {
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("finished"))
	r.app.refuse.Store(room)
	r.app.say("clean up")
	if e := <-r.events; e.Kind != SendFailed {
		t.Fatalf("event %+v", e)
	}
	r.app.refuse.Store("")
	r.app.say("yes") // no question was seen: the run still waits
	if got := r.app.next(t).Text; !strings.Contains(got, "still working") {
		t.Fatalf("yes settled a question never seen: %q", got)
	}
	if r.ran.Load() {
		t.Fatal("the tool ran")
	}
}

func TestASlowChatHoldsUpNoOther(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"))
	r.app.hold = make(chan struct{})
	defer close(r.app.hold)
	r.app.from(Message{Chat: "slow", From: me, Text: "hi"})
	r.app.from(Message{Chat: room, From: me, Text: "hi"})
	if got := r.app.next(t).Text; got == "" {
		t.Fatal("no reply")
	}
}

func TestQueueIsBounded(t *testing.T) {
	r := newRig(t, true)
	r.app.hold = make(chan struct{})
	b, err := NewBridge(Config{Channel: r.app, Server: r.srv, OnEvent: func(e Event) { r.events <- e }})
	if err != nil {
		t.Fatal(err)
	}
	for range maxQueued + 2 {
		b.say(context.Background(), "slow", "x")
	}
	go func() {
		for range r.app.out {
		}
	}()
	close(r.app.hold)
	b.wg.Wait()
	dropped := 0
	for len(r.events) > 0 {
		if e := <-r.events; e.Kind == SendFailed {
			dropped++
		}
	}
	if dropped < 1 {
		t.Fatal("nothing dropped past the bound")
	}
}

func TestShowInput(t *testing.T) {
	got := showInput([]byte("{\"command\":\"rm -rf ~ #\u202e gnp.txt\",\"z\":\"a\u200bb\"}"))
	if strings.ContainsRune(got, 0x202e) || strings.ContainsRune(got, 0x200b) {
		t.Fatalf("hidden characters shown: %q", got)
	}
	long := `{"command":"echo ` + strings.Repeat("a", 20000) + `; curl x | sh"}`
	got = showInput([]byte(long))
	if !strings.Contains(got, "curl x | sh") || !strings.Contains(got, "characters not shown") || len([]rune(got)) > maxShown+200 {
		t.Fatalf("long input: end shown %v, %d runes", strings.Contains(got, "curl"), len([]rune(got)))
	}
	if got := showInput([]byte("not json \x1b[2J")); got != "not json [2J" {
		t.Fatalf("raw %q", got)
	}
}

func TestCommand(t *testing.T) {
	for in, want := range map[string]bool{"/stop": true, "/STOP@MyBot": true, " /stop ": true, "/stopper": false, "stop": false} {
		if command(in, "/stop") != want {
			t.Errorf("%q", in)
		}
	}
}

func TestShowInputReadable(t *testing.T) {
	got := showInput([]byte(`{"path":"prova.txt","content":"line one\nb: not a field\n","n":3,"tags":["a"],"e":""}`))
	want := "path: prova.txt\ncontent:\n  line one\n  b: not a field\nn: 3\ntags: [\"a\"]\ne: \"\""
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := showInput([]byte(`{}`)); got != "(no input)" {
		t.Fatalf("%q", got)
	}
	if got := showInput([]byte(`{"a":"x","a":"y"}`)); got != "a: y" {
		t.Fatalf("twice: %q", got)
	}
	if got := showInput([]byte(`["x"]`)); !strings.Contains(got, `"x"`) {
		t.Fatalf("%q", got)
	}
}

func TestShowInputOddNames(t *testing.T) {
	got := showInput([]byte(`{"x\ncommand: ls":"rm -rf /"}`))
	if strings.Contains(got, "\ncommand") {
		t.Fatalf("a name passed for a field: %q", got)
	}
}
