// live_test.go: tests against a real OpenAI-compatible endpoint
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

// Live tests run only with `make live`, which loads AGENT_LIVE_BASE_URL,
// AGENT_LIVE_API_KEY and AGENT_LIVE_MODEL from the git-ignored .env file.
// They use real models: they check behaviour the SDK guarantees (streaming,
// tool call assembly, result round trip), never the wording of an answer.

package openai

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/budget"
	"github.com/agilira/ringil-agent-sdk/provider/circuit"
	"github.com/agilira/ringil-agent-sdk/provider/ratelimit"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
)

var weather = agent.ToolSpec{
	Name:        "weather",
	Description: "Current temperature in a city, in Celsius",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}

// live builds the full decorator stack around the configured endpoint, as a
// host would, with a small budget so a misbehaving test cannot spend much.
func live(t *testing.T) (agent.Provider, budget.Budget) {
	t.Helper()
	base, model := os.Getenv("AGENT_LIVE_BASE_URL"), os.Getenv("AGENT_LIVE_MODEL")
	if base == "" || model == "" {
		t.Skip("AGENT_LIVE_BASE_URL and AGENT_LIVE_MODEL not set")
	}
	p, err := New(Config{BaseURL: base, Model: model, APIKey: os.Getenv("AGENT_LIVE_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	spend, err := budget.New(budget.Limits{Calls: 10})
	if err != nil {
		t.Fatal(err)
	}
	pace, err := ratelimit.New(ratelimit.Config{RequestsPerMinute: 30, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := circuit.New(circuit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return retry.Wrap(guard.Wrap(pace.Wrap(spend.Wrap(p))), retry.Config{}), spend
}

func liveContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveStreaming(t *testing.T) {
	p, spend := live(t)
	var deltas []string
	req := &agent.Request{Messages: []agent.Message{agent.UserText("Count from one to twenty in words, comma separated.")}}
	resp, err := p.Generate(liveContext(t), req, func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) < 2 || strings.Join(deltas, "") != resp.Message.Text() {
		t.Fatalf("streamed %q, returned %q", strings.Join(deltas, ""), resp.Message.Text())
	}
	t.Logf("deltas=%d stop=%s usage=%+v spent=%+v", len(deltas), resp.Stop, resp.Usage, spend.Spent())
}

func TestLivePlain(t *testing.T) {
	p, _ := live(t)
	req := &agent.Request{Messages: []agent.Message{agent.UserText("Reply with the word ok.")}}
	resp, err := p.Generate(liveContext(t), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() == "" || resp.Stop != agent.StopEnd {
		t.Fatalf("response %+v", resp)
	}
}

func TestLiveToolRoundTrip(t *testing.T) {
	p, _ := live(t)
	ctx := liveContext(t)
	req := &agent.Request{
		Messages: []agent.Message{agent.UserText("What is the temperature in Rome right now? Use the tool.")},
		Tools:    []agent.ToolSpec{weather},
	}
	first, err := p.Generate(ctx, req, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.Calls) == 0 || first.Stop != agent.StopToolUse {
		t.Fatalf("no tool call: %+v", first)
	}
	req.Messages = append(req.Messages, answerCalls(t, first.Message, "21")...)
	second, err := p.Generate(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Message.Text(), "21") {
		t.Fatalf("tool result not used: %q", second.Message.Text())
	}
}

// answerCalls checks the model's tool calls and answers each with result.
func answerCalls(t *testing.T, msg agent.Message, result string) []agent.Message {
	t.Helper()
	out := []agent.Message{msg}
	for _, call := range msg.Calls {
		if call.Name != "weather" || call.BadInput || call.ID == "" {
			t.Fatalf("call %+v", call)
		}
		out = append(out, agent.ToolMessage(agent.ToolResult{CallID: call.ID, Content: result}))
	}
	return out
}
