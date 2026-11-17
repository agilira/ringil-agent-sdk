// failure.go: classification of HTTP failures
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bytes"
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

func statusError(resp *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	code := resp.StatusCode
	if err := knownStatus(resp, bytes.ToLower(body)); err != nil {
		return withSecondary(err, readErr)
	}
	if code == http.StatusRequestTimeout || code >= 500 && code != http.StatusNotImplemented {
		err := agent.WithRetryAfter(errors.New(agent.ErrProviderUnavailable, "server error ("+strconv.Itoa(code)+")"), retryAfter(resp))
		return withSecondary(err.AsRetryable(), readErr)
	}
	return withSecondary(errors.New(agent.ErrProviderInput, "request refused ("+strconv.Itoa(code)+")"), readErr)
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
// fix an exhausted quota, so that one is not retryable.
func rateError(resp *http.Response, body []byte) error {
	if bytes.Contains(body, []byte("insufficient_quota")) {
		return errors.New(agent.ErrProviderRate, "quota exhausted")
	}
	return agent.WithRetryAfter(errors.New(agent.ErrProviderRate, "rate limited"), retryAfter(resp)).AsRetryable()
}

func retryAfter(resp *http.Response) time.Duration {
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

// withSecondary keeps a second failure without hiding the first one's code.
// errors.HasCode follows single-error chains only, so a joined error would
// lose the classification callers branch on; the secondary failure goes into
// the coded error's context instead.
func withSecondary(err, secondary error) error {
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
