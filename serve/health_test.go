// health_test.go: /health, the one path without the token
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"io"
	"net/http"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func get(t *testing.T, url string, headers ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(resp.Body)
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// 22/11: a supervisor learns whether the server answers, without the
// token, and nothing more; once closing, 503.
func TestHealth(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "p", agent.Config{Provider: fake.New(fake.Reply("x"))})}})
	if code, body := get(t, a.url+"/health"); code != http.StatusOK || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := get(t, a.url+"/v1/profiles"); code != http.StatusUnauthorized {
		t.Fatalf("the API without the token: %d", code)
	}
	a.srv.Close()
	if code, body := get(t, a.url+"/health"); code != http.StatusServiceUnavailable || !strings.Contains(body, "closing") {
		t.Fatalf("closing: %d %s", code, body)
	}
}

// /health keeps the guard's other rules: the loopback name, no page.
func TestSecurityHealthStillGuarded(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "p", agent.Config{Provider: fake.New(fake.Reply("x"))})}})
	if code, _ := get(t, a.url+"/health", "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("from a page: %d", code)
	}
	req, _ := http.NewRequest("GET", a.url+"/health", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeQuietly(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound name: %d", resp.StatusCode)
	}
	checkHealthMethods(t, a.url)
	// Nothing else is open: /health/ or /healthz go through the guard.
	for _, path := range []string{"/health/", "/healthz", "/health/../v1/profiles", "//health", "/%68ealth/x"} {
		if code, _ := get(t, a.url+path); code != http.StatusUnauthorized && code != http.StatusNotFound {
			t.Errorf("%s: %d", path, code)
		}
	}
}

// checkHealthMethods: GET and HEAD only; a bad origin still fails with 403 first.
func checkHealthMethods(t *testing.T, url string) {
	t.Helper()
	for method, want := range map[string]int{"POST": http.StatusMethodNotAllowed, "HEAD": http.StatusOK} {
		req, _ := http.NewRequest(method, url+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		closeQuietly(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s: %d", method, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", url+"/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeQuietly(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST from a page: %d", resp.StatusCode)
	}
}
