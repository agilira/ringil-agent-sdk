// errors.go: error codes shared by the agent and its adapters
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	stderrors "errors"
	"time"

	"github.com/agilira/go-errors"
)

// Provider error codes. Adapters classify every failure into one of these, so
// hosts and decorators react to the kind of failure, never to vendor text.
const (
	// ErrProviderConfig: the adapter was configured badly.
	ErrProviderConfig errors.ErrorCode = "PROVIDER_CONFIG"
	// ErrProviderAuth: the credential is missing or was refused.
	ErrProviderAuth errors.ErrorCode = "PROVIDER_AUTH"
	// ErrProviderRate: the provider asked to slow down. Retryable.
	ErrProviderRate errors.ErrorCode = "PROVIDER_RATE"
	// ErrProviderUnavailable: server error or network failure before a
	// response arrived. Retryable.
	ErrProviderUnavailable errors.ErrorCode = "PROVIDER_UNAVAILABLE"
	// ErrProviderContext: the request does not fit the model's context.
	ErrProviderContext errors.ErrorCode = "PROVIDER_CONTEXT"
	// ErrProviderInput: the provider refused the request.
	ErrProviderInput errors.ErrorCode = "PROVIDER_INPUT"
	// ErrProviderResponse: the provider answered with something malformed.
	ErrProviderResponse errors.ErrorCode = "PROVIDER_RESPONSE"
	// ErrProviderInterrupted: the response stopped before it was complete.
	// Retryable: a model call has no external effects.
	ErrProviderInterrupted errors.ErrorCode = "PROVIDER_INTERRUPTED"
)

// retryAfterKey is the error context key holding a provider's Retry-After.
const retryAfterKey = "retry_after"

// WithRetryAfter records how long the provider asked to wait before retrying.
func WithRetryAfter(err *errors.Error, wait time.Duration) *errors.Error {
	if wait > 0 {
		err = err.WithContext(retryAfterKey, wait)
	}
	return err
}

// RetryAfter returns the wait a provider asked for, if err carries one.
func RetryAfter(err error) (time.Duration, bool) {
	var structured *errors.Error
	if !stderrors.As(err, &structured) || structured.Context == nil {
		return 0, false
	}
	wait, ok := structured.Context[retryAfterKey].(time.Duration)
	return wait, ok && wait > 0
}
