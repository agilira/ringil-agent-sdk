// wire.go: translation between agent types and the OpenAI wire format
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
)

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	MaxCompletion int            `json:"max_completion_tokens,omitempty"`
	Effort        string         `json:"reasoning_effort,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // *string, or []wirePart for a user message with images
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Reasoning  string         `json:"reasoning_content,omitempty"`
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
		Messages:    p.encodeMessages(req),
		Tools:       encodeTools(req.Tools),
		ToolChoice:  string(req.ToolChoice),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if wire.MaxTokens == 0 {
		wire.MaxTokens = p.cfg.MaxTokens
	}
	if p.cfg.MaxCompletionTokens {
		wire.MaxCompletion, wire.MaxTokens = wire.MaxTokens, 0
	}
	wire.Effort = p.cfg.ReasoningEffort
	if stream {
		wire.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(wire)
	if err != nil || len(p.cfg.Extra) == 0 {
		return body, err
	}
	return withExtra(body, p.cfg.Extra)
}

// withExtra adds the host's own fields to the request body; a field the
// provider sets itself is never replaced.
func withExtra(body []byte, extra map[string]any) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, set := fields[k]; set || ownFields[k] {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		fields[k] = raw
	}
	return json.Marshal(fields)
}

// ownFields are the body's fields only the provider writes, set or not.
var ownFields = map[string]bool{"model": true, "messages": true, "tools": true, "tool_choice": true, "stream": true,
	"stream_options": true, "max_tokens": true, "max_completion_tokens": true}

// encodeMessages is the conversation in the API's messages. The model's
// reasoning goes back to it only within the current round of tool calls,
// and only to the model that made it: that is what servers that send it
// want (DeepSeek's thinking mode with tools), and others refuse it after.
func (p *provider) encodeMessages(req *agent.Request) []wireMessage {
	out := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, wireMessage{Role: "system", Content: text(req.System)})
	}
	round := currentRound(req.Messages)
	var pending []agent.Part // images of the tool results just sent
	for i, msg := range req.Messages {
		if msg.Role != agent.RoleTool && len(pending) > 0 {
			out, pending = append(out, p.toolImages(pending)), nil
		}
		out = append(out, p.encodeTurn(msg, i >= round))
		if msg.Role == agent.RoleTool && msg.Result != nil {
			pending = append(pending, msg.Result.Images...)
		}
	}
	if len(pending) > 0 {
		out = append(out, p.toolImages(pending))
	}
	return out
}

// encodeTurn is msg; in the current round, an assistant message carries
// its reasoning back.
func (p *provider) encodeTurn(msg agent.Message, inRound bool) wireMessage {
	wire := encodeMessage(msg, p.cfg.Images)
	if inRound && msg.Role == agent.RoleAssistant {
		wire.Reasoning = joinReasoning(msg.Reasoning(p.source()))
	}
	return wire
}

// toolImages is the images tool results returned, as a user message after
// them: the API's tool messages carry text only.
func (p *provider) toolImages(images []agent.Part) wireMessage {
	if !p.cfg.Images { // the tools sent them, not the user (audit 21/11)
		return wireMessage{Role: "user", Content: text("[The tool results above returned " + strconv.Itoa(len(images)) + " images, which this model cannot see.]")}
	}
	return encodeUser(agent.UserMessage("[The images the tool results above returned.]", images...), p.cfg.Images)
}

// currentRound is where the current round of tool calls starts: after the
// last message the user wrote.
func currentRound(msgs []agent.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agent.RoleUser {
			return i + 1
		}
	}
	return 0
}

func joinReasoning(parts []agent.Part) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func text(s string) *string { return &s }

func encodeMessage(msg agent.Message, images bool) wireMessage {
	switch msg.Role {
	case agent.RoleTool:
		return encodeResult(msg.Result)
	case agent.RoleAssistant:
		return encodeAssistant(msg)
	case agent.RoleUser:
		return encodeUser(msg, images)
	case agent.RoleSystem:
		// Many servers' chat templates take a system message only first:
		// the host's note goes as a marked user message, which every one
		// takes.
		return wireMessage{Role: "user", Content: text("<system-reminder>\n" + msg.ModelText() + "\n</system-reminder>")}
	default:
		return wireMessage{Role: string(msg.Role), Content: text(msg.ModelText())}
	}
}

type wirePart struct {
	Type     string   `json:"type"`
	Text     string   `json:"text,omitempty"`
	ImageURL *wireURL `json:"image_url,omitempty"`
}

type wireURL struct {
	URL string `json:"url"`
}

// noImages tells a model that cannot see images that the user sent some,
// so that it says so instead of answering as if they were not there.
const noImages = "[The user sent images here, which this model cannot see: tell them.]"

// encodeUser sends a user message's images, in order with its text, as
// data URLs; to a model configured without images, a note in their place.
func encodeUser(msg agent.Message, images bool) wireMessage {
	if msg.Images() == 0 {
		return wireMessage{Role: "user", Content: text(msg.ModelText())}
	}
	if !images {
		return wireMessage{Role: "user", Content: text(strings.TrimSpace(msg.ModelText() + "\n" + noImages))}
	}
	parts := make([]wirePart, 0, len(msg.Parts))
	for _, p := range msg.Parts {
		switch p.Type {
		case agent.PartText, agent.PartContext:
			parts = append(parts, wirePart{Type: "text", Text: p.Text})
		case agent.PartImage:
			url := "data:" + p.MIME + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
			parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireURL{URL: url}})
		}
	}
	return wireMessage{Role: "user", Content: parts}
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

// assistant is the model's message: its reasoning (reasoning_content, as
// DeepSeek and others send it) first, then its text.
func assistant(reasoning, body string, calls []agent.ToolCall) agent.Message {
	msg := agent.Message{Role: agent.RoleAssistant, Calls: calls}
	if reasoning != "" {
		msg.Parts = append(msg.Parts, agent.Part{Type: agent.PartReasoning, Text: reasoning})
	}
	if body != "" {
		msg.Parts = append(msg.Parts, agent.Part{Type: agent.PartText, Text: body})
	}
	return msg
}

// source marks the reasoning this provider's model made.
func (p *provider) source() string { return "openai:" + p.cfg.Model }

// stamp marks m's reasoning as this model's.
func (p *provider) stamp(m *agent.Message) {
	for i := range m.Parts {
		if m.Parts[i].Type == agent.PartReasoning {
			m.Parts[i].Source = p.source()
		}
	}
}
