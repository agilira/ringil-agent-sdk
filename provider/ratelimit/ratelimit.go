// ratelimit.go: pacing calls to stay inside a provider's quota
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package ratelimit paces model calls: a requests-per-minute rate with a
// burst, and a cap on calls in flight. Share one [Limiter] across every
// provider that draws from the same upstream quota (same key, same account).
//
// Calls wait for their turn; they are never refused. A cancelled context
// stops the wait.
package ratelimit

import (
	"context"

	"github.com/agilira/go-errors"
	"golang.org/x/time/rate"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Config sets the pace. A zero field means no limit.
type Config struct {
	// RequestsPerMinute is the sustained rate.
	RequestsPerMinute int
	// Burst is how many calls may start at once after a quiet period.
	// Zero means 1: a minute's whole allowance at once is rarely what the
	// provider tolerates.
	Burst int
	// MaxConcurrent caps calls in flight.
	MaxConcurrent int
}

// Limiter paces the providers it wraps.
type Limiter interface {
	// Wrap returns p paced by this limiter.
	Wrap(p agent.Provider) agent.Provider
}

// New returns a limiter.
func New(cfg Config) (Limiter, error) {
	if cfg.RequestsPerMinute < 0 || cfg.Burst < 0 || cfg.MaxConcurrent < 0 {
		return nil, errors.New(agent.ErrProviderConfig, "rate limits cannot be negative")
	}
	l := &limiter{}
	if cfg.RequestsPerMinute > 0 {
		l.rate = rate.NewLimiter(rate.Limit(float64(cfg.RequestsPerMinute)/60), max(cfg.Burst, 1))
	}
	if cfg.MaxConcurrent > 0 {
		l.slots = make(chan struct{}, cfg.MaxConcurrent)
	}
	return l, nil
}

type limiter struct {
	rate  *rate.Limiter
	slots chan struct{}
}

func (l *limiter) Wrap(p agent.Provider) agent.Provider { return &provider{next: p, limiter: l} }

// acquire waits for a slot, then for the rate. Taking the slot first means a
// call that waits for the rate already holds its place among the in-flight.
func (l *limiter) acquire(ctx context.Context) (func(), error) {
	release := func() {}
	if l.slots != nil {
		select {
		case l.slots <- struct{}{}:
			release = func() { <-l.slots }
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if l.rate != nil {
		if err := l.rate.Wait(ctx); err != nil {
			release()
			return nil, waitError(ctx, err)
		}
	}
	return release, nil
}

// waitError keeps a cancellation a context error. rate.Wait also fails when
// the deadline is shorter than the wait it would need; that is reported as a
// rate limit, so the caller knows no request was sent.
func waitError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return errors.Wrap(err, agent.ErrProviderRate, "rate wait exceeds the deadline")
}

type provider struct {
	next    agent.Provider
	limiter *limiter
}

func (p *provider) Info() agent.ModelInfo { return p.next.Info() }

func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := p.limiter.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return p.next.Generate(ctx, req, sink)
}
