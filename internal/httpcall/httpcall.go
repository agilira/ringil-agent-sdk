// httpcall.go: what every HTTP model provider shares
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package httpcall classifies the failures of an HTTP call to a model API
// and guards its body: upstream bodies are read only to classify, never
// returned (they may echo the prompt or a credential); a body that stays
// silent is cut; server-sent events are split with bounds.
package httpcall

import (
	"bufio"
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// maxErrorBody bounds what is read of an error body. The body is only
// inspected for classification and never returned: it may echo the prompt.
const maxErrorBody = 64 << 10

var contextMarkers = [][]byte{
	[]byte("context_length_exceeded"),
	[]byte("maximum context length"),
	[]byte("context window"),
	[]byte("context length"),
	[]byte("prompt is too long"),
	[]byte("too many tokens"),
	[]byte("exceeds the context"),
}

// StatusError classifies a response that is not 2xx.
func StatusError(resp *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	code := resp.StatusCode
	if err := knownStatus(resp, bytes.ToLower(body)); err != nil {
		return WithSecondary(err, readErr)
	}
	if code == http.StatusRequestTimeout || code >= 500 && code != http.StatusNotImplemented {
		err := agent.WithRetryAfter(errors.New(agent.ErrProviderUnavailable, "server error ("+strconv.Itoa(code)+")"), RetryAfter(resp))
		return WithSecondary(err.AsRetryable(), readErr)
	}
	return WithSecondary(errors.New(agent.ErrProviderInput, "request refused ("+strconv.Itoa(code)+")").WithContext("status", code), readErr)
}

func knownStatus(resp *http.Response, body []byte) error {
	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return errors.New(agent.ErrProviderAuth, "credential refused ("+strconv.Itoa(code)+")")
	case code == http.StatusTooManyRequests:
		return rateError(resp, body)
	case code == http.StatusRequestEntityTooLarge || isContextError(code, body):
		return errors.New(agent.ErrProviderContext, "request exceeds the model context")
	case code == http.StatusNotFound:
		return errors.New(agent.ErrProviderConfig, "endpoint or model not found (404)")
	}
	return nil
}

func isContextError(code int, body []byte) bool {
	if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		return false
	}
	for _, marker := range contextMarkers {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

// rateError separates "slow down" from "out of credit": waiting does not
// fix an exhausted quota, nor a subscription's usage cap (ChatGPT's
// usage_limit_reached, reset after hours), so those are not retryable.
func rateError(resp *http.Response, body []byte) error {
	if bytes.Contains(body, []byte("insufficient_quota")) || bytes.Contains(body, []byte("usage_limit_reached")) {
		return errors.New(agent.ErrProviderRate, "quota or usage limit exhausted")
	}
	return agent.WithRetryAfter(errors.New(agent.ErrProviderRate, "rate limited"), RetryAfter(resp)).AsRetryable()
}

// RetryAfter is the wait a response asks for, if any.
func RetryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(max(seconds, 0)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

// WithSecondary keeps a second failure without hiding the first one's code.
// errors.HasCode follows single-error chains only, so a joined error would
// lose the classification callers branch on; the secondary failure goes into
// the coded error's context instead.
func WithSecondary(err, secondary error) error {
	if secondary == nil {
		return err
	}
	if err == nil {
		return secondary
	}
	var coded *errors.Error
	if stderrors.As(err, &coded) {
		return coded.WithContext("secondary", secondary.Error())
	}
	return stderrors.Join(err, secondary)
}

// Finish turns a body error into the right classification: the caller's
// cancellation stays a context error, a stall becomes an interruption.
func Finish(ctx context.Context, resp *agent.Response, err error) (*agent.Response, error) {
	if err == nil {
		return resp, nil
	}
	if context.Cause(ctx) == ErrStalled {
		return nil, errors.New(agent.ErrProviderInterrupted, "response stalled").AsRetryable()
	}
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, err
}

// CloseError closes resp's body; a failure means the connection broke.
func CloseError(resp *http.Response) error {
	if err := resp.Body.Close(); err != nil {
		return Interrupted(err)
	}
	return nil
}

// NetworkError: no response came; the caller's cancellation stays its own.
func NetworkError(ctx context.Context, err error) error {
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		return ctxErr
	}
	return errors.Wrap(err, agent.ErrProviderUnavailable, "request failed before a response").AsRetryable()
}

// Interrupted: the body broke mid-way; retryable.
func Interrupted(err error) error {
	return errors.Wrap(err, agent.ErrProviderInterrupted, "response body was cut").AsRetryable()
}

// StallReader cancels the request when the body stays silent too long. A
// stalled TCP connection otherwise blocks forever: the server is alive enough
// to keep it open and never sends another byte.
type StallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

// ErrStalled is the cause a stalled request is cancelled with.
var ErrStalled = errors.New(agent.ErrProviderInterrupted, "response stalled")

// NewStallReader cuts r, by cancel, when it stays silent for d (0 or
// less: never).
func NewStallReader(r io.Reader, d time.Duration, cancel context.CancelCauseFunc) *StallReader {
	s := &StallReader{r: r, d: d}
	if d > 0 {
		s.timer = time.AfterFunc(d, func() { cancel(ErrStalled) })
	}
	return s
}

func (s *StallReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 && s.timer != nil {
		s.timer.Reset(s.d)
	}
	return n, err
}

// Stop disarms the timer.
func (s *StallReader) Stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

// Events splits server-sent events: each event's data lines, joined, go
// to fn, which says when the stream is done. Comments and other fields
// are skipped. An event larger than maxEvent is refused.
func Events(r io.Reader, maxEvent int, fn func(data []byte) (done bool, err error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEvent)
	var data []byte
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			if done, err := dispatch(data, fn); done || err != nil {
				return err
			}
			data = nil
			continue
		}
		var err error
		if data, err = addData(data, line, maxEvent); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return errors.New(agent.ErrProviderResponse, "stream line exceeds the size bound")
		}
		return Interrupted(err)
	}
	_, err := dispatch(data, fn)
	return err
}

// addData adds a data line's value to the event's; other lines are
// skipped.
func addData(data, line []byte, maxEvent int) ([]byte, error) {
	field, value, _ := bytes.Cut(line, []byte(":"))
	if string(field) != "data" {
		return data, nil
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if data != nil {
		data = append(data, '\n')
	}
	if len(data)+len(value) > maxEvent {
		return nil, errors.New(agent.ErrProviderResponse, "stream event exceeds the size bound")
	}
	return append(data, value...), nil
}

func dispatch(data []byte, fn func([]byte) (bool, error)) (bool, error) {
	if data == nil {
		return false, nil
	}
	return fn(data)
}
