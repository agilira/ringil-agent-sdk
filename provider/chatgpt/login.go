// login.go: signing in with a ChatGPT subscription
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chatgpt

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
)

// The OpenAI sign-in Codex uses, which OpenAI lets other agents use for
// a ChatGPT subscription: its public client, and the callback it was
// registered with.
const (
	DefaultIssuer = "https://auth.openai.com"
	clientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	callbackPort  = 1455
	callbackPath  = "/auth/callback"
	scopes        = "openid profile email offline_access"
	originator    = "ringil"
)

// Wait bounds: the browser sign-in and the device code (OpenAI's code
// lasts 15 minutes).
const (
	browserWait = 10 * time.Minute
	deviceWait  = 15 * time.Minute
)

// Prompt is what the user is shown to sign in: a page to open and, for
// the sign-in by code, the code to type there.
type Prompt struct {
	URL  string
	Code string
}

// Login signs a user in with their ChatGPT subscription.
type Login struct {
	// Show tells the user where to sign in (required).
	Show func(Prompt) error
	// Open opens the page in a browser; nil leaves it to the user.
	Open func(url string) error
	// Client makes the requests to the issuer; nil is a client with a
	// timeout. Redirects are never followed.
	Client *http.Client
	// Issuer is OpenAI's sign-in; tests point it elsewhere.
	Issuer string
	// Port is the browser callback's (OpenAI registered 1455; tests
	// change it).
	Port int
}

func (l Login) issuer() string {
	if l.Issuer != "" {
		return strings.TrimRight(l.Issuer, "/")
	}
	return DefaultIssuer
}

func (l Login) client() *http.Client {
	c := &http.Client{Timeout: 45 * time.Second}
	if l.Client != nil {
		copied := *l.Client
		c = &copied
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// Browser signs in through the browser: a page on OpenAI, and a callback
// on this machine's loopback, so it needs a browser here. On a server use
// Device.
func (l Login) Browser(ctx context.Context) (Tokens, error) {
	if l.Show == nil {
		return Tokens{}, errors.New(ErrSignIn, "Login.Show is required")
	}
	p, err := newPKCE()
	if err != nil {
		return Tokens{}, err
	}
	port := l.Port
	if port == 0 {
		port = callbackPort
	}
	redirect := "http://localhost:" + strconv.Itoa(port) + callbackPath
	cb, err := listen(port, p.state)
	if err != nil {
		return Tokens{}, err
	}
	defer cb.close()
	page := l.issuer() + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "scope": {scopes},
		"code_challenge": {p.challenge}, "code_challenge_method": {"S256"}, "state": {p.state},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {originator},
	}.Encode()
	if err := l.Show(Prompt{URL: page}); err != nil {
		return Tokens{}, err
	}
	if l.Open != nil {
		_ = l.Open(page) // #nosec G104 -- the page was shown: the user can open it
	}
	ctx, cancel := context.WithTimeout(ctx, browserWait)
	defer cancel()
	code, err := cb.wait(ctx)
	if err != nil {
		return Tokens{}, err
	}
	return l.exchange(ctx, code, p.verifier, redirect)
}

// pkce is one sign-in's proof (RFC 7636, S256) and state.
type pkce struct{ verifier, challenge, state string }

func newPKCE() (pkce, error) {
	verifier, err := random(64)
	if err != nil {
		return pkce{}, err
	}
	state, err := random(32)
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return pkce{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:]), state: state}, nil
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.Wrap(err, ErrSignIn, "no randomness for the sign-in")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// callback receives the browser's redirect, once: the first answer that
// carries the right state ends the wait.
type callback struct {
	srv   *http.Server
	state string
	once  sync.Once
	done  chan result
}

type result struct {
	code string
	err  error
}

