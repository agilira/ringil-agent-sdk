// budget_test.go: tests for the shared budget
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package budget

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func mustNew(t *testing.T, limits Limits) Budget {
	t.Helper()
	b, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func call(p agent.Provider) error {
	_, err := p.Generate(context.Background(), &agent.Request{}, nil)
	return err
}

func used(input, output int) fake.Step {
	return fake.Step{Chunks: []string{"x"}, Usage: agent.Usage{Input: input, Output: output, CacheRead: 1}}
}

func TestNewRejectsNegative(t *testing.T) {
	for _, l := range []Limits{{Calls: -1}, {Tokens: -1}} {
		if _, err := New(l); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("%+v accepted: %v", l, err)
		}
	}
}

func TestUnlimitedByDefault(t *testing.T) {
	b := mustNew(t, Limits{})
	p := b.Wrap(fake.New(used(1000, 1000), used(1000, 1000), used(1000, 1000)))
	for range 3 {
		if err := call(p); err != nil {
			t.Fatal(err)
		}
	}
	if s := b.Spent(); s.Calls != 3 || s.Tokens() != 6000 || s.CacheRead != 3 {
		t.Fatalf("spent %+v", s)
	}
}

func TestCallCapSharedAcrossProviders(t *testing.T) {
	b := mustNew(t, Limits{Calls: 2})
	main, worker := b.Wrap(fake.New(used(1, 1), used(1, 1))), b.Wrap(fake.New(used(1, 1)))
	if err := call(main); err != nil {
		t.Fatal(err)
	}
	if err := call(worker); err != nil {
		t.Fatal(err)
	}
	if err := call(main); !errors.HasCode(err, ErrCalls) {
		t.Fatalf("third call: %v", err)
	}
	if b.Spent().Calls != 2 {
		t.Fatalf("refused call was counted: %+v", b.Spent())
	}
}

func TestFailedCallStillCounts(t *testing.T) {
	b := mustNew(t, Limits{Calls: 1})
	p := b.Wrap(fake.New(fake.Fail(stderrors.New("503")), used(1, 1)))
	if err := call(p); err == nil {
		t.Fatal("failure swallowed")
	}
	if err := call(p); !errors.HasCode(err, ErrCalls) {
		t.Fatalf("failed call was refunded: %v", err)
	}
	if s := b.Spent(); s.Unreported != 1 {
		t.Fatalf("spent %+v", s)
	}
}

func TestTokenCapStopsTheNextCall(t *testing.T) {
	b := mustNew(t, Limits{Tokens: 100})
	p := b.Wrap(fake.New(used(60, 10), used(50, 10), used(1, 1)))
	if err := call(p); err != nil {
		t.Fatal(err)
	}
	if err := call(p); err != nil {
		t.Fatalf("the call that crosses the cap must be delivered: %v", err)
	}
	if err := call(p); !errors.HasCode(err, ErrTokens) {
		t.Fatalf("call after the cap: %v", err)
	}
}

func TestUnreportedUsage(t *testing.T) {
	b := mustNew(t, Limits{})
	if err := call(b.Wrap(fake.New(fake.Reply("no usage")))); err != nil {
		t.Fatal(err)
	}
	if s := b.Spent(); s.Unreported != 1 || s.Tokens() != 0 {
		t.Fatalf("spent %+v", s)
	}
}

func TestNegativeUsageIgnored(t *testing.T) {
	b := mustNew(t, Limits{})
	step := fake.Step{Usage: agent.Usage{Input: -5, Output: 3, CacheRead: -1, CacheWrite: 2}}
	if err := call(b.Wrap(fake.New(step))); err != nil {
		t.Fatal(err)
	}
	if s := b.Spent(); s.Input != 0 || s.Output != 3 || s.CacheRead != 0 || s.CacheWrite != 2 {
		t.Fatalf("spent %+v", s)
	}
}

func TestCancelledContextSpendsNothing(t *testing.T) {
	b := mustNew(t, Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Wrap(fake.New(used(1, 1))).Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if b.Spent().Calls != 0 {
		t.Fatal("cancelled call was counted")
	}
}

func TestInfoAndLimits(t *testing.T) {
	b := mustNew(t, Limits{Calls: 3, Tokens: 9})
	if b.Limits() != (Limits{Calls: 3, Tokens: 9}) {
		t.Fatal("Limits")
	}
	if b.Wrap(fake.NewWithInfo(agent.ModelInfo{Name: "eos"})).Info().Name != "eos" {
		t.Fatal("Info not forwarded")
	}
}

func TestConcurrentCallsNeverExceedCap(t *testing.T) {
	const limit, callers = 10, 50
	b := mustNew(t, Limits{Calls: limit})
	steps := make([]fake.Step, callers)
	for i := range steps {
		steps[i] = used(1, 1)
	}
	p := b.Wrap(fake.New(steps...))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if call(p) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != limit || b.Spent().Calls != limit {
		t.Fatalf("%d calls succeeded, %d counted, cap %d", ok, b.Spent().Calls, limit)
	}
}
