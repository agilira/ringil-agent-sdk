// generate.go: one chat completion call
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
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
	resp, err := p.send(ctx, body)
	if err != nil {
		return nil, err
	}
	out, err := p.read(resp, sink, cancel)
	return finish(ctx, out, err)
}

// read decodes the body and closes it. A close error is not dropped: it
// means the connection broke, so the call counts as interrupted even when
// the body looked complete.
func (p *provider) read(resp *http.Response, sink agent.TextSink, cancel context.CancelCauseFunc) (*agent.Response, error) {
	reader := newStallReader(resp.Body, p.stall, cancel)
	defer reader.stop()
	var out *agent.Response
	var err error
	if sink == nil {
		out, err = decodeBody(reader)
	} else {
		out, err = decodeStream(reader, sink)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, withSecondary(err, interrupted(closeErr))
	}
	return out, err
}

func (p *provider) send(ctx context.Context, body []byte) (*http.Response, error) {
	key, err := p.apiKey(ctx)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "request cannot be built")
	}
	p.setHeaders(httpReq, key)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, networkError(ctx, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, withSecondary(statusError(resp), closeError(resp))
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

// finish turns a body error into the right classification: the caller's
// cancellation stays a context error, a stall becomes an interruption.
func finish(ctx context.Context, resp *agent.Response, err error) (*agent.Response, error) {
	if err == nil {
		return resp, nil
	}
	if context.Cause(ctx) == errStalled {
		return nil, errors.New(agent.ErrProviderInterrupted, "response stalled").AsRetryable()
	}
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, err
}

func closeError(resp *http.Response) error {
	if err := resp.Body.Close(); err != nil {
		return interrupted(err)
	}
	return nil
}

func networkError(ctx context.Context, err error) error {
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		return ctxErr
	}
	return errors.Wrap(err, agent.ErrProviderUnavailable, "request failed before a response").AsRetryable()
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

func decodeBody(r io.Reader) (*agent.Response, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, interrupted(err)
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
	return &agent.Response{
		Message: assistant(body, calls),
		Usage:   decodeUsage(wire.Usage),
		Stop:    decodeStop(choice.FinishReason, len(calls)),
	}, nil
}

func interrupted(err error) error {
	return errors.Wrap(err, agent.ErrProviderInterrupted, "response body was cut").AsRetryable()
}

// stallReader cancels the request when the body stays silent too long. A
// stalled TCP connection otherwise blocks forever: the server is alive enough
// to keep it open and never sends another byte.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

var errStalled = errors.New(agent.ErrProviderInterrupted, "response stalled")

func newStallReader(r io.Reader, d time.Duration, cancel context.CancelCauseFunc) *stallReader {
	s := &stallReader{r: r, d: d}
	if d > 0 {
		s.timer = time.AfterFunc(d, func() { cancel(errStalled) })
	}
	return s
}

func (s *stallReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 && s.timer != nil {
		s.timer.Reset(s.d)
	}
	return n, err
}

func (s *stallReader) stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}
