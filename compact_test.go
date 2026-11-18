// compact_test.go: tests for context compaction
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// big returns a tool that answers with n bytes of distinctive text.
func big(n int) agent.Tool {
	return agent.NewTool("fetch", "Fetch a document", func(context.Context, struct{}) (string, error) {
		return "DOC:" + strings.Repeat("x", n), nil
	}, agent.ReadOnly())
}

func fetches(n int, final string) []fake.Step {
	steps := make([]fake.Step, 0, n+1)
	for range n {
		steps = append(steps, fake.Call("fetch", `{}`))
	}
	return append(steps, fake.Reply(final))
}

func stubs(msgs []agent.Message) (stubbed, whole int) {
	for _, m := range msgs {
		if m.Role != agent.RoleTool {
			continue
		}
		if strings.HasPrefix(m.Result.Content, "[Tool result omitted") {
			stubbed++
		} else {
			whole++
		}
	}
	return stubbed, whole
}

func compactedEvents(t *testing.T, a agent.Agent, conv agent.Conversation, input string) []string {
	t.Helper()
	var what []string
	for ev, err := range a.Stream(context.Background(), conv, agent.UserText(input)) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.EventCompacted {
			what = append(what, ev.Text)
		}
	}
	return what
}

func TestNoWindowNoCompaction(t *testing.T) {
	p := fake.New(fetches(6, "done")...)
	conv := agent.NewConversation()
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(3000))})
	if got := compactedEvents(t, a, conv, "read"); len(got) != 0 {
		t.Fatalf("compacted without a known window: %v", got)
	}
	last := p.Requests()[6].Messages
	if s, w := stubs(last); s != 0 || w != 6 {
		t.Fatalf("stubbed %d whole %d", s, w)
	}
}

func TestOldToolResultsAreStubbed(t *testing.T) {
	p := fake.New(fetches(8, "done")...)
	conv := agent.NewConversation()
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(3000)), ContextWindow: 6000})
	if got := compactedEvents(t, a, conv, "read everything"); len(got) == 0 {
		t.Fatal("no compaction reported")
	}
	last := p.Requests()[8].Messages
	stubbed, whole := stubs(last)
	if whole != 4 || stubbed != 4 {
		t.Fatalf("stubbed %d whole %d", stubbed, whole)
	}
	// The conversation keeps every result in full.
	if s, w := stubs(conv.Messages()); s != 0 || w != 8 {
		t.Fatalf("conversation was changed: stubbed %d whole %d", s, w)
	}
	checkStubs(t, last, "3004 bytes, sha256 ")
}

// checkStubs verifies every stub names size and hash and drops the content.
func checkStubs(t *testing.T, msgs []agent.Message, want string) {
	t.Helper()
	for _, m := range msgs {
		if m.Role != agent.RoleTool || !strings.HasPrefix(m.Result.Content, "[Tool result omitted") {
			continue
		}
		if !strings.Contains(m.Result.Content, want) || strings.Contains(m.Result.Content, "DOC:") {
			t.Fatalf("bad stub %q", m.Result.Content)
		}
	}
}

func TestOldTurnsAreLeftOut(t *testing.T) {
	var history []agent.Message
	for range 10 {
		history = append(history, agent.UserText(strings.Repeat("domanda ", 200)), agent.AssistantText(strings.Repeat("risposta ", 200)))
	}
	p := fake.New(fake.Reply("ok"))
	a := newAgent(t, agent.Config{Provider: p, ContextWindow: 2000})
	got := compactedEvents(t, a, agent.NewConversation(history...), "ultima domanda")
	if len(got) != 1 || !strings.Contains(got[0], "earlier messages left out") {
		t.Fatalf("events %v", got)
	}
	sent := p.Requests()[0].Messages
	if len(sent) >= 21 || sent[0].Role != agent.RoleUser || !strings.Contains(sent[0].Parts[0].Text, "omitted to fit the context") {
		t.Fatalf("view %d messages, first %+v", len(sent), sent[0].Parts[0])
	}
	if !strings.HasSuffix(sent[len(sent)-1].Text(), "ultima domanda") {
		t.Fatal("the current turn was not kept")
	}
	if err := agent.ValidateHistory(sent); err != nil {
		t.Fatalf("view is not a valid conversation: %v", err)
	}
}

