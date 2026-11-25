// generate.go: one chat completion call
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/httpcall"
)

// maxBody bounds a non-streamed response. It protects memory against a
// hostile or broken server and sits far above any real completion.
const maxBody = 32 << 20

// Generate implements [agent.Provider]. A nil sink makes a plain request; a
// non-nil sink streams.
func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if req == nil {
		return nil, errors.New(agent.ErrProviderInput, "nil request")
	}
	body, err := p.encode(req, sink != nil)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderInput, "request does not encode")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	resp, err := p.send(ctx, body, cancel)
	if err != nil && p.formatRefused(req, err) {
		resp, err = p.sendInWords(ctx, req, sink != nil, cancel)
	}
	if err != nil {
		return httpcall.Finish(ctx, nil, err)
	}
	out, err := p.read(resp, sink, cancel)
	if out != nil {
		p.stamp(&out.Message)
		out.Usage.Cost = p.statedCost(out.Usage.Cost)
	}
	return httpcall.Finish(ctx, out, err)
}

// statedCost is the cost the server put in usage, when the host says it
// states one and it is an amount; else zero.
func (p *provider) statedCost(v float64) float64 {
	if !p.cfg.StatesCost || !(v >= 0) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// formatRefused: the request asked structured output, and the server
// refused it: many OpenAI-compatible models have none.
func (p *provider) formatRefused(req *agent.Request, err error) bool {
	var e *errors.Error
	if req.Format == nil || p.formatFlag(req).Load() || !stderrors.As(err, &e) || e.Code != agent.ErrProviderInput {
		return false
	}
	status, _ := e.Context["status"].(int)
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// formatFlag is the memory of a refused format for requests like req:
// with tools, or without.
func (p *provider) formatFlag(req *agent.Request) *atomic.Bool {
	if len(req.Tools) > 0 {
		return &p.unformattedTools
	}
	return &p.unformatted
}

// sendInWords sends the request again with the schema asked in words;
// when the server takes it, the provider asks so from then on. The answer
// is checked by Parse either way.
func (p *provider) sendInWords(ctx context.Context, req *agent.Request, stream bool, cancel context.CancelCauseFunc) (*http.Response, error) {
	body, err := p.encodeWith(req, stream, true)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderInput, "request does not encode")
	}
	resp, err := p.send(ctx, body, cancel)
	if err == nil {
		p.formatFlag(req).Store(true)
	}
	return resp, err
}

// read decodes the body and closes it. A close error is not dropped: it
// means the connection broke, so the call counts as interrupted even when
// the body looked complete.
func (p *provider) read(resp *http.Response, sink agent.TextSink, cancel context.CancelCauseFunc) (*agent.Response, error) {
	reader := httpcall.NewStallReader(resp.Body, p.stall, cancel)
	defer reader.Stop()
	var out *agent.Response
	var err error
	if sink == nil {
		out, err = decodeBody(reader)
	} else {
		out, err = decodeStream(reader, sink)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, httpcall.WithSecondary(err, httpcall.Interrupted(closeErr))
	}
	return out, err
}

// send posts the request; a server that takes it and sends no answer for
// the stall timeout is cut, as a silent body is.
func (p *provider) send(ctx context.Context, body []byte, cancel context.CancelCauseFunc) (*http.Response, error) {
	key, err := p.apiKey(ctx)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "request cannot be built")
	}
	p.setHeaders(httpReq, key)
	if p.stall > 0 {
		t := time.AfterFunc(p.stall, func() { cancel(httpcall.ErrStalled) })
		defer t.Stop()
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, httpcall.NetworkError(ctx, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, httpcall.WithSecondary(httpcall.StatusError(resp), httpcall.CloseError(resp))
	}
	return resp, nil
}

func (p *provider) setHeaders(req *http.Request, key string) {
	for name, value := range p.cfg.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   *string        `json:"content"`
			Reasoning *string        `json:"reasoning_content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

func decodeBody(r io.Reader) (*agent.Response, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, httpcall.Interrupted(err)
	}
	if len(data) > maxBody {
		return nil, errors.New(agent.ErrProviderResponse, "response body exceeds the size bound")
	}
	return parseBody(data)
}

func parseBody(data []byte) (*agent.Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, errors.New(agent.ErrProviderResponse, "response is not valid JSON")
	}
	if len(wire.Choices) == 0 {
		return nil, errors.New(agent.ErrProviderResponse, "response has no choices")
	}
	choice := wire.Choices[0]
	calls := make([]agent.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		calls = append(calls, decodeCall(call.ID, call.Function.Name, call.Function.Arguments))
	}
	body := ""
	if choice.Message.Content != nil {
		body = *choice.Message.Content
	}
	if len(calls) == 0 {
		calls = nil
	}
	reasoning := ""
	if choice.Message.Reasoning != nil {
		reasoning = *choice.Message.Reasoning
	}
	return &agent.Response{
		Message: assistant(reasoning, body, calls),
		Usage:   decodeUsage(wire.Usage),
		Stop:    decodeStop(choice.FinishReason, len(calls)),
	}, nil
}
