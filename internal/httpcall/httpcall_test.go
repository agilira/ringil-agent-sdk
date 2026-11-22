// httpcall_test.go: failure classification and the stall guard
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package httpcall

import (
	stderrors "errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, stderrors.New("connection reset") }

// brokenBody serves content, then fails on Close like a broken connection.
type brokenBody struct {
	io.Reader
	closeErr error
}

func (b brokenBody) Close() error { return b.closeErr }

func TestRetryAfterIgnoresGarbage(t *testing.T) {
	for _, value := range []string{"", "soon", "-5", "Mon, 01 Jan 2001 00:00:00 GMT"} {
		resp := &http.Response{Header: http.Header{"Retry-After": {value}}}
		if got := RetryAfter(resp); got != 0 {
			t.Errorf("RetryAfter(%q) = %v", value, got)
		}
	}
}

func TestStallDisabled(t *testing.T) {
	s := NewStallReader(strings.NewReader("x"), -1, nil)
	defer s.Stop()
	if s.timer != nil {
		t.Fatal("negative stall timeout still armed a timer")
	}
	buf := make([]byte, 4)
	if n, err := s.Read(buf); err != nil || n != 1 {
		t.Fatal("read through the stall reader failed")
	}
}

func TestStatusErrorKeepsCodeWhenBodyFails(t *testing.T) {
	for status, code := range map[int]errors.ErrorCode{401: agent.ErrProviderAuth, 503: agent.ErrProviderUnavailable, 418: agent.ErrProviderInput} {
		resp := &http.Response{StatusCode: status, Header: http.Header{},
			Body: brokenBody{io.MultiReader(strings.NewReader("{"), errReader{}), nil}}
		if err := StatusError(resp); !errors.HasCode(err, code) {
			t.Errorf("status %d with broken body: %v", status, err)
		}
	}
	resp := &http.Response{StatusCode: 401, Header: http.Header{}, Body: brokenBody{strings.NewReader("{}"), stderrors.New("reset")}}
	if err := WithSecondary(StatusError(resp), CloseError(resp)); !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Fatalf("close failure hid the status code: %v", err)
	}
}

func TestWithSecondary(t *testing.T) {
	primary := errors.New(agent.ErrProviderAuth, "x")
	second := stderrors.New("second")
	if WithSecondary(primary, nil) != error(primary) || WithSecondary(nil, second) != second {
		t.Fatal("nil cases")
	}
	got := WithSecondary(primary, second)
	if !errors.HasCode(got, agent.ErrProviderAuth) || primary.Context["secondary"] != "second" {
		t.Fatalf("coded primary: %v %v", got, primary.Context)
	}
	plain := stderrors.New("plain")
	joined := WithSecondary(plain, second)
	if !stderrors.Is(joined, plain) || !stderrors.Is(joined, second) {
		t.Fatalf("plain primary: %v", joined)
	}
}
