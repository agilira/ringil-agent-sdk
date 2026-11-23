// provider.go: the contract between the agent and a model
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
)

// Provider generates one model response. Adapters live in their own packages
// (provider/openai, provider/fake) and translate between these types and a
// vendor wire format; no vendor type crosses this boundary.
//
// When sink is not nil the adapter streams provisional text to it as it
// arrives. Tool calls are never streamed: the adapter assembles them and
// returns them complete in the [Response]. An error from sink aborts the call
// and is returned as is.
//
// Errors carry the codes in errors.go. An adapter never puts an upstream
// response body into an error, because it may echo prompts or credentials.
type Provider interface {
	Generate(ctx context.Context, req *Request, sink TextSink) (*Response, error)
	Info() ModelInfo
}

// TextSink receives streamed text deltas.
type TextSink func(delta string) error

// ModelInfo describes the model behind a [Provider].
type ModelInfo struct {
	Name string
	// ContextWindow is the model's context size in tokens; zero means unknown.
	ContextWindow int
}

// Request is the input of one model call.
type Request struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
	// MaxTokens limits the output; zero leaves the choice to the adapter.
	MaxTokens int
	// Temperature is sent only when set.
	Temperature *float64
	// ToolChoice is "", "auto", "none" or "required"; empty means the
	// provider's default.
	ToolChoice ToolChoice
	// Format, when set, asks the answer in JSON of its schema: a provider
	// with structured output constrains it; the host's Parse checks it
	// either way.
	Format *Format
}

// ToolChoice tells the model whether it may, must or must not call tools.
type ToolChoice string

// Tool choices.
const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceNone     ToolChoice = "none"
	ToolChoiceRequired ToolChoice = "required"
)

// Response is the output of one model call.
type Response struct {
	Message Message
	Usage   Usage
	Stop    StopReason
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Annotations Annotations     `json:"annotations,omitzero"`
}

// Annotations are hints about a tool's effects. The agent loop uses them to
// run read-only tools in parallel and passes them to the host's approval hook;
// they are never sent to the model as permissions.
type Annotations struct {
	ReadOnly    bool `json:"read_only,omitempty"`
	Destructive bool `json:"destructive,omitempty"`
	OpenWorld   bool `json:"open_world,omitempty"`
	// Deferrable: the tool may be listed by name only (see Discovery).
	Deferrable bool `json:"deferrable,omitempty"`
	// Parallel: calls may run at the same time as other parallel or
	// read-only calls of the same turn, such as sub-agents.
	Parallel bool `json:"parallel,omitempty"`
	// Edits: the tool changes files of the workspace (accept-edits allows
	// it without asking).
	Edits bool `json:"edits,omitempty"`
	// SelfConfirms: the tool confines or confirms its own risky calls (bash
	// and skill scripts in the sandbox, asking for what leaves it), so the
	// policy does not ask again for the others; rules and modes still apply.
	SelfConfirms bool `json:"self_confirms,omitempty"`
	// Internal: the tool changes only the agent's own state (its to-do
	// list, its checks, its workers, its memory): it runs without asking,
	// in plan mode too. What others read (a published artifact) is Edits. A tool marked nothing asks, as one that may change
	// anything.
	Internal bool `json:"internal,omitempty"`
}

// Clone returns a deep copy of r.
func (r *Request) Clone() *Request {
	clone := *r
	if r.Messages != nil {
		clone.Messages = make([]Message, len(r.Messages))
		for i, msg := range r.Messages {
			clone.Messages[i] = msg.Clone()
		}
	}
	if r.Tools != nil {
		clone.Tools = make([]ToolSpec, len(r.Tools))
		for i, spec := range r.Tools {
			spec.InputSchema = append(json.RawMessage(nil), spec.InputSchema...)
			clone.Tools[i] = spec
		}
	}
	if r.Temperature != nil {
		temperature := *r.Temperature
		clone.Temperature = &temperature
	}
	if r.Format != nil {
		f := *r.Format
		f.Schema = append(json.RawMessage(nil), f.Schema...)
		clone.Format = &f
	}
	return &clone
}
