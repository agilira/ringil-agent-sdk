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

func writeProfiles(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "ringil"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ringil", "profiles.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileOverridesModelAndPrompt(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"profiles":{"writer":{"description":"writes","model":"big-model","base":"http://localhost:2/v1","key_env":"WRITER_KEY","system":"You write prose."}}}`)
	h.c.getenv = func(k string) string {
		return map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "WRITER_KEY": "wk"}[k]
	}
	if code := h.run("--profile", "writer", "--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.Model != "big-model" || h.config.BaseURL != "http://localhost:2/v1" || h.key(t) != "wk" {
		t.Fatalf("config %+v", h.config)
	}
	if sys := h.script.Requests()[0].System; !strings.HasPrefix(sys, "You write prose.") {
		t.Fatalf("system %q", sys)
	}
}

func TestProfilesRefused(t *testing.T) {
	for _, body := range []string{
		`{"profiles":{"default":{}}}`,
		`{"profiles":{"bad name":{}}}`,
		`{"profiles":{"w":{"key_env":"no spaces allowed"}}}`,
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

// Audit 18/11 (1): a role with its own base and no key_env got the main
// key. It gets none; and key_env without base is refused.
func TestRoleNeverGetsTheMainKey(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"profiles":{"other":{"base":"http://localhost:9/v1"}}}`)
	if code := h.run("--profile", "other", "--no-tools", "run", "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.KeyFunc != nil || h.config.BaseURL != "http://localhost:9/v1" {
		t.Fatalf("base %s, a key is sent", h.config.BaseURL)
	}
	h = newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"profiles":{"other":{"key_env":"OTHER_KEY"}}}`)
	if code := h.run("--profile", "other", "--no-tools", "run", "hi"); code == 0 {
		t.Fatal("a role's key would go to the main endpoint")
	}
}

// Audit 18/11 (2): memory embeddings went to the main endpoint with the
// main key. Under a role with its own base they use its base, key and
// embed_model, or none.
func TestRoleEmbeddings(t *testing.T) {
	for body, want := range map[string]string{
		`{"profiles":{"local":{"base":"http://localhost:9/v1","key_env":"LOCAL_KEY","embed_model":"le"}}}`: "http://localhost:9/v1 le lk",
		`{"profiles":{"local":{"base":"http://localhost:9/v1","key_env":"LOCAL_KEY"}}}`:                    "none",
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		writeProfiles(t, h.dir, body)
		h.c.getenv = func(k string) string {
			return map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "RINGIL_API_KEY": "main", "LOCAL_KEY": "lk", "RINGIL_EMBED_MODEL": "e"}[k]
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
				tokens[i], _, errs[i] = c.serveToken()
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || tokens[0] != tokens[1] {
			t.Fatalf("%v %v same=%v", errs[0], errs[1], tokens[0] == tokens[1])
		}
	}
}

// Audit 18/11 (5): data after the profiles object was accepted.
func TestProfilesTrailingData(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeProfiles(t, h.dir, `{"profiles":{"w":{"model":"x"}}} {"garbage":true}`)
	if code := h.run("--profile", "w", "--no-tools", "run", "hi"); code == 0 {
		t.Fatal("trailing data accepted")
	}
}

func TestChatSystem(t *testing.T) {
	c := &cli{runConversation: "telegram-42"}
	if got := c.chatSystem(""); !strings.HasPrefix(got, agent.DefaultSystem) || !strings.HasSuffix(got, channel.Style) {
		t.Fatalf("%q", got)
	}
	c.runConversation = "chat-1"
	if got := c.chatSystem("mine"); got != "mine" {
		t.Fatalf("%q", got)
	}
}
