// wire.go: translation between agent types and the Messages API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"

	"github.com/agilira/go-errors"
	"github.com/anthropics/anthropic-sdk-go/v2"

	agent "github.com/agilira/ringil-agent-sdk"
)

// betas are the beta features every request uses: the controls that let
// a thinking block the API finds invalid be dropped instead of refused.
var betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaThinkingBindingControls2026_08_01}

// params is the request for req.
func (p *provider) params(req *agent.Request) (anthropic.BetaMessageNewParams, error) {
	tools, err := encodeTools(req.Tools)
	if err != nil {
		return anthropic.BetaMessageNewParams{}, err
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(p.cfg.Model),
		MaxTokens: int64(p.maxTokens(req)),
		Messages:  p.encodeMessages(req.Messages),
		Tools:     tools,
		Betas:     betas,
		// The prefix up to the last message is cached: in an agent loop
		// each request repeats the previous one and adds to it.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if req.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: req.System}}
	}
	if choice, ok := p.toolChoice(req.ToolChoice); ok {
		params.ToolChoice = choice
	}
	if req.ToolChoice == agent.ToolChoiceRequired && p.unforced.Load() {
		params.Messages = append(params.Messages, mustCallTools)
	}
	if p.cfg.Effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(p.cfg.Effort)}
	}
	if !p.cfg.NoThinking {
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			BlockBinding: anthropic.BetaThinkingBlockBindingParam{PrefixMismatchBehavior: anthropic.BetaThinkingPrefixMismatchBehaviorDropBlock},
		}}
	}
	return params, nil
}

func (p *provider) maxTokens(req *agent.Request) int {
	switch {
	case req.MaxTokens > 0:
		return req.MaxTokens
	case p.cfg.MaxTokens > 0:
		return p.cfg.MaxTokens
	}
	return defaultMaxTokens
}

// mustCallTools asks in words for what a model that refuses a forced tool
// choice will not be forced to: a mid-conversation system message, after
// the last turn, as the API advises.
var mustCallTools = anthropic.BetaMessageParam{OfSystem: &anthropic.BetaSystemMessageParam{
	Content: []anthropic.BetaSystemMessageParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{
		Text: "This turn must call one of the tools: call the one that fits, and do not answer in text instead.",
	}}},
}}

func (p *provider) toolChoice(choice agent.ToolChoice) (anthropic.BetaToolChoiceUnionParam, bool) {
	if choice == agent.ToolChoiceRequired && p.unforced.Load() {
		choice = agent.ToolChoiceAuto
	}
	switch choice {
	case agent.ToolChoiceAuto:
		return anthropic.BetaToolChoiceUnionParam{OfAuto: &anthropic.BetaToolChoiceAutoParam{}}, true
	case agent.ToolChoiceNone:
		return anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}, true
	case agent.ToolChoiceRequired:
		return anthropic.BetaToolChoiceUnionParam{OfAny: &anthropic.BetaToolChoiceAnyParam{}}, true
	}
	return anthropic.BetaToolChoiceUnionParam{}, false
}

// encodeTools sends each tool's schema as it is: its properties and
// required list, and whatever else it says ($defs, additionalProperties).
func encodeTools(specs []agent.ToolSpec) ([]anthropic.BetaToolUnionParam, error) {
	out := make([]anthropic.BetaToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		schema, err := inputSchema(spec.InputSchema)
		if err != nil {
			return nil, errors.Wrap(err, agent.ErrProviderInput, "tool "+spec.Name+" has a schema that is not a JSON object")
		}
		tool := anthropic.BetaToolParam{Name: spec.Name, InputSchema: schema}
		if spec.Description != "" {
			tool.Description = anthropic.String(spec.Description)
		}
		out = append(out, anthropic.BetaToolUnionParam{OfTool: &tool})
	}
	return out, nil
}

func inputSchema(raw json.RawMessage) (anthropic.BetaToolInputSchemaParam, error) {
	schema := anthropic.BetaToolInputSchemaParam{Properties: map[string]any{}}
	if len(raw) == 0 {
		return schema, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return schema, err
	}
	if props, ok := fields["properties"]; ok {
		schema.Properties = props
	}
	if req, ok := fields["required"]; ok {
		if err := json.Unmarshal(req, &schema.Required); err != nil {
			return schema, err
		}
	}
	schema.ExtraFields = extras(fields)
	return schema, nil
}

