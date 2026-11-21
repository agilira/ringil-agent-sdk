// files_test.go: tests for the file tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// tree writes files (name -> content) under a new directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// resolved resolves a test directory the way the workspace does (macOS
// /var -> /private/var, Windows 8.3 short names).
func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func open(t *testing.T, cfg Config) agent.Registry {
	t.Helper()
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := agent.NewRegistry(w.ReadTools()...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// call runs a tool and returns what the model sees and the host error.
func call(t *testing.T, r agent.Registry, name string, args map[string]any) (string, error) {
	t.Helper()
	in, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Execute(context.Background(), agent.ToolCall{ID: "1", Name: name, Input: in})
	return res.Content, err
}

func mustCall(t *testing.T, r agent.Registry, name string, args map[string]any) string {
	t.Helper()
	out, err := call(t, r, name, args)
	if err != nil {
		t.Fatalf("%s %v: %v (%s)", name, args, err, out)
	}
	return out
}

func failCall(t *testing.T, r agent.Registry, name string, args map[string]any, code errors.ErrorCode) string {
	t.Helper()
	out, err := call(t, r, name, args)
	if !errors.HasCode(err, code) {
		t.Fatalf("%s %v: want %s, got %v (%s)", name, args, code, err, out)
	}
	return out
}

func TestNewValidates(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{{}, {Dirs: []string{"/nonexistent-ringil"}}, {Dirs: []string{file}}, {Dirs: []string{t.TempDir()}, Exclude: []string{"["}}} {
		if _, err := New(cfg); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%+v accepted: %v", cfg, err)
		}
	}
}

func TestReadFilePaging(t *testing.T) {
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "riga "+string(rune('0'+i%10)))
	}
	dir := tree(t, map[string]string{"a.txt": strings.Join(lines, "\n") + "\n", "empty.txt": ""})
	r := open(t, Config{Dirs: []string{dir}})
	out := mustCall(t, r, "read_file", map[string]any{"path": "a.txt", "offset": 3, "limit": 2})
	if !strings.HasPrefix(out, "a.txt: lines 3-4 of 10 (continue with offset 5)\n") || !strings.Contains(out, "     3\triga 3\n     4\triga 4\n") || strings.Contains(out, "riga 5") {
		t.Fatalf("out %q", out)
	}
	if out := mustCall(t, r, "read_file", map[string]any{"path": "a.txt"}); !strings.HasPrefix(out, "a.txt: lines 1-10 of 10\n") {
		t.Fatalf("whole file %q", out)
	}
	if out := mustCall(t, r, "read_file", map[string]any{"path": "a.txt", "offset": 50}); !strings.Contains(out, "past the end") {
		t.Fatalf("past end %q", out)
	}
	if out := mustCall(t, r, "read_file", map[string]any{"path": "empty.txt"}); out != "empty.txt is empty.\n" {
		t.Fatalf("empty %q", out)
	}
}

func TestReadFileRefusals(t *testing.T) {
	dir := tree(t, map[string]string{"bin.dat": "ab\x00cd", "sub/x.txt": "x"})
	r := open(t, Config{Dirs: []string{dir}})
	failCall(t, r, "read_file", map[string]any{"path": "bin.dat"}, ErrBinary)
	failCall(t, r, "read_file", map[string]any{"path": "sub"}, ErrInput)
	failCall(t, r, "read_file", map[string]any{"path": "missing.txt"}, ErrNotFound)
}

func TestLongLinesAreClipped(t *testing.T) {
	dir := tree(t, map[string]string{"long.txt": strings.Repeat("é", maxLineBytes/2+5)})
	out := mustCall(t, open(t, Config{Dirs: []string{dir}}), "read_file", map[string]any{"path": "long.txt"})
	if !strings.Contains(out, "[line truncated, 10 more bytes]") {
		t.Fatalf("out %q", out[len(out)-60:])
	}
}

func TestPathsAbsoluteRelativeAndOutside(t *testing.T) {
	first := tree(t, map[string]string{"a.txt": "primo"})
	second := tree(t, map[string]string{"b.txt": "secondo"})
	r := open(t, Config{Dirs: []string{first, second}})
	if out := mustCall(t, r, "read_file", map[string]any{"path": filepath.Join(second, "b.txt")}); !strings.Contains(out, "secondo") {
		t.Fatalf("absolute in second dir: %q", out)
	}
	if out := mustCall(t, r, "read_file", map[string]any{"path": filepath.Join(first, "a.txt")}); !strings.Contains(out, "primo") {
		t.Fatalf("absolute in first dir: %q", out)
	}
	outside := tree(t, map[string]string{"secret.txt": "no"})
	for _, p := range []string{filepath.Join(outside, "secret.txt"), "../" + filepath.Base(outside) + "/secret.txt", "a/../../x"} {
		msg := failCall(t, r, "read_file", map[string]any{"path": p}, ErrOutside)
		if !strings.Contains(msg, resolved(t, first)) {
			t.Fatalf("model not told where it can read: %q", msg)
		}
	}
}

func TestSymlinkOutOfGrantIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := tree(t, map[string]string{})
	outside := tree(t, map[string]string{"secret.txt": "TOPSECRET"})
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	r := open(t, Config{Dirs: []string{dir}})
	out, err := call(t, r, "read_file", map[string]any{"path": "link/secret.txt"})
	if err == nil || strings.Contains(out, "TOPSECRET") {
		t.Fatalf("followed a symlink out of the grant: %q %v", out, err)
	}
	if out := mustCall(t, r, "list_directory", map[string]any{}); !strings.Contains(out, "link (symlink)") {
		t.Fatalf("list %q", out)
	}
}

func TestSecretsExcludedByDefault(t *testing.T) {
	dir := tree(t, map[string]string{".env": "KEY=1", ".env.local": "K=2", ".env.example": "KEY=", ".ssh/id_ed25519": "k", "cert.pem": "p", "ok.txt": "ok"})
	r := open(t, Config{Dirs: []string{dir}})
	for _, p := range []string{".env", ".env.local", ".ssh/id_ed25519", "cert.pem"} {
		failCall(t, r, "read_file", map[string]any{"path": p}, ErrExcluded)
	}
	mustCall(t, r, "read_file", map[string]any{"path": ".env.example"})
	list := mustCall(t, r, "list_directory", map[string]any{"depth": 0})
	if strings.Contains(list, ".ssh") || strings.Contains(list, "cert.pem") || strings.Contains(list, ".env\n") || !strings.Contains(list, ".env.example") {
		t.Fatalf("list %q", list)
	}
	if out := mustCall(t, r, "grep", map[string]any{"pattern": "KEY"}); strings.Contains(out, "KEY=1") {
		t.Fatalf("grep read an excluded file: %q", out)
	}
	open := open(t, Config{Dirs: []string{dir}, Exclude: []string{}})
	mustCall(t, open, "read_file", map[string]any{"path": ".env"})
}

func TestListDirectory(t *testing.T) {
	dir := tree(t, map[string]string{"a/b/c.txt": "123", "top.txt": "12345"})
	r := open(t, Config{Dirs: []string{dir}})
	out := mustCall(t, r, "list_directory", map[string]any{})
	if out != "2 entries:\na/\ntop.txt  5 bytes\n" {
		t.Fatalf("depth 1: %q", out)
	}
	if out := mustCall(t, r, "list_directory", map[string]any{"depth": 0}); !strings.Contains(out, "a/b/c.txt  3 bytes") {
		t.Fatalf("all: %q", out)
	}
	if out := mustCall(t, r, "list_directory", map[string]any{"depth": 0, "limit": 1, "offset": 1}); !strings.Contains(out, "4 entries, showing 2-2 (continue with offset 2)") {
		t.Fatalf("page: %q", out)
	}
}

func TestGlob(t *testing.T) {
	dir := tree(t, map[string]string{"main.go": "", "pkg/a.go": "", "pkg/deep/b.go": "", "docs/x.md": "", "docs/y.txt": ""})
	r := open(t, Config{Dirs: []string{dir}})
	if out := mustCall(t, r, "glob", map[string]any{"pattern": "**/*.go"}); out != "3 files:\nmain.go\npkg/a.go\npkg/deep/b.go\n" {
		t.Fatalf("**: %q", out)
	}
	if out := mustCall(t, r, "glob", map[string]any{"pattern": "*.md", "path": "docs"}); out != "1 files:\ndocs/x.md\n" {
		t.Fatalf("in dir: %q", out)
	}
	failCall(t, r, "glob", map[string]any{"pattern": "["}, ErrInput)
	failCall(t, r, "glob", map[string]any{"pattern": "**/**/**/**/**/x"}, ErrInput)
	if out := mustCall(t, r, "glob", map[string]any{"pattern": "*.rs"}); out != "No files match.\n" {
		t.Fatalf("none: %q", out)
	}
}

