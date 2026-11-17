// fake_test.go: tests for the scripted provider
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package fake

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func request(text string) *agent.Request {
	return &agent.Request{Messages: []agent.Message{agent.UserText(text)}}
}

func generate(t *testing.T, p agent.Provider, text string) *agent.Response {
	t.Helper()
	resp, err := p.Generate(context.Background(), request(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestReplyStreamsAndReturnsText(t *testing.T) {
	p := New(Stream("ci", "ao"))
	var got []string
	resp, err := p.Generate(context.Background(), request("x"), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "ci" || resp.Message.Text() != "ciao" || resp.Stop != agent.StopEnd {
		t.Fatalf("chunks %v, response %+v", got, resp)
	}
}

func TestNilSinkStillReturnsText(t *testing.T) {
	resp, err := New(Reply("ok")).Generate(context.Background(), request("x"), nil)
	if err != nil || resp.Message.Text() != "ok" || resp.Message.Role != agent.RoleAssistant {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestCallsAreNumberedAcrossSteps(t *testing.T) {
	p := New(Call("a", `{}`), Call("b", `{"x":1}`))
	first, second := generate(t, p, "1"), generate(t, p, "2")
	if first.Message.Calls[0].ID != "call_1" || second.Message.Calls[0].ID != "call_2" {
		t.Fatalf("ids %q %q", first.Message.Calls[0].ID, second.Message.Calls[0].ID)
	}
	if second.Stop != agent.StopToolUse || string(second.Message.Calls[0].Input) != `{"x":1}` {
		t.Fatalf("second %+v", second)
	}
}

func TestExplicitIDAndStopAreKept(t *testing.T) {
	step := Step{Calls: []agent.ToolCall{{ID: "mine", Name: "a", Input: []byte(`{}`)}}, Stop: agent.StopMaxTokens}
	resp := generate(t, New(step), "x")
	if resp.Message.Calls[0].ID != "mine" || resp.Stop != agent.StopMaxTokens {
		t.Fatalf("resp %+v", resp)
	}
}

func TestFailReturnsErrorAfterChunks(t *testing.T) {
	boom := stderrors.New("boom")
	var streamed string
	_, err := New(Step{Chunks: []string{"part"}, Err: boom}).Generate(context.Background(), request("x"),
		func(d string) error { streamed += d; return nil })
	if !stderrors.Is(err, boom) || streamed != "part" {
		t.Fatalf("err %v streamed %q", err, streamed)
	}
	if _, err := New(Fail(boom)).Generate(context.Background(), request("x"), nil); !stderrors.Is(err, boom) {
		t.Fatalf("Fail: %v", err)
	}
}

func TestSinkErrorAborts(t *testing.T) {
	stop := stderrors.New("consumer gone")
	_, err := New(Stream("a", "b")).Generate(context.Background(), request("x"), func(string) error { return stop })
	if !stderrors.Is(err, stop) {
		t.Fatalf("err %v", err)
	}
}

func TestExhaustedScript(t *testing.T) {
	_, err := New().Generate(context.Background(), request("x"), nil)
	if !errors.HasCode(err, agent.ErrProviderInput) {
		t.Fatalf("err %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := New(Reply("x"))
	if _, err := p.Generate(ctx, request("x"), nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if p.Remaining() != 1 {
		t.Fatal("a cancelled call consumed a step")
	}
}

func TestCancelDuringStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := New(Stream("a", "b")).Generate(ctx, request("x"), func(string) error { cancel(); return nil })
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestRequestsAreCopies(t *testing.T) {
	p := New(Reply("x"))
	req := request("orig")
	if _, err := p.Generate(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Parts[0].Text = "changed"
	got := p.Requests()
	got[0].System = "changed too"
	if p.Requests()[0].Messages[0].Text() != "orig" || p.Requests()[0].System != "" {
		t.Fatal("recorded requests share memory with caller")
	}
}

func TestInfo(t *testing.T) {
	p := New()
	if p.Info().Name != "fake" {
		t.Fatalf("default info %+v", p.Info())
	}
	if NewWithInfo(agent.ModelInfo{Name: "m", ContextWindow: 8}).Info().ContextWindow != 8 {
		t.Fatal("NewWithInfo ignored")
	}
}

func TestResponseCallsDoNotShareScript(t *testing.T) {
	step := Call("a", `{"k":1}`)
	resp := generate(t, New(step), "x")
	resp.Message.Calls[0].Input[2] = 'X'
	if string(step.Calls[0].Input) != `{"k":1}` {
		t.Fatal("response shares memory with the script")
	}
}

func TestConcurrentUse(t *testing.T) {
	const n = 32
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Call("a", `{}`)
	}
	p := New(steps...)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Generate(context.Background(), request("x"), nil); err != nil {
				t.Error(err)
			}
			if p.Info().Name != "fake" {
				t.Error("info changed")
			}
		}()
	}
	wg.Wait()
	if p.Remaining() != 0 || len(p.Requests()) != n {
		t.Fatalf("remaining %d requests %d", p.Remaining(), len(p.Requests()))
	}
}
