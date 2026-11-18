// audit_test.go: regressions for the memory audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Finding 3: search was quadratic; with a common word it took minutes.
func TestSearchIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 20k records")
	}
	recs := make([]agent.MemoryRecord, 20000)
	for i := range recs {
		recs[i] = agent.MemoryRecord{ID: fmt.Sprint(i), Content: fmt.Sprintf("the note number %d about the project and the client %d", i, i%97)}
	}
	start := time.Now()
	got := rank(recs, strings.Repeat("the project client note ", 75), 5)
	if len(got) != 5 || time.Since(start) > 3*time.Second {
		t.Fatalf("%d results in %v", len(got), time.Since(start))
	}
}

// Finding 6: duplicate IDs in the file, IDs with control characters.
func TestFileRecordsAreChecked(t *testing.T) {
	m, dir := newStore(t)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, dataFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":1,"records":[{"id":"a","content":"vecchio","created":"2026-01-01T00:00:00Z"},{"id":"a","content":"nuovo","created":"2026-01-01T00:00:00Z"}]}`)
	got, err := m.Search(bg, "", 10)
	if err != nil || ids(got) != "nuovo" {
		t.Fatalf("duplicates: %s %v", ids(got), err)
	}
	write(`{"version":1,"records":[{"id":"x]\r\u001b","content":"c","created":"2026-01-01T00:00:00Z"}]}`)
	if _, err := m.Search(bg, "", 10); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("bad id accepted: %v", err)
	}
}

func TestIDsAreRestricted(t *testing.T) {
	m, _ := newStore(t)
	for _, id := range []string{"x]\r", "a\x1b", "<b>", "a b"} {
		if _, err := m.Put(bg, agent.MemoryRecord{ID: id, Content: "c"}); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("%q accepted: %v", id, err)
		}
	}
}

// Repeating the same fact does not grow the memory.
func TestSameFactStoredOnce(t *testing.T) {
	m, _ := newStore(t)
	a := put(t, m, "il cliente è Acme")
	b := put(t, m, "il cliente è Acme")
	got, err := m.Search(bg, "", 10)
	if err != nil || a.ID != b.ID || len(got) != 1 {
		t.Fatalf("ids %s %s, %d records %v", a.ID, b.ID, len(got), err)
	}
}
