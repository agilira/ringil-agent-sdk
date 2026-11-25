// github_test.go: installing skills from GitHub links, against a fake GitHub
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

const commitA = "0123456789abcdef0123456789abcdef01234567"

func skillMD(name string) string {
	return "---\nname: " + name + "\ndescription: Does " + name + ".\n---\n\nSteps for " + name + ".\n"
}

// entry is one archive entry; a link when target is set.
type entry struct {
	name, body, target string
	kind               byte
}

func file(name, body string) entry { return entry{name: name, body: body, kind: tar.TypeReg} }

// archive builds a gzipped tar the way GitHub does: everything under one
// top folder, after a global header.
func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commitA}}))
	must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "o-r-0123456/", Mode: 0o755}))
	for _, e := range entries {
		h := &tar.Header{Typeflag: e.kind, Name: "o-r-0123456/" + e.name, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.target}
		if e.kind != tar.TypeReg {
			h.Size = 0
		}
		must(t, tw.WriteHeader(h))
		if e.kind == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			must(t, err)
		}
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return buf.Bytes()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeGitHub serves the commits API and codeload for repository o/r.
type fakeGitHub struct {
	*httptest.Server
	tarball    []byte
	apiStatus  int
	apiBody    string
	apiCalls   atomic.Int32
	lastRef    atomic.Value
	lastAuth   atomic.Value
	lastCommit atomic.Value
}

func newFake(t *testing.T, tarball []byte) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tarball: tarball, apiStatus: http.StatusOK, apiBody: commitA}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/o/r/commits/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls.Add(1)
		f.lastRef.Store(r.PathValue("ref"))
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			http.Error(w, "accept", http.StatusBadRequest)
			return
		}
		w.WriteHeader(f.apiStatus)
		_, _ = w.Write([]byte(f.apiBody))
	})
	mux.HandleFunc("/load/o/r/tar.gz/{commit}", func(w http.ResponseWriter, r *http.Request) {
		f.lastCommit.Store(r.PathValue("commit"))
		_, _ = w.Write(f.tarball)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) gh() GitHub {
	return GitHub{API: f.URL + "/api", Codeload: f.URL + "/load"}
}

func installLink(t *testing.T, f *fakeGitHub, link string) (Installed, string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "skills")
	s, err := InstallGitHub(bg, link, dir, f.gh())
	return s, dir, err
}

func TestInstallGitHubRootSkill(t *testing.T) {
	f := newFake(t, archive(t,
		file("SKILL.md", skillMD("triage")),
		file("scripts/run.py", "print(1)\n"),
		file("README.md", "readme"),
		file(".github/workflows/ci.yml", "hidden"),
	))
	s, dir, err := installLink(t, f, "https://github.com/o/r")
	must(t, err)
	if s.Name != "triage" || s.Source != "github.com/o/r@"+commitA {
		t.Fatalf("installed %+v", s)
	}
	if got := strings.Join(s.Files, ",") + " | " + strings.Join(s.Scripts, ","); got != "README.md,SKILL.md,scripts/run.py | scripts/run.py" {
		t.Fatalf("files | scripts: %s", got)
	}
	if f.lastRef.Load() != "HEAD" || f.lastCommit.Load() != commitA {
		t.Fatalf("asked %v, archive of %v: want HEAD and its commit", f.lastRef.Load(), f.lastCommit.Load())
	}
	checkCatalogHas(t, dir, s)
}

// checkCatalogHas: the catalog loads the installed skill and records its
// source, in the manifest too.
func checkCatalogHas(t *testing.T, dir string, s Installed) {
	t.Helper()
	c, err := Open(dir)
	must(t, err)
	if body, err := c.Load(s.Name); err != nil || !strings.Contains(body, "Steps for "+s.Name) {
		t.Fatalf("load: %q %v", body, err)
	}
	if c.Source(s.Name) != s.Source || c.Source("nope") != "" {
		t.Fatalf("catalog source %q", c.Source(s.Name))
	}
	if got := manifestSource(t, dir, s.Name); got != s.Source {
		t.Fatalf("manifest source %q, want %q", got, s.Source)
	}
}

