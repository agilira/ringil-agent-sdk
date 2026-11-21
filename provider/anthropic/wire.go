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
	type turn struct {
		assistant bool
		blocks    []anthropic.BetaContentBlockParamUnion
	}
	var turns []turn
	round := currentRound(msgs)
	for i, msg := range msgs {
		assistant := msg.Role == agent.RoleAssistant
		blocks := p.encodeMessage(msg, i >= round)
		if len(blocks) == 0 {
			continue
		}
		if n := len(turns); n > 0 && turns[n-1].assistant == assistant {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			continue
		}
		turns = append(turns, turn{assistant, blocks})
	}
	out := make([]anthropic.BetaMessageParam, len(turns))
	for i, t := range turns {
		if t.assistant {
			out[i] = anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: t.blocks}}
		} else {
			out[i] = anthropic.NewBetaUserMessage(t.blocks...)
		}
	}
	return out
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

func (p *provider) encodeMessage(msg agent.Message, inRound bool) []anthropic.BetaContentBlockParamUnion {
	switch msg.Role {
	case agent.RoleTool:
		if msg.Result == nil {
			return nil
		}
		return []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaToolResultBlock(msg.Result.CallID, msg.Result.Content, msg.Result.IsError)}
	case agent.RoleAssistant:
		return p.encodeAssistant(msg, inRound)
	default:
		return encodeUser(msg)
	}
}

// encodeUser sends text and images in their order.
func encodeUser(msg agent.Message) []anthropic.BetaContentBlockParamUnion {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		switch {
		case part.Type == agent.PartText && part.Text != "":
			out = append(out, anthropic.NewBetaTextBlock(part.Text))
		case part.Type == agent.PartImage:
			out = append(out, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
				Data: base64.StdEncoding.EncodeToString(part.Data), MediaType: anthropic.BetaBase64ImageSourceMediaType(part.MIME),
			}))
		}
	}
	return out
}

// encodeAssistant is the turn: in the current round, exactly as the model
// made it (its thinking, text and calls in their order), when it is the
// turn kept; else its text and calls, without thinking.
func (p *provider) encodeAssistant(msg agent.Message, inRound bool) []anthropic.BetaContentBlockParamUnion {
	if inRound && len(msg.Calls) > 0 {
		if turn := p.rounds.turn(msg.Calls[0].ID); sameCalls(turn, msg.Calls) {
			return turn
		}
	}
	var out []anthropic.BetaContentBlockParamUnion
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

// decode turns a finished message into the agent's response, keeping the
// whole turn, as the model made it, for the round its calls open (the API
// wants thinking back with the results, unchanged and in its place).
func (p *provider) decode(msg *anthropic.BetaMessage) *agent.Response {
	var text string
	var calls []agent.ToolCall
	thinks := false
	turn := make([]anthropic.BetaContentBlockParamUnion, 0, len(msg.Content))
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text += b.Text
		case anthropic.BetaToolUseBlock:
			calls = append(calls, decodeCall(b.ID, b.Name, json.RawMessage(b.JSON.Input.Raw())))
		case anthropic.BetaThinkingBlock, anthropic.BetaRedactedThinkingBlock:
			thinks = true
		}
		turn = append(turn, block.ToParam())
	}
	// Without block_binding a changed round would be refused, not its
	// thinking dropped: with NoThinking, none goes back.
	if len(calls) > 0 && thinks && !p.cfg.NoThinking {
		p.rounds.keep(calls[0].ID, turn)
	}
	out := agent.Message{Role: agent.RoleAssistant, Calls: calls}
	if text != "" {
		out.Parts = []agent.Part{{Type: agent.PartText, Text: text}}
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
