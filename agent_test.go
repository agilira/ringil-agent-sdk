// agent_test.go: tests for the agent loop
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type addIn struct {
	A int `json:"a"`
	B int `json:"b"`
}

// addMu guards the counters: add is read-only, so the agent may run
// several calls of it at once.
var addMu sync.Mutex

func adder(calls *int) agent.Tool {
	return agent.NewTool("add", "Add two integers", func(_ context.Context, in addIn) (int, error) {
		addMu.Lock()
		*calls++
		addMu.Unlock()
		return in.A + in.B, nil
	}, agent.ReadOnly())
}

func newAgent(t *testing.T, cfg agent.Config) agent.Agent {
	t.Helper()
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func tools(t *testing.T, ts ...agent.Tool) agent.Registry {
	t.Helper()
	r, err := agent.NewRegistry(ts...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func checkResult(t *testing.T, res *agent.Result, text string, turns, messages int) {
	t.Helper()
	if res.Text != text || res.Stop != agent.StopEnd || res.Turns != turns || len(res.Messages) != messages {
		t.Fatalf("result %+v", res)
	}
}

func TestNewValidates(t *testing.T) {
	cases := []agent.Config{
		{},
		{Provider: fake.New(), MaxTurns: -1},
		{Provider: fake.New(), MaxTokens: -1},
	}
	for i, cfg := range cases {
		if _, err := agent.New(cfg); !errors.HasCode(err, agent.ErrAgentConfig) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
}

func TestPlainAnswer(t *testing.T) {
	p := fake.New(fake.Step{Chunks: []string{"cia", "o"}, Usage: agent.Usage{Input: 5, Output: 2}})
	res, err := newAgent(t, agent.Config{Provider: p}).Run(context.Background(), nil, agent.UserText("hi"))
	if err != nil {
		t.Fatal(err)
	}
	checkResult(t, res, "ciao", 1, 2)
	if res.Usage.Input != 5 {
		t.Fatalf("usage %+v", res.Usage)
	}
	req := p.Requests()[0]
	if req.System != agent.DefaultSystem || len(req.Tools) != 0 || req.Messages[0].Text() != "hi" {
		t.Fatalf("request %+v", req)
	}
}

func TestToolRoundTrip(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":2,"b":3}`), fake.Reply("5"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), System: "custom"})
	res, err := a.Run(context.Background(), nil, agent.UserText("2+3?"))
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant with call, tool result, final assistant
	checkResult(t, res, "5", 2, 4)
	if calls != 1 || res.Messages[2].Result.Content != "5" || res.Messages[2].Result.CallID != res.Messages[1].Calls[0].ID {
		t.Fatalf("calls %d messages %+v", calls, res.Messages)
	}
	second := p.Requests()[1]
	if second.System != "custom" || len(second.Tools) != 1 || len(second.Messages) != 3 {
		t.Fatalf("second request %+v", second)
	}
}

func TestToolErrorsGoBackToTheModel(t *testing.T) {
	p := fake.New(fake.Call("add", `{"a":"x"}`), fake.Call("missing", `{}`), fake.Reply("sorry"))
	calls := 0
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	hostErrs := toolErrors(t, a.Stream(context.Background(), nil, agent.UserText("go")))
	if calls != 0 || len(hostErrs) != 2 || !errors.HasCode(hostErrs[0], agent.ErrToolInput) || !errors.HasCode(hostErrs[1], agent.ErrToolUnknown) {
		t.Fatalf("calls %d errs %v", calls, hostErrs)
	}
	third := p.Requests()[2].Messages
	if !strings.Contains(third[2].Result.Content, "Invalid arguments") {
		t.Fatalf("model not told: %+v", third[2].Result)
	}
}

// toolErrors drains a run and returns the host errors of failed tool calls.
func toolErrors(t *testing.T, events iter.Seq2[agent.Event, error]) []error {
	t.Helper()
	var errs []error
	for ev, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != agent.EventToolEnd {
			continue
		}
		if !ev.Output.IsError {
			t.Fatalf("failure not marked: %+v", ev.Output)
		}
		errs = append(errs, ev.Err)
	}
	return errs
}

func TestEventOrder(t *testing.T) {
	calls := 0
	p := fake.New(fake.Step{Chunks: []string{"let me add"}, Calls: []agent.ToolCall{{Name: "add", Input: json.RawMessage(`{"a":1,"b":1}`)}}}, fake.Reply("2"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	var kinds []string
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("1+1")) {
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, string(ev.Kind))
	}
	want := "started,text,model,tool_start,tool_end,text,model,done"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("events %s\nwant   %s", strings.Join(kinds, ","), want)
	}
}

func TestMaxTurnsStopsCleanly(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":1,"b":1}`), fake.Call("add", `{"a":1,"b":1}`), fake.Reply("never"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), MaxTurns: 2})
	res, err := a.Run(context.Background(), nil, agent.UserText("loop"))
	if err != nil || res.Stop != agent.StopMaxTurns || res.Turns != 2 || calls != 2 || p.Remaining() != 1 {
		t.Fatalf("res %+v err %v calls %d", res, err, calls)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != agent.RoleTool {
		t.Fatalf("history does not end at a complete turn: %+v", last)
	}
}

func TestHistoryIsUsedAndNotModified(t *testing.T) {
	history := []agent.Message{agent.UserText("earlier"), agent.AssistantText("ok")}
	p := fake.New(fake.Reply("again"))
	res, err := newAgent(t, agent.Config{Provider: p}).Run(context.Background(), agent.NewConversation(history...), agent.UserText("now"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Requests()[0].Messages; len(got) != 3 || got[0].Text() != "earlier" {
		t.Fatalf("request messages %+v", got)
	}
	if len(res.Messages) != 2 || len(history) != 2 || history[1].Text() != "ok" {
		t.Fatalf("result %+v history %+v", res.Messages, history)
	}
}

func TestDuplicateCallIDsAreRenamed(t *testing.T) {
	calls := 0
	dup := agent.ToolCall{ID: "call_0", Name: "add", Input: json.RawMessage(`{"a":1,"b":2}`)}
	history := []agent.Message{
		agent.UserText("x"),
		{Role: agent.RoleAssistant, Calls: []agent.ToolCall{dup}},
		agent.ToolMessage(agent.ToolResult{CallID: "call_0", Content: "3"}),
		agent.AssistantText("3"),
	}
	p := fake.New(fake.Step{Calls: []agent.ToolCall{dup, dup}}, fake.Reply("done"))
	res, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))}).Run(context.Background(), agent.NewConversation(history...), agent.UserText("again"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{"call_0": true}
	for _, call := range res.Messages[1].Calls {
		if ids[call.ID] {
			t.Fatalf("duplicate id %q kept", call.ID)
		}
		ids[call.ID] = true
	}
	if res.Messages[2].Result.CallID != res.Messages[1].Calls[0].ID || res.Messages[3].Result.CallID != res.Messages[1].Calls[1].ID {
		t.Fatalf("results not paired: %+v", res.Messages)
	}
}

func TestProviderErrorEndsTheRun(t *testing.T) {
	boom := errors.New(agent.ErrProviderAuth, "401")
	_, err := newAgent(t, agent.Config{Provider: fake.New(fake.Fail(boom))}).Run(context.Background(), nil, agent.UserText("x"))
	if !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Fatalf("err %v", err)
	}
}

func TestBreakingOutStopsTheRun(t *testing.T) {
	calls := 0
	p := fake.New(fake.Stream("a", "b", "c"), fake.Reply("unused"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	seen := 0
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if ev.Kind == agent.EventText {
			break
		}
	}
	if seen != 2 || p.Remaining() != 1 { // started, then the first text
		t.Fatalf("seen %d remaining %d", seen, p.Remaining())
	}
}

// cancelling cancels the context while a tool runs.
func TestCancelDuringTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	slow := agent.NewTool("slow", "", func(context.Context, struct{}) (string, error) { cancel(); return "late", nil })
	p := fake.New(fake.Call("slow", `{}`), fake.Reply("unused"))
	_, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, slow)}).Run(ctx, nil, agent.UserText("x"))
	if !stderrors.Is(err, context.Canceled) || p.Remaining() != 1 {
		t.Fatalf("err %v remaining %d", err, p.Remaining())
	}
}

