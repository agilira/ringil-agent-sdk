// apptriggers_test.go: triggers and saved workflows through the app's API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func triggersOf(t *testing.T, call appCall) map[string]TriggerOut {
	t.Helper()
	var list []TriggerOut
	if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/triggers", "", http.StatusOK)), &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]TriggerOut{}
	for _, tr := range list {
		out[tr.Name] = tr
	}
	return out
}

// runByHand runs a trigger now, once the runner holds it, and waits for
// its run to end.
func runByHand(t *testing.T, call appCall, name string) TriggerOut {
	t.Helper()
	waitFor(t, func() bool {
		code, _ := call("POST", "/v1/triggers/"+name+"/run", "")
		return code == http.StatusAccepted
	})
	waitFor(t, func() bool { st := triggersOf(t, call)[name].Status; return st.Runs == 1 && !st.LastEnd.IsZero() })
	return triggersOf(t, call)[name]
}

// A trigger is made, paused and run by hand from the app.
func TestAppTriggers(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", []fake.Step{fake.Reply("report done")}, func(_ *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/triggers/daily", `{"when":"0 8 * * 1-5","time_zone":"Europe/Rome","prompt":"the report"}`, http.StatusNoContent)
		must(t, call, "POST", "/v1/triggers/daily/pause", "", http.StatusNoContent)
		if !triggersOf(t, call)["daily"].Paused {
			t.Fatal("not paused")
		}
		if st := runByHand(t, call, "daily").Status; st.LastText != "report done" || st.LastError != "" {
			t.Fatalf("status %+v", st)
		}
		must(t, call, "POST", "/v1/triggers/daily/resume", "", http.StatusNoContent)
		if tr := triggersOf(t, call)["daily"]; tr.Paused || tr.Next.IsZero() {
			t.Fatalf("resumed %+v", tr)
		}
		must(t, call, "DELETE", "/v1/triggers/daily", "", http.StatusNoContent)
		must(t, call, "DELETE", "/v1/triggers/daily", "", http.StatusNotFound)
		must(t, call, "POST", "/v1/triggers/none/run", "", http.StatusNotFound)
	})
}

// A webhook's secret is made, shown once, kept on an edit, never listed.
func TestAppWebhookSecret(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		var made map[string]string
		if err := json.Unmarshal([]byte(must(t, call, "PUT", "/v1/triggers/hook", `{"kind":"webhook","prompt":"look"}`, http.StatusCreated)), &made); err != nil {
			t.Fatal(err)
		}
		if len(made["secret"]) < 32 || !strings.HasSuffix(made["path"], "hook") {
			t.Fatalf("%+v", made)
		}
		must(t, call, "PUT", "/v1/triggers/hook", `{"kind":"webhook","prompt":"look again"}`, http.StatusNoContent)
		store, err := h.c.triggerStore()
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(store)
		if keptSecret(store, "hook") != made["secret"] {
			t.Error("the secret changed on an edit")
		}
		if raw := must(t, call, "GET", "/v1/triggers", "", http.StatusOK); strings.Contains(raw, made["secret"]) || !strings.Contains(raw, `"has_secret":true`) {
			t.Fatalf("the list: %s", raw)
		}
	})
}

// What a trigger may not be is refused, as the CLI and the agent's tool
// refuse it.
func TestAppTriggerRefusals(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(h *harness, _ *serve.Server, call appCall) {
		for _, bad := range []struct{ path, body string }{
			{"/v1/triggers/x", `{"when":"@daily","prompt":"x","secret":"` + strings.Repeat("s", 40) + `"}`},
			{"/v1/triggers/x", `{"when":"@daily","prompt":"x","admin":true}`},
			{"/v1/triggers/x", `{"when":"@daily","prompt":"x","profile":"nobody"}`},
			{"/v1/triggers/x", `{"when":"@daily","workflow":"none"}`},
			{"/v1/triggers/x", `{"kind":"tool","tool":"write_file","prompt":"x"}`},
			{"/v1/triggers/x", `{"kind":"file","path":"` + filepath.ToSlash(filepath.Join(h.dir, "ringil")) + `","prompt":"x"}`},
			{"/v1/triggers/x", `{"when":"@daily","prompt":"x","allow":["no_such_tool"]}`},
			{"/v1/triggers/bad%20name", `{"when":"@daily","prompt":"x"}`},
		} {
			if code, out := call("PUT", bad.path, bad.body); code != http.StatusBadRequest {
				t.Errorf("%s %s: %d %s", bad.path, bad.body, code, out)
			}
		}
	})
}

