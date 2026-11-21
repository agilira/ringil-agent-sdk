// generate.go: one Messages API call, plain or streamed
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"
	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	agent "github.com/agilira/ringil-agent-sdk"
)

// maxBody bounds a response body, plain or streamed. It protects memory
// against a hostile or broken server and sits far above any real answer.
const maxBody = 64 << 20

// Generate implements [agent.Provider]. The call always streams, so that a
// long answer meets no HTTP timeout; a non-nil sink gets the text as it
// comes.
func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if req == nil {
		return nil, errors.New(agent.ErrProviderInput, "nil request")
	}
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}
	key, err := p.apiKey(ctx)
	if err != nil {
		return nil, err
	}
	msg, err := p.stream(ctx, params, key, sink)
	if p.refusedForcing(req, err) {
		msg, err = p.unforcedRetry(ctx, req, key, sink)
	}
	if err != nil {
		return nil, failure(ctx, err)
	}
	if msg == nil || msg.StopReason == "" { // every message the API ends says why
		return nil, errors.New(agent.ErrProviderResponse, "the answer is not a whole message")
	}
	return p.decode(msg), nil
}

// sinkError carries the sink's own error out of the stream, to be
// returned as is.
type sinkError struct{ err error }

func (e sinkError) Error() string { return e.err.Error() }

// stream reads the events, sends each piece of text to sink and builds
// the message.
func (p *provider) stream(ctx context.Context, params anthropic.BetaMessageNewParams, key string, sink agent.TextSink) (*anthropic.BetaMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := p.client.Beta.Messages.NewStreaming(ctx, params, option.WithAPIKey(key))
	defer func() { _ = events.Close() }()
	msg := &anthropic.BetaMessage{}
	for events.Next() {
		event := events.Current()
		if err := msg.Accumulate(event); err != nil {
			return nil, errors.Wrap(err, agent.ErrProviderResponse, "stream events out of order")
		}
		if err := forward(event, sink); err != nil {
			return nil, err
		}
	}
	if err := events.Err(); err != nil {
		return nil, err
	}
	if msg.StopReason == "" {
		return nil, errors.New(agent.ErrProviderInterrupted, "stream ended before the message did").AsRetryable()
	}
	return msg, nil
}

// forward sends the text an event carries to sink, when there is one.
func forward(event anthropic.BetaRawMessageStreamEventUnion, sink agent.TextSink) error {
	if sink == nil {
		return nil
	}
	delta, ok := event.AsAny().(anthropic.BetaRawContentBlockDeltaEvent)
	if !ok {
		return nil
	}
	if text, ok := delta.Delta.AsAny().(anthropic.BetaTextDelta); ok && text.Text != "" {
		if err := sink(text.Text); err != nil {
			return sinkError{err}
		}
	}
	return nil
}

// unforcedRetry asks again with "required" in words: this model takes no
// forced choice, now or later.
func (p *provider) unforcedRetry(ctx context.Context, req *agent.Request, key string, sink agent.TextSink) (*anthropic.BetaMessage, error) {
	p.unforced.Store(true)
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}
	return p.stream(ctx, params, key, sink)
}

// refusedForcing: the call failed because the model takes no forced tool
// choice, which it has not said before.
func (p *provider) refusedForcing(req *agent.Request, err error) bool {
	return err != nil && req.ToolChoice == agent.ToolChoiceRequired && !p.unforced.Load() && forcedRefused(err)
}

// forcedRefused: the model refused the request for its forced tool choice.
func forcedRefused(err error) bool {
	var api *anthropic.Error
	return stderrors.As(err, &api) && api.StatusCode == http.StatusBadRequest && strings.Contains(api.RawJSON(), "tool_choice")
}