func manifestSource(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name, manifestName))
	must(t, err)
	var m manifest
	must(t, json.Unmarshal(data, &m))
	return m.Source
}

func multiRepo(t *testing.T) []byte {
	return archive(t,
		file("README.md", "readme"),
		file("skills/pdf/SKILL.md", skillMD("pdf")),
		file("skills/pdf/forms.md", "forms"),
		file("skills/docx/SKILL.md", skillMD("docx")),
		file("skills/docx/scripts/x.sh", "echo"),
	)
}

func TestInstallGitHubFolderLinks(t *testing.T) {
	for _, link := range []string{
		"https://github.com/o/r/tree/main/skills/pdf",
		"https://github.com/o/r/blob/main/skills/pdf/SKILL.md",
		"github.com/o/r/skills/pdf@main",
		"o/r/skills/pdf@main",
		"https://github.com/o/r/tree/main/skills/pdf/?tab=readme#top",
	} {
		t.Run(link, func(t *testing.T) {
			f := newFake(t, multiRepo(t))
			s, _, err := installLink(t, f, link)
			must(t, err)
			if s.Name != "pdf" || s.Source != "github.com/o/r/skills/pdf@"+commitA || strings.Join(s.Files, ",") != "SKILL.md,forms.md" {
				t.Fatalf("installed %+v", s)
			}
			if got := f.lastRef.Load(); got != "main" {
				t.Fatalf("ref %v", got)
			}
		})
	}
}

func TestInstallGitHubSeveralSkillsListsThem(t *testing.T) {
	f := newFake(t, multiRepo(t))
	_, dir, err := installLink(t, f, "https://github.com/o/r")
	msg := agent.ModelMessage(err)
	for _, want := range []string{"several skills", "github.com/o/r/skills/pdf@" + commitA, "github.com/o/r/skills/docx@" + commitA} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Fatalf("something was installed: %v", entries)
		}
	}
	// The listed link installs that one, with no further API call.
	calls := f.apiCalls.Load()
	s, err := InstallGitHub(bg, "github.com/o/r/skills/docx@"+commitA, dir, f.gh())
	must(t, err)
	if s.Name != "docx" || f.apiCalls.Load() != calls {
		t.Fatalf("installed %+v, api calls %d -> %d", s, calls, f.apiCalls.Load())
	}
}

func TestInstallGitHubSlashedBranch(t *testing.T) {
	f := newFake(t, archive(t, file("SKILL.md", skillMD("a"))))
	var raw atomic.Value
	inner := f.Config.Handler
	f.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			raw.Store(r.URL.EscapedPath())
		}
		inner.ServeHTTP(w, r)
	})
	_, _, err := installLink(t, f, "o/r@feature/x")
	must(t, err)
	if raw.Load() != "/api/repos/o/r/commits/feature/x" {
		t.Fatalf("asked %v", raw.Load())
	}
}

func TestInstallGitHubSingleSkillBelow(t *testing.T) {
	f := newFake(t, archive(t, file("README.md", "x"), file("skill/SKILL.md", skillMD("only"))))
	s, _, err := installLink(t, f, "o/r")
	must(t, err)
	if s.Name != "only" || s.Source != "github.com/o/r/skill@"+commitA {
		t.Fatalf("installed %+v", s)
	}
}

func TestInstallGitHubRefusals(t *testing.T) {
	cases := []struct {
		name    string
		entries []entry
		link    string
		want    string
	}{
		{"no skill", []entry{file("main.go", "package main")}, "o/r", "no SKILL.md"},
		{"missing folder", []entry{file("SKILL.md", skillMD("a"))}, "o/r/nope@main", "Nothing found"},
		{"link in skill", []entry{file("SKILL.md", skillMD("a")), {name: "key", target: "/home/u/.ssh/id_rsa", kind: tar.TypeSymlink}}, "o/r", "link or special file: key"},
		{"hard link in skill", []entry{file("SKILL.md", skillMD("a")), {name: "h", target: "SKILL.md", kind: tar.TypeLink}}, "o/r", "link or special file: h"},
		{"fifo in skill", []entry{file("s/SKILL.md", skillMD("a")), {name: "s/p", kind: tar.TypeFifo}}, "o/r/s@main", "link or special file: p"},
		{"bad front matter", []entry{file("SKILL.md", "no front matter")}, "o/r", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t, archive(t, c.entries...))
			_, _, err := installLink(t, f, c.link)
			if err == nil {
				t.Fatal("installed")
			}
			if msg := agent.ModelMessage(InstallToolError(err)); c.want != "" && !strings.Contains(msg, c.want) {
				t.Fatalf("message %q lacks %q", msg, c.want)
			}
		})
	}
}