const weeklyPlan = `{"description":"the Monday deck","brief":"Q4","plan":{"steps":[{"name":"numbers","worker":"default","task":"Read the sales."},{"name":"deck","worker":"default","task":"Make the deck.","needs":["numbers"]}],"output":"deck"}}`

// workflowRun waits for a run of a workflow to be done.
func workflowRun(t *testing.T, call appCall, id string) WorkflowRun {
	t.Helper()
	var run WorkflowRun
	waitFor(t, func() bool {
		_ = json.Unmarshal([]byte(must(t, call, "GET", "/v1/workflow-runs/"+id, "", http.StatusOK)), &run)
		return run.State == "done"
	})
	return run
}

// A saved workflow runs its steps on the agents, from the app; the
// journal shows it step by step.
func TestAppWorkflows(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", []fake.Step{fake.Reply("numbers: 42"), fake.Reply("deck made")}, func(h *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/workflows/weekly", weeklyPlan, http.StatusNoContent)
		var id map[string]string
		if err := json.Unmarshal([]byte(must(t, call, "POST", "/v1/workflows/weekly/runs", `{"input":"for the board"}`, http.StatusAccepted)), &id); err != nil {
			t.Fatal(err)
		}
		if run := workflowRun(t, call, id["id"]); run.Output != "deck made" || run.Steps["numbers"].Report != "numbers: 42" {
			t.Fatalf("%+v", run)
		}
		reqs := h.script.Requests()
		if last := reqs[len(reqs)-1].Messages[0].Text(); !strings.Contains(last, "numbers: 42") || !strings.Contains(last, "for the board") {
			t.Fatalf("the second step was told %q", last)
		}
		must(t, call, "POST", "/v1/workflow-runs/"+id["id"]+"/resume", "", http.StatusAccepted) // done: nothing left, it ends at once
		waitFor(t, func() bool {
			code, _ := call("DELETE", "/v1/workflow-runs/"+id["id"], "")
			return code == http.StatusNoContent
		})
		must(t, call, "DELETE", "/v1/workflows/weekly", "", http.StatusNoContent)
		must(t, call, "GET", "/v1/workflows/weekly", "", http.StatusNotFound)
	})
}

// A workflow that is not one, or names what is not there, is refused.
func TestAppWorkflowRefusals(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		for _, bad := range []string{
			`{"description":"x","plan":{"steps":[{"name":"a","worker":"nobody","task":"x"}],"output":"a"}}`,
			`{"description":"x","plan":{"steps":[{"name":"a","worker":"default","skill":"not-installed","task":"x"}],"output":"a"}}`,
			`{"description":"x","plan":{"steps":[],"output":"a"}}`,
		} {
			if code, out := call("PUT", "/v1/workflows/bad", bad); code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", bad, code, out)
			}
		}
	})
}

// A trigger runs a saved workflow, by hand here; the workflow stays while
// a trigger runs it.
func TestAppTriggerRunsAWorkflow(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", []fake.Step{fake.Reply("numbers: 43"), fake.Reply("deck again")}, func(_ *harness, _ *serve.Server, call appCall) {
		must(t, call, "PUT", "/v1/workflows/weekly", weeklyPlan, http.StatusNoContent)
		must(t, call, "PUT", "/v1/triggers/monday", `{"when":"0 9 * * 1","workflow":"weekly"}`, http.StatusNoContent)
		must(t, call, "DELETE", "/v1/workflows/weekly", "", http.StatusConflict)
		if st := runByHand(t, call, "monday").Status; st.LastText != "deck again" || !strings.HasPrefix(st.LastRun, "weekly-") {
			t.Fatalf("status %+v", st)
		}
		var runs []WorkflowRun
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/workflow-runs", "", http.StatusOK)), &runs); err != nil || len(runs) != 1 || runs[0].State != "done" {
			t.Fatalf("%v %+v", err, runs)
		}
	})
}
