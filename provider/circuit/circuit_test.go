// circuit_test.go: tests for the circuit breaker
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package circuit

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
)

// clock is a manual time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newBreaker(t *testing.T, cfg Config) (*breaker, *clock) {
	t.Helper()
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Unix(1_000_000, 0)}
	inner := b.(*breaker)
	inner.now = c.now
	return inner, c
}

func down() fake.Step {
	return fake.Fail(errors.New(agent.ErrProviderUnavailable, "503").AsRetryable())
}

func call(p agent.Provider) error {
	_, err := p.Generate(context.Background(), &agent.Request{}, nil)
	return err
}

func TestDefaultsAndValidation(t *testing.T) {
	b, _ := newBreaker(t, Config{})
	if b.cfg.Failures != DefaultFailures || b.cfg.OpenFor != DefaultOpenFor || b.State() != Closed {
		t.Fatalf("defaults %+v state %s", b.cfg, b.State())
	}
	for _, cfg := range []Config{{Failures: -1}, {OpenFor: -time.Second}} {
		if _, err := New(cfg); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("%+v accepted: %v", cfg, err)
		}
	}
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	b, _ := newBreaker(t, Config{Failures: 3, OpenFor: time.Minute})
	inner := fake.New(down(), down(), down(), fake.Reply("never sent"))
	p := b.Wrap(inner)
	for range 3 {
		if err := call(p); !errors.HasCode(err, agent.ErrProviderUnavailable) {
			t.Fatalf("err %v", err)
		}
	}
	err := call(p)
	wait, ok := agent.RetryAfter(err)
	if !errors.HasCode(err, ErrOpen) || !ok || wait != time.Minute || b.State() != Open {
		t.Fatalf("err %v wait %v state %s", err, wait, b.State())
	}
	if inner.Remaining() != 1 {
		t.Fatal("an open circuit let a call through")
	}
}

func TestSuccessResetsTheCount(t *testing.T) {
	b, _ := newBreaker(t, Config{Failures: 2})
	p := b.Wrap(fake.New(down(), fake.Reply("ok"), down(), fake.Reply("ok")))
	for i := range 4 {
		if err := call(p); (err != nil) != (i%2 == 0) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if b.State() != Closed {
		t.Fatalf("state %s", b.State())
	}
}

func TestErrorsThatProveTheServerIsUpDoNotCount(t *testing.T) {
	b, _ := newBreaker(t, Config{Failures: 1})
	for _, code := range []errors.ErrorCode{agent.ErrProviderInput, agent.ErrProviderAuth, agent.ErrProviderRate, agent.ErrProviderContext} {
		if err := call(b.Wrap(fake.New(fake.Fail(errors.New(code, "x"))))); !errors.HasCode(err, code) {
			t.Fatalf("err %v", err)
		}
	}
	if b.State() != Closed {
		t.Fatalf("state %s", b.State())
	}
}

func TestCancelledCallsDoNotCount(t *testing.T) {
	b, _ := newBreaker(t, Config{Failures: 1})
	ctx, cancel := context.WithCancel(context.Background())
	step := fake.Step{Chunks: []string{"a"}, Err: errors.New(agent.ErrProviderInterrupted, "cut")}
	_, err := b.Wrap(fake.New(step)).Generate(ctx, &agent.Request{}, func(string) error { cancel(); return nil })
	// The failure arrives after the caller gave up: it says nothing about
	// the server and must not open the circuit.
	if err == nil || b.State() != Closed {
		t.Fatalf("err %v state %s", err, b.State())
	}
}

func TestProbeClosesOrReopens(t *testing.T) {
	b, c := newBreaker(t, Config{Failures: 1, OpenFor: time.Minute})
	p := b.Wrap(fake.New(down(), down(), fake.Reply("ok")))
	mustFail(t, call(p), agent.ErrProviderUnavailable)
	c.advance(time.Minute)
	if b.State() != HalfOpen {
		t.Fatalf("state %s", b.State())
	}
	mustFail(t, call(p), agent.ErrProviderUnavailable) // failed probe
	if b.State() != Open {
		t.Fatalf("failed probe left state %s", b.State())
	}
	c.advance(time.Minute)
	if err := call(p); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if b.State() != Closed {
		t.Fatalf("successful probe left state %s", b.State())
	}
}

func mustFail(t *testing.T, err error, code errors.ErrorCode) {
	t.Helper()
	if !errors.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// gate blocks calls until released.
type gate struct {
	agent.Provider
	entered chan struct{}
	release chan struct{}
}

func (g *gate) Generate(context.Context, *agent.Request, agent.TextSink) (*agent.Response, error) {
	g.entered <- struct{}{}
	<-g.release
	return &agent.Response{Message: agent.AssistantText("ok")}, nil
}

func TestOnlyOneProbeAtATime(t *testing.T) {
	b, c := newBreaker(t, Config{Failures: 1, OpenFor: time.Minute})
	mustFail(t, call(b.Wrap(fake.New(down()))), agent.ErrProviderUnavailable)
	c.advance(time.Minute)
	g := &gate{Provider: fake.New(), entered: make(chan struct{}), release: make(chan struct{})}
	p := b.Wrap(g)
	done := make(chan error)
	go func() { done <- call(p) }()
	<-g.entered
	err := call(p)
	if wait, _ := agent.RetryAfter(err); !errors.HasCode(err, ErrOpen) || wait != probeWait {
		t.Fatalf("second call during probe: %v", err)
	}
	close(g.release)
	if err := <-done; err != nil || b.State() != Closed {
		t.Fatalf("probe %v state %s", err, b.State())
	}
}

func TestAbandonedProbeFreesTheSlot(t *testing.T) {
	b, c := newBreaker(t, Config{Failures: 1, OpenFor: time.Minute})
	mustFail(t, call(b.Wrap(fake.New(down()))), agent.ErrProviderUnavailable)
	c.advance(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pm, err := b.acquire()
	if err != nil || !pm.probe {
		t.Fatalf("probe permit %+v %v", pm, err)
	}
	b.finish(pm, classify(ctx, context.Canceled))
	if err := call(b.Wrap(fake.New(fake.Reply("ok")))); err != nil {
		t.Fatalf("next probe refused: %v", err)
	}
}

func TestStaleResultIgnored(t *testing.T) {
	b, _ := newBreaker(t, Config{Failures: 1, OpenFor: time.Minute})
	old, err := b.acquire()
	if err != nil {
		t.Fatal(err)
	}
	mustFail(t, call(b.Wrap(fake.New(down()))), agent.ErrProviderUnavailable)
	b.finish(old, healthy) // started before the circuit opened
	if b.State() != Open {
		t.Fatalf("a stale success closed the circuit: %s", b.State())
	}
}

func TestRetryWaitsForTheProbe(t *testing.T) {
	b, err := New(Config{Failures: 1, OpenFor: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	p := retry.Wrap(b.Wrap(fake.New(down(), fake.Reply("ok"))), retry.Config{MaxAttempts: 3, BaseDelay: time.Millisecond})
	resp, err := p.Generate(context.Background(), &agent.Request{}, nil)
	if err != nil || resp.Message.Text() != "ok" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	b, _ := newBreaker(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Wrap(fake.New()).Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestInfoForwarded(t *testing.T) {
	b, _ := newBreaker(t, Config{})
	if b.Wrap(fake.NewWithInfo(agent.ModelInfo{Name: "eos"})).Info().Name != "eos" {
		t.Fatal("Info not forwarded")
	}
}
