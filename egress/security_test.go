// security_test.go: fuzz and SSRF/smuggling security tests for the egress proxy
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
	"testing"
)

func FuzzTargetAndAllow(f *testing.F) {
	f.Add([]byte("CONNECT ok.test:443 HTTP/1.1\r\nHost: ok.test:443\r\n\r\n"), "ok.test")
	f.Add([]byte("GET http://ok.test@evil.com/ HTTP/1.1\r\nHost: ok.test\r\n\r\n"), "*.sub.test")
	f.Add([]byte("GET https://ok.test/ HTTP/1.1\r\nHost: ok.test\r\n\r\n"), "sub.test")
	f.Add([]byte("CONNECT [::1]:443 HTTP/1.1\r\nHost: [::1]:443\r\n\r\n"), "a.sub.test")
	f.Add([]byte("CONNECT bad:port HTTP/1.1\r\nHost: bad:port\r\n\r\n"), "оk.test")
	f.Add([]byte("GET /rel HTTP/1.1\r\nHost: ok.test\r\n\r\n"), "*.com")
	f.Add([]byte("CONNECT :443 HTTP/1.1\r\n\r\n"), strings.Repeat("a", 64)+".test")

	p, err := New(Config{Allow: []string{"ok.test", "*.sub.test"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, rawReq []byte, host string) {
		if len(rawReq) > 4096 || len(host) > 1024 {
			return
		}
		if req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(string(rawReq)))); err == nil {
			h, _, ok := target(req)
			if ok && strings.ContainsAny(h, "[]") {
				t.Fatalf("target returned unstripped brackets: %q", h)
			}
		}
		_, _ = New(Config{Allow: []string{host}})
		checkAllowInvariants(t, p, host)
	})
}

func checkAllowInvariants(t *testing.T, p *Proxy, host string) {
	t.Helper()
	n := normal(host)
	if validName(n) && badNormalHost(n) {
		t.Fatalf("validName accepted invalid host %q", n)
	}
	ok, err := p.Allowed(context.Background(), host)
	if err != nil {
		t.Fatalf("Allowed error without Ask: %v", err)
	}
	if ok && !matchesTestAllow(n) {
		t.Fatalf("Allowed accepted unexpected host %q (normal=%q)", host, n)
	}
}

func badNormalHost(n string) bool { return len(n) > 253 || isIP(n) || !asciiHost(n) }

func matchesTestAllow(n string) bool {
	return validName(n) && (n == "ok.test" || strings.HasSuffix(n, ".sub.test"))
}

func asciiHost(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

var forbiddenPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
}

func FuzzPublicIP(f *testing.F) {
	for _, s := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "224.0.0.1", "240.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd00::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::7f00:1", "64:ff9b:1::1", "2001::1", "2002:7f00:1::1", "2001:db8::1", "100::1",
		"1.1.1.1", "8.8.8.8",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 256 {
			return
		}
		if a, err := netip.ParseAddr(string(raw)); err == nil {
			checkPublicAddr(t, a)
		}
		if len(raw) == 4 {
			checkPublicAddr(t, netip.AddrFrom4([4]byte(raw)))
		}
		if len(raw) == 16 {
			checkPublicAddr(t, netip.AddrFrom16([16]byte(raw)))
		}
	})
}

func checkPublicAddr(t *testing.T, a netip.Addr) {
	t.Helper()
	if !public(a) {
		return
	}
	u := a.Unmap()
	if !isGlobalPublic(u) {
		t.Fatalf("non-global address marked public: %v", a)
	}
	for _, pref := range forbiddenPublicPrefixes {
		if pref.Contains(u) {
			t.Fatalf("reserved prefix %v marked public: %v", pref, a)
		}
	}
}

func isGlobalPublic(u netip.Addr) bool {
	return u.IsGlobalUnicast() && !u.IsPrivate() && !u.IsLoopback() && !u.IsLinkLocalUnicast() && !u.IsMulticast() && !u.IsUnspecified()
}