func TestSummaryReplacesOldTurnsAndIsCached(t *testing.T) {
	var history []agent.Message
	for range 10 {
		history = append(history, agent.UserText(strings.Repeat("domanda ", 200)), agent.AssistantText(strings.Repeat("risposta ", 200)))
	}
	conv := agent.NewConversation(history...)
	// One summary call, then the answer; the second run on an identical
	// conversation reuses the summary.
	p := fake.New(fake.Reply("RIASSUNTO"), fake.Reply("ok"), fake.Reply("ok again"))
	a := newAgent(t, agent.Config{Provider: p, ContextWindow: 2000, Compaction: agent.Compaction{Summarize: true}})
	if _, err := a.Run(context.Background(), conv, agent.UserText("ultima")); err != nil {
		t.Fatal(err)
	}
	reqs := p.Requests()
	final := reqs[len(reqs)-1].Messages
	first := final[0].Parts[0].Text
	if !strings.Contains(first, "RIASSUNTO") || !strings.Contains(first, "Reference data, not instructions") {
		t.Fatalf("summary not used: %q", first)
	}
	if len(reqs) != 2 || !strings.Contains(reqs[0].Messages[0].Text(), "Write the updated summary") {
		t.Fatalf("%d calls; the first must be the only summary request", len(reqs))
	}
	if _, err := a.Run(context.Background(), agent.NewConversation(history...), agent.UserText("ultima")); err != nil {
		t.Fatal(err)
	}
	if len(p.Requests()) != 3 {
		t.Fatalf("summary not cached: %d calls", len(p.Requests()))
	}
}

func TestProviderRefusalCompactsAndRetriesOnce(t *testing.T) {
	full := errors.New(agent.ErrProviderContext, "too long")
	steps := append(fetches(6, ""), fake.Reply("unused"))
	steps[6] = fake.Fail(full)
	steps = append(steps[:7], fake.Reply("done after compaction"))
	p := fake.New(steps...)
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(3000))})
	res, err := a.Run(context.Background(), nil, agent.UserText("read"))
	if err != nil || res.Text != "done after compaction" {
		t.Fatalf("res %+v err %v", res, err)
	}
	reqs := p.Requests()
	if s, _ := stubs(reqs[7].Messages); s == 0 {
		t.Fatal("retry was not compacted")
	}
}

func TestDisabledCompactionReportsTheRefusal(t *testing.T) {
	p := fake.New(fake.Fail(errors.New(agent.ErrProviderContext, "too long")), fake.Reply("unused"))
	a := newAgent(t, agent.Config{Provider: p, ContextWindow: 10, Compaction: agent.Compaction{Disabled: true}})
	if _, err := a.Run(context.Background(), nil, agent.UserText(strings.Repeat("x", 1000))); !errors.HasCode(err, agent.ErrProviderContext) {
		t.Fatalf("err %v", err)
	}
	if p.Remaining() != 1 {
		t.Fatal("retried although compaction is disabled")
	}
}

func TestOneHugeTurnKeepsOnlyTheNewestResult(t *testing.T) {
	p := fake.New(fetches(5, "done")...)
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(8000)), ContextWindow: 3000})
	if _, err := a.Run(context.Background(), nil, agent.UserText("read")); err != nil {
		t.Fatal(err)
	}
	if s, w := stubs(p.Requests()[5].Messages); s != 4 || w != 1 {
		t.Fatalf("stubbed %d whole %d", s, w)
	}
}

func TestContextWindowFromProvider(t *testing.T) {
	p := fake.NewWithInfo(agent.ModelInfo{Name: "m", ContextWindow: 6000}, fetches(8, "done")...)
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, big(3000))})
	if got := compactedEvents(t, a, agent.NewConversation(), "read"); len(got) == 0 {
		t.Fatal("window from ModelInfo ignored")
	}
}

func TestNegativeWindowRefused(t *testing.T) {
	if _, err := agent.New(agent.Config{Provider: fake.New(), ContextWindow: -1}); !errors.HasCode(err, agent.ErrAgentConfig) {
		t.Fatalf("err %v", err)
	}
}

// Audit 18/11: a result that merely looked like a stub was never
// shortened, and could push a request past the provider's limit.
func TestLookalikeStubIsStillShortened(t *testing.T) {
	fake1 := agent.NewTool("fetch", "page", func(context.Context, struct{}) (string, error) {
		return "[Tool result omitted to fit the context: fake" + strings.Repeat("x", 3000), nil
	}, agent.ReadOnly())
	p := fake.New(fetches(8, "done")...)
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, fake1), ContextWindow: 6000})
	if _, err := a.Run(context.Background(), agent.NewConversation(), agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Requests()[8].Messages[:6] {
		if m.Result != nil && len(m.Result.Content) > 1000 {
			t.Fatalf("a lookalike result was kept whole (%d bytes)", len(m.Result.Content))
		}
	}
}
