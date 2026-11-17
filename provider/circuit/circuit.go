// circuit.go: stop calling a provider that keeps failing
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package circuit stops sending calls to a provider that keeps failing, so
// an outage costs one fast error per call instead of a timeout per call.
//
// After Failures consecutive health failures (server errors, cut streams)
// the circuit opens: calls fail at once with [ErrOpen] for OpenFor. Then
// one call goes through as a probe; its success closes the circuit, its
// failure opens it again. Errors that prove the server is up (a refused
// request, a bad key) do not count, and neither does a cancelled call.
//
// ErrOpen is retryable and carries the remaining wait, so the retry
// decorator wrapped around the breaker waits for the probe on its own.
package circuit

import (
	"context"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ErrOpen is returned without contacting the provider while the circuit is
// open.
const ErrOpen errors.ErrorCode = "CIRCUIT_OPEN"

// Defaults applied to zero fields of [Config].
const (
	DefaultFailures = 5
	DefaultOpenFor  = 30 * time.Second
)

// probeWait is the retry hint given to calls that arrive while the probe
// is in flight.
const probeWait = time.Second

// Config controls when the circuit opens and for how long.
type Config struct {
	Failures int
	OpenFor  time.Duration
}

// State is the breaker's current state.
type State string

// States.
const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half_open"
)

// Breaker guards the providers it wraps. Wrap several providers with one
// breaker only when they share the same upstream.
type Breaker interface {
	Wrap(p agent.Provider) agent.Provider
	State() State
}

// New returns a closed breaker.
func New(cfg Config) (Breaker, error) {
	if cfg.Failures < 0 || cfg.OpenFor < 0 {
		return nil, errors.New(agent.ErrProviderConfig, "circuit settings cannot be negative")
	}
	if cfg.Failures == 0 {
		cfg.Failures = DefaultFailures
	}
	if cfg.OpenFor == 0 {
		cfg.OpenFor = DefaultOpenFor
	}
	return &breaker{cfg: cfg, now: time.Now}, nil
}

type breaker struct {
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	failures int
	until    time.Time // zero while closed
	probing  bool
	// generation changes whenever the circuit opens or closes, so a call
	// that started under an old state cannot decide the new one.
	generation uint64
}

type permit struct {
	generation uint64
	probe      bool
}

func (b *breaker) Wrap(p agent.Provider) agent.Provider { return &provider{next: p, breaker: b} }

func (b *breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.until.IsZero():
		return Closed
	case b.probing || !b.now().Before(b.until):
		return HalfOpen
	default:
		return Open
	}
}

func (b *breaker) acquire() (permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until.IsZero() {
		return permit{generation: b.generation}, nil
	}
	if wait := b.until.Sub(b.now()); wait > 0 {
		return permit{}, openError(wait)
	}
	if b.probing {
		return permit{}, openError(probeWait)
	}
	b.probing = true
	return permit{generation: b.generation, probe: true}, nil
}

func openError(wait time.Duration) error {
	return agent.WithRetryAfter(errors.New(ErrOpen, "circuit open; no request sent"), wait).AsRetryable()
}

// finish records a call's outcome.
func (b *breaker) finish(p permit, outcome outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.generation != b.generation {
		return
	}
	switch outcome {
	case abandoned:
		b.probing = b.probing && !p.probe
	case healthy:
		b.close()
	case unhealthy:
		b.fail(p.probe)
	}
}

func (b *breaker) close() {
	if !b.until.IsZero() {
		b.generation++
	}
	b.failures, b.until, b.probing = 0, time.Time{}, false
}

func (b *breaker) fail(probe bool) {
	b.failures++
	if probe || b.failures >= b.cfg.Failures {
		b.failures, b.probing = 0, false
		b.until = b.now().Add(b.cfg.OpenFor)
		b.generation++
	}
}

type outcome int

const (
	healthy outcome = iota
	unhealthy
	abandoned
)

// classify decides whether a result says anything about the server's
// health. Only failures that point at the server count.
func classify(ctx context.Context, err error) outcome {
	switch {
	case err == nil:
		return healthy
	case ctx.Err() != nil:
		return abandoned
	case errors.HasCode(err, agent.ErrProviderUnavailable), errors.HasCode(err, agent.ErrProviderInterrupted):
		return unhealthy
	default:
		return healthy
	}
}

type provider struct {
	next    agent.Provider
	breaker *breaker
}

func (p *provider) Info() agent.ModelInfo { return p.next.Info() }

func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	permit, err := p.breaker.acquire()
	if err != nil {
		return nil, err
	}
	resp, err := p.next.Generate(ctx, req, sink)
	p.breaker.finish(permit, classify(ctx, err))
	return resp, err
}
