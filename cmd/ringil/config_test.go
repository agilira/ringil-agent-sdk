// config_test.go: ringil.yaml
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agilira/argus"
	"github.com/agilira/orpheus/pkg/orpheus"
	agent "github.com/agilira/ringil-agent-sdk"

	"github.com/agilira/ringil-agent-sdk/memory/chroma"
	"github.com/agilira/ringil-agent-sdk/memory/mem0"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

const fullConfig = `
model: file-model
key_env: ACME_KEY
instructions: company.md
max_turns: 7
allow_domains: [pypi.org, "*.github.com"]
profiles:
  support:
    description: answers customers
    instructions: support.md
    tools: [read_file]
    memory: chat
mcp_servers:
  files:
    command: mcp-files
    args: [--root, /srv]
    env: {TOKEN: "${FILES_TOKEN}"}
agents:
  billing:
    url: https://billing.example
    headers: {X-Api-Key: "${BILLING_KEY}"}
serve:
  listen: 127.0.0.1:9000
  member_profile: support
  replies: text
  telegram:
    owners: ["1"]
    members: ["2", "3"]
`

// writeConfig writes ringil.yaml and the files it names in dir/ringil.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	root := filepath.Join(dir, "ringil")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"company.md": "We are Acme.", "support.md": "Be kind."} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "ringil.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loaded runs fn after loading the configuration, as a command does.
func loaded(t *testing.T, c *cli, args []string, fn func(ctx *orpheus.Context) error) {
	t.Helper()
	probe(t, c, args, func(ctx *orpheus.Context) error {
		defer c.shutdown()
		if err := c.loadConfig(ctx); err != nil {
			return err
		}
		return fn(ctx)
	})
}

func TestConfigIsRead(t *testing.T) {
	h := newHarness(t, "")
	writeConfig(t, h.dir, fullConfig)
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		f := h.c.conf()
		root := filepath.Join(h.dir, "ringil")
		switch {
		case f.Instructions != filepath.Join(root, "company.md") || f.Profiles["support"].Instructions != filepath.Join(root, "support.md"):
			t.Errorf("relative paths are not from the file's directory: %q %q", f.Instructions, f.Profiles["support"].Instructions)
		case f.MCPServers["files"].Env["TOKEN"] != "${FILES_TOKEN}" || f.Agents["billing"].Headers["X-Api-Key"] != "${BILLING_KEY}":
			t.Errorf("references must stay unexpanded: %+v %+v", f.MCPServers, f.Agents)
		case f.Serve.listen() != "127.0.0.1:9000" || f.Serve.replies() != "text" || len(f.Serve.Telegram.Members) != 2:
			t.Errorf("serve %+v", f.Serve)
		}
		checkSettings(t, h.c, ctx)
		return nil
	})
}

func checkSettings(t *testing.T, c *cli, ctx *orpheus.Context) {
	t.Helper()
	if c.flagString(ctx, "allow-domains") != "pypi.org,*.github.com" || c.flagInt(ctx, "max-turns") != 7 || c.keyEnv(ctx) != "ACME_KEY" {
		t.Errorf("settings %q %d %q", c.flagString(ctx, "allow-domains"), c.flagInt(ctx, "max-turns"), c.keyEnv(ctx))
	}
}

func TestServeDefaults(t *testing.T) {
	var s serveConfig
	if s.listen() != defaultListen || s.maxRuns() != defaultMaxRuns || s.maxPerHour() != defaultMaxPerHour ||
		s.language() != "en" || s.replies() != "voice" || s.newAfter() != "24h" {
		t.Errorf("defaults %+v", s)
	}
	none := -1
	if (serveConfig{MaxPerHour: &none}).maxPerHour() != -1 {
		t.Error("a negative max_per_hour (no bound) was not kept")
	}
	zero := 0
	if (serveConfig{MaxPerHour: &zero}).maxPerHour() != 0 {
		t.Error("max_per_hour: 0 was read as the default")
	}
}

