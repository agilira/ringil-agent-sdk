// budget.go: a spending cap shared by every provider that wraps it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package budget caps how many model calls and tokens a group of providers
// may spend. One [Budget] can wrap several providers (the main agent, its
// workers, a summarizer) so they draw from the same allowance.
//
// A call is counted before it is sent: a request that fails may still have
// been billed. Tokens are known only after the response, so the call that
// crosses the token cap is delivered and the next one is refused.
//
// Wrap the budget inside the retry decorator, so every attempt is counted.
package budget

import (
	"context"
	"sync"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Error codes returned when the budget refuses a call. Neither is
// retryable: waiting does not refill a budget.
const (
	ErrCalls  errors.ErrorCode = "BUDGET_CALLS"
	ErrTokens errors.ErrorCode = "BUDGET_TOKENS"
)

// Limits caps spending. A zero field means no cap.
type Limits struct {
	Calls  int
	Tokens int64
}

// Spent is what the providers have consumed so far.
type Spent struct {
	Calls int
	// Input, Output, CacheRead and CacheWrite are the totals the
	// providers reported.
	Input, Output, CacheRead, CacheWrite int64
	// Unreported counts calls that ended without usage: failed calls and
	// servers that do not report tokens. Their tokens are not in the totals,
	// so with a token cap the real spending can be higher.
	Unreported int
}

// Tokens is the input plus output total.
func (s Spent) Tokens() int64 { return s.Input + s.Output }

// Budget is a shared allowance.
type Budget interface {
	// Wrap returns p drawing from this budget.
	Wrap(p agent.Provider) agent.Provider
	// Spent returns a snapshot of the consumption.
	Spent() Spent
	// Limits returns the caps.
	Limits() Limits
}

// New returns a budget with the given caps.
func New(limits Limits) (Budget, error) {
	if limits.Calls < 0 || limits.Tokens < 0 {
		return nil, errors.New(agent.ErrProviderConfig, "budget limits cannot be negative")
	}
	return &budget{limits: limits}, nil
}

type budget struct {
	limits Limits
	mu     sync.Mutex
	spent  Spent
}

func (b *budget) Wrap(p agent.Provider) agent.Provider { return &provider{next: p, budget: b} }

func (b *budget) Limits() Limits { return b.limits }

func (b *budget) Spent() Spent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// reserve counts the call before it is sent, or refuses it.
func (b *budget) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits.Calls > 0 && b.spent.Calls >= b.limits.Calls {
		return errors.New(ErrCalls, "model call budget exhausted")
	}
	if b.limits.Tokens > 0 && b.spent.Tokens() >= b.limits.Tokens {
		return errors.New(ErrTokens, "token budget exhausted")
	}
	b.spent.Calls++
	return nil
}

func (b *budget) settle(resp *agent.Response, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil || resp == nil || resp.Usage == (agent.Usage{}) {
		b.spent.Unreported++
		return
	}
	b.spent.Input += int64(max(resp.Usage.Input, 0))
	b.spent.Output += int64(max(resp.Usage.Output, 0))
	b.spent.CacheRead += int64(max(resp.Usage.CacheRead, 0))
	b.spent.CacheWrite += int64(max(resp.Usage.CacheWrite, 0))
}

type provider struct {
	next   agent.Provider
	budget *budget
}

func (p *provider) Info() agent.ModelInfo { return p.next.Info() }

func (p *provider) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.budget.reserve(); err != nil {
		return nil, err
	}
	resp, err := p.next.Generate(ctx, req, sink)
	p.budget.settle(resp, err)
	return resp, err
}
