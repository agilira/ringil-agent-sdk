// stream.go: the events of a streamed response
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/httpcall"
)

// Bounds against a hostile or broken server, far above any real answer.
const (
	maxEvent    = 16 << 20
	maxStreamed = 64 << 20
	maxItems    = 1024
)

type wireEvent struct {
	Type     string        `json:"type"`
	Delta    string        `json:"delta"`
	Item     *wireOutput   `json:"item"`
	Response *wireResponse `json:"response"`
	Code     string        `json:"code"`
}

type wireOutput struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	Summary          []wireSummary `json:"summary"`
	EncryptedContent string        `json:"encrypted_content"`
}

type wireResponse struct {
	Usage *struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
		Cached *struct {
			Tokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

type streamState struct {
	sink      agent.TextSink
	source    string
	streamed  strings.Builder
	text      strings.Builder
	reasoning []agent.Part
	calls     []agent.ToolCall
	usage     agent.Usage
	stop      agent.StopReason
	done      bool
	size      int
}

func decodeStream(r io.Reader, sink agent.TextSink, source string) (*agent.Response, error) {
	s := &streamState{sink: sink, source: source}
	err := httpcall.Events(r, maxEvent, func(data []byte) (bool, error) {
		if s.size += len(data); s.size > maxStreamed {
			return true, errors.New(agent.ErrProviderResponse, "the response exceeds the size bound")
		}
		if err := s.event(data); err != nil {
			return true, err
		}
		return s.done, nil
	})
	if err != nil {
		return nil, err
	}
	if !s.done {
		return nil, errors.New(agent.ErrProviderInterrupted, "the stream ended before the response did").AsRetryable()
	}
	return s.result(), nil
}

func (s *streamState) event(data []byte) error {
	var ev wireEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return errors.New(agent.ErrProviderResponse, "stream event is not valid JSON")
	}
	switch ev.Type {
	case "response.output_text.delta":
		s.streamed.WriteString(ev.Delta)
		if s.sink != nil && ev.Delta != "" {
			return s.sink(ev.Delta)
		}
	case "response.output_item.done":
		return s.item(ev.Item)
	case "response.completed":
		s.finish(ev.Response, agent.StopEnd)
	case "response.incomplete":
		s.finish(ev.Response, incompleteStop(ev.Response))
	case "response.failed", "error":
		return failure(ev)
	}
	return nil
}

// item takes one finished output item: the answer's text, a call, or
// the reasoning, kept encrypted with its summary.
func (s *streamState) item(it *wireOutput) error {
	if it == nil {
		return nil
	}
	if len(s.calls)+len(s.reasoning) >= maxItems {
		return errors.New(agent.ErrProviderResponse, "the response has too many items")
	}
	switch it.Type {
	case "message":
		for _, c := range it.Content {
			if c.Type == "output_text" {
				s.text.WriteString(c.Text)
			}
		}
	case "function_call":
		s.calls = append(s.calls, decodeCall(it.CallID, it.Name, it.Arguments))
	case "reasoning":
		s.addReasoning(it)
	}
	return nil
}

// addReasoning keeps the thinking encrypted, as given, with its summary.
func (s *streamState) addReasoning(it *wireOutput) {
	var summary []string
	for _, part := range it.Summary {
		summary = append(summary, part.Text)
	}
	if it.EncryptedContent != "" || len(summary) > 0 {
		s.reasoning = append(s.reasoning, agent.Part{Type: agent.PartReasoning, Text: strings.Join(summary, "\n\n"), Signature: it.EncryptedContent, Source: s.source})
	}
}

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

func (s *streamState) finish(r *wireResponse, stop agent.StopReason) {
	s.done, s.stop = true, stop
	if r == nil || r.Usage == nil {
		return
	}
	s.usage = agent.Usage{Input: max(r.Usage.Input, 0), Output: max(r.Usage.Output, 0)}
	if r.Usage.Cached != nil {
		s.usage.CacheRead = max(r.Usage.Cached.Tokens, 0)
	}
}

func incompleteStop(r *wireResponse) agent.StopReason {
	if r != nil && r.Incomplete != nil && r.Incomplete.Reason == "content_filter" {
		return agent.StopFiltered
	}
	return agent.StopMaxTokens
}

// failure classifies a failed response by its code, never its message.
func failure(ev wireEvent) error {
	code := ev.Code
	if ev.Response != nil && ev.Response.Error != nil {
		code = ev.Response.Error.Code
	}
	switch code {
	case "context_length_exceeded":
		return errors.New(agent.ErrProviderContext, "request exceeds the model context")
	case "usage_limit_reached", "insufficient_quota":
		return errors.New(agent.ErrProviderRate, "the subscription's usage limit is reached")
	case "rate_limit_exceeded":
		return errors.New(agent.ErrProviderRate, "rate limited").AsRetryable()
	case "invalid_prompt", "invalid_request_error":
		return errors.New(agent.ErrProviderInput, "the request was refused ("+code+")")
	}
	return errors.New(agent.ErrProviderUnavailable, "the response failed ("+safeCode(code)+")").AsRetryable()
}

func (s *streamState) result() *agent.Response {
	text := s.text.String()
	if text == "" {
		text = s.streamed.String()
	}
	msg := agent.Message{Role: agent.RoleAssistant, Parts: s.reasoning, Calls: s.calls}
	if text != "" {
		msg.Parts = append(msg.Parts, agent.Part{Type: agent.PartText, Text: text})
	}
	stop := s.stop
	if len(s.calls) > 0 {
		stop = agent.StopToolUse
	}
	return &agent.Response{Message: msg, Usage: s.usage, Stop: stop}
}
