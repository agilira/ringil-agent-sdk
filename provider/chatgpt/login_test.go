// login_test.go: browser and device sign-ins, the session's refresh
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"context"

	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/secrets"

	"github.com/agilira/go-errors"
)

// browserLogin runs Login.Browser and answers its callback with query
// (state filled in when it holds STATE).
func browserLogin(t *testing.T, is *issuer, query string) (Tokens, error) {
	t.Helper()
	port := freePort(t)
	shown := make(chan string, 1)
	l := Login{Issuer: is.URL, Port: port, Show: func(p Prompt) error { shown <- p.URL; return nil }}
	type out struct {
		t   Tokens
		err error
	}
	done := make(chan out, 1)
	go func() { tok, err := l.Browser(bg); done <- out{tok, err} }()
	page, _ := url.Parse(<-shown)
	q := page.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "http://localhost:"+strconv.Itoa(port)+"/auth/callback" || q.Get("client_id") != clientID {
		t.Fatalf("authorize page: %s", page)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/auth/callback?" + strings.ReplaceAll(query, "STATE", url.QueryEscape(q.Get("state"))))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	r := <-done
	return r.t, r.err
}

func TestBrowserSignIn(t *testing.T) {
	is := newIssuer(t)
	tok, err := browserLogin(t, is, "code=good&state=STATE")
	if err != nil || tok.Account != "acct-1" || tok.Refresh != "refresh-0" || tok.Expires.IsZero() {
		t.Fatalf("%v %v", tok, err)
	}
	if f := is.form(); f["grant_type"] != "authorization_code" || f["code"] != "good" || f["code_verifier"] == "" || f["client_id"] != clientID {
		t.Fatalf("exchange %v", f)
	}
}

func TestBrowserSignInRefused(t *testing.T) {
	is := newIssuer(t)
	for query, want := range map[string]string{
		"error=access_denied&state=STATE": "access_denied",
		"state=STATE":                     "no code",
		"code=bad&state=STATE":            "did not give the tokens",
	} {
		if _, err := browserLogin(t, is, query); !errors.HasCode(err, ErrSignIn) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", query, err)
		}
	}
}

// A callback with another state is not this sign-in: the wait goes on.
func TestCallbackOfAnotherSignInIsIgnored(t *testing.T) {
	cb, err := listen(freePort(t), "the-state")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.close()
	rec := &recorder{header: http.Header{}}
	cb.handle(rec, &http.Request{URL: &url.URL{RawQuery: "code=c&state=other"}})
	if rec.status != http.StatusBadRequest {
		t.Fatalf("status %d", rec.status)
	}
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if _, err := cb.wait(ctx); err == nil {
		t.Fatal("a foreign callback ended the wait")
	}
}

type recorder struct {
	header http.Header
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(s int)           { r.status = s }

func TestBusyPortIsTold(t *testing.T) {
	port := freePort(t)
	cb, err := listen(port, "s")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.close()
	if _, err := listen(port, "s"); !errors.HasCode(err, ErrSignIn) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("second listener: %v", err)
	}
}

func TestDeviceSignIn(t *testing.T) {
	is := newIssuer(t)
	is.pending.Store(2)
	var shown Prompt
	tok, err := Login{Issuer: is.URL, Show: func(p Prompt) error { shown = p; return nil }}.Device(bg)
	if err != nil || tok.Account != "acct-1" {
		t.Fatalf("%v %v", tok, err)
	}
	if shown.Code != "ABCD-1234" || shown.URL != is.URL+"/codex/device" {
		t.Fatalf("shown %+v", shown)
	}
	if f := is.form(); f["redirect_uri"] != is.URL+"/deviceauth/callback" || f["code"] != "dev-code" || f["code_verifier"] != "v" {
		t.Fatalf("exchange %v", f)
	}
}

func TestDeviceSignInCancelled(t *testing.T) {
	is := newIssuer(t)
	is.pending.Store(1000)
	ctx, cancel := context.WithTimeout(bg, 1500*time.Millisecond)
	defer cancel()
	if _, err := (Login{Issuer: is.URL, Show: func(Prompt) error { return nil }}).Device(ctx); err == nil {
		t.Fatal("a sign-in never completed succeeded")
	}
}

