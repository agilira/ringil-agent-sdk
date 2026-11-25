// fallback.go: another model when a provider does not answer
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package fallback answers with the next model when a provider does not
// answer: its server is down, it asks to slow down, its answer stops
// halfway or comes malformed, or it refuses the key. A request the first
// model refuses (too long, not valid) is returned as it is: another model
// would refuse it too, or answer what was not asked. A model standing in
// that cannot take a request (a smaller context, no tools) is passed, and
// when none answers the error returned is the one the retry decorator can
// wait out (the first model's 429 with its Retry-After), as with no
// fallback.
//
// A provider that failed is passed over for a cooldown, doubled at each
// failure in a row; then the next call tries it again, so the run goes
// back to the first model as soon as it answers. When every provider is
// cooling down they are tried anyway, in order: a call is never refused
// for lack of trying.
//
// With one provider [New] returns it as it is: nothing changes.
//
// Wrap each provider in its own price (and circuit, rate limit) and the
// fallback in the retry decorator: a provider that fails hands over at
// once, and the retry waits only when none answered.
//
//	chain, _ := fallback.New([]agent.Provider{a, b}, fallback.Config{})
//	model := retry.Wrap(chain, retry.Config{})
package fallback

import (
	"context"
	stderrors "errors"
	"slices"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Defaults applied to zero fields of [Config].
const (
	DefaultCooldown    = time.Minute
	DefaultMaxCooldown = 15 * time.Minute
)

// Config controls the fallback. Zero fields take the defaults.
type Config struct {
	// Cooldown is how long a provider that failed is passed over; each
	// failure in a row doubles it, up to MaxCooldown.
	Cooldown, MaxCooldown time.Duration
	// OnRestart is called before handing over a call that had already
	// streamed text, so the host discards what it showed. Nil means such
	// a call is not handed over: its error is returned.
	OnRestart func()
	// OnSwitch is told when the model that answers changes: before the
	// next model is tried after a failure, and when a model answers again.
	OnSwitch func(Switch)
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Switch is a change of the model that answers.
type Switch struct {
	// From and To are the models' names (ModelInfo.Name).
	From, To string
	// Err is why From was left; nil when the run goes back to a model
	// that answers again.
	Err error
}

// New returns a provider that tries providers in order. One provider is
// returned as it is.
func New(providers []agent.Provider, cfg Config) (agent.Provider, error) {
	if len(providers) == 0 {
		return nil, errors.New(agent.ErrProviderConfig, "fallback needs at least one provider")
	}
	for _, p := range providers {
		if p == nil {
			return nil, errors.New(agent.ErrProviderConfig, "fallback: a provider is nil")
		}
	}
	if len(providers) == 1 {
		return providers[0], nil
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	if cfg.MaxCooldown < cfg.Cooldown {
		cfg.MaxCooldown = max(DefaultMaxCooldown, cfg.Cooldown)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &chain{cfg: cfg, links: make([]link, len(providers)), providers: slices.Clone(providers)}, nil
}

type chain struct {
	cfg       Config
	providers []agent.Provider

	mu      sync.Mutex
	links   []link
	current int // the one that answered last
}

// link is a provider's health.
type link struct {
	failures int
	until    time.Time // passed over before this
}

// Info describes the model that would answer the next call: its name and
// context window are those the agent works with.
func (c *chain) Info() agent.ModelInfo { return c.providers[c.order()[0]].Info() }

// Generate tries the providers in order, passing over those cooling down.
func (c *chain) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	var best error // what to say when none answers
	order := c.order()
	for k, i := range order {
		streamed, aborted, attempt := track(sink)
		resp, err := c.providers[i].Generate(ctx, req, attempt)
		if err == nil {
			c.answered(i, best)
			return resp, nil
		}
		if *aborted {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A model standing in that cannot take this request (too long for
		// it, no tools, a key it refused) is passed: the error that counts
		// is the first model's, which the retry waits out.
		standIn := isStandIn(best, err, *streamed)
		if !standIn && !c.leave(i, err, *streamed, k+1 < len(order)) {
			return nil, err
		}
		if !standIn {
			best = pick(best, err)
		}
		c.tell(i, order[k+1:], err)
	}
	return nil, best
}

func isStandIn(best, err error, streamed bool) bool {
	return best != nil && !handOver(err) && !streamed
}

// pick is the error to return of two: one the retry can wait out first,
// then the one with the shorter wait.
func pick(best, err error) error {
	if best == nil {
		return err
	}
	if retryable(err) != retryable(best) {
		if retryable(err) {
			return err
		}
		return best
	}
	wb, _ := agent.RetryAfter(best)
	we, _ := agent.RetryAfter(err)
	if we < wb {
		return err
	}
	return best
}

func retryable(err error) bool {
	var r errors.Retryable
	return stderrors.As(err, &r) && r.IsRetryable()
}

// leave hands a failed call over to the next provider, telling the host
// when another model follows, or says it cannot: the request caused it, or
// text went out and the host cannot take it back.
func (c *chain) leave(i int, err error, streamed, more bool) bool {
	if !handOver(err) {
		return false
	}
	c.failed(i)
	if streamed && c.cfg.OnRestart == nil {
		return false
	}
	if streamed && more {
		c.cfg.OnRestart()
	}
	return true
}

// tell says the call goes from i to the next model, if there is one.
func (c *chain) tell(i int, rest []int, err error) {
	if len(rest) > 0 && c.cfg.OnSwitch != nil {
		c.cfg.OnSwitch(Switch{From: c.providers[i].Info().Name, To: c.providers[rest[0]].Info().Name, Err: err})
	}
}

// order is the providers to try: those not cooling down first, in their
// order, then the others, the soonest back first.
func (c *chain) order() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	ready, cooling := make([]int, 0, len(c.links)), []int{}
	for i, l := range c.links {
		if now.Before(l.until) {
			cooling = append(cooling, i)
		} else {
			ready = append(ready, i)
		}
	}
	for a := 1; a < len(cooling); a++ { // few: insertion sort by return time
		for b := a; b > 0 && c.links[cooling[b]].until.Before(c.links[cooling[b-1]].until); b-- {
			cooling[b], cooling[b-1] = cooling[b-1], cooling[b]
		}
	}
	return append(ready, cooling...)
}

// answered records that i answered. A switch was told as it happened;
// here only the return to a model, with no failure in this call.
func (c *chain) answered(i int, failedNow error) {
	c.mu.Lock()
	c.links[i] = link{}
	from := c.current
	c.current = i
	c.mu.Unlock()
	if from != i && failedNow == nil && c.cfg.OnSwitch != nil {
		c.cfg.OnSwitch(Switch{From: c.providers[from].Info().Name, To: c.providers[i].Info().Name})
	}
}

// failed passes i over for its cooldown, doubled at each failure in a
// row. Calls that fail together (workers in parallel) are one failure:
// one already cooling down is the same outage.
func (c *chain) failed(i int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := &c.links[i]
	if c.cfg.Now().Before(l.until) {
		return
	}
	l.failures++
	wait := c.cfg.Cooldown
	for n := 1; n < l.failures && wait < c.cfg.MaxCooldown; n++ {
		if wait > c.cfg.MaxCooldown/2 {
			wait = c.cfg.MaxCooldown
			break
		}
		wait *= 2
	}
	l.until = c.cfg.Now().Add(min(wait, c.cfg.MaxCooldown))
}

// handOver says whether another model may answer instead: the provider
// did not answer, or refused the key. What the request itself caused is
// not handed over.
func handOver(err error) bool {
	for _, code := range []errors.ErrorCode{
		agent.ErrProviderRate, agent.ErrProviderUnavailable, agent.ErrProviderInterrupted,
		agent.ErrProviderResponse, agent.ErrProviderAuth, circuitOpen,
	} {
		if errors.HasCode(err, code) {
			return true
		}
	}
	return retryable(err) && !errors.HasCode(err, agent.ErrProviderInput) && !errors.HasCode(err, agent.ErrProviderContext)
}

// circuitOpen is the code of provider/circuit, named here so this package
// does not import it.
const circuitOpen errors.ErrorCode = "CIRCUIT_OPEN"

// track wraps sink to record whether any text went out or sink failed.
func track(sink agent.TextSink) (*bool, *bool, agent.TextSink) {
	streamed, aborted := new(bool), new(bool)
	if sink == nil {
		return streamed, aborted, nil
	}
	return streamed, aborted, func(delta string) error {
		*streamed = true
		err := sink(delta)
		if err != nil {
			*aborted = true
		}
		return err
	}
}
