// cli_test.go: tests for the ringil command
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

type harness struct {
	c        *cli
	out, err *bytes.Buffer
	script   fake.Script
	config   openai.Config
	dir      string
}

func newHarness(t *testing.T, input string, steps ...fake.Step) *harness {
	t.Helper()
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, script: fake.New(steps...), dir: t.TempDir()}
	h.c = newCLI(strings.NewReader(input), h.out, h.err)
	t.Cleanup(func() { _ = h.c.secrets.Close() }) // as main does: Windows keeps an open store's directory
	h.c.getenv = func(k string) string {
		return map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k"}[k]
	}
	h.c.newProvider = func(cfg openai.Config) (agent.Provider, error) {
		h.config = cfg
		return h.script, nil
	}
	h.c.turnContext = func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
	h.c.prices = testPrices // never the network
	h.c.configDir = func() (string, error) { return h.dir, nil }
	h.c.dataDir = func() (string, error) { return filepath.Join(h.dir, "data"), nil }
	h.c.home = func() (string, error) { return "", os.ErrNotExist }
	h.c.restart = func() {} // a test never starts itself again
	h.c.openURL = func(string) error { return nil }
	h.c.noKeyring = true // never the system's keyring of whoever runs the tests
	return h
}

func (h *harness) run(args ...string) int { return run(context.Background(), args, h.c) }