func TestMaxTokensPassedThrough(t *testing.T) {
	p := fake.New(fake.Reply("x"))
	if _, err := newAgent(t, agent.Config{Provider: p, MaxTokens: 77}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	if p.Requests()[0].MaxTokens != 77 {
		t.Fatal("MaxTokens not passed")
	}
}

func TestConversationReceivesEveryMessage(t *testing.T) {
	calls := 0
	conv := agent.NewConversation(agent.UserText("earlier"), agent.AssistantText("ok"))
	p := fake.New(fake.Call("add", `{"a":1,"b":2}`), fake.Reply("3"))
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))}).Run(context.Background(), conv, agent.UserText("1+2")); err != nil {
		t.Fatal(err)
	}
	roles := ""
	for _, m := range conv.Messages() {
		roles += string(m.Role[0])
	}
	if roles != "uauata" {
		t.Fatalf("conversation roles %s", roles)
	}
}

// failingConv refuses to record after n messages.
type failingConv struct {
	agent.Conversation
	left int
}

func (f *failingConv) Append(ctx context.Context, msgs ...agent.Message) error {
	if f.left == 0 {
		return errors.New(agent.ErrSessionIO, "disk full")
	}
	f.left--
	return f.Conversation.Append(ctx, msgs...)
}

func TestToolDoesNotRunIfIntentCannotBeRecorded(t *testing.T) {
	calls := 0
	p := fake.New(fake.Call("add", `{"a":1,"b":2}`), fake.Reply("unused"))
	conv := &failingConv{Conversation: agent.NewConversation(), left: 1} // input only
	_, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))}).Run(context.Background(), conv, agent.UserText("x"))
	if !errors.HasCode(err, agent.ErrSessionIO) || calls != 0 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

