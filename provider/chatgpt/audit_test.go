// audit_test.go: what the audit of 22/11 found, kept from coming back
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	stderrors "errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// The sign-in fits a keyring item: Windows takes 2560 bytes. Measured on
// 22/11: the tokens together were 4016, the access token alone ~1.75k.
func TestKeptSignInFitsAKeyring(t *testing.T) {
	big := access("acct-1", time.Now().Add(time.Hour)) + strings.Repeat("x", 1800)
	m := secrets.Map{}
	st := SecretStore(mapStore{m}, "LOGIN")
	tok := Tokens{Access: big, Refresh: strings.Repeat("r", 220), Account: "acct-1", Expires: expiryOf(big)}
	if err := st.Save(bg, tok); err != nil {
		t.Fatal(err)
	}
	for name, v := range m {
		if len(v) > 2560 {
			t.Errorf("%s: %d bytes", name, len(v))
		}
	}
	got, ok, err := st.Load(bg)
	if err != nil || !ok || got.Access != big || got.Refresh != tok.Refresh {
		t.Fatalf("%v %v", ok, err)
	}
}

// A save cut between the two secrets leaves an access token the record
// does not expect: it is not used, the session refreshes.
func TestAStaleAccessTokenIsNotUsed(t *testing.T) {
	is := newIssuer(t)
	m := secrets.Map{}
	st := SecretStore(mapStore{m}, "LOGIN")
	old := access("acct-1", time.Now().Add(time.Hour))
	if err := st.Save(bg, Tokens{Access: old, Refresh: "refresh-0", Account: "acct-1", Expires: expiryOf(old)}); err != nil {
		t.Fatal(err)
	}
	m["LOGIN_ACCESS"] = access("acct-1", time.Now().Add(3*time.Hour)) // the new one, the record not yet
	s := &Session{Store: st, Issuer: is.URL}
	if _, err := s.Token(bg); err != nil || is.refreshes.Load() != 1 {
		t.Fatalf("%v refreshes %d", err, is.refreshes.Load())
	}
}

// failingSaves fails its first n saves.
type failingSaves struct {
	*memStore
	n atomic.Int32
}

func (f *failingSaves) Save(ctx context.Context, t Tokens) error {
	if f.n.Add(-1) >= 0 {
		return stderrors.New("keyring locked")
	}
	return f.memStore.Save(ctx, t)
}

// Audit 22/11: tokens refreshed but not kept were dropped, and the store's
// refresh token was spent: the sign-in was lost. They are used, kept at
// the next call, and their refresh token is the one used next.
func TestRefreshedTokensSurviveAFailedSave(t *testing.T) {
	is := newIssuer(t)
	s, mem := signedIn(t, is)
	mem.t.Expires = time.Now().Add(time.Minute)
	store := &failingSaves{memStore: mem}
	store.n.Store(1)
	var warned atomic.Int32
	s.Store, s.Warn = store, func(error) { warned.Add(1) }
	tok, err := s.Token(bg)
	if err != nil || tok.Refresh != "refresh-1" || warned.Load() != 1 || mem.t.Refresh != "refresh-0" {
		t.Fatalf("%v %v warned %d kept %q", tok, err, warned.Load(), mem.t.Refresh)
	}
	if _, err := s.Token(bg); err != nil || mem.t.Refresh != "refresh-1" {
		t.Fatalf("not kept at the next call: %v %q", err, mem.t.Refresh)
	}
}

// The refresh after a failed save uses the session's refresh token, not
// the spent one in the store.
func TestRenewAfterAFailedSaveUsesTheValidToken(t *testing.T) {
	is := newIssuer(t)
	s, mem := signedIn(t, is)
	mem.t.Expires = time.Now().Add(time.Minute)
	store := &failingSaves{memStore: mem}
	store.n.Store(5)
	s.Store = store
	tok, _ := s.Token(bg)
	if _, err := s.Renew(bg, tok); err != nil {
		t.Fatal(err)
	}
	if f := is.form(); f["refresh_token"] != "refresh-1" {
		t.Fatalf("refreshed with %q", f["refresh_token"])
	}
}