// key resolves the API key the provider would send.
func (h *harness) key(t *testing.T) string {
	t.Helper()
	if h.config.KeyFunc == nil {
		t.Fatal("no key function configured")
	}
	k, err := h.config.KeyFunc(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRunPrintsAnswerAndUsesEnvironment(t *testing.T) {
	h := newHarness(t, "", fake.Stream("Ciao", " Antonio"))
	if code := h.run("run", "saluta"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.out.String() != "Ciao Antonio" || !strings.Contains(h.err.String(), "[end, 1 model calls") {
		t.Fatalf("out %q err %q", h.out, h.err)
	}
	if h.config.BaseURL != "https://api.openai.com/v1" || h.config.Model != "m" || h.key(t) != "k" {
		t.Fatalf("config %+v", h.config)
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "NVIDIA_API_KEY": "nv"})
	if code := h.run("--provider", "nvidia", "--model", "other", "--allow-http", "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.BaseURL != "https://integrate.api.nvidia.com/v1" || h.config.Model != "other" || h.key(t) != "nv" || !h.config.AllowHTTP {
		t.Fatalf("config %+v", h.config)
	}
}

func TestRunShowsToolCalls(t *testing.T) {
	h := newHarness(t, "", fake.Call("calculate", `{"a":2,"op":"*","b":21}`), fake.Reply("42"))
	if code := h.run("run", "2*21"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "-> calculate") || !strings.Contains(h.err.String(), "<- 42") || h.out.String() != "42" {
		t.Fatalf("out %q err %q", h.out, h.err)
	}
}

func TestNoToolsFlag(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("--no-tools", "run", "x"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if len(h.script.Requests()[0].Tools) != 0 {
		t.Fatal("tools offered despite --no-tools")
	}
}

func TestRunErrors(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("run"); code != exitError || !strings.Contains(h.err.String(), "nothing to send") {
		t.Fatalf("empty prompt: %d %q", code, h.err)
	}
	h = newHarness(t, "", fake.Fail(errors.New(agent.ErrProviderAuth, "401")))
	if code := h.run("run", "x"); code != exitError || !strings.Contains(h.err.String(), "API key was refused") {
		t.Fatalf("auth: %d %q", code, h.err)
	}
	h = newHarness(t, "")
	h.c.getenv = func(string) string { return "" }
	if code := h.run("run", "x"); code != exitError || !strings.Contains(h.err.String(), "no model") {
		t.Fatalf("missing config: %d %q", code, h.err)
	}
	h = newHarness(t, "")
	env(h, map[string]string{"RINGIL_PROVIDER": "openai"})
	if code := h.run("run", "x"); code != exitError || !strings.Contains(h.err.String(), "set --model") {
		t.Fatalf("missing config: %d %q", code, h.err)
	}
}

func TestChatKeepsHistoryAndCommands(t *testing.T) {
	h := newHarness(t, "ciao\n\nancora\n/reset\ndopo reset\n/exit\nnever\n",
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
	h := newHarness(t, "primo\nsecondo\n", fake.Fail(stderrors.New("boom")), fake.Reply("ok"))
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
		`{"zone":"UTC","extra":"x"}`:    "were ignored",
		`{"a":2,"op":"*","b":3,"x":""}`: "were ignored",
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
	return newHarness(t, input, steps...)
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
	h := newHarness(t, "")
	if err := h.c.render(ev); err != nil || !strings.Contains(h.err.String(), "calculate was interrupted") {
		t.Fatalf("render %v %q", err, h.err)
	}
}

func TestContextWindowFlagCompacts(t *testing.T) {
	long := strings.Repeat("parola ", 400)
	h := newHarness(t, long+"\n"+long+"\n"+long+"\n", fake.Reply(long), fake.Reply(long), fake.Reply("ok"))
	if code := h.run("--context-window", "1500", "chat"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "~ context compacted:") {
		t.Fatalf("err %q", h.err)
	}
}

func envFile(t *testing.T, mode os.FileMode, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ringil.env")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvFileProvidesSettingsAndKey(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	h.c.getenv = func(string) string { return "" }
	path := envFile(t, 0o600, "RINGIL_PROVIDER=deepseek # comment\nRINGIL_MODEL=filemodel\nDEEPSEEK_API_KEY=sk-file\n")
	if code := h.run("--env-file", path, "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.BaseURL != "https://api.deepseek.com" || h.config.Model != "filemodel" || h.key(t) != "sk-file" {
		t.Fatalf("config %+v", h.config)
	}
}

func TestEnvironmentWinsOverEnvFile(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	path := envFile(t, 0o600, "RINGIL_MODEL=filemodel\nOPENAI_API_KEY=sk-file\n")
	if code := h.run("--env-file", path, "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.Model != "m" || h.key(t) != "k" {
		t.Fatalf("config %+v", h.config)
	}
}

func TestUnsafeEnvFileRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("--env-file", envFile(t, 0o644, "A=1\n"), "run", "x"); code != exitError || !strings.Contains(h.err.String(), "secrets:") {
		t.Fatalf("exit %d: %q", code, h.err)
	}
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func checkSamePath(t *testing.T, got []sandbox.Mount) {
	t.Helper()
	if got[0].Target != "" || got[0].Writable || got[1].Source != abs(t, "/same/rw") || !got[1].Writable {
		t.Fatalf("same-path mounts %+v", got)
	}
}

func TestParseMountDriveLetter(t *testing.T) {
	if m, err := parseMount(`C:\data:/in:rw`); err != nil || m.Target != "/in" || !m.Writable || !strings.HasSuffix(m.Source, "data") {
		t.Fatalf("drive-letter source: %+v %v", m, err)
	}
}

func TestParseMounts(t *testing.T) {
	got, err := parseMounts([]string{"data:/in", "/abs:/out:rw", "/same", "/same/rw:rw"})
	if err != nil || len(got) != 4 || !filepath.IsAbs(got[0].Source) || got[0].Writable || !got[1].Writable || got[1].Target != "/out" {
		t.Fatalf("%+v %v", got, err)
	}
	checkSamePath(t, got[2:])
	for _, bad := range []string{"", ":/b", "a:/b:ro", ":rw"} {
		if _, err := parseMounts([]string{bad}); !errors.HasCode(err, ErrUsage) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

// stubSandbox records the spec and exits with the given error.
type stubSandbox struct {
	spec sandbox.Spec
	exit error
}

func (s *stubSandbox) Start(_ context.Context, spec sandbox.Spec) (sandbox.Process, error) {
	s.spec = spec
	return s, nil
}

func (s *stubSandbox) Wait() error { return s.exit }

func checkSandboxSpec(t *testing.T, spec sandbox.Spec) {
	t.Helper()
	if strings.Join(spec.Argv, " ") != "python3 -V" || spec.Network != sandbox.ProxiedNetwork || spec.Proxy == "" || spec.Dir != "/w" || len(spec.Mounts) != 1 {
		t.Fatalf("spec %+v", spec)
	}
	if len(spec.Protected) != 1 || !strings.HasSuffix(spec.Protected[0], "ringil") {
		t.Fatalf("protected %v", spec.Protected)
	}
}

func TestSandboxCommand(t *testing.T) {
	h := newHarness(t, "")
	stub := &stubSandbox{}
	h.c.newSandbox = func() (sandbox.Sandbox, error) { return stub, nil }
	if code := h.run("sandbox", "--mount", "x:/w:rw", "--allow-domain", "pypi.org", "--dir", "/w", "--", "python3", "-V"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	checkSandboxSpec(t, stub.spec)
	if code := h.run("sandbox"); code != exitError {
		t.Fatal("empty command accepted")
	}
	h.c.newSandbox = func() (sandbox.Sandbox, error) { return nil, errors.New(sandbox.ErrUnavailable, "none here") }
	if code := h.run("sandbox", "--", "true"); code != exitError || !strings.Contains(h.err.String(), "none here") {
		t.Fatalf("unavailable: %q", h.err)
	}
}

// flash-flags v1.3.0 kept only the last of repeated --mount flags; v1.3.1
// accumulates them.
func TestRepeatedMountFlags(t *testing.T) {
	h := newHarness(t, "")
	stub := &stubSandbox{}
	h.c.newSandbox = func() (sandbox.Sandbox, error) { return stub, nil }
	if code := h.run("sandbox", "--mount", "/a", "--mount", "/b:rw", "--", "true"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if len(stub.spec.Mounts) != 2 || stub.spec.Mounts[0].Source != abs(t, "/a") || stub.spec.Mounts[1].Source != abs(t, "/b") {
		t.Fatalf("mounts %+v", stub.spec.Mounts)
	}
}

func TestWorkspaceFlagGrantsFileTools(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("run", "--workspace", dir, "--workspace", t.TempDir(), "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	names := map[string]bool{}
	for _, spec := range h.script.Requests()[0].Tools {
		names[spec.Name] = true
	}
	for _, want := range []string{"read_file", "list_directory", "glob", "grep", "now", "calculate"} {
		if !names[want] {
			t.Errorf("%s not offered: %v", want, names)
		}
	}
	h = newHarness(t, "", fake.Reply("ok"))
	if code := h.run("run", "--workspace", "/nonexistent-ringil", "x"); code != exitError || !strings.Contains(h.err.String(), "does not exist") {
		t.Fatalf("missing dir: %d %q", code, h.err)
	}
}

// readOutside scripts a model that reads a file outside the start folder.
func readOutside(t *testing.T, answer string) (*harness, string) {
	t.Helper()
	start, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "nota.txt"), []byte("contenuto riservato"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"path": filepath.Join(other, "nota.txt")})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, answer, fake.Call("read_file", string(input)), fake.Reply("fatto"))
	h.c.cwd = func() (string, error) { return start, nil }
	if code := h.run("run", "leggi la nota"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	return h, other
}

func TestOutsideFolderAskedAndAllowed(t *testing.T) {
	h, other := readOutside(t, "s\n")
	// The question names the real directory (macOS /private/var, Windows
	// long names), as the workspace resolves it.
	real, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.err.String(), "wants to access "+real) {
		t.Fatalf("not asked: %q", h.err)
	}
	if msgs := h.script.Requests()[1].Messages; !strings.Contains(msgs[len(msgs)-1].Result.Content, "contenuto riservato") {
		t.Fatalf("not read: %+v", msgs[len(msgs)-1].Result)
	}
}

func TestOutsideFolderDeniedByDefault(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", ""} {
		h, _ := readOutside(t, answer)
		res := h.script.Requests()[1].Messages
		if got := res[len(res)-1].Result; !got.IsError || strings.Contains(got.Content, "riservato") {
			t.Fatalf("answer %q: %+v", answer, got)
		}
	}
}

func TestHomeIsAccessibleWithoutAsking(t *testing.T) {
	start, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "doc.txt"), []byte("nella home"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"path": filepath.Join(home, "doc.txt")})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "", fake.Call("read_file", string(input)), fake.Reply("ok"))
	h.c.cwd = func() (string, error) { return start, nil }
	h.c.home = func() (string, error) { return home, nil }
	if code := h.run("run", "leggi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	msgs := h.script.Requests()[1].Messages
	if strings.Contains(h.err.String(), "wants to access") || !strings.Contains(msgs[len(msgs)-1].Result.Content, "nella home") {
		t.Fatalf("err %q result %+v", h.err, msgs[len(msgs)-1].Result)
	}
}

func TestMemoryOnByDefaultAndCommands(t *testing.T) {
	h := newHarness(t, "", fake.Call("remember", `{"content":"Il cliente principale è Acme"}`), fake.Reply("ok"))
	if code := h.run("run", "ricordati che il cliente principale è Acme"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	checkOffered(t, h.script.Requests()[0].Tools, "remember", "recall", "forget")
	id := listedID(t, h, "search", "acme")
	if code := h.run("memory", "rm", id); code != exitOK {
		t.Fatalf("rm %d %q", code, h.err)
	}
	h.err.Reset()
	if code := h.run("memory"); code != exitOK || !strings.Contains(h.err.String(), "nothing remembered") {
		t.Fatalf("after rm %q", h.err)
	}
}

// listedID lists memories and returns the first ID shown.
func listedID(t *testing.T, h *harness, args ...string) string {
	t.Helper()
	h.err.Reset()
	if code := h.run(append([]string{"memory"}, args...)...); code != exitOK || !strings.Contains(h.err.String(), "Il cliente principale è Acme") {
		t.Fatalf("memory %v: %d %q", args, code, h.err)
	}
	return strings.Fields(h.err.String())[0]
}

func checkOffered(t *testing.T, specs []agent.ToolSpec, want ...string) {
	t.Helper()
	names := map[string]bool{}
	for _, spec := range specs {
		names[spec.Name] = true
	}
	for _, w := range want {
		if !names[w] {
			t.Fatalf("%s not offered: %v", w, names)
		}
	}
}

func TestRecallReachesTheModel(t *testing.T) {
	h := newHarness(t, "", fake.Call("remember", `{"content":"Il budget 2027 è 50000 euro"}`), fake.Reply("ok"), fake.Reply("50000"))
	for _, prompt := range []string{"ricorda il budget", "qual è il budget 2027?"} {
		if code := h.run("run", prompt); code != exitOK {
			t.Fatalf("exit %d: %s", code, h.err)
		}
	}
	reqs := h.script.Requests()
	last := reqs[len(reqs)-1].Messages
	if got := last[len(last)-1].ModelText(); !strings.Contains(got, "Il budget 2027 è 50000 euro") {
		t.Fatalf("not recalled: %q", got)
	}
	if !strings.Contains(h.err.String(), "* recalled 1 memory") {
		t.Fatalf("recall not shown: %q", h.err)
	}
}

func TestNoMemoryFlag(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("--no-memory", "run", "x"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, spec := range h.script.Requests()[0].Tools {
		if spec.Name == "remember" {
			t.Fatal("memory tools offered with --no-memory")
		}
	}
	if code := h.run("memory", "rm"); code != exitError {
		t.Fatal("rm without ids accepted")
	}
}

func TestDestructiveToolsAskFirst(t *testing.T) {
	for answer, wantRun := range map[string]bool{"y\n": true, "n\n": false, "": false} {
		h := newHarness(t, answer, fake.Call("forget", `{"id":"nope"}`), fake.Reply("ok"))
		if code := h.run("run", "dimentica"); code != exitOK {
			t.Fatalf("exit %d: %s", code, h.err)
		}
		if !strings.Contains(h.err.String(), "wants to run forget") {
			t.Fatalf("not asked: %q", h.err)
		}
		msgs := h.script.Requests()[1].Messages
		got := msgs[len(msgs)-1].Result.Content
		if ran := !strings.Contains(got, "did not approve"); ran != wantRun {
			t.Errorf("answer %q: result %q", answer, got)
		}
	}
}

func TestCleanStripsBidiOverrides(t *testing.T) {
	if got := clean("a\u202eb\u2066c\u200fd"); got != "abcd" {
		t.Fatalf("clean %q", got)
	}
}

func TestSkillInstallListUseRemove(t *testing.T) {
	src := t.TempDir()
	skill := "---\nname: invoice\ndescription: Check an invoice total. Use for invoices.\n---\nAdd the lines and compare with the total.\n"
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(skill), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "", fake.Call("load_skill", `{"name":"invoice"}`), fake.Reply("ok"))
	if code := h.run("skill", "install", src); code != exitOK || !strings.Contains(h.err.String(), "installed invoice") {
		t.Fatalf("install %d %q", code, h.err)
	}
	if code := h.run("run", "controlla la fattura"); code != exitOK {
		t.Fatalf("run %d %q", code, h.err)
	}
	reqs := h.script.Requests()
	checkOffered(t, reqs[0].Tools, "load_skill", "read_skill_file")
	// 25/11: the list is a context block of the turn, so a skill installed
	// during a session is listed; the system prompt stays as it is.
	if strings.Contains(reqs[0].System, "invoice") || !strings.Contains(fmt.Sprintf("%+v", reqs[0].Messages), `{"name":"invoice"`) {
		t.Fatalf("skill not listed in the turn's context: %q / %+v", reqs[0].System, reqs[0].Messages)
	}
	if !strings.HasPrefix(reqs[0].System, agent.DefaultSystem) { // 20/11: the skills' list took the base's place
		t.Fatalf("the base prompt is gone: %q", reqs[0].System)
	}
	if got := reqs[1].Messages[len(reqs[1].Messages)-1].Result.Content; !strings.Contains(got, "Add the lines") {
		t.Fatalf("load_skill %q", got)
	}
	removeSkill(t, h, "invoice")
}

func removeSkill(t *testing.T, h *harness, name string) {
	t.Helper()
	h.err.Reset()
	if code := h.run("skill", "rm", name); code != exitOK {
		t.Fatalf("rm %d %q", code, h.err)
	}
	if code := h.run("skill"); code != exitOK || !strings.Contains(h.err.String(), "no skills installed") {
		t.Fatalf("after rm %q", h.err)
	}
}

// Audit 18/11: scripts saw the start directory's secrets, and system
// start directories made every script fail.
func TestStartMountForScripts(t *testing.T) {
	h := newHarness(t, "")
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, ".env"), []byte("K=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.c.cwd = func() (string, error) { return work, nil }
	m, ok := h.c.startMount("/home/x", h.dir)
	if !ok || strings.Join(m.Hide, ",") != ".env" {
		t.Fatalf("mount %+v %v", m, ok)
	}
	for _, cwd := range []string{"/", "/tmp", "/usr/share", "/home/x", h.dir, filepath.Dir(h.dir)} {
		h.c.cwd = func() (string, error) { return cwd, nil }
		if _, ok := h.c.startMount("/home/x", filepath.Join(h.dir, "ringil")); ok {
			t.Errorf("%s mounted", cwd)
		}
	}
}

// Audit 18/11: delivered files reached .git in the start directory.
func TestScriptOutputOutsideProjects(t *testing.T) {
	h := newHarness(t, "")
	home := t.TempDir()
	if got := h.c.outputDir(home); got != filepath.Join(home, "ringil-output") {
		t.Fatalf("output %q", got)
	}
	if got := h.c.outputDir(""); got != "" {
		t.Fatalf("no home, output %q", got)
	}
}

// Audit 18/11: runtime prefixes exposed ~/.local or the whole home.
func TestRuntimePrefixes(t *testing.T) {
	home := "/home/u"
	cases := map[string]bool{
		"/home/u/.nvm/versions/node/v22": true,
		"/opt/node":                      true,
		"/home/u":                        false,
		"/home":                          false,
		"/home/u/.local":                 false,
		"/home/u/.cargo":                 false,
		"/home/u/tools/node":             true,
	}
	for prefix, want := range cases {
		if got := allowedPrefix(prefix, home); got != want {
			t.Errorf("%s: %v", prefix, got)
		}
	}
}

// "a" allows a tool for the rest of the session: asked once, run twice.
func TestAlwaysAllowForSession(t *testing.T) {
	h := newHarness(t, "a\n", fake.Call("forget", `{"id":"x1"}`), fake.Call("forget", `{"id":"x2"}`), fake.Reply("ok"))
	if code := h.run("run", "dimentica"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if n := strings.Count(h.err.String(), "wants to run forget"); n != 1 {
		t.Fatalf("asked %d times: %q", n, h.err)
	}
	msgs := h.script.Requests()[2].Messages
	if got := msgs[len(msgs)-1].Result.Content; strings.Contains(got, "did not approve") {
		t.Fatalf("second call refused: %q", got)
	}
}

// A memory by meaning whose embedder is down still recalls by words, and
// the chat says so instead of "memory unavailable".
func TestPartialRecallIsANote(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"), fake.Reply("ok"))
	env := h.c.getenv
	h.c.getenv = func(k string) string {
		if k == "RINGIL_EMBED_MODEL" {
			return "emb"
		}
		return env(k)
	}
	h.c.newEmbedder = func(openai.Config) (agent.Embedder, error) { return downEmbedder{}, nil }
	if code := h.run("run", "Ricordati che Acme paga a 30 giorni"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if code := h.run("run", "Acme paga a quanti giorni?"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if strings.Contains(h.err.String(), "memory unavailable") {
		t.Fatalf("partial recall shown as failure: %q", h.err)
	}
}

type downEmbedder struct{}

func (downEmbedder) Model() string { return "emb" }
func (downEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New(agent.ErrEmbed, "down")
}

// Audit 18/11: invisible and line-separator characters reached the terminal.
func TestCleanStripsInvisibles(t *testing.T) {
	if got := clean("a\u200bb\u2028c\u061cd\ufeffe"); got != "abcde" {
		t.Fatalf("clean %q", got)
	}
}

// The project notes are kept without asking, as Claude Code keeps its
// memory (21/11); deleting one asks. Audit 18/11: a session name could
// point the scratchpad outside the config directory.
func TestProjectNotesAskAndSessionScratch(t *testing.T) {
	h := newHarness(t, "n\n", fake.Call("notes", `{"command":"create","path":"/project/INDEX.md","file_text":"x"}`),
		fake.Call("notes", `{"command":"delete","path":"/project/INDEX.md"}`), fake.Reply("ok"))
	if code := h.run("run", "salva"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if out := h.err.String(); strings.Count(out, "wants to run notes") != 1 || !strings.Contains(out, "Saved /project/INDEX.md") {
		t.Fatalf("asked other than for the delete: %q", out)
	}
	h2 := newHarness(t, "")
	if code := h2.run("--session", "../../x", "run", "ciao"); code == exitOK {
		t.Fatal("bad session name accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(h2.dir)), "x")); !os.IsNotExist(err) {
		t.Fatalf("folder created outside: %v", err)
	}
}

// Audit 18/11: the general worker never ran (its notes and task list
// clashed with the parent's), and a worker's "always" applied to the
// parent.
func TestGeneralWorkerRunsAndAsksAsItself(t *testing.T) {
	h := newHarness(t, "a\n",
		fake.Call("general", `{"task":"forget x1"}`),
		fake.Call("forget", `{"id":"x1"}`), // the worker has no memory tools: unknown
		fake.Reply("worker done"),
		fake.Reply("ok"))
	if code := h.run("run", "vai"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if strings.Contains(h.err.String(), "DELEGATE_CONFIG") || !strings.Contains(h.err.String(), "[general 1] done") {
		t.Fatalf("general worker did not run: %q", h.err)
	}
}

// Audit 18/11: a background worker's question read the same line as the
// chat prompt. Now the prompt holds the terminal: the user's message goes
// to the chat, and the question is asked, and answered, after it.
func TestBackgroundQuestionWaitsForThePrompt(t *testing.T) {
	pr, pw := io.Pipe()
	c := newCLI(pr, &bytes.Buffer{}, &bytes.Buffer{})
	chat := make(chan string, 1)
	go func() {
		line, _, _ := c.promptLine()
		chat <- line
	}()
	time.Sleep(50 * time.Millisecond)
	approved := make(chan bool, 1)
	go func() {
		ok, err := c.approve(context.Background(), agent.ToolCall{Name: "wipe", Input: []byte(`{}`)}, agent.ToolSpec{Annotations: agent.Annotations{Destructive: true}})
		if err != nil {
			t.Error(err)
		}
		approved <- ok
	}()
	time.Sleep(50 * time.Millisecond)
	for _, line := range []string{"riassumi il repo\n", "y\n"} {
		if _, err := io.WriteString(pw, line); err != nil {
			t.Fatal(err)
		}
	}
	if got := <-chat; got != "riassumi il repo" {
		t.Fatalf("chat got %q", got)
	}
	if ok := <-approved; !ok {
		t.Fatal("the answer after the message did not reach the question")
	}
}
