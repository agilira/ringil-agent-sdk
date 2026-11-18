// oauth.go: signing in to a remote MCP server from a desktop agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/agilira/go-errors"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// ErrAuth marks a failed sign-in.
const ErrAuth errors.ErrorCode = "MCP_AUTH"

// Login configures the authorization code flow for a desktop or terminal
// agent: the user opens a link, signs in with the server's provider, and
// the browser comes back to a listener on 127.0.0.1.
type Login struct {
	// ClientID of a client registered with the provider; empty registers
	// one dynamically, as most MCP servers allow.
	ClientID     string
	ClientSecret string
	// Port of the callback listener; 0 picks a free one (fine with dynamic
	// registration; a registered client needs the port it was given).
	Port int
	// Show presents the sign-in link to the user. Required.
	Show func(url string)
	// Tokens, when set, keeps tokens across runs (FileTokens).
	Tokens TokenStore
	// Timeout for the user to finish signing in; default 5 minutes.
	Timeout time.Duration
	// HTTPClient for the provider's endpoints; default http.DefaultClient.
	HTTPClient *http.Client
}

// TokenStore keeps a server's tokens.
type TokenStore interface {
	Load() (*oauth2.Token, error)
	Save(*oauth2.Token) error
}

// NewLogin returns the handler for Server.OAuth and a function that stops
// its listener.
func NewLogin(l Login) (auth.OAuthHandler, func() error, error) {
	if l.Show == nil {
		return nil, nil, errors.New(ErrConfig, "a login needs a way to show the sign-in link")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(l.Port))
	if err != nil {
		return nil, nil, errors.Wrap(err, ErrConfig, "cannot open the sign-in callback port")
	}
	cb := &callback{ln: ln, addr: ln.Addr().String()}
	redirect := "http://" + ln.Addr().String() + "/callback"
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: cb.fetcher(l),
		RequestRefreshToken:      l.Tokens != nil,
		Client:                   l.HTTPClient,
	}
	if l.ClientID != "" {
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: l.ClientID}
		if l.ClientSecret != "" {
			cfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: l.ClientSecret}
		}
	} else {
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirect}, ClientName: "ringil", TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		}}
	}
	if l.Tokens != nil {
		withTokens(cfg, l.Tokens)
	}
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		closeListener(ln)
		return nil, nil, errors.Wrap(err, ErrConfig, "invalid sign-in configuration")
	}
	return h, cb.close, nil
}

// withTokens starts from stored tokens and saves every new one.
func withTokens(cfg *auth.AuthorizationCodeHandlerConfig, store TokenStore) {
	if tok, err := store.Load(); err == nil && tok != nil {
		cfg.InitialTokenSource = oauth2.StaticTokenSource(tok)
	}
	cfg.NewTokenSource = func(ctx context.Context, c *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		return &savingSource{next: c.TokenSource(ctx, tok), store: store}, nil
	}
}

type savingSource struct {
	next  oauth2.TokenSource
	store TokenStore
	mu    sync.Mutex
	last  string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.next.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok.AccessToken != s.last {
		if err := s.store.Save(tok); err != nil {
			return nil, errors.Wrap(err, ErrAuth, "cannot save the sign-in")
		}
		s.last = tok.AccessToken
	}
	return tok, nil
}

// callback receives the browser's return on the loopback listener. The
// port is held from the start (the redirect URI names it); the listener
// is closed after each sign-in and opened again on the same port for the
// next one.
type callback struct {
	mu   sync.Mutex // one sign-in at a time
	ln   net.Listener
	addr string
}

// listener returns the open listener, reopening the port if needed.
func (cb *callback) listener() (net.Listener, error) {
	if cb.ln != nil {
		return cb.ln, nil
	}
	ln, err := net.Listen("tcp", cb.addr)
	if err != nil {
		return nil, errors.Wrap(err, ErrAuth, "cannot reopen the sign-in callback port")
	}
	cb.ln = ln
	return ln, nil
}

func (cb *callback) close() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.ln == nil {
		return nil
	}
	err := cb.ln.Close()
	cb.ln = nil
	return err
}

func (cb *callback) fetcher(l Login) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		cb.mu.Lock()
		defer cb.mu.Unlock()
		timeout := l.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ln, err := cb.listener()
		if err != nil {
			return nil, err
		}
		cb.ln = nil // Shutdown closes it
		l.Show(args.URL)
		return wait(ctx, ln)
	}
}

// wait serves the callback until one request carries a code or an error.
// The state is checked by the SDK against the one it sent.
func wait(ctx context.Context, ln net.Listener) (*auth.AuthorizationResult, error) {
	got := make(chan *auth.AuthorizationResult, 1)
	failed := make(chan error, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path != "/callback":
			http.NotFound(w, r)
		case q.Get("error") != "":
			writePage(w, "Sign-in refused. You can close this tab.")
			sendOnce[error](failed, errors.New(ErrAuth, "the provider refused the sign-in: "+q.Get("error")))
		case q.Get("code") != "" && q.Get("state") != "":
			writePage(w, "Signed in. You can close this tab and go back to the agent.")
			sendOnce(got, &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")})
		default:
			http.Error(w, "missing code", http.StatusBadRequest)
		}
	})}
	go serve(srv, ln)
	defer shutdown(srv)
	select {
	case res := <-got:
		return res, nil
	case err := <-failed:
		return nil, err
	case <-ctx.Done():
		return nil, errors.Wrap(ctx.Err(), ErrAuth, "the sign-in was not completed in time")
	}
}

func sendOnce[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func serve(srv *http.Server, ln net.Listener) {
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return // the wait reports the outcome
	}
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return // the next sign-in reopens the port
	}
}

func writePage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := io.WriteString(w, msg); err != nil {
		return // the browser left; the result stands
	}
}

func closeListener(ln net.Listener) {
	if err := ln.Close(); err != nil {
		return
	}
}

// FileTokens keeps tokens in a private file (0600, in a 0700 directory).
type FileTokens string

// Load returns the stored token, or nil when there is none.
func (f FileTokens) Load() (*oauth2.Token, error) {
	root, err := os.OpenRoot(filepath.Dir(string(f)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closeQuiet(root)
	data, err := root.ReadFile(filepath.Base(string(f)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, errors.New(ErrAuth, "the stored sign-in is damaged")
	}
	return &tok, nil
}

// Save replaces the stored token atomically.
func (f FileTokens) Save(tok *oauth2.Token) error {
	dir := filepath.Dir(string(f))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(tok) // #nosec G117 -- storing the token is this type's job; the file is 0600 in a 0700 directory
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer closeQuiet(root)
	tmp := filepath.Base(string(f)) + ".tmp"
	if err := root.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return root.Rename(tmp, filepath.Base(string(f)))
}

func closeQuiet(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}