func interrupted() []agent.Message {
	return []agent.Message{
		agent.UserText("delete the file"),
		{Role: agent.RoleAssistant, Calls: []agent.ToolCall{
			{ID: "c1", Name: "add", Input: json.RawMessage(`{"a":1,"b":1}`)},
			{ID: "c2", Name: "add", Input: json.RawMessage(`{"a":2,"b":2}`)},
		}},
		agent.ToolMessage(agent.ToolResult{CallID: "c1", Content: "2"}),
	}
}

func TestInterruptedCallsAreReportedNotRepeated(t *testing.T) {
	calls := 0
	conv := agent.NewConversation(interrupted()...)
	p := fake.New(fake.Reply("I will check first"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))})
	recovered := recoveredIDs(t, a.Stream(context.Background(), conv, agent.UserText("continue")))
	msgs := p.Requests()[0].Messages
	if calls != 0 || len(recovered) != 1 || recovered[0] != "c2" || !msgs[3].Result.IsError || msgs[3].Result.CallID != "c2" {
		t.Fatalf("calls %d recovered %v msgs %+v", calls, recovered, msgs)
	}
	if !strings.Contains(msgs[3].Result.Content, "may or may not have happened") || msgs[4].Text() != "continue" {
		t.Fatalf("model not told: %+v", msgs[3:])
	}
}

func recoveredIDs(t *testing.T, events iter.Seq2[agent.Event, error]) []string {
	t.Helper()
	var ids []string
	for ev, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.EventRecovered {
			ids = append(ids, ev.Call.ID)
		}
	}
	return ids
}

func TestStrictResumeRefuses(t *testing.T) {
	p := fake.New(fake.Reply("unused"))
	_, err := newAgent(t, agent.Config{Provider: p, StrictResume: true}).Run(context.Background(), agent.NewConversation(interrupted()...), agent.UserText("x"))
	if !errors.HasCode(err, agent.ErrSessionIncomplete) || p.Remaining() != 1 {
		t.Fatalf("err %v", err)
	}
}

func TestCorruptHistoryRefused(t *testing.T) {
	bad := []agent.Message{agent.ToolMessage(agent.ToolResult{CallID: "x", Content: "orphan"})}
	p := fake.New(fake.Reply("unused"))
	_, err := newAgent(t, agent.Config{Provider: p}).Run(context.Background(), agent.NewConversation(bad...), agent.UserText("x"))
	if !errors.HasCode(err, agent.ErrSessionCorrupt) || p.Remaining() != 1 {
		t.Fatalf("err %v", err)
	}
}

func TestCancelledToolResultIsStillRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := agent.NewTool("slow", "", func(context.Context, struct{}) (string, error) { cancel(); return "done", nil })
	conv := agent.NewConversation()
	p := fake.New(fake.Call("slow", `{}`), fake.Reply("unused"))
	_, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, tool)}).Run(ctx, conv, agent.UserText("x"))
	msgs := conv.Messages()
	if !stderrors.Is(err, context.Canceled) || len(msgs) != 3 || msgs[2].Result.Content != "done" {
		t.Fatalf("err %v msgs %+v", err, msgs)
	}
}

// block is a tool that waits until released.
func block(started, release chan struct{}) agent.Tool {
	return agent.NewTool("slow", "", func(context.Context, struct{}) (string, error) {
		close(started)
		<-release
		return "done", nil
	})
}

// Audit finding 1: a second run on a busy conversation is refused instead
// of reporting the first run's call as interrupted.
func TestSecondRunOnBusyConversationRefused(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	conv := agent.NewConversation()
	p := fake.New(fake.Call("slow", `{}`), fake.Reply("first done"), fake.Reply("second"))
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, block(started, release))})
	done := make(chan error)
	go func() {
		_, err := a.Run(context.Background(), conv, agent.UserText("one"))
		done <- err
	}()
	<-started
	if _, err := a.Run(context.Background(), conv, agent.UserText("two")); !errors.HasCode(err, agent.ErrSessionBusy) {
		t.Fatalf("second run: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), conv, agent.UserText("two")); err != nil {
		t.Fatalf("run after the first ended: %v", err)
	}
	if err := agent.ValidateHistory(conv.Messages()); err != nil {
		t.Fatal(err)
	}
}

// emptyRegistry claims a call never ran by leaving CallID empty.
type emptyRegistry struct{ agent.Registry }

func (emptyRegistry) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func TestRegistryThatDidNotRunStopsTheRun(t *testing.T) {
	calls := 0
	conv := agent.NewConversation()
	p := fake.New(fake.Call("add", `{"a":1,"b":1}`), fake.Reply("unused"))
	a := newAgent(t, agent.Config{Provider: p, Tools: emptyRegistry{tools(t, adder(&calls))}})
	if _, err := a.Run(context.Background(), conv, agent.UserText("x")); !errors.HasCode(err, agent.ErrAgentConfig) {
		t.Fatalf("err %v", err)
	}
	if n := len(conv.Messages()); n != 2 {
		t.Fatalf("recorded %d messages", n)
	}
}

func TestMemoryConversationRefusesInvalidAppend(t *testing.T) {
	conv := agent.NewConversation(agent.UserText("x"))
	if err := conv.Append(context.Background(), agent.ToolMessage(agent.ToolResult{CallID: "nope"})); !errors.HasCode(err, agent.ErrSessionCorrupt) {
		t.Fatalf("err %v", err)
	}
}

func TestInvalidUTF8ToolOutputIsRepaired(t *testing.T) {
	bin := agent.NewTool("bin", "", func(context.Context, struct{}) (string, error) { return "a\xff\xfeb", nil })
	res, err := tools(t, bin).Execute(context.Background(), agent.ToolCall{ID: "1", Name: "bin"})
	if err != nil || res.Content != "a�b" {
		t.Fatalf("content %q err %v", res.Content, err)
	}
}

func TestApproveHook(t *testing.T) {
	calls := 0
	var asked []string
	approve := func(_ context.Context, call agent.ToolCall, spec agent.ToolSpec) (bool, error) {
		asked = append(asked, call.Name+":"+map[bool]string{true: "ro", false: "rw"}[spec.Annotations.ReadOnly])
		return false, nil
	}
	p := fake.New(fake.Call("add", `{"a":1,"b":1}`), fake.Reply("ok"))
	res, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), Approve: approve}).Run(context.Background(), nil, agent.UserText("x"))
	if err != nil {
		t.Fatal(err)
	}
	result := res.Messages[2].Result
	if calls != 0 || len(asked) != 1 || asked[0] != "add:ro" || !result.IsError || !strings.Contains(result.Content, "did not approve") {
		t.Fatalf("calls %d asked %v result %+v", calls, asked, result)
	}
}