// A flag wins over the file; the model in the environment too.
func TestFlagsAndEnvironmentWin(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeConfig(t, h.dir, fullConfig)
	if code := h.run("--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.Model != "m" { // RINGIL_MODEL, from the harness's environment
		t.Errorf("the environment did not win over the file: %q", h.config.Model)
	}
	h = newHarness(t, "", fake.Reply("ok"))
	h.c.getenv = func(k string) string {
		return map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "ACME_KEY": "k"}[k]
	}
	writeConfig(t, h.dir, fullConfig)
	if code := h.run("--no-tools", "run", "hi"); code != 0 || h.config.Model != "file-model" {
		t.Fatalf("the file's model: %d %q %s", code, h.config.Model, h.err)
	}
	h = newHarness(t, "", fake.Reply("ok"))
	writeConfig(t, h.dir, fullConfig)
	if code := h.run("--model", "flag-model", "--no-tools", "run", "hi"); code != 0 || h.config.Model != "flag-model" {
		t.Fatalf("the flag's model: %d %q", code, h.config.Model)
	}
}

func TestConfigRefused(t *testing.T) {
	for name, body := range map[string]string{
		"a typo":                       "modle: x\n",
		"a typo deep down":             "mcp_servers:\n  f: {comand: x}\n",
		"a profile named default":      "profiles:\n  default: {}\n",
		"a bad profile":                "profiles:\n  w: {max_turns: -1}\n",
		"a chat profile with all":      "profiles:\n  w: {memory: chat}\nserve:\n  member_profile: w\n",
		"a chat profile not there":     "serve:\n  public_profile: nobody\n",
		"bad replies":                  "serve:\n  replies: shout\n",
		"webhooks without triggers":    "serve:\n  no_triggers: true\n  webhooks: 127.0.0.1:1\n",
		"a bad new_after":              "serve:\n  new_after: soon\n",
		"two documents":                "model: a\n---\nmodel: b\n",
		"the old JSON of the profiles": `{"profiles":{"w":{"system":"x"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "", fake.Reply("ok"))
			writeConfig(t, h.dir, body)
			if code := h.run("--no-tools", "run", "hi"); code == 0 {
				t.Errorf("accepted %q", body)
			}
		})
	}
}

func TestConfigFiles(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"), fake.Reply("ok"))
	if code := h.run("--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("no file is no configuration: %d %s", code, h.err)
	}
	h.c.settings = nil // one command per process: a new one reads again
	missing := filepath.Join(h.dir, "nowhere.yaml")
	if code := h.run("--config", missing, "--no-tools", "run", "hi"); code == 0 {
		t.Error("a missing --config was accepted")
	}
	other := filepath.Join(t.TempDir(), "acme.yaml")
	if err := os.WriteFile(other, []byte("model: acme-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.c.settings = nil
	h.c.getenv = func(k string) string { return map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1"}[k] }
	if code := h.run("--config", other, "--no-tools", "run", "hi"); code != 0 || h.config.Model != "acme-model" {
		t.Errorf("--config: %d %q %s", code, h.config.Model, h.err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(other, 0o666); err != nil {
		t.Fatal(err)
	}
	h.c.settings = nil
	if code := h.run("--config", other, "--no-tools", "run", "hi"); code == 0 {
		t.Error("a configuration others can write was accepted")
	}
}

// An edit is applied to what starts next; one that fails a check is not,
// and the last good file stays.
func TestConfigReloads(t *testing.T) {
	h := newHarness(t, "")
	h.c.configEvery = 20 * time.Millisecond
	path := writeConfig(t, h.dir, "model: one\nprofiles:\n  w: {model: a}\n")
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		rewrite := func(body string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		rewrite("model: two\nprofiles:\n  w: {model: b}\n")
		waitFor(t, func() bool { return h.c.conf().Model == "two" && h.c.conf().Profiles["w"].Model == "b" })

		for _, bad := range []string{"model: three\nmodle: x\n", "model: three\ninstructions: missing.md\n"} {
			rewrite(bad)
			waitFor(t, func() bool { return strings.Contains(h.c.errOutText(), "not applied") })
			if h.c.conf().Model != "two" {
				t.Fatalf("a refused edit was applied: %q", h.c.conf().Model)
			}
			h.c.resetErrOut()
		}
		if runtime.GOOS != "windows" {
			rewrite("model: four\n")
			waitFor(t, func() bool { return h.c.conf().Model == "four" })
			if err := os.Chmod(path, 0o666); err != nil {
				t.Fatal(err)
			}
			rewrite("model: five\n")
			waitFor(t, func() bool { return strings.Contains(h.c.errOutText(), "not applied") })
			if h.c.conf().Model != "four" {
				t.Fatal("an edit to a file others can write was applied")
			}
		}
		return nil
	})
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not in 5s")
}

// A run of serve takes its profile from the file as it is now.
func TestServedRoleIsReadAtEachRun(t *testing.T) {
	h := newHarness(t, "")
	h.c.settings = &settings{}
	h.c.settings.current.Store(&config{Profiles: map[string]profile{"w": {Model: "a"}}})
	if r, err := h.c.servedRole("w"); err != nil || r.Model != "a" {
		t.Fatalf("%+v %v", r, err)
	}
	h.c.settings.current.Store(&config{Profiles: map[string]profile{"w": {Model: "b"}}})
	if r, err := h.c.servedRole("w"); err != nil || r.Model != "b" {
		t.Fatalf("the edit did not reach the run: %+v %v", r, err)
	}
	h.c.settings.current.Store(&config{})
	if _, err := h.c.servedRole("w"); err == nil {
		t.Fatal("a profile taken out of the file still ran")
	}
	if r, err := h.c.servedRole("default"); err != nil || r != nil {
		t.Fatalf("default: %+v %v", r, err)
	}
}

func TestWhatNeedsARestart(t *testing.T) {
	old := &config{Profiles: map[string]profile{"w": {}}, Serve: serveConfig{Listen: "a"}}
	if got := restartFor(old, &config{Profiles: map[string]profile{"w": {Model: "x"}}, Serve: serveConfig{Listen: "a"}, Model: "m"}); got != "" {
		t.Errorf("an edit applied at once was told to need a restart: %q", got)
	}
	people := &config{Profiles: map[string]profile{"w": {}}, Serve: serveConfig{Listen: "a", MemberProfile: "w", Telegram: telegramConfig{Owners: []string{"1"}, Members: []string{"2"}}}}
	if got := restartFor(old, people); got != "" {
		t.Errorf("who the chats talk with was told to need a restart: %q", got)
	}
	got := restartFor(old, &config{Profiles: map[string]profile{"v": {}}, Serve: serveConfig{Listen: "b"}, MCPServers: map[string]mcpEntry{"f": {}}})
	for _, want := range []string{"mcp_servers", "serve", "the list of profiles"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not name %s", got, want)
		}
	}
}

func TestTheConfigurationIsProtected(t *testing.T) {
	h := newHarness(t, "")
	writeConfig(t, h.dir, fullConfig)
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		got := strings.Join(h.c.protectedFiles(ctx), "\n")
		root := filepath.Join(h.dir, "ringil")
		for _, want := range []string{"ringil.yaml", "company.md", "support.md"} {
			if !strings.Contains(got, filepath.Join(root, want)) {
				t.Errorf("%s is not protected", want)
			}
		}
		return nil
	})
}

// Audit 20/11: relative paths of the file, from the file's directory.
func TestRelativePathsAreFromTheFile(t *testing.T) {
	f := (config{SessionsDir: "sess", Instructions: "/abs.md"}).from("/etc/ringil")
	if f.SessionsDir != filepath.Join("/etc/ringil", "sess") || f.Instructions != "/abs.md" {
		t.Errorf("%q %q", f.SessionsDir, f.Instructions)
	}
}

// Audit 20/11: what needs a restart is told against what serve started
// with: an edit taken back is no change.
func TestRestartIsToldAgainstTheStart(t *testing.T) {
	h := newHarness(t, "")
	h.c.configEvery = 20 * time.Millisecond
	path := writeConfig(t, h.dir, "serve:\n  listen: 127.0.0.1:1\n")
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		write := func(body string) {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		write("serve:\n  listen: 127.0.0.1:2\n")
		waitFor(t, func() bool { return strings.Contains(h.c.errOutText(), "next start") })
		h.c.resetErrOut()
		write("serve:\n  listen: 127.0.0.1:1\n")
		waitFor(t, func() bool { return strings.Contains(h.c.errOutText(), "applied") })
		if strings.Contains(h.c.errOutText(), "next start") {
			t.Error("an edit taken back was told to need a restart")
		}
		return nil
	})
}

// Audit 20/11: a chat profile serve does not offer yet is refused at a
// reload, rather than failing at every message.
func TestAChatProfileMustBeServed(t *testing.T) {
	s := &settings{path: writeConfig(t, t.TempDir(), "x: 1\n")}
	s.served.Store(&map[string]bool{"default": true, "team": true})
	team := map[string]profile{"team": {Tools: &[]string{}, Memory: "chat"}, "new": {Tools: &[]string{}, Memory: "chat"}}
	if err := s.acceptable(&config{Profiles: team, Serve: serveConfig{MemberProfile: "team"}}); err != nil {
		t.Errorf("a served profile: %v", err)
	}
	if err := s.acceptable(&config{Profiles: team, Serve: serveConfig{PublicProfile: "new"}}); err == nil {
		t.Error("a profile not served was given to the chats")
	}
}

// Audit 20/11: no_approvals reaches the chats at a reload; the flag wins.
func TestNoApprovalsFollowsTheFile(t *testing.T) {
	on := serveConfig{}.people(true)
	if !on.NoApprovals || (serveConfig{}).people(false).NoApprovals {
		t.Fatal("people does not carry no_approvals")
	}
	h := newHarness(t, "")
	writeConfig(t, h.dir, "no_approvals: true\n")
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		if !h.c.settings.noApprovals(&config{NoApprovals: true}) || h.c.settings.noApprovals(&config{}) {
			t.Error("the file's no_approvals is not followed")
		}
		return nil
	})
	h = newHarness(t, "")
	writeConfig(t, h.dir, "no_approvals: true\n")
	loaded(t, h.c, []string{"--no-approvals=false"}, func(*orpheus.Context) error {
		if h.c.settings.noApprovals(&config{NoApprovals: true}) {
			t.Error("the flag did not win over the file")
		}
		return nil
	})
}

func TestBadMCPEntriesAreRefused(t *testing.T) {
	f := &config{MCPServers: map[string]mcpEntry{"x": {Type: "ws", URL: "https://x"}}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "mcp_servers.x") {
		t.Errorf("%v", err)
	}
}

func (c *cli) errOutText() string {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return c.errOut.(*bytes.Buffer).String()
}

func (c *cli) resetErrOut() {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.errOut.(*bytes.Buffer).Reset()
}

// The example in docs is the schema: it binds strictly and passes Validate.
func TestTheExampleIsValid(t *testing.T) {
	watched, err := argus.Setup("ringil").File(filepath.Join("..", "..", "docs", "ringil.example.yaml")).Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watched.Close() }()
	b, err := argus.Bind[config](watched, argus.Strict())
	if err != nil {
		t.Fatal(err)
	}
	f := b.Value()
	if f.Profiles["support"].Tools == nil || f.MCPServers["crm"].OAuth == nil || f.Serve.Telegram.Owners[0] != "623292473" {
		t.Errorf("example %+v", f)
	}
}

// Instructions and skills are read with the file, as one revision: an
// edit to any of them is a new revision with its own digest, and a file
// named only by an edit is followed from then on.
func TestInstructionsAndSkillsAreOfTheRevision(t *testing.T) {
	h := newHarness(t, "")
	h.c.configEvery = 20 * time.Millisecond
	path := writeConfig(t, h.dir, fullConfig)
	root := filepath.Dir(path)
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		f := h.c.conf()
		if f.texts[filepath.Join(root, "company.md")] != "We are Acme." || f.digest == "" {
			t.Fatalf("texts %v digest %q", f.texts, f.digest)
		}
		first := f.digest

		write(filepath.Join(root, "company.md"), "We are Acme, since 1990.")
		waitFor(t, func() bool { return h.c.conf().texts[filepath.Join(root, "company.md")] == "We are Acme, since 1990." })
		second := h.c.conf().digest
		if second == first {
			t.Error("an instructions edit kept the digest")
		}

		write(filepath.Join(root, "skills", "greet", "SKILL.md"), "---\nname: greet\ndescription: d\n---\nSay hi.\n")
		waitFor(t, func() bool { return h.c.conf().digest != second })

		write(filepath.Join(root, "sales.md"), "Sell.")
		write(path, fullConfig+"\n  # x\n")
		body := strings.Replace(fullConfig, "profiles:\n", "profiles:\n  sales:\n    instructions: sales.md\n", 1)
		write(path, body)
		waitFor(t, func() bool { return h.c.conf().texts[filepath.Join(root, "sales.md")] == "Sell." })
		write(filepath.Join(root, "sales.md"), "Sell more.")
		waitFor(t, func() bool { return h.c.conf().texts[filepath.Join(root, "sales.md")] == "Sell more." })
		return nil
	})
}

// Every revision goes to the audit file, by name and hash, never the
// text of the instructions.
func TestRevisionsAreAudited(t *testing.T) {
	h := newHarness(t, "")
	h.c.configEvery = 20 * time.Millisecond
	path := writeConfig(t, h.dir, fullConfig)
	audit := filepath.Join(filepath.Dir(path), "audit.jsonl")
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), "company.md"), []byte("A secret plan."), 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			return strings.Contains(h.c.conf().texts[filepath.Join(filepath.Dir(path), "company.md")], "secret")
		})
		return nil
	})
	data, err := os.ReadFile(audit)
	if err != nil || !strings.Contains(string(data), "company") {
		t.Fatalf("audit %q %v", data, err)
	}
	if strings.Contains(string(data), "secret plan") || strings.Contains(string(data), "We are Acme") {
		t.Error("the audit holds the instructions' text")
	}
}

func TestNoAudit(t *testing.T) {
	h := newHarness(t, "")
	path := writeConfig(t, h.dir, "no_audit: true\n")
	loaded(t, h.c, nil, func(*orpheus.Context) error { return nil })
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "audit.jsonl")); err == nil {
		t.Error("an audit file with no_audit")
	}
}

// A run of serve reads one revision from start to end.
func TestARunIsPinnedToARevision(t *testing.T) {
	s := &settings{}
	s.current.Store(&config{Model: "a", digest: "1"})
	p := s.pinned()
	s.current.Store(&config{Model: "b", digest: "2"})
	if p.get().Model != "a" {
		t.Error("a pinned run saw the next revision")
	}
}

func TestPatternsAreNotInstructionsPaths(t *testing.T) {
	for _, f := range []*config{{Instructions: "a*.md"}, {Profiles: map[string]profile{"w": {Instructions: "x[1].md"}}}} {
		if err := f.Validate(); err == nil {
			t.Errorf("accepted %+v", f)
		}
	}
	if err := (&config{Speech: audioConfig{KeyEnv: "no spaces"}}).Validate(); err == nil {
		t.Error("a bad speech key_env")
	}
}

// Audit 20/11 (revisions): a refusal whose reason goes away is applied
// without another edit: an instructions file named before it existed, a
// file made private again.
func TestARefusalIsRetried(t *testing.T) {
	h := newHarness(t, "")
	h.c.configEvery = 20 * time.Millisecond
	path := writeConfig(t, h.dir, "instructions: company.md\n")
	root := filepath.Dir(path)
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		if err := os.WriteFile(path, []byte("instructions: later.md\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return strings.Contains(h.c.errOutText(), "not applied") })
		if err := os.WriteFile(filepath.Join(root, "later.md"), []byte("Later."), 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return h.c.conf().texts[filepath.Join(root, "later.md")] == "Later." })
		if runtime.GOOS == "windows" {
			return nil
		}
		later := filepath.Join(root, "later.md")
		if err := os.WriteFile(later, []byte("Shared."), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(later, 0o666); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return strings.Contains(h.c.errOutText(), "not writable by others") })
		if h.c.conf().texts[later] != "Later." {
			t.Fatal("instructions others can write were applied")
		}
		if err := os.Chmod(later, 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return h.c.conf().texts[later] == "Shared." })
		return nil
	})
}

// Audit 20/11 (revisions): --instructions is a path, not a pattern, and
// works without a user config directory.
func TestTheInstructionsFlag(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "x[1].md")
	if err := os.WriteFile(odd, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "")
	app := h.c.app()
	app.AddCommand(orpheus.NewCommand("probe", "p").SetHandler(h.c.withConfig(func(*orpheus.Context) error { return nil })))
	if err := app.RunContext(t.Context(), []string{"--instructions", odd, "probe"}); err == nil {
		t.Error("a pattern was taken as the instructions' path")
	}

	plain := filepath.Join(dir, "i.md")
	if err := os.WriteFile(plain, []byte("Be brief."), 0o600); err != nil {
		t.Fatal(err)
	}
	h = newHarness(t, "")
	h.c.configDir = func() (string, error) { return "", os.ErrNotExist }
	loaded(t, h.c, []string{"--instructions", plain}, func(ctx *orpheus.Context) error {
		if got, err := h.c.instructions(ctx); err != nil || got != "Be brief." {
			t.Errorf("without a config directory: %q %v", got, err)
		}
		return nil
	})
}

func TestMemoryStoreChecked(t *testing.T) {
	for _, m := range []memoryStore{{Kind: "redis"}, {Kind: "chroma"}, {Kind: "chroma", URL: "http://x", Prefix: "-bad"}, {Kind: "mem0"}, {Raw: true}, {Kind: "chroma", URL: "http://x", Raw: true}} {
		if err := (&config{Memory: m}).Validate(); err == nil {
			t.Errorf("accepted %+v", m)
		}
	}
	if err := (&config{Memory: memoryStore{Kind: "chroma", URL: "http://127.0.0.1:8000", Prefix: "acme.it"}}).Validate(); err != nil {
		t.Errorf("a good store: %v", err)
	}
}

// Each memory in a collection of its own: the user's, and a chat's.
func TestChromaCollections(t *testing.T) {
	var names []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name     string         `json:"name"`
			Metadata map[string]any `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		names = append(names, body.Name)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "00000000-0000-4000-8000-000000000001", "metadata": body.Metadata,
			"configuration_json": map[string]any{"hnsw": map[string]any{"space": "cosine"}}})
	}))
	defer srv.Close()
	h := newHarness(t, "")
	h.c.newEmbedder = func(cfg openai.Config) (agent.Embedder, error) { return fakeEmbedder{cfg.Model}, nil }
	writeConfig(t, h.dir, "embed_model: emb\nmemory_store:\n  kind: chroma\n  url: "+srv.URL+"\n  collection_prefix: acme\n")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		if _, err := h.c.memory(ctx); err != nil {
			t.Fatal(err)
		}
		r := h.c.forRun()
		r.settings = h.c.settings
		r.role, r.runConversation = &profile{Memory: "chat"}, "telegram-42_3"
		if _, err := r.memory(ctx); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if strings.Join(names, " ") != "acme-user acme-chat-telegram-42" {
		t.Fatalf("collections %v", names)
	}
}

