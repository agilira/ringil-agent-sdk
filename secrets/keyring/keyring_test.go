// keyring_test.go: saving, reading and removing secrets
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	"context"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

var _ secrets.Store = (*Keyring)(nil)

func TestSetLookupDelete(t *testing.T) {
	ctx := context.Background()
	k := withBackend(newMemory())
	if _, found, err := k.Lookup(ctx, "OPENAI_API_KEY"); err != nil || found {
		t.Fatalf("absent: found %v, %v", found, err)
	}
	for _, v := range []string{"sk-1", "sk-2"} { // the second replaces the first
		if err := k.Set(ctx, "OPENAI_API_KEY", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, _, _ := k.Lookup(ctx, "OPENAI_API_KEY"); v != "sk-2" {
		t.Fatalf("read %q", v)
	}
	// Through the package's checks, as a host resolves a credential.
	if v, err := secrets.Credential(ctx, secrets.Chain(k, secrets.Environment{}), "OPENAI_API_KEY"); err != nil || v != "sk-2" {
		t.Fatalf("credential %q %v", v, err)
	}
	deleteTwice(t, k, "OPENAI_API_KEY")
}

// deleteTwice: deleting what is not there is no error, and the entry is
// gone.
func deleteTwice(t *testing.T, k *Keyring, name string) {
	t.Helper()
	ctx := context.Background()
	for range 2 {
		if err := k.Delete(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, _ := k.Lookup(ctx, name); found {
		t.Fatal("still there after Delete")
	}
}

// Each application keeps its own entries.
func TestServicesAreSeparate(t *testing.T) {
	ctx := context.Background()
	m := newMemory()
	a, b := &Keyring{service: "app-a", sys: m}, &Keyring{service: "app-b", sys: m}
	if err := a.Set(ctx, "KEY", "a"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := b.Lookup(ctx, "KEY"); found {
		t.Fatal("another service's entry was read")
	}
}

func TestServiceNames(t *testing.T) {
	for _, bad := range []string{"", "-", ".", "..", "-ringil", ".ringil", " ringil", "ringil\n", "rìngil", "my app", `a"b`, "a'b", `a\b`, string(make([]byte, 65))} {
		if _, err := New(bad); !errors.HasCode(err, secrets.ErrSource) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"a", "ringil.agent-sdk_2"} {
		if _, err := New(ok); err != nil {
			t.Fatalf("%q rejected: %v", ok, err)
		}
	}
}

// A value the keyring cannot hold (Windows: about 2.5 KB) is a value
// error, not a broken keyring.
func TestTooLongForTheKeyring(t *testing.T) {
	m := newMemory()
	m.max = 8
	if err := withBackend(m).Set(context.Background(), "KEY", "0123456789"); !errors.HasCode(err, secrets.ErrValue) {
		t.Fatalf("%v", err)
	}
}
