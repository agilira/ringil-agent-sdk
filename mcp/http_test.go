// http_test.go: remote servers over Streamable HTTP
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRemoteServerWithHeaders(t *testing.T) {
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() }, nil)
	var unauthorized atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k-123" {
			unauthorized.Add(1)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	calls := atomic.Int32{}
	c, err := Connect(bg, Server{
		Name: "remote", URL: srv.URL + "/mcp", HTTPClient: srv.Client(),
		Headers: func(context.Context) (http.Header, error) {
			calls.Add(1)
			return http.Header{"X-Api-Key": {"k-123"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Log(err)
		}
	}()
	out, err := byName(t, c.Tools(), "remote__add").Call(bg, json.RawMessage(`{"a":1,"b":1}`))
	if err != nil || !strings.Contains(out.Content, `"sum":2`) || unauthorized.Load() != 0 || calls.Load() < 2 {
		t.Fatalf("out %q err %v unauthorized %d header calls %d", out.Content, err, unauthorized.Load(), calls.Load())
	}
}

// Credentials must not follow a redirect to another origin.
func TestRedirectElsewhereNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	var redirected atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	trustBoth := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // #nosec G402 -- two local test servers
	_, err := Connect(bg, Server{
		Name: "r", URL: srv.URL + "/mcp", HTTPClient: trustBoth,
		Headers: func(context.Context) (http.Header, error) { return http.Header{"X-Api-Key": {"secret"}}, nil },
	})
	if err == nil || leaked.Load() || !redirected.Load() {
		t.Fatalf("err %v leaked %v redirected %v", err, leaked.Load(), redirected.Load())
	}
}
