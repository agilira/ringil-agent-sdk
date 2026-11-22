// helpers_test.go: a fake OpenAI sign-in and ChatGPT backend
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

var bg = context.Background()

// jwt is an unsigned token with claims: the package reads only claims.
func jwt(claims map[string]any) string {
	data, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(data) + ".sig"
}

// access is an access token for account, expiring at exp.
func access(account string, exp time.Time) string {
	return jwt(map[string]any{"exp": exp.Unix(), authClaim: map[string]any{"chatgpt_account_id": account}})
}

// memStore is a Store in memory, counting its saves.
type memStore struct {
	mu    sync.Mutex
	t     Tokens
	ok    bool
	saves int
}

func (m *memStore) Load(context.Context) (Tokens, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t, m.ok, nil
}

func (m *memStore) Save(_ context.Context, t Tokens) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t, m.ok = t, true
	m.saves++
	return nil
}

func (m *memStore) Delete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t, m.ok = Tokens{}, false
	return nil
}

// issuer is a fake auth.openai.com: it issues tokens numbered by refresh.
type issuer struct {
	*httptest.Server
	refreshes   atomic.Int32
	revoked     atomic.Int32
	refuse      atomic.Value // a refresh error code to answer, or ""
	pending     atomic.Int32 // device polls answered 403 before success
	lastForm    atomic.Value
	lastRevoked atomic.Value
}

func newIssuer(t *testing.T) *issuer {
	is := &issuer{}
	is.refuse.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", is.token)
	mux.HandleFunc("/oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		is.lastRevoked.Store(body["token"])
		is.revoked.Add(1)
	})
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"1"}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, _ *http.Request) {
		if is.pending.Add(-1) >= 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"authorization_code":"dev-code","code_challenge":"c","code_verifier":"v"}`))
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

func (is *issuer) token(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") == "application/json" {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		is.lastForm.Store(body)
		if code := is.refuse.Load().(string); code != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"PROMPT-ECHO secret"}}`))
			return
		}
		n := is.refreshes.Add(1)
		writeTokens(w, "acct-1", "refresh-"+strconv.Itoa(int(n)), time.Hour)
		return
	}
	_ = r.ParseForm()
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	is.lastForm.Store(form)
	if form["code"] == "bad" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeTokens(w, "acct-1", "refresh-0", time.Hour)
}

func writeTokens(w http.ResponseWriter, account, refresh string, life time.Duration) {
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token": access(account, time.Now().Add(life)), "refresh_token": refresh,
		"id_token": jwt(map[string]any{authClaim: map[string]any{"chatgpt_account_id": account}}),
	})
}

func (is *issuer) form() map[string]string {
	f, _ := is.lastForm.Load().(map[string]string)
	return f
}

// freePort is a loopback port free a moment ago.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// signedIn is a session holding fresh tokens, against is.
func signedIn(t *testing.T, is *issuer) (*Session, *memStore) {
	t.Helper()
	store := &memStore{t: Tokens{Access: access("acct-1", time.Now().Add(time.Hour)), Refresh: "refresh-0", Account: "acct-1", Expires: time.Now().Add(time.Hour)}, ok: true}
	return &Session{Store: store, Issuer: is.URL}, store
}

// sse writes events as the backend does.
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_, _ = w.Write([]byte("event: x\ndata: " + e + "\n\n"))
	}
}

const completed = `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}}}`

// mapStore is a secrets.Store over a map.
type mapStore struct{ secrets.Map }

func (m mapStore) Set(_ context.Context, name, value string) error {
	m.Map[name] = value
	return nil
}

func (m mapStore) Delete(_ context.Context, name string) error {
	delete(m.Map, name)
	return nil
}
