// kinds_test.go: tools, files, messages and webhooks
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// answers is a tool whose answer the test sets.
type answers struct {
	mu   sync.Mutex
	text string
	args []string
}

func (a *answers) set(s string) { a.mu.Lock(); a.text = s; a.mu.Unlock() }

func (a *answers) call(_ context.Context, tool string, args json.RawMessage) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.args = append(a.args, tool+" "+string(args))
	return a.text, nil
}

func (r *recorder) lastFired() Fired {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fired[len(r.fired)-1]
}

func TestToolFiresWhenItsAnswerChanges(t *testing.T) {
	tr := Trigger{Name: "inbox", Kind: Tool, Tool: "mail_unread", Args: json.RawMessage(`{"box":"in"}`), When: "@every 5m", Skill: "triage"}
	store, clock, rec := harness(t, tr)
	tool := &answers{text: "3 unread"}
	launchWith(t, store, clock, rec, tool.call)
	step := func() { clock.advance(5 * time.Minute); clock.settle(t) }
	step() // first look: only takes note
	time.Sleep(20 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatal("the first look fired")
	}
	step() // the same
	time.Sleep(20 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatal("fired without a change")
	}
	tool.set("4 unread")
	step()
	eventually(t, "no run on a change", func() bool { return rec.count() == 1 })
	if f := rec.lastFired(); f.Kind != Tool || f.Detail != "4 unread" {
		t.Fatalf("%+v", f)
	}
	if tool.args[0] != `mail_unread {"box":"in"}` {
		t.Fatalf("%v", tool.args)
	}
}

func TestWhatWasSeenOutlivesARestart(t *testing.T) {
	tr := Trigger{Name: "inbox", Kind: Tool, Tool: "mail_unread", When: "@every 5m", Prompt: "triage"}
	store, clock, rec := harness(t, tr)
	tool := &answers{text: "3 unread"}
	func() {
		s, _ := New(Config{Store: store, Run: rec.run, Call: tool.call, Clock: clock, Poll: time.Hour})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		clock.settle(t)
		clock.advance(5 * time.Minute)
		clock.settle(t)
		eventually(t, "no look", func() bool { st, _ := store.Status(); return st["inbox"].Seen != "" })
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	launchWith(t, store, clock, rec, tool.call) // the same answer after the restart: no run
	clock.advance(5 * time.Minute)
	clock.settle(t)
	time.Sleep(20 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatal("fired after a restart without a change")
	}
}

func TestFileFiresOnCreatedChangedRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, clock, rec := harness(t, Trigger{Name: "drop", Kind: File, Path: dir, When: "@every 1m", Skill: "file-in"})
	launch(t, store, clock, rec)
	step := func() { clock.advance(time.Minute); clock.settle(t) }
	step() // takes note
	eventually(t, "no look", func() bool { st, _ := store.Status(); return st["drop"].Seen != "" })
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	step()
	eventually(t, "no run", func() bool { return rec.count() == 1 })
	if d := rec.lastFired().Detail; d != "Watched: "+dir+"\ncreated: b.txt\nremoved: a.txt" {
		t.Fatalf("%q", d)
	}
}

