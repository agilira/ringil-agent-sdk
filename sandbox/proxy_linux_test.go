// proxy_linux_test.go: the proxied network, home, path and read-only
// paths, exercised for real
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package sandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/egress"
)

type oneName struct{}

func (oneName) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func proxied(t *testing.T) Sandbox {
	t.Helper()
	s := sandboxOrSkip(t)
	if !executable(socatPath) || !executable(nsenterPath) {
		if os.Getenv("RINGIL_REQUIRE_SANDBOX") != "" {
			t.Fatal("socat and nsenter are required")
		}
		t.Skip("socat or nsenter not installed")
	}
	return s
}

func TestProxiedNetworkReachesOnlyTheProxy(t *testing.T) {
	s := proxied(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through")
	}))
	defer srv.Close()
	port := srv.Listener.Addr().String()
	port = port[strings.LastIndexByte(port, ':')+1:]
	// The test server is on loopback: the proxy may reach it (Private),
	// the sandbox directly must not.
	p, err := egress.New(egress.Config{Allow: []string{"ok.test"}, Ports: []int{atoi(t, port)}, Private: true, Resolver: oneName{}})
	if err != nil {
		t.Fatal(err)
	}
	sock, stop, err := p.Listen(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	script := `echo "proxy=$HTTP_PROXY"
curl -s -m 5 http://ok.test:` + port + `/ ; echo
curl -s -m 5 -o /dev/null -w "%{http_code}\n" http://no.test:` + port + `/
timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/` + port + `' 2>/dev/null && echo host-reached || echo host-blocked
timeout 3 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null && echo internet || echo no-internet
getent hosts example.com >/dev/null && echo dns || echo no-dns
python3 -c "import socket
try:
 socket.socket(socket.AF_UNIX)
 print('unix-open')
except OSError:
 print('unix-refused')"`
	out, errOut, err := run(t, s, Spec{Argv: sh(script), Network: ProxiedNetwork, Proxy: sock})
	if err != nil {
		t.Fatalf("%v: %s", err, errOut)
	}
	want := "proxy=http://" + ProxyAddr + "\nthrough\n403\nhost-blocked\nno-internet\nno-dns\nunix-refused\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s\nstderr %s", out, want, errOut)
	}
}

func TestProxiedNetworkWithoutProxyRunning(t *testing.T) {
	s := proxied(t)
	out, _, err := run(t, s, Spec{Argv: sh(`curl -s -m 5 -o /dev/null -w "%{http_code}" http://ok.test/; echo " $?"`),
		Network: ProxiedNetwork, Proxy: filepath.Join(t.TempDir(), "none.sock")})
	if err != nil || strings.HasPrefix(out, "200") {
		t.Fatalf("%q %v", out, err)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHomePathAndFileMounts(t *testing.T) {
	s := sandboxOrSkip(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "gitconfig")
	bin := filepath.Join(dir, "tool", "bin")
	if err := os.WriteFile(conf, []byte("[user]\n\tname = Tester\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "hello"), []byte("#!/bin/sh\necho hi-from-tool\n"), 0o700); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
	out, errOut, err := run(t, s, Spec{
		Argv: sh(`echo "$HOME"; cat ~/.gitconfig | head -1; hello; touch ~/new && echo home-writable;
echo x >> ~/.gitconfig 2>/dev/null && echo conf-writable || echo conf-ro`),
		Home: "/home/me",
		Path: []string{"/opt/tool/bin", "/usr/bin", "/bin"},
		Mounts: []Mount{
			{Source: conf, Target: "/home/me/.gitconfig"},
			{Source: filepath.Join(dir, "tool"), Target: "/opt/tool"},
		},
	})
	if err != nil || out != "/home/me\n[user]\nhi-from-tool\nhome-writable\nconf-ro\n" {
		t.Fatalf("%q %v %s", out, err, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err == nil {
		t.Fatal("the private home wrote to the host")
	}
}

func TestReadOnlyPathsInAWritableMount(t *testing.T) {
	s := sandboxOrSkip(t)
	dir := t.TempDir()
	for _, p := range []string{".git/hooks/pre-commit", ".git/config", ".git/objects/x", "src/a.go"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, err := run(t, s, Spec{
		Argv: sh(`cd /w; for f in .git/hooks/pre-commit .git/hooks/post-checkout .git/config .git/objects/y src/a.go; do
(echo new > $f) 2>/dev/null && echo "$f ok" || echo "$f ro"; done`),
		Mounts: []Mount{{Source: dir, Target: "/w", Writable: true, ReadOnly: []string{".git/hooks", ".git/config", "missing"}}},
	})
	want := ".git/hooks/pre-commit ro\n.git/hooks/post-checkout ro\n.git/config ro\n.git/objects/y ok\nsrc/a.go ok\n"
	if err != nil || out != want {
		t.Fatalf("%q %v %s", out, err, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".git/hooks/pre-commit")); string(b) != "old" {
		t.Fatal("a read-only path changed")
	}
}

func TestNewSpecFieldsValidated(t *testing.T) {
	for name, spec := range map[string]Spec{
		"home relative":     {Home: "home/me"},
		"home reserved":     {Home: "/usr/me"},
		"home is tmp":       {Home: "/tmp"},
		"home inside mount": {Home: "/w/me", Mounts: []Mount{{Source: "/x", Target: "/w"}}},
		"path relative":     {Path: []string{"bin"}},
		"path colon":        {Path: []string{"/a:/b"}},
		"proxy without net": {Proxy: "/x.sock"},
		"net without proxy": {Network: ProxiedNetwork},
		"proxy relative":    {Network: ProxiedNetwork, Proxy: "x.sock"},
		"proxy env":         {Env: map[string]string{"HTTPS_PROXY": "http://evil"}},
		"ro on read mount":  {Mounts: []Mount{{Source: "/x", Target: "/w", ReadOnly: []string{"a"}}}},
		"ro escaping":       {Mounts: []Mount{{Source: "/x", Target: "/w", Writable: true, ReadOnly: []string{"../a"}}}},
	} {
		spec.Argv = []string{"true"}
		if err := validate(spec); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Audit 18/11: moving .git aside and copying it back made its hooks
// writable; the directories above a read-only path are now mount points.
func TestReadOnlyParentsCannotBeMoved(t *testing.T) {
	s := sandboxOrSkip(t)
	dir := t.TempDir()
	for _, p := range []string{".git/hooks/pre-commit", ".git/config", "sub/deep/CLAUDE.md"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, err := run(t, s, Spec{
		Argv: sh(`cd /w; mv .git .g 2>/dev/null && echo moved-git || echo git-pinned
mv sub s2 2>/dev/null && echo moved-sub || echo sub-pinned
mv sub/deep sub/d2 2>/dev/null && echo moved-deep || echo deep-pinned
echo new > .git/HEAD && echo git-writable`),
		Mounts: []Mount{{Source: dir, Target: "/w", Writable: true, ReadOnly: []string{".git/hooks", ".git/config", "sub/deep/CLAUDE.md"}}},
	})
	if err != nil || out != "git-pinned\nsub-pinned\ndeep-pinned\ngit-writable\n" {
		t.Fatalf("%q %v %s", out, err, errOut)
	}
}

func TestSymlinkSourceRefused(t *testing.T) {
	s := sandboxOrSkip(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "f"), filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Start(context.Background(), Spec{Argv: []string{"true"}, Mounts: []Mount{{Source: filepath.Join(dir, "l"), Target: "/x"}}})
	if !errors.HasCode(err, ErrGrant) {
		t.Fatalf("%v", err)
	}
}
