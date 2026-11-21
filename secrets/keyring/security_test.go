// security_test.go: what a keyring, a model or the user could do
//
// THREAT MODEL:
//   - A backend's error repeats what it was given (the value, the name):
//     no error carries the backend's text.
//   - A keyring that waits for the user to unlock it, or never answers:
//     the call ends with its context; calls keep their order, so a save
//     left waiting cannot land after a later one; a call given up before
//     it started never runs; the queue is bounded.
//   - A malformed answer of the keyring service (a forged D-Bus signal)
//     that would make a backend panic: a failure, not a crash.
//   - Names and values from configuration or from a model: checked as
//     the secrets package checks them, before the keyring sees them.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

func TestSecurityErrorsHoldNoBackendText(t *testing.T) {
	m := newMemory()
	m.fail = errEchoing
	k := withBackend(m)
	ctx := context.Background()
	_, _, lookupErr := k.Lookup(ctx, "KEY")
	for _, err := range []error{lookupErr, k.Set(ctx, "KEY", "sk-live-SECRETVALUE"), k.Delete(ctx, "KEY")} {
		if !errors.HasCode(err, ErrUnavailable) || strings.Contains(err.Error(), "SECRETVALUE") || strings.Contains(err.Error(), "dbus") {
			t.Errorf("%v", err)
		}
	}
	// Through secrets.Lookup too: a failing source is a lookup error.
	if _, _, err := secrets.Lookup(ctx, k, "KEY"); !errors.HasCode(err, secrets.ErrLookup) {
		t.Fatalf("%v", err)
	}
}

func TestSecurityAHangingKeyringEndsWithTheContext(t *testing.T) {
	m := newMemory()
	m.hang = make(chan struct{})
	t.Cleanup(func() { close(m.hang) })
	k := withBackend(m)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := k.Lookup(ctx, "KEY"); err != context.DeadlineExceeded || time.Since(start) > 2*time.Second {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
	if err := k.Set(ctx, "KEY", "v"); err != context.DeadlineExceeded {
		t.Fatalf("%v", err)
	}
}

func TestSecurityNamesAndValuesAreChecked(t *testing.T) {
	m := newMemory()
	k := withBackend(m)
	ctx := context.Background()
	for _, name := range []string{"", "1KEY", "KEY-X", "../KEY", "KEY\x00"} {
		if err := k.Set(ctx, name, "v"); !errors.HasCode(err, secrets.ErrName) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, value := range []string{"a\x00b", "\xff", strings.Repeat("x", 64<<10+1)} {
		if err := k.Set(ctx, "KEY", value); !errors.HasCode(err, secrets.ErrValue) {
			t.Errorf("value of %d bytes: %v", len(value), err)
		}
	}
	if len(m.entries) != 0 {
		t.Fatalf("the keyring got %d entries", len(m.entries))
	}
}

func FuzzNames(f *testing.F) {
	for _, s := range []string{"KEY", "", "a b", "\xff", "OPENAI_API_KEY"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		k := withBackend(newMemory())
		err := k.Set(context.Background(), name, "v")
		if (secrets.ValidateName(name) == nil) != (err == nil) {
			t.Fatalf("%q: %v", name, err)
		}
	})
}

// Audit 21/11: a save that timed out could land after a later one, once
// the user unlocked the keyring. Calls run in order: the late one first.
func TestSecurityCallsKeepTheirOrder(t *testing.T) {
	m := newMemory()
	m.hang = make(chan struct{})
	k := withBackend(m)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := k.Set(ctx, "KEY", "old"); !errors.HasCode(err, ErrUnavailable) || !strings.Contains(err.Error(), "may still be applied") {
		t.Fatalf("a save left running: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- k.Set(context.Background(), "KEY", "new") }()
	time.Sleep(20 * time.Millisecond)
	close(m.hang)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v, _, _ := k.Lookup(context.Background(), "KEY"); v != "new" {
		t.Fatalf("the late save won: %q, calls %v", v, m.calls)
	}
}

// A call given up while it waited behind another never runs.
func TestSecurityAGivenUpCallNeverRuns(t *testing.T) {
	m := newMemory()
	m.hang = make(chan struct{})
	k := withBackend(m)
	go func() { _, _, _ = k.Lookup(context.Background(), "FIRST") }()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := k.Delete(ctx, "KEY"); err != context.DeadlineExceeded {
		t.Fatalf("%v", err)
	}
	close(m.hang)
	if _, _, err := k.Lookup(context.Background(), "LAST"); err != nil {
		t.Fatal(err)
	}
	for _, c := range m.calls {
		if c == "del KEY" {
			t.Fatalf("the given-up delete ran: %v", m.calls)
		}
	}
}

func TestSecurityTheQueueIsBounded(t *testing.T) {
	m := newMemory()
	m.hang = make(chan struct{})
	defer close(m.hang)
	k := withBackend(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range maxQueued + 1 { // one running, the rest waiting
		go func() { _, _, _ = k.Lookup(ctx, "KEY") }()
	}
	time.Sleep(50 * time.Millisecond)
	short, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, _, err := k.Lookup(short, "KEY"); !errors.HasCode(err, ErrUnavailable) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("%v", err)
	}
}

func TestSecurityAPanickingBackendIsAFailure(t *testing.T) {
	m := newMemory()
	m.panics = true
	if _, _, err := withBackend(m).Lookup(context.Background(), "KEY"); !errors.HasCode(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}
