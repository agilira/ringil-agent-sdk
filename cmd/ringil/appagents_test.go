// appagents_test.go: the agents, triggers and workflows of the app's API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
		if len(list) != 2 || !list[0].Main || list[1].Instructions != "You write in Italian." || list[1].Tools == nil || len(*list[1].Tools) != 0 {
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

// The main agent takes a title, a description and instructions of its
// own (agent: in ringil.yaml); an agent's title is free text and goes with
// it to the list the chat chooses from.
func TestAppAgentTitles(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", []fake.Step{fake.Reply("ok")}, func(h *harness, srv *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"title":"Assistente Vendite","description":"segue i clienti","instructions":"Rispondi in italiano."}`, http.StatusNoContent)
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"title":"Scrittore dei report","description":"w"}`, http.StatusNoContent)
		waitFor(t, func() bool {
			return profileLabel(srv, "default") == "Assistente Vendite|segue i clienti" && profileLabel(srv, "writer") == "Scrittore dei report|w"
		})
		checkTitledProfiles(t, h, srv, listProfiles(t, call))
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"title":"Vendite","description":"segue i clienti"}`, http.StatusNoContent)
		waitFor(t, func() bool { return profileLabel(srv, "default") == "Vendite|segue i clienti" })
		if got := listProfiles(t, call)[0]; got.Instructions != "Rispondi in italiano." {
			t.Fatalf("renaming lost the instructions: %+v", got)
		}
		if strings.Contains(h.c.errOutText(), "not applied") {
			t.Errorf("a revision was refused on the way: %s", h.c.errOutText())
		}
	})
}

func checkTitledProfiles(t *testing.T, h *harness, srv *serve.Server, list []ProfileOut) {
	t.Helper()
	if len(list) != 2 || !list[0].Main || list[0].Title != "Assistente Vendite" || list[0].Instructions != "Rispondi in italiano." || list[1].Title != "Scrittore dei report" {
		t.Fatalf("%+v", list)
	}
	privateFile(t, filepath.Join(h.dir, "ringil", "profiles", "default.md"))
	fileHas(t, filepath.Join(h.dir, "ringil", "ringil.yaml"), "agent:", "Assistente Vendite", "profiles/default.md")
	ranOn(t, srv, "default")
	if reqs := h.script.Requests(); !strings.Contains(reqs[len(reqs)-1].System, "Rispondi in italiano.") {
		t.Fatal("the main agent's instructions are not in its prompt")
	}
}

func profileLabel(srv *serve.Server, name string) string {
	for _, p := range srv.Profiles() {
		if p.Name == name {
			return p.Title + "|" + p.Description
		}
	}
	return ""
}

func listProfiles(t *testing.T, call appCall) []ProfileOut {
	t.Helper()
	var list []ProfileOut
	if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/settings/profiles", "", http.StatusOK)), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

// An edit from the app is in force when it answers: the list it reads
// next, and serve's agents, already have it (found 28/11: the app read the
// revision before, and showed an agent removed).
func TestAppEditInForceOnAnswer(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		for i := range 5 {
			must(t, call, "PUT", "/v1/settings/profiles/writer", `{"title":"Scrittore `+strconv.Itoa(i)+`","description":"w"}`, http.StatusNoContent)
			if !hasProfile(srv, "writer") || !strings.Contains(must(t, call, "GET", "/v1/settings/profiles", "", http.StatusOK), "Scrittore "+strconv.Itoa(i)) {
				t.Fatalf("round %d: the edit is not in force on the answer", i)
			}
			must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusNoContent)
			if hasProfile(srv, "writer") || strings.Contains(must(t, call, "GET", "/v1/settings/profiles", "", http.StatusOK), "writer") {
				t.Fatalf("round %d: the agent removed is still there on the answer", i)
			}
		}
	})
}

// The watcher, catching up with edits put in force at once, does not go
// back to one in between; and a revision put back by hand is applied once
// the watcher has had its time to see the writes.
func TestAppEditsNotUndoneByWatcher(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 4 {
			must(t, call, "PUT", "/v1/settings/profiles/writer", `{"title":"T`+strconv.Itoa(i)+`"}`, http.StatusNoContent)
		}
		title := func() string { return h.c.conf().Profiles["writer"].Title }
		deadline := time.Now().Add(3 * h.c.settings.every)
		for time.Now().Before(deadline) {
			if got := title(); got != "T3" {
				t.Fatalf("the watcher went back to %q", got)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return !hasProfile(srv, "writer") })
	})
}

