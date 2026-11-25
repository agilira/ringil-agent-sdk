// fallback_test.go: tests for answering with another model
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package fallback

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

var down = errors.New(agent.ErrProviderUnavailable, "down").AsRetryable()

func named(name string, steps ...fake.Step) fake.Script {
	return fake.NewWithInfo(agent.ModelInfo{Name: name, ContextWindow: len(name)}, steps...)
}

// clock is a settable time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func mustNew(t *testing.T, ps []agent.Provider, cfg Config) agent.Provider {
	t.Helper()
	p, err := New(ps, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func call(p agent.Provider) (string, error) {
	resp, err := p.Generate(context.Background(), &agent.Request{}, nil)
	if err != nil {
		return "", err
	}
	return resp.Message.Text(), nil
}

// One provider is returned as it is: a single model changes nothing.
func TestOneProviderIsItself(t *testing.T) {
	a := named("a")
	if p := mustNew(t, []agent.Provider{a}, Config{}); p != agent.Provider(a) {
		t.Fatal("one provider was wrapped")
	}
	if _, err := New(nil, Config{}); !errors.HasCode(err, agent.ErrProviderConfig) {
		t.Fatalf("none: %v", err)
	}
	if _, err := New([]agent.Provider{a, nil}, Config{}); !errors.HasCode(err, agent.ErrProviderConfig) {
		t.Fatalf("nil: %v", err)
	}
	if _, err := New([]agent.Provider{nil}, Config{}); !errors.HasCode(err, agent.ErrProviderConfig) {
		t.Fatalf("single nil: %v", err)
	}
}

// A provider down hands over at once; it is passed over while it cools
// down, tried again after, and the run goes back to it.
func TestDownHandsOverAndComesBack(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var switches []Switch
	a := named("a", fake.Fail(down), fake.Reply("a back"))
	b := named("bb", fake.Reply("b1"), fake.Reply("b2"))
	p := mustNew(t, []agent.Provider{a, b}, Config{Now: clk.now, OnSwitch: func(s Switch) { switches = append(switches, s) }})
	if got, err := call(p); err != nil || got != "b1" {
		t.Fatalf("first: %q %v", got, err)
	}
	if p.Info().Name != "bb" || p.Info().ContextWindow != 2 {
		t.Fatalf("info while a cools down: %+v", p.Info())
	}
	if got, _ := call(p); got != "b2" || a.Remaining() != 1 {
		t.Fatalf("a not passed over: %q, %d left", got, a.Remaining())
	}
	clk.t = clk.t.Add(DefaultCooldown)
	if got, _ := call(p); got != "a back" {
		t.Fatalf("not back to a: %q", got)
	}
	checkSwitches(t, switches)
}

// checkSwitches: away from a because it was down, then back to it.
func checkSwitches(t *testing.T, s []Switch) {
	t.Helper()
	if len(s) != 2 {
		t.Fatalf("switches %+v", s)
	}
	away, back := s[0], s[1]
	if away.From != "a" || away.To != "bb" || !errors.HasCode(away.Err, agent.ErrProviderUnavailable) {
		t.Errorf("away %+v", away)
	}
	if back.From != "bb" || back.To != "a" || back.Err != nil {
		t.Errorf("back %+v", back)
	}
}

// A request the provider refuses is the request's fault: no other model
// gets it.
func TestRequestErrorsStay(t *testing.T) {
	for _, code := range []errors.ErrorCode{agent.ErrProviderInput, agent.ErrProviderContext, agent.ErrProviderConfig} {
		b := named("b", fake.Reply("never"))
		p := mustNew(t, []agent.Provider{named("a", fake.Fail(errors.New(code, "x"))), b}, Config{})
		if _, err := call(p); !errors.HasCode(err, code) || b.Remaining() != 1 {
			t.Errorf("%s handed over", code)
		}
	}
}

func TestEveryKindOfNoAnswerHandsOver(t *testing.T) {
	for _, err := range []error{
		errors.New(agent.ErrProviderRate, "slow").AsRetryable(),
		errors.New(agent.ErrProviderInterrupted, "cut").AsRetryable(),
		errors.New(agent.ErrProviderResponse, "garbled"),
		errors.New(agent.ErrProviderAuth, "bad key"),
		errors.New("CIRCUIT_OPEN", "open").AsRetryable(),
	} {
		p := mustNew(t, []agent.Provider{named("a", fake.Fail(err)), named("b", fake.Reply("b"))}, Config{})
		if got, e := call(p); got != "b" {
			t.Errorf("%v: %q %v", err, got, e)
		}
	}
}

// Every provider cooling down: they are tried anyway, the soonest back
// first; when all fail the last error comes back.
func TestAllDown(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	a := named("a", fake.Fail(down), fake.Fail(down), fake.Reply("a"))
	b := named("b", fake.Fail(down), fake.Reply("b"))
	p := mustNew(t, []agent.Provider{a, b}, Config{Now: clk.now})
	if _, err := call(p); !errors.HasCode(err, agent.ErrProviderUnavailable) {
		t.Fatalf("all down: %v", err)
	}
	// a failed first, so it is back first; it fails again, b answers.
	if got, err := call(p); got != "b" || err != nil || a.Remaining() != 1 {
		t.Fatalf("second: %q %v, a %d left", got, err, a.Remaining())
	}
}

// Failures in a row double the cooldown, up to the cap.
func TestCooldownDoubles(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	a := named("a", fake.Fail(down), fake.Fail(down), fake.Reply("a"))
	b := named("b", fake.Reply("b"), fake.Reply("b"), fake.Reply("b"), fake.Reply("b"))
	p := mustNew(t, []agent.Provider{a, b}, Config{Now: clk.now, Cooldown: time.Minute, MaxCooldown: 3 * time.Minute})
	_, _ = call(p) // a fails: 1 minute
	clk.t = clk.t.Add(time.Minute)
	_, _ = call(p) // a fails again: 2 minutes
	clk.t = clk.t.Add(time.Minute + 59*time.Second)
	if got, _ := call(p); got != "b" {
		t.Fatalf("a tried before its 2 minutes: %q", got)
	}
	clk.t = clk.t.Add(time.Second)
	if got, _ := call(p); got != "a" {
		t.Fatalf("a not tried after 2 minutes: %q", got)
	}
}

// Text already shown is never shown twice: without OnRestart the error
// comes back, with it the host is told before the next model speaks.
func TestStreamedTextIsNotRepeated(t *testing.T) {
	cut := fake.Step{Chunks: []string{"half"}, Err: errors.New(agent.ErrProviderInterrupted, "cut").AsRetryable()}
	var shown []string
	sink := func(d string) error { shown = append(shown, d); return nil }

	p := mustNew(t, []agent.Provider{named("a", cut), named("b", fake.Stream("whole"))}, Config{})
	if _, err := p.Generate(context.Background(), &agent.Request{}, sink); !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatalf("handed over without OnRestart: %v", err)
	}

	shown, restarts := nil, 0
	p = mustNew(t, []agent.Provider{named("a", cut), named("b", fake.Stream("whole"))}, Config{OnRestart: func() { restarts++; shown = nil }})
	if _, err := p.Generate(context.Background(), &agent.Request{}, sink); err != nil || restarts != 1 || len(shown) != 1 || shown[0] != "whole" {
		t.Fatalf("restart: %v, %d restarts, shown %q", err, restarts, shown)
	}
}

func TestCancelledStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := named("b", fake.Reply("never"))
	p := mustNew(t, []agent.Provider{named("a", fake.Fail(down)), b}, Config{})
	if _, err := p.Generate(ctx, &agent.Request{}, nil); err == nil || b.Remaining() != 1 {
		t.Fatalf("cancelled call handed over: %v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	steps := make([]fake.Step, 0, 64)
	for range 32 {
		steps = append(steps, fake.Fail(down), fake.Reply("a"))
	}
	more := make([]fake.Step, 64)
	for i := range more {
		more[i] = fake.Reply("b")
	}
	p := mustNew(t, []agent.Provider{named("a", steps...), named("b", more...)}, Config{OnSwitch: func(Switch) {}})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { _, _ = call(p) })
	}
	wg.Wait()
}

// The switch is told before the next model speaks, and once for each
// model left.
func TestSwitchToldBeforeTheNextModel(t *testing.T) {
	var log []string
	sink := func(d string) error { log = append(log, "text "+d); return nil }
	p := mustNew(t, []agent.Provider{named("a", fake.Fail(down)), named("b", fake.Fail(down)), named("c", fake.Stream("hi"))},
		Config{OnSwitch: func(s Switch) { log = append(log, s.From+">"+s.To) }})
	if _, err := p.Generate(context.Background(), &agent.Request{}, sink); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "a>b,b>c,text hi" {
		t.Fatalf("order %s", got)
	}
}

