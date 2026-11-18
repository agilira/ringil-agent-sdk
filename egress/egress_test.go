// egress_test.go: what the proxy lets through and what it refuses
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"
)

// names is a resolver from a fixed table.
type names map[string][]string

func (n names) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range n[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

// upstream is a local HTTP server; the proxy reaches it only with Private.
func upstream(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.URL.Path+" auth="+r.Header.Get("Proxy-Authorization"))
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return srv, n
}

func start(t *testing.T, cfg Config) string {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, stop, err := p.Listen(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return path
}

// ask sends raw through the proxy and returns the status line and the rest.
func ask(t *testing.T, sock, raw string) (string, string) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	rest, _ := io.ReadAll(br)
	return strings.TrimSpace(status), string(rest)
}

func TestConnectAndPlainHTTPToAllowedDomain(t *testing.T) {
	_, port := upstream(t)
	sock := start(t, Config{Allow: []string{"ok.test"}, Ports: []int{port}, Private: true,
		Resolver: names{"ok.test": {"127.0.0.1"}}})
	p := strconv.Itoa(port)

	status, body := ask(t, sock, "GET http://ok.test:"+p+"/x HTTP/1.1\r\nHost: ok.test:"+p+"\r\nProxy-Authorization: Basic c2VjcmV0\r\n\r\n")
	if !strings.Contains(status, "200") || !strings.Contains(body, "hello /x auth=\n") && !strings.HasSuffix(body, "hello /x auth=") {
		t.Fatalf("plain http: %q %q", status, body)
	}

	status, body = ask(t, sock, "CONNECT ok.test:"+p+" HTTP/1.1\r\nHost: ok.test:"+p+"\r\n\r\nGET /tunnel HTTP/1.1\r\nHost: ok.test\r\nConnection: close\r\n\r\n")
	if !strings.Contains(status, "200") || !strings.Contains(body, "hello /tunnel") {
		t.Fatalf("connect: %q %q", status, body)
	}
}

func TestRefusals(t *testing.T) {
	_, port := upstream(t)
	p := strconv.Itoa(port)
	sock := start(t, Config{Allow: []string{"ok.test", "*.wild.test", "local.test", "meta.test", "mapped.test", "nat64.test"}, Ports: []int{port},
		Resolver: names{
			"ok.test":     {"127.0.0.1"},
			"local.test":  {"10.1.2.3"},
			"meta.test":   {"169.254.169.254"},
			"mapped.test": {"::ffff:127.0.0.1"},
			"nat64.test":  {"64:ff9b::a01:203"},
			"other.test":  {"8.8.8.8"},
		}})
	for _, c := range []struct{ host, want string }{
		{"ok.test", "private or local"}, // allowed name, loopback address
		{"local.test", "private or local"},
		{"meta.test", "private or local"},
		{"mapped.test", "private or local"},
		{"nat64.test", "private or local"},
		{"other.test", "not allowed"},
		{"wild.test", "not allowed"}, // *.wild.test is the subdomains only
		{"127.0.0.1", "not allowed"},
		{"bad_name.test", "not allowed"},
		{"оk.test", "not allowed"}, // Cyrillic o
	} {
		status, body := ask(t, sock, "CONNECT "+c.host+":"+p+" HTTP/1.1\r\nHost: x\r\n\r\n")
		if !strings.Contains(status, "403") || !strings.Contains(body, c.want) {
			t.Errorf("%s: %q %q", c.host, status, body)
		}
	}
	status, body := ask(t, sock, "CONNECT ok.test:22 HTTP/1.1\r\nHost: x\r\n\r\n")
	if !strings.Contains(status, "403") || !strings.Contains(body, "port 22") {
		t.Errorf("port: %q %q", status, body)
	}
	status, _ = ask(t, sock, "GET https://ok.test/ HTTP/1.1\r\nHost: ok.test\r\n\r\n")
	if !strings.Contains(status, "400") {
		t.Errorf("absolute https request: %q", status)
	}
}

func TestWildcardAllowsSubdomains(t *testing.T) {
	p, err := New(Config{Allow: []string{"*.github.com", "Example.ORG."}})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"api.github.com": true, "a.b.github.com": true, "github.com": false, "evilgithub.com": false,
		"example.org": true, "EXAMPLE.org.": true, "www.example.org": false,
	} {
		if got, _ := p.Allowed(context.Background(), host); got != want {
			t.Errorf("%s: %v", host, got)
		}
	}
	for _, bad := range []string{"*", "*.com", "", "a..b", "-a.com", "ex ample.com", "1.2.3.4", "*.*.com"} {
		if _, err := New(Config{Allow: []string{bad}}); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := New(Config{Ports: []int{0}}); !errors.HasCode(err, ErrConfig) {
		t.Error("port 0 accepted")
	}
}

func TestAskOnceAndRemember(t *testing.T) {
	var asked atomic.Int32
	p, err := New(Config{Ask: func(_ context.Context, host string) (bool, error) {
		asked.Add(1)
		return host == "yes.test", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if ok, _ := p.Allowed(context.Background(), "yes.test"); !ok {
			t.Fatal("yes refused")
		}
		if ok, _ := p.Allowed(context.Background(), "no.test"); ok {
			t.Fatal("no allowed")
		}
	}
	if asked.Load() != 2 {
		t.Fatalf("asked %d times, want 2", asked.Load())
	}
	q, _ := New(Config{})
	if ok, _ := q.Allowed(context.Background(), "yes.test"); ok {
		t.Fatal("no Ask must refuse")
	}
}

func TestStopEndsOpenTunnels(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n, _ := strconv.Atoi(port)
	p, _ := New(Config{Allow: []string{"ok.test"}, Ports: []int{n}, Private: true, Resolver: names{"ok.test": {"127.0.0.1"}}})
	sock, stop, err := p.Listen(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "CONNECT ok.test:"+port+" HTTP/1.1\r\nHost: x\r\n\r\n")
	br := bufio.NewReader(c)
	if status, _ := br.ReadString('\n'); !strings.Contains(status, "200") {
		t.Fatalf("connect: %q", status)
	}
	_, _ = br.ReadString('\n') // the blank line ending the answer
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop hangs on an open tunnel")
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("tunnel still open after stop")
	}
}

// Audit 18/11: a call whose run had ended waited on another call's open
// question, past its own timeout.
func TestWaitingForAQuestionEndsWithTheContext(t *testing.T) {
	release := make(chan struct{})
	p, _ := New(Config{Ask: func(ctx context.Context, host string) (bool, error) {
		<-release
		return true, nil
	}})
	go func() { _, _ = p.Allowed(context.Background(), "a.test") }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if ok, err := p.Allowed(ctx, "b.test"); ok || err == nil {
		t.Fatalf("%v %v", ok, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waited past the context")
	}
	close(release)
}
