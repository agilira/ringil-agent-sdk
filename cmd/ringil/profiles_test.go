// profiles_test.go: roles with their own model, endpoint and prompt
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

// writeProfiles writes ringil.yaml (JSON is YAML) in the config dir.
func writeProfiles(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "ringil"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ringil", "ringil.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileOverridesModelAndPrompt(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"providers":{"writers":{"base":"http://localhost:2/v1","key_env":"WRITER_KEY"}},"profiles":{"writer":{"description":"writes","model":"big-model","provider":"writers","instructions":"writer.md"}}}`)
	if err := os.WriteFile(filepath.Join(h.dir, "ringil", "writer.md"), []byte("You write prose.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.c.getenv = func(k string) string {
		return map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "main", "WRITER_KEY": "wk"}[k]
	}
	if code := h.run("--profile", "writer", "--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.Model != "big-model" || h.config.BaseURL != "http://localhost:2/v1" || h.key(t) != "wk" {
		t.Fatalf("config %+v", h.config)
	}
	if sys := h.script.Requests()[0].System; !strings.HasPrefix(sys, agent.DefaultSystem) || !strings.HasSuffix(sys, "\n\nYou write prose.") {
		t.Fatalf("system %q", sys)
	}
}

// The operator's instructions follow the base prompt: --instructions for
// every role, then the role's own. The base stays, also with skills.
func TestInstructionsFollowTheBase(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	dir := filepath.Join(h.dir, "ringil")
	writeProfiles(t, h.dir, `{"profiles":{"support":{"instructions":"`+filepath.ToSlash(filepath.Join(dir, "support.md"))+`"}}}`)
	for name, text := range map[string]string{"company.md": "We are Acme.", "support.md": "Answer customers kindly."} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code := h.run("--instructions", filepath.Join(dir, "company.md"), "--profile", "support", "--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	sys := h.script.Requests()[0].System
	if !strings.HasPrefix(sys, agent.DefaultSystem) || !strings.HasSuffix(sys, "We are Acme.\n\nAnswer customers kindly.") {
		t.Fatalf("system %q", sys)
	}
}

func TestInstructionsRefused(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", agent.MaxInstructions+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{big, bad, filepath.Join(dir, "missing.md")} {
		h := newHarness(t, "", fake.Reply("ok"))
		if code := h.run("--instructions", path, "--no-tools", "run", "hi"); code == 0 {
			t.Errorf("%s accepted", filepath.Base(path))
		}
	}
}

func TestProfilesRefused(t *testing.T) {
	for _, body := range []string{
		`{"profiles":{"default":{}}}`,
		`{"profiles":{"bad name":{}}}`,
		`{"profiles":{"w":{"key_env":"K"}}}`,
		`{"profiles":{"w":{"provider":"nowhere"}}}`,
		`{"profiles":{"w":{"temperature":1}}}`,
		`{"profiles":{"w":{"max_turns":-1}}}`,
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		writeProfiles(t, h.dir, body)
		if code := h.run("--profile", "w", "--no-tools", "run", "hi"); code == 0 {
			t.Errorf("accepted %s", body)
		}
	}
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("--profile", "missing", "--no-tools", "run", "hi"); code == 0 || !strings.Contains(h.err.String(), "no profile missing") {
		t.Errorf("unknown profile: %d %s", code, h.err)
	}
}

// probe runs fn as a command with args, for checking settings directly.
func probe(t *testing.T, c *cli, args []string, fn func(ctx *orpheus.Context) error) {
	t.Helper()
	app := c.app()
	app.AddCommand(withWorkspace(orpheus.NewCommand("probe", "p").SetHandler(fn)))
	if err := app.RunContext(context.Background(), append(args, "probe")); err != nil {
		t.Fatal(err)
	}
}

// Audit 18/11 (1): a role with its own endpoint and no key got the main
// key. A provider without key_env gets none.
func TestRoleNeverGetsTheMainKey(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"providers":{"lan":{"base":"http://localhost:9/v1"}},"profiles":{"other":{"provider":"lan"}}}`)
	if code := h.run("--profile", "other", "--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.KeyFunc != nil || h.config.BaseURL != "http://localhost:9/v1" {
		t.Fatalf("base %s, a key is sent", h.config.BaseURL)
	}
}

// Audit 18/11 (2): memory embeddings went to the main endpoint with the
// main key. Under a role on another provider they use its endpoint, key
// and embed_model, or none.
func TestRoleEmbeddings(t *testing.T) {
	for body, want := range map[string]string{
		`{"providers":{"lan":{"base":"http://localhost:9/v1","key_env":"LOCAL_KEY"}},"profiles":{"local":{"provider":"lan","embed_model":"le"}}}`: "http://localhost:9/v1 le lk",
		`{"providers":{"lan":{"base":"http://localhost:9/v1","key_env":"LOCAL_KEY"}},"profiles":{"local":{"provider":"lan"}}}`:                    "none",
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		writeProfiles(t, h.dir, body)
		h.c.getenv = func(k string) string {
			return map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "main", "LOCAL_KEY": "lk", "RINGIL_EMBED_MODEL": "e"}[k]
		}
		got := "none"
		h.c.newEmbedder = func(cfg openai.Config) (agent.Embedder, error) {
			k, _ := cfg.KeyFunc(context.Background())
			got = cfg.BaseURL + " " + cfg.Model + " " + k
			return nil, os.ErrInvalid
		}
		h.run("--profile", "local", "run", "hi")
		if got != want {
			t.Errorf("%s: embedder %q, want %q", body, got, want)
		}
	}
}

// Audit 18/11 (3): under serve, --session gave every run one scratchpad.
func TestServedRunsHaveTheirOwnScratchpad(t *testing.T) {
	h := newHarness(t, "")
	probe(t, h.c, []string{"--session", "s1"}, func(ctx *orpheus.Context) error {
		a, b := h.c.forRun(), h.c.forRun()
		da, err := a.scratchDir(ctx, filepath.Join(h.dir, "ringil"))
		if err != nil {
			return err
		}
		db, err := b.scratchDir(ctx, filepath.Join(h.dir, "ringil"))
		if err != nil {
			return err
		}
		if da == db {
			t.Fatalf("shared %s", da)
		}
		a.shutdown()
		b.shutdown()
		return nil
	})
}

// Audit 18/11 (4): two serve starting at once on a fresh config: the one
// that lost the race failed.
func TestTokenRace(t *testing.T) {
	for range 50 {
		dir := t.TempDir()
		var wg sync.WaitGroup
		tokens := make([]string, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
				c.getenv = func(string) string { return "" }
				c.configDir = func() (string, error) { return dir, nil }
				tokens[i], _, errs[i] = c.keptServeToken()
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || tokens[0] != tokens[1] {
			t.Fatalf("%v %v same=%v", errs[0], errs[1], tokens[0] == tokens[1])
		}
	}
}

// Audit 18/11 (5): data after the configuration was accepted.
func TestProfilesTrailingData(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"profiles":{"w":{"model":"x"}}} {"garbage":true}`)
	if code := h.run("--profile", "w", "--no-tools", "run", "hi"); code == 0 {
		t.Fatal("trailing data accepted")
	}
}

func TestChatSystem(t *testing.T) {
	c := &cli{runConversation: "telegram-42"}
	if got := c.chatSystem(agent.DefaultSystem); !strings.HasPrefix(got, agent.DefaultSystem) || !strings.HasSuffix(got, channel.Style) {
		t.Fatalf("%q", got)
	}
	c.runConversation = "chat-1"
	if got := c.chatSystem("mine"); got != "mine" {
		t.Fatalf("%q", got)
	}
}
