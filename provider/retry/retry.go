// retry.go: retrying transient provider failures
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package retry wraps an [agent.Provider] so that transient failures (rate
// limits, server errors, cut streams) are retried with backoff.
//
// A model call has no external effects, so repeating it is safe; it only
// costs tokens. The one subtlety is streaming: if a failed attempt already
// sent text to the sink, a retry would send text again. Such a call is
// retried only when the host sets OnRestart, which tells it to discard what
// it showed.
package retry

import (
	"context"
	stderrors "errors"
	"math/rand/v2"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Defaults applied to zero fields of [Config].
const (
	DefaultMaxAttempts   = 4
	DefaultBaseDelay     = time.Second
	DefaultMaxDelay      = 30 * time.Second
	DefaultMaxRetryAfter = 2 * time.Minute
)

// Config controls retries. Zero fields take the defaults above.
type Config struct {
	// MaxAttempts counts the first call too; 1 disables retries.
	MaxAttempts int
	// BaseDelay is the first backoff; each retry doubles it, up to MaxDelay.
	BaseDelay time.Duration
	MaxDelay  time.Duration
	// MaxRetryAfter is the longest provider-requested wait accepted; a
	// longer one returns the error to the caller instead of blocking.
	MaxRetryAfter time.Duration
	// OnRestart is called before retrying an attempt that had already
	// streamed text. Nil means such attempts are not retried.
	OnRestart func()
}

// provider is an [agent.Provider] with retries.
type provider struct {
	next  agent.Provider
	cfg   Config
	sleep func(context.Context, time.Duration) error
}

var _ agent.Provider = (*provider)(nil)

// Wrap returns next with retries.
func Wrap(next agent.Provider, cfg Config) agent.Provider {
	return &provider{next: next, cfg: withDefaults(cfg), sleep: sleep}
}

func withDefaults(cfg Config) Config {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = DefaultBaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = DefaultMaxDelay
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = DefaultMaxRetryAfter
	}
	return cfg
}

// Info implements [agent.Provider].
func (p *provider) Info() agent.ModelInfo { return p.next.Info() }

// Generate implements [agent.Provider].
func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	for attempt := 1; ; attempt++ {
		streamed, attemptSink := track(sink)
		resp, err := p.next.Generate(ctx, req, attemptSink)
		if err == nil {
			return resp, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr // cancelled: that is why it stopped, not the attempt's error
		}
		wait, ok := p.plan(ctx, err, attempt, *streamed)
		if !ok {
			return nil, err
		}
		if *streamed {
			p.cfg.OnRestart()
		}
		if err := p.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// plan decides whether to retry and how long to wait first.
func (p *provider) plan(ctx context.Context, err error, attempt int, streamed bool) (time.Duration, bool) {
	if attempt >= p.cfg.MaxAttempts || ctx.Err() != nil || !retryable(err) {
		return 0, false
	}
	if streamed && p.cfg.OnRestart == nil {
		return 0, false
	}
	if wait, ok := agent.RetryAfter(err); ok {
		return wait, wait <= p.cfg.MaxRetryAfter
	}
	return p.backoff(attempt), true
}

func retryable(err error) bool {
	var r errors.Retryable
	return stderrors.As(err, &r) && r.IsRetryable()
}

// backoff doubles the delay per attempt and keeps a random half of it, so
// clients that failed together do not retry together.
func (p *provider) backoff(attempt int) time.Duration {
	d := p.cfg.BaseDelay
	for i := 1; i < attempt && d < p.cfg.MaxDelay; i++ {
		d *= 2
	}
	d = min(d, p.cfg.MaxDelay)
	half := d / 2
	return half + rand.N(half+1) // #nosec G404 -- jitter, not a secret
}

// track wraps sink to record whether any text went out during an attempt.
func track(sink agent.TextSink) (*bool, agent.TextSink) {
	streamed := new(bool)
	if sink == nil {
		return streamed, nil
	}
	return streamed, func(delta string) error {
		*streamed = true
		return sink(delta)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
