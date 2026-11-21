// providers_test.go: which API and which key each model gets
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/anthropic"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

// claude records the Anthropic config the CLI builds, and the key its
// KeyFunc reads.
func claude(h *harness) (*anthropic.Config, *string) {
	var got anthropic.Config
	key := "none"
	h.c.newAnthropic = func(cfg anthropic.Config) (agent.Provider, error) {
		got = cfg
		if cfg.KeyFunc != nil {
			key, _ = cfg.KeyFunc(context.Background())
		}
		return h.script, nil
	}
	return &got, &key
}

func env(h *harness, vars map[string]string) {
	h.c.getenv = func(k string) string { return vars[k] }
}

// provider: anthropic needs no base and reads ANTHROPIC_API_KEY; neither
// RINGIL_API_KEY goes to it nor its key to RINGIL_BASE_URL (found live
// 21/11: the Anthropic key went to the OpenAI-compatible router).
func TestAnthropicMainModel(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ciao"))
	cfg, key := claude(h)
	env(h, map[string]string{"RINGIL_BASE_URL": "https://router.example.com/v1", "RINGIL_MODEL": "claude-haiku-5-5", "RINGIL_API_KEY": "main", "ANTHROPIC_API_KEY": "sk-ant"})
	if code := h.run("--provider", "anthropic", "--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if cfg.Model != "claude-haiku-5-5" || cfg.BaseURL != "" || *key != "sk-ant" {
		t.Fatalf("config %+v, key %q", *cfg, *key)
	}
	h = newHarness(t, "", fake.Reply("ciao"))
	cfg, key = claude(h)
	env(h, map[string]string{"RINGIL_PROVIDER": "anthropic", "RINGIL_MODEL": "m", "MY_KEY": "mine"})
	if code := h.run("--key-env", "MY_KEY", "--no-tools", "run", "hi"); code != exitOK || *key != "mine" {
		t.Fatalf("exit %d, key %q: %s", code, *key, h.err)
	}
	_ = cfg
	h = newHarness(t, "", fake.Reply("ciao"))
	cfg, _ = claude(h)
	env(h, map[string]string{"RINGIL_BASE_URL": "https://router.example.com/v1", "RINGIL_MODEL": "m", "ANTHROPIC_API_KEY": "k"})
	if code := h.run("--provider", "anthropic", "--base", "https://gw.example.com", "--no-tools", "run", "hi"); code != exitOK || cfg.BaseURL != "https://gw.example.com" {
		t.Fatalf("exit %d, base %q: %s", code, cfg.BaseURL, h.err)
	}
}

// The file's base is the file's provider's: with provider anthropic from
// the environment, a base the file gives its OpenAI-compatible endpoint
// does not take the Anthropic key (audit 21/11).
func TestTheFilesBaseIsItsProviders(t *testing.T) {
	for body, want := range map[string]string{
		`{"base":"https://router.example.com/v1","key_env":"OPENAI_KEY"}`: " sk-ant",
		`{"provider":"anthropic","base":"https://gw.example.com"}`:        "https://gw.example.com sk-ant",
		`{"provider":"anthropic","key_env":"OPENAI_KEY"}`:                 " oai",
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		cfg, key := claude(h)
		env(h, map[string]string{"RINGIL_PROVIDER": "anthropic", "RINGIL_MODEL": "m", "ANTHROPIC_API_KEY": "sk-ant", "OPENAI_KEY": "oai"})
		writeProfiles(t, h.dir, body)
		h.run("--no-tools", "run", "hi")
		if got := cfg.BaseURL + " " + *key; got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

// A role on Anthropic, the main model elsewhere: its own endpoint, so it
// neither inherits the main base nor sends the main key.
func TestAnthropicRole(t *testing.T) {
	for body, want := range map[string]string{
		`{"profiles":{"claude":{"provider":"anthropic","model":"claude-haiku-5-5"}}}`:                      " claude-haiku-5-5 sk-ant",
		`{"profiles":{"claude":{"provider":"anthropic","model":"claude-haiku-5-5","key_env":"TEAM_KEY"}}}`: " claude-haiku-5-5 team",
		`{"profiles":{"claude":{"provider":"anthropic","base":"https://gw.example.com","model":"c"}}}`:     "https://gw.example.com c sk-ant",
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		cfg, key := claude(h)
		env(h, map[string]string{"RINGIL_BASE_URL": "https://router.example.com/v1", "RINGIL_MODEL": "m", "RINGIL_API_KEY": "main",
			"ANTHROPIC_API_KEY": "sk-ant", "TEAM_KEY": "team"})
		writeProfiles(t, h.dir, body)
		if code := h.run("--profile", "claude", "--no-tools", "run", "hi"); code != exitOK {
			t.Fatalf("%s: exit %d: %s", body, code, h.err)
		}
		if got := cfg.BaseURL + " " + cfg.Model + " " + *key; got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

// A role on the main provider stays on the main endpoint and key.
func TestRoleOnTheMainProvider(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_BASE_URL": "http://localhost:1/v1", "RINGIL_MODEL": "m", "RINGIL_API_KEY": "main"})
	writeProfiles(t, h.dir, `{"profiles":{"same":{"provider":"openai","model":"other"}}}`)
	if code := h.run("--profile", "same", "--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.BaseURL != "http://localhost:1/v1" || h.config.Model != "other" || h.key(t) != "main" {
		t.Fatalf("config %+v", h.config)
	}
}

func TestProvidersChecked(t *testing.T) {
	for body, want := range map[string]string{
		`{"provider":"gemini"}`:                                         "provider is openai or anthropic",
		`{"profiles":{"x":{"provider":"cohere"}}}`:                      "provider is openai or anthropic",
		`{"profiles":{"x":{"provider":"anthropic","embed_model":"e"}}}`: "no embedding models",
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		writeProfiles(t, h.dir, body)
		if code := h.run("--no-tools", "run", "hi"); code == exitOK || !strings.Contains(h.err.String(), want) {
			t.Errorf("%s: exit %d: %s", body, code, h.err)
		}
	}
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_PROVIDER": "anthropic", "RINGIL_MODEL": "m", "ANTHROPIC_API_KEY": "k"})
	claude(h)
	h.c.newEmbedder = func(openai.Config) (agent.Embedder, error) { t.Fatal("an embedder at Anthropic"); return nil, nil }
	if code := h.run("--embed-model", "e", "run", "hi"); code == exitOK || !strings.Contains(h.err.String(), "no embedding models") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}

// RINGIL_EMBED_MODEL is the OpenAI-compatible endpoint's: on Anthropic the
// memory works by words, the run goes on (found live 21/11: it stopped).
func TestAnthropicIgnoresTheOtherEndpointsEmbedModel(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_PROVIDER": "anthropic", "RINGIL_MODEL": "m", "ANTHROPIC_API_KEY": "k", "RINGIL_EMBED_MODEL": "openai/emb"})
	claude(h)
	h.c.newEmbedder = func(openai.Config) (agent.Embedder, error) { t.Fatal("an embedder at Anthropic"); return nil, nil }
	if code := h.run("--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}
