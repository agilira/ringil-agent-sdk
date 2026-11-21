// live_test.go: tests against the real Anthropic API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

// Live tests run only with the live tag and ANTHROPIC_API_KEY set; they
// use a small, cheap model (RINGIL_ANTHROPIC_MODEL, default
// claude-haiku-5-5) and check behaviour the SDK guarantees (streaming, the
// tool round trip with thinking, error codes), never an answer's wording.

package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

var weather = agent.ToolSpec{
	Name:        "weather",
	Description: "Current temperature in a city, in Celsius",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}

func live(t *testing.T, cfg Config) agent.Provider {
	t.Helper()
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	cfg.APIKey = key
	if cfg.Model == "" {
		cfg.Model = os.Getenv("RINGIL_ANTHROPIC_MODEL")
	}
	if cfg.Model == "" {
		cfg.Model = "claude-haiku-5-5"
	}
	cfg.MaxTokens = 2048
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	return c
}

func TestLivePlainAndStreamed(t *testing.T) {
	p := live(t, Config{Effort: "low"})
	req := &agent.Request{System: "Answer in one word.", Messages: []agent.Message{agent.UserText("Capital of France?")}}
	plain, err := p.Generate(ctx(t), req, nil)
	if err != nil || plain.Stop != agent.StopEnd || !strings.Contains(strings.ToLower(plain.Message.Text()), "paris") || plain.Usage.Input == 0 {
		t.Fatalf("plain: %+v %v", plain, err)
	}
	var streamed strings.Builder
	got, err := p.Generate(ctx(t), req, func(d string) error { streamed.WriteString(d); return nil })
	if err != nil || streamed.String() != got.Message.Text() || got.Usage.Output == 0 {
		t.Fatalf("streamed %q, message %+v: %v", streamed.String(), got, err)
	}
}

// A round of tool calls: the call, its result, the answer; thinking goes
// back with the result (the API refuses a round that leaves it out on
// models that require it, and drops nothing here).
func TestLiveToolRound(t *testing.T) {
	p := live(t, Config{Effort: "medium"})
	msgs := []agent.Message{agent.UserText("What's the temperature in Rome right now? Use the tool.")}
	req := &agent.Request{System: "You have a weather tool.", Messages: msgs, Tools: []agent.ToolSpec{weather}}
	first, err := p.Generate(ctx(t), req, nil)
	if err != nil || first.Stop != agent.StopToolUse || len(first.Message.Calls) != 1 || first.Message.Calls[0].Name != "weather" {
		t.Fatalf("first: %+v %v", first, err)
	}
	call := first.Message.Calls[0]
	req.Messages = append(req.Messages, first.Message, agent.ToolMessage(agent.ToolResult{CallID: call.ID, Content: "21 C, sunny"}))
	var streamed strings.Builder
	second, err := p.Generate(ctx(t), req, func(d string) error { streamed.WriteString(d); return nil })
	if err != nil || second.Stop != agent.StopEnd || !strings.Contains(second.Message.Text(), "21") {
		t.Fatalf("second: %+v %v", second, err)
	}
	nextTurn(t, p, req, second.Message)
}

// nextTurn: after the round the user writes again; the round's thinking
// is left out, and the API takes the history as it is.
func nextTurn(t *testing.T, p agent.Provider, req *agent.Request, answer agent.Message) {
	t.Helper()
	req.Messages = append(req.Messages, answer, agent.UserText("And in Paris? Use the tool."))
	third, err := p.Generate(ctx(t), req, nil)
	if err != nil || third.Stop != agent.StopToolUse {
		t.Fatalf("third: %+v %v", third, err)
	}
}

func TestLiveBadKey(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	p, err := New(Config{Model: "claude-haiku-5-5", APIKey: "sk-ant-not-a-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Generate(ctx(t), &agent.Request{Messages: []agent.Message{agent.UserText("hi")}}, nil)
	if !errors.HasCode(err, agent.ErrProviderAuth) || strings.Contains(err.Error(), "sk-ant") {
		t.Fatalf("bad key: %v", err)
	}
}