// Audit 25/11: the main model rate limited, the stand-in refusing the
// request (too small, a broken key): the error returned is the main
// model's, with its wait, so the retry waits it out as with no fallback.
func TestStandInRefusalKeepsTheWait(t *testing.T) {
	slow := agent.WithRetryAfter(errors.New(agent.ErrProviderRate, "slow").AsRetryable(), 20*time.Second)
	for _, standIn := range []error{
		errors.New(agent.ErrProviderContext, "too long for me"),
		errors.New(agent.ErrProviderInput, "no tools here"),
		errors.New(agent.ErrProviderAuth, "bad key"),
	} {
		p := mustNew(t, []agent.Provider{named("a", fake.Fail(slow)), named("b", fake.Fail(standIn))}, Config{})
		_, err := call(p)
		if wait, ok := agent.RetryAfter(err); !errors.HasCode(err, agent.ErrProviderRate) || !ok || wait != 20*time.Second {
			t.Errorf("stand-in %v: got %v", standIn, err)
		}
	}
}

// Workers failing together in one blip are one failure: the cooldown
// does not climb to the cap.
func TestParallelFailuresAreOneOutage(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	p := mustNew(t, []agent.Provider{named("a"), named("b")}, Config{Now: clk.now})
	c := p.(*chain)
	for range 8 { // eight workers' calls failing in the same moment
		c.failed(0)
	}
	if c.links[0].failures != 1 || !c.links[0].until.Equal(clk.t.Add(DefaultCooldown)) {
		t.Fatalf("one outage counted as %+v", c.links[0])
	}
}

func TestNewClonesProvidersSlice(t *testing.T) {
	ps := []agent.Provider{named("a", fake.Reply("a")), named("b")}
	p := mustNew(t, ps, Config{})
	ps[0], ps[1] = nil, nil
	if got, err := call(p); err != nil || got != "a" {
		t.Fatalf("caller slice mutation broke chain: got=%q err=%v", got, err)
	}
}

func TestLastStreamFailureSkipsOnRestartAndCoolsDown(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	cut := fake.Step{Chunks: []string{"half"}, Err: errors.New(agent.ErrProviderInterrupted, "cut").AsRetryable()}
	restarts := 0
	p := mustNew(t, []agent.Provider{named("a", fake.Fail(down)), named("b", cut)}, Config{
		Now: clk.now, OnRestart: func() { restarts++ },
	})
	if _, err := p.Generate(context.Background(), &agent.Request{}, func(string) error { return nil }); err == nil || restarts != 0 {
		t.Fatalf("last provider called OnRestart: err=%v restarts=%d", err, restarts)
	}
	p = mustNew(t, []agent.Provider{named("a", cut), named("b", fake.Reply("b"))}, Config{Now: clk.now})
	if _, err := p.Generate(context.Background(), &agent.Request{}, func(string) error { return nil }); !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatalf("want ErrProviderInterrupted, got %v", err)
	}
	if got, err := call(p); err != nil || got != "b" {
		t.Fatalf("broken provider did not enter cooldown: got=%q err=%v", got, err)
	}
}

func TestSinkErrorAbortsAndCooldownNoOverflow(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	sinkErr := errors.New(agent.ErrProviderRate, "sink stop").AsRetryable()
	restarts := 0
	b := named("b", fake.Reply("never"))
	p := mustNew(t, []agent.Provider{named("a", fake.Stream("hi")), b}, Config{
		Now: clk.now, OnRestart: func() { restarts++ },
	})
	_, err := p.Generate(context.Background(), &agent.Request{}, func(string) error { return sinkErr })
	if err != sinkErr || b.Remaining() != 1 || restarts != 0 {
		t.Fatalf("sink error mishandled: err=%v b=%d restarts=%d", err, b.Remaining(), restarts)
	}
	c := mustNew(t, []agent.Provider{named("a"), named("b")}, Config{
		Now: clk.now, Cooldown: time.Hour, MaxCooldown: math.MaxInt64,
	}).(*chain)
	for range 64 {
		c.failed(0)
		if !c.links[0].until.After(clk.t) {
			t.Fatalf("cooldown overflowed: until=%v now=%v", c.links[0].until, clk.t)
		}
		clk.t = c.links[0].until
	}
}
