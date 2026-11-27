// providers_test.go: which API and which key each model gets
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"

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

// A provider is chosen by name; each sends its own key to its own
// endpoint and no other (found live 21/11: the Anthropic key went to the
// OpenAI-compatible router).
func TestAnthropicMainModel(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ciao"))
	cfg, key := claude(h)
	env(h, map[string]string{"RINGIL_MODEL": "claude-haiku-5-5", "OPENAI_API_KEY": "main", "ANTHROPIC_API_KEY": "sk-ant"})
	if code := h.run("--provider", "anthropic", "--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if cfg.Model != "claude-haiku-5-5" || cfg.BaseURL != "" || *key != "sk-ant" {
		t.Fatalf("config %+v, key %q", *cfg, *key)
	}
	h = newHarness(t, "", fake.Reply("ciao"))
	cfg, key = claude(h)
	env(h, map[string]string{"RINGIL_PROVIDER": "anthropic", "RINGIL_MODEL": "m", "MY_KEY": "mine", "ANTHROPIC_API_KEY": "default"})
	writeProfiles(t, h.dir, `{"providers":{"anthropic":{"base":"https://gw.example.com","key_env":"MY_KEY"}}}`)
	if code := h.run("--no-tools", "run", "hi"); code != exitOK || *key != "mine" || cfg.BaseURL != "https://gw.example.com" {
		t.Fatalf("exit %d, key %q, base %q: %s", code, *key, cfg.BaseURL, h.err)
	}
}

