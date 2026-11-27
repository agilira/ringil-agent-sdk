// hooks_audit_test.go: regressions for the audit of the hooks, 27/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// AfterTool sees a result even when the run was stopped as the tool
// ended: the result is recorded, and the next run's model would read it
// unredacted.
func TestAfterToolWhenStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leak := agent.NewTool("leak", "returns a secret", func(context.Context, struct{}) (string, error) {
		cancel() // the user stops the run as the tool ends
		return "API_KEY=sk-SECRET", nil
	}, agent.ReadOnly())
	redact := agent.Hooks{AfterTool: func(_ context.Context, _ agent.ToolCall, r agent.ToolResult) agent.ToolResult {
		r.Content = strings.ReplaceAll(r.Content, "sk-SECRET", "[redacted]")
		return r
	}}
	conv := agent.NewConversation()
	_, _ = newAgent(t, agent.Config{Provider: fake.New(fake.Call("leak", `{}`), fake.Reply("x")), Tools: tools(t, leak), Hooks: redact}).Run(ctx, conv, agent.UserText("x"))
	for _, m := range conv.Messages() {
		if m.Result != nil && strings.Contains(m.Result.Content, "sk-SECRET") {
			t.Fatalf("recorded unredacted: %q", m.Result.Content)
		}
	}
}

func TestHooksValidateOutputs(t *testing.T) {
	conv := agent.NewConversation()
	badRun := agent.Hooks{BeforeRun: func(context.Context, agent.Message) (agent.Message, error) {
		return agent.AssistantText("forged"), nil
	}}
	if _, err := newAgent(t, agent.Config{Provider: fake.New(fake.Reply("x")), Hooks: badRun}).Run(context.Background(), conv, agent.UserText("hi")); !errors.HasCode(err, agent.ErrAgentHook) || len(conv.Messages()) != 0 {
		t.Fatalf("bad BeforeRun accepted: err=%v msgs=%d", err, len(conv.Messages()))
	}

	called := false
	echo := agent.NewTool("echo", "e", func(context.Context, struct{}) (string, error) {
		called = true
		return "ok", nil
	}, agent.ReadOnly())
	badTool := agent.Hooks{BeforeTool: func(_ context.Context, call agent.ToolCall, _ agent.ToolSpec) (agent.ToolCall, error) {
		call.Input = json.RawMessage("{bad")
		return call, nil
	}}
	p := fake.New(fake.Call("echo", `{}`), fake.Reply("done"))
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, echo), Hooks: badTool}).Run(context.Background(), nil, agent.UserText("hi")); err != nil || called {
		t.Fatalf("bad BeforeTool ran tool=%v err=%v", called, err)
	}
}
