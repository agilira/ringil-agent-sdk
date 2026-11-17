// ratelimit_test.go: tests for the rate limiter
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package ratelimit

import (
	"context"
	stderrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func mustNew(t *testing.T, cfg Config) Limiter {
	t.Helper()
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func replies(n int) []fake.Step {
	steps := make([]fake.Step, n)
	for i := range steps {
		steps[i] = fake.Reply("ok")
	}
	return steps
}

func TestNewRejectsNegative(t *testing.T) {
	for _, cfg := range []Config{{RequestsPerMinute: -1}, {Burst: -1}, {MaxConcurrent: -1}} {
		if _, err := New(cfg); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("%+v accepted: %v", cfg, err)
		}
	}
}

func TestZeroConfigDoesNotLimit(t *testing.T) {
	p := mustNew(t, Config{}).Wrap(fake.New(replies(100)...))
	start := time.Now()
	for range 100 {
		if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("unlimited limiter waited")
	}
}

func TestRatePacesCalls(t *testing.T) {
	// 1200 per minute = one every 50ms, burst 1: three calls take >= 100ms.
	p := mustNew(t, Config{RequestsPerMinute: 1200}).Wrap(fake.New(replies(3)...))
	start := time.Now()
	for range 3 {
		if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("three calls took %v; rate not applied", elapsed)
	}
}

func TestDeadlineShorterThanWaitIsRateError(t *testing.T) {
	inner := fake.New(replies(2)...)
	p := mustNew(t, Config{RequestsPerMinute: 1}).Wrap(inner)
	if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Generate(ctx, &agent.Request{}, nil); !errors.HasCode(err, agent.ErrProviderRate) {
		t.Fatalf("err %v", err)
	}
	if inner.Remaining() != 1 {
		t.Fatal("a refused call reached the provider")
	}
}

func TestCancelWhileWaitingForRate(t *testing.T) {
	p := mustNew(t, Config{RequestsPerMinute: 1}).Wrap(fake.New(replies(2)...))
	if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := p.Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// slow blocks every call until released and records the peak concurrency.
type slow struct {
	agent.Provider
	release      chan struct{}
	now, highest atomic.Int32
}

func (s *slow) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	n := s.now.Add(1)
	for {
		h := s.highest.Load()
		if n <= h || s.highest.CompareAndSwap(h, n) {
			break
		}
	}
	<-s.release
	s.now.Add(-1)
	return &agent.Response{Message: agent.AssistantText("ok")}, nil
}

// waitFor polls cond until it holds, failing the test after five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMaxConcurrent(t *testing.T) {
	inner := &slow{Provider: fake.New(), release: make(chan struct{})}
	p := mustNew(t, Config{MaxConcurrent: 2}).Wrap(inner)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	waitFor(t, func() bool { return inner.now.Load() == 2 })
	for range 6 {
		inner.release <- struct{}{}
	}
	wg.Wait()
	if inner.highest.Load() != 2 {
		t.Fatalf("peak concurrency %d, want 2", inner.highest.Load())
	}
}

func TestCancelWhileWaitingForSlot(t *testing.T) {
	inner := &slow{Provider: fake.New(), release: make(chan struct{})}
	p := mustNew(t, Config{MaxConcurrent: 1}).Wrap(inner)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, func() bool { return inner.now.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	inner.release <- struct{}{}
	<-done
}

func TestRateFailureReleasesSlot(t *testing.T) {
	p := mustNew(t, Config{RequestsPerMinute: 1, MaxConcurrent: 1}).Wrap(fake.New(replies(1)...))
	if _, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Generate(ctx, &agent.Request{}, nil); !errors.HasCode(err, agent.ErrProviderRate) {
		t.Fatalf("err %v", err)
	}
	l := p.(*provider).limiter
	if len(l.slots) != 0 {
		t.Fatal("a refused call kept its slot")
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := mustNew(t, Config{}).Wrap(fake.New(replies(1)...))
	if _, err := p.Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestInfoForwarded(t *testing.T) {
	if mustNew(t, Config{}).Wrap(fake.NewWithInfo(agent.ModelInfo{Name: "eos"})).Info().Name != "eos" {
		t.Fatal("Info not forwarded")
	}
}
