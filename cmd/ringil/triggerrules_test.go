// triggerrules_test.go: a trigger does not take a conversation's answers (audit 27/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func TestTriggerTakesNoConversationAnswers(t *testing.T) {
	w1 := fake.Call("write_file", `{"path":"notes/a.txt","content":"x"}`)
	w2 := fake.Call("write_file", `{"path":"notes/b.txt","content":"y"}`)
	steps := []fake.Step{w1, fake.Reply("ok"), w2, fake.Reply("ok2")}
	appAPIFlags(t, nil, "models: [openai:m]\n", steps, func(h *harness, srv *serve.Server, call appCall) {
		r1 := startIn(t, call, "trigger-x")
		sn := settled(t, call, r1)
		if len(sn.Pending) != 1 {
			t.Fatalf("%+v", sn)
		}
		t.Logf("rule offered: %q", sn.Pending[0].Rule)
		must(t, call, "POST", "/v1/runs/"+r1+"/approvals/"+sn.Pending[0].ID, `{"allow":true,"remember":"session"}`, http.StatusNoContent)
		settled(t, call, r1)
		key := "nothing-allowed"
		allowed.Store(key, []string{}) // the trigger was allowed nothing
		defer allowed.Delete(key)
		snap, err := srv.StartFrom(serve.Request{Source: triggerSource + key, Profile: "default", Conversation: "trigger-x", Input: agent.UserText("go")})
		if err != nil {
			t.Fatal(err)
		}
		end, err := srv.Wait(t.Context(), snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		msgs := must(t, call, "GET", "/v1/conversations/trigger-x/messages", "", http.StatusOK)
		t.Logf("trigger run state=%v text=%q", end.State, end.Text)
		if strings.Contains(msgs, "not allowed this call") {
			t.Logf("refused as expected")
		} else {
			t.Errorf("the unattended trigger run wrote notes/b.txt on the app's session answer:\n%s", msgs[len(msgs)-min(len(msgs), 1500):])
		}
	})
}
