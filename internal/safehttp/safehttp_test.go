// safehttp_test.go: credentials stay home, bodies stay bounded
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package safehttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"
)

func TestCheckURL(t *testing.T) {
	ok := []string{"https://a.example/x", "http://127.0.0.1:8080/", "http://[::1]/"}
	bad := []string{"http://a.example", "https://u:p@a.example", "https://a.example/#f", "ftp://a", "http://localhost/", "https://"}
	for _, u := range ok {
		if err := CheckURL(u, false); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := CheckURL(u, false); !errors.HasCode(err, ErrURL) {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestHeadersSameOriginOnly(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Key") != "" {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Key") != "k" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := Client(nil, func(context.Context) (http.Header, error) { return http.Header{"X-Key": {"k"}}, nil }, 1<<20, false)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect || leaked.Load() {
		t.Fatalf("status %d leaked %v", resp.StatusCode, leaked.Load())
	}
}

// Audit 18/11: a body labelled as an event stream escaped the bound.
func TestBodyBoundAndStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, strings.Repeat("x", 4096)); err != nil {
			return
		}
	}))
	defer srv.Close()
	for streams, wantErr := range map[bool]bool{false: true, true: false} {
		resp, err := Client(nil, nil, 1024, streams).Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if (rerr != nil) != wantErr || (rerr != nil && !errors.HasCode(rerr, ErrTooLarge)) {
			t.Errorf("streams=%v: %v", streams, rerr)
		}
	}
}

// A body of exactly the bound is not mistaken for one that grew past it
// (regression: the limit was off by one at the exact bound).
func TestBodyBoundAllowsExactMax(t *testing.T) {
	const max = 1024
	for _, n := range []int{max - 1, max, max + 1} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", n))
		}))
		resp, err := Client(nil, nil, max, false).Get(srv.URL)
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		got, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		srv.Close()
		wantErr := n > max
		if (rerr != nil) != wantErr {
			t.Fatalf("n=%d: err=%v want error=%v", n, rerr, wantErr)
		}
		if !wantErr && len(got) != n {
			t.Fatalf("n=%d: read %d bytes", n, len(got))
		}
		if wantErr && !errors.HasCode(rerr, ErrTooLarge) {
			t.Fatalf("n=%d: want ErrTooLarge, got %v", n, rerr)
		}
	}
}

// A host headers-func error must not surface in the returned error's message
// (it could carry a credential path or value); it stays in the chain only.
func TestCredentialErrorNotInMessage(t *testing.T) {
	const secret = "s3cr3t-token-/home/u/.config/key"
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	bad := func(context.Context) (http.Header, error) {
		return nil, errors.New(ErrCredentials, secret)
	}
	resp, err := Client(nil, bad, 1<<20, false).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected a credentials error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error message leaked the host func's text: %q", err.Error())
	}
}

// A transport error is returned as it is, also by a client that streams.
func TestTransportErrorWithStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	resp, err := Client(nil, nil, 1<<10, true).Get(url)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a closed server answered")
	}
}
