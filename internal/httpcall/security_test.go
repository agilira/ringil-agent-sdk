// security_test.go: upstream bodies never come back, events are bounded
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

// An error body may echo the prompt or a credential: no status's error
// carries it.
func TestSecurityStatusErrorsCarryNoBody(t *testing.T) {
	const marker = "PROMPT-ECHO-sk-live-123"
	for _, status := range []int{400, 401, 403, 404, 413, 429, 500, 503} {
		resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"` + marker + ` context length"}`))}
		err := StatusError(resp)
		if err == nil || strings.Contains(err.Error(), marker) {
			t.Fatalf("status %d: %v", status, err)
		}
		var e *errors.Error
		if stderrors.As(err, &e) {
			for k, v := range e.Context {
				if strings.Contains(k+":"+toString(v), marker) {
					t.Fatalf("status %d: the body is in the context", status)
				}
			}
		}
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// An error body larger than the bound is not read whole.
func TestSecurityErrorBodyIsBounded(t *testing.T) {
	read := &countingReader{r: strings.NewReader(strings.Repeat("x", 10*maxErrorBody))}
	_ = StatusError(&http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(read)})
	if read.n > maxErrorBody {
		t.Fatalf("read %d bytes of an error body", read.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += n
	return n, err
}

// A hostile stream cannot grow an event without bound, in one line or in
// many.
func TestSecurityEventsAreBounded(t *testing.T) {
	const bound = 1 << 10
	for name, stream := range map[string]string{
		"one line":   "data: " + strings.Repeat("x", 4*bound) + "\n\n",
		"many lines": strings.Repeat("data: "+strings.Repeat("x", 100)+"\n", 64) + "\n",
	} {
		err := Events(strings.NewReader(stream), bound, func([]byte) (bool, error) { return false, nil })
		if !errors.HasCode(err, agent.ErrProviderResponse) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func FuzzEvents(f *testing.F) {
	f.Add("data: {\"a\":1}\n\ndata: [DONE]\n\n")
	f.Add(": comment\r\nevent: x\r\ndata: a\r\ndata: b\r\n\r\n")
	f.Add("data:")
	f.Fuzz(func(t *testing.T, stream string) {
		total := 0
		_ = Events(strings.NewReader(stream), 4<<10, func(data []byte) (bool, error) {
			if len(data) > 4<<10 {
				t.Fatalf("event of %d bytes", len(data))
			}
			total += len(data)
			return false, nil
		})
		if total > len(stream) {
			t.Fatalf("more data than the stream held")
		}
	})
}
