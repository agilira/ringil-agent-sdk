// oauth_test.go: signing in to a remote MCP server, end to end
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeProvider is an authorization server with dynamic registration and
// an MCP server that wants its token.
type fakeProvider struct {
	as, rs  *httptest.Server
	logins  atomic.Int32
	granted atomic.Int32
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{}
	f.as = httptest.NewServer(http.HandlerFunc(f.authServer))
	t.Cleanup(f.as.Close)
	mcpHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() }, nil)
	f.rs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" || r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
			writeJSON(w, map[string]any{"resource": f.rs.URL + "/mcp", "authorization_servers": []string{f.as.URL}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.rs.URL+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "sign in", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.rs.Close)
	return f
}

func (f *fakeProvider) authServer(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		writeJSON(w, map[string]any{
			"issuer": f.as.URL, "authorization_endpoint": f.as.URL + "/authorize", "token_endpoint": f.as.URL + "/token",
			"registration_endpoint": f.as.URL + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	case "/register":
		var meta map[string]any
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		meta["client_id"] = "client-1"
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, meta)
	case "/authorize":
		f.logins.Add(1)
		q := r.URL.Query()
		back, _ := url.Parse(q.Get("redirect_uri"))
		v := back.Query()
		v.Set("code", "code-1")
		v.Set("state", q.Get("state"))
		back.RawQuery = v.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	case "/token":
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != "code-1" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.granted.Add(1)
		writeJSON(w, map[string]any{"access_token": "tok-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "ref-1"})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// browser plays the user: it opens the link and follows the redirects
// back to the agent's callback.
func browser(t *testing.T) func(string) {
	return func(link string) {
		go func() {
			resp, err := http.Get(link) // #nosec G107 -- test server URL
			if err != nil {
				t.Error(err)
				return
			}
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
}

func TestOAuthSignInAndStoredToken(t *testing.T) {
	f := newFakeProvider(t)
	store := FileTokens(filepath.Join(t.TempDir(), "oauth", "srv.json"))
	connectAndAdd := func() { signInAndAdd(t, f, store) }
	connectAndAdd()
	if f.logins.Load() != 1 || f.granted.Load() != 1 {
		t.Fatalf("first run: %d logins, %d tokens", f.logins.Load(), f.granted.Load())
	}
	connectAndAdd() // the stored token avoids a second sign-in
	if f.logins.Load() != 1 {
		t.Fatalf("signed in again: %d logins", f.logins.Load())
	}
}

func signInAndAdd(t *testing.T, f *fakeProvider, store TokenStore) {
	t.Helper()
	{
		h, stop, err := NewLogin(Login{Show: browser(t), Tokens: store})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := stop(); err != nil {
				t.Log(err)
			}
		}()
		c, err := Connect(bg, Server{Name: "crm", URL: f.rs.URL + "/mcp", OAuth: h})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := c.Close(); err != nil {
				t.Log(err)
			}
		}()
		out, err := byName(t, c.Tools(), "crm__add").Call(bg, json.RawMessage(`{"a":2,"b":2}`))
		if err != nil || !strings.Contains(out.Content, `"sum":4`) {
			t.Fatalf("add %q %v", out.Content, err)
		}
	}
}