// listen takes the callback port on both loopbacks: the redirect names
// localhost, which a browser may resolve to either, and a port left free
// on one could be taken by someone else (PKCE makes a stolen code
// useless, but they could block the sign-in). IPv6 may be off: then the
// IPv4 one alone.
func listen(port int, state string) (*callback, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, errors.Wrap(err, ErrSignIn, "port "+strconv.Itoa(port)+" is busy (another sign-in, or Codex?): close it, or sign in by code")
	}
	cb := &callback{state: state, done: make(chan result, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, cb.handle)
	cb.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() { _ = cb.srv.Serve(ln) }() // #nosec G104 -- ends with close
	if ln6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		go func() { _ = cb.srv.Serve(ln6) }() // #nosec G104 -- ends with close
	}
	return cb, nil
}

func (cb *callback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if len(state) != len(cb.state) || subtle.ConstantTimeCompare([]byte(state), []byte(cb.state)) != 1 {
		page(w, http.StatusBadRequest, "This sign-in is not the one ringil started. Start it again from ringil.")
		return // not ours: the wait goes on
	}
	switch code := q.Get("code"); {
	case q.Get("error") != "":
		page(w, http.StatusOK, "The sign-in was refused. You can close this tab.")
		cb.finish(result{err: errors.New(ErrSignIn, "OpenAI refused the sign-in ("+safeCode(q.Get("error"))+")")})
	case code == "":
		page(w, http.StatusBadRequest, "The sign-in gave no code. Start it again from ringil.")
		cb.finish(result{err: errors.New(ErrSignIn, "the sign-in gave no code")})
	default:
		page(w, http.StatusOK, "ringil received the sign-in and is finishing it: you can close this tab and go back to ringil.")
		cb.finish(result{code: code})
	}
}

func (cb *callback) finish(r result) { cb.once.Do(func() { cb.done <- r }) }

func (cb *callback) wait(ctx context.Context) (string, error) {
	select {
	case r := <-cb.done:
		return r.code, r.err
	case <-ctx.Done():
		if stderrors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New(ErrSignIn, "the sign-in was not completed in time")
		}
		return "", ctx.Err()
	}
}

func (cb *callback) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = cb.srv.Shutdown(ctx) // #nosec G104 -- the sign-in is over
}

func page(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>ringil</title><p style="font:16px system-ui;margin:4em auto;max-width:30em;text-align:center">`+html.EscapeString(text)+`</p>`) // #nosec G104 G705 -- escaped
}

// codePattern keeps an OAuth error code, never a description, which may
// carry anything.
var codePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func safeCode(s string) string {
	if codePattern.MatchString(s) {
		return s
	}
	return "unknown"
}

// Device signs in by code, for a machine without a browser: the user
// opens the page shown on any device and types the code there.
func (l Login) Device(ctx context.Context) (Tokens, error) {
	if l.Show == nil {
		return Tokens{}, errors.New(ErrSignIn, "Login.Show is required")
	}
	var uc struct {
		ID       string          `json:"device_auth_id"`
		Code     string          `json:"user_code"`
		Usercode string          `json:"usercode"`
		Interval json.RawMessage `json:"interval"` // "5" today; 5 tomorrow
	}
	status, err := l.postJSON(ctx, "/api/accounts/deviceauth/usercode", map[string]string{"client_id": clientID}, &uc)
	switch {
	case err != nil:
		return Tokens{}, err
	case status == http.StatusNotFound:
		return Tokens{}, errors.New(ErrSignIn, "OpenAI does not offer the sign-in by code here: sign in through the browser")
	case status/100 != 2 || uc.ID == "" || uc.Code+uc.Usercode == "":
		return Tokens{}, errors.New(ErrSignIn, "OpenAI did not give a code ("+strconv.Itoa(status)+")")
	}
	if uc.Code == "" {
		uc.Code = uc.Usercode
	}
	if err := l.Show(Prompt{URL: l.issuer() + "/codex/device", Code: uc.Code}); err != nil {
		return Tokens{}, err
	}
	interval, _ := strconv.Atoi(strings.Trim(strings.TrimSpace(string(uc.Interval)), `"`))
	return l.pollDevice(ctx, uc.ID, uc.Code, time.Duration(min(max(interval, 1), 30))*time.Second)
}

