//go:build live

// live_test.go: a real Vault (VAULT_ADDR, VAULT_TOKEN), on a path of its own
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

func TestLiveVault(t *testing.T) {
	addr := os.Getenv("VAULT_ADDR")
	if addr == "" || os.Getenv("VAULT_TOKEN") == "" {
		t.Skip("VAULT_ADDR and VAULT_TOKEN are not set")
	}
	v, err := New(Config{URL: addr, Path: "ringil-live-test/" + rand.Text()[:8], Token: secrets.KeyFunc(secrets.Environment{}, "VAULT_TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { // every version, not only the latest: nothing of the test stays
		_, _ = v.call(context.Background(), http.MethodDelete, "/"+v.cfg.Mount+"/metadata/"+v.cfg.Path+"/LIVE_KEY", nil, nil)
	})
	checkAbsent(t, v)
	for _, value := range []string{"uno", " spazi ", `"quote' \ back`, "a\nb", "àèìòù € 日本", ""} {
		roundTrip(t, v, value)
	}
	if err := v.Delete(ctx, "LIVE_KEY"); err != nil {
		t.Fatal(err)
	}
	checkAbsent(t, v)
	bad, _ := New(Config{URL: addr, Path: "x", Token: func(context.Context) (string, error) { return "hvs.wrong", nil }})
	if _, _, err := bad.Lookup(ctx, "LIVE_KEY"); err == nil {
		t.Fatal("a wrong token was accepted")
	}
}

func checkAbsent(t *testing.T, v *Vault) {
	t.Helper()
	if _, found, err := v.Lookup(context.Background(), "LIVE_KEY"); err != nil || found {
		t.Fatalf("not absent: %v %v", found, err)
	}
}

func roundTrip(t *testing.T, v *Vault, value string) {
	t.Helper()
	ctx := context.Background()
	if err := v.Set(ctx, "LIVE_KEY", value); err != nil {
		t.Fatalf("%q: %v", value, err)
	}
	if got, found, err := v.Lookup(ctx, "LIVE_KEY"); err != nil || !found || got != value {
		t.Fatalf("%q came back %q %v %v", value, got, found, err)
	}
}

// Audit 21/11, on the real server: a missing mount, a KV version 1 one
// (cubbyhole) and a namespace on the open-source server are errors, and
// nothing is written.
func TestLiveVaultWrongPlaces(t *testing.T) {
	addr := os.Getenv("VAULT_ADDR")
	if addr == "" || os.Getenv("VAULT_TOKEN") == "" {
		t.Skip("VAULT_ADDR and VAULT_TOKEN are not set")
	}
	tok := secrets.KeyFunc(secrets.Environment{}, "VAULT_TOKEN")
	for _, cfg := range []Config{{Mount: "nomount"}, {Mount: "cubbyhole"}, {Namespace: "team"}} {
		cfg.URL, cfg.Path, cfg.Token = addr, "ringil-live-test/wrong", tok
		v, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := v.Lookup(context.Background(), "KEY"); err == nil {
			t.Errorf("%+v: a lookup answered", cfg)
		}
		if err := v.Set(context.Background(), "KEY", "v"); err == nil {
			t.Errorf("%+v: a write was accepted", cfg)
		}
	}
}
