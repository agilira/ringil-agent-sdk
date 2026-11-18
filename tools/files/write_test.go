// write_test.go: write_file and edit_file
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

// openRW is a workspace with the read and write tools.
func openRW(t *testing.T, cfg Config) agent.Registry {
	t.Helper()
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := agent.NewRegistry(append(w.ReadTools(), w.WriteTools()...)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func content(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteCreatesAndReplaces(t *testing.T) {
	dir := tree(t, map[string]string{"a.txt": "old\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	if out := mustCall(t, r, "write_file", map[string]any{"path": "sub/new.txt", "content": "hello\n"}); !strings.HasPrefix(out, "Created") {
		t.Fatal(out)
	}
	if got := content(t, filepath.Join(dir, "sub", "new.txt")); got != "hello\n" {
		t.Fatalf("%q", got)
	}
	// An existing file not read first is refused.
	failCall(t, r, "write_file", map[string]any{"path": "a.txt", "content": "x"}, ErrNotRead)
	mustCall(t, r, "read_file", map[string]any{"path": "a.txt"})
	mustCall(t, r, "write_file", map[string]any{"path": "a.txt", "content": "new\n"})
	if got := content(t, filepath.Join(dir, "a.txt")); got != "new\n" {
		t.Fatalf("%q", got)
	}
	// A file it wrote it may write again without reading.
	mustCall(t, r, "write_file", map[string]any{"path": "a.txt", "content": "newer\n"})
}

func TestEditRules(t *testing.T) {
	dir := tree(t, map[string]string{"main.go": "package main\n\nfunc a() {}\nfunc b() {}\nfunc a2() {}\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	failCall(t, r, "edit_file", map[string]any{"path": "main.go", "old_string": "b()", "new_string": "c()"}, ErrNotRead)
	mustCall(t, r, "read_file", map[string]any{"path": "main.go"})
	failCall(t, r, "edit_file", map[string]any{"path": "main.go", "old_string": "nothere", "new_string": "x"}, ErrNoMatch)
	failCall(t, r, "edit_file", map[string]any{"path": "main.go", "old_string": "func a", "new_string": "func z"}, ErrNoMatch)
	failCall(t, r, "edit_file", map[string]any{"path": "main.go", "old_string": "b()", "new_string": "b()"}, ErrInput)
	out := mustCall(t, r, "edit_file", map[string]any{"path": "main.go", "old_string": "func b() {}", "new_string": "func c() {}"})
	if !strings.Contains(out, "first at line 4") {
		t.Fatal(out)
	}
	mustCall(t, r, "edit_file", map[string]any{"path": "main.go", "old_string": "func a", "new_string": "func z", "replace_all": true})
	if got := content(t, filepath.Join(dir, "main.go")); got != "package main\n\nfunc z() {}\nfunc c() {}\nfunc z2() {}\n" {
		t.Fatalf("%q", got)
	}
}

// A file changed by someone else since the model read it is not
// overwritten: their work would be lost.
func TestChangedSinceReadIsRefused(t *testing.T) {
	dir := tree(t, map[string]string{"notes.md": "mine\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "notes.md"})
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("the user's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failCall(t, r, "edit_file", map[string]any{"path": "notes.md", "old_string": "mine", "new_string": "x"}, ErrChanged)
	failCall(t, r, "write_file", map[string]any{"path": "notes.md", "content": "x"}, ErrChanged)
	if got := content(t, filepath.Join(dir, "notes.md")); got != "the user's edit\n" {
		t.Fatalf("%q", got)
	}
}

func TestCRLFFilesAreEditedWithLF(t *testing.T) {
	dir := tree(t, map[string]string{"win.txt": "one\r\ntwo\r\nthree\r\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "win.txt"})
	mustCall(t, r, "edit_file", map[string]any{"path": "win.txt", "old_string": "one\ntwo", "new_string": "uno\ndue"})
	if got := content(t, filepath.Join(dir, "win.txt")); got != "uno\r\ndue\r\nthree\r\n" {
		t.Fatalf("%q", got)
	}
}

// Protected paths need the host's yes, every time; secrets are never
// written; nothing outside the grant.
func TestProtectedExcludedOutside(t *testing.T) {
	dir := tree(t, map[string]string{"AGENTS.md": "rules\n", ".env": "K=v\n"})
	asked := 0
	answer := false
	r := openRW(t, Config{Dirs: []string{dir}, ConfirmProtected: func(_ context.Context, p string) (bool, error) {
		asked++
		return answer, nil
	}})
	mustCall(t, r, "read_file", map[string]any{"path": "AGENTS.md"})
	failCall(t, r, "edit_file", map[string]any{"path": "AGENTS.md", "old_string": "rules", "new_string": "mine"}, ErrProtected)
	failCall(t, r, "write_file", map[string]any{"path": ".git/hooks/pre-commit", "content": "evil"}, ErrProtected)
	answer = true
	mustCall(t, r, "edit_file", map[string]any{"path": "AGENTS.md", "old_string": "rules", "new_string": "mine"})
	if asked != 3 {
		t.Fatalf("asked %d times", asked)
	}
	failCall(t, r, "write_file", map[string]any{"path": ".env", "content": "x"}, ErrExcluded)
	failCall(t, r, "write_file", map[string]any{"path": filepath.Join(t.TempDir(), "x.txt"), "content": "x"}, ErrOutside)
	// Without a way to ask, protected paths are refused.
	r = openRW(t, Config{Dirs: []string{dir}})
	failCall(t, r, "write_file", map[string]any{"path": ".github/workflows/ci.yml", "content": "x"}, ErrProtected)
}

// A link out of the grant is refused; a link inside it is resolved: the
// real file is checked and written, the link stays a link.
func TestLinksAreNotWritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := tree(t, map[string]string{"real.txt": "real\n"})
	outside := tree(t, map[string]string{"target.txt": "outside\n"})
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "inner.txt")); err != nil {
		t.Fatal(err)
	}
	r := openRW(t, Config{Dirs: []string{dir}})
	failCall(t, r, "write_file", map[string]any{"path": "link.txt", "content": "pwned"}, ErrOutside)
	if got := content(t, filepath.Join(outside, "target.txt")); got != "outside\n" {
		t.Fatalf("written through a link: %q", got)
	}
	mustCall(t, r, "read_file", map[string]any{"path": "inner.txt"})
	mustCall(t, r, "write_file", map[string]any{"path": "inner.txt", "content": "via link\n"})
	if got := content(t, filepath.Join(dir, "real.txt")); got != "via link\n" {
		t.Fatalf("%q", got)
	}
	if info, err := os.Lstat(filepath.Join(dir, "inner.txt")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
}

func TestWriteKeepsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions")
	}
	dir := tree(t, map[string]string{"run.sh": "echo hi\n"})
	p := filepath.Join(dir, "run.sh")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "run.sh"})
	mustCall(t, r, "edit_file", map[string]any{"path": "run.sh", "old_string": "hi", "new_string": "ciao"})
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// Audit 18/11 (1): two edits at once both passed the check and one was
// lost. A file is held for one write at a time.
func TestConcurrentEditsBothLand(t *testing.T) {
	for range 100 {
		dir := tree(t, map[string]string{"f.txt": "AAA\nBBB\n"})
		w, err := New(Config{Dirs: []string{dir}})
		if err != nil {
			t.Fatal(err)
		}
		ws := w.(*workspace)
		if _, err := ws.read(context.Background(), readIn{Path: "f.txt"}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, pair := range [][2]string{{"AAA", "aaa"}, {"BBB", "bbb"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = ws.edit(context.Background(), editIn{Path: "f.txt", OldString: pair[0], NewString: pair[1]})
			}()
		}
		wg.Wait()
		if got := content(t, filepath.Join(dir, "f.txt")); errs[0] != nil || errs[1] != nil || got != "aaa\nbbb\n" {
			t.Fatalf("%v %v %q", errs[0], errs[1], got)
		}
	}
}

// Audit 18/11 (2): a one-line old_string was found as is, and a
// multi-line new_string went in with bare LFs.
func TestCRLFKeptEvenForOneLineMatches(t *testing.T) {
	dir := tree(t, map[string]string{"w.txt": "a\r\nb\r\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "w.txt"})
	mustCall(t, r, "edit_file", map[string]any{"path": "w.txt", "old_string": "a", "new_string": "x\ny"})
	mustCall(t, r, "edit_file", map[string]any{"path": "w.txt", "old_string": "y", "new_string": "y\r\nz"})
	if got := content(t, filepath.Join(dir, "w.txt")); got != "x\r\ny\r\nz\r\nb\r\n" {
		t.Fatalf("%q", got)
	}
}

// Audit 18/11 (3): folders were created before the checks, inside .ssh
// through a link, inside .git for a refused write.
func TestNoFoldersForRefusedWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := tree(t, map[string]string{".ssh/known_hosts": "x"})
	if err := os.Symlink(".ssh", filepath.Join(dir, "keys")); err != nil {
		t.Fatal(err)
	}
	r := openRW(t, Config{Dirs: []string{dir}})
	failCall(t, r, "write_file", map[string]any{"path": "keys/newdir/x.txt", "content": "x"}, ErrExcluded)
	failCall(t, r, "write_file", map[string]any{"path": "sub/.git/hooks/pre-commit", "content": "x"}, ErrProtected)
	for _, p := range []string{".ssh/newdir", "sub/.git", "sub"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}

// Audit 18/11 (4): a long valid name could not be written.
func TestLongNames(t *testing.T) {
	dir := tree(t, nil)
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "write_file", map[string]any{"path": strings.Repeat("n", 250) + ".md", "content": "x"})
}

// Audit 18/11 (6): a read that showed nothing (offset past the end) let
// the model replace the whole file. Replacing needs the whole file seen;
// editing a part needs the part to match.
func TestReplaceNeedsAWholeRead(t *testing.T) {
	dir := tree(t, map[string]string{"f.txt": "precious\nmore\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "f.txt", "offset": 99})
	failCall(t, r, "write_file", map[string]any{"path": "f.txt", "content": "clobber\n"}, ErrNotRead)
	mustCall(t, r, "read_file", map[string]any{"path": "f.txt", "limit": 1})
	failCall(t, r, "write_file", map[string]any{"path": "f.txt", "content": "clobber\n"}, ErrNotRead)
	mustCall(t, r, "edit_file", map[string]any{"path": "f.txt", "old_string": "precious", "new_string": "kept"})
	mustCall(t, r, "write_file", map[string]any{"path": "f.txt", "content": "after my own edit\n"})
}

// Audit 18/11 (7): a file with a bad byte past the first 8 KiB read as
// text but could not be replaced.
func TestReplaceFileWithBadByteFarDown(t *testing.T) {
	dir := tree(t, map[string]string{"f.txt": strings.Repeat(strings.Repeat("a", 99)+"\n", 100) + "\xff\n"})
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "f.txt"})
	mustCall(t, r, "write_file", map[string]any{"path": "f.txt", "content": "clean\n"})
}

// Read-only files stay read-only: the user made them so.
func TestReadOnlyFilesRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions")
	}
	dir := tree(t, map[string]string{"lock.txt": "x\n"})
	if err := os.Chmod(filepath.Join(dir, "lock.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	r := openRW(t, Config{Dirs: []string{dir}})
	mustCall(t, r, "read_file", map[string]any{"path": "lock.txt"})
	failCall(t, r, "write_file", map[string]any{"path": "lock.txt", "content": "y"}, ErrInput)
}

// Case folding as the file systems do: "ſ" is "s".
func TestFoldedNamesMatch(t *testing.T) {
	if !Excluded("/home/u/.ſsh/id", nil) || !Protected("/r/AGENTſ.md", nil) {
		t.Fatal("folded names escaped the policy")
	}
}