// extras is what a schema says besides its type, properties and required.
func extras(fields map[string]json.RawMessage) map[string]any {
	var out map[string]any
	for name, value := range fields {
		if name == "properties" || name == "required" || name == "type" {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[name] = value
	}
	return out
}

// encodeMessages turns the conversation into the API's turns. Tool results
// travel as user turns, all the results of a round in one (and the text
// that follows them in the same turn, as the API asks); the thinking of
// the current round of tool calls goes back with the turn that made it.
func (p *provider) encodeMessages(msgs []agent.Message) []anthropic.BetaMessageParam {
	msgs = fitImages(msgs)
	var turns []turn
	for _, msg := range msgs {
		if msg.Role == agent.RoleSystem {
			turns = appendSystem(turns, msg)
			continue
		}
		role := roleUser
		if msg.Role == agent.RoleAssistant {
			role = roleAssistant
		}
		blocks := p.encodeMessage(msg)
		if len(blocks) == 0 {
			continue
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			continue
		}
		turns = append(turns, turn{role: role, blocks: blocks})
	}
	turns = placeSystem(turns)
	out := make([]anthropic.BetaMessageParam, len(turns))
	for i, t := range turns {
		out[i] = t.param()
	}
	return out
}

// placeSystem keeps a system message where the API takes it (last, or
// before the model's turn); any other becomes a marked note at the start
// of the user turn after it.
func placeSystem(turns []turn) []turn {
	out := make([]turn, 0, len(turns))
	for i, t := range turns {
		if t.role == roleSystem && i+1 < len(turns) && turns[i+1].role != roleAssistant {
			note := anthropic.NewBetaTextBlock("<system-reminder>\n" + t.system + "\n</system-reminder>")
			turns[i+1].blocks = append([]anthropic.BetaContentBlockParamUnion{note}, turns[i+1].blocks...)
			continue
		}
		if n := len(out); n > 0 && t.role == roleUser && out[n-1].role == roleUser {
			// A note moved into the turn after a tool result: one user
			// turn, the results first, as the API wants (audit 21/11).
			out[n-1].blocks = append(out[n-1].blocks, t.blocks...)
			continue
		}
		out = append(out, t)
	}
	return out
}

type turnRole int

const (
	roleUser turnRole = iota
	roleAssistant
	roleSystem
)

type turn struct {
	role   turnRole
	blocks []anthropic.BetaContentBlockParamUnion
	system string
}

func (t turn) param() anthropic.BetaMessageParam {
	switch t.role {
	case roleAssistant:
		return anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: t.blocks}}
	case roleSystem:
		return anthropic.BetaMessageParam{OfSystem: &anthropic.BetaSystemMessageParam{
			Content: []anthropic.BetaSystemMessageParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: t.system}}},
		}}
	}
	return anthropic.NewBetaUserMessage(t.blocks...)
}

// appendSystem adds a host's mid-conversation message: a system message
// where the API takes one (after a user turn: a message, or tool
// results), else, first or after the model's turn, a marked note in a user
// turn.
func appendSystem(turns []turn, msg agent.Message) []turn {
	text := msg.ModelText()
	if n := len(turns); n > 0 && turns[n-1].role == roleUser {
		return append(turns, turn{role: roleSystem, system: text})
	}
	note := anthropic.NewBetaTextBlock("<system-reminder>\n" + text + "\n</system-reminder>")
	return append(turns, turn{role: roleUser, blocks: []anthropic.BetaContentBlockParamUnion{note}})
}

func (p *provider) encodeMessage(msg agent.Message) []anthropic.BetaContentBlockParamUnion {
	switch msg.Role {
	case agent.RoleTool:
		if msg.Result == nil {
			return nil
		}
		return []anthropic.BetaContentBlockParamUnion{toolResult(*msg.Result)}
	case agent.RoleAssistant:
		return p.encodeAssistant(msg)
	default:
		return encodeUser(msg)
	}
}

// toolResult is a tool's result: its text, and its images after it.
func toolResult(res agent.ToolResult) anthropic.BetaContentBlockParamUnion {
	block := anthropic.NewBetaToolResultBlock(res.CallID, res.Content, res.IsError)
	for _, img := range res.Images {
		block.OfToolResult.Content = append(block.OfToolResult.Content, anthropic.BetaToolResultBlockParamContentUnion{
			OfImage: &anthropic.BetaImageBlockParam{Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &anthropic.BetaBase64ImageSourceParam{
				Data: base64.StdEncoding.EncodeToString(img.Data), MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MIME),
			}}},
		})
	}
	return block
}

// encodeUser sends text and images in their order.
func encodeUser(msg agent.Message) []anthropic.BetaContentBlockParamUnion {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		switch {
		case (part.Type == agent.PartText || part.Type == agent.PartContext) && part.Text != "":
			out = append(out, anthropic.NewBetaTextBlock(part.Text))
		case part.Type == agent.PartImage:
			out = append(out, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
				Data: base64.StdEncoding.EncodeToString(part.Data), MediaType: anthropic.BetaBase64ImageSourceMediaType(part.MIME),
			}))
		}
	}
	return out
}

