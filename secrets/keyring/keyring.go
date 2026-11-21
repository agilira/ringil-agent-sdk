// keyring.go: secrets kept in the operating system's keyring
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package keyring is a [secrets.Store] on the operating system's keyring:
// the macOS Keychain, the Windows Credential Manager, the Secret Service
// of Linux desktops (GNOME Keyring, KWallet). Each secret is an entry of
// one service (the application's name) under the secret's name, so the
// user sees and removes them with the system's own tools.
//
// Calls run one at a time, in the order they were made: a save the user
// has not unlocked yet cannot land after a later one. The keyring may ask
// the user to unlock it: a call waits until its context ends; one that
// had started by then may still be applied, and says so.
//
// A machine without a keyring (a server with no desktop session) answers
// every call with ErrUnavailable. In a [secrets.Chain] a failing source
// ends the lookup, so on such a machine give the chain another source
// instead of the keyring.
package keyring

import (
	"context"
	stderrors "errors"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

// ErrUnavailable: there is no keyring to use, or it refused the call.
const ErrUnavailable errors.ErrorCode = "SECRET_KEYRING"

// maxQueued bounds the calls waiting for the keyring (a locked one waits
// for the user): past it a call fails at once.
const maxQueued = 16

// Errors of the backends; their own texts never leave the package.
var (
	errNoKeyring = stderrors.New("no keyring")
	errTooBig    = stderrors.New("value too long for this keyring")
	errRefused   = stderrors.New("the user did not unlock the keyring")
	errCaseClash = stderrors.New("another entry differs only in case")
)

// backend is a system keyring: one per platform, a fake in tests.
type backend interface {
	get(service, name string) (value string, found bool, err error)
	set(service, name, value string) error
	del(service, name string) error // an absent entry is no error
}

// Keyring keeps secrets under one service name.
type Keyring struct {
	service string
	sys     backend

	mu      sync.Mutex
	queue   []*op
	running bool
}

// op is one call in the queue.
type op struct {
	run   func() error
	state atomic.Int32
	done  chan error
}

const (
	queued int32 = iota
	started
	dropped
)

var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// New is the keyring's entries of service, such as "ringil": starts
// with a letter or digit, followed by letters, digits, '.', '_' and
// '-', at most 64 in all.
func New(service string) (*Keyring, error) {
	if !serviceName.MatchString(service) {
		return nil, errors.New(secrets.ErrSource, "a keyring service name starts with a letter or digit and holds 1 to 64 letters, digits, '.', '_' or '-'")
	}
	return &Keyring{service: service, sys: system()}, nil
}

// Lookup implements [secrets.Resolver]: an absent entry is not found.
func (k *Keyring) Lookup(ctx context.Context, name string) (string, bool, error) {
	if err := secrets.ValidateName(name); err != nil {
		return "", false, err
	}
	var value string
	var found bool
	err := k.do(ctx, func() (err error) {
		value, found, err = k.sys.get(k.service, name)
		return err
	})
	if err == errLate {
		err = ctx.Err() // a read left running changes nothing
	}
	if err != nil {
		return "", false, k.failed(err, "read", name)
	}
	return value, found, nil
}

// Set implements [secrets.Store]: it adds or replaces the entry.
func (k *Keyring) Set(ctx context.Context, name, value string) error {
	if err := secrets.ValidateName(name); err != nil {
		return err
	}
	if err := secrets.ValidateValue(name, value); err != nil {
		return err
	}
	if err := k.do(ctx, func() error { return k.sys.set(k.service, name, value) }); err != nil {
		return k.failed(err, "save", name)
	}
	return nil
}

// Delete implements [secrets.Store]: an absent entry is no error.
func (k *Keyring) Delete(ctx context.Context, name string) error {
	if err := secrets.ValidateName(name); err != nil {
		return err
	}
	if err := k.do(ctx, func() error { return k.sys.del(k.service, name) }); err != nil {
		return k.failed(err, "delete", name)
	}
	return nil
}

var (
	errBusy = errors.New(ErrUnavailable, "the keyring is busy: too many calls wait for it")
	errLate = errors.New(ErrUnavailable, "the keyring did not answer in time; the call may still be applied once it is unlocked")
)

// do queues run and waits for it until ctx ends. A call whose context
// ended before it started never runs; one already running goes on, and
// the caller is told it may still be applied.
func (k *Keyring) do(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o := &op{run: run, done: make(chan error, 1)}
	k.mu.Lock()
	if len(k.queue) >= maxQueued {
		k.mu.Unlock()
		return errBusy
	}
	k.queue = append(k.queue, o)
	if !k.running {
		k.running = true
		go k.work()
	}
	k.mu.Unlock()
	select {
	case err := <-o.done:
		return err
	case <-ctx.Done():
	}
	if o.state.CompareAndSwap(queued, dropped) {
		return ctx.Err()
	}
	select {
	case err := <-o.done: // it ended as the context did: its answer stands
		return err
	default:
		return errLate
	}
}

// work runs the queue in order, until it is empty.
func (k *Keyring) work() {
	for {
		k.mu.Lock()
		if len(k.queue) == 0 {
			k.queue = nil
			k.running = false
			k.mu.Unlock()
			return
		}
		o := k.queue[0]
		k.queue[0] = nil // release popped closure (which may hold a secret) for GC
		k.queue = k.queue[1:]
		k.mu.Unlock()
		if !o.state.CompareAndSwap(queued, started) {
			continue // its caller gave up before it started
		}
		o.done <- safely(o.run)
	}
}

// safely runs call; a panic in a backend (a malformed answer of the
// keyring service) is a failure, not a crash.
func safely(call func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errNoKeyring
		}
	}()
	return call()
}

// failed is the error for a call that failed: the context's, one of
// ours, or a code without the backend's text, which may echo a value.
func (k *Keyring) failed(err error, what, name string) error {
	switch {
	case stderrors.Is(err, context.Canceled), stderrors.Is(err, context.DeadlineExceeded):
		return err
	case errors.HasCode(err, ErrUnavailable):
		return err
	case stderrors.Is(err, errTooBig):
		return errors.New(secrets.ErrValue, "secret "+name+" is too long for this keyring")
	case stderrors.Is(err, errCaseClash):
		return errors.New(secrets.ErrValue, "secret "+name+": this keyring ignores case, and another of "+k.service+" differs from it only in case")
	case stderrors.Is(err, errRefused):
		return errors.New(ErrUnavailable, "the keyring stayed locked: "+name+" was not "+past(what))
	}
	return errors.New(ErrUnavailable, "the keyring could not "+what+" the secret "+name+" of "+k.service)
}

func past(what string) string {
	switch what {
	case "save":
		return "saved"
	case "delete":
		return "deleted"
	}
	return "read"
}
