// live_test.go: the sandboxed network against the real internet
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package bash

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestLiveNetwork: an allowed domain answers over HTTPS through the proxy,
// another is asked about once and refused, and the host's own services
// stay out of reach even by name.
func TestLiveNetwork(t *testing.T) {
	box := boxOrSkip(t)
	var mu sync.Mutex
	var asked []string
	tl := newBash(t, Config{Sandbox: box, Read: []string{}, Network: Network{
		Allow: []string{"example.com"},
		Ask: func(_ context.Context, host string) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, host)
			return host == "localhost", nil // even a yes cannot reach the host
		},
	}})
	out, err := call(t, tl, map[string]any{"command": `curl -s -o /dev/null -w "%{http_code}\n" https://example.com/
curl -s -w "\n%{http_code}\n" https://www.iana.org/ | tail -2
curl -s https://www.iana.org/ | tail -1
curl -s http://localhost/`})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out)
	for _, want := range []string{"200\n", "[Sandbox: the network refused: the user did not allow the domain www.iana.org", "localhost resolves to a private or local address"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(asked) != 2 || asked[0] != "www.iana.org" || asked[1] != "localhost" {
		t.Errorf("asked %v", asked)
	}
}
