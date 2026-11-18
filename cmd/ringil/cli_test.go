// cli_test.go: tests for the ringil command
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	stderrors "errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

type harness struct {
	c        *cli
	out, err *bytes.Buffer
	script   fake.Script
	config   openai.Config
	dir      string
}

func newHarness(input string, steps ...fake.Step) *harness {
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, script: fake.New(steps...), dir: os.TempDir()}
	h.c = newCLI(strings.NewReader(input), h.out, h.err)
	h.c.getenv = func(k string) string {
		return map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "RINGIL_API_KEY": "k"}[k]
	}
	h.c.newProvider = func(cfg openai.Config) (agent.Provider, error) {
		h.config = cfg
		return h.script, nil
	}
	h.c.turnContext = func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
	h.c.configDir = func() (string, error) { return h.dir, nil }
	return h
}

func (h *harness) run(args ...string) int { return run(context.Background(), args, h.c) }

func TestRunPrintsAnswerAndUsesEnvironment(t *testing.T) {
	h := newHarness("", fake.Stream("Ciao", " Antonio"))
	if code := h.run("run", "saluta"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.out.String() != "Ciao Antonio" || !strings.Contains(h.err.String(), "[end, 1 model calls") {
		t.Fatalf("out %q err %q", h.out, h.err)
	}
	if h.config.BaseURL != "http://localhost:1/v1" || h.config.Model != "m" || h.config.APIKey != "k" {
		t.Fatalf("config %+v", h.config)
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	h := newHarness("", fake.Reply("ok"))
	if code := h.run("--base", "https://x.test/v1", "--model", "other", "--key-env", "NOPE", "--allow-http", "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.BaseURL != "https://x.test/v1" || h.config.Model != "other" || h.config.APIKey != "" || !h.config.AllowHTTP {
		t.Fatalf("config %+v", h.config)
	}
}

func TestRunShowsToolCalls(t *testing.T) {
	h := newHarness("", fake.Call("calculate", `{"a":2,"op":"*","b":21}`), fake.Reply("42"))
	if code := h.run("run", "2*21"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "-> calculate") || !strings.Contains(h.err.String(), "<- 42") || h.out.String() != "42" {
		t.Fatalf("out %q err %q", h.out, h.err)
	}
}

func TestNoToolsFlag(t *testing.T) {
	h := newHarness("", fake.Reply("ok"))
	if code := h.run("--no-tools", "run", "x"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if len(h.script.Requests()[0].Tools) != 0 {
		t.Fatal("tools offered despite --no-tools")
	}
}

func TestRunErrors(t *testing.T) {
	h := newHarness("")
	if code := h.run("run"); code != exitError || !strings.Contains(h.err.String(), "nothing to send") {
		t.Fatalf("empty prompt: %d %q", code, h.err)
	}
	h = newHarness("", fake.Fail(errors.New(agent.ErrProviderAuth, "401")))
	if code := h.run("run", "x"); code != exitError || !strings.Contains(h.err.String(), "API key was refused") {
		t.Fatalf("auth: %d %q", code, h.err)
	}
	h = newHarness("")
	h.c.getenv = func(string) string { return "" }
	if code := h.run("run", "x"); code != exitError || !strings.Contains(h.err.String(), "--base and --model") {
		t.Fatalf("missing config: %d %q", code, h.err)
	}
}

func TestChatKeepsHistoryAndCommands(t *testing.T) {
	h := newHarness("ciao\n\nancora\n/reset\ndopo reset\n/exit\nnever\n",
		fake.Reply("uno"), fake.Reply("due"), fake.Reply("tre"))
	if code := h.run(); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	reqs := h.script.Requests()
	if len(reqs) != 3 || len(reqs[1].Messages) != 3 || len(reqs[2].Messages) != 1 {
		t.Fatalf("history per request: %d %d %d", len(reqs[0].Messages), len(reqs[1].Messages), len(reqs[2].Messages))
	}
	if !strings.Contains(h.err.String(), "history cleared") {
		t.Fatalf("err %q", h.err)
	}
}

func TestChatSurvivesAFailedTurn(t *testing.T) {
	h := newHarness("primo\nsecondo\n", fake.Fail(stderrors.New("boom")), fake.Reply("ok"))
	if code := h.run("chat"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	reqs := h.script.Requests()
	// The failed prompt stays in the conversation, so the model sees both.
	if !strings.Contains(h.err.String(), "error: boom") || len(reqs) != 2 || len(reqs[1].Messages) != 2 {
		t.Fatalf("err %q requests %d", h.err, len(reqs))
	}
}

func TestCleanStripsTerminalControls(t *testing.T) {
	in := "ok\x1b[2J\x1b]0;pwned\x07\r\u009bhidden\ttab\nline\x7f"
	if got := clean(in); got != "ok[2J]0;pwnedhidden\ttab\nline" {
		t.Fatalf("clean = %q", got)
	}
}

func TestShortTruncates(t *testing.T) {
	if got := short(strings.Repeat("é", 300)); len([]rune(got)) != 203 {
		t.Fatalf("len %d", len([]rune(got)))
	}
	if got := short("a\n  b"); got != "a b" {
		t.Fatalf("short = %q", got)
	}
}

func TestBuiltinTools(t *testing.T) {
	clock = func() time.Time { return time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC) }
	defer func() { clock = time.Now }()
	cases := map[string]string{
		`{"zone":"UTC"}`:                "Wednesday 18 November 2026, 09:30:00 UTC",
		`{"zone":"Mars/Olympus"}`:       "Unknown time zone",
		`{"a":1,"op":"/","b":0}`:        "Division by zero.",
		`{"a":1e308,"op":"*","b":10}`:   "not a finite number",
		`{"a":7,"op":"-","b":10}`:       "-3",
		`{"a":1.5,"op":"+","b":1}`:      "2.5",
		`{"a":6,"op":"/","b":4}`:        "1.5",
		`{"a":6,"op":"%","b":4}`:        "must be one of",
		`{"a":"6","op":"+","b":4}`:      "must be a number",
		`{"zone":"UTC","extra":"x"}`:    "allowed: zone",
		`{"a":2,"op":"*","b":3,"x":""}`: "allowed: a, op, b",
	}
	tools, err := builtinTools()
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range cases {
		name := "calculate"
		if strings.Contains(input, "zone") {
			name = "now"
		}
		res, err := tools.Execute(context.Background(), agent.ToolCall{ID: "1", Name: name, Input: []byte(input)})
		if res.IsError != (err != nil) || !strings.Contains(res.Content, want) {
			t.Errorf("%s %s = %q, want %q", name, input, res.Content, want)
		}
	}
}

func sessionHarness(t *testing.T, input string, steps ...fake.Step) *harness {
	h := newHarness(input, steps...)
	h.dir = t.TempDir()
	return h
}

func TestSessionResumesAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	first := sessionHarness(t, "", fake.Reply("ti chiami Antonio"))
	first.dir = dir
	if code := first.run("--session", "lavoro", "run", "mi chiamo Antonio"); code != exitOK {
		t.Fatalf("exit %d: %s", code, first.err)
	}
	second := sessionHarness(t, "", fake.Reply("Antonio"))
	second.dir = dir
	if code := second.run("-S", "lavoro", "run", "come mi chiamo?"); code != exitOK {
		t.Fatalf("exit %d: %s", code, second.err)
	}
	msgs := second.script.Requests()[0].Messages
	if len(msgs) != 3 || msgs[0].Text() != "mi chiamo Antonio" || !strings.Contains(second.err.String(), "resumed session lavoro (2 messages)") {
		t.Fatalf("messages %+v err %q", msgs, second.err)
	}
}

func TestSessionsListAndRemove(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"uno", "due"} {
		h := sessionHarness(t, "", fake.Reply("ok"))
		h.dir = dir
		if code := h.run("--session", id, "run", "x"); code != exitOK {
			t.Fatalf("exit %d: %s", code, h.err)
		}
	}
	h := sessionHarness(t, "")
	h.dir = dir
	if code := h.run("sessions"); code != exitOK || !strings.Contains(h.err.String(), "uno") || !strings.Contains(h.err.String(), "due") {
		t.Fatalf("list %d %q", code, h.err)
	}
	if code := h.run("sessions", "rm", "uno"); code != exitOK {
		t.Fatalf("rm %d %q", code, h.err)
	}
	h.err.Reset()
	if code := h.run("sessions"); code != exitOK || strings.Contains(h.err.String(), "uno") {
		t.Fatalf("after rm %q", h.err)
	}
}

func TestSessionsEdgeCases(t *testing.T) {
	h := sessionHarness(t, "")
	if code := h.run("sessions"); code != exitOK || !strings.Contains(h.err.String(), "no saved sessions") {
		t.Fatalf("empty list %q", h.err)
	}
	if code := h.run("sessions", "rm"); code != exitError {
		t.Fatal("rm without names")
	}
	if code := h.run("--session", "../evil", "run", "x"); code != exitError || !strings.Contains(h.err.String(), "session ID") {
		t.Fatalf("bad id %q", h.err)
	}
	h.c.configDir = func() (string, error) { return "", stderrors.New("no home") }
	if code := h.run("sessions"); code != exitError || !strings.Contains(h.err.String(), "--sessions-dir") {
		t.Fatalf("no config dir %q", h.err)
	}
	if code := h.run("--sessions-dir", t.TempDir(), "sessions"); code != exitOK {
		t.Fatalf("explicit dir %q", h.err)
	}
}

func TestRecoveredCallIsShown(t *testing.T) {
	ev := agent.Event{Kind: agent.EventRecovered, Call: &agent.ToolCall{Name: "calculate"}, Output: &agent.ToolResult{}}
	h := newHarness("")
	if err := h.c.render(ev); err != nil || !strings.Contains(h.err.String(), "calculate was interrupted") {
		t.Fatalf("render %v %q", err, h.err)
	}
}

func TestContextWindowFlagCompacts(t *testing.T) {
	long := strings.Repeat("parola ", 400)
	h := newHarness(long+"\n"+long+"\n"+long+"\n", fake.Reply(long), fake.Reply(long), fake.Reply("ok"))
	if code := h.run("--context-window", "1500", "chat"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "~ context compacted:") {
		t.Fatalf("err %q", h.err)
	}
}
