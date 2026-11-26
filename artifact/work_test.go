// work_test.go: making, editing and rewriting artifacts, as an app sees them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package artifact

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func callCtx(t *testing.T, ctx context.Context, tool agent.Tool, in any) (agent.ToolOutput, error) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call(ctx, raw)
}

// The run's scope comes from its context; each version is announced.
func TestCreateEditRewrite(t *testing.T) {
	s, _ := store(t)
	var mu sync.Mutex
	var seen []Info
	ctx := WithPublished(WithScope(bg, "conv-1"), func(i Info) { mu.Lock(); seen = append(seen, i); mu.Unlock() })
	ts := index2(Tools(s, Access{Scope: "fallback", Write: true}))
	steps := []struct {
		tool string
		in   any
		code errors.ErrorCode
	}{
		{"create_artifact", createIn{Name: "page", Title: "Landing", Kind: HTML, Content: "<h1>Hi</h1><p>old</p>"}, ""},
		{"create_artifact", createIn{Name: "page", Title: "Again", Kind: HTML, Content: "x"}, ErrExists},
		{"edit_artifact", editIn{Name: "page", Edits: []edit{{Old: "old", New: "new"}, {Old: "Hi", New: "Hello"}}}, ""},
		{"rewrite_artifact", rewriteIn{Name: "page", Content: "<p>whole</p>", Title: "Landing v3"}, ""},
	}
	for _, st := range steps {
		if _, err := callCtx(t, ctx, ts[st.tool], st.in); (st.code == "" && err != nil) || (st.code != "" && !errors.HasCode(err, st.code)) {
			t.Fatalf("%s: %v", st.tool, err)
		}
	}
	checkThreeVersions(t, s)
	if len(seen) != 3 || seen[2].Version != 3 {
		t.Fatalf("announced %+v", seen)
	}
	if list, _ := s.List(bg, "fallback"); len(list) != 0 {
		t.Fatal("written to the fallback scope over the run's")
	}
}

func checkThreeVersions(t *testing.T, s *Local) {
	t.Helper()
	versions, err := s.Versions(bg, "conv-1", "page")
	if err != nil || len(versions) != 3 {
		t.Fatalf("%d versions %v", len(versions), err)
	}
	_, v2, _ := s.Get(bg, "conv-1", "page", 2)
	if string(v2) != "<h1>Hello</h1><p>new</p>" || versions[2].Title != "Landing v3" || versions[2].Kind != HTML || versions[2].MediaType != "text/html" {
		t.Fatalf("v2 %q, v3 %+v", v2, versions[2])
	}
}

// Edits are all or none: one that does not match, or matches twice,
// changes nothing.
func TestEditsAllOrNone(t *testing.T) {
	s, _ := store(t)
	ctx := WithScope(bg, "c")
	ts := index2(Tools(s, Access{Write: true}))
	if _, err := callCtx(t, ctx, ts["create_artifact"], createIn{Name: "a.go", Kind: Code, Language: "go", Content: "x := 1\nx := 1\ny := 2\n"}); err != nil {
		t.Fatal(err)
	}
	for name, edits := range map[string][]edit{
		"twice":   {{Old: "y := 2", New: "y := 3"}, {Old: "x := 1", New: "x := 9"}},
		"missing": {{Old: "y := 2", New: "y := 3"}, {Old: "z", New: "w"}},
		"empty":   {{Old: "", New: "w"}},
	} {
		if _, err := callCtx(t, ctx, ts["edit_artifact"], editIn{Name: "a.go", Edits: edits}); !errors.HasCode(err, ErrInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if info, data, _ := s.Get(bg, "c", "a.go", 0); info.Version != 1 || !strings.Contains(string(data), "y := 2") || info.Language != "go" {
		t.Fatalf("changed: %+v %q", info, data)
	}
}

// Without a scope, from the run or the host, there are no artifacts.
func TestNoScope(t *testing.T) {
	s, _ := store(t)
	ts := index2(Tools(s, Access{Write: true}))
	if _, err := callCtx(t, bg, ts["list_artifacts"], listIn{}); !errors.HasCode(err, ErrInput) {
		t.Fatalf("%v", err)
	}
}

// A file the agent made is saved as a file artifact; an image is shown to
// the model as itself.
func TestSaveFileAndReadImage(t *testing.T) {
	s, _ := store(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	files := func(_ context.Context, p string) ([]byte, string, error) {
		if p != "out/chart.png" {
			return nil, "", errors.New(ErrNotFound, "no such file")
		}
		return png, "image/png", nil
	}
	ctx := WithScope(bg, "c")
	ts := index2(Tools(s, Access{Write: true, Files: files}))
	if _, err := callCtx(t, ctx, ts["save_file_artifact"], fileIn{Path: "out/chart.png", Title: "Chart"}); err != nil {
		t.Fatal(err)
	}
	out, err := callCtx(t, ctx, ts["read_artifact"], readIn{Name: "chart.png"})
	if err != nil || len(out.Images) != 1 || out.Images[0].MIME != "image/png" {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := callCtx(t, ctx, ts["edit_artifact"], editIn{Name: "chart.png", Edits: []edit{{Old: "a", New: "b"}}}); !errors.HasCode(err, ErrInput) {
		t.Fatalf("edited a file: %v", err)
	}
	if _, ok := index2(Tools(s, Access{Write: true}))["save_file_artifact"]; ok {
		t.Fatal("save_file_artifact without Files")
	}
}

// Kinds, titles and languages are normalized; unknown kinds refused.
func TestMetaNormalized(t *testing.T) {
	s, _ := store(t)
	info, err := s.Put(bg, "c", "n", Meta{Kind: Markdown, Title: "  a\x1b[31m\nb  ", Language: "go"}, []byte("x"))
	if err != nil || info.Title != "a [31m b" || info.Language != "" || info.MediaType != "text/markdown" {
		t.Fatalf("%+v %v", info, err)
	}
	if info, _ := s.Put(bg, "c", "m", Meta{}, []byte("x")); info.Kind != Text || info.Title != "m" {
		t.Fatalf("defaults %+v", info)
	}
	for _, bad := range []Meta{{Kind: "exe"}, {Kind: Code, Language: "Go Lang"}} {
		if _, err := s.Put(bg, "c", "z", bad, []byte("x")); !errors.HasCode(err, ErrInput) {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// A conversation gone takes its artifacts with it.
func TestDeleteScope(t *testing.T) {
	s, _ := store(t)
	for _, sc := range []string{"gone", "stays"} {
		if _, err := s.Put(bg, sc, "a.md", Meta{Kind: Markdown}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteScope(bg, "gone"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.List(bg, "gone"); len(list) != 0 {
		t.Fatal("still there")
	}
	if list, _ := s.List(bg, "stays"); len(list) != 1 {
		t.Fatal("the other scope went too")
	}
	if err := s.DeleteScope(bg, "never"); err != nil {
		t.Fatalf("a scope never made: %v", err)
	}
}
