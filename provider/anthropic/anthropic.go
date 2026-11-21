// anthropic.go: provider for Anthropic's Claude models
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package anthropic is an [agent.Provider] for Claude on the Anthropic
// API, through Anthropic's official Go SDK. It is a module of its own, so
// that an agent that does not use Claude does not carry the SDK.
//
// Claude thinks before it answers (adaptive thinking): its thinking comes
// back as reasoning parts of the message, and goes back to it, unchanged,
// at every turn, as the API keeps it (the agent's history only grows at its
// end). A block the API finds invalid anyway is dropped by it, never a
// refused request.
//
// Claude takes no sampling parameters: a request's Temperature is not
// sent (the models refuse it); the prompt and Effort steer the answer. A
// model that takes no forced tool choice (Opus 5.5, Sonnet 5.5, Fable 5.1
// and later) gets ToolChoiceRequired in words, as the API advises.
//
// Safe defaults, all configurable: the environment is not read (no
// ANTHROPIC_* variable changes where the prompts go or which key they
// carry), HTTPS except towards loopback, redirects never followed,
// response bodies bounded, upstream error bodies never returned, streams
// that stall are cut, no retries of its own (provider/retry does it).
package anthropic

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"
	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	agent "github.com/agilira/ringil-agent-sdk"
)

// DefaultBaseURL is the Anthropic API.
const DefaultBaseURL = "https://api.anthropic.com"

// DefaultStallTimeout is how long a response body may stay silent before
// the call is treated as interrupted.
const DefaultStallTimeout = 5 * time.Minute

// defaultMaxTokens is the output limit when neither the request nor
// Config sets one. Every call streams (a plain one too, without a sink),
// so no limit meets an HTTP timeout.
const defaultMaxTokens = 64000

// Config configures the provider returned by [New].
type Config struct {
	// Model is the model ID, e.g. claude-opus-5-5 or claude-haiku-5-5.
	Model string
	// BaseURL is the API root; empty is DefaultBaseURL. A proxy or
	// gateway that speaks the Anthropic API goes here.
	BaseURL string
	// APIKey is the key; the SDK sends it as a bearer token.
	APIKey string
	// KeyFunc, when set, is called for every request and wins over APIKey:
	// the hook for keyrings, vaults and rotating credentials.
	KeyFunc func(context.Context) (string, error)
	// ContextWindow is reported by Info, in tokens; zero means unknown.
	ContextWindow int
	// MaxTokens is used when a request does not set its own; zero is
	// 64000.
	MaxTokens int
	// Effort is how much the model thinks: low, medium, high, xhigh or
	// max; empty is the model's default.
	Effort string
	// NoThinking leaves thinking to the model's default, for the models
	// that do not take adaptive thinking (Claude Haiku 4.5 and older).
	NoThinking bool
	// AllowHTTP permits plain HTTP towards any host. Without it plain HTTP
	// is accepted only towards loopback.
	AllowHTTP bool
	// Client is the HTTP client to use. It is copied and its copy never
	// follows redirects, so a redirect cannot carry the key elsewhere.
	Client *http.Client
	// StallTimeout cuts a response body that stays silent for this long.
	// Zero means DefaultStallTimeout; negative disables the check.
	StallTimeout time.Duration
}

var efforts = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// provider talks to one Claude model.
type provider struct {
	cfg    Config
	client anthropic.Client
	rounds *rounds
	// unforced: the model refused a forced tool choice (Opus 5.5, Sonnet
	// 5.5, Fable 5.1 and later do); "required" is asked for in words.
	unforced atomic.Bool
}

var _ agent.Provider = (*provider)(nil)

// New validates cfg and returns a provider.
func New(cfg Config) (agent.Provider, error) {
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	stall := cfg.StallTimeout
	if stall == 0 {
		stall = DefaultStallTimeout
	}
	client := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		// The SDK's tracing reads ANTHROPIC_OPEN_TELEMETRY_* (one of
		// them puts the prompts in the spans): off, until the agent's own
		// telemetry decides what is traced.
		option.WithoutOpenTelemetry(),
		option.WithBaseURL(cfg.BaseURL),
		option.WithHTTPClient(noRedirectClient(cfg.Client)),
		option.WithMaxRetries(0),
		option.WithMiddleware(guard(stall)),
	)
	return &provider{cfg: cfg, client: client, rounds: newRounds()}, nil
}

// Info implements [agent.Provider].
func (p *provider) Info() agent.ModelInfo {
	return agent.ModelInfo{Name: p.cfg.Model, ContextWindow: p.cfg.ContextWindow}
}

func configError(msg string) error { return errors.New(agent.ErrProviderConfig, msg) }

func validate(cfg *Config) error {
	if err := checkSettings(*cfg); err != nil {
		return err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	base, err := baseURL(cfg.BaseURL, cfg.AllowHTTP)
	if err != nil {
		return err
	}
	cfg.BaseURL = base
	return nil
}

func checkSettings(cfg Config) error {
	switch {
	case strings.TrimSpace(cfg.Model) == "":
		return configError("model is required")
	case cfg.MaxTokens < 0 || cfg.ContextWindow < 0:
		return configError("max tokens and context window cannot be negative")
	case !efforts[cfg.Effort]:
		return configError("effort is low, medium, high, xhigh or max")
	case !validHeaderValue(cfg.APIKey):
		return configError("API key contains control characters")
	case cfg.APIKey == "" && cfg.KeyFunc == nil:
		return configError("an API key or a KeyFunc is required")
	}
	return nil
}

func baseURL(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", configError("base URL does not parse")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", configError("base URL needs a host and no credentials, query or fragment")
	}
	if err := checkScheme(u, allowHTTP); err != nil {
		return "", err
	}
	return strings.TrimRight(u.String(), "/") + "/", nil
}

func checkScheme(u *url.URL, allowHTTP bool) error {
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (allowHTTP || isLoopback(u.Hostname())):
		return nil
	case u.Scheme == "http":
		return configError("plain HTTP to a non-loopback host needs AllowHTTP")
	}
	return configError("base URL scheme must be http or https")
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHeaderValue(value string) bool {
	for _, r := range value {
		if r < ' ' && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

func noRedirectClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		copied := *base
		client = &copied
	}
	// A redirect would resend the prompt, and the key, to a host the
	// operator never configured.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

// apiKey is the key for one request.
func (p *provider) apiKey(ctx context.Context) (string, error) {
	if p.cfg.KeyFunc == nil {
		return p.cfg.APIKey, nil
	}
	key, err := p.cfg.KeyFunc(ctx)
	switch {
	case err != nil:
		return "", errors.Wrap(err, agent.ErrProviderAuth, "key source failed")
	case key == "":
		return "", errors.New(agent.ErrProviderAuth, "key source returned no key")
	case !validHeaderValue(key):
		return "", errors.New(agent.ErrProviderAuth, "key source returned control characters")
	}
	return key, nil
}
