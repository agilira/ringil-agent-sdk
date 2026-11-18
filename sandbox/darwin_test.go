// darwin_test.go: the Seatbelt backend, exercised for real
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package sandbox

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/egress"
)

func darwinOrSkip(t *testing.T) Sandbox {
	t.Helper()
	s, err := New()
	if err != nil {
		if os.Getenv("RINGIL_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	return s
}

func runMac(t *testing.T, s Sandbox, spec Spec) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	spec.Stdout, spec.Stderr = &out, &errOut
	p, err := s.Start(ctx, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	werr := p.Wait()
	return out.String(), errOut.String(), werr
}

func shMac(script string) []string { return []string{"/bin/bash", "-c", script} }

func files(t *testing.T, dir string, m map[string]string) {
	t.Helper()
	for name, body := range m {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMacRunsIsolatedAndWritesOnlyGrants(t *testing.T) {
	s := darwinOrSkip(t)
	t.Setenv("RINGIL_HOST_SECRET", "leak")
	home, _ := os.UserHomeDir()
	dir := t.TempDir()
	out, errOut, err := runMac(t, s, Spec{
		Argv: shMac(`echo "secret=${RINGIL_HOST_SECRET:-none} given=$GIVEN"
echo hi > /w.txt 2>/dev/null && echo root-writable || echo root-denied
echo hi > "$W/new.txt" && echo work-writable
echo hi > "$TMPDIR/t" && echo tmp-writable
ls "$H" >/dev/null 2>&1 && echo home-listed || echo home-denied
cat /etc/hosts >/dev/null && echo etc-readable`),
		Env:    map[string]string{"GIVEN": "yes", "W": dir, "H": home},
		Dir:    dir,
		Mounts: []Mount{{Source: dir, Writable: true}},
	})
	want := "secret=none given=yes\nroot-denied\nwork-writable\ntmp-writable\nhome-denied\netc-readable\n"
	if err != nil || out != want {
		t.Fatalf("got %q want %q: %v %s", out, want, err, errOut)
	}
}

func TestMacHiddenAndReadOnlyHold(t *testing.T) {
	s := darwinOrSkip(t)
	dir := t.TempDir()
	files(t, dir, map[string]string{"cfg/.env": "SECRET=1", ".git/hooks/pre-commit": "old", ".git/config": "old", "src/a": "a"})
	out, errOut, err := runMac(t, s, Spec{
		Argv: shMac(`cd "$W"
cat cfg/.env 2>/dev/null || echo env-hidden
mv cfg c2 2>/dev/null && echo cfg-moved || echo cfg-pinned
ln cfg/.env x 2>/dev/null && cat x 2>/dev/null | grep -q SECRET && echo link-leaked || echo link-held
echo evil > .git/hooks/pre-commit 2>/dev/null && echo hook-written || echo hook-ro
echo evil > .git/hooks/new 2>/dev/null && echo hook-created || echo hooks-ro
mv .git .g 2>/dev/null && echo git-moved || echo git-pinned
ln .git/config c 2>/dev/null && (echo evil >> c) 2>/dev/null && echo config-via-link || echo config-held
echo ok > .git/HEAD && echo git-writable
echo b > src/a && echo src-writable`),
		Env:    map[string]string{"W": dir},
		Mounts: []Mount{{Source: dir, Writable: true, Hide: []string{"cfg/.env"}, ReadOnly: []string{".git/hooks", ".git/config"}}},
	})
	want := "env-hidden\ncfg-pinned\nlink-held\nhook-ro\nhooks-ro\ngit-pinned\nconfig-held\ngit-writable\nsrc-writable\n"
	if err != nil || out != want {
		t.Fatalf("got\n%s\nwant\n%s\n%v %s", out, want, err, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".git/config")); string(b) != "old" {
		t.Fatal("a read-only file changed")
	}
}

func TestMacNoNetwork(t *testing.T) {
	s := darwinOrSkip(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "x") }))
	defer srv.Close()
	out, _, _ := runMac(t, s, Spec{Argv: shMac(`curl -s -m 3 ` + srv.URL + ` >/dev/null && echo local || echo no-local
curl -s -m 3 https://example.com >/dev/null && echo internet || echo no-internet`)})
	if out != "no-local\nno-internet\n" {
		t.Fatalf("%q", out)
	}
}

type loopName struct{}

func (loopName) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func TestMacProxiedNetworkReachesOnlyTheProxy(t *testing.T) {
	s := darwinOrSkip(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "through") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	p, err := egress.New(egress.Config{Allow: []string{"ok.test"}, Ports: []int{n}, Private: true, Resolver: loopName{}})
	if err != nil {
		t.Fatal(err)
	}
	sock, stop, err := p.Listen(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	out, errOut, err := runMac(t, s, Spec{Network: ProxiedNetwork, Proxy: sock, Argv: shMac(`curl -s -m 5 http://ok.test:` + port + `/; echo
curl -s -m 5 -o /dev/null -w "%{http_code}\n" http://no.test:` + port + `/
curl -s -m 3 --noproxy '*' http://127.0.0.1:` + port + `/ >/dev/null && echo direct || echo no-direct`)})
	if err != nil || out != "through\n403\nno-direct\n" {
		t.Fatalf("%q %v %s", out, err, errOut)
	}
}

func TestMacGrantsRefused(t *testing.T) {
	s := darwinOrSkip(t)
	dir := t.TempDir()
	files(t, dir, map[string]string{"secret/x": "x", "other/y": "y"})
	_, err := s.Start(context.Background(), Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: dir}}, Protected: []string{filepath.Join(dir, "secret")}})
	if !errors.HasCode(err, ErrGrant) {
		t.Errorf("protected overlap: %v", err)
	}
	_, err = s.Start(context.Background(), Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: filepath.Join(dir, "other"), Target: "/elsewhere"}}})
	if !errors.HasCode(err, ErrConfig) {
		t.Errorf("a grant placed elsewhere: %v", err)
	}
}

func TestMacCancelKillsTheGroup(t *testing.T) {
	s := darwinOrSkip(t)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := s.Start(ctx, Spec{Argv: shMac("sleep 60 & sleep 60")})
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not end the sandbox")
	}
}
