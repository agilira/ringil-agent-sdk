// security_test.go: what a server, the network or a model could do
//
// THREAT MODEL:
//   - The token is the team's key to every secret: it goes only to the
//     configured origin (no redirect elsewhere, no plain http to a remote
//     host), and only in its header.
//   - A hostile or broken server: error bodies that repeat the token or
//     the value, endless bodies, answers that are not Vault's.
//   - Names from configuration or a model: no traversal into other paths.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/secrets"
)

func TestSecurityTheTokenNeverLeavesTheOrigin(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("X-Vault-Token") != "")
	}))
	t.Cleanup(other.Close)
	standby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect) // a standby node
	}))
	t.Cleanup(standby.Close)
	v := openFake(t, standby.URL, Config{})
	if _, _, err := v.Lookup(context.Background(), "KEY"); !errors.HasCode(err, ErrVault) {
		t.Fatalf("%v", err)
	}
	if err := v.Set(context.Background(), "KEY", "secret-value"); err == nil {
		t.Fatal("a redirect was taken as an answer")
	}
	if leaked.Load() {
		t.Fatal("the token followed a redirect to another origin")
	}
}

func TestSecurityErrorsHoldNoServerText(t *testing.T) {
	for _, status := range []int{400, 403, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"errors":["ECHO hvs.test-token sk-VALUE"]}`, status)
		}))
		v := openFake(t, srv.URL, Config{})
		_, _, lookupErr := v.Lookup(context.Background(), "KEY")
		for _, err := range []error{lookupErr, v.Set(context.Background(), "KEY", "sk-VALUE")} {
			if err == nil || strings.Contains(err.Error(), "ECHO") || strings.Contains(err.Error(), "hvs.") || strings.Contains(err.Error(), "sk-VALUE") {
				t.Errorf("%d: %v", status, err)
			}
		}
		srv.Close()
	}
}

func TestSecurityHugeOrForeignAnswers(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", maxResponse+10), "not json", `{"data":{"data":{"value":42},"metadata":{"version":1}}}`} {
		if _, found, err := openFake(t, answering(t, http.StatusOK, body), Config{}).Lookup(context.Background(), "KEY"); err == nil || found {
			t.Errorf("%.20q: found %v, %v", body, found, err)
		}
	}
}

// Audit 21/11: with a proxy in the environment, a plain http Vault got its
// token through the proxy, in clear, and the proxy's answers were taken.
func TestSecurityNoProxyReadsTheToken(t *testing.T) {
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Store(true)
		http.Error(w, `{"errors":[]}`, http.StatusNotFound)
	}))
	t.Cleanup(proxy.Close)
	// A transport with a proxy, as the environment's (read once per
	// process, so set here) gives one.
	u, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	_, _, _ = openFake(t, "http://vault.invalid:8200", Config{AllowHTTP: true, Client: client}).Lookup(context.Background(), "KEY")
	if proxied.Load() {
		t.Fatal("a plain http request went through the proxy")
	}
}

func TestSecurityBadTokensAreNotSent(t *testing.T) {
	f, url := newFake(t)
	for _, tok := range []string{"", "a b", "tok\r\nX-Evil: 1"} {
		v := openFake(t, url, Config{Token: func(context.Context) (string, error) { return tok, nil }})
		if _, _, err := v.Lookup(context.Background(), "KEY"); !errors.HasCode(err, ErrVault) {
			t.Errorf("%q: %v", tok, err)
		}
	}
	if len(f.tokens) != 0 {
		t.Fatalf("requests were sent: %v", f.tokens)
	}
}

func FuzzNames(f *testing.F) {
	for _, s := range []string{"KEY", "../KEY", "a/b", "KEY?x=1", "%2e%2e", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		fv := &fakeVault{mount: "secret", versions: map[string][]map[string]any{}, deleted: map[string]bool{}}
		srv := httptest.NewServer(fv)
		defer srv.Close()
		err := openFake(t, srv.URL, Config{}).Set(context.Background(), name, "v")
		if secrets.ValidateName(name) != nil {
			if err == nil || len(fv.versions) != 0 {
				t.Fatalf("%q reached Vault", name)
			}
			return
		}
		for path := range fv.versions {
			if path != "ringil/"+name {
				t.Fatalf("%q wrote %q", name, path)
			}
		}
	})
}
