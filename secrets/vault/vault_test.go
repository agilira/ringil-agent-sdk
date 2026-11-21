// vault_test.go: saving, reading and removing secrets in Vault
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

var _ secrets.Store = (*Vault)(nil)

func TestSetLookupDelete(t *testing.T) {
	f, url := newFake(t)
	f.enter = true
	v := openFake(t, url, Config{Namespace: "team/sales"})
	ctx := context.Background()
	for _, value := range []string{"sk-1", "sk-2"} {
		if err := v.Set(ctx, "OPENAI_API_KEY", value); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := secrets.Credential(ctx, v, "OPENAI_API_KEY"); err != nil || got != "sk-2" {
		t.Fatalf("%q %v", got, err)
	}
	deleteTwice(t, v, "OPENAI_API_KEY")
	if err := v.Set(ctx, "OPENAI_API_KEY", "sk-3"); err != nil { // after a delete: the deleted version counts
		t.Fatal(err)
	}
	if len(f.versions["ringil/OPENAI_API_KEY"]) != 3 || f.space[0] != "team/sales" {
		t.Fatalf("versions %v, namespace %v", f.versions, f.space)
	}
}

// deleteTwice: deleting what is not there is no error, and the entry is
// gone.
func deleteTwice(t *testing.T, v *Vault, name string) {
	t.Helper()
	for range 2 {
		if err := v.Delete(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := v.Lookup(context.Background(), name); err != nil || found {
		t.Fatalf("after delete: %v %v", found, err)
	}
}

// An entry another tool wrote without "value" is an error, not absent: a
// chain must not fall through to another source.
func TestAnEntryWithoutValue(t *testing.T) {
	f, url := newFake(t)
	f.versions["ringil/KEY"] = []map[string]any{{"password": "x"}}
	if _, _, err := openFake(t, url, Config{}).Lookup(context.Background(), "KEY"); !errors.HasCode(err, secrets.ErrValue) {
		t.Fatalf("%v", err)
	}
}

func TestConfigChecked(t *testing.T) {
	for _, cfg := range []Config{
		{URL: "http://vault.example.com", Path: "p", Token: token}, // plain http elsewhere
		{URL: "https://vault.example.com", Token: token},           // no path
		{URL: "https://vault.example.com", Path: "../x", Token: token},
		{URL: "https://vault.example.com", Path: "a//b", Token: token},
		{URL: "https://vault.example.com", Path: "p", Mount: "a b", Token: token},
		{URL: "https://vault.example.com", Path: "p"}, // no token
		{URL: "https://u:p@vault.example.com", Path: "p", Token: token},
		{URL: "https://vault.example.com/?a=b", Path: "p", Token: token},
	} {
		if _, err := New(cfg); !errors.HasCode(err, secrets.ErrSource) {
			t.Errorf("%+v accepted: %v", cfg, err)
		}
	}
}

// Audit 21/11: a wrong engine, or something else answering, read as
// "absent", and a chain fell through to another source. Only Vault's own
// answers for an absent or deleted entry are absent.
func TestOnlyVaultsAbsentIsAbsent(t *testing.T) {
	ctx := context.Background()
	f, url := newFake(t)
	if _, _, err := openFake(t, url, Config{Mount: "nomount"}).Lookup(ctx, "KEY"); !errors.HasCode(err, ErrVault) {
		t.Errorf("a missing mount: %v", err)
	}
	f.kvVer = "1"
	if err := openFake(t, url, Config{}).Set(ctx, "KEY", "v"); !errors.HasCode(err, ErrVault) || len(f.versions) != 0 {
		t.Errorf("a KV version 1 mount: %v, wrote %v", err, f.versions)
	}
	if _, _, err := openFake(t, url, Config{Namespace: "team"}).Lookup(ctx, "KEY"); !errors.HasCode(err, ErrVault) {
		t.Errorf("a namespace on the open-source server: %v", err)
	}
	for _, answer := range []string{`{}`, `null`, `{"data":null}`, `{"data":{"data":null}}`} {
		srv := answering(t, http.StatusOK, answer)
		if _, found, err := openFake(t, srv, Config{}).Lookup(ctx, "KEY"); err == nil || found {
			t.Errorf("200 %s: %v %v", answer, found, err)
		}
	}
	srv := answering(t, http.StatusNotFound, `{"errors":["no handler for route \"x\""]}`)
	if _, _, err := openFake(t, srv, Config{}).Lookup(ctx, "KEY"); !errors.HasCode(err, ErrVault) {
		t.Errorf("a 404 that is not an absent entry: %v", err)
	}
}

// answering is a server that passes the engine check and answers every
// other call with status and body.
func answering(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/sys/internal/ui/mounts/") {
			_, _ = io.WriteString(w, `{"data":{"type":"kv","options":{"version":"2"}}}`)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