// failure classifies an error into the agent's codes. The API's message is
// never kept: it may echo the prompt.
func failure(ctx context.Context, err error) error {
	var sink sinkError
	var api *anthropic.Error
	var coded *errors.Error
	switch {
	case stderrors.As(err, &sink):
		return sink.err
	case stderrors.Is(err, errStalled):
		return errors.New(agent.ErrProviderInterrupted, "response stalled").AsRetryable()
	case stderrors.Is(err, errTooLarge):
		return errors.New(agent.ErrProviderResponse, "response body exceeds the size bound")
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case stderrors.As(err, &api):
		return statusError(api)
	case stderrors.As(err, &coded):
		return err
	}
	return errors.New(agent.ErrProviderUnavailable, "request failed before a complete response").AsRetryable()
}

func statusError(api *anthropic.Error) error {
	code := api.StatusCode
	status := "(" + strconv.Itoa(code) + ")"
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return errors.New(agent.ErrProviderAuth, "credential refused "+status)
	case code == http.StatusTooManyRequests:
		return agent.WithRetryAfter(errors.New(agent.ErrProviderRate, "rate limited"), retryAfter(api.Response)).AsRetryable()
	case code == http.StatusRequestEntityTooLarge || code == http.StatusBadRequest && tooLong(api):
		return errors.New(agent.ErrProviderContext, "request exceeds the model context")
	case code == http.StatusNotFound:
		return errors.New(agent.ErrProviderConfig, "endpoint or model not found "+status)
	case serverSide(api):
		return agent.WithRetryAfter(errors.New(agent.ErrProviderUnavailable, "server error "+status), retryAfter(api.Response)).AsRetryable()
	}
	return errors.New(agent.ErrProviderInput, "request refused "+status)
}

// serverSide: the server failed or is overloaded (529), not the request;
// an error event in a stream that began well (its status a success) is
// the server's too, whatever its type.
func serverSide(api *anthropic.Error) bool {
	code := api.StatusCode
	return code == http.StatusRequestTimeout || code >= 500 || code < 400 || apiErrorType(api) == "overloaded_error"
}

// apiErrorType is the API's error type (overloaded_error, ...), when the
// body names one; an error mid-stream has the stream's status.
func apiErrorType(api *anthropic.Error) string {
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(api.RawJSON()), &body)
	return body.Error.Type
}

// tooLong: the API says the prompt does not fit, in its words.
func tooLong(api *anthropic.Error) bool {
	raw := strings.ToLower(api.RawJSON())
	return strings.Contains(raw, "prompt is too long") || strings.Contains(raw, "context window") || strings.Contains(raw, "too many tokens")
}

func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(max(seconds, 0)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

var (
	errStalled  = stderrors.New("response stalled")
	errTooLarge = stderrors.New("response body exceeds the size bound")
)

// guard bounds every response body and cuts one that stays silent for
// stall (when positive): a stalled connection otherwise blocks forever.
func guard(stall time.Duration) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		ctx, cancel := context.WithCancel(req.Context())
		var waited atomic.Bool
		if stall > 0 { // a server that takes the request and never answers
			t := time.AfterFunc(stall, func() { waited.Store(true); cancel() })
			defer t.Stop()
		}
		resp, err := next(req.WithContext(ctx))
		if err != nil {
			cancel()
			if waited.Load() {
				return resp, errStalled
			}
			return resp, err
		}
		resp.Body = newGuardedBody(resp.Body, stall, cancel)
		return resp, nil
	}
}

type guardedBody struct {
	rc      io.ReadCloser
	left    int64
	stall   time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	once    sync.Once
	stalled chan struct{}
}

func newGuardedBody(rc io.ReadCloser, stall time.Duration, cancel context.CancelFunc) *guardedBody {
	b := &guardedBody{rc: rc, left: maxBody, stall: stall, cancel: cancel, stalled: make(chan struct{})}
	if stall > 0 {
		b.timer = time.AfterFunc(stall, func() { b.once.Do(func() { close(b.stalled) }); cancel() })
	}
	return b
}

func (b *guardedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if n > 0 && b.timer != nil {
		b.timer.Reset(b.stall)
	}
	if err != nil && err != io.EOF && b.didStall() {
		return n, errStalled
	}
	return n, err
}

func (b *guardedBody) didStall() bool {
	select {
	case <-b.stalled:
		return true
	default:
		return false
	}
}

func (b *guardedBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel()
	return b.rc.Close()
}
