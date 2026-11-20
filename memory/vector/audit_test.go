// audit_test.go: regressions for the vector memory audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vector

import (
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/memory/internal/embedgate"
)

// Finding 1: one record too long for the model disabled meaning search.
func TestLongRecordIsClipped(t *testing.T) {
	e := &conceptEmbedder{model: "m", maxLen: embedgate.MaxChars}
	m, _ := newMemory(t, e)
	put(t, m, "Fattura 42 da pagare "+strings.Repeat("dettagli ", 3000))
	put(t, m, "Riunione con Acme martedì")
	if got, err := m.Search(bg, "meeting", 1); err != nil || !strings.Contains(got[0].Content, "Acme") {
		t.Fatalf("search %v %v", got, err)
	}
}

// Finding 2: a down embedder was called again on every search.
func TestDownEmbedderRests(t *testing.T) {
	e := &conceptEmbedder{model: "m", fail: true}
	m, _ := newMemory(t, e)
	for range 20 {
		put(t, m, "fatto "+strings.Repeat("x", 3))
	}
	for range 3 {
		if _, err := m.Search(bg, "fatto", 5); err == nil {
			t.Fatal("no partial error while down")
		}
	}
	if n := e.calls.Load(); n > 2 {
		t.Fatalf("%d calls to a down embedder", n)
	}
}

// Finding 3: same model name, other length: vectors were compared anyway.
func TestSameNameOtherDimsRebuilds(t *testing.T) {
	m, dir := newMemory(t, &conceptEmbedder{model: "m"})
	put(t, m, "Riunione con Acme")
	m2, err := New(dir, &conceptEmbedder{model: "m", dims: 8})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m2.Search(bg, "meeting", 1); err != nil || len(got) != 1 {
		t.Fatalf("after dims change %v %v", got, err)
	}
	if v, _, err := m2.(*store).read(); err != nil || v.Dims != 8 {
		t.Fatalf("vectors not rebuilt: %+v %v", v, err)
	}
}

// Finding 4: an upsert whose embedding failed kept the old vector.
func TestStaleVectorAfterUpsert(t *testing.T) {
	e := &conceptEmbedder{model: "m"}
	m, _ := newMemory(t, e)
	rec, err := m.Put(bg, agent.MemoryRecord{ID: "x", Content: "Il gatto si chiama Lira"})
	if err != nil {
		t.Fatal(err)
	}
	e.fail = true
	if _, err := m.Put(bg, agent.MemoryRecord{ID: rec.ID, Content: "Fattura 42 da pagare"}); err != nil {
		t.Fatal(err)
	}
	e.fail = false
	m.(*store).gate.Wake()
	if got, err := m.Search(bg, "cat", 1); err != nil || len(got) != 0 {
		t.Fatalf("stale vector used: %v %v", got, err)
	}
}

// Finding 11: an unrelated question recalled every record.
func TestUnrelatedQueryRecallsNothing(t *testing.T) {
	m, _ := newMemory(t, &conceptEmbedder{model: "m"})
	put(t, m, "Riunione con Acme")
	put(t, m, "Fattura 42 da pagare")
	if got, err := m.Search(bg, "come stai oggi", 3); err != nil || len(got) != 0 {
		t.Fatalf("unrelated: %v %v", got, err)
	}
}

// Finding 10: vectors of forgotten records stayed in the file.
func TestOrphanVectorsArePruned(t *testing.T) {
	e := &conceptEmbedder{model: "m"}
	m, _ := newMemory(t, e)
	keep, err := m.Put(bg, agent.MemoryRecord{Content: "Riunione con Acme"})
	if err != nil {
		t.Fatal(err)
	}
	s := m.(*store)
	if err := s.update(bg, 0, func(v *vectors) { v.Vectors["ghost"] = v.Vectors[keep.ID] }); err != nil {
		t.Fatal(err)
	}
	put(t, m, "Fattura 42") // a new record makes the next search write
	e.fail = true
	put(t, m, "Il gatto Lira")
	e.fail = false
	s.gate.Wake()
	if _, err := m.Search(bg, "meeting", 1); err != nil {
		t.Fatal(err)
	}
	if v, _, err := s.read(); err != nil || v.Vectors["ghost"].Vec != "" {
		t.Fatalf("orphan kept: %v", err)
	}
}
