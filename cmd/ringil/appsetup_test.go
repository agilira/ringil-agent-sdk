// appsetup_test.go: the app's first start
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// setupAPI is serve with the settings, setup and state routes, on a
// ringil.yaml of body and the environment vars.
func setupAPI(t *testing.T, body string, vars map[string]string, fn func(h *harness, call func(method, p, body string) (int, string), path string)) {
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
		h.c.mountSetup(ctx, srv)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		fn(h, caller(t, ts.URL), path)
		return nil
	})
}

// From nothing: the app learns what is missing, the user gives a model,
// a key and their own instructions, and it is ready.
func TestFirstStart(t *testing.T) {
	setupAPI(t, "", map[string]string{}, func(h *harness, call func(method, p, body string) (int, string), path string) {
		if code, out := call("GET", "/v1/setup", ""); code != http.StatusOK || !strings.Contains(out, `"ready":false`) {
			t.Fatalf("setup %d %s", code, out)
		}
		steps := []struct{ method, path, body string }{
			{"PUT", "/v1/settings/models", `{"models":["anthropic:claude-opus-5-5"]}`},
			{"PUT", "/v1/settings/secrets/ANTHROPIC_API_KEY", `{"value":"sk-test"}`},
			{"PUT", "/v1/settings/instructions", `{"text":"Answer in Italian. I am Antonio."}`},
		}
		for _, s := range steps {
			if code, out := call(s.method, s.path, s.body); code != http.StatusNoContent {
				t.Fatalf("%s: %d %s", s.path, code, out)
			}
		}
		checkFirstStartDone(t, h, call, path)
	})
}

func checkFirstStartDone(t *testing.T, h *harness, call func(method, p, body string) (int, string), path string) {
	t.Helper()
	fileHas(t, path, "secrets:", "store: file", "instructions:")
	key, err := os.ReadFile(filepath.Join(h.dir, "ringil", secretsDir, keyFile))
	if err != nil || !strings.HasPrefix(string(key), appPassphrase+"=") {
		t.Fatalf("key file %q %v", key, err)
	}
	// Unix modes; on Windows the user's profile keeps it private.
	if info, _ := os.Stat(filepath.Join(h.dir, "ringil", secretsDir, keyFile)); info == nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		t.Fatal("the key file is not private")
	}
	if code, out := call("GET", "/v1/settings/instructions", ""); code != http.StatusOK || !strings.Contains(out, "I am Antonio") {
		t.Fatalf("instructions %d %s", code, out)
	}
	enc, _ := os.ReadFile(filepath.Join(h.dir, "ringil", secretsDir, secretsEnc))
	if strings.Contains(string(enc), "sk-test") {
		t.Fatal("the key in clear")
	}
	waitReady(t, h, call)
}

// waitReady: the watcher applies ringil.yaml in a moment, then the app is
// ready.
func waitReady(t *testing.T, h *harness, call func(method, p, body string) (int, string)) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, out := call("GET", "/v1/setup", "")
		if strings.Contains(out, `"ready":true`) && strings.Contains(out, `"instructions":true`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %s; %s", out, h.err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Only a link of this machine is ever handed to the browser.
func TestOpenBrowserOnlyLoopback(t *testing.T) {
	for _, bad := range []string{"https://evil.example.com", "file:///etc/passwd", "http://10.0.0.1:8765/"} {
		if err := openBrowser(bad); err == nil {
			t.Errorf("%s opened", bad)
		}
	}
}

// A passphrase of another store in the environment never opens or locks
// the one ringil made: its own name, answered by the key file alone.
func TestAppStoreOwnPassphrase(t *testing.T) {
	setupAPI(t, "models: [openai:m]\n", map[string]string{"RINGIL_SECRETS_PASSPHRASE": "someone-elses", appPassphrase: "planted"}, func(h *harness, call func(method, p, body string) (int, string), path string) {
		if code, out := call("PUT", "/v1/settings/secrets/OPENAI_API_KEY", `{"value":"sk-1"}`); code != http.StatusNoContent {
			t.Fatalf("%d %s", code, out)
		}
		fileHas(t, path, "passphrase_env: "+appPassphrase)
		if code, out := call("PUT", "/v1/settings/secrets/OTHER_KEY", `{"value":"sk-2"}`); code != http.StatusNoContent {
			t.Fatalf("second key %d %s", code, out)
		}
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), "secrets:") != 1 {
			t.Fatalf("the store set up twice:\n%s", b)
		}
		res, err := h.c.withKeyFile(lookupEnv{h.c.getenv})
		if err != nil {
			t.Fatal(err)
		}
		if v, _, _ := res.Lookup(t.Context(), appPassphrase); v == "planted" || v == "" {
			t.Fatalf("the passphrase came from the environment: %q", v)
		}
	})
}
