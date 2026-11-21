// fake_test.go: a keyring in memory, and one that fails or hangs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	stderrors "errors"
	"sync"
)

type memory struct {
	mu      sync.Mutex
	entries map[string]string
	fail    error         // returned by every call when set
	hang    chan struct{} // calls wait on it when set
	max     int           // set refuses longer values when > 0
	panics  bool
	calls   []string // what ran, in order
}

func newMemory() *memory { return &memory{entries: map[string]string{}} }

func (m *memory) before(what string) error {
	if m.hang != nil {
		<-m.hang
	}
	if m.panics {
		panic("malformed signal")
	}
	m.mu.Lock()
	m.calls = append(m.calls, what)
	m.mu.Unlock()
	return m.fail
}

func (m *memory) get(service, name string) (string, bool, error) {
	if err := m.before("get " + name); err != nil {
		return "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.entries[service+"\x00"+name]
	return v, ok, nil
}

func (m *memory) set(service, name, value string) error {
	if err := m.before("set " + name + "=" + value); err != nil {
		return err
	}
	if m.max > 0 && len(value) > m.max {
		return errTooBig
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[service+"\x00"+name] = value
	return nil
}

func (m *memory) del(service, name string) error {
	if err := m.before("del " + name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, service+"\x00"+name)
	return nil
}

// errEchoing is a backend error that repeats what it was given, as some do.
var errEchoing = stderrors.New("dbus: org.freedesktop.Secret.Error: item sk-live-SECRETVALUE refused")

func withBackend(b backend) *Keyring { return &Keyring{service: "ringil-test", sys: b} }
