// appagents_test.go: the agents, triggers and workflows of the app's API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/trigger"
)

// appCall sends a request to the app's API with the token.
type appCall func(method, path, body string) (int, string)

// appAPI runs fn with serve as `ringil serve` builds it (its agents
// answer with steps), its triggers running, over body as ringil.yaml.
func appAPI(t *testing.T, body string, steps []fake.Step, fn func(h *harness, srv *serve.Server, call appCall)) {
	t.Helper()
	appAPIWith(t, nil, body, steps, fn)
}

// appAPIWith is appAPI with more flags.
func appAPIWith(t *testing.T, args []string, body string, steps []fake.Step, fn func(h *harness, srv *serve.Server, call appCall)) {
	t.Helper()
	h := newHarness(t, "", steps...)
	writeConfig(t, h.dir, body)
	loaded(t, h.c, append([]string{"--no-approvals"}, args...), func(ctx *orpheus.Context) error {
		srv, _, err := h.c.newServer(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		h.c.server, h.c.serveCtx = srv, ctx
		h.c.serving.Store(&serving{srv: srv, ctx: ctx})
		run, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop, err := h.c.startTriggers(ctx, run, srv)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		token, _, err := h.c.serveToken(ctx)
		if err != nil {
			t.Fatal(err)
		}
		call := func(method, p, b string) (int, string) {
			req, _ := http.NewRequest(method, ts.URL+p, strings.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			out, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(out)
		}
		fn(h, srv, call)
		return nil
	})
}

func must(t *testing.T, call appCall, method, path, body string, want int) string {
	t.Helper()
	code, out := call(method, path, body)
	if code != want {
		t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	return out
}

func hasProfile(srv *serve.Server, name string) bool {
	for _, p := range srv.Profiles() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// An agent made from the app answers at once, with its own prompt; its
// instructions are a file of its own, beside ringil.yaml.
func TestAppAgents(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", []fake.Step{fake.Reply("written")}, func(h *harness, srv *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"writes the reports","instructions":"You write in Italian.","tools":[]}`, http.StatusNoContent)
		waitFor(t, func() bool { return hasProfile(srv, "writer") })
		var list []ProfileOut
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/settings/profiles", "", http.StatusOK)), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Instructions != "You write in Italian." || list[0].Tools == nil || len(*list[0].Tools) != 0 {
			t.Fatalf("%+v", list)
		}
		privateFile(t, filepath.Join(h.dir, "ringil", "profiles", "writer.md"))
		fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "writer:", "profiles/writer.md")
		if end := ranOn(t, srv, "writer"); end.Text != "written" {
			t.Fatalf("%+v", end)
		}
		if reqs := h.script.Requests(); !strings.Contains(reqs[len(reqs)-1].System, "You write in Italian.") {
			t.Fatal("the agent's instructions are not in its prompt")
		}
	})
}

// ranOn runs profile once on srv and returns how it ended.
func ranOn(t *testing.T, srv *serve.Server, profile string) serve.Snapshot {
	t.Helper()
	snap, err := srv.Start(profile, "", "the report")
	if err != nil {
		t.Fatal(err)
	}
	end, err := srv.Wait(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	return end
}

func privateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !onWindows() && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s is %v", path, info.Mode())
	}
}

// What an agent may not be is refused, leaving no file.
func TestAppAgentRefusals(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"description":"x"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/bad%20name", `{"description":"x"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/x", `{"description":"x","tools":["no_such_tool"]}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/x", `{"description":"x","instructions":"y","memory":"everyone"}`, http.StatusBadRequest)
		if _, err := os.Stat(filepath.Join(h.dir, "ringil", "profiles", "x.md")); !os.IsNotExist(err) {
			t.Error("a refused agent left a file")
		}
	})
}

// An agent a trigger uses stays; then it goes, with its file, and takes
// no run.
func TestAppAgentRemoved(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"w","instructions":"x"}`, http.StatusNoContent)
		waitFor(t, func() bool { return hasProfile(srv, "writer") })
		file := filepath.Join(h.dir, "ringil", "profiles", "writer.md")
		must(t, call, "PUT", "/v1/triggers/daily", `{"when":"@daily","prompt":"report","profile":"writer"}`, http.StatusNoContent)
		must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusConflict)
		must(t, call, "DELETE", "/v1/triggers/daily", "", http.StatusNoContent)
		must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusNoContent)
		waitFor(t, func() bool { return !hasProfile(srv, "writer") })
		waitFor(t, func() bool { _, err := os.Stat(file); return os.IsNotExist(err) })
		if strings.Contains(h.c.errOutText(), "not applied") {
			t.Errorf("a revision was refused on the way: %s", h.c.errOutText())
		}
		must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusNotFound)
	})
}

func onWindows() bool { return filepath.Separator == '\\' }

// no_tools: true returns 400 without panicking when configuring profile tools.
func TestAppAgentNoToolsNoPanic(t *testing.T) {
	appAPI(t, "models: [openai:m]\nno_tools: true\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/profiles/w", `{"description":"w","tools":["read_file"]}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/w", `{"description":"w","tools":[]}`, http.StatusNoContent)
	})
}

// Audit 27/11: ringil.yaml names the app's files from its own folder (an
// export imported elsewhere finds them), and an edit made before the
// watcher has caught up keeps the instructions written a moment ago.
func TestAppPathsRelativeAndFromDisk(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/instructions", `{"text":"Be brief."}`, http.StatusNoContent)
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"a","instructions":"You write.","tools":[]}`, http.StatusNoContent)
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"b","tools":[]}`, http.StatusNoContent) // at once: the watcher lags
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		fileHas(t, path, "instructions.md", "profiles/writer.md", "b")
		if b, _ := os.ReadFile(path); strings.Contains(string(b), filepath.ToSlash(h.dir)) || strings.Contains(string(b), h.dir) {
			t.Fatalf("an absolute path in ringil.yaml:\n%s", b)
		}
		waitFor(t, func() bool { return hasProfile(srv, "writer") && h.c.conf().Profiles["writer"].Description == "b" })
		var list []ProfileOut
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/settings/profiles", "", http.StatusOK)), &list); err != nil || len(list) != 1 || list[0].Instructions != "You write." {
			t.Fatalf("%+v %v", list, err)
		}
		if got := h.c.conf().Instructions; got != filepath.Join(h.dir, "ringil", "instructions.md") {
			t.Fatalf("read back as %q", got)
		}
	})
}

func TestDependencyChecksFailClosed(t *testing.T) {
	c := newCLI(strings.NewReader(""), io.Discard, io.Discard)
	c.configDir = func() (string, error) { return "", errors.New("boom") }
	c.dataDir = func() (string, error) { return "", errors.New("boom") }
	if users := c.triggersOf(func(trigger.Trigger) bool { return false }); len(users) == 0 {
		t.Fatal("triggersOf failed open on triggerStore error")
	}
	if users := c.workflowsWith("writer"); len(users) == 0 {
		t.Fatal("workflowsWith failed open on savedWorkflows error")
	}
}
