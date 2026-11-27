// appapi_test.go: the settings an app changes through serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

const settingsToken = "0123456789abcdef0123456789abcdef"

// settingsAPI runs fn with the settings API of a CLI loaded with body as
// ringil.yaml; call sends a request with the token and answers its code
// and body.
func settingsAPI(t *testing.T, body string, vars map[string]string, fn func(call func(method, path, body string) (int, string), path string)) {
	t.Helper()
	h := newHarness(t, "")
	env(h, vars)
	path := writeConfig(t, h.dir, body)
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		a, err := agent.New(agent.Config{Provider: fake.New()})
		if err != nil {
			t.Fatal(err)
		}
		srv, err := serve.New(serve.Config{Token: settingsToken, NoStore: true, Profiles: []serve.Profile{{Name: "default", New: serve.Fixed(a)}}})
		if err != nil {
			t.Fatal(err)
		}
		h.c.mountSettings(ctx, srv)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		call := func(method, p, b string) (int, string) {
			bare, anon := strings.CutPrefix(p, "!") // "!" asks without the token
			req, _ := http.NewRequest(method, ts.URL+bare, strings.NewReader(b))
			if !anon {
				req.Header.Set("Authorization", "Bearer "+settingsToken)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			out, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(out)
		}
		fn(call, path)
		return nil
	})
}

func fileHas(t *testing.T, path string, want ...string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Errorf("ringil.yaml lacks %q:\n%s", w, b)
		}
	}
}

// The settings say which keys are set, never their values.
func TestSettingsRead(t *testing.T) {
	settingsAPI(t, "models: [openai:m]\nmcp_servers:\n  files: {command: mcp-files, args: [--root, /srv]}\n",
		map[string]string{"OPENAI_API_KEY": "sk-the-secret-value"}, func(call func(string, string, string) (int, string), _ string) {
			code, out := call("GET", "/v1/settings", "")
			if code != http.StatusOK || strings.Contains(out, "sk-the-secret-value") {
				t.Fatalf("%d %s", code, out)
			}
			for _, want := range []string{`"models":["openai:m"]`, `"name":"openai"`, `"key_set":true`, `"name":"files"`, `"command":["mcp-files","--root","/srv"]`} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %s in %s", want, out)
				}
			}
			if !strings.Contains(out, `"name":"deepseek","kind":"openai","base":"https://api.deepseek.com","key_env":"DEEPSEEK_API_KEY","builtin":true,"key_set":false`) {
				t.Errorf("a provider without its key: %s", out)
			}
		})
}

// A change is written when the file with it passes every check; a wrong
// one leaves the file as it was.
func TestSettingsWrite(t *testing.T) {
	settingsAPI(t, "# my notes\nmodels: [openai:m]\n", map[string]string{"OPENAI_API_KEY": "k"}, func(call func(string, string, string) (int, string), path string) {
		for _, c := range []struct {
			method, path, body string
			code               int
		}{
			{"PUT", "/v1/settings/models", `{"models":["openai:big","ollama:qwen3:8b"]}`, http.StatusNoContent},
			{"PUT", "/v1/settings/models", `{"models":["nowhere:x"]}`, http.StatusBadRequest},
			{"PUT", "/v1/settings/limits", `{"max_cost":0.5,"workflow_cost":5}`, http.StatusNoContent},
			{"PUT", "/v1/settings/limits", `{"max_cost":-1}`, http.StatusBadRequest},
			{"PUT", "/v1/settings/providers/mine", `{"base":"https://llm.example.com/v1","key_env":"MINE_KEY"}`, http.StatusNoContent},
			{"PUT", "/v1/settings/providers/bad", `{"base":"file:///etc"}`, http.StatusBadRequest},
			{"PUT", "/v1/settings/models", `{"models":["openai:big"],"admin":true}`, http.StatusBadRequest},
			{"DELETE", "/v1/settings/providers/nope", "", http.StatusNotFound},
			{"DELETE", "/v1/settings/mcp_servers/nope", "", http.StatusNotFound},
			{"PUT", "/v1/settings/secrets/OPENAI_API_KEY", `{"value":"x"}`, http.StatusNoContent}, // no store set up: one is made
			{"PUT", "/v1/settings/secrets/bad name", `{"value":"x"}`, http.StatusBadRequest},
		} {
			if code, out := call(c.method, c.path, c.body); code != c.code {
				t.Errorf("%s %s: %d %s", c.method, c.path, code, out)
			}
		}
		fileHas(t, path, "# my notes", "openai:big", "ollama:qwen3:8b", "max_cost: 0.5", "workflow_cost: 5", "mine:", "https://llm.example.com/v1", "MINE_KEY")
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "nowhere") || strings.Contains(string(b), "file:///etc") || strings.Contains(string(b), "-1") {
			t.Errorf("a refused change was written:\n%s", b)
		}
	})
}