func TestRenewAfterAFailedSaveWithFreshStoredToken(t *testing.T) {
	is := newIssuer(t)
	s, mem := signedIn(t, is)
	stale := mem.t
	store := &failingSaves{memStore: mem}
	store.n.Store(5)
	s.Store = store
	tok, err := s.Renew(bg, stale)
	if err != nil || tok.Refresh != "refresh-1" {
		t.Fatalf("first renew: %v %v", tok, err)
	}
	next, err := s.Renew(bg, tok)
	if err != nil || next.Refresh != "refresh-2" {
		t.Fatalf("second renew: %v %v", next, err)
	}
	if f := is.form(); f["refresh_token"] != "refresh-1" {
		t.Fatalf("refreshed with %q", f["refresh_token"])
	}
}

func TestLogoutAfterAFailedSaveRevokesActiveToken(t *testing.T) {
	is := newIssuer(t)
	s, mem := signedIn(t, is)
	mem.t.Expires = time.Now().Add(time.Minute)
	store := &failingSaves{memStore: mem}
	store.n.Store(5)
	s.Store = store
	if _, err := s.Token(bg); err != nil {
		t.Fatal(err)
	}
	unrevoked, err := s.Logout(bg)
	if unrevoked != nil || err != nil {
		t.Fatalf("logout: %v %v", unrevoked, err)
	}
	if got, _ := is.lastRevoked.Load().(string); got != "refresh-1" || mem.ok {
		t.Fatalf("revoked %q ok %v", got, mem.ok)
	}
}

// Audit 22/11: login and logout did not take the lock, so a refresh of
// another process could undo them.
func TestSaveAndLogoutTakeTheLock(t *testing.T) {
	is := newIssuer(t)
	s, _ := signedIn(t, is)
	var locks atomic.Int32
	s.Lock = func(context.Context) (func(), error) { locks.Add(1); return func() {}, nil }
	if err := s.Save(bg, Tokens{Access: access("acct-2", time.Now().Add(time.Hour)), Refresh: "r", Account: "acct-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Logout(bg); err != nil {
		t.Fatal(err)
	}
	if locks.Load() != 2 {
		t.Fatalf("%d locks", locks.Load())
	}
}

// The device code's interval may come as a number; 429 slows the polls
// down instead of failing the sign-in.
func TestDevicePollingSlowsDown(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"device_auth_id":"d","user_code":"C","interval":1}`)
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"authorization_code":"c","code_verifier":"v"}`)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) { writeTokens(w, "acct-1", "r", time.Hour) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	start := time.Now()
	tok, err := Login{Issuer: srv.URL, Show: func(Prompt) error { return nil }}.Device(bg)
	if err != nil || tok.Account != "acct-1" || polls.Load() != 2 || time.Since(start) < 2*time.Second {
		t.Fatalf("%v polls %d after %v", err, polls.Load(), time.Since(start))
	}
}

func TestModelsRenewARefusedToken(t *testing.T) {
	is := newIssuer(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"m","visibility":"list"}]}`)
	}))
	defer srv.Close()
	s, _ := signedIn(t, is)
	if models, err := Models(bg, s, srv.URL, nil); err != nil || len(models) != 1 || is.refreshes.Load() != 1 {
		t.Fatalf("%v %v refreshes %d", models, err, is.refreshes.Load())
	}
}

// The cache key is the conversation's: the same over its turns, another
// for another conversation, and no text of it.
func TestCacheKeyIsTheConversations(t *testing.T) {
	first := agent.UserText("plan the trip")
	turn1 := &agent.Request{System: "sys", Messages: []agent.Message{first}}
	turn2 := &agent.Request{System: "sys", Messages: []agent.Message{first, agent.AssistantText("ok"), agent.UserText("and then?")}}
	other := &agent.Request{System: "sys", Messages: []agent.Message{agent.UserText("something else")}}
	if cacheKey(turn1) != cacheKey(turn2) || cacheKey(turn1) == cacheKey(other) || strings.Contains(cacheKey(turn1), "trip") {
		t.Fatalf("%s %s %s", cacheKey(turn1), cacheKey(turn2), cacheKey(other))
	}
}
