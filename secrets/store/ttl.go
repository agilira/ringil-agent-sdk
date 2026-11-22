// ttl.go: what a store read, kept for a while
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package store

import (
	"context"
	"sync"
	"time"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

// ttlStore keeps each answer of its store (found or not, never an error)
// for ttl; a Set or Delete through it forgets the name.
type ttlStore struct {
	secrets.Store
	ttl time.Duration

	mu   sync.Mutex
	kept map[string]answer
}

type answer struct {
	value string
	found bool
	at    time.Time
}

func newTTLStore(s secrets.Store, ttl time.Duration) secrets.Store {
	if ttl < 0 {
		return s
	}
	return &ttlStore{Store: s, ttl: ttl, kept: map[string]answer{}}
}

func (t *ttlStore) Lookup(ctx context.Context, name string) (string, bool, error) {
	t.mu.Lock()
	a, ok := t.kept[name]
	t.mu.Unlock()
	if ok && time.Since(a.at) < t.ttl {
		return a.value, a.found, nil
	}
	value, found, err := t.Store.Lookup(ctx, name)
	if err != nil {
		return "", false, err
	}
	t.mu.Lock()
	t.kept[name] = answer{value: value, found: found, at: time.Now()}
	t.mu.Unlock()
	return value, found, nil
}

func (t *ttlStore) Set(ctx context.Context, name, value string) error {
	t.forget(name)
	defer t.forget(name)
	return t.Store.Set(ctx, name, value)
}

func (t *ttlStore) Delete(ctx context.Context, name string) error {
	t.forget(name)
	defer t.forget(name)
	return t.Store.Delete(ctx, name)
}

func (t *ttlStore) forget(name string) {
	t.mu.Lock()
	delete(t.kept, name)
	t.mu.Unlock()
}

// Close closes the store, if it holds resources.
func (t *ttlStore) Close() error {
	if cl, ok := t.Store.(interface{ Close() error }); ok {
		return cl.Close()
	}
	return nil
}