// Without the token, nothing is read or changed.
func TestSettingsNeedTheToken(t *testing.T) {
	settingsAPI(t, "models: [openai:m]\n", nil, func(call func(string, string, string) (int, string), path string) {
		for _, c := range [][3]string{{"GET", "!/v1/settings", ""}, {"PUT", "!/v1/settings/models", `{"models":["openai:x"]}`}} {
			if code, _ := call(c[0], c[1], c[2]); code != http.StatusUnauthorized {
				t.Errorf("%s %s without the token: %d", c[0], c[1], code)
			}
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "openai:x") {
			t.Error("changed without the token")
		}
	})
}

// The voice models are chosen from the app: written to transcribe: and
// speech:, checked as the rest; and the app sees them.
func TestSettingsVoice(t *testing.T) {
	settingsAPI(t, "models: [openai:m]\n", map[string]string{"OPENAI_API_KEY": "k"}, func(call func(string, string, string) (int, string), path string) {
		ok := `{"transcribe":{"provider":"openai","model":"whisper-1"},"speech":{"provider":"openai","model":"tts-1","voice":"alloy"}}`
		if code, out := call("PUT", "/v1/settings/voice", ok); code != http.StatusNoContent {
			t.Fatalf("%d %s", code, out)
		}
		fileHas(t, path, "transcribe:", "whisper-1", "speech:", "tts-1", "alloy")
		for _, bad := range []string{
			`{"transcribe":{"provider":"anthropic","model":"x"}}`, // no audio models there
			`{"transcribe":{"model":"x","voice":"alloy"}}`,
		} {
			if code, out := call("PUT", "/v1/settings/voice", bad); code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", bad, code, out)
			}
		}
	})
}

// The voice routes take the models as they are when asked: none, off.
func TestServeVoiceFollowsTheSettings(t *testing.T) {
	h := newHarness(t, "")
	env(h, map[string]string{"OPENAI_API_KEY": "k"})
	writeConfig(t, h.dir, "models: [openai:m]\ntranscribe: {provider: openai, model: whisper-1}\n")
	loaded(t, h.c, nil, func(ctx *orpheus.Context) error {
		v := h.c.voice(ctx)
		tr, err := v.Transcriber(ctx.Context())
		sy, serr := v.Synthesizer(ctx.Context())
		if err != nil || tr == nil || serr != nil || sy != nil {
			t.Fatalf("transcriber %v %v, synthesizer %v %v", tr, err, sy, serr)
		}
		return nil
	})
}

// Changes one after the other are each checked on the file as the one
// before left it, not on the revision read seconds ago.
func TestSettingsCheckTheFileOnDisk(t *testing.T) {
	yaml := "models: [openai:m]\nproviders:\n  mine: {kind: openai, base: \"https://llm.example.com/v1\", key_env: MINE_KEY}\n"
	settingsAPI(t, yaml, map[string]string{"OPENAI_API_KEY": "k", "MINE_KEY": "k"}, func(call func(string, string, string) (int, string), path string) {
		if code, out := call("DELETE", "/v1/settings/providers/mine", ""); code != http.StatusNoContent {
			t.Fatalf("delete %d %s", code, out)
		}
		if code, out := call("PUT", "/v1/settings/models", `{"models":["mine:m"]}`); code != http.StatusBadRequest {
			t.Fatalf("a model of the provider just deleted: %d %s", code, out)
		}
		if code, out := call("PUT", "/v1/settings/models", `{"models":["openai:a","openai:b"]}`); code != http.StatusNoContent {
			t.Fatalf("%d %s", code, out)
		}
		fileHas(t, path, "openai:a", "openai:b")
	})
}

// A file broken by hand is not changed on top: it is said.
func TestSettingsRefuseABrokenFile(t *testing.T) {
	settingsAPI(t, "models: [openai:m]\n", map[string]string{"OPENAI_API_KEY": "k"}, func(call func(string, string, string) (int, string), path string) {
		if err := os.WriteFile(path, []byte("models: [nowhere:m]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := call("PUT", "/v1/settings/limits", `{"max_cost":1}`)
		if code != http.StatusConflict || !strings.Contains(out, "not valid") {
			t.Fatalf("%d %s", code, out)
		}
		fileHas(t, path, "nowhere:m")
	})
}

// The same MCP server twice: the second is refused, not merged.
func TestSettingsMCPTwice(t *testing.T) {
	settingsAPI(t, "models: [openai:m]\n", map[string]string{"OPENAI_API_KEY": "k"}, func(call func(string, string, string) (int, string), path string) {
		first := `{"name":"docs","url":"http://127.0.0.1:1/mcp","headers":{"Authorization":"Bearer ${DOCS_TOKEN}"}}`
		if code, out := call("POST", "/v1/settings/mcp_servers", first); code != http.StatusCreated {
			t.Fatalf("first %d %s", code, out)
		}
		second := `{"name":"docs","command":["echo"]}`
		if code, out := call("POST", "/v1/settings/mcp_servers", second); code != http.StatusConflict {
			t.Fatalf("second %d %s", code, out)
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "echo") {
			t.Fatalf("merged:\n%s", b)
		}
	})
}
