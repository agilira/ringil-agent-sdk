// wire.go: translation between agent types and the OpenAI wire format
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"crypto/rand"
	"encoding/json"

	agent "github.com/agilira/ringil-agent-sdk"
)

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	Details          *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

func (p *provider) encode(req *agent.Request, stream bool) ([]byte, error) {
	wire := wireRequest{
		Model:       p.cfg.Model,
		Messages:    encodeMessages(req),
		Tools:       encodeTools(req.Tools),
		ToolChoice:  string(req.ToolChoice),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if wire.MaxTokens == 0 {
		wire.MaxTokens = p.cfg.MaxTokens
	}
	if stream {
		wire.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return json.Marshal(wire)
}

func encodeMessages(req *agent.Request) []wireMessage {
	out := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, wireMessage{Role: "system", Content: text(req.System)})
	}
	for _, msg := range req.Messages {
		out = append(out, encodeMessage(msg))
	}
	return out
}

func text(s string) *string { return &s }

func encodeMessage(msg agent.Message) wireMessage {
	switch msg.Role {
	case agent.RoleTool:
		return encodeResult(msg.Result)
	case agent.RoleAssistant:
		return encodeAssistant(msg)
	default:
		return wireMessage{Role: string(msg.Role), Content: text(msg.Text())}
	}
}

func encodeResult(result *agent.ToolResult) wireMessage {
	if result == nil {
		return wireMessage{Role: "tool", Content: text("")}
	}
	return wireMessage{Role: "tool", Content: text(result.Content), ToolCallID: result.CallID}
}

func encodeAssistant(msg agent.Message) wireMessage {
	wire := wireMessage{Role: "assistant"}
	if body := msg.Text(); body != "" || len(msg.Calls) == 0 {
		wire.Content = text(body)
	}
	for _, call := range msg.Calls {
		args := string(call.Input)
		if args == "" {
			args = "{}"
		}
		wire.ToolCalls = append(wire.ToolCalls, wireToolCall{
			ID: call.ID, Type: "function", Function: wireFunction{Name: call.Name, Arguments: args},
		})
	}
	return wire
}

func encodeTools(specs []agent.ToolSpec) []wireTool {
	out := make([]wireTool, 0, len(specs))
	for _, spec := range specs {
		schema := spec.InputSchema
		if len(schema) == 0 {
			schema = emptySchema
		}
		out = append(out, wireTool{Type: "function", Function: wireToolDef{
			Name: spec.Name, Description: spec.Description, Parameters: schema,
		}})
	}
	return out
}

// decodeCall turns the model's raw arguments into an agent tool call. Models
// served by small runtimes regularly emit broken JSON; that is the model's
// mistake to correct, so it becomes BadInput rather than a failed call.
func decodeCall(id, name, args string) agent.ToolCall {
	if id == "" {
		id = "call_" + rand.Text()
	}
	call := agent.ToolCall{ID: id, Name: name, Input: json.RawMessage(args)}
	if args == "" {
		call.Input = json.RawMessage("{}")
	} else if !json.Valid(call.Input) {
		call.Input, call.BadInput = json.RawMessage("{}"), true
	}
	return call
}

func decodeUsage(u *wireUsage) agent.Usage {
	if u == nil {
		return agent.Usage{}
	}
	usage := agent.Usage{Input: max(u.PromptTokens, 0), Output: max(u.CompletionTokens, 0)}
	if u.Details != nil {
		usage.CacheRead = max(u.Details.CachedTokens, 0)
	}
	return usage
}

func decodeStop(finish string, calls int) agent.StopReason {
	if calls > 0 {
		return agent.StopToolUse
	}
	switch finish {
	case "length":
		return agent.StopMaxTokens
	case "content_filter":
		return agent.StopFiltered
	default:
		return agent.StopEnd
	}
}

func assistant(body string, calls []agent.ToolCall) agent.Message {
	msg := agent.Message{Role: agent.RoleAssistant, Calls: calls}
	if body != "" {
		msg.Parts = []agent.Part{{Type: agent.PartText, Text: body}}
	}
	return msg
}
