// appaudit_test.go: regressions for the audit of the app's API, 27/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/workflow"
)

// A run of serve has the saved workflows' tool; a run that starts one
// lets its steps start even with one place on serve (it does not wait
// for them holding its own).
func TestWorkflowToolUnderServe(t *testing.T) {
	steps := []fake.Step{fake.Call("run_saved_workflow", `{"name":"weekly"}`), fake.Reply("started it"), fake.Reply("numbers: 1"), fake.Reply("deck")}
	appAPI(t, "models: [openai:m]\nserve:\n  max_runs: 1\n", steps, func(h *harness, srv *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/workflows/weekly", weeklyPlan, http.StatusNoContent)
		r := h.c.forRun()
		names := ""
		for _, tool := range r.workflowTools() {
			names += tool.Spec().Name + " "
		}
		r.shutdown()
		if !strings.Contains(names, workflow.RunSavedToolName) {
			t.Fatalf("a run of serve has %q", names)
		}
		if end := ranOn(t, srv, "default"); end.Text != "started it" {
			t.Fatalf("%+v", end)
		}
		var runs []WorkflowRun
		waitFor(t, func() bool {
			_ = json.Unmarshal([]byte(must(t, call, "GET", "/v1/workflow-runs", "", http.StatusOK)), &runs)
			return len(runs) == 1 && runs[0].State == "done"
		})
	})
}

// A trigger's own state is refused through a link to it, and above it.
func TestOwnStateThroughALink(t *testing.T) {
	h := newHarness(t, "")
	state := filepath.Join(h.dir, "ringil")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "looks-innocent")
	if err := os.Symlink(state, link); err != nil {
		t.Skip("no links here")
	}
	for _, p := range []string{state, filepath.Join(state, "triggers"), link, h.dir} {
		if !h.c.inOwnState(p) {
			t.Errorf("%s passed", p)
		}
	}
	if h.c.inOwnState(t.TempDir()) {
		t.Error("another folder was refused")
	}
}

// What the app may not set is refused in any case of its letters; a
// workflow's trigger has no agent of its own; a step of no run is not
// found.
func TestAppAuditRefusals(t *testing.T) {
	appAPI(t, "models: [openai:m]\nprofiles:\n  writer: {}\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/triggers/x", `{"when":"@daily","prompt":"x","Paused":true}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/workflows/weekly", weeklyPlan, http.StatusNoContent)
		must(t, call, "PUT", "/v1/triggers/x", `{"when":"@daily","workflow":"weekly","profile":"writer"}`, http.StatusBadRequest)
		must(t, call, "POST", "/v1/workflow-runs/nope/steps/a/retry", "", http.StatusNotFound)
		must(t, call, "PUT", "/v1/settings/remote_agents/w", `{"url":"https://a.example","disabled":true,"headers":{"a b":"${K}"}}`, http.StatusBadRequest)
		must(t, call, "PUT", "/v1/settings/profiles/Writer", `{"description":"x"}`, http.StatusBadRequest)
	})
}

// An agent removed and made again keeps its file: the watcher goes on.
func TestAgentRemovedAndMadeAgain(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, srv *serve.Server, call appCall) {
		for range 3 {
			must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"w","instructions":"same"}`, http.StatusNoContent)
			waitFor(t, func() bool { return hasProfile(srv, "writer") })
			must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusNoContent)
			waitFor(t, func() bool { return !hasProfile(srv, "writer") })
		}
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"w","instructions":"same"}`, http.StatusNoContent)
		waitFor(t, func() bool { return hasProfile(srv, "writer") })
		must(t, call, "PUT", "/v1/settings/profiles/writer", `{"description":"w2","instructions":"same"}`, http.StatusNoContent)
		waitFor(t, func() bool { return h.c.conf().Profiles["writer"].Description == "w2" })
		if _, err := os.Stat(filepath.Join(h.dir, "ringil", "profiles", "writer.md")); err != nil {
			t.Fatal("the file of the agent made again is gone")
		}
	})
}

// A trigger on an agent just removed is refused, before the watcher has
// applied the removal.
func TestTriggerOnAgentJustRemoved(t *testing.T) {
	appAPI(t, "models: [openai:m]\nprofiles:\n  writer: {}\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusNoContent)
		must(t, call, "PUT", "/v1/triggers/daily", `{"when":"@daily","prompt":"x","profile":"writer"}`, http.StatusBadRequest)
	})
}

// The agent serve was started with stays; a workflow going holds a
// restart.
func TestServeHoldsItsOwn(t *testing.T) {
	appAPIWith(t, []string{"--profile", "writer"}, "models: [openai:m]\nprofiles:\n  writer: {}\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		must(t, call, "DELETE", "/v1/settings/profiles/writer", "", http.StatusConflict)
		h.c.wfRuns.start("wf-1")
		if why := h.c.busyForRestart(); !strings.Contains(why, "workflow") {
			t.Fatalf("a restart with a workflow going: %q", why)
		}
	})
}
