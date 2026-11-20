// embed_test.go: Ollama's /api/embed, against a fake server or the real one
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package ollama

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type seenRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Truncate   bool     `json:"truncate"`
	Dimensions int      `json:"dimensions"`
}

// fakeOllama embeds each input as [len, first byte], remembering what it got.
func fakeOllama(t *testing.T, seen *[]seenRequest) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req seenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		*seen = append(*seen, req)
		if req.Model != "emb" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model '` + req.Model + `' not found"}`))
			return
		}
		out := make([][]float32, len(req.Input))
		for i, s := range req.Input {
			out[i] = []float32{float32(len(s)), float32(s[0])}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "embeddings": out})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestEmbedInOrderAndInBatches(t *testing.T) {
	var seen []seenRequest
	e, err := NewEmbedder(Config{URL: fakeOllama(t, &seen), Model: "emb"})
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, batch+3)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	vecs, err := e.Embed(t.Context(), texts)
	if err != nil || len(vecs) != len(texts) {
		t.Fatalf("%d vectors, %v", len(vecs), err)
	}
	if vecs[0][0] != 1 || vecs[len(texts)-1][0] != float32(len(texts)) {
		t.Fatal("the vectors are not in the order of the texts")
	}
	checkBatches(t, seen)
}

func checkBatches(t *testing.T, seen []seenRequest) {
	t.Helper()
	if len(seen) != 2 || len(seen[0].Input) != batch || !seen[0].Truncate {
		t.Fatalf("requests %d, first %d inputs, truncate %v", len(seen), len(seen[0].Input), seen[0].Truncate)
	}
}

func TestPrefixesAndDimensions(t *testing.T) {
	var seen []seenRequest
	e, err := NewEmbedder(Config{URL: fakeOllama(t, &seen), Model: "emb", QueryPrefix: "search_query: ", DocumentPrefix: "search_document: ", Dimensions: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(t.Context(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EmbedQuery(t.Context(), []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if seen[0].Input[0] != "search_document: a" || seen[1].Input[0] != "search_query: b" || seen[0].Dimensions != 256 {
		t.Fatalf("%+v", seen)
	}
	if e.Model() != "emb#search_document: @256" {
		t.Fatalf("model %q: another prefix or size makes other vectors", e.Model())
	}
	plain, _ := NewEmbedder(Config{URL: "http://127.0.0.1:1", Model: "emb"})
	if plain.Model() != "emb" {
		t.Fatalf("plain model %q", plain.Model())
	}
}

func TestRefusals(t *testing.T) {
	var seen []seenRequest
	url := fakeOllama(t, &seen)
	e, _ := NewEmbedder(Config{URL: url, Model: "missing"})
	_, err := e.Embed(t.Context(), []string{"a"})
	if !errors.HasCode(err, agent.ErrEmbed) || !strings.Contains(err.Error(), "not found") || len(seen) != 1 {
		t.Fatalf("a missing model: %v (%d calls: a refusal is not retried)", err, len(seen))
	}
	for name, cfg := range map[string]Config{
		"no model":       {URL: url},
		"http not local": {URL: "http://ollama.example.com", Model: "m"},
		"dimensions":     {URL: url, Model: "m", Dimensions: -1},
	} {
		if _, err := NewEmbedder(cfg); !errors.HasCode(err, agent.ErrProviderConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if e, err := NewEmbedder(Config{Model: "m"}); err != nil || e.(*embedder).endpoint != DefaultURL+"/api/embed" {
		t.Errorf("default URL: %v", err)
	}
}

func TestAServerErrorIsTriedAgain(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"embeddings":[[1,2]]}`))
	}))
	defer srv.Close()
	e, _ := NewEmbedder(Config{URL: srv.URL, Model: "m"})
	if vecs, err := e.Embed(t.Context(), []string{"a"}); err != nil || len(vecs) != 1 || calls.Load() != 2 {
		t.Fatalf("%v %d calls", err, calls.Load())
	}
}

// RINGIL_OLLAMA_EMBED_MODEL (and RINGIL_OLLAMA_URL) run it against a real
// server: free, local.
func TestLiveOllama(t *testing.T) {
	model := os.Getenv("RINGIL_OLLAMA_EMBED_MODEL")
	if model == "" {
		t.Skip("RINGIL_OLLAMA_EMBED_MODEL is not set")
	}
	e, err := NewEmbedder(Config{URL: os.Getenv("RINGIL_OLLAMA_URL"), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := e.Embed(t.Context(), []string{"Riunione con il cliente Acme martedì", "Il gatto dorme sul divano"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.EmbedQuery(t.Context(), []string{"quando vedo il cliente?"})
	if err != nil {
		t.Fatal(err)
	}
	near, far := cosine(q[0], vecs[0]), cosine(q[0], vecs[1])
	t.Logf("%s: %d dims, related %.3f, unrelated %.3f", model, len(q[0]), near, far)
	if near <= far {
		t.Fatalf("the related text is not nearer: %.3f <= %.3f", near, far)
	}
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot, na, nb = dot+float64(a[i])*float64(b[i]), na+float64(a[i])*float64(a[i]), nb+float64(b[i])*float64(b[i])
	}
	return dot / math.Sqrt(na*nb)
}

// Audit 20/11: a model that cannot embed (501) is not asked twice.
func TestAModelThatCannotEmbedIsAskedOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"this model does not support embeddings"}`))
	}))
	defer srv.Close()
	e, _ := NewEmbedder(Config{URL: srv.URL, Model: "chat-model"})
	if _, err := e.Embed(t.Context(), []string{"a"}); !errors.HasCode(err, agent.ErrEmbed) || calls.Load() != 1 {
		t.Fatalf("%v, %d calls", err, calls.Load())
	}
}