func TestSessionRefreshesNearTheEnd(t *testing.T) {
	is := newIssuer(t)
	s, store := signedIn(t, is)
	if _, err := s.Token(bg); err != nil || is.refreshes.Load() != 0 {
		t.Fatalf("a fresh token was refreshed: %v %d", err, is.refreshes.Load())
	}
	store.t.Expires = time.Now().Add(time.Minute)
	s.cur = Tokens{}
	tok, err := s.Token(bg)
	if err != nil || is.refreshes.Load() != 1 || tok.Refresh != "refresh-1" || store.t.Refresh != "refresh-1" {
		t.Fatalf("%v %v refreshes %d kept %q", tok, err, is.refreshes.Load(), store.t.Refresh)
	}
	if f := is.form(); f["grant_type"] != "refresh_token" || f["refresh_token"] != "refresh-0" {
		t.Fatalf("refresh %v", f)
	}
}

// Another process refreshed first: its tokens are taken, not refreshed
// again (the old refresh token would be refused as reused).
func TestSessionTakesAnotherProcesssRefresh(t *testing.T) {
	is := newIssuer(t)
	s, store := signedIn(t, is)
	stale, _ := s.Token(bg)
	store.t.Access = access("acct-1", time.Now().Add(2*time.Hour))
	tok, err := s.Renew(bg, stale)
	if err != nil || tok.Access != store.t.Access || is.refreshes.Load() != 0 {
		t.Fatalf("%v refreshes %d", err, is.refreshes.Load())
	}
}

func TestSessionRefreshRefused(t *testing.T) {
	for code, want := range map[string]string{
		"refresh_token_reused":      "refreshed elsewhere",
		"refresh_token_expired":     "expired",
		"refresh_token_invalidated": "revoked",
		"something_else":            "refused the refresh",
	} {
		is := newIssuer(t)
		is.refuse.Store(code)
		s, _ := signedIn(t, is)
		stale, _ := s.Token(bg)
		_, err := s.Renew(bg, stale)
		if !errors.HasCode(err, ErrSignedOut) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", code, err)
		}
	}
}

func TestSessionWithoutSignIn(t *testing.T) {
	s := &Session{Store: &memStore{}}
	if _, err := s.Token(bg); !errors.HasCode(err, ErrSignedOut) {
		t.Fatal(err)
	}
}

func TestLogoutRevokesAndForgets(t *testing.T) {
	is := newIssuer(t)
	s, store := signedIn(t, is)
	unrevoked, err := s.Logout(bg)
	if err != nil || unrevoked != nil || is.revoked.Load() != 1 || store.ok {
		t.Fatalf("unrevoked %v %v calls %d kept %v", unrevoked, err, is.revoked.Load(), store.ok)
	}
	if _, err := s.Logout(bg); !errors.HasCode(err, ErrSignedOut) {
		t.Fatalf("signed out already: %v", err)
	}
}

// Offline: forgotten all the same, and the cause told.
func TestLogoutOffline(t *testing.T) {
	is := newIssuer(t)
	s, store := signedIn(t, is)
	s.Issuer = "http://127.0.0.1:1"
	if unrevoked, err := s.Logout(bg); err != nil || unrevoked == nil || !strings.Contains(unrevoked.Error(), "could not be reached") || store.ok {
		t.Fatalf("offline: %v %v kept %v", unrevoked, err, store.ok)
	}
}

// Audit 22/11: a damaged sign-in could not be signed out of.
func TestLogoutForgetsADamagedSignIn(t *testing.T) {
	m := secrets.Map{"LOGIN": "{not json"}
	s := &Session{Store: SecretStore(mapStore{m}, "LOGIN")}
	if unrevoked, err := s.Logout(bg); err != nil || unrevoked == nil {
		t.Fatalf("%v %v", unrevoked, err)
	}
	if _, ok := m["LOGIN"]; ok {
		t.Fatal("the damaged sign-in stayed")
	}
}

// Processes sharing the store refresh one at a time: the second finds the
// first's tokens under the lock and does not refresh.
func TestFileLockSerialisesRefreshes(t *testing.T) {
	is := newIssuer(t)
	dir := t.TempDir()
	store := &memStore{t: Tokens{Access: access("acct-1", time.Now().Add(time.Minute)), Refresh: "refresh-0", Account: "acct-1", Expires: time.Now().Add(time.Minute)}, ok: true}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &Session{Store: store, Issuer: is.URL, Lock: FileLock(dir, "chatgpt.lock")}
			if _, err := s.Token(bg); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := is.refreshes.Load(); n != 1 {
		t.Fatalf("%d refreshes", n)
	}
}
