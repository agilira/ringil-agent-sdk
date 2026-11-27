// proxy_test.go: the proxy is ringil.yaml's, never the environment's
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"testing"

	"github.com/agilira/orpheus/pkg/orpheus"
)

// 23/11: HTTPS_PROXY in the environment is ignored; ringil.yaml's proxy
// carries https alone, and no_proxy hosts go direct.
func TestTheProxyIsTheFiles(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://planted.example:3128")
	t.Setenv("ALL_PROXY", "http://planted.example:3128")
	saved := http.DefaultTransport.(*http.Transport).Proxy
	t.Cleanup(func() { http.DefaultTransport.(*http.Transport).Proxy = saved })
	h := newHarness(t, "")
	h.c.ownProxy()
	for body, want := range map[string]map[string]string{
		"models: [openai:m]\n": {"https://api.openai.com/v1": ""},
		"proxy: {url: \"http://proxy.corp:3128\", no_proxy: [internal.corp, 10.0.0.0/8]}\n": {
			"https://api.openai.com/v1": "http://proxy.corp:3128", "http://api.openai.com/v1": "",
			"https://llm.internal.corp/v1": "", "https://10.1.2.3/v1": ""},
	} {
		writeConfig(t, h.dir, body)
		h.c.settings = nil
		loaded(t, h.c, nil, func(*orpheus.Context) error {
			for target, proxy := range want {
				req, _ := http.NewRequest(http.MethodGet, target, nil)
				u, err := http.DefaultTransport.(*http.Transport).Proxy(req)
				if got := ""; err != nil || (u == nil) != (proxy == "") || u != nil && u.String() != proxy {
					if u != nil {
						got = u.String()
					}
					t.Errorf("%q %s: %q %v, want %q", body, target, got, err, proxy)
				}
			}
			return nil
		})
	}
}

func TestProxyConfigRefused(t *testing.T) {
	for _, p := range []proxyConfig{
		{URL: "socks5://p:1080"}, {URL: "http://user:pass@p:3128"}, {URL: "http://p:3128/path"},
		{URL: "p:3128"}, {NoProxy: []string{"x"}}, {URL: "http://p:3128?q=1"},
	} {
		if p.check() == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	if (proxyConfig{URL: "https://p.corp:3128/", NoProxy: []string{".corp"}}).check() != nil {
		t.Error("a good proxy refused")
	}
}

// The client given to libraries that copy the default transport early
// takes the file's proxy too.
func TestTheProxiedClient(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://planted.example:3128")
	h := newHarness(t, "")
	writeConfig(t, h.dir, "proxy: {url: \"http://proxy.corp:3128\"}\n")
	loaded(t, h.c, nil, func(*orpheus.Context) error {
		req, _ := http.NewRequest(http.MethodGet, "https://auth.example.com/.well-known/oauth-authorization-server", nil)
		u, err := h.c.proxiedClient().Transport.(*http.Transport).Proxy(req)
		if err != nil || u == nil || u.String() != "http://proxy.corp:3128" {
			t.Errorf("%v %v", u, err)
		}
		return nil
	})
}