// InstallToolError is the error as install_skill hands it to the model.
func InstallToolError(err error) error { return modelError(err) }

func TestInstallGitHubLinkOutsideSkillIsLeftOut(t *testing.T) {
	f := newFake(t, archive(t,
		file("docs/x.md", "x"),
		entry{name: "docs/latest", target: "x.md", kind: tar.TypeSymlink},
		file("skills/a/SKILL.md", skillMD("a")),
	))
	s, _, err := installLink(t, f, "o/r")
	must(t, err)
	if s.Name != "a" {
		t.Fatalf("installed %+v", s)
	}
}

func TestInstallGitHubStatus(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "Not found on GitHub"},
		{http.StatusUnauthorized, "refused the token"},
		{http.StatusForbidden, "rate limit"},
		{http.StatusTooManyRequests, "rate limit"},
		{http.StatusBadGateway, "try later"},
	} {
		f := newFake(t, nil)
		f.apiStatus = c.status
		_, _, err := installLink(t, f, "o/r")
		if !errors.HasCode(err, ErrFetch) || !strings.Contains(agent.ModelMessage(err), c.want) {
			t.Fatalf("%d: %v / %q", c.status, err, agent.ModelMessage(err))
		}
	}
}

func TestInstallGitHubBadCommitAnswer(t *testing.T) {
	f := newFake(t, nil)
	f.apiBody = "<html>not a sha</html>"
	if _, _, err := installLink(t, f, "o/r"); !errors.HasCode(err, ErrFetch) {
		t.Fatalf("err %v", err)
	}
}

func TestInstallGitHubNotAnArchive(t *testing.T) {
	f := newFake(t, []byte("not gzip"))
	if _, _, err := installLink(t, f, "o/r"); !errors.HasCode(err, ErrFetch) {
		t.Fatalf("err %v", err)
	}
}

func TestInstallGitHubHeaders(t *testing.T) {
	f := newFake(t, archive(t, file("SKILL.md", skillMD("a"))))
	g := f.gh()
	g.Headers = func(_ context.Context) (http.Header, error) {
		return http.Header{"Authorization": {"Bearer tok"}}, nil
	}
	_, err := InstallGitHub(bg, "o/r", filepath.Join(t.TempDir(), "s"), g)
	must(t, err)
	if f.lastAuth.Load() != "Bearer tok" {
		t.Fatalf("auth %v", f.lastAuth.Load())
	}
}

func TestInstallGitHubAlreadyInstalled(t *testing.T) {
	f := newFake(t, archive(t, file("SKILL.md", skillMD("a"))))
	_, dir, err := installLink(t, f, "o/r")
	must(t, err)
	_, err = InstallGitHub(bg, "o/r", dir, f.gh())
	if !errors.HasCode(err, ErrExists) || !strings.Contains(agent.ModelMessage(modelError(err)), "already installed") {
		t.Fatalf("err %v", err)
	}
}

func TestInstallGitHubRequiresHTTPS(t *testing.T) {
	g := GitHub{API: "http://api.example.com", Codeload: "https://codeload.example.com"}
	if _, err := InstallGitHub(bg, "o/r", t.TempDir(), g); !errors.HasCode(err, ErrInvalid) {
		t.Fatalf("err %v", err)
	}
}

