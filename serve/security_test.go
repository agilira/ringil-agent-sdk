// security_test.go: fuzz and HTTP guard/parsing security tests for serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

func FuzzServeHTTP(f *testing.F) {
	f.Add("127.0.0.1:8080", "0", "application/json", []byte(`{"profile":"p","input":"hi"}`))
	f.Add("[::1]:8080", "42", "application/json; charset=utf-8", []byte(`{"profile":"p","input":"hi","extra":1}`))
	f.Add("localhost", "-1", "application/json", []byte(`{"profile":"p","input":"hi"}{"two":2}`))
	f.Add("localhost.evil.com", "abc", "text/plain", []byte("\xff\xfe"))
	f.Add("127.0.0.1.nip.io", "99999999999999999999999", "application/json", []byte(`{}`))
	f.Add("0.0.0.0", "", "application/json", []byte(`null`))
	f.Add("[::]", "", "", []byte{})

	f.Fuzz(func(t *testing.T, host, lastID, contentType string, body []byte) {
		if len(body) > 8192 {
			return
		}
		_ = loopbackHost(host)
		req := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
		req.Host = host
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if n, ok := resumeFrom(req); ok && n < 0 {
			t.Fatalf("resumeFrom accepted negative id %d for %q", n, lastID)
		}
		var in startIn
		rec := httptest.NewRecorder()
		if readJSON(rec, req, &in) && !utf8.Valid(body) {
			t.Fatalf("readJSON accepted invalid UTF-8 body")
		}
	})
}

func TestSecurityGuardBlocksRebindingOriginAndBadAuth(t *testing.T) {
	s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(nil)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.Handler()

	assertGuardRejectsHost(t, h, []string{"evil.com", "localhost.evil.com", "127.0.0.1.evil.com", "0.0.0.0:8080", "10.0.0.1:8080", "[::]:8080", ""})
	assertGuardRejectsOrigin(t, h, []string{"https://evil.com", "http://localhost:3000", "null"})
	assertGuardRejectsAuth(t, h, []string{"", "Basic dXNlcjpwYXNz", "Bearer wrong", "Bearer " + token[:len(token)-1], "Bearer " + token + "x"})
	for _, host := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost", "LOCALHOST:8080"} {
		rec := callGuard(h, host, "", "Bearer "+token)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("valid host %q: code=%d cache=%q", host, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
}

func assertGuardRejectsHost(t *testing.T, h http.Handler, hosts []string) {
	t.Helper()
	for _, host := range hosts {
		rec := callGuard(h, host, "", "Bearer "+token)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SERVE_HOST") {
			t.Errorf("host %q: code=%d body=%s", host, rec.Code, rec.Body.String())
		}
	}
}

func assertGuardRejectsOrigin(t *testing.T, h http.Handler, origins []string) {
	t.Helper()
	for _, origin := range origins {
		rec := callGuard(h, "127.0.0.1:8080", origin, "Bearer "+token)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SERVE_ORIGIN") {
			t.Errorf("origin %q: code=%d body=%s", origin, rec.Code, rec.Body.String())
		}
	}
}

func assertGuardRejectsAuth(t *testing.T, h http.Handler, auths []string) {
	t.Helper()
	for _, auth := range auths {
		rec := callGuard(h, "127.0.0.1:8080", "", auth)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("auth %q: code=%d www=%q", auth, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
}

func callGuard(h http.Handler, host, origin, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/profiles", nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSecurityReadJSONRejectsBadBodies(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{{Name: "p", New: Fixed(nil)}}})
	cases := []struct {
		name, ctype, path, body string
		want                    int
	}{
		{"wrongContentType", "text/plain", "/v1/runs", `{"profile":"p","input":"hi"}`, http.StatusUnsupportedMediaType},
		{"oversized", "application/json", "/v1/runs", `{"profile":"p","input":"` + strings.Repeat("a", maxBody+1) + `"}`, http.StatusBadRequest},
		{"invalidUTF8", "application/json", "/v1/runs", "{\"profile\":\"p\",\"input\":\"\xff\"}", http.StatusBadRequest},
		{"unknownField", "application/json", "/v1/runs", `{"profile":"p","input":"hi","admin":true}`, http.StatusBadRequest},
		{"trailingJSON", "application/json", "/v1/runs", `{"profile":"p","input":"hi"}{"profile":"p","input":"two"}`, http.StatusBadRequest},
		{"missingAllow", "application/json", "/v1/runs/x/approvals/y", `{}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodPost, a.url+c.path, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", c.ctype)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

type stubListener struct {
	addr     net.Addr
	accepted bool
}

func (l *stubListener) Accept() (net.Conn, error) {
	l.accepted = true
	return nil, net.ErrClosed
}
func (l *stubListener) Close() error   { return nil }
func (l *stubListener) Addr() net.Addr { return l.addr }

func TestSecurityServeRefusesNonLoopbackListener(t *testing.T) {
	s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(nil)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, addr := range []net.Addr{
		&net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
		&net.UnixAddr{Name: "/tmp/ringil-test.sock", Net: "unix"},
	} {
		ln := &stubListener{addr: addr}
		if err := s.Serve(t.Context(), ln); !errors.HasCode(err, ErrConfig) || ln.accepted {
			t.Errorf("addr %v: err=%v accepted=%v", addr, err, ln.accepted)
		}
	}
}
