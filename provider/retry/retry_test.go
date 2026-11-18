// retry_test.go: tests for the retry decorator
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package retry

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func transient() error {
	return errors.New(agent.ErrProviderUnavailable, "503").AsRetryable()
}

// recorder replaces the real sleep and records every wait.
type recorder struct{ waits []time.Duration }

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.waits = append(r.waits, d)
	return nil
}

func wrap(p agent.Provider, cfg Config) (*provider, *recorder) {
	rec := &recorder{}
	w := Wrap(p, cfg).(*provider)
	w.sleep = rec.sleep
	return w, rec
}

func call(p agent.Provider, sink agent.TextSink) (*agent.Response, error) {
	return p.Generate(context.Background(), &agent.Request{}, sink)
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	inner := fake.New(fake.Fail(transient()), fake.Fail(transient()), fake.Reply("ok"))
	p, rec := wrap(inner, Config{})
	resp, err := call(p, nil)
	if err != nil || resp.Message.Text() != "ok" || len(rec.waits) != 2 {
		t.Fatalf("resp %+v err %v waits %v", resp, err, rec.waits)
	}
}

func TestStopsAtMaxAttempts(t *testing.T) {
	inner := fake.New(fake.Fail(transient()), fake.Fail(transient()), fake.Fail(transient()), fake.Reply("late"))
	p, rec := wrap(inner, Config{MaxAttempts: 3})
	if _, err := call(p, nil); !errors.HasCode(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err %v", err)
	}
	if len(rec.waits) != 2 || inner.Remaining() != 1 {
		t.Fatalf("waits %v remaining %d", rec.waits, inner.Remaining())
	}
}

func TestNonRetryableIsReturnedAtOnce(t *testing.T) {
	for _, err := range []error{
		errors.New(agent.ErrProviderAuth, "401"),
		errors.New(agent.ErrProviderRate, "quota exhausted"),
		stderrors.New("plain"),
		context.Canceled,
	} {
		inner := fake.New(fake.Fail(err), fake.Reply("never"))
		p, rec := wrap(inner, Config{})
		if _, got := call(p, nil); !stderrors.Is(got, err) || len(rec.waits) != 0 {
			t.Errorf("%v: got %v after %v", err, got, rec.waits)
		}
	}
}

func TestHonoursRetryAfter(t *testing.T) {
	rate := agent.WithRetryAfter(errors.New(agent.ErrProviderRate, "429"), 9*time.Second).AsRetryable()
	p, rec := wrap(fake.New(fake.Fail(rate), fake.Reply("ok")), Config{})
	if _, err := call(p, nil); err != nil || len(rec.waits) != 1 || rec.waits[0] != 9*time.Second {
		t.Fatalf("err %v waits %v", err, rec.waits)
	}
}

func TestRefusesExcessiveRetryAfter(t *testing.T) {
	rate := agent.WithRetryAfter(errors.New(agent.ErrProviderRate, "429"), time.Hour).AsRetryable()
	p, rec := wrap(fake.New(fake.Fail(rate), fake.Reply("ok")), Config{})
	if _, err := call(p, nil); !errors.HasCode(err, agent.ErrProviderRate) || len(rec.waits) != 0 {
		t.Fatalf("err %v waits %v", err, rec.waits)
	}
}

func TestStreamedAttemptIsNotRetriedWithoutOnRestart(t *testing.T) {
	cut := fake.Step{Chunks: []string{"mezza"}, Err: transient()}
	inner := fake.New(cut, fake.Reply("ok"))
	p, _ := wrap(inner, Config{})
	if _, err := call(p, func(string) error { return nil }); !errors.HasCode(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err %v", err)
	}
	if inner.Remaining() != 1 {
		t.Fatal("a streamed attempt was retried without OnRestart")
	}
}

func TestStreamedAttemptRetriedWithOnRestart(t *testing.T) {
	cut := fake.Step{Chunks: []string{"mezza"}, Err: transient()}
	restarts := 0
	var shown []string
	p, _ := wrap(fake.New(cut, fake.Reply("intera")), Config{OnRestart: func() { restarts++; shown = nil }})
	resp, err := call(p, func(d string) error { shown = append(shown, d); return nil })
	if err != nil || restarts != 1 || len(shown) != 1 || shown[0] != "intera" || resp.Message.Text() != "intera" {
		t.Fatalf("err %v restarts %d shown %v", err, restarts, shown)
	}
}

func TestSilentFailureBeforeTextIsRetriedWithoutOnRestart(t *testing.T) {
	p, _ := wrap(fake.New(fake.Fail(transient()), fake.Reply("ok")), Config{})
	if _, err := call(p, func(string) error { return nil }); err != nil {
		t.Fatalf("err %v", err)
	}
}

// Cancelled while waiting to retry, or during an attempt (macOS CI, 18/11:
// the attempt's 503 came back instead of the cancellation): the caller
// is told it was cancelled.
func TestCancelledWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := Wrap(fake.New(fake.Fail(transient()), fake.Reply("ok")), Config{BaseDelay: time.Hour})
	go cancel()
	if _, err := p.Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestCancelledDuringAnAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelling := cancelOnCall{fake.New(fake.Fail(transient())), cancel}
	if _, err := Wrap(cancelling, Config{}).Generate(ctx, &agent.Request{}, nil); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// cancelOnCall cancels the context while its attempt runs.
type cancelOnCall struct {
	agent.Provider
	cancel context.CancelFunc
}

func (c cancelOnCall) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	c.cancel()
	return c.Provider.Generate(context.WithoutCancel(ctx), req, sink)
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	p, _ := wrap(fake.New(), Config{BaseDelay: time.Second, MaxDelay: 8 * time.Second})
	bounds := [][2]time.Duration{{500 * time.Millisecond, time.Second}, {time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second}, {4 * time.Second, 8 * time.Second}, {4 * time.Second, 8 * time.Second}}
	for i, b := range bounds {
		for range 50 {
			if d := p.backoff(i + 1); d < b[0] || d > b[1] {
				t.Fatalf("attempt %d backoff %v outside %v", i+1, d, b)
			}
		}
	}
}

func TestDefaultsAndInfo(t *testing.T) {
	p, _ := wrap(fake.NewWithInfo(agent.ModelInfo{Name: "eos"}), Config{MaxAttempts: -1})
	if p.cfg.MaxAttempts != DefaultMaxAttempts || p.cfg.BaseDelay != DefaultBaseDelay ||
		p.cfg.MaxDelay != DefaultMaxDelay || p.cfg.MaxRetryAfter != DefaultMaxRetryAfter {
		t.Fatalf("defaults %+v", p.cfg)
	}
	if p.Info().Name != "eos" {
		t.Fatal("Info not forwarded")
	}
}

func TestRealSleep(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
