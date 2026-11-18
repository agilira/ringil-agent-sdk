// embed_test.go: the /embeddings client
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// embedServer answers with vectors [index, len(text)] in reverse order,
// to check that the client puts them back in input order.
func embedServer(t *testing.T, reply func(n int) string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Model != "emb" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if reply != nil {
			write(t, w, reply(len(req.Input)))
			return
		}
		var data []string
		for i := len(req.Input) - 1; i >= 0; i-- {
			data = append(data, fmt.Sprintf(`{"index":%d,"embedding":[%d,%d]}`, i, i, len(req.Input[i])))
		}
		write(t, w, `{"data":[`+strings.Join(data, ",")+`]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestEmbedOrderAndBatches(t *testing.T) {
	srv, requests := embedServer(t, nil)
	e, err := NewEmbedder(Config{BaseURL: srv.URL + "/v1", Model: "emb", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 70)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil || len(vecs) != 70 || vecs[69][1] != 70 || vecs[0][1] != 1 || requests.Load() != 2 {
		t.Fatalf("vecs %d err %v requests %d", len(vecs), err, requests.Load())
	}
	if e.Model() != "emb" {
		t.Fatalf("model %s", e.Model())
	}
}

func TestEmbedRejectsMismatchedReplies(t *testing.T) {
	for name, reply := range map[string]func(int) string{
		"missing":   func(int) string { return `{"data":[{"index":0,"embedding":[1]}]}` },
		"duplicate": func(int) string { return `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}` },
		"outside":   func(int) string { return `{"data":[{"index":0,"embedding":[1]},{"index":7,"embedding":[2]}]}` },
		"dims":      func(int) string { return `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2,3]}]}` },
		"garbage":   func(int) string { return `not json` },
		"trailing":  func(int) string { return `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]}]} junk` },
		"huge dims": func(int) string {
			return `{"data":[{"index":0,"embedding":[` + strings.Repeat("0,", 20000) + `0]},{"index":1,"embedding":[1]}]}`
		},
	} {
		srv, _ := embedServer(t, reply)
		e, err := NewEmbedder(Config{BaseURL: srv.URL + "/v1", Model: "emb", APIKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Embed(context.Background(), []string{"a", "b"}); !errors.HasCode(err, agent.ErrEmbed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func write(t *testing.T, w http.ResponseWriter, body string) {
	if _, err := fmt.Fprint(w, body); err != nil {
		t.Error(err)
	}
}

// Asymmetric models: stored texts go as passages, searches as queries.
func TestEmbedInputType(t *testing.T) {
	var kinds []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		kinds = append(kinds, req["input_type"])
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	for _, on := range []bool{false, true} {
		e, err := NewEmbedder(Config{BaseURL: srv.URL, Model: "m", AllowHTTP: true, EmbedInputType: on})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Embed(context.Background(), []string{"doc"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.EmbedQuery(context.Background(), []string{"q"}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(kinds) != "[<nil> <nil> passage query]" {
		t.Fatalf("input_type sent: %v", kinds)
	}
}
