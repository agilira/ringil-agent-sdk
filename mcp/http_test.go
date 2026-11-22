// http_test.go: remote servers over Streamable HTTP
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	stdErrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
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

// 22/11: a refused key is told apart from a server that fails, so a host
// can ask for the key again instead of reporting the server down.
func TestRefusedKeyIsToldApart(t *testing.T) {
	srv := keyServer()
	defer srv.Close()
	connect := func(key string) error {
		c, err := Connect(bg, Server{
			Name: "remote", URL: srv.URL + "/mcp", HTTPClient: srv.Client(),
			Headers: func(context.Context) (http.Header, error) { return http.Header{"X-Api-Key": {key}}, nil },
		})
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	for key, want := range map[string]string{"wrong": "401", "forbidden": "403", "broken": "", "good": "ok"} {
		if err := connect(key); !answered(err, want) {
			t.Fatalf("%s: %v", key, err)
		}
	}
}

// answered: want is "ok", "" for a failure that is not a refusal, or the
// status of a refusal.
func answered(err error, want string) bool {
	switch want {
	case "ok":
		return err == nil
	case "":
		return err != nil && !errors.HasCode(err, ErrRefused)
	}
	return errors.HasCode(err, ErrRefused) && strings.Contains(err.Error(), want)
}

// keyServer serves the key "good", fails for "broken", forbids
// "forbidden" and refuses any other.
func keyServer() *httptest.Server {
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() }, nil)
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Api-Key") {
		case "good":
			handler.ServeHTTP(w, r)
		case "broken":
			http.Error(w, "down", http.StatusInternalServerError)
		case "forbidden":
			http.Error(w, "no", http.StatusForbidden)
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
}

// With OAuth a 401 starts the sign-in: a sign-in that fails is not told
// as a refused key (audit 22/11).
func TestOAuthFailureIsNotARefusedKey(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sign in", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := Connect(bg, Server{Name: "remote", URL: srv.URL + "/mcp", HTTPClient: srv.Client(), OAuth: failingLogin{}})
	if err == nil || errors.HasCode(err, ErrRefused) {
		t.Fatalf("err %v", err)
	}
}

type failingLogin struct{}

func (failingLogin) TokenSource(context.Context) (oauth2.TokenSource, error) { return nil, nil }

func (failingLogin) Authorize(context.Context, *http.Request, *http.Response) error {
	return stdErrors.New("the sign-in was not completed")
}