func TestParseLink(t *testing.T) {
	good := map[string]ghRef{
		"https://github.com/o/r":                          {owner: "o", repo: "r"},
		"https://github.com/o/r.git":                      {owner: "o", repo: "r"},
		"https://GitHub.com/o/r/":                         {owner: "o", repo: "r"},
		"https://github.com/o/r/tree/v1.2/a/b":            {owner: "o", repo: "r", ref: "v1.2", path: "a/b"},
		"https://github.com/o/r/blob/main/a/SKILL.md":     {owner: "o", repo: "r", ref: "main", path: "a"},
		"https://github.com/o/r/blob/main/a/notes.md":     {owner: "o", repo: "r", ref: "main", path: "a/notes.md"},
		"github.com/o/r":                                  {owner: "o", repo: "r"},
		"o/r":                                             {owner: "o", repo: "r"},
		"o/r@feature/x":                                   {owner: "o", repo: "r", ref: "feature/x"},
		"  o/r/a@" + commitA + "  ":                       {owner: "o", repo: "r", ref: commitA, path: "a"},
		"github.com/anthropics/skills/skills/pdf@main":    {owner: "anthropics", repo: "skills", ref: "main", path: "skills/pdf"},
		"https://github.com/o/r/tree/main?x=1#frag":       {owner: "o", repo: "r", ref: "main"},
		"github.com/o/r/tree/main/a#readme":               {owner: "o", repo: "r", ref: "main", path: "a"},
		"https://github.com/my-org/my_repo.2/tree/dev/sk": {owner: "my-org", repo: "my_repo.2", ref: "dev", path: "sk"},
	}
	for link, want := range good {
		got, err := parseLink(link, "github.com")
		if err != nil || got != want {
			t.Errorf("%q: %+v %v, want %+v", link, got, err, want)
		}
	}
	bad := []string{
		"", "o", "https://gitlab.com/o/r", "http://github.com/o/r", "https://user:pw@github.com/o/r",
		"https://github.com.evil.com/o/r", "ftp://github.com/o/r", "../r", "o/..", "o/r/../../etc",
		"o/r@../main", "o/r@bad ref", "o/r/a\\b", "https://github.com/o/r/tree", "o/r/tree/main/x@v1",
		"https://github.com/o/r/tree/main/%2e%2e/x", "my.org/r", "evil.com/o/r", "o/r/tree", "o r/x",
	}
	for _, link := range bad {
		if got, err := parseLink(link, "github.com"); err == nil {
			t.Errorf("%q accepted as %+v", link, got)
		}
	}
}

func TestInstallToolCall(t *testing.T) {
	f := newFake(t, archive(t, file("SKILL.md", skillMD("triage")), file("scripts/a.py", "x")))
	dir := filepath.Join(t.TempDir(), "skills")
	var heard []string
	tool := InstallTool(dir, f.gh(), func(s Installed) { heard = append(heard, s.Name) })
	spec := tool.Spec()
	if spec.Name != "install_skill" || !spec.Annotations.Destructive || !spec.Annotations.OpenWorld || spec.Annotations.SelfConfirms {
		t.Fatalf("spec %+v", spec)
	}
	out, err := tool.Call(bg, json.RawMessage(`{"link":"o/r"}`))
	must(t, err)
	for _, want := range []string{"Installed skill triage", "github.com/o/r@" + commitA, "scripts/a.py", "sandbox"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("output %q lacks %q", out.Content, want)
		}
	}
	if strings.Join(heard, ",") != "triage" {
		t.Fatalf("heard %v", heard)
	}
	_, err = tool.Call(bg, json.RawMessage(`{"link":"https://gitlab.com/o/r"}`))
	if msg := agent.ModelMessage(err); !strings.Contains(msg, "github.com") {
		t.Fatalf("model sees %q", msg)
	}
}

func TestInstallToolAsksUnderDefaultPolicy(t *testing.T) {
	tool := InstallTool(t.TempDir(), GitHub{}, nil)
	p := &agent.Policy{}
	call := agent.ToolCall{Name: "install_skill", Input: json.RawMessage(`{"link":"o/r"}`)}
	if got := p.Effect(call, tool.Spec()); got != agent.EffectAsk {
		t.Fatalf("effect %v, want ask", got)
	}
	plan := &agent.Policy{Mode: agent.ModePlan}
	if got := plan.Effect(call, tool.Spec()); got != agent.EffectDeny {
		t.Fatalf("plan mode effect %v, want deny", got)
	}
}
