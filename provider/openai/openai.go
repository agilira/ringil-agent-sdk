// openai.go: provider for OpenAI-compatible chat completion APIs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package openai is an [agent.Provider] for the OpenAI chat completions API
// and every server that speaks it: OpenAI, vLLM, Ollama, llama.cpp,
// OpenRouter, Groq, DeepSeek, Together and most free tiers.
//
// Safe defaults, all configurable: HTTPS except towards loopback (AllowHTTP
// lifts it), redirects never followed, upstream error bodies never returned,
// streams that stall are cut.
package openai

import (
	"context"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// DefaultStallTimeout is how long a response body may stay silent before
// the call is treated as interrupted.
const DefaultStallTimeout = 5 * time.Minute

// Config configures the provider returned by [New].
type Config struct {
	// BaseURL is the API root, e.g. https://api.openai.com/v1 or
	// http://localhost:11434/v1. "/chat/completions" is appended.
	BaseURL string
	// Model is sent with every request and reported by Info.
	Model string
	// APIKey is sent as a bearer token. Empty sends no Authorization header,
	// as local servers expect.
	APIKey string
	// KeyFunc, when set, is called for every request and wins over APIKey.
	// It is the hook for keyrings, vaults and rotating credentials.
	KeyFunc func(context.Context) (string, error)
	// ContextWindow is reported by Info, in tokens; zero means unknown.
	ContextWindow int
	// MaxTokens is used when a request does not set its own; zero sends none.
	MaxTokens int
	// AllowHTTP permits plain HTTP towards any host, e.g. a model server on
	// the LAN. Without it plain HTTP is accepted only towards loopback.
	AllowHTTP bool
	// EmbedInputType sends input_type ("passage" for stored texts,
	// "query" for searches) on embedding requests, as asymmetric models
	// require (NVIDIA NIM's embedding models refuse requests without it).
	// Chat requests ignore it.
	EmbedInputType bool
	// StatesCost: the API puts what a call cost, in US dollars, in
	// usage.cost (OpenRouter does); it fills Usage.Cost, and price.Wrap
	// keeps it. Off, a "cost" from the server is ignored: its unit is not
	// known.
	StatesCost bool
	// Images sends the images of user messages to the model, which must
	// read them (a vision model). Without it, the model is told the user
	// sent images it cannot see.
	Images bool
	// Documents sends PDFs (of user messages and tool results) as files,
	// for an API that reads them (OpenAI, OpenRouter). Without it, the
	// model is told there is a PDF it cannot read; a tool's text of it
	// still goes.
	Documents bool
	// Headers are added to every request (e.g. OpenRouter's HTTP-Referer).
	// They cannot replace Authorization, Content-Type, Accept or Host.
	Headers map[string]string
	// Client is the HTTP client to use. It is copied and its copy never
	// follows redirects, so a redirect cannot carry the key elsewhere.
	Client *http.Client
	// MaxCompletionTokens sends the output limit as max_completion_tokens,
	// which OpenAI's reasoning models want in place of max_tokens (most
	// other servers take either). New sets it for api.openai.com.
	MaxCompletionTokens bool
	// ReasoningEffort is sent as reasoning_effort (minimal, low, medium,
	// high) for the models that take it; empty sends none.
	ReasoningEffort string
	// Extra are fields added to every request body as they are (vLLM's
	// top_k, OpenRouter's provider routing, Ollama's options...). Those the
	// provider writes itself (model, messages, tools, stream, the output
	// limit) cannot be set this way.
	Extra map[string]any
	// StallTimeout cuts a response body that stays silent for this long.
	// Zero means DefaultStallTimeout; negative disables the check.
	StallTimeout time.Duration
}

// provider talks to one model behind an OpenAI-compatible endpoint.
type provider struct {
	cfg      Config
	endpoint string
	client   *http.Client
	stall    time.Duration
	// unformatted: the server refused response_format, and took the same
	// request with the schema asked in words; Parse checks the answer. Kept
	// apart with tools and without: some servers refuse the two together
	// only.
	unformatted, unformattedTools atomic.Bool
}

var _ agent.Provider = (*provider)(nil)

// New validates cfg and returns a provider.
func New(cfg Config) (agent.Provider, error) {
	endpoint, err := endpointURL(cfg.BaseURL, cfg.AllowHTTP)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Headers = cloneHeaders(cfg.Headers)
	if u, err := url.Parse(endpoint); err == nil && strings.EqualFold(u.Hostname(), "api.openai.com") {
		cfg.MaxCompletionTokens = true
	}
	return &provider{cfg: cfg, endpoint: endpoint, client: noRedirectClient(cfg.Client), stall: stallTimeout(cfg.StallTimeout)}, nil
}

// Info implements [agent.Provider].
func (p *provider) Info() agent.ModelInfo {
	return agent.ModelInfo{Name: p.cfg.Model, ContextWindow: p.cfg.ContextWindow}
}

func configError(msg string) error { return errors.New(agent.ErrProviderConfig, msg) }

func endpointURL(base string, allowHTTP bool) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", configError("base URL does not parse")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", configError("base URL needs a host and no credentials, query or fragment")
	}
	if err := checkScheme(u, allowHTTP); err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	u.RawPath = ""
	return u.String(), nil
}

func checkScheme(u *url.URL, allowHTTP bool) error {
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (allowHTTP || isLoopback(u.Hostname())):
		return nil
	case u.Scheme == "http":
		return configError("plain HTTP to a non-loopback host needs AllowHTTP")
	default:
		return configError("base URL scheme must be http or https")
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var reservedHeaders = map[string]bool{"Authorization": true, "Content-Type": true, "Accept": true, "Host": true, "Content-Length": true}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Model) == "" {
		return configError("model is required")
	}
	if cfg.MaxTokens < 0 || cfg.ContextWindow < 0 {
		return configError("max tokens and context window cannot be negative")
	}
	if !validHeaderValue(cfg.APIKey) {
		return configError("API key contains control characters")
	}
	for name, value := range cfg.Headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if reservedHeaders[canonical] || !validHeaderName(name) || !validHeaderValue(value) {
			return configError("header is reserved or malformed")
		}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`()<>@,;:\"/[]?={}`, r) {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, r := range value {
		if r < ' ' && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func noRedirectClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		copied := *base
		client = &copied
	}
	// A redirect would resend the prompt, and possibly the key, to a host the
	// operator never configured.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func stallTimeout(d time.Duration) time.Duration {
	if d == 0 {
		return DefaultStallTimeout
	}
	return d
}

func (p *provider) apiKey(ctx context.Context) (string, error) {
	if p.cfg.KeyFunc == nil {
		return p.cfg.APIKey, nil
	}
	key, err := p.cfg.KeyFunc(ctx)
	if err != nil {
		return "", errors.Wrap(err, agent.ErrProviderAuth, "key source failed")
	}
	if !validHeaderValue(key) {
		return "", errors.New(agent.ErrProviderAuth, "key source returned control characters")
	}
	return key, nil
}