func TestGrep(t *testing.T) {
	dir := tree(t, map[string]string{"a.go": "func Main() {}\nfunc helper() {}\n", "b.txt": "MAIN street\n", "bin": "main\x00"})
	big := filepath.Join(dir, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxGrepFile + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r := open(t, Config{Dirs: []string{dir}})
	out := mustCall(t, r, "grep", map[string]any{"pattern": "main", "ignore_case": true})
	if !strings.Contains(out, "a.go:\n  1: func Main() {}") || !strings.Contains(out, "b.txt:\n  1: MAIN street") || !strings.Contains(out, "(2 binary, unreadable or very large files skipped)") {
		t.Fatalf("out %q", out)
	}
	if out := mustCall(t, r, "grep", map[string]any{"pattern": `func \w+\(`, "include": "*.go"}); !strings.HasPrefix(out, "2 matching lines:") {
		t.Fatalf("include: %q", out)
	}
	if out := mustCall(t, r, "grep", map[string]any{"pattern": "helper", "path": "a.go"}); !strings.Contains(out, "a.go:\n  2: func helper") {
		t.Fatalf("single file: %q", out)
	}
	failCall(t, r, "grep", map[string]any{"pattern": "("}, ErrInput)
	failCall(t, r, "grep", map[string]any{"pattern": "x", "include": "**/**/**/**/**/x"}, ErrInput)
}

func TestGlobMatch(t *testing.T) {
	cases := map[[2]string]bool{
		{"**/*.go", "a.go"}: true, {"**/*.go", "x/y/a.go"}: true, {"*.go", "x/a.go"}: false,
		{"docs/**", "docs/a/b"}: true, {"a/**/b", "a/b"}: true, {"a/**/b", "a/x/y/b"}: true, {"a/*/b", "a/x/y/b"}: false,
	}
	for c, want := range cases {
		if got := globMatch(c[0], c[1]); got != want {
			t.Errorf("globMatch(%q, %q) = %v", c[0], c[1], got)
		}
	}
}

// FuzzGlobMatch: model-written patterns never panic or run away.
func FuzzGlobMatch(f *testing.F) {
	f.Add("**/*.go", "a/b/c.go")
	f.Add("a/**/b/**", "a/x/b/y")
	f.Fuzz(func(t *testing.T, pattern, name string) {
		if validGlob(pattern) == nil {
			globMatch(pattern, name)
		}
	})
}

// asker records the directories it was asked about and answers allow.
type asker struct {
	allow bool
	err   error
	asked []string
}

func (a *asker) ask(_ context.Context, dir string) (bool, error) {
	a.asked = append(a.asked, dir)
	return a.allow, a.err
}

func TestOutsidePathAsksAndGrants(t *testing.T) {
	first := tree(t, map[string]string{"a.txt": "a"})
	other := tree(t, map[string]string{"sub/b.txt": "altra cartella"})
	a := &asker{allow: true}
	r := open(t, Config{Dirs: []string{first}, Ask: a.ask})
	target := filepath.Join(other, "sub", "b.txt")
	if out := mustCall(t, r, "read_file", map[string]any{"path": target}); !strings.Contains(out, "altra cartella") {
		t.Fatalf("out %q", out)
	}
	if out := mustCall(t, r, "list_directory", map[string]any{"path": filepath.Join(other, "sub")}); !strings.Contains(out, "b.txt") {
		t.Fatalf("list %q", out)
	}
	if len(a.asked) != 1 || a.asked[0] != resolved(t, filepath.Join(other, "sub")) {
		t.Fatalf("asked %v", a.asked)
	}
}

func TestRelativeEscapeAsksForTheRealDirectory(t *testing.T) {
	parent := tree(t, map[string]string{"proj/a.txt": "a", "notes/n.txt": "nota"})
	a := &asker{allow: true}
	r := open(t, Config{Dirs: []string{filepath.Join(parent, "proj")}, Ask: a.ask})
	if out := mustCall(t, r, "read_file", map[string]any{"path": "../notes/n.txt"}); !strings.Contains(out, "nota") {
		t.Fatalf("out %q", out)
	}
	if len(a.asked) != 1 || filepath.Base(a.asked[0]) != "notes" {
		t.Fatalf("asked %v", a.asked)
	}
}

func TestDeniedOrFailingAsk(t *testing.T) {
	first, other := tree(t, map[string]string{}), tree(t, map[string]string{"x.txt": "x"})
	no := &asker{}
	msg := failCall(t, open(t, Config{Dirs: []string{first}, Ask: no.ask}), "read_file", map[string]any{"path": filepath.Join(other, "x.txt")}, ErrOutside)
	if !strings.Contains(msg, "did not allow") {
		t.Fatalf("msg %q", msg)
	}
	broken := &asker{err: os.ErrClosed}
	failCall(t, open(t, Config{Dirs: []string{first}, Ask: broken.ask}), "read_file", map[string]any{"path": filepath.Join(other, "x.txt")}, ErrIO)
}

func TestSecretDirectoriesAreNeverOffered(t *testing.T) {
	home := tree(t, map[string]string{".ssh/id_ed25519": "KEY"})
	a := &asker{allow: true}
	r := open(t, Config{Dirs: []string{t.TempDir()}, Ask: a.ask})
	failCall(t, r, "read_file", map[string]any{"path": filepath.Join(home, ".ssh", "id_ed25519")}, ErrExcluded)
	if len(a.asked) != 0 {
		t.Fatalf("asked about a secret directory: %v", a.asked)
	}
}

func TestHiddenListsExcluded(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{".env", ".env.example", "src/main.go", "src/.ssh/id_rsa", "conf/server.key"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Hidden(context.Background(), dir, nil)
	if err != nil || strings.Join(got, ",") != ".env,conf/server.key,src/.ssh" {
		t.Fatalf("hidden %v %v", got, err)
	}
	inside, err := Hidden(context.Background(), filepath.Join(dir, "src", ".ssh"), nil)
	if err != nil || strings.Join(inside, ",") != "id_rsa" {
		t.Fatalf("inside an excluded dir: %v %v", inside, err)
	}
}

func TestMatchPartAgreesWithPathMatch(t *testing.T) {
	pats := []string{".env", ".env.*", "*.pem", "id_rsa*", "a?c", "[ab]x", "*.cargo*", "x\\*"}
	names := []string{".env", ".env.local", "a.pem", "pem", "id_rsa.pub", "abc", "ax", "bx", "q.cargo.z", "x*", "", ".envx"}
	for _, p := range pats {
		for _, n := range names {
			want, err := path.Match(p, n)
			if got := matchPart(p, n); got != (want && err == nil) {
				t.Errorf("%q %q: %v, path.Match %v", p, n, got, want)
			}
		}
	}
}

func TestProtectedPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"/r/CLAUDE.md": true, "/r/.vscode/settings.json": true, "/r/.github/workflows/ci.yml": true,
		"/r/.git/config": true, "/r/sub/AGENTS.md": true, "/r/main.go": false, "/r/docs/guide.md": false,
		"/r/AGENTS.md::$DATA": true, "/r/AGENTS.md:x": true, "/r/.mcp.json": true, "/r/.envrc": true, "/r/.husky/pre-commit": true,
	} {
		if got := Protected(p, nil); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

// The ASCII fast path of fold agrees with the full one.
func TestFoldASCIIFastPath(t *testing.T) {
	for r := rune(0); r < 0x80; r++ {
		s := string(r)
		full := strings.Map(func(r rune) rune {
			least := r
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				least = min(least, f)
			}
			return unicode.ToLower(least)
		}, s)
		if got := fold(s); got != full {
			t.Errorf("%q: %q, want %q", s, got, full)
		}
	}
}

// Anchored finds a home's secrets where they lie, without walking: at the
// top, and where the multi-name patterns point.
func TestAnchored(t *testing.T) {
	dir := tree(t, map[string]string{".ssh/id_rsa": "k", ".netrc": "n", ".config/gh/hosts.yml": "t", ".config/other/x": "ok",
		"id_ed25519": "k", "notes.txt": "ok", "proj/.env": "deep", ".local/share/keyrings/login": "k"})
	got, err := Anchored(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{".config/gh", ".local/share/keyrings", ".netrc", ".ssh", "id_ed25519"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// Multi-name patterns are matched ignoring case, as the policy does
// (audit 21/11: on macOS Glob missed ~/.cargo/Credentials.toml).
func TestAnchoredIgnoresCase(t *testing.T) {
	dir := tree(t, map[string]string{".cargo/Credentials.toml": "t", ".M2/settings-security.xml": "s", ".cargo/config.toml": "ok"})
	got, err := Anchored(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := []string{".M2/settings-security.xml", ".cargo/Credentials.toml"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// A path that does not exist never offers the root, the home or a
// directory above the granted ones: one yes would grant the whole disk
// (found live 21/11: a notes path, /project/INDEX.md, asked for /).
func TestBroadGrantsAreNeverOffered(t *testing.T) {
	parent := tree(t, map[string]string{"proj/a.txt": "a"})
	a := &asker{allow: true}
	r := open(t, Config{Dirs: []string{filepath.Join(parent, "proj")}, Ask: a.ask})
	home, _ := os.UserHomeDir()
	root := filepath.VolumeName(parent) + string(filepath.Separator) // "/" or "C:\\"
	for _, p := range []string{filepath.Join(root, "project-ringil-none", "INDEX.md"), filepath.Join(parent, "nowhere", "x.txt"), filepath.Join(home, "no-such-dir-ringil", "x")} {
		if msg := failCall(t, r, "read_file", map[string]any{"path": p}, ErrOutside); !strings.Contains(msg, "No such file") {
			t.Errorf("%s: %q", p, msg)
		}
	}
	if len(a.asked) != 0 {
		t.Fatalf("offered %v", a.asked)
	}
}
