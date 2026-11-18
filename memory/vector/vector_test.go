// vector_test.go: search by meaning with a deterministic embedder
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vector

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

var bg = context.Background()

// conceptEmbedder maps words to concepts, so that synonyms land close:
// a stand-in for a real model that tests can reason about.
type conceptEmbedder struct {
	model  string
	calls  atomic.Int32
	texts  atomic.Int32
	fail   bool
	dims   int // default 5
	maxLen int // texts longer than this are refused, as real models do
}

var concepts = map[string]int{
	"riunione": 0, "meeting": 0, "incontro": 0,
	"cliente": 1, "acme": 1, "customer": 1,
	"fattura": 2, "invoice": 2, "pagamento": 2,
	"gatto": 3, "cat": 3,
}

func (e *conceptEmbedder) Model() string { return e.model }

func (e *conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	e.texts.Add(int32(len(texts)))
	if e.fail {
		return nil, errors.New(agent.ErrEmbed, "down")
	}
	for _, t := range texts {
		if e.maxLen > 0 && len(t) > e.maxLen {
			return nil, errors.New(agent.ErrEmbed, "text too long")
		}
	}
	dims := max(e.dims, 5)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dims)
		v[4] = 0.01
		for _, w := range strings.Fields(strings.ToLower(t)) {
			if c, ok := concepts[strings.Trim(w, ".,:")]; ok {
				v[c]++
			}
		}
		out[i] = v
	}
	return out, nil
}

func newMemory(t *testing.T, e *conceptEmbedder) (agent.Memory, string) {
	t.Helper()
	dir := t.TempDir() + "/mem"
	m, err := New(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func put(t *testing.T, m agent.Memory, text string) {
	t.Helper()
	if _, err := m.Put(bg, agent.MemoryRecord{Content: text}); err != nil {
		t.Fatal(err)
	}
}

func TestFindsByMeaning(t *testing.T) {
	e := &conceptEmbedder{model: "m1"}
	m, _ := newMemory(t, e)
	put(t, m, "Riunione con Acme martedì alle 10")
	put(t, m, "Il gatto si chiama Lira")
	put(t, m, "Fattura 42 da pagare entro venerdì")
	got, err := m.Search(bg, "meeting customer", 1)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Content, "Acme") {
		t.Fatalf("by meaning: %v %v", got, err)
	}
	got, err = m.Search(bg, "invoice", 1) // no shared word with "Fattura"
	if err != nil || !strings.Contains(got[0].Content, "Fattura") {
		t.Fatalf("by meaning: %v %v", got, err)
	}
}

func TestEmbedderDownFallsBackToWords(t *testing.T) {
	e := &conceptEmbedder{model: "m1"}
	m, _ := newMemory(t, e)
	put(t, m, "Riunione con Acme martedì")
	e.fail = true
	put(t, m, "Fattura Acme da pagare")
	got, err := m.Search(bg, "Acme", 5)
	if len(got) != 2 || !errors.HasCode(err, agent.ErrMemoryPartial) {
		t.Fatalf("fallback: %d results, err %v", len(got), err)
	}
	e.fail = false
	m.(*store).resting = time.Time{} // the pause after a failure is over
	if got, err := m.Search(bg, "invoice", 1); err != nil || !strings.Contains(got[0].Content, "Fattura") {
		t.Fatalf("missing vector not rebuilt: %v %v", got, err)
	}
}

func TestModelChangeRebuildsVectors(t *testing.T) {
	e := &conceptEmbedder{model: "m1"}
	m, dir := newMemory(t, e)
	put(t, m, "Riunione con Acme")
	m2, err := New(dir, &conceptEmbedder{model: "m2"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m2.Search(bg, "meeting", 1); err != nil || len(got) != 1 {
		t.Fatalf("after model change: %v %v", got, err)
	}
	v, _, err := m2.(*store).read()
	if err != nil || v.Model != "m2" || len(v.Vectors) != 1 {
		t.Fatalf("vectors %+v %v", v, err)
	}
}

func TestDeleteRemovesVector(t *testing.T) {
	e := &conceptEmbedder{model: "m1"}
	m, _ := newMemory(t, e)
	rec, err := m.Put(bg, agent.MemoryRecord{Content: "Il gatto si chiama Lira"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(bg, rec.ID); err != nil {
		t.Fatal(err)
	}
	v, _, err := m.(*store).read()
	if err != nil || len(v.Vectors) != 0 {
		t.Fatalf("vector kept: %+v %v", v, err)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	in := []float32{0, -1.5, 3.25e-8, 1e30}
	out, ok := decode(encode(in))
	if !ok || len(out) != len(in) || out[1] != -1.5 || out[3] != 1e30 {
		t.Fatalf("%v %v", out, ok)
	}
	if _, ok := decode("not base64!"); ok {
		t.Fatal("garbage decoded")
	}
}