// Every built-in provider resolves to its documented endpoint and key;
// an entry of ringil.yaml adds one, or changes a built-in's base or key.
func TestBuiltinAndOwnProviders(t *testing.T) {
	f := &config{Providers: map[string]providerEntry{
		"vllm":     {Base: "https://vllm.lan/v1", KeyEnv: "VLLM_KEY"},
		"gateway":  {Kind: providerAnthropic, Base: "https://gw.example.com"},
		"open":     {Base: "http://127.0.0.1:8000/v1"},
		"deepseek": {KeyEnv: "TEAM_DEEPSEEK"},
	}}
	for name, want := range map[string]endpoint{
		"google":   {"google", providerOpenAI, "https://generativelanguage.googleapis.com/v1beta/openai", "GEMINI_API_KEY"},
		"moonshot": {"moonshot", providerOpenAI, "https://api.moonshot.ai/v1", "MOONSHOT_API_KEY"},
		"xai":      {"xai", providerOpenAI, "https://api.x.ai/v1", "XAI_API_KEY"},
		"mistral":  {"mistral", providerOpenAI, "https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
		"ollama":   {"ollama", providerOpenAI, ollamaBase, ""},
		"chatgpt":  {"chatgpt", providerChatGPT, "", ""},
		"vllm":     {"vllm", providerOpenAI, "https://vllm.lan/v1", "VLLM_KEY"},
		"gateway":  {"gateway", providerAnthropic, "https://gw.example.com", ""},
		"open":     {"open", providerOpenAI, "http://127.0.0.1:8000/v1", ""},
		"deepseek": {"deepseek", providerOpenAI, "https://api.deepseek.com", "TEAM_DEEPSEEK"},
	} {
		if got, err := f.endpointNamed(name); err != nil || got != want {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.endpointNamed("nowhere"); err == nil {
		t.Fatal("an unknown provider resolved")
	}
}

// Four OpenAI-compatible providers side by side: the main model on one,
// a role on another, each with its own key.
func TestManyCompatibleProviders(t *testing.T) {
	for role, want := range map[string]string{"": "https://a.test/v1 m a", "coder": "https://api.deepseek.com deepseek-chat ds",
		"glm": "https://api.z.ai/api/paas/v4 glm z", "other": "https://b.test/v1 x b"} {
		h := newHarness(t, "", fake.Reply("ok"))
		env(h, map[string]string{"A_KEY": "a", "B_KEY": "b", "DEEPSEEK_API_KEY": "ds", "ZAI_API_KEY": "z"})
		writeProfiles(t, h.dir, `{"models":["a:m"],"providers":{"a":{"base":"https://a.test/v1","key_env":"A_KEY"},"b":{"base":"https://b.test/v1","key_env":"B_KEY"}},
			"profiles":{"coder":{"models":["deepseek:deepseek-chat"]},"glm":{"models":["zai:glm"]},"other":{"models":["b:x"]}}}`)
		args := []string{"--no-tools", "run", "hi"}
		if role != "" {
			args = append([]string{"--profile", role}, args...)
		}
		if code := h.run(args...); code != exitOK {
			t.Fatalf("%s: exit %d: %s", role, code, h.err)
		}
		if got := h.config.BaseURL + " " + h.config.Model + " " + h.key(t); got != want {
			t.Errorf("%s: %q, want %q", role, got, want)
		}
	}
}

// A role on Anthropic, the main model elsewhere: Anthropic's endpoint and
// key, never the main ones.
func TestAnthropicRole(t *testing.T) {
	for body, want := range map[string]string{
		`{"profiles":{"claude":{"models":["anthropic:claude-haiku-5-5"]}}}`:                                                                        " claude-haiku-5-5 sk-ant",
		`{"providers":{"team":{"kind":"anthropic","key_env":"TEAM_KEY"}},"profiles":{"claude":{"models":["team:claude-haiku-5-5"]}}}`:              " claude-haiku-5-5 team",
		`{"providers":{"gw":{"kind":"anthropic","base":"https://gw.example.com","key_env":"TEAM_KEY"}},"profiles":{"claude":{"models":["gw:c"]}}}`: "https://gw.example.com c team",
	} {
		h := newHarness(t, "", fake.Reply("ok"))
		cfg, key := claude(h)
		env(h, map[string]string{"RINGIL_PROVIDER": "openrouter", "RINGIL_MODEL": "m", "OPENROUTER_API_KEY": "main",
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
	writeProfiles(t, h.dir, `{"profiles":{"same":{"models":["openai:other"]}}}`)
	if code := h.run("--profile", "same", "--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.config.BaseURL != "https://api.openai.com/v1" || h.config.Model != "other" || h.key(t) != "k" {
		t.Fatalf("config %+v", h.config)
	}
}

func TestProvidersChecked(t *testing.T) {
	for body, want := range map[string]string{
		`{"models":["gemini:m"]}`:                                                            "no provider gemini",
		`{"profiles":{"x":{"models":["cohere:m"]}}}`:                                         "no provider cohere",
		`{"profiles":{"x":{"models":["anthropic:m"],"embed_model":"e"}}}`:                    "anthropic speaks anthropic, which has no embedding",
		`{"profiles":{"x":{"models":["chatgpt:m"],"embed_model":"e"}}}`:                      "chatgpt speaks chatgpt, which has no embedding",
		`{"base":"https://x.test/v1"}`:                                                       "base and key_env are gone: name a provider",
		`{"profiles":{"x":{"key_env":"K"}}}`:                                                 "profiles.x: base and key_env are gone",
		`{"speech":{"model":"t","base":"https://x.test/v1"}}`:                                "speech: base and key_env are gone",
		`{"providers":{"gemini":{"base":"https://gemini/v1","key_env":"GEMINI_API_KEY"}}}`:   "is a built-in provider's key",
		`{"providers":{"mine":{"base":"https://mine/v1","key_env":"RINGIL_API_KEY"}}}`:       "is a built-in provider's key",
		`{"providers":{"openrouter":{"base":"https://gw.internal/v1"}}}`:                     "another base needs its key_env",
		`{"providers":{"chatgpt":{"base":"https://other.example/v1"}}}`:                      "at its own address and with no key",
		`{"providers":{"openrouter":{"key_env":"OPENAI_API_KEY"}}}`:                          "is another provider's key",
		`{"providers":{"ollama":{"key_env":"OPENAI_API_KEY"}}}`:                              "is another provider's key",
		`{"providers":{"google":{"base":"https://x.test/v1","key_env":"DEEPSEEK_API_KEY"}}}`: "is another provider's key",
		`{"providers":{"mine":{"base":"https://x.test/v1","key_env":"openai_api_key"}}}`:     "is a built-in provider's key",
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

// RINGIL_EMBED_MODEL is for an OpenAI-compatible provider: on Anthropic
// the memory works by words, the run goes on (found live 21/11: it
// stopped).
func TestAnthropicIgnoresTheOtherEndpointsEmbedModel(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_PROVIDER": "anthropic", "RINGIL_MODEL": "m", "ANTHROPIC_API_KEY": "k", "RINGIL_EMBED_MODEL": "openai/emb"})
	claude(h)
	h.c.newEmbedder = func(openai.Config) (agent.Embedder, error) { t.Fatal("an embedder at Anthropic"); return nil, nil }
	if code := h.run("--no-tools", "run", "hi"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
}

// Audit 23/11: a role on its own provider runs without a main one.
func TestRoleWithoutAMainProvider(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"LAN_KEY": "lk"})
	writeProfiles(t, h.dir, `{"providers":{"lan":{"base":"http://localhost:9/v1","key_env":"LAN_KEY"}},"profiles":{"local":{"models":["lan:x"]}}}`)
	if code := h.run("--profile", "local", "--no-tools", "run", "hi"); code != exitOK || h.config.BaseURL != "http://localhost:9/v1" || h.key(t) != "lk" {
		t.Fatalf("exit %d, %+v: %s", code, h.config, h.err)
	}
}

// Each audio model gets its own provider's endpoint and key: the main
// one's, or the one it names (audit 23/11: no test held it).
func TestAudioKeysGoToTheirProviders(t *testing.T) {
	for _, tc := range []struct {
		body, envProvider, want string
	}{
		{`{"models":["a:m"],"providers":{"a":{"base":"https://a.test/v1","key_env":"A_KEY"}},"speech":{"model":"tts"}}`, "", "https://a.test/v1 tts a"},
		{`{"models":["a:m"],"providers":{"a":{"base":"https://a.test/v1","key_env":"A_KEY"}},"speech":{"model":"tts","provider":"openai"}}`, "", "https://api.openai.com/v1 tts oai"},
		{`{"models":["a:m"],"providers":{"a":{"base":"https://a.test/v1","key_env":"A_KEY"}},"speech":{"model":"tts"}}`, "openai", "https://api.openai.com/v1 tts oai"},
		{`{"models":["anthropic:m"],"speech":{"model":"tts","provider":"openai"}}`, "", "https://api.openai.com/v1 tts oai"},
	} {
		h := newHarness(t, "")
		env(h, map[string]string{"A_KEY": "a", "OPENAI_API_KEY": "oai", "ANTHROPIC_API_KEY": "ant", "RINGIL_SPEECH_PROVIDER": tc.envProvider})
		writeProfiles(t, h.dir, tc.body)
		loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
			res, err := h.c.resolver(ctx)
			if err != nil {
				return err
			}
			cfg, err := h.c.audioConfig(ctx, res, "RINGIL_SPEECH", h.c.conf().Speech)
			if err != nil {
				return err
			}
			k, _ := cfg.KeyFunc(context.Background())
			if got := cfg.BaseURL + " " + cfg.Model + " " + k; got != tc.want {
				t.Errorf("%s %q: %q, want %q", tc.body, tc.envProvider, got, tc.want)
			}
			return nil
		})
	}
	h := newHarness(t, "")
	env(h, map[string]string{"ANTHROPIC_API_KEY": "ant"})
	writeProfiles(t, h.dir, `{"models":["anthropic:m"],"speech":{"model":"tts"}}`)
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		res, _ := h.c.resolver(ctx)
		if _, err := h.c.audioConfig(ctx, res, "RINGIL_SPEECH", h.c.conf().Speech); err == nil {
			t.Error("speech at Anthropic, with its key")
		}
		return nil
	})
}
