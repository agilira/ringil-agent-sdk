// approvals_test.go: --no-approvals
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/tools/bash"
)

func TestNoApprovalsAsksNothing(t *testing.T) {
	var out bytes.Buffer
	c := &cli{in: strings.NewReader(""), out: &out, errOut: &out, noApprovals: true}
	ctx := context.Background()
	destructive := agent.ToolSpec{Annotations: agent.Annotations{Destructive: true}}
	checks := map[string]func() (bool, error){
		"tool":      func() (bool, error) { return c.approve(ctx, agent.ToolCall{Name: "wipe"}, destructive) },
		"command":   func() (bool, error) { return c.confirmCommand(ctx, bash.Request{Command: "rm -rf build"}) },
		"domain":    func() (bool, error) { return c.askDomain(ctx, "pypi.org") },
		"protected": func() (bool, error) { return c.confirmProtected(ctx, ".git/config") },
	}
	for name, check := range checks {
		if ok, err := check(); !ok || err != nil {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
	if out.Len() > 0 {
		t.Fatalf("asked: %q", out.String())
	}
}

func TestNoApprovalsLeavesChatsToTheBridge(t *testing.T) {
	c := &cli{noApprovals: true, runConversation: "telegram-42"}
	if c.unasked() {
		t.Fatal("a chat's run went unasked: the Bridge decides by who started it")
	}
	c.runConversation = ""
	if !c.unasked() {
		t.Fatal("asked")
	}
}

func TestEveryChatIsLeftToTheBridge(t *testing.T) {
	for _, name := range chats {
		c := &cli{noApprovals: true, runConversation: name + "-42"}
		if c.unasked() || c.chatSystem("x") == "x" {
			t.Errorf("%s", name)
		}
	}
}
