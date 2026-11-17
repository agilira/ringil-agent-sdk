// message.go: conversation messages, tool calls and usage
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"strings"
)

// Role identifies who produced a [Message].
type Role string

// Roles of a conversation. The system prompt is not a message: it travels in
// [Request.System], so providers that want it separate get it separate and
// host context never leaks into the stored conversation.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType identifies the kind of content a [Part] carries.
type PartType string

// PartText is plain text. Other part types (images, reasoning) will be added
// as new constants and fields; Part is a struct so that adding them does not
// break existing code.
const PartText PartType = "text"

// Part is one piece of message content.
type Part struct {
	Type PartType `json:"type"`
	Text string   `json:"text,omitempty"`
}

// ToolCall is a request from the model to run a tool.
//
// Input is always valid JSON. When the model produced arguments that are not
// valid JSON, the provider adapter sets Input to an empty object and BadInput
// to true, so the agent loop can ask the model to try again while the stored
// conversation stays serializable.
type ToolCall struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	BadInput bool            `json:"bad_input,omitempty"`
}

// ToolResult answers one [ToolCall].
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Message is one turn of a conversation. Assistant messages may carry tool
// calls; tool messages carry exactly one result.
type Message struct {
	Role   Role        `json:"role"`
	Parts  []Part      `json:"parts,omitempty"`
	Calls  []ToolCall  `json:"calls,omitempty"`
	Result *ToolResult `json:"result,omitempty"`
}

// UserText returns a user message holding text.
func UserText(text string) Message {
	return Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: text}}}
}

// AssistantText returns an assistant message holding text.
func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: text}}}
}

// ToolMessage returns a tool message holding result.
func ToolMessage(result ToolResult) Message {
	return Message{Role: RoleTool, Result: &result}
}

// Text concatenates the text parts of m.
func (m Message) Text() string {
	var b strings.Builder
	for _, part := range m.Parts {
		if part.Type == PartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Clone returns a deep copy of m, so a caller that keeps a message cannot be
// affected by later changes made by whoever produced it.
func (m Message) Clone() Message {
	m.Parts = append([]Part(nil), m.Parts...)
	if m.Calls != nil {
		calls := make([]ToolCall, len(m.Calls))
		for i, call := range m.Calls {
			call.Input = append(json.RawMessage(nil), call.Input...)
			calls[i] = call
		}
		m.Calls = calls
	}
	if m.Result != nil {
		result := *m.Result
		m.Result = &result
	}
	return m
}

// Usage reports the tokens a model call consumed, as the provider stated
// them. Zero means not reported.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

// Add returns the sum of u and other.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		Input:      u.Input + other.Input,
		Output:     u.Output + other.Output,
		CacheRead:  u.CacheRead + other.CacheRead,
		CacheWrite: u.CacheWrite + other.CacheWrite,
	}
}

// StopReason says why a model call or a run ended.
type StopReason string

// Stop reasons.
const (
	StopEnd       StopReason = "end"        // the model finished its answer
	StopToolUse   StopReason = "tool_use"   // the model asked for tools
	StopMaxTokens StopReason = "max_tokens" // the output token limit was reached
	StopFiltered  StopReason = "filtered"   // the provider withheld content
	StopMaxTurns  StopReason = "max_turns"  // the host's turn limit was reached
	StopCancelled StopReason = "cancelled"  // the context was cancelled
)
