// fallback_test.go: other models when the main one does not answer
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"strings"
	"testing"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

// models gives each model its own script, and records the endpoints asked.
func models(h *harness, scripts map[string]fake.Script) *[]openai.Config {
	var asked []openai.Config
	h.c.newProvider = func(cfg openai.Config) (agent.Provider, error) {
		asked = append(asked, cfg)
		return scripts[cfg.Model], nil
	}
	return &asked
}

var unavailable = errors.New(agent.ErrProviderUnavailable, "down").AsRetryable()

// The main model down: the next of the list answers at once, and the
// terminal says so.
func TestFallbackAnswers(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "DEEPSEEK_API_KEY": "dk"})
	main := fake.NewWithInfo(agent.ModelInfo{Name: "m"}, fake.Fail(unavailable))
	other := fake.NewWithInfo(agent.ModelInfo{Name: "deepseek-chat"}, fake.Reply("from deepseek"))
	asked := models(h, map[string]fake.Script{"m": main, "deepseek-chat": other})
	if code := h.run("--models", "openai:m,deepseek:deepseek-chat", "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.out.String() != "from deepseek" || !strings.Contains(h.err.String(), "* m did not answer") || !strings.Contains(h.err.String(), "trying deepseek-chat") {
		t.Fatalf("out %q err %q", h.out, h.err)
	}
	if len(*asked) != 2 || (*asked)[1].BaseURL != "https://api.deepseek.com" || (*asked)[1].KeyFunc == nil {
		t.Fatalf("endpoints %+v", *asked)
	}
}

// Without a fallback, or with only models it cannot reach, the main
// model is alone: nothing changes.
func TestFallbackOnlyWhenReachable(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k"})
	asked := models(h, map[string]fake.Script{"m": fake.New(fake.Reply("ok"))})
	if code := h.run("--models", "openai:m,deepseek:deepseek-chat", "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if len(*asked) != 1 || !strings.Contains(h.err.String(), "no key for deepseek, left out") {
		t.Fatalf("%d endpoints, err %q", len(*asked), h.err)
	}
}

// A request the model refuses is not sent to another.
func TestFallbackKeepsRequestErrors(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "DEEPSEEK_API_KEY": "dk"})
	other := fake.New(fake.Reply("never"))
	models(h, map[string]fake.Script{"m": fake.New(fake.Fail(errors.New(agent.ErrProviderInput, "bad"))), "d": other})
	if code := h.run("--models", "openai:m,deepseek:d", "run", "x"); code == exitOK || other.Remaining() != 1 {
		t.Fatalf("exit %d, the request went to the fallback", code)
	}
}

func TestModelsChecked(t *testing.T) {
	for _, f := range []*config{
		{Models: []string{"no-colon"}},
		{Models: []string{"openai:"}},
		{Models: []string{"nowhere:m"}},
		{Profiles: map[string]profile{"p": {Models: []string{"nowhere:m"}}}},
		{OldModel: "m"},
		{Profiles: map[string]profile{"p": {OldProvider: "openai"}}},
	} {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
	m, err := parseModel(" ollama:qwen3:8b ")
	if err != nil || m.provider != "ollama" || m.model != "qwen3:8b" {
		t.Fatalf("%+v %v", m, err)
	}
}

// A role with its own model sends its conversations nowhere else unless
// it lists its own fallback.
func TestFallbackOfARole(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "DEEPSEEK_API_KEY": "dk"})
	writeConfig(t, h.dir, "models: [openai:m, deepseek:d]\nprofiles:\n  p:\n    models: [openai:own]\n")
	asked := models(h, map[string]fake.Script{"own": fake.New(fake.Reply("ok")), "d": fake.New()})
	if code := h.run("--profile", "p", "--models", "openai:m,deepseek:d", "run", "x"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if len(*asked) != 1 || (*asked)[0].Model != "own" {
		t.Fatalf("the role's conversation reached %+v", *asked)
	}
}

// fileModel is the file's main model's name.
func fileModel(f *config) string {
	m, _ := first(f.Models)
	return m.model
}

// --provider and --model choose the main model; the list follows it,
// without it twice.
func TestMainModelRules(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		args []string
		want string
	}{
		{map[string]string{}, nil, "openai:m,deepseek:d"},
		{map[string]string{"RINGIL_MODEL": "other"}, nil, "openai:other,openai:m,deepseek:d"},
		{map[string]string{"RINGIL_PROVIDER": "openai"}, nil, "openai:m,deepseek:d"},
		{map[string]string{}, []string{"--provider", "deepseek", "--model", "d"}, "deepseek:d,openai:m"},
		{map[string]string{}, []string{"--provider", "deepseek"}, "error"},
	} {
		h := newHarness(t, "")
		vars := map[string]string{"OPENAI_API_KEY": "k", "DEEPSEEK_API_KEY": "dk"}
		for k, v := range tc.env {
			vars[k] = v
		}
		env(h, vars)
		writeConfig(t, h.dir, "models: [openai:m, deepseek:d]\n")
		var got string
		loaded(t, h.c, tc.args, func(ctx *orpheus.Context) error {
			res, err := h.c.resolver(ctx)
			if err != nil {
				return err
			}
			list, err := h.c.runModels(ctx, res)
			if err != nil {
				got = "error"
				return nil
			}
			names := make([]string, len(list))
			for i, m := range list {
				names[i] = m.String()
			}
			got = strings.Join(names, ",")
			return nil
		})
		if got != tc.want {
			t.Errorf("%v %v: %s, want %s", tc.env, tc.args, got, tc.want)
		}
	}
}

// The settings of before the list are refused, saying where they went.
func TestOldModelSettingsSayWhereTheyWent(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	writeConfig(t, h.dir, "model: m\nprovider: openai\n")
	if code := h.run("run", "x"); code == exitOK || !strings.Contains(h.err.String(), "models: [provider:model") {
		t.Fatalf("exit %d %q", code, h.err)
	}
}
