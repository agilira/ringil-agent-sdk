// metrics_test.go: ringil serve counts its runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// apiCaller calls the API with the token.
func apiCaller(t *testing.T, base, token string) func(method, path, body string) (int, string) {
	return func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
}

// waitDone waits for the run started (its snapshot in started) to end.
func waitDone(call func(method, path, body string) (int, string), started string) {
	var run struct{ ID string }
	_ = json.Unmarshal([]byte(started), &run)
	for range 100 {
		if _, s := call("GET", "/v1/runs/"+run.ID, ""); strings.Contains(s, `"done"`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 22/11: the served runs are counted, per profile, at GET /metrics behind
// the token; /health answers without it.
func TestServeCountsItsRuns(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	probe(t, h.c, []string{"--no-tools"}, func(ctx *orpheus.Context) error {
		srv, _, err := h.c.newServer(ctx)
		if err != nil {
			return err
		}
		defer srv.Close()
		web := httptest.NewServer(srv.Handler())
		defer web.Close()
		token, _, _ := h.c.serveToken(ctx)
		call := apiCaller(t, web.URL, token)
		_, out := call("POST", "/v1/runs", `{"profile":"default","input":"hi"}`)
		waitDone(call, out)
		code, metrics := call("GET", "/metrics", "")
		if code != http.StatusOK || !strings.Contains(metrics, `ringil_runs_finished_total{profile="default",status="done",reason="end"} 1`) || !strings.Contains(metrics, `ringil_model_calls_total{profile="default"} 1`) {
			t.Fatalf("%d %s", code, metrics)
		}
		resp, err := http.Get(web.URL + "/metrics")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("metrics without the token: %d", resp.StatusCode)
		}
		if resp, err = http.Get(web.URL + "/health"); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("health: %v", err)
		}
		_ = resp.Body.Close()
		return nil
	})
}
