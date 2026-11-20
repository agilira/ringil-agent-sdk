// security_test.go: what hostile text, or a hostile file, could do
//
// THREAT MODEL:
//   - Hostile text in a memory or a question: FTS5 syntax (quotes, NEAR,
//     column filters, *, ^), SQL in disguise, control characters, huge
//     inputs. The text is data: bound as parameters, quoted as FTS terms.
//   - A damaged database: vectors of the wrong length or not finite, rows
//     another program wrote. They are left out, never a panic.
//   - The files: the directory must be the user's alone (no one else can
//     plant a link beside the database), the database never a link.
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sqlite

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Whatever is remembered and asked, the memory answers or refuses the
// input: never an I/O error from text read as syntax, never a panic.
func FuzzPutAndSearch(f *testing.F) {
	for _, s := range []string{`"`, `a" OR "b`, `content: x`, `NEAR(a b)`, `x*`, `^start`, `'); DROP TABLE records; --`, "\x00", "é\u202e", `{a b}`, `-x`, `a AND`, "ﬁ"} {
		f.Add(s, s)
	}
	dir := f.TempDir()
	m, err := New(dir, &conceptEmbedder{model: "concepts"})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = m.Close() })
	f.Fuzz(func(t *testing.T, content, query string) {
		if len(content) > 4096 || len(query) > 4096 {
			return
		}
		ctx := context.Background()
		if _, err := m.Put(ctx, agent.MemoryRecord{Content: content}); err != nil && !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Fatalf("Put %q: %v", content, err)
		}
		if _, err := m.Search(ctx, query, 5); err != nil {
			t.Fatalf("Search %q: %v", query, err)
		}
	})
}

// The query is never FTS syntax: a column filter or a quote in it is a
// word, and the table survives.
func TestSecurityQueryIsText(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "il magazzino apre alle otto")
	for _, q := range []string{`content: magazzino`, `"magazzino" OR "x"`, `magazzino*`, `'); DROP TABLE records; --`, `NEAR(magazzino otto)`} {
		if _, err := m.Search(t.Context(), q, 3); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	if got, _ := m.Search(t.Context(), "", 3); len(got) != 1 {
		t.Fatal("the records are gone")
	}
}

// Damaged vectors are left out: another length, not finite, empty.
func TestSecurityDamagedVectors(t *testing.T) {
	dir := t.TempDir()
	m := openIn(t, dir, &conceptEmbedder{model: "concepts"})
	a := put(t, m, "Riunione con Acme")
	b := put(t, m, "Un incontro con il cliente")
	c := put(t, m, "Incontro col customer")
	raw, err := sql.Open("sqlite", dsn(filepath.Join(dir, File)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	nan := encode([]float32{float32(math.NaN()), 1, 1, 1, 1})
	for id, blob := range map[string][]byte{a.ID: {1, 2, 3}, b.ID: nan, c.ID: {}} {
		if _, err := raw.Exec(`UPDATE vectors SET vec = ? WHERE id = ?`, blob, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Search(t.Context(), "incontro", 5); err != nil {
		t.Fatalf("damaged vectors broke the search: %v", err)
	}
}

func TestSecurityHugeQueryIsBounded(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "una nota")
	q := strings.Repeat("parola ", 20000)
	if _, err := m.Search(t.Context(), q, 3); err != nil {
		t.Fatalf("a huge question: %v", err)
	}
}
