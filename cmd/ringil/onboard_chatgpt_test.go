// onboard_chatgpt_test.go: the setup with a ChatGPT subscription
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOpenAI is the sign-in by code and the ChatGPT backend in one.
func fakeOpenAI(t *testing.T) *httptest.Server {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-7"}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"device_auth_id":"d","user_code":"WXYZ-0000","interval":"1"}`)
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"authorization_code":"c","code_challenge":"x","code_verifier":"v"}`)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token, "refresh_token": "r", "id_token": token})
	})
	mux.HandleFunc("/oauth/revoke", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-ID") != "acct-7" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-a","visibility":"list","priority":1},{"slug":"gpt-b","visibility":"list","priority":2}]}`)
	})
	mux.HandleFunc("/responses", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ready\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// 22/11: a ChatGPT subscription in place of a key: signed in by code (no
// browser here), the subscription's models offered, the sign-in kept in
// the store, and ringil.yaml without a key or an endpoint.
func TestSetupWithAChatGPTSubscription(t *testing.T) {
	srv := fakeOpenAI(t)
	h, _ := setupHarness(t, []string{"2", "frase lunga davvero", "frase lunga davvero", "2", "n", "1", "", "1", "1", "n", "n"}, map[string]string{})
	h.c.chatgptIssuer, h.c.chatgptBase = srv.URL, srv.URL
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	said := h.out.String() + h.err.String()
	for _, want := range []string{"WXYZ-0000", "gpt-b", "signed in to ChatGPT", "the model answered: ready"} {
		if !strings.Contains(said, want) {
			t.Errorf("not said %q:\n%s", want, said)
		}
	}
	yamlHas(t, h.dir, []string{"- chatgpt:gpt-a", "store: file"}, []string{"base:", "key_env", "access_token", "acct-7"})
	if v := storedSecret(t, h.dir, chatgptLogin); !strings.Contains(v, `"account_id":"acct-7"`) {
		t.Fatalf("the sign-in in the store: %.40q", v)
	}
	runThenSignOut(t, h)
}

// runThenSignOut: the CLI talks to the subscription's model, signs out,
// and then asks to sign in.
func runThenSignOut(t *testing.T, h *harness) {
	t.Helper()
	h.out.Reset()
	if code := h.run("--no-tools", "run", "hi"); code != exitOK || !strings.Contains(h.out.String(), "ready") {
		t.Fatalf("run: exit %d: %s %s", code, h.out, h.err)
	}
	if code := h.run("logout", "chatgpt"); code != exitOK || !strings.Contains(h.out.String()+h.err.String(), "signed out of ChatGPT") {
		t.Fatalf("logout: exit %d: %s %s", code, h.out, h.err)
	}
	if code := h.run("--no-tools", "run", "hi"); code == exitOK || !strings.Contains(h.err.String(), "ringil login chatgpt") {
		t.Fatalf("after logout: exit %d: %s", code, h.err)
	}
}