// Mem0 keeps each memory under a user_id of its own, and needs no
// embedder of ours.
func TestMem0Users(t *testing.T) {
	var users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		users = append(users, r.URL.Query().Get("user_id"))
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()
	h := newHarness(t, "")
	writeConfig(t, h.dir, "memory_store:\n  kind: mem0\n  url: "+srv.URL+"\n  collection_prefix: acme\n")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		if _, err := h.c.memory(ctx); err != nil {
			t.Fatal(err)
		}
		r := h.c.forRun()
		r.settings = h.c.settings
		r.role, r.runConversation = &profile{Memory: "chat"}, "telegram-42_3"
		if _, err := r.memory(ctx); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if strings.Join(users, " ") != "acme-user acme-chat-telegram-42" {
		t.Fatalf("users %q", users)
	}
	long := chatCollection(strings.Repeat("p", 64), strings.Repeat("c", 64), mem0.ValidUser)
	if !mem0.ValidUser(long) { // the longest prefix and chat: hashed to fit
		t.Fatalf("%q", long)
	}
}

func TestChromaNeedsAnEmbedder(t *testing.T) {
	h := newHarness(t, "")
	writeConfig(t, h.dir, "memory_store:\n  kind: chroma\n  url: http://127.0.0.1:1\n")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		if _, err := h.c.memory(ctx); err == nil || !strings.Contains(err.Error(), "embed_model") {
			t.Errorf("chroma without an embedder: %v", err)
		}
		return nil
	})
}

