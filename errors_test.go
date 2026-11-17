// errors_test.go: tests for error helpers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

func TestRetryAfterRoundTrip(t *testing.T) {
	err := WithRetryAfter(errors.New(ErrProviderRate, "slow down"), 3*time.Second)
	wait, ok := RetryAfter(err)
	if !ok || wait != 3*time.Second {
		t.Fatalf("RetryAfter = %v %v", wait, ok)
	}
}

func TestRetryAfterThroughWrapping(t *testing.T) {
	err := fmt.Errorf("call: %w", WithRetryAfter(errors.New(ErrProviderRate, "x"), time.Second))
	if wait, ok := RetryAfter(err); !ok || wait != time.Second {
		t.Fatalf("RetryAfter through fmt wrapping = %v %v", wait, ok)
	}
}

func TestRetryAfterAbsent(t *testing.T) {
	cases := []error{
		nil,
		stderrors.New("plain"),
		errors.New(ErrProviderRate, "no wait"),
		WithRetryAfter(errors.New(ErrProviderRate, "zero"), 0),
		WithRetryAfter(errors.New(ErrProviderRate, "negative"), -time.Second),
		errors.New(ErrProviderRate, "wrong type").WithContext(retryAfterKey, "3s"),
		&errors.Error{Code: ErrProviderRate},
	}
	for i, err := range cases {
		if wait, ok := RetryAfter(err); ok || wait != 0 {
			t.Errorf("case %d: RetryAfter = %v %v, want none", i, wait, ok)
		}
	}
}