func TestApproveErrorRefuses(t *testing.T) {
	calls := 0
	approve := func(context.Context, agent.ToolCall, agent.ToolSpec) (bool, error) {
		return true, stderrors.New("dialog closed")
	}
	p := fake.New(fake.Call("add", `{"a":1,"b":1}`), fake.Reply("ok"))
	var denied error
	a := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls)), Approve: approve})
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("x")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.EventToolEnd {
			denied = ev.Err
		}
	}
	if calls != 0 || !errors.HasCode(denied, agent.ErrToolDenied) {
		t.Fatalf("calls %d err %v", calls, denied)
	}
}

// Read-only and parallel calls of one turn run at the same time; the
// results keep the order of the calls.
func TestParallelCallsRunTogether(t *testing.T) {
	var running, peak int32
	var mu sync.Mutex
	slow := func(name string, opt agent.ToolOption) agent.Tool {
		return agent.NewTool(name, "slow", func(context.Context, struct{}) (string, error) {
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			return name, nil
		}, opt)
	}
	reg, err := agent.NewRegistry(slow("a", agent.ReadOnly()), slow("b", agent.Parallel()), slow("c", agent.Destructive()))
	if err != nil {
		t.Fatal(err)
	}
	var calls []agent.ToolCall
	for _, n := range []string{"a", "b", "c", "a"} {
		calls = append(calls, agent.ToolCall{Name: n, Input: json.RawMessage(`{}`)})
	}
	p := fake.New(fake.Step{Calls: calls}, fake.Reply("ok"))
	res, err := newAgent(t, agent.Config{Provider: p, Tools: reg}).Run(context.Background(), nil, agent.UserText("x"))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, m := range res.Messages {
		if m.Result != nil {
			order = append(order, m.Result.Content)
		}
	}
	if peak != 2 || strings.Join(order, ",") != "a,b,c,a" {
		t.Fatalf("peak %d order %v", peak, order)
	}
}

// Audit 18/11: a cancel in the middle of a parallel batch lost the
// results of calls that had already run.
func TestCancelMidBatchKeepsFinishedResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ran sync.WaitGroup
	ran.Add(3)
	tool := func(name string) agent.Tool {
		return agent.NewTool(name, "x", func(context.Context, struct{}) (string, error) {
			ran.Done()
			ran.Wait() // all three have run their effect
			if name == "a" {
				cancel()
			}
			return name + " done", nil
		}, agent.ReadOnly())
	}
	reg, err := agent.NewRegistry(tool("a"), tool("b"), tool("c"))
	if err != nil {
		t.Fatal(err)
	}
	var calls []agent.ToolCall
	for _, n := range []string{"a", "b", "c"} {
		calls = append(calls, agent.ToolCall{Name: n, Input: json.RawMessage(`{}`)})
	}
	conv := agent.NewConversation()
	_, err = newAgent(t, agent.Config{Provider: fake.New(fake.Step{Calls: calls}, fake.Reply("ok")), Tools: reg}).Run(ctx, conv, agent.UserText("x"))
	if err == nil {
		t.Fatal("cancel not reported")
	}
	recorded := 0
	for _, m := range conv.Messages() {
		if m.Result != nil && strings.HasSuffix(m.Result.Content, "done") {
			recorded++
		}
	}
	if recorded != 3 {
		t.Fatalf("%d of 3 finished results recorded", recorded)
	}
}

// An empty answer (often a tool call the provider dropped for an unknown
// name) is asked again once, saying so; it is not recorded.
func TestEmptyAnswerIsAskedAgain(t *testing.T) {
	calls := 0
	p := fake.New(fake.Reply(""), fake.Reply("ok"))
	conv := agent.NewConversation()
	res, err := newAgent(t, agent.Config{Provider: p, Tools: tools(t, adder(&calls))}).Run(context.Background(), conv, agent.UserText("x"))
	if err != nil || res.Text != "ok" {
		t.Fatalf("result %q %v", res.Text, err)
	}
	reqs := p.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].System, "Your last reply was empty") || len(conv.Messages()) != 2 {
		t.Fatalf("%d requests, %d messages", len(reqs), len(conv.Messages()))
	}
}
