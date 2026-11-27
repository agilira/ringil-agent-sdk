// appapicheck_test.go: what the app's reference found uneven, 27/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/serve"
)

// A body that is not JSON, or too large, is said as such; a skill not
// there is not found; a webhook made says its whole address.
func TestAppAPIAnswersPlainly(t *testing.T) {
	port := freePort(t)
	appAPI(t, "models: [openai:m]\nserve:\n  webhooks: 127.0.0.1:"+port+"\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		if code, _ := call("PUT", "/v1/settings/limits", `{"max_cost":1,"pad":"`+strings.Repeat("x", 70<<10)+`"}`); code != http.StatusRequestEntityTooLarge {
			t.Errorf("a body too large: %d", code)
		}
		must(t, call, "DELETE", "/v1/settings/skills/not-there", "", http.StatusNotFound)
		out := must(t, call, "PUT", "/v1/triggers/gh", `{"kind":"webhook","prompt":"look"}`, http.StatusCreated)
		if !strings.Contains(out, `"url":"http://127.0.0.1:`+port+`/hooks/gh"`) {
			t.Errorf("the webhook's address: %s", out)
		}
	})
}

// A body not sent as JSON is refused as such.
func TestBodyMustBeJSON(t *testing.T) {
	req := httptest.NewRequest("PUT", "/v1/settings/limits", strings.NewReader(`{"max_cost":1}`))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	var v map[string]any
	if decodeJSON(w, req, &v) || w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// A read error while reading the body is a bad request, not 413.
func TestBodyReadErrorIsBadRequest(t *testing.T) {
	req := httptest.NewRequest("PUT", "/v1/settings/limits", errReader{})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	var v map[string]any
	if decodeJSON(w, req, &v) || w.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// freePort is a port nothing listens on, just now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}
