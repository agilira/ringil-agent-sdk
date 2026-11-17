// fake.go: a scripted provider for tests
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package fake provides a scripted [agent.Provider] for tests. Each call to
// Generate consumes the next [Step] of the script and records the request, so
// a test can drive an agent through an exact sequence of answers and then
// inspect what the agent sent.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Step is one scripted model answer.
type Step struct {
	// Chunks are streamed to the sink in order. When Message is empty they
	// also form the assistant's text.
	Chunks []string
	// Calls are returned as tool calls; an empty ID gets "call_<n>".
	Calls []agent.ToolCall
	// Stop overrides the stop reason; empty derives it from Calls.
	Stop  agent.StopReason
	Usage agent.Usage
	// Err is returned instead of a response, after streaming Chunks.
	Err error
}

// Reply is a step that streams text in one chunk and ends the turn.
func Reply(text string) Step { return Step{Chunks: []string{text}} }

// Stream is a step that streams text in several chunks and ends the turn.
func Stream(chunks ...string) Step { return Step{Chunks: chunks} }

// Call is a step that asks for one tool with the given JSON input.
func Call(name, input string) Step {
	return Step{Calls: []agent.ToolCall{{Name: name, Input: json.RawMessage(input)}}}
}

// Fail is a step that returns err.
func Fail(err error) Step { return Step{Err: err} }

// Script is a provider that replays scripted steps and records what it was
// asked.
type Script interface {
	agent.Provider
	// Requests returns copies of every request received so far.
	Requests() []*agent.Request
	// Remaining returns how many steps are left.
	Remaining() int
}

// script replays steps. It is safe for concurrent use.
type script struct {
	mu       sync.Mutex
	info     agent.ModelInfo
	steps    []Step
	requests []*agent.Request
	calls    int
}

// New returns a script that answers with steps, in order, as model "fake".
func New(steps ...Step) Script {
	return NewWithInfo(agent.ModelInfo{Name: "fake"}, steps...)
}

// NewWithInfo is New with the model information reported by Info.
func NewWithInfo(info agent.ModelInfo, steps ...Step) Script {
	return &script{info: info, steps: steps}
}

// Info implements [agent.Provider].
func (p *script) Info() agent.ModelInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

// Generate implements [agent.Provider].
func (p *script) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	step, err := p.next(req)
	if err != nil {
		return nil, err
	}
	if err := stream(ctx, step.Chunks, sink); err != nil {
		return nil, err
	}
	if step.Err != nil {
		return nil, step.Err
	}
	return p.response(step), nil
}

// Requests implements [Script].
func (p *script) Requests() []*agent.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*agent.Request, len(p.requests))
	for i, req := range p.requests {
		out[i] = req.Clone()
	}
	return out
}

// Remaining implements [Script].
func (p *script) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.steps)
}

func (p *script) next(req *agent.Request) (Step, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.steps) == 0 {
		return Step{}, errors.New(agent.ErrProviderInput, "fake provider script exhausted")
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	p.requests = append(p.requests, req.Clone())
	return step, nil
}

func stream(ctx context.Context, chunks []string, sink agent.TextSink) error {
	if sink == nil {
		return nil
	}
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sink(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (p *script) response(step Step) *agent.Response {
	msg := agent.Message{Role: agent.RoleAssistant}
	if text := strings.Join(step.Chunks, ""); text != "" {
		msg = agent.AssistantText(text)
	}
	msg.Calls = p.number(step.Calls)
	stop := step.Stop
	if stop == "" {
		stop = agent.StopEnd
		if len(msg.Calls) > 0 {
			stop = agent.StopToolUse
		}
	}
	return &agent.Response{Message: msg, Usage: step.Usage, Stop: stop}
}

func (p *script) number(calls []agent.ToolCall) []agent.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]agent.ToolCall, len(calls))
	for i, call := range calls {
		p.calls++
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d", p.calls)
		}
		call.Input = append(json.RawMessage(nil), call.Input...)
		out[i] = call
	}
	return out
}
