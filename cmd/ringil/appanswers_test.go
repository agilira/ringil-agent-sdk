// appanswers_test.go: the app's four answers, through ringil serve
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
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// settled is a run once it waits for an answer not given yet, or is over.
func settled(t *testing.T, call appCall, id string) serve.Snapshot {
	t.Helper()
	for range 400 {
		var sn serve.Snapshot
		_ = json.Unmarshal([]byte(must(t, call, "GET", "/v1/runs/"+id, "", http.StatusOK)), &sn)
		if (sn.State == serve.Waiting && len(sn.Pending) > 0) || !sn.Finished.IsZero() {
			return sn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run neither waited nor ended")
	return serve.Snapshot{}
}

func startIn(t *testing.T, call appCall, conversation string) string {
	t.Helper()
	var sn serve.Snapshot
	_ = json.Unmarshal([]byte(must(t, call, "POST", "/v1/runs", `{"profile":"default","conversation":"`+conversation+`","input":"note it"}`, http.StatusCreated)), &sn)
	return sn.ID
}

// "Yes, this session" holds in its conversation only; "yes, always" is
// kept in ringil.yaml.
func TestAppAnswersSessionAndAlways(t *testing.T) {
	write := fake.Call("write_file", `{"path":"notes/a.txt","content":"x"}`)
	steps := []fake.Step{write, fake.Reply("ok"), write, fake.Reply("ok"), write, fake.Reply("ok"), write, fake.Reply("ok")}
	appAPIFlags(t, nil, "models: [openai:m]\n", steps, func(h *harness, _ *serve.Server, call appCall) {
		r1 := startIn(t, call, "c1")
		sn := settled(t, call, r1)
		if len(sn.Pending) != 1 || sn.Pending[0].Rule == "" {
			t.Fatalf("the first call: %+v", sn)
		}
		must(t, call, "POST", "/v1/runs/"+r1+"/approvals/"+sn.Pending[0].ID, `{"allow":true,"remember":"session"}`, http.StatusNoContent)
		settled(t, call, r1)
		if end := settled(t, call, startIn(t, call, "c1")); end.State == serve.Waiting {
			t.Fatalf("asked again in the same conversation: %+v", end)
		}
		if l, _ := h.c.labels.Labels(t.Context(), "c1"); len(l.Allowed[""]) != 1 { // kept with the conversation: a restart reads it there
			t.Fatalf("the answer is not with the conversation: %+v", l)
		}
		h.c.learned = &learnedRules{} // serve again, nothing in memory
		if end := settled(t, call, startIn(t, call, "c1")); end.State == serve.Waiting {
			t.Fatalf("the answer did not outlive a restart: %+v", end)
		}
		must(t, call, "DELETE", "/v1/conversations/c1", "", http.StatusNoContent)
		if l, _ := h.c.labels.Labels(t.Context(), "c1"); l.Allowed != nil {
			t.Fatalf("a deleted conversation kept its answers: %+v", l)
		}
		r3 := startIn(t, call, "c2")
		sn = settled(t, call, r3)
		if len(sn.Pending) != 1 {
			t.Fatalf("another conversation was not asked: %+v", sn)
		}
		must(t, call, "POST", "/v1/runs/"+r3+"/approvals/"+sn.Pending[0].ID, `{"allow":true,"remember":"always"}`, http.StatusNoContent)
		settled(t, call, r3)
		data, _ := os.ReadFile(filepath.Join(h.dir, "ringil", "ringil.yaml"))
		if !strings.Contains(string(data), sn.Pending[0].Rule) {
			t.Fatalf("always was not kept:\n%s", data)
		}
	})
}

// Labels left by a conversation of the same id (deleted another way, or
// a run that never saved) are not a new conversation's: its first run
// starts with no answer kept.
func TestNewConversationKeepsNoOldAnswers(t *testing.T) {
	write := fake.Call("write_file", `{"path":"notes/a.txt","content":"x"}`)
	appAPIFlags(t, nil, "models: [openai:m]\n", []fake.Step{write, fake.Reply("ok")}, func(h *harness, _ *serve.Server, call appCall) {
		if err := h.c.labels.Label(t.Context(), "old", func(l *agent.Labels) { l.Allowed = map[string][]string{"": {"write_file"}} }); err != nil {
			t.Fatal(err)
		}
		if sn := settled(t, call, startIn(t, call, "old")); sn.State != serve.Waiting {
			t.Fatalf("a new conversation ran on old answers: %+v", sn)
		}
	})
}

func TestKeepRuleWithoutConfigAndImmediateSettle(t *testing.T) {
	c := newCLI(strings.NewReader(""), os.Stdout, os.Stderr)
	if err := c.keepRule(agent.Rule{Effect: agent.EffectAllow, Tool: "read_file"}); err == nil {
		t.Fatal("keepRule succeeded without a config file")
	}
	write := fake.Call("write_file", `{"path":"notes/a.txt","content":"x"}`)
	appAPIFlags(t, nil, "models: [openai:m]\n", []fake.Step{write, fake.Reply("ok"), write, fake.Reply("ok")}, func(h *harness, _ *serve.Server, call appCall) {
		r := startIn(t, call, "c1")
		sn := settled(t, call, r)
		must(t, call, "POST", "/v1/runs/"+r+"/approvals/"+sn.Pending[0].ID, `{"allow":true,"remember":"always"}`, http.StatusNoContent)
		settled(t, call, r)
		if end := settled(t, call, startIn(t, call, "c-other")); end.State == serve.Waiting {
			t.Fatalf("always rule not in force on immediate next run: %+v", end.State)
		}
	})
}
