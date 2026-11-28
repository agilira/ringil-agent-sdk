// answers_test.go: the four answers to a tool call
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// policyAPI runs delete_file under a Policy whose Ask is the server's;
// what is remembered is recorded.
func policyAPI(t *testing.T, kept *[]string, steps ...fake.Step) (*api, fake.Script) {
	t.Helper()
	ran := false
	reg, err := agent.NewRegistry(waiter(&ran))
	if err != nil {
		t.Fatal(err)
	}
	var s *Server
	pol := &agent.Policy{Ask: func(ctx context.Context, c agent.ToolCall, sp agent.ToolSpec) (agent.Answer, error) {
		return s.Ask(ctx, c, sp)
	},
		Remember: func(r agent.Rule, scope agent.Scope) error {
			*kept = append(*kept, r.String()+map[agent.Scope]string{agent.RememberSession: " session", agent.RememberProject: " always"}[scope])
			return nil
		}}
	model := fake.New(steps...)
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: model, Tools: reg, Approve: pol.Approve})}})
	s = a.srv
	return a, model
}

func (a *api) waiting(id string) Approval {
	a.t.Helper()
	w := a.until(id, func(s Snapshot) bool { return s.State == Waiting })
	if len(w.Pending) != 1 {
		a.t.Fatalf("%+v", w)
	}
	return w.Pending[0]
}

// Yes always keeps the call's rule; the approval says which.
func TestAnswerAlways(t *testing.T) {
	var kept []string
	a, _ := policyAPI(t, &kept, fake.Call("delete_file", `{}`), fake.Reply("ok"))
	run := a.start(`{"profile":"default","input":"x"}`)
	p := a.waiting(run.ID)
	if p.Rule != "delete_file" || !p.Explain {
		t.Fatalf("the approval: %+v", p)
	}
	path := "/v1/runs/" + run.ID + "/approvals/" + p.ID
	for _, bad := range []string{`{"allow":false,"remember":"always"}`, `{"allow":true,"remember":"forever"}`, `{"allow":true,"message":"x"}`} {
		if code, out := a.do("POST", path, bad); code != 400 {
			t.Errorf("%s: %d %s", bad, code, out)
		}
	}
	if code, out := a.do("POST", path, `{"allow":true,"remember":"always"}`); code != 204 {
		t.Fatalf("%d %s", code, out)
	}
	if end := a.until(run.ID, over); end.State != Completed || len(kept) != 1 || kept[0] != "delete_file always" {
		t.Fatalf("%+v %v", end, kept)
	}
}

// No, with what to do instead: the agent reads the person's words.
func TestAnswerNoAndWhy(t *testing.T) {
	var kept []string
	a, model := policyAPI(t, &kept, fake.Call("delete_file", `{}`), fake.Reply("ok, I'll archive it"))
	run := a.start(`{"profile":"default","input":"x"}`)
	p := a.waiting(run.ID)
	if code, out := a.do("POST", "/v1/runs/"+run.ID+"/approvals/"+p.ID, `{"allow":false,"message":"archive it instead"}`); code != 204 {
		t.Fatalf("%d %s", code, out)
	}
	a.until(run.ID, over)
	reqs := model.Requests()
	told, _ := json.Marshal(reqs[len(reqs)-1].Messages)
	if !strings.Contains(string(told), "and said: archive it instead") || len(kept) != 0 {
		t.Fatalf("the agent did not read why: %s", told)
	}
}

// A run with no conversation has no session: "yes, in this conversation"
// is not offered, and refused.
func TestAnswerSessionNeedsAConversation(t *testing.T) {
	var kept []string
	a, _ := policyAPI(t, &kept, fake.Call("delete_file", `{}`), fake.Reply("ok"))
	run := a.start(`{"profile":"default","input":"x"}`)
	p := a.waiting(run.ID)
	if p.Session {
		t.Fatalf("session offered without a conversation: %+v", p)
	}
	if code, out := a.do("POST", "/v1/runs/"+run.ID+"/approvals/"+p.ID, `{"allow":true,"remember":"session"}`); code != 400 {
		t.Fatalf("%d %s", code, out)
	}
}
