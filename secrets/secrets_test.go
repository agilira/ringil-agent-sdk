// secrets_test.go: tests for resolvers
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

var bg = context.Background()

// failing is a source whose backend error echoes a secret.
type failing struct{}

func (failing) Lookup(context.Context, string) (string, bool, error) {
	return "", false, stderrors.New("vault said: token sk-live-LEAK is invalid")
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"A", "_x", "OPENAI_API_KEY", strings.Repeat("a", 128)} {
		if ValidateName(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "1A", "A-B", "A B", "A=B", "é", strings.Repeat("a", 129), "a/b"} {
		if !errors.HasCode(ValidateName(bad), ErrName) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEnvironment(t *testing.T) {
	t.Setenv("RINGIL_TEST_SET", "v")
	t.Setenv("RINGIL_TEST_EMPTY", "")
	cases := map[string]struct {
		value string
		found bool
	}{
		"RINGIL_TEST_SET": {"v", true}, "RINGIL_TEST_EMPTY": {"", true}, "RINGIL_TEST_ABSENT": {"", false},
	}
	for name, want := range cases {
		v, found, err := Environment{}.Lookup(bg, name)
		if err != nil || v != want.value || found != want.found {
			t.Errorf("%s: %q %v %v", name, v, found, err)
		}
	}
	if _, _, err := (Environment{}).Lookup(bg, "bad name"); !errors.HasCode(err, ErrName) {
		t.Fatalf("bad name: %v", err)
	}
}

func TestChainOrder(t *testing.T) {
	c := Chain(Map{"A": "first"}, Map{"A": "second", "B": "b"})
	if v, found, err := c.Lookup(bg, "A"); err != nil || !found || v != "first" {
		t.Fatalf("A = %q %v %v", v, found, err)
	}
	if v, found, err := c.Lookup(bg, "B"); err != nil || !found || v != "b" {
		t.Fatalf("B = %q %v %v", v, found, err)
	}
	if _, found, err := c.Lookup(bg, "C"); err != nil || found {
		t.Fatalf("C found %v %v", found, err)
	}
}

func TestChainDoesNotFallBackAfterFailure(t *testing.T) {
	broken := Chain(failing{}, Map{"A": "fallback"})
	if _, _, err := broken.Lookup(bg, "A"); err == nil {
		t.Fatal("chain fell back after a failing source")
	}
}

func TestLookupHidesBackendErrors(t *testing.T) {
	_, _, err := Lookup(bg, failing{}, "TOKEN")
	if !errors.HasCode(err, ErrLookup) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "LEAK") {
		t.Fatalf("err %v", err)
	}
}

func TestLookupChecksValues(t *testing.T) {
	bad := Map{"NUL": "a\x00b", "UTF": "a\xffb", "BIG": strings.Repeat("a", maxValue+1)}
	for name := range bad {
		if _, _, err := Lookup(bg, bad, name); !errors.HasCode(err, ErrValue) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := Lookup(bg, Map{}, "bad name"); !errors.HasCode(err, ErrName) {
		t.Fatalf("name: %v", err)
	}
	if v, found, err := Lookup(bg, Map{"OK": "fine"}, "OK"); err != nil || !found || v != "fine" {
		t.Fatalf("ok: %q %v %v", v, found, err)
	}
}

func TestLookupCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(bg)
	cancel()
	for _, r := range []Resolver{Environment{}, Map{}} {
		if _, _, err := Lookup(ctx, r, "A"); !stderrors.Is(err, context.Canceled) {
			t.Errorf("%T: %v", r, err)
		}
	}
}

func TestCredential(t *testing.T) {
	m := Map{"OK": "sk-123", "EMPTY": "", "SPACE": "sk 123", "NL": "sk\n123", "UNI": "sk-è"}
	if v, err := Credential(bg, m, "OK"); err != nil || v != "sk-123" {
		t.Fatalf("ok: %q %v", v, err)
	}
	for name, code := range map[string]errors.ErrorCode{"EMPTY": ErrMissing, "ABSENT": ErrMissing, "SPACE": ErrValue, "NL": ErrValue, "UNI": ErrValue} {
		if _, err := Credential(bg, m, name); !errors.HasCode(err, code) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestKeyFuncResolvesEachTime(t *testing.T) {
	m := Map{"K": "one"}
	f := KeyFunc(m, "K")
	if v, err := f(bg); err != nil || v != "one" {
		t.Fatalf("%q %v", v, err)
	}
	m["K"] = "two"
	if v, err := f(bg); err != nil || v != "two" {
		t.Fatalf("rotation not picked up: %q %v", v, err)
	}
}

func TestOptionalKeyFunc(t *testing.T) {
	f := OptionalKeyFunc(Map{"K": "v"}, "K")
	if v, err := f(bg); err != nil || v != "v" {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := OptionalKeyFunc(Map{}, "K")(bg); err != nil || v != "" {
		t.Fatalf("absent: %q %v", v, err)
	}
	if _, err := OptionalKeyFunc(Map{"K": "a b"}, "K")(bg); !errors.HasCode(err, ErrValue) {
		t.Fatalf("bad value: %v", err)
	}
	if _, err := OptionalKeyFunc(failing{}, "K")(bg); !errors.HasCode(err, ErrLookup) {
		t.Fatalf("failing source: %v", err)
	}
}
