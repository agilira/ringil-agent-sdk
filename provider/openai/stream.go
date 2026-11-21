// stream.go: server-sent events of a streamed chat completion
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Bounds on a streamed response. They protect memory against a hostile or
// broken server and sit far above any real completion.
const (
	maxLine      = 8 << 20
	maxStreamed  = 64 << 20
	maxToolCalls = 1024
)

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string        `json:"content"`
			Reasoning *string        `json:"reasoning_content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type partialCall struct {
	id, name string
	args     strings.Builder
}

type streamState struct {
	sink      agent.TextSink
	text      strings.Builder
	reasoning strings.Builder
	calls     []*partialCall
	byIndex   map[int]*partialCall
	usage     agent.Usage
	finish    *string
	done      bool
	size      int
}

func decodeStream(r io.Reader, sink agent.TextSink) (*agent.Response, error) {
	s := &streamState{sink: sink, byIndex: map[int]*partialCall{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
	var data []byte
	for !s.done && scanner.Scan() {
		var err error
		if data, err = s.line(scanner.Bytes(), data); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, scanError(err)
	}
	if err := s.dispatch(data); err != nil {
		return nil, err
	}
	return s.result()
}

func scanError(err error) error {
	if err == bufio.ErrTooLong {
		return errors.New(agent.ErrProviderResponse, "stream line exceeds the size bound")
	}
	return interrupted(err)
}

// line handles one SSE line. Data lines accumulate until a blank line ends
// the event, as the SSE format specifies; comments and other fields are
// ignored.
func (s *streamState) line(line, data []byte) ([]byte, error) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		return nil, s.dispatch(data)
	}
	field, value, _ := bytes.Cut(line, []byte(":"))
	if string(field) != "data" {
		return data, nil
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if data != nil {
		data = append(data, '\n')
	}
	if len(data)+len(value) > maxLine {
		return nil, errors.New(agent.ErrProviderResponse, "stream event exceeds the size bound")
	}
	return append(data, value...), nil
}

func (s *streamState) dispatch(data []byte) error {
	if data == nil {
		return nil
	}
	if string(bytes.TrimSpace(data)) == "[DONE]" {
		s.done = true
		return nil
	}
	var chunk streamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return errors.New(agent.ErrProviderResponse, "stream event is not valid JSON")
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return errors.New(agent.ErrProviderUnavailable, "server reported an error mid-stream").AsRetryable()
	}
	if chunk.Usage != nil {
		s.usage = decodeUsage(chunk.Usage)
	}
	return s.choices(chunk)
}

func (s *streamState) choices(chunk streamChunk) error {
	if len(chunk.Choices) == 0 {
		return nil
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		s.finish = choice.FinishReason
	}
	for _, call := range choice.Delta.ToolCalls {
		if err := s.callDelta(call); err != nil {
			return err
		}
	}
	if err := s.reasoningDelta(choice.Delta.Reasoning); err != nil {
		return err
	}
	if choice.Delta.Content == nil || *choice.Delta.Content == "" {
		return nil
	}
	return s.textDelta(*choice.Delta.Content)
}

func (s *streamState) reasoningDelta(r *string) error {
	if r == nil || *r == "" {
		return nil
	}
	if err := s.grow(len(*r)); err != nil {
		return err
	}
	s.reasoning.WriteString(*r)
	return nil
}

func (s *streamState) grow(n int) error {
	s.size += n
	if s.size > maxStreamed {
		return errors.New(agent.ErrProviderResponse, "stream exceeds the size bound")
	}
	return nil
}

func (s *streamState) textDelta(delta string) error {
	if err := s.grow(len(delta)); err != nil {
		return err
	}
	s.text.WriteString(delta)
	return s.sink(delta)
}

// callDelta merges a tool call fragment. Fragments are keyed by index; some
// servers omit it when they send whole calls, so a missing index opens a new
// call when the fragment carries an id, and continues the last one otherwise.
func (s *streamState) callDelta(delta wireToolCall) error {
	if err := s.grow(len(delta.ID) + len(delta.Function.Name) + len(delta.Function.Arguments)); err != nil {
		return err
	}
	call, err := s.callFor(delta)
	if err != nil {
		return err
	}
	if delta.ID != "" {
		call.id = delta.ID
	}
	call.name += delta.Function.Name
	call.args.WriteString(delta.Function.Arguments)
	return nil
}

func (s *streamState) callFor(delta wireToolCall) (*partialCall, error) {
	if delta.Index != nil {
		if call, ok := s.byIndex[*delta.Index]; ok {
			return call, nil
		}
	} else if delta.ID == "" && len(s.calls) > 0 {
		return s.calls[len(s.calls)-1], nil
	}
	if len(s.calls) >= maxToolCalls {
		return nil, errors.New(agent.ErrProviderResponse, "stream has too many tool calls")
	}
	call := &partialCall{}
	s.calls = append(s.calls, call)
	if delta.Index != nil {
		s.byIndex[*delta.Index] = call
	}
	return call, nil
}

// result requires an end marker. A stream that just stops without a finish
// reason or [DONE] was cut, and its tool calls may be truncated.
func (s *streamState) result() (*agent.Response, error) {
	if s.finish == nil && !s.done {
		return nil, errors.New(agent.ErrProviderInterrupted, "stream ended without a finish marker").AsRetryable()
	}
	var calls []agent.ToolCall
	for _, call := range s.calls {
		calls = append(calls, decodeCall(call.id, call.name, call.args.String()))
	}
	finish := ""
	if s.finish != nil {
		finish = *s.finish
	}
	return &agent.Response{
		Message: assistant(s.reasoning.String(), s.text.String(), calls),
		Usage:   s.usage,
		Stop:    decodeStop(finish, len(calls)),
	}, nil
}
