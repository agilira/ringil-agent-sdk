// artifact_test.go: versions, digests, scopes and tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package artifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

var bg = context.Background()

func store(t *testing.T) (*Local, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "artifacts")
	s, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestVersionsAndDigest(t *testing.T) {
	s, dir := store(t)
	a, err := s.Put(bg, "proj", "report.md", Meta{Kind: Markdown, By: "task 1"}, []byte("first"))
	if err != nil || a.Version != 1 {
		t.Fatalf("put %+v %v", a, err)
	}
	if b, err := s.Put(bg, "proj", "report.md", Meta{Kind: Markdown, By: "task 2"}, []byte("second")); err != nil || b.Version != 2 {
		t.Fatalf("second %+v %v", b, err)
	}
	if _, data, err := s.Get(bg, "proj", "report.md", 0); err != nil || string(data) != "second" {
		t.Fatalf("latest %q %v", data, err)
	}
	if _, data, err := s.Get(bg, "proj", "report.md", 1); err != nil || string(data) != "first" {
		t.Fatalf("v1 %q %v", data, err)
	}
	checkTamperAndDelete(t, s, dir, a)
}

// checkTamperAndDelete: a content changed on disk is refused; delete
// removes every version.
func checkTamperAndDelete(t *testing.T, s *Local, dir string, a Info) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "proj", blobDir, a.SHA256), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(bg, "proj", "report.md", 1); !errors.HasCode(err, ErrDamaged) {
		t.Fatalf("tampered read: %v", err)
	}
	if err := s.Delete(bg, "proj", "report.md"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.List(bg, "proj"); err != nil || len(list) != 0 {
		t.Fatalf("after delete %v %v", list, err)
	}
}

func TestBadNamesAndConcurrentPuts(t *testing.T) {
	s, _ := store(t)
	for _, n := range []string{"../x", ".hidden", "a/b", ""} {
		if _, err := s.Put(bg, "p", n, Meta{}, []byte("x")); !errors.HasCode(err, ErrInput) {
			t.Errorf("%q accepted", n)
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(bg, "p", "log.txt", Meta{}, []byte("x")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if info, _, err := s.Get(bg, "p", "log.txt", 0); err != nil || info.Version != 20 {
		t.Fatalf("versions %d %v", info.Version, err)
	}
}

func call(t *testing.T, tool agent.Tool, in any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(bg, raw)
	return out.Content, err
}

func TestToolsRespectScopes(t *testing.T) {
	s, _ := store(t)
	if _, err := s.Put(bg, "project", "spec.md", Meta{Kind: Markdown, Title: "Spec"}, []byte("the spec")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "secret", "keys.txt", Meta{}, []byte("no")); err != nil {
		t.Fatal(err)
	}
	byName := index2(Tools(s, Access{Scope: "task-1", By: "worker", Write: true, Read: []string{"project"}}))
	if out, err := call(t, byName["create_artifact"], createIn{Name: "out.md", Title: "Out", Kind: Markdown, Content: "done"}); err != nil || !strings.Contains(out, "v1") {
		t.Fatalf("create %q %v", out, err)
	}
	if out, err := call(t, byName["read_artifact"], readIn{Name: "spec.md", Scope: "project"}); err != nil || out != "[spec.md v1, markdown, \"Spec\"]\nthe spec" {
		t.Fatalf("read project %q %v", out, err)
	}
	if _, err := call(t, byName["read_artifact"], readIn{Name: "keys.txt", Scope: "secret"}); err == nil {
		t.Fatal("read an ungranted scope")
	}
	if ro := Tools(s, Access{Scope: "x"}); len(ro) != 2 {
		t.Fatalf("writing without the grant: %d tools", len(ro))
	}
}

func index2(ts []agent.Tool) map[string]agent.Tool {
	m := map[string]agent.Tool{}
	for _, tl := range ts {
		m[tl.Spec().Name] = tl
	}
	return m
}

// Audit 18/11: scopes from project keys starting with - or _ were refused;
// the version cap drops the oldest; Delete saves the index before
// removing contents; a damaged content is rewritten by a new publish.
func TestAuditRegressions(t *testing.T) {
	s, dir := store(t)
	s.MaxVersions = 3
	for i := range 5 {
		if _, err := s.Put(bg, "-dotfiles-1a2b", "log.txt", Meta{}, []byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
	}
	info, data, err := s.Get(bg, "-dotfiles-1a2b", "log.txt", 0)
	if err != nil || info.Version != 5 || string(data) != "e" {
		t.Fatalf("latest %+v %q %v", info, data, err)
	}
	if _, _, err := s.Get(bg, "-dotfiles-1a2b", "log.txt", 1); err == nil {
		t.Fatal("version 1 should be gone")
	}
	blobs, err := os.ReadDir(filepath.Join(dir, "-dotfiles-1a2b", blobDir))
	if err != nil || len(blobs) != 3 {
		t.Fatalf("%d contents kept, want 3: %v", len(blobs), err)
	}
}

func TestDamagedContentHealedByPublish(t *testing.T) {
	s, dir := store(t)
	a, err := s.Put(bg, "p", "x.txt", Meta{}, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p", blobDir, a.SHA256), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(bg, "p", "x.txt", Meta{}, []byte("same")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := s.Get(bg, "p", "x.txt", 0); err != nil || string(data) != "same" {
		t.Fatalf("not healed: %q %v", data, err)
	}
}
