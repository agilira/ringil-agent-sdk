// chatgpt.go: provider for the models of a ChatGPT subscription
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package chatgpt is an [agent.Provider] for the models of a ChatGPT
// subscription (Plus, Pro, Business), as Codex uses them: OpenAI lets
// agents other than its own use a subscription. The user signs in once
// ([Login]); a [Session] keeps the sign-in fresh in a secrets store.
//
// It speaks OpenAI's Responses API at the ChatGPT backend, always
// streamed and never stored at OpenAI (store: false): the model's
// reasoning comes back encrypted and is sent back with the conversation.
// As the other adapters: HTTPS, redirects never followed, upstream error
// bodies never returned, streams that stall cut.
package chatgpt

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/httpcall"
)

// DefaultBaseURL is the ChatGPT backend Codex uses.
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

// DefaultStallTimeout is how long a response may stay silent before the
// call counts as interrupted.
const DefaultStallTimeout = 5 * time.Minute

// Config configures the provider returned by [New].
type Config struct {
	// Model is the subscription's model, as [Models] lists them.
	Model string
	// Session gives the access token (required).
	Session *Session
	// BaseURL is the backend; tests point it elsewhere.
	BaseURL string
	// ReasoningEffort is the model's thinking: low, medium, high (the
	// models' own names); empty leaves the model's default.
	ReasoningEffort string
	// ContextWindow is reported by Info, in tokens; zero means unknown.
	ContextWindow int
	// Client is copied and never follows redirects.
	Client *http.Client
	// StallTimeout cuts a response silent for this long; zero means
	// DefaultStallTimeout, negative disables the check.
	StallTimeout time.Duration
}

type provider struct {
	cfg      Config
	endpoint string
	client   *http.Client
	stall    time.Duration
}

var _ agent.Provider = (*provider)(nil)

// New validates cfg and returns a provider.
func New(cfg Config) (agent.Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" || cfg.Session == nil || cfg.Session.Store == nil {
		return nil, errors.New(agent.ErrProviderConfig, "a model and a session with a store are required")
	}
	if cfg.ContextWindow < 0 {
		return nil, errors.New(agent.ErrProviderConfig, "the context window cannot be negative")
	}
	switch cfg.ReasoningEffort {
	case "", "minimal", "low", "medium", "high", "xhigh":
	default:
		return nil, errors.New(agent.ErrProviderConfig, "reasoning effort must be minimal, low, medium, high or xhigh")
	}
	endpoint, err := endpointURL(cfg.BaseURL, "/responses")
	if err != nil {
		return nil, err
	}
	stall := cfg.StallTimeout
	if stall == 0 {
		stall = DefaultStallTimeout
	}
	return &provider{cfg: cfg, endpoint: endpoint, client: noRedirect(cfg.Client), stall: stall}, nil
}

func endpointURL(base, path string) (string, error) {
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New(agent.ErrProviderConfig, "the base URL does not parse")
	}
	if err := checkBase(u); err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

// checkBase: a host, no credentials, query or fragment, and https (http
// only to this machine, for tests and local proxies).
func checkBase(u *url.URL) error {
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New(agent.ErrProviderConfig, "the base URL needs a host and no credentials, query or fragment")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return errors.New(agent.ErrProviderConfig, "the base URL must be https")
	}
	return nil
}

func loopback(host string) bool { return host == "127.0.0.1" || host == "::1" || host == "localhost" }

func noRedirect(c *http.Client) *http.Client {
	out := &http.Client{}
	if c != nil {
		copied := *c
		out = &copied
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return out
}

// Info implements [agent.Provider].
func (p *provider) Info() agent.ModelInfo {
	return agent.ModelInfo{Name: p.cfg.Model, ContextWindow: p.cfg.ContextWindow}
}

// Generate implements [agent.Provider]. The backend only streams: with a
// nil sink the stream is read whole.
func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if req == nil {
		return nil, errors.New(agent.ErrProviderInput, "nil request")
	}
	body, err := p.encode(req)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderInput, "request does not encode")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	resp, err := p.send(ctx, body, cancel)
	if err != nil {
		return httpcall.Finish(ctx, nil, err)
	}
	reader := httpcall.NewStallReader(resp.Body, p.stall, cancel)
	out, err := decodeStream(reader, sink, p.source())
	reader.Stop()
	if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
		err = httpcall.Interrupted(closeErr)
	}
	return httpcall.Finish(ctx, out, err)
}

// send posts the request with a fresh token; a token refused (401) is
// renewed once, as another process may have refreshed it.
func (p *provider) send(ctx context.Context, body []byte, cancel context.CancelCauseFunc) (*http.Response, error) {
	tok, err := p.cfg.Session.Token(ctx)
	if err != nil {
		return nil, signedOut(err)
	}
	resp, err := p.post(ctx, body, tok, cancel)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return p.checked(resp, err)
	}
	_ = httpcall.CloseError(resp) // #nosec G104 -- refused: asked again below
	if tok, err = p.cfg.Session.Renew(ctx, tok); err != nil {
		return nil, signedOut(err)
	}
	return p.checked(p.post(ctx, body, tok, cancel))
}

func (p *provider) checked(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, httpcall.WithSecondary(httpcall.StatusError(resp), httpcall.CloseError(resp))
	}
	return resp, nil
}

// signedOut keeps the sign-in's own code, so a host can ask the user to
// sign in again, and marks it a credential failure.
func signedOut(err error) error {
	if errors.HasCode(err, ErrSignedOut) {
		return errors.Wrap(err, agent.ErrProviderAuth, err.Error())
	}
	return err
}

func (p *provider) post(ctx context.Context, body []byte, tok Tokens, cancel context.CancelCauseFunc) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "request cannot be built")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok.Access)
	req.Header.Set("ChatGPT-Account-ID", tok.Account)
	req.Header.Set("originator", originator)
	if p.stall > 0 {
		t := time.AfterFunc(p.stall, func() { cancel(httpcall.ErrStalled) })
		defer t.Stop()
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, httpcall.NetworkError(ctx, err)
	}
	return resp, nil
}

// source marks the reasoning this provider's model made.
func (p *provider) source() string { return "chatgpt:" + p.cfg.Model }
