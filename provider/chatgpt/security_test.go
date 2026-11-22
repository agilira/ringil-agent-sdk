// security_test.go: tokens never leak, hostile answers are bounded
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Tokens printed by mistake show the account, never a token.
func TestSecurityTokensDoNotPrint(t *testing.T) {
	tok := Tokens{Access: "ACCESS-SECRET", Refresh: "REFRESH-SECRET", Account: "acct"}
	s := &Session{Store: &memStore{}}
	s.cur = tok
	for _, out := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok), fmt.Sprintf("%v %+v %#v", s, s, s)} {
		if strings.Contains(out, "SECRET") {
			t.Fatalf("printed: %s", out)
		}
	}
}

// What a server sends back (it may echo the prompt or a token) never
// reaches an error: not the backend's, not the sign-in's.
func TestSecurityErrorsCarryNoUpstreamText(t *testing.T) {
	const marker = "PROMPT-ECHO-secret"
	for _, status := range []int{400, 401, 403, 429, 500} {
		p, _, _ := backend(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"` + marker + `","code":"usage_limit_reached"}}`))
		})
		if _, err := p.Generate(bg, &agent.Request{}, nil); err == nil || strings.Contains(err.Error(), marker) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
	p, _, _ := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.failed","response":{"error":{"code":"`+marker+`","message":"`+marker+`"}}}`)
	})
	if _, err := p.Generate(bg, &agent.Request{}, nil); err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("failed event: %v", err)
	}
	is := newIssuer(t)
	is.refuse.Store("x")
	s, _ := signedIn(t, is)
	stale, _ := s.Token(bg)
	if _, err := s.Renew(bg, stale); err == nil || strings.Contains(err.Error(), "PROMPT-ECHO") || strings.Contains(err.Error(), "refresh-0") {
		t.Fatalf("refresh: %v", err)
	}
}

// The sign-in's error description is the server's text: only a code
// made of plain letters comes through.
func TestSecurityCallbackErrorIsNotReflected(t *testing.T) {
	is := newIssuer(t)
	_, err := browserLogin(t, is, "error="+url.QueryEscape("<script>x</script>")+"&error_description=evil&state=STATE")
	if err == nil || strings.Contains(err.Error(), "script") || strings.Contains(err.Error(), "evil") {
		t.Fatal(err)
	}
}

// A redirect never carries the token elsewhere.
func TestSecurityRedirectsAreNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("ChatGPT-Account-ID") != "" {
			leaked.Store(true)
		}
	}))
	defer evil.Close()
	p, _, _ := backend(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusTemporaryRedirect)
	})
	if _, err := p.Generate(bg, &agent.Request{}, nil); err == nil || leaked.Load() {
		t.Fatalf("err %v leaked %v", err, leaked.Load())
	}
}

// Tokens a request could not carry (a header split) are refused, from a
// sign-in, a refresh or the store.
func TestSecurityTokensAreChecked(t *testing.T) {
	for _, tok := range []Tokens{
		{Access: "a\r\nX-Evil: 1", Account: "x"},
		{Access: "a", Account: "x\ny"},
		{Access: "", Account: "x"},
	} {
		s := &Session{Store: &memStore{t: tok, ok: true}}
		if _, err := s.Token(bg); err == nil {
			t.Errorf("%q/%q accepted", tok.Access, tok.Account)
		}
	}
}

// Claims are read from at most a bounded payload, and garbage gives none.
func TestSecurityClaimsAreBounded(t *testing.T) {
	huge := "e30." + strings.Repeat("A", 128<<10) + ".s"
	for _, tok := range []string{huge, "", "a.b", "a.!!!.c", "e30.bnVsbA.s"} {
		if accountOf(tok) != "" || !expiryOf(tok).IsZero() {
			t.Errorf("claims from %.20q", tok)
		}
	}
}

// A stream cannot grow without bound, in one event or in many.
func TestSecurityStreamIsBounded(t *testing.T) {
	_, err := decodeStream(strings.NewReader("data: {\"type\":\"x\",\"delta\":\""+strings.Repeat("a", maxEvent+10)+"\"}\n\n"), nil, "s")
	if err == nil {
		t.Fatal("an event above the bound was read")
	}
	var b strings.Builder
	for range maxItems + 2 {
		b.WriteString(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}}` + "\n\n")
	}
	if _, err := decodeStream(strings.NewReader(b.String()), nil, "s"); err == nil {
		t.Fatal("unbounded calls were read")
	}
}

// The session refreshes at most once for many concurrent callers.
func TestSecurityConcurrentTokensRefreshOnce(t *testing.T) {
	is := newIssuer(t)
	s, store := signedIn(t, is)
	store.t.Expires = time.Now().Add(time.Second)
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := s.Token(bg); err != nil {
				t.Error(err)
			}
		}()
	}
	for range 8 {
		<-done
	}
	if n := is.refreshes.Load(); n != 1 {
		t.Fatalf("%d refreshes", n)
	}
}

func FuzzDecodeStream(f *testing.F) {
	f.Add(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" + `data: ` + completed + "\n\n")
	f.Add(`data: {"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"x","summary":[{"text":"t"}]}}` + "\n\ndata: {\"type\":\"response.failed\"}\n\n")
	f.Add("data: {\"type\":\"response.output_item.done\",\"item\":null}\n\n")
	f.Fuzz(func(t *testing.T, stream string) {
		resp, err := decodeStream(strings.NewReader(stream), func(string) error { return nil }, "s")
		if err == nil && resp == nil {
			t.Fatal("no response and no error")
		}
	})
}

func TestSecurityIssuerMustBeHTTPS(t *testing.T) {
	l := Login{Issuer: "http://auth.example.com", Show: func(Prompt) error { return nil }}
	if _, err := l.Browser(bg); !errors.HasCode(err, ErrSignIn) {
		t.Fatalf("Browser: %v", err)
	}
	if _, err := l.Device(bg); !errors.HasCode(err, ErrSignIn) {
		t.Fatalf("Device: %v", err)
	}
	s := &Session{Store: &memStore{t: Tokens{Access: "a", Refresh: "r", Account: "acct"}, ok: true}, Issuer: "http://auth.example.com"}
	if _, err := s.Renew(bg, Tokens{Access: "a"}); err == nil {
		t.Fatal("Session.Renew accepted plain http issuer")
	}
}