type fakeEmbedder struct{ model string }

func (e fakeEmbedder) Model() string { return e.model }

func (e fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestOllamaEmbedder(t *testing.T) {
	for _, e := range []embedderConfig{{Kind: "cohere"}, {Kind: "ollama"}, {Kind: "ollama", Model: "m", Dimensions: -1}} {
		if err := (&config{Embedder: e}).Validate(); err == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	h := newHarness(t, "")
	writeConfig(t, h.dir, "embedder:\n  kind: ollama\n  model: qwen3-embedding:0.6b\n  document_prefix: \"doc: \"\n")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		e, err := h.c.embedder(ctx)
		if err != nil || e == nil || e.Model() != "qwen3-embedding:0.6b#doc: " {
			t.Fatalf("%v %v", e, err)
		}
		return nil
	})
}

// Audit 20/11: a chat whose name Chroma would refuse still has a collection.
func TestChatCollectionNames(t *testing.T) {
	if got := chatCollection("acme", "telegram-42", chroma.ValidCollection); got != "acme-chat-telegram-42" {
		t.Errorf("%q", got)
	}
	for _, chat := range []string{"support-", "team_x_", "a..b"} {
		if got := chatCollection("acme", chat, chroma.ValidCollection); !chroma.ValidCollection(got) || !strings.HasPrefix(got, "acme-chat-") {
			t.Errorf("%s: %q", chat, got)
		}
	}
	if chatCollection("acme", "support-", chroma.ValidCollection) == chatCollection("acme", "support_", chroma.ValidCollection) {
		t.Error("two chats share a collection")
	}
}