// encodeAssistant is the turn as the model made it: its thinking, text and
// calls in their order when this provider kept the turn (a round of tool
// calls), else its thinking (Claude's, from the message) before its text
// and calls. The thinking goes back at every turn, as the API keeps it:
// the agent's history only grows at its end, so it stays valid, and a
// block the API finds otherwise is dropped by it (block_binding). With
// NoThinking none goes back: without block_binding a changed history
// would be refused instead.
func (p *provider) encodeAssistant(msg agent.Message) []anthropic.BetaContentBlockParamUnion {
	if !p.cfg.NoThinking && len(msg.Calls) > 0 {
		if turn := p.rounds.turn(msg.Calls[0].ID); sameCalls(turn, msg.Calls) {
			return turn
		}
	}
	var out []anthropic.BetaContentBlockParamUnion
	if !p.cfg.NoThinking {
		out = append(out, encodeReasoning(msg.Reasoning(reasoningSource))...)
	}
	if text := msg.Text(); text != "" {
		out = append(out, anthropic.NewBetaTextBlock(text))
	}
	for _, call := range msg.Calls {
		input := call.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		out = append(out, anthropic.NewBetaToolUseBlock(call.ID, input, call.Name))
	}
	return out
}

// reasoningSource marks the reasoning parts Claude made.
const reasoningSource = "anthropic"

func encodeReasoning(parts []agent.Part) []anthropic.BetaContentBlockParamUnion {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(parts))
	for _, p := range parts {
		if p.Redacted {
			out = append(out, anthropic.BetaContentBlockParamUnion{OfRedactedThinking: &anthropic.BetaRedactedThinkingBlockParam{Data: p.Signature}})
			continue
		}
		out = append(out, anthropic.BetaContentBlockParamUnion{OfThinking: &anthropic.BetaThinkingBlockParam{Thinking: p.Text, Signature: p.Signature}})
	}
	return out
}

// sameCalls: turn holds exactly calls, in order (the agent's message is
// the one the turn was kept for).
func sameCalls(turn []anthropic.BetaContentBlockParamUnion, calls []agent.ToolCall) bool {
	if turn == nil {
		return false
	}
	i := 0
	for _, block := range turn {
		if block.OfToolUse == nil {
			continue
		}
		if i >= len(calls) || block.OfToolUse.ID != calls[i].ID {
			return false
		}
		i++
	}
	return i == len(calls)
}

// decode turns a finished message into the agent's response: its thinking
// as reasoning parts (so a saved conversation keeps it), and the whole turn,
// as the model made it, kept for the round its calls open (the API wants
// thinking back unchanged and in its place, which a message's parts do not
// keep when it is interleaved with the calls).
func (p *provider) decode(msg *anthropic.BetaMessage) *agent.Response {
	var text string
	var calls []agent.ToolCall
	var reasoning []agent.Part
	turn := make([]anthropic.BetaContentBlockParamUnion, 0, len(msg.Content))
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text += b.Text
		case anthropic.BetaToolUseBlock:
			calls = append(calls, decodeCall(b.ID, b.Name, json.RawMessage(b.JSON.Input.Raw())))
		case anthropic.BetaThinkingBlock:
			reasoning = append(reasoning, agent.Part{Type: agent.PartReasoning, Text: b.Thinking, Signature: b.Signature, Source: reasoningSource})
		case anthropic.BetaRedactedThinkingBlock:
			reasoning = append(reasoning, agent.Part{Type: agent.PartReasoning, Signature: b.Data, Redacted: true, Source: reasoningSource})
		}
		turn = append(turn, block.ToParam())
	}
	if len(calls) > 0 && len(reasoning) > 0 {
		p.rounds.keep(calls[0].ID, turn)
	}
	out := agent.Message{Role: agent.RoleAssistant, Calls: calls, Parts: reasoning}
	if text != "" {
		out.Parts = append(out.Parts, agent.Part{Type: agent.PartText, Text: text})
	}
	return &agent.Response{Message: out, Usage: decodeUsage(msg.Usage), Stop: decodeStop(msg.StopReason, len(calls))}
}

// decodeCall keeps the model's input as it came; input that is not a JSON
// object is the model's mistake to correct, so it becomes BadInput.
func decodeCall(id, name string, input json.RawMessage) agent.ToolCall {
	if id == "" {
		id = "toolu_" + rand.Text()
	}
	call := agent.ToolCall{ID: id, Name: name, Input: input}
	var object map[string]json.RawMessage
	if len(input) == 0 || json.Unmarshal(input, &object) != nil {
		call.Input, call.BadInput = json.RawMessage("{}"), len(input) > 0
	}
	return call
}

// decodeUsage counts the prompt whole, as agent.Usage does: the API counts
// the parts read from the cache and written to it apart from the rest.
func decodeUsage(u anthropic.BetaUsage) agent.Usage {
	read, write := int(max(u.CacheReadInputTokens, 0)), int(max(u.CacheCreationInputTokens, 0))
	return agent.Usage{
		Input:      int(max(u.InputTokens, 0)) + read + write,
		Output:     int(max(u.OutputTokens, 0)),
		CacheRead:  read,
		CacheWrite: write,
	}
}

func decodeStop(reason anthropic.BetaStopReason, calls int) agent.StopReason {
	if calls > 0 {
		return agent.StopToolUse
	}
	switch reason {
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		return agent.StopMaxTokens
	case anthropic.BetaStopReasonRefusal:
		return agent.StopFiltered
	}
	return agent.StopEnd
}