func TestFireAndHeard(t *testing.T) {
	store, clock, rec := harness(t,
		Trigger{Name: "urgent", Kind: Message, Chat: "telegram:-50", Match: "URGENTE", Prompt: "tell me"},
		Trigger{Name: "hook", Kind: Webhook, Secret: strings.Repeat("s", 32), Skill: "deploy"},
		trigger("daily", "@daily"))
	s := launch(t, store, clock, rec)
	if got := s.Heard("telegram:-50", "è urgente!"); len(got) != 1 || got[0] != "urgent" {
		t.Fatalf("%v", got)
	}
	if got := s.Heard("telegram:-50", "tutto bene"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := s.Heard("telegram:-51", "urgente"); len(got) != 0 {
		t.Fatalf("another chat: %v", got)
	}
	if err := s.Fire("hook", `{"ref":"main"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.Fire("daily", "x"); err == nil {
		t.Fatal("a time trigger set off by the host")
	}
	if err := s.Fire("nope", "x"); err == nil {
		t.Fatal("no such trigger")
	}
	eventually(t, "runs", func() bool { return rec.count() == 2 })
}

func TestFireWhenStopped(t *testing.T) {
	store, _, rec := harness(t, Trigger{Name: "hook", Kind: Webhook, Secret: strings.Repeat("s", 32), Skill: "deploy"})
	s, err := New(Config{Store: store, Run: rec.run})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Fire("hook", "x"); err == nil {
		t.Fatal("fired while not running")
	}
}

func TestCheckKinds(t *testing.T) {
	ok := []Trigger{
		{Name: "a", When: "@daily", Prompt: "x"},
		{Name: "b", Kind: Tool, Tool: "mail_unread", Skill: "triage"},
		{Name: "c", Kind: File, Path: filepath.Join(os.TempDir(), "in"), Skill: "file-in"},
		{Name: "d", Kind: Message, Chat: "telegram:1", Prompt: "x"},
		{Name: "e", Kind: Webhook, Secret: strings.Repeat("k", 32), Prompt: "x", Allow: []string{"write_file"}},
	}
	for _, tr := range ok {
		if _, err := tr.Check(); err != nil {
			t.Errorf("%s: %v", tr.Name, err)
		}
	}
	bad := map[string]Trigger{
		"no run":       {Name: "a", When: "@daily"},
		"skill name":   {Name: "a", When: "@daily", Skill: "Triage Mail"},
		"no when":      {Name: "a", Prompt: "x"},
		"no tool":      {Name: "a", Kind: Tool, Prompt: "x"},
		"bad args":     {Name: "a", Kind: Tool, Tool: "t", Args: json.RawMessage(`{`), Prompt: "x"},
		"relative":     {Name: "a", Kind: File, Path: "in", Prompt: "x"},
		"no chat":      {Name: "a", Kind: Message, Prompt: "x"},
		"short secret": {Name: "a", Kind: Webhook, Secret: "123", Prompt: "x"},
		"kind":         {Name: "a", Kind: "email", Prompt: "x"},
		"allow":        {Name: "a", When: "@daily", Prompt: "x", Allow: []string{"rm -rf"}},
		"every 10s":    {Name: "a", Kind: Tool, Tool: "t", When: "@every 10s", Prompt: "x"},
	}
	for name, tr := range bad {
		if _, err := tr.Check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestARunPastItsTimeoutIsStopped(t *testing.T) {
	store, clock, rec := harness(t, Trigger{Name: "hook", Kind: Webhook, Secret: strings.Repeat("s", 32), Prompt: "x"})
	rec.block = make(chan struct{}) // the run lasts until its ctx ends
	s := launch(t, store, clock, rec)
	s.mu.Lock()
	s.shortLimit = 50 * time.Millisecond
	s.mu.Unlock()
	if err := s.Fire("hook", "x"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "not stopped", func() bool {
		st, _ := store.Status()
		return strings.Contains(st["hook"].LastError, "stopped after")
	})
}

func TestTimeoutBounds(t *testing.T) {
	for _, d := range []string{"10s", "3h", "soon"} {
		if _, err := (Trigger{Name: "a", When: "@daily", Prompt: "x", Timeout: d}).Check(); err == nil {
			t.Errorf("%s accepted", d)
		}
	}
}

func TestFieldsOfAnotherKind(t *testing.T) {
	for name, tr := range map[string]Trigger{
		"path on time":     {Name: "a", When: "@daily", Path: "/x", Prompt: "x"},
		"secret on tool":   {Name: "a", Kind: Tool, Tool: "t", Secret: strings.Repeat("s", 32), Prompt: "x"},
		"when on webhook":  {Name: "a", Kind: Webhook, Secret: strings.Repeat("s", 32), When: "@daily", Prompt: "x"},
		"catch-up on file": {Name: "a", Kind: File, Path: filepath.Join(os.TempDir(), "x"), CatchUp: true, Prompt: "x"},
	} {
		if _, err := tr.Check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestClipStaysWithinItsBound(t *testing.T) {
	if got := clipTo(strings.Repeat("é", 20000), MaxDetail); len(got) > MaxDetail {
		t.Fatalf("%d", len(got))
	}
}

func TestANewTargetStartsAfresh(t *testing.T) {
	store, clock, rec := harness(t, Trigger{Name: "inbox", Kind: Tool, Tool: "mail_a", Prompt: "x"})
	if err := store.saveStatus(map[string]Status{"inbox": {Target: (Trigger{Name: "inbox", Kind: Tool, Tool: "mail_old", Prompt: "x"}).target(), Seen: "old", LastFire: start}}); err != nil {
		t.Fatal(err)
	}
	tool := &answers{text: "other answer"}
	launchWith(t, store, clock, rec, tool.call)
	clock.advance(5 * time.Minute)
	clock.settle(t)
	eventually(t, "no look", func() bool { st, _ := store.Status(); return st["inbox"].Seen != "" })
	time.Sleep(20 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatal("the first look on a new target fired")
	}
	if st, _ := store.Status(); !st["inbox"].LastFire.IsZero() {
		t.Fatal("the old trigger's status kept")
	}
}

func TestPeriod(t *testing.T) {
	tr := Trigger{Name: "dec", When: "@daily", Prompt: "x", TimeZone: "Europe/Rome", From: "2026-12-01", Until: "2026-12-31"}
	if _, err := tr.Check(); err != nil {
		t.Fatal(err)
	}
	rome, _ := time.LoadLocation("Europe/Rome")
	for at, want := range map[time.Time]bool{
		time.Date(2026, 11, 30, 23, 59, 0, 0, rome): false,
		time.Date(2026, 12, 1, 0, 0, 0, 0, rome):    true,
		time.Date(2026, 12, 31, 23, 59, 0, 0, rome): true, // the whole last day
		time.Date(2027, 1, 1, 0, 0, 0, 0, rome):     false,
	} {
		if tr.Working(at) != want {
			t.Errorf("%v: %v", at, !want)
		}
	}
	if !tr.Ended(time.Date(2027, 1, 1, 0, 0, 0, 0, rome)) || tr.Ended(time.Date(2026, 12, 31, 12, 0, 0, 0, rome)) {
		t.Fatal("ended")
	}
	for _, bad := range []Trigger{
		{Name: "a", When: "@daily", Prompt: "x", From: "1 december"},
		{Name: "a", When: "@daily", Prompt: "x", From: "2026-12-31", Until: "2026-12-01"},
	} {
		if _, err := bad.Check(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestAnEndedTriggerIsRemoved(t *testing.T) {
	store, clock, rec := harness(t,
		Trigger{Name: "past", When: "@every 10m", Prompt: "x", Until: "2026-11-01"},
		Trigger{Name: "later", When: "@every 10m", Prompt: "x", From: "2027-01-01"},
		trigger("now", "@every 10m"))
	launch(t, store, clock, rec)
	list, _ := store.Triggers()
	if len(list) != 2 || rec.kinds("past", Ended) != 1 {
		t.Fatalf("%+v", list)
	}
	clock.advance(10 * time.Minute)
	clock.settle(t)
	eventually(t, "the working one", func() bool { return rec.kinds("now", Finished) == 1 })
	if rec.kinds("later", Started) != 0 {
		t.Fatal("ran before its period")
	}
}
