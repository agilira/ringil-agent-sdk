// audit_test.go: what the audit of 26/11 found in artifacts
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package artifact

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agilira/go-errors"
)

// A change made on a version that is no longer the latest is refused,
// and of two creates of one name, one wins.
func TestNoVersionLost(t *testing.T) {
	s, _ := store(t)
	if _, err := s.Put(bg, "c", "r.md", Meta{Kind: Markdown, Base: NoBase}, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "c", "r.md", Meta{Kind: Markdown, Base: 1}, []byte("v2 by one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "c", "r.md", Meta{Kind: Markdown, Base: 1}, []byte("v2 by another")); !errors.HasCode(err, ErrConflict) {
		t.Fatalf("a stale change: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(bg, "c", "new.md", Meta{Base: NoBase}, []byte("x")); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d creates won", won)
	}
}

// A run's scope is its only one: no Read scope, and none at all when the
// run has no conversation.
func TestRunScopeIsTheOnlyOne(t *testing.T) {
	s, _ := store(t)
	if _, err := s.Put(bg, "project", "p.md", Meta{}, []byte("terminal's")); err != nil {
		t.Fatal(err)
	}
	ts := index2(Tools(s, Access{Scope: "project", Write: true, Read: []string{"project"}}))
	if _, err := callCtx(t, WithScope(bg, "conv"), ts["read_artifact"], readIn{Name: "p.md", Scope: "project"}); err == nil {
		t.Fatal("a conversation read the terminal's scope")
	}
	if _, err := callCtx(t, WithScope(bg, ""), ts["list_artifacts"], listIn{}); !errors.HasCode(err, ErrInput) {
		t.Fatalf("a run without a conversation: %v", err)
	}
}

// Reads of a scope never made leave nothing; a scope deleted takes new
// versions afterwards.
func TestReadsMakeNothing(t *testing.T) {
	s, dir := store(t)
	if _, _, err := s.Get(bg, "ghost", "x", 0); !errors.HasCode(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := s.Delete(bg, "ghost", "x"); !errors.HasCode(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "ghost")); !os.IsNotExist(err) {
		t.Fatal("a read made the scope")
	}
	if _, err := s.Put(bg, "c", "a", Meta{}, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteScope(bg, "c"); err != nil {
		t.Fatal(err)
	}
	if info, err := s.Put(bg, "c", "a", Meta{Base: NoBase}, []byte("new")); err != nil || info.Version != 1 {
		t.Fatalf("after the delete: %+v %v", info, err)
	}
}

// A rewrite made on the version the model read is refused when someone
// else made a newer one meanwhile.
func TestRewriteOnTheVersionRead(t *testing.T) {
	s, _ := store(t)
	ctx := WithScope(bg, "c")
	ts := index2(Tools(s, Access{Write: true}))
	if _, err := callCtx(t, ctx, ts["create_artifact"], createIn{Name: "r.md", Kind: Markdown, Content: "agent's"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "c", "r.md", Meta{Kind: Markdown, By: "worker", Base: 1}, []byte("worker's")); err != nil {
		t.Fatal(err)
	}
	if _, err := callCtx(t, ctx, ts["rewrite_artifact"], rewriteIn{Name: "r.md", Version: 1, Content: "over it"}); !errors.HasCode(err, ErrConflict) {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := callCtx(t, ctx, ts["edit_artifact"], editIn{Name: "r.md", Version: 1, Edits: []edit{{Old: "worker", New: "agent"}}}); !errors.HasCode(err, ErrConflict) {
		t.Fatalf("edit: %v", err)
	}
	if _, err := callCtx(t, ctx, ts["rewrite_artifact"], rewriteIn{Name: "r.md", Version: 2, Content: "seen it"}); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedIndexRejected(t *testing.T) {
	s, dir := store(t)
	scopeDir := filepath.Join(dir, "c")
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(scopeDir, indexFile)
	if err := os.WriteFile(idxPath, []byte(`{"version":1,"artifacts":{"a":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(bg, "c"); !errors.HasCode(err, ErrDamaged) {
		t.Fatalf("empty versions slice: %v", err)
	}
	badDigest := `{"version":1,"artifacts":{"a":[{"name":"a","version":1,"sha256":"../index.json"}]}}`
	if err := os.WriteFile(idxPath, []byte(badDigest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(bg, "c", "a"); !errors.HasCode(err, ErrDamaged) {
		t.Fatalf("bad digest delete: %v", err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("index.json touched: %v", err)
	}
}