// pollDevice waits for the user to type the code: OpenAI answers 403 or
// 404 until then.
func (l Login) pollDevice(ctx context.Context, id, code string, every time.Duration) (Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, deviceWait)
	defer cancel()
	for {
		var got struct {
			Code     string `json:"authorization_code"`
			Verifier string `json:"code_verifier"`
		}
		status, err := l.postJSON(ctx, "/api/accounts/deviceauth/token", map[string]string{"device_auth_id": id, "user_code": code}, &got)
		if err != nil {
			return Tokens{}, err
		}
		if status/100 == 2 && got.Code != "" && got.Verifier != "" {
			return l.exchange(ctx, got.Code, got.Verifier, l.issuer()+"/deviceauth/callback")
		}
		if every, err = nextPoll(status, every); err != nil {
			return Tokens{}, err
		}
		if err := pause(ctx, every); err != nil {
			return Tokens{}, err
		}
	}
}

// nextPoll is the wait before the next poll: the same while the user has
// not typed the code (403, 404), longer when asked to slow down (429).
func nextPoll(status int, every time.Duration) (time.Duration, error) {
	switch status {
	case http.StatusForbidden, http.StatusNotFound:
		return every, nil
	case http.StatusTooManyRequests:
		return min(2*every, 30*time.Second), nil
	}
	return 0, errors.New(ErrSignIn, "the sign-in by code failed ("+strconv.Itoa(status)+")")
}

// pause waits between polls; the end of ctx ends the sign-in.
func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		if stderrors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New(ErrSignIn, "the code was not typed in time")
		}
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// postJSON posts body and decodes a 2xx answer into out; the status is
// returned whatever it is. Bodies of other answers are not read.
func (l Login) postJSON(ctx context.Context, path string, body, out any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, errors.Wrap(err, ErrSignIn, "the request does not encode")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.issuer()+path, bytes.NewReader(data))
	if err != nil {
		return 0, errors.Wrap(err, ErrSignIn, "the request cannot be built")
	}
	req.Header.Set("Content-Type", "application/json")
	return l.do(ctx, req, out)
}

func (l Login) do(ctx context.Context, req *http.Request, out any) (int, error) {
	resp, err := l.client().Do(req) // #nosec G704 -- the issuer is OpenAI's, or the host's own
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.Wrap(err, ErrSignIn, "OpenAI's sign-in did not answer")
	}
	defer func() { _ = resp.Body.Close() }() // #nosec G104 -- read below
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return resp.StatusCode, errors.New(ErrSignIn, "OpenAI's sign-in answered in an unknown form")
	}
	return resp.StatusCode, nil
}

// exchange trades the sign-in's code for tokens.
func (l Login) exchange(ctx context.Context, code, verifier, redirect string) (Tokens, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {clientID}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.issuer()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, errors.Wrap(err, ErrSignIn, "the request cannot be built")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var got tokenAnswer
	status, err := l.do(ctx, req, &got)
	if err != nil {
		return Tokens{}, err
	}
	if status/100 != 2 {
		return Tokens{}, errors.New(ErrSignIn, "OpenAI did not give the tokens ("+strconv.Itoa(status)+")")
	}
	t := got.tokens(Tokens{})
	if err := t.check(); err != nil {
		return Tokens{}, err
	}
	return t, nil
}

// tokenAnswer is the token endpoint's answer, to a code or a refresh.
type tokenAnswer struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	ID      string `json:"id_token"`
}

// tokens are the answer's, keeping prev's where a refresh gives none.
// The id token serves only to find the account: it is not kept.
func (a tokenAnswer) tokens(prev Tokens) Tokens {
	t := Tokens{Access: a.Access, Refresh: a.Refresh}
	if t.Refresh == "" {
		t.Refresh = prev.Refresh
	}
	t.Account = accountOf(t.Access, a.ID)
	if t.Account == "" {
		t.Account = prev.Account
	}
	t.Expires = expiryOf(t.Access)
	return t
}
