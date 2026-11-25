// voice_test.go: voice answered with voice, when wanted and possible
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

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// voiceApp is a chat app that sends voice.
type voiceApp struct {
	*chatApp
	voices chan Audio
	refuse atomic.Bool
}

func (a *voiceApp) SendVoice(_ context.Context, _ string, v Audio) error {
	if a.refuse.Load() {
		return errors.New("no voice today")
	}
	a.voices <- v
	return nil
}

type mouth struct{ fail bool }

func (m *mouth) Synthesize(_ context.Context, text string) ([]byte, string, error) {
	if m.fail {
		return nil, "", errors.New("hoarse")
	}
	return []byte("spoken:" + text), "audio/ogg", nil
}

type voiceRig struct {
	*rig
	app    *voiceApp
	bridge *Bridge
	events chan Event
}

func newVoiceRig(t *testing.T, cfg Config, steps ...fake.Step) *voiceRig {
	t.Helper()
	r := newRig(t, true, steps...)
	v := &voiceRig{rig: r, app: &voiceApp{chatApp: newApp(), voices: make(chan Audio, 4)}, events: make(chan Event, 16)}
	cfg.Channel, cfg.Server, cfg.Owners = v.app, r.srv, []string{me}
	cfg.Transcriber = &ear{}
	cfg.OnEvent = func(e Event) { v.events <- e }
	b, err := NewBridge(ephemeral(cfg))
	if err != nil {
		t.Fatal(err)
	}
	v.bridge = b
	t.Cleanup(b.wg.Wait)
	return v
}

func (v *voiceRig) voiceMessage() {
	f := fileOf("", "audio/ogg", []byte("OggS"))
	f.Voice = true
	v.bridge.handle(context.Background(), Message{Chat: room, From: me, Files: []File{f}})
}

func (v *voiceRig) text(text string) {
	v.bridge.handle(context.Background(), Message{Chat: room, From: me, Text: text})
}

func TestVoiceIsAnsweredWithVoice(t *testing.T) {
	v := newVoiceRig(t, Config{Synthesizer: &mouth{}, VoiceReplies: true}, fake.Reply("**Milk** bought"), fake.Reply("in text"))
	v.voiceMessage()
	if got := <-v.app.voices; string(got.Data) != "spoken:Milk bought" || got.MIME != "audio/ogg" {
		t.Fatalf("voice %+v", got)
	}
	v.text("and now?")
	if got := v.app.next(t).Text; got != "in text" {
		t.Fatalf("a text message answered %q", got)
	}
}

func TestTextCommandWins(t *testing.T) {
	v := newVoiceRig(t, Config{Synthesizer: &mouth{}, VoiceReplies: true}, fake.Reply("hello"))
	v.text("/text")
	v.app.next(t)
	v.voiceMessage()
	if got := v.app.next(t).Text; got != "hello" {
		t.Fatalf("reply %q", got)
	}
}

func TestVoiceCommand(t *testing.T) {
	v := newVoiceRig(t, Config{Synthesizer: &mouth{}}, fake.Reply("hello"))
	v.voiceMessage() // host default: text
	if got := v.app.next(t).Text; got != "hello" {
		t.Fatalf("reply %q", got)
	}
	v2 := newVoiceRig(t, Config{Synthesizer: &mouth{}}, fake.Reply("hi"))
	v2.text("/voice@MyBot")
	if got := v2.app.next(t).Text; !strings.Contains(got, "with voice") {
		t.Fatalf("voice on: %q", got)
	}
	v2.voiceMessage()
	if got := <-v2.app.voices; string(got.Data) != "spoken:hi" {
		t.Fatalf("voice %q", got.Data)
	}
}

func TestVoiceCannotBeTurnedOnWithoutASynthesizer(t *testing.T) {
	v := newVoiceRig(t, Config{VoiceReplies: true}, fake.Reply("hi"))
	v.text("/voice")
	if got := v.app.next(t).Text; !strings.Contains(got, "cannot") {
		t.Fatalf("reply %q", got)
	}
	v.voiceMessage()
	if got := v.app.next(t).Text; got != "hi" {
		t.Fatalf("reply %q", got)
	}
}

func TestVoiceFallsBackToText(t *testing.T) {
	cases := map[string]struct {
		mouth  *mouth
		answer string
		refuse bool
		event  bool
	}{
		"hoarse":     {&mouth{fail: true}, "hi", false, true},
		"long":       {&mouth{}, strings.Repeat("a", maxSpoken+1), false, false},
		"code":       {&mouth{}, "run ```ls```", false, false},
		"no sending": {&mouth{}, "hi", true, true},
	}
	for name, c := range cases {
		v := newVoiceRig(t, Config{Synthesizer: c.mouth, VoiceReplies: true}, fake.Reply(c.answer))
		v.app.refuse.Store(c.refuse)
		v.voiceMessage()
		if got := v.app.next(t).Text; got != c.answer {
			t.Errorf("%s: reply %q", name, got)
		}
		if got := len(v.events) > 0; got != c.event {
			t.Errorf("%s: event %v", name, got)
		}
	}
}

func TestPlain(t *testing.T) {
	for in, want := range map[string]string{
		"# Title\n- **one**\n> `two`":    "Title\none\ntwo",
		"-5 °C and 2*3*4 = 24":           "-5 °C and 2*3*4 = 24",
		"snake_case_name":                "snake_case_name",
		"1. see [the docs](https://x.y)": "see the docs",
		"_italic_ and *more*":            "italic and more",
	} {
		if got := plain(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// slowMouth speaks only when its context ends, and says so.
type slowMouth struct{ started chan struct{} }

func (m slowMouth) Synthesize(ctx context.Context, _ string) ([]byte, string, error) {
	close(m.started)
	<-ctx.Done()
	return nil, "", ctx.Err()
}

func TestStopWhileSpeakingSendsText(t *testing.T) {
	m := slowMouth{started: make(chan struct{})}
	v := newVoiceRig(t, Config{Synthesizer: m, VoiceReplies: true}, fake.Reply("the answer"))
	v.voiceMessage()
	<-m.started
	v.text("after") // the chat is still busy with its answer
	if got := v.app.next(t).Text; !strings.Contains(got, "still working") {
		t.Fatalf("busy: %q", got)
	}
	v.text("/stop")
	if got := v.app.next(t).Text; got != "the answer" {
		t.Fatalf("the answer after /stop: %q", got)
	}
	if len(v.events) != 0 {
		t.Fatalf("a stop reported as a failure: %+v", <-v.events)
	}
}
