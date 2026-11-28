// revoke_test.go: an always removed from ringil.yaml is revoked (audit 27/11)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func TestAlwaysRemovedIsRevoked(t *testing.T) {
	write := fake.Call("write_file", `{"path":"notes/a.txt","content":"x"}`)
	steps := []fake.Step{write, fake.Reply("ok"), write, fake.Reply("ok")}
	appAPIFlags(t, nil, "models: [openai:m]\n", steps, func(h *harness, _ *serve.Server, call appCall) {
		r := startIn(t, call, "c1")
		sn := settled(t, call, r)
		rule := sn.Pending[0].Rule
		must(t, call, "POST", "/v1/runs/"+r+"/approvals/"+sn.Pending[0].ID, `{"allow":true,"remember":"always"}`, http.StatusNoContent)
		settled(t, call, r)
		path := filepath.Join(h.dir, "ringil", "ringil.yaml")
		for i := 0; i < 300 && !slices.Contains(h.c.conf().Permissions.Allow, rule); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if !slices.Contains(h.c.conf().Permissions.Allow, rule) {
			t.Fatal("config never showed the rule")
		}
		if err := os.WriteFile(path, []byte("models: [openai:m]\n"), 0o600); err != nil { // the user revokes it
			t.Fatal(err)
		}
		for i := 0; i < 300 && slices.Contains(h.c.conf().Permissions.Allow, rule); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if slices.Contains(h.c.conf().Permissions.Allow, rule) {
			t.Fatal("reload never happened")
		}
		end := settled(t, call, startIn(t, call, "c-new"))
		if end.State != serve.Waiting {
			t.Errorf("rule %q removed from ringil.yaml still allows in a new conversation: %+v", rule, end.State)
		}
	})
}
