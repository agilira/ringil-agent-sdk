// wire.go: a request in the Responses API's shape
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	agent "github.com/agilira/ringil-agent-sdk"
)

type wireRequest struct {
	Model             string         `json:"model"`
	Instructions      string         `json:"instructions,omitempty"`
	Input             []wireItem     `json:"input"`
	Tools             []wireTool     `json:"tools,omitempty"`
	ToolChoice        string         `json:"tool_choice"`
	ParallelToolCalls bool           `json:"parallel_tool_calls"`
	Reasoning         *wireReasoning `json:"reasoning,omitempty"`
	Store             bool           `json:"store"`
	Stream            bool           `json:"stream"`
	Include           []string       `json:"include"`
	PromptCacheKey    string         `json:"prompt_cache_key,omitempty"`
}

type wireReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// wireItem is one input item: a message, a call, its output, or
// reasoning; the fields of the others stay empty.
type wireItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role,omitempty"`
	Content []wireContent `json:"content,omitempty"`
	// function_call and function_call_output
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // a string or content, "" included
	// reasoning, sent back without its id: nothing is stored at OpenAI
	Summary          *[]wireSummary `json:"summary,omitempty"`
	EncryptedContent string         `json:"encrypted_content,omitempty"`
}

type wireContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type wireSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (p *provider) encode(req *agent.Request) ([]byte, error) {
	w := wireRequest{
		Model: p.cfg.Model, Instructions: req.System, Input: p.items(req.Messages),
		ToolChoice: "auto", ParallelToolCalls: true, Store: false, Stream: true,
		Include: []string{"reasoning.encrypted_content"},
		// A summary of the thinking, as Codex asks: the thinking itself
		// stays encrypted.
		Reasoning:      &wireReasoning{Effort: p.cfg.ReasoningEffort, Summary: "auto"},
		PromptCacheKey: cacheKey(req),
	}
	if req.ToolChoice != "" {
		w.ToolChoice = string(req.ToolChoice)
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, wireTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schema})
	}
	return json.Marshal(w)
}

// cacheKey names the conversation for OpenAI's prompt cache, as Codex
// does with its conversation's id: the same system prompt and first
// message are the same conversation, turn after turn, so its cached
// prefix is reused (a subscription's quota counts what is not cached).
// A hash: nothing of the text itself.
func cacheKey(req *agent.Request) string {
	h := sha256.New()
	h.Write([]byte(req.System))
	h.Write([]byte{0})
	if len(req.Messages) > 0 {
		h.Write([]byte(req.Messages[0].ModelText()))
	}
	return "ringil-" + hex.EncodeToString(h.Sum(nil)[:16])
}

// items is the conversation as input items. The host's mid-conversation
// instructions go as developer messages, which the Responses API has for
// them.
func (p *provider) items(msgs []agent.Message) []wireItem {
	out := make([]wireItem, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case agent.RoleUser:
			out = append(out, wireItem{Type: "message", Role: "user", Content: userContent(m.ModelText(), m.Parts)})
		case agent.RoleSystem:
			out = append(out, wireItem{Type: "message", Role: "developer", Content: []wireContent{{Type: "input_text", Text: m.ModelText()}}})
		case agent.RoleAssistant:
			out = append(out, p.assistantItems(m)...)
		case agent.RoleTool:
			if m.Result != nil {
				out = append(out, wireItem{Type: "function_call_output", CallID: m.Result.CallID, Output: toolOutput(*m.Result)})
			}
		}
	}
	return out
}

func userContent(text string, parts []agent.Part) []wireContent {
	var c []wireContent
	if text != "" {
		c = append(c, wireContent{Type: "input_text", Text: text})
	}
	for _, part := range parts {
		if part.Type == agent.PartImage {
			c = append(c, image(part))
		}
	}
	if len(c) == 0 {
		c = append(c, wireContent{Type: "input_text", Text: ""})
	}
	return c
}

func image(part agent.Part) wireContent {
	return wireContent{Type: "input_image", ImageURL: "data:" + part.MIME + ";base64," + base64.StdEncoding.EncodeToString(part.Data)}
}

// assistantItems are the model's turn: its reasoning (this model's only,
// encrypted, as it gave it), its text, its calls.
func (p *provider) assistantItems(m agent.Message) []wireItem {
	var out []wireItem
	for _, r := range m.Reasoning(p.source()) {
		if r.Signature == "" {
			continue
		}
		summary := []wireSummary{}
		if r.Text != "" {
			summary = append(summary, wireSummary{Type: "summary_text", Text: r.Text})
		}
		out = append(out, wireItem{Type: "reasoning", Summary: &summary, EncryptedContent: r.Signature})
	}
	if text := m.Text(); text != "" {
		out = append(out, wireItem{Type: "message", Role: "assistant", Content: []wireContent{{Type: "output_text", Text: text}}})
	}
	for _, call := range m.Calls {
		args := string(call.Input)
		if args == "" {
			args = "{}"
		}
		out = append(out, wireItem{Type: "function_call", CallID: call.ID, Name: call.Name, Arguments: args})
	}
	return out
}

// toolOutput is a tool's result: text, or text and images as content.
func toolOutput(r agent.ToolResult) json.RawMessage {
	text := r.Content
	if r.IsError && text == "" {
		text = "error"
	}
	var v any = text
	if len(r.Images) > 0 {
		c := []wireContent{{Type: "input_text", Text: text}}
		for _, img := range r.Images {
			c = append(c, image(img))
		}
		v = c
	}
	data, _ := json.Marshal(v) // strings and content always encode
	return data
}
