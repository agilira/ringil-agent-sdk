// local_test.go: tests for the local memory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package local

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

var bg = context.Background()

func newStore(t *testing.T) (agent.Memory, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	m, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func put(t *testing.T, m agent.Memory, content string) agent.MemoryRecord {
	t.Helper()
	rec, err := m.Put(bg, agent.MemoryRecord{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func ids(recs []agent.MemoryRecord) string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Content)
	}
	return strings.Join(out, " | ")
}

func TestPutSearchDeleteAcrossInstances(t *testing.T) {
	m, dir := newStore(t)
	a := put(t, m, "Il progetto ORIONE scade il 12 marzo")
	put(t, m, "Antonio preferisce Go e poche dipendenze")
	again, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Search(bg, "quando scade orione?", 5)
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("search %s %v", ids(got), err)
	}
	if err := again.Delete(bg, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Search(bg, "orione", 5); err != nil || len(got) != 0 {
		t.Fatalf("after delete %s %v", ids(got), err)
	}
	if err := m.Delete(bg, a.ID); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("second delete %v", err)
	}
}

func TestRankingPrefersRareAndRepeatedTerms(t *testing.T) {
	m, _ := newStore(t)
	put(t, m, "riunione con il cliente lunedì")
	put(t, m, "il budget del cliente è 42000 euro")
	put(t, m, "budget budget: approvato il budget del marketing")
	got, err := m.Search(bg, "budget marketing", 3)
	if err != nil || len(got) != 2 || !strings.Contains(got[0].Content, "marketing") {
		t.Fatalf("ranking %s %v", ids(got), err)
	}
}

func TestEmptyQueryListsMostRecent(t *testing.T) {
	m, _ := newStore(t)
	s := m.(*store)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Hour); return clock }
	for _, c := range []string{"primo", "secondo", "terzo"} {
		put(t, m, c)
	}
	got, err := m.Search(bg, "", 2)
	if err != nil || ids(got) != "terzo | secondo" {
		t.Fatalf("recent %s %v", ids(got), err)
	}
	if got, err := m.Search(bg, "x", 0); err != nil || len(got) != 0 {
		t.Fatalf("limit 0: %s %v", ids(got), err)
	}
}

func TestUpdateKeepsCreation(t *testing.T) {
	m, _ := newStore(t)
	rec := put(t, m, "versione uno")
	updated, err := m.Put(bg, agent.MemoryRecord{ID: rec.ID, Content: "versione due"})
	if err != nil || !updated.Created.Equal(rec.Created) {
		t.Fatalf("update %+v %v", updated, err)
	}
	if got, err := m.Search(bg, "", 10); err != nil || ids(got) != "versione due" {
		t.Fatalf("after update %s %v", ids(got), err)
	}
}

func TestInvalidRecords(t *testing.T) {
	m, _ := newStore(t)
	for _, rec := range []agent.MemoryRecord{
		{Content: "  "}, {Content: strings.Repeat("a", agent.MaxMemoryContent+1)}, {Content: "a\x00b"}, {Content: "\xff"},
		{ID: "has space", Content: "x"}, {ID: "a/b", Content: "x"}, {ID: strings.Repeat("i", 65), Content: "x"},
	} {
		if _, err := m.Put(bg, rec); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("%q accepted: %v", rec.ID+rec.Content[:min(len(rec.Content), 10)], err)
		}
	}
}

func TestConcurrentWritersLoseNothing(t *testing.T) {
	_, dir := newStore(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := New(dir) // separate instances, as separate processes
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := m.Put(bg, agent.MemoryRecord{Content: "fatto numero " + string(rune('a'+i))}); err != nil {
				var coded *errors.Error
				if stderrors.As(err, &coded) {
					t.Errorf("%v %v", err, coded.Context)
					return
				}
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m2.Search(bg, "", 100); err != nil || len(got) != 20 {
		t.Fatalf("%d records %v", len(got), err)
	}
}

func TestDamagedFileIsReported(t *testing.T) {
	m, dir := newStore(t)
	if err := os.WriteFile(filepath.Join(dir, dataFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Search(bg, "x", 1); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("err %v", err)
	}
}

func TestSharedDirectoryRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ACLs")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("err %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	m, _ := newStore(t)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := m.Put(ctx, agent.MemoryRecord{Content: "x"}); err == nil {
		t.Fatal("put")
	}
}

func TestTokens(t *testing.T) {
	if got := strings.Join(tokens("Perché il TEST-42, è OK?"), ","); got != "perché,il,test,42,è,ok" {
		t.Fatalf("tokens %s", got)
	}
}

// An accent written as one character finds it written as two, and back.
func TestEitherFormOfAnAccent(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(t.Context(), agent.MemoryRecord{Content: "non so perché"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"perché", "perché"} {
		if got, _ := m.Search(t.Context(), q, 3); len(got) != 1 {
			t.Errorf("%q: %v", q, got)
		}
	}
}