// Audit 28/11: an agent removed from the app, its file with it, leaves the
// watcher following the file still: a hand edit after is applied (the
// watcher was left requiring the removed file, and refused every reload).
func TestAppAgentRemovedThenHandEdit(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		every := h.c.settings.every
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"w","instructions":"x"}`, http.StatusNoContent)
		time.Sleep(2 * every) // the watcher has seen the write too
		file := filepath.Join(h.dir, "ringil", "profiles", "writer.md")
		must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusNoContent)
		waitFor(t, func() bool { _, err := os.Stat(file); return os.IsNotExist(err) })
		time.Sleep(2 * every)
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		if err := os.WriteFile(path, []byte("models: [openai:m]\nprofiles:\n  hand:\n    description: by hand\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(6 * every)
		for !hasProfile(srv, "hand") {
			if time.Now().After(deadline) {
				t.Fatalf("the hand edit was never applied: %s", h.c.errOutText())
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// A profile removed by hand with its instructions file at once: the
// watcher, which followed the file, moves to what ringil.yaml names now
// (audit 28/11: it kept requiring the file, and refused every reload).
func TestHandRemovedInstructionsFile(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, _ appCall) {
		every := h.c.settings.every
		dir := filepath.Join(h.dir, "ringil")
		file := filepath.Join(dir, "w.md")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "ringil.yaml")
		if err := os.WriteFile(path, []byte("models: [openai:m]\nprofiles:\n  w:\n    instructions: w.md\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(6 * every)
		for !hasProfile(srv, "w") {
			if time.Now().After(deadline) {
				t.Fatalf("not applied: %s", h.c.errOutText())
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := os.WriteFile(path, []byte("models: [openai:m]\nprofiles:\n  hand:\n    description: by hand\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(6 * every)
		for !hasProfile(srv, "hand") {
			if time.Now().After(deadline) {
				t.Fatalf("the watcher did not follow the file: %s", h.c.errOutText())
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// An agent's instructions file another agent still names stays when the
// app removes the agent (audit 28/11).
func TestAppAgentRemovedFileStillNamed(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/settings/profiles/a", `{"description":"a","instructions":"shared"}`, http.StatusNoContent)
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		if err := os.WriteFile(path, []byte("models: [openai:m]\nprofiles:\n  a:\n    instructions: profiles/a.md\n  b:\n    instructions: profiles/a.md\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return hasProfile(srv, "b") })
		must(t, call, "DELETE", "/v1/settings/profiles/a", "", http.StatusNoContent)
		waitFor(t, func() bool { return !hasProfile(srv, "a") })
		time.Sleep(200 * time.Millisecond) // the removal runs after the revision
		if _, err := os.Stat(filepath.Join(h.dir, "ringil", "profiles", "a.md")); err != nil {
			t.Fatalf("a file still named was removed: %v", err)
		}
		must(t, call, "PUT", "/v1/settings/profiles/c", `{"description":"c"}`, http.StatusNoContent)
	})
}

// A revision the app puts in force has the hash of each instructions file
// in the audit, not the text (audit 28/11: settle's revisions, which the
// watcher may never publish, had only their digest).
func TestAppRevisionAuditedWithDocuments(t *testing.T) {
	var dir string
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		dir = h.dir
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"title":"Main","instructions":"Lead the sales."}`, http.StatusNoContent)
	})
	sum := sha256.Sum256([]byte("Lead the sales."))
	data := auditRecords(t, filepath.Join(dir, "ringil", "audit.db"))
	if !strings.Contains(data, hex.EncodeToString(sum[:])) || strings.Contains(data, "Lead the sales") {
		t.Fatalf("audit %s", data)
	}
}

// The same refusal made again after the file went back to the revision in
// force is told again, in the audit too (audit 28/11: it was told once).
func TestRefusalToldAgain(t *testing.T) {
	var dir string
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, _ appCall) {
		dir = h.dir
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		good, err := os.ReadFile(path) // the revision in force
		if err != nil {
			t.Fatal(err)
		}
		bad := []byte("models: [openai:m]\nprofiles:\n  w:\n    instructions: missing.md\n")
		for _, body := range [][]byte{bad, good, bad} {
			if err := writePrivateFile(path, body); err != nil { // whole: no half-written revision between
				t.Fatal(err)
			}
			time.Sleep(2 * h.c.settings.every)
		}
	})
	if n := strings.Count(auditRecords(t, filepath.Join(dir, "ringil", "audit.db")), "ringil_refused"); n < 2 {
		t.Fatalf("refused %d times in the audit, want 2", n)
	}
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
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"description":"x","models":["openai:x"]}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"description":"x","tools":[]}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/Default", `{"description":"x"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/x", `{"title":"two\nlines"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/x", `{"title":" spaced"}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/x", `{"title":"`+strings.Repeat("è", maxTitle+1)+`"}`, http.StatusBadRequest)
		must(t, call, "DELETE", "/v1/settings/profiles/default", "", http.StatusNotFound)
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
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"title":"Main","instructions":"You lead."}`, http.StatusNoContent)
		must(t, call, "PUT", "/v1/settings/profiles/default", `{"title":"Main agent"}`, http.StatusNoContent)
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		fileHas(t, path, "instructions.md", "profiles/writer.md", "b", "profiles/default.md", "Main agent")
		if b, _ := os.ReadFile(path); strings.Contains(string(b), filepath.ToSlash(h.dir)) || strings.Contains(string(b), h.dir) {
			t.Fatalf("an absolute path in ringil.yaml:\n%s", b)
		}
		waitFor(t, func() bool { return hasProfile(srv, "writer") && h.c.conf().Profiles["writer"].Description == "b" })
		var list []ProfileOut
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/settings/profiles", "", http.StatusOK)), &list); err != nil || len(list) != 2 || list[1].Instructions != "You write." {
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
