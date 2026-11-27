// proxy.go: the outbound proxy, from ringil.yaml alone
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/agilira/go-errors"
	"golang.org/x/net/http/httpproxy"
)

// proxyConfig is ringil.yaml's proxy: the one every request of ringil goes
// through, never HTTPS_PROXY or ALL_PROXY from the environment (23/11: a
// variable planted there, with a certificate the process trusts, reads
// every key on the wire; the file is the user's, and Cerberus watches it).
// Only https goes through it, tunnelled: the proxy never reads it.
type proxyConfig struct {
	URL string `argus:"url"`
	// NoProxy are hosts reached directly: names (a leading dot or none,
	// subdomains too), addresses, CIDR ranges, as NO_PROXY takes them.
	NoProxy []string `argus:"no_proxy"`
}

func (p proxyConfig) check() error {
	if p.URL == "" && len(p.NoProxy) > 0 {
		return errors.New(ErrUsage, "proxy.no_proxy without proxy.url")
	}
	if p.URL == "" {
		return nil
	}
	return checkProxyURL(p.URL)
}

func checkProxyURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return errors.New(ErrUsage, "proxy.url is an http:// or https:// address")
	case u.User != nil:
		return errors.New(ErrUsage, "proxy.url holds no credentials: ringil.yaml holds no secret")
	case strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "":
		return errors.New(ErrUsage, "proxy.url is the proxy's address alone, without a path or a query")
	}
	return nil
}

// proxyFor is the proxy of a request under the configuration in force: the
// file's, read at each request, so an edit applies to the next one.
func (c *cli) proxyFor(req *http.Request) (*url.URL, error) {
	p := c.conf().Proxy
	if p.URL == "" {
		return nil, nil
	}
	cfg := httpproxy.Config{HTTPSProxy: p.URL, NoProxy: strings.Join(p.NoProxy, ",")}
	return cfg.ProxyFunc()(req.URL)
}

// ownProxy makes every client of this process that uses Go's default
// transport, as ringil's and the SDK's do, take the proxy from ringil.yaml
// and never from the environment. Called once, at the start of main.
func (c *cli) ownProxy() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.Proxy = c.proxyFor
	}
}

// proxiedClient is a client on a transport of its own with ringil.yaml's
// proxy, for libraries that would otherwise use a copy of Go's default
// transport made before main set it (the MCP SDK's OAuth discovery,
// audit 23/11).
func (c *cli) proxiedClient() *http.Client {
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		t := def.Clone()
		t.Proxy = c.proxyFor
		return &http.Client{Transport: t}
	}
	return &http.Client{Transport: &http.Transport{Proxy: c.proxyFor}}
}