func TestSecurityNonPublicAddressesRefused(t *testing.T) {
	nonPublicCases := []string{
		"127.0.0.1", "127.255.255.254", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"100.64.0.1", "169.254.169.254", "0.0.0.0", "0.1.2.3",
		"224.0.0.1", "255.255.255.255", "240.0.0.1", "198.18.0.1",
		"192.0.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1",
		"64:ff9b::7f00:1", "64:ff9b:1::1", "2001::1", "2002:7f00:1::1", "2001:db8::1", "100::1",
	}
	for _, s := range nonPublicCases {
		if public(netip.MustParseAddr(s)) {
			t.Errorf("expected %s to be non-public", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if !public(netip.MustParseAddr(s)) {
			t.Errorf("expected %s to be public", s)
		}
	}
}

func TestSecurityMixedDNSOnlyDialsPublicAddress(t *testing.T) {
	var dialed []string
	p, err := New(Config{
		Allow: []string{"mixed.test", "priv.test"},
		Resolver: names{
			"mixed.test": {"127.0.0.1", "169.254.169.254", "93.184.216.34"},
			"priv.test":  {"127.0.0.1", "::1"},
		},
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, status, why := p.open(context.Background(), "mixed.test", 443)
	if conn == nil || status != 0 {
		t.Fatalf("mixed open failed: status=%d why=%q", status, why)
	}
	_ = conn.Close()
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:443" {
		t.Fatalf("dialed=%v, want only public 93.184.216.34:443", dialed)
	}
	dialed = nil
	conn, status, why = p.open(context.Background(), "priv.test", 443)
	if conn != nil || status != http.StatusForbidden || len(dialed) != 0 {
		t.Fatalf("private DNS dialed=%v status=%d why=%q", dialed, status, why)
	}
}

func TestSecurityHopByHopAndURLSmuggling(t *testing.T) {
	var seenHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	sock := start(t, Config{
		Allow:    []string{"ok.test"},
		Ports:    []int{port},
		Private:  true,
		Resolver: names{"ok.test": {"127.0.0.1"}, "evil.test": {"127.0.0.1"}},
	})
	req := "GET http://ok.test:" + portStr + "/x HTTP/1.1\r\n" +
		"Host: ok.test:" + portStr + "\r\n" +
		"Connection: X-Secret-Hop\r\n" +
		"X-Secret-Hop: leak\r\n" +
		"Proxy-Authorization: secret\r\n" +
		"Proxy-Authenticate: x\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"Keep-Alive: timeout=5\r\n" +
		"Te: trailers\r\n" +
		"Upgrade: websocket\r\n\r\n"
	status, _ := ask(t, sock, req)
	if !strings.Contains(status, "200") {
		t.Fatalf("unexpected status: %s", status)
	}
	for _, h := range []string{"X-Secret-Hop", "Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection", "Keep-Alive", "Te", "Upgrade"} {
		if seenHeaders.Get(h) != "" {
			t.Errorf("hop-by-hop header %s leaked upstream", h)
		}
	}

	assertRefusedStatus(t, sock, "GET http://ok.test@evil.test:"+portStr+"/ HTTP/1.1\r\nHost: ok.test\r\n\r\n", "403")
	assertRefusedStatus(t, sock, "GET /secret HTTP/1.1\r\nHost: ok.test\r\n\r\n", "400")
	assertRefusedStatus(t, sock, "GET ftp://ok.test/ HTTP/1.1\r\nHost: ok.test\r\n\r\n", "400")
	assertRefusedStatus(t, sock, "CONNECT :443 HTTP/1.1\r\nHost: :443\r\n\r\n", "400")
	assertRefusedStatus(t, sock, "CONNECT ok.test:0 HTTP/1.1\r\nHost: ok.test:0\r\n\r\n", "403")
	assertRefusedStatus(t, sock, "CONNECT ok.test:99999 HTTP/1.1\r\nHost: ok.test:99999\r\n\r\n", "403")
	if status, _ := ask(t, sock, "CONNECT ok.test:abc HTTP/1.1\r\nHost: ok.test:abc\r\n\r\n"); status != "" && !strings.Contains(status, "400") {
		t.Errorf("non-numeric port status=%q, want closed or 400", status)
	}
}

func assertRefusedStatus(t *testing.T, sock, raw, wantCode string) {
	t.Helper()
	status, _ := ask(t, sock, raw)
	if !strings.Contains(status, wantCode) {
		t.Errorf("%q -> %q, want %s", raw, status, wantCode)
	}
}
