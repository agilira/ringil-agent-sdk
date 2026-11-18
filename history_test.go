// history_test.go: reading what compaction left out
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func names2(specs []agent.ToolSpec) string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

// A stubbed result names its message; read_history, offered once the
// view left something out, brings the original back.
func TestReadHistoryAfterCompaction(t *testing.T) {
	steps := append(fetches(8, ""), fake.Call(agent.HistoryToolName, `{"from":3,"to":3}`), fake.Reply("done"))
	steps = append(steps[:8], steps[9:]...) // drop the empty reply between
	p := fake.New(steps...)
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(3000)), ContextWindow: 6000})
	if _, err := a.Run(context.Background(), agent.NewConversation(), agent.UserText("read everything")); err != nil {
		t.Fatal(err)
	}
	reqs := p.Requests()
	if strings.Contains(names2(reqs[0].Tools), agent.HistoryToolName) {
		t.Fatal("read_history offered before anything was left out")
	}
	last := reqs[len(reqs)-1]
	if !strings.Contains(names2(last.Tools), agent.HistoryToolName) {
		t.Fatalf("read_history not offered after compaction: %s", names2(last.Tools))
	}
	stubbed := ""
	for _, m := range last.Messages {
		if m.Result != nil && strings.HasPrefix(m.Result.Content, "[Tool result omitted") {
			stubbed = m.Result.Content
			break
		}
	}
	if !strings.Contains(stubbed, "It is message 3") {
		t.Fatalf("stub without its number: %q", stubbed)
	}
	got := last.Messages[len(last.Messages)-1].Result.Content
	if !strings.HasPrefix(got, "#3 tool: [result] DOC:xxx") {
		t.Fatalf("read_history %q", got[:min(80, len(got))])
	}
}

func TestReadHistoryBounds(t *testing.T) {
	steps := append(fetches(8, ""), fake.Call(agent.HistoryToolName, `{"from":999}`), fake.Reply("done"))
	steps = append(steps[:8], steps[9:]...)
	p := fake.New(steps...)
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(3000)), ContextWindow: 6000})
	if _, err := a.Run(context.Background(), agent.NewConversation(), agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	last := p.Requests()[len(p.Requests())-1].Messages
	if got := last[len(last)-1].Result.Content; !strings.HasPrefix(got, "No message 999") {
		t.Fatalf("out of range: %q", got)
	}
}
