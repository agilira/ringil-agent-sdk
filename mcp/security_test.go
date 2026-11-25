// security_test.go: fuzz and boundary tests for what the agent may add
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"net"
	"net/url"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Whatever passes Check shows in full in an approval and keeps no key in
// a header: every value prints, names follow their rules, a URL is https
// or loopback, a header is only a reference.
func FuzzEntryCheck(f *testing.F) {
	f.Add("github", "npx", "-y @org/srv", "", "GITHUB_TOKEN", "${GITHUB_TOKEN}", "")
	f.Add("r", "", "", "https://x.example.com/mcp", "", "", "Bearer ${T}")
	f.Add("r", "", "", "http://169.254.169.254/", "", "", "Bearer sk-1")
	f.Add("a", "np\u200bx", "a\u202eb", "", "A-B", "x\ny", "")
	f.Fuzz(func(t *testing.T, name, prog, args, u, envKey, envVal, header string) {
		e := Entry{Name: name, URL: u}
		if prog != "" {
			e.Command = append([]string{prog}, strings.Fields(args)...)
		}
		if envKey != "" {
			e.Env = map[string]string{envKey: envVal}
		}
		if header != "" {
			e.Headers = map[string]string{"Authorization": header}
		}
		if e.Check() == nil {
			checkAccepted(t, e)
		}
	})
}

func checkAccepted(t *testing.T, e Entry) {
	t.Helper()
	all := append(append([]string{e.Name, e.URL, e.Source}, e.Command...), values(e.Env)...)
	for _, s := range append(all, values(e.Headers)...) {
		if !shows(s) {
			t.Fatalf("accepted a value that does not print: %q", s)
		}
	}
	if e.URL != "" && !plainOrLoopback(e.URL) {
		t.Fatalf("accepted URL %q", e.URL)
	}
	for _, v := range e.Headers {
		if !headerRef.MatchString(v) {
			t.Fatalf("accepted header value %q", v)
		}
	}
}

func shows(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// plainOrLoopback: https, or http to a loopback address.
func plainOrLoopback(raw string) bool {
	p, err := url.Parse(raw)
	if err != nil || p.User != nil {
		return false
	}
	ip := net.ParseIP(p.Hostname())
	return p.Scheme == "https" || (ip != nil && ip.IsLoopback())
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Loopback over http is the only plain address: no private network, no
// cloud metadata, no host that merely looks local.
func TestEntryURLBoundaries(t *testing.T) {
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data", "http://10.0.0.1/mcp", "http://localhost.evil.com/mcp",
		"http://[::ffff:127.0.0.1]:80@evil.com/", "file:///etc/passwd", "javascript:alert(1)", "https://",
	} {
		if err := (Entry{Name: "a", URL: u}).Check(); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
