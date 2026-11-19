// sessions_test.go: /new, a long silence, and both across restarts
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// sessionRig is a bridge with a sessions file and a clock of its own.
func sessionRig(t *testing.T, r *rig, file string, idle time.Duration) (*Bridge, *time.Time) {
	t.Helper()
	b, err := NewBridge(Config{Channel: r.app, Server: r.srv, Owners: []string{me}, Sessions: file, IdleReset: idle})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	t.Cleanup(b.wg.Wait)
	return b, &clock
}

func (r *rig) turn(t *testing.T, b *Bridge, text string) string {
	t.Helper()
	b.handle(context.Background(), Message{Chat: room, From: me, Text: text})
	return r.app.next(t).Text
}

// seen is how many earlier messages the model saw in the last request.
func (r *rig) seen() int {
	reqs := r.model.Requests()
	return len(reqs[len(reqs)-1].Messages) - 1
}

func TestNewStartsAFreshConversation(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"), fake.Reply("three"))
	file := filepath.Join(t.TempDir(), "state", "sessions.json")
	b, _ := sessionRig(t, r, file, 0)
	r.turn(t, b, "first")
	r.turn(t, b, "second")
	if r.seen() != 2 {
		t.Fatalf("the same conversation saw %d", r.seen())
	}
	if got := r.turn(t, b, "/new"); !strings.Contains(got, "New conversation") {
		t.Fatalf("new: %q", got)
	}
	r.turn(t, b, "third")
	if r.seen() != 0 {
		t.Fatalf("a new conversation saw %d earlier messages", r.seen())
	}
	again, _ := sessionRig(t, r, file, 0) // a restart keeps the number
	if got := again.current(room); got != "test-7_2" {
		t.Fatalf("after a restart: %q", got)
	}
	if info, err := os.Stat(file); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("sessions file %v %v", info, err)
	}
}

func TestSilenceStartsANewConversation(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"), fake.Reply("three"))
	b, clock := sessionRig(t, r, "", 0)
	r.turn(t, b, "first")
	*clock = clock.Add(23 * time.Hour)
	r.turn(t, b, "second")
	if r.seen() != 2 {
		t.Fatalf("23 hours later: saw %d", r.seen())
	}
	*clock = clock.Add(25 * time.Hour)
	if got := r.turn(t, b, "third"); !strings.Contains(got, "new conversation") {
		t.Fatalf("not told: %q", got)
	}
	r.app.next(t)
	if r.seen() != 0 {
		t.Fatalf("after a day of silence: saw %d", r.seen())
	}
}

func TestSilenceResetCanBeTurnedOff(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"))
	b, clock := sessionRig(t, r, "", -1)
	r.turn(t, b, "first")
	*clock = clock.Add(1000 * time.Hour)
	r.turn(t, b, "second")
	if r.seen() != 2 {
		t.Fatalf("saw %d", r.seen())
	}
}

func TestNoNewDuringARun(t *testing.T) {
	r, q := approval(t, fake.Reply("finished"))
	r.app.say("/new")
	if got := r.app.next(t).Text; !strings.Contains(got, "/stop first") {
		t.Fatalf("new during a run: %q", got)
	}
	r.app.press(q.Buttons[2].ID)
	r.app.next(t)
	r.app.next(t)
}

func TestDamagedSessionsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(file, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, true)
	if _, err := NewBridge(Config{Channel: r.app, Server: r.srv, Sessions: file}); err == nil {
		t.Fatal("a damaged sessions file was taken as empty")
	}
}

func TestSilenceCountsFromTheLastExchange(t *testing.T) {
	r := newRig(t, true, fake.Call("delete_file", `{}`), fake.Reply("done"), fake.Reply("next"))
	b, clock := sessionRig(t, r, "", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.handle(ctx, Message{Chat: room, From: me, Text: "clean up"})
	q := r.app.next(t)
	*clock = clock.Add(50 * time.Minute) // the user answers late
	b.handle(ctx, Message{Chat: room, From: me, Pressed: q.Buttons[0].ID})
	r.app.next(t)
	r.app.next(t)
	*clock = clock.Add(50 * time.Minute) // 100 minutes after the start, 50 after the answer
	if got := r.turn(t, b, "and then?"); got != "next" {
		t.Fatalf("reset while talking: %q", got)
	}
}

func TestALostFileNeverReopensAnOldConversation(t *testing.T) {
	r := newRig(t, true, fake.Reply("one"), fake.Reply("two"), fake.Reply("three"))
	b, _ := sessionRig(t, r, "", 0)
	r.turn(t, b, "first")
	r.turn(t, b, "/new")
	r.turn(t, b, "second")              // test-7_2
	fresh, _ := sessionRig(t, r, "", 0) // a restart without the numbers
	if got := r.turn(t, fresh, "/new"); !strings.Contains(got, "New conversation") {
		t.Fatalf("new: %q", got)
	}
	r.turn(t, fresh, "third")
	if r.seen() != 0 || fresh.current(room) != "test-7_3" {
		t.Fatalf("the new conversation is %s and saw %d", fresh.current(room), r.seen())
	}
}

func TestBadSessionsFiles(t *testing.T) {
	r := newRig(t, true)
	for _, body := range []string{"null", `{"7":{"n":0}}`, `{"7":{"n":99999999999}}`, `[]`} {
		file := filepath.Join(t.TempDir(), "sessions.json")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewBridge(Config{Channel: r.app, Server: r.srv, Sessions: file}); err == nil {
			t.Errorf("%s taken", body)
		}
	}
}

func TestHighest(t *testing.T) {
	ids := []string{"tg-7", "tg-7_2", "TG-7_10", "tg-7_x", "tg-7_03", "tg-70_5", "tg-7_1", "other"}
	if got := highest(ids, "tg-7"); got != 10 {
		t.Fatalf("%d", got)
	}
	if got := highest(nil, "tg-7"); got != 0 {
		t.Fatalf("%d", got)
	}
}
