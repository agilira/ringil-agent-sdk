// write.go: write_file and edit_file
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// Write error codes the model may see.
const (
	ErrNotRead   errors.ErrorCode = "FILE_NOT_READ"
	ErrChanged   errors.ErrorCode = "FILE_CHANGED"
	ErrProtected errors.ErrorCode = "FILE_PROTECTED"
	ErrNoMatch   errors.ErrorCode = "FILE_NO_MATCH"
)

// maxWrite bounds what one call writes.
const maxWrite = 10 << 20

// stamp is what a file held when the model last read or wrote it, and
// whether it saw all of it.
type stamp struct {
	sum   [sha256.Size]byte
	whole bool
}

type writeIn struct {
	Path    string `json:"path" desc:"File path: absolute, or relative to the first granted directory"`
	Content string `json:"content" desc:"The whole new content of the file"`
}

type editIn struct {
	Path       string `json:"path" desc:"File path: absolute, or relative to the first granted directory"`
	OldString  string `json:"old_string" desc:"The exact text to replace, copied from the file, with enough context to appear only once"`
	NewString  string `json:"new_string" desc:"The text that takes its place"`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"Replace every occurrence instead of exactly one"`
}

func (w *workspace) writeTool() agent.Tool {
	return agent.NewTool("write_file",
		"Create a file, or replace a whole file. An existing file must have been read with read_file first, and not changed since; "+
			"to change part of a file, use edit_file. Missing folders are created. Granted directories: "+w.dirList()+".",
		w.write, agent.Edits())
}

func (w *workspace) editTool() agent.Tool {
	return agent.NewTool("edit_file",
		"Replace text in a file: old_string must appear exactly once (or set replace_all), copied exactly from read_file's output "+
			"without the line-number prefix. The file must have been read first, and not changed since. Granted directories: "+w.dirList()+".",
		w.edit, agent.Edits())
}

// WriteTools implements Workspace.
func (w *workspace) WriteTools() []agent.Tool {
	return []agent.Tool{w.writeTool(), w.editTool()}
}

func (w *workspace) write(ctx context.Context, in writeIn) (string, error) {
	if len(in.Content) > maxWrite {
		return "", agent.ToolError(ErrInput, "The content is larger than 10 MiB.")
	}
	t, err := w.target(ctx, in.Path, true)
	if err != nil {
		return "", err
	}
	defer t.close()
	unlock := w.lock(t.real)
	defer unlock()
	if t.exists {
		if _, err := w.current(t, true); err != nil {
			return "", err
		}
	}
	if err := w.commit(ctx, t, []byte(in.Content)); err != nil {
		return "", err
	}
	verb := "Created "
	if t.exists {
		verb = "Wrote "
	}
	return verb + w.display(t.real) + " (" + strconv.Itoa(len(in.Content)) + " bytes).", nil
}

func (w *workspace) edit(ctx context.Context, in editIn) (string, error) {
	if in.OldString == "" || in.OldString == in.NewString {
		return "", agent.ToolError(ErrInput, "old_string must be non-empty and differ from new_string.")
	}
	t, err := w.target(ctx, in.Path, false)
	if err != nil {
		return "", err
	}
	defer t.close()
	unlock := w.lock(t.real)
	defer unlock()
	data, err := w.current(t, false)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", agent.ToolError(ErrBinary, "That is not a text file.")
	}
	next, n, line, err := replace(string(data), in)
	if err != nil {
		return "", err
	}
	if len(next) > maxWrite {
		return "", agent.ToolError(ErrInput, "The edited file would be larger than 10 MiB.")
	}
	if err := w.commit(ctx, t, []byte(next)); err != nil {
		return "", err
	}
	return "Edited " + w.display(t.real) + ": " + strconv.Itoa(n) + " replacement(s), the first at line " + strconv.Itoa(line) + ".", nil
}

// replace applies an edit. In a file with CRLF line ends, the text the
// model sends (usually LF) takes the file's: bare LFs become CRLF.
func replace(text string, in editIn) (string, int, int, error) {
	old, repl := in.OldString, in.NewString
	if strings.Contains(text, "\r\n") {
		old, repl = crlf(old), crlf(repl)
	}
	n := strings.Count(text, old)
	switch {
	case n == 0:
		return "", 0, 0, agent.ToolError(ErrNoMatch, "old_string does not appear in the file. Read it again and copy the text exactly, including spaces and indentation.")
	case n > 1 && !in.ReplaceAll:
		return "", 0, 0, agent.ToolError(ErrNoMatch, "old_string appears "+strconv.Itoa(n)+" times: add surrounding lines so it is unique, or set replace_all.")
	}
	line := strings.Count(text[:strings.Index(text, old)], "\n") + 1
	if in.ReplaceAll {
		return strings.ReplaceAll(text, old, repl), n, line, nil
	}
	return strings.Replace(text, old, repl, 1), 1, line, nil
}

// crlf turns bare LFs into CRLF, leaving CRLFs as they are.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// lock holds a file for one write: two edits at once would both check
// the same content and one would be lost.
func (w *workspace) lock(real string) func() {
	w.mu.Lock()
	if w.locks == nil {
		w.locks = map[string]*fileLock{}
	}
	l := w.locks[real]
	if l == nil {
		l = &fileLock{}
		w.locks[real] = l
	}
	l.users++
	w.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		w.mu.Lock()
		if l.users--; l.users == 0 {
			delete(w.locks, real)
		}
		w.mu.Unlock()
	}
}

type fileLock struct {
	sync.Mutex
	users int
}

// target is where a write goes: its directory, opened through the grant,
// checked where it really is, and the file's name in it.
type target struct {
	dir       *os.Root
	name      string
	real      string // the file's real path
	exists    bool
	mode      os.FileMode
	confirmed bool // a protected path the host already confirmed
}

func (t *target) close() {
	if err := t.dir.Close(); err != nil {
		return // read-only handle
	}
}

// target resolves path for writing. The directory is opened through the
// grant (os.Root: no escape by links) and then asked where it really is;
// the name must not be a link; nothing excluded is written, and anything
// protected only with the host's confirmation.
func (w *workspace) target(ctx context.Context, path string, create bool) (*target, error) {
	abs, err := w.resolve(ctx, path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(abs)
	if name == "." || name == string(filepath.Separator) {
		return nil, agent.ToolError(ErrInput, "Give a file path, not a directory.")
	}
	if err := shortName(name); err != nil {
		return nil, err
	}
	// Checked before anything is created: where the file would really be.
	confirmed, err := w.vet(ctx, prospective(abs))
	if err != nil {
		return nil, err
	}
	dir, realDir, err := w.writeDir(filepath.Dir(abs), create)
	if err != nil {
		return nil, err
	}
	t := &target{dir: dir, name: name, real: filepath.Join(realDir, name), confirmed: confirmed}
	if err := w.check(ctx, t); err != nil {
		t.close()
		return nil, err
	}
	return t, nil
}

// prospective is where abs would really be: its deepest existing
// ancestor with links resolved, and the rest of the path after it.
func prospective(abs string) string {
	rest := ""
	for p := abs; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(p) == p {
			return abs
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// vet refuses an excluded path and confirms a protected one; it reports
// whether it confirmed.
func (w *workspace) vet(ctx context.Context, real string) (bool, error) {
	if w.policy.excluded(real) {
		return false, excludedError()
	}
	if !w.protects(real) {
		return false, nil
	}
	return true, w.confirm(ctx, real)
}

// writeDir opens dir through its grant, creating it when asked, and
// returns it with its real location, verified.
func (w *workspace) writeDir(dir string, create bool) (*os.Root, string, error) {
	top, rel, err := w.grantRoot(dir)
	if err != nil {
		return nil, "", err
	}
	defer closeRoot(top)
	if create && rel != "." {
		if err := top.MkdirAll(rel, 0o755); err != nil {
			return nil, "", statError(err)
		}
	}
	root, err := top.OpenRoot(rel)
	if err != nil {
		return nil, "", statError(err)
	}
	real, err := realDir(root)
	if err != nil {
		closeRoot(root)
		return nil, "", err
	}
	return root, real, nil
}

// grantRoot opens the grant holding dir and gives dir relative to it.
func (w *workspace) grantRoot(dir string) (*os.Root, string, error) {
	grant, ok := w.grantOf(dir)
	if !ok {
		return nil, "", w.outside()
	}
	rel, err := filepath.Rel(grant, dir)
	if err != nil {
		return nil, "", w.outside()
	}
	top, err := os.OpenRoot(grant)
	if err != nil {
		return nil, "", agent.ToolError(ErrIO, "The granted directory cannot be opened.")
	}
	return top, rel, nil
}

// realDir is where an opened directory really is, checked: inside a
// grant, not excluded.
func realDir(root *os.Root) (string, error) {
	f, err := root.Open(".")
	if err != nil {
		return "", statError(err)
	}
	defer closeQuiet(f)
	return fdPath(f)
}

// check vets the target: inside a grant, not excluded, not a link or
// anything but a regular file, protected only if the host confirms.
func (w *workspace) check(ctx context.Context, t *target) error {
	if _, ok := w.grantOf(filepath.Dir(t.real)); !ok {
		return agent.ToolError(ErrOutside, "That path leads outside the granted directories.")
	}
	if w.policy.excluded(t.real) {
		return excludedError()
	}
	if err := w.existing(t); err != nil {
		return err
	}
	if w.protects(t.real) && !t.confirmed {
		return w.confirm(ctx, t.real) // the real place turned out protected
	}
	return nil
}

// existing looks at what is at the name: nothing, or a regular file the
// user did not make read-only.
func (w *workspace) existing(t *target) error {
	info, err := t.dir.Lstat(t.name)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return statError(err)
	case !info.Mode().IsRegular():
		return agent.ToolError(ErrInput, "That is not a regular file (a link, a directory or a device): it is not written.")
	case info.Mode().Perm()&0o200 == 0:
		return agent.ToolError(ErrInput, w.display(t.real)+" is read-only: the user made it so; ask before changing that.")
	}
	t.exists, t.mode = true, info.Mode().Perm()
	return nil
}

// confirm asks the host about a protected path; without a way to ask,
// it is refused.
func (w *workspace) confirm(ctx context.Context, real string) error {
	refused := agent.ToolError(ErrProtected, w.display(real)+" holds people's own rules or settings: it is written only with the user's confirmation, and it was not given.")
	if w.confirmProtected == nil {
		return refused
	}
	ok, err := w.confirmProtected(ctx, real)
	if err != nil {
		return errors.Wrap(err, ErrIO, "asking for confirmation failed")
	}
	if !ok {
		return refused
	}
	return nil
}

// current reads the file as it is now and checks it is what the model
// last saw: read, and not changed since; whole, to replace it whole.
func (w *workspace) current(t *target, whole bool) ([]byte, error) {
	f, err := t.dir.OpenFile(t.name, os.O_RDONLY|nonBlock|lockfile.NoFollow, 0)
	if err != nil {
		return nil, statError(err)
	}
	defer closeQuiet(f)
	data, err := io.ReadAll(io.LimitReader(f, maxWrite+1))
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot read the file")
	}
	if len(data) > maxWrite {
		return nil, agent.ToolError(ErrInput, "The file is larger than 10 MiB: it is not edited here.")
	}
	seen, ok := w.seenStamp(t.real)
	switch {
	case !ok:
		return nil, agent.ToolError(ErrNotRead, "Read "+w.display(t.real)+" with read_file before changing it.")
	case seen.sum != sha256.Sum256(data):
		return nil, agent.ToolError(ErrChanged, w.display(t.real)+" changed since you read it (the user or another program edited it): read it again.")
	case whole && !seen.whole:
		return nil, agent.ToolError(ErrNotRead, "You have seen only part of "+w.display(t.real)+": read it whole before replacing it, or change parts of it with edit_file.")
	}
	return data, nil
}

// commit writes data whole: a temporary file in the same directory,
// synced, then renamed over the name, keeping the file's permissions. A
// reader sees the old content or the new, never half of it.
func (w *workspace) commit(ctx context.Context, t *target, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mode := t.mode
	if !t.exists {
		mode = 0o644
	}
	// Short whatever the name: a 255-byte name must still be writable.
	tmp := ".ringil-" + strings.ToLower(rand.Text()[:12]) + ".tmp"
	err := writeTemp(t.dir, tmp, data, mode)
	if err == nil && t.exists {
		err = t.dir.Chmod(tmp, mode) // the umask may have narrowed it
	}
	if err == nil {
		err = t.dir.Rename(tmp, t.name)
	}
	if err != nil {
		_ = t.dir.Remove(tmp)
		return errors.Wrap(err, ErrIO, "cannot write the file")
	}
	w.remember(t.real, stamp{sum: sha256.Sum256(data), whole: true})
	return nil
}

// writeTemp writes data to a new file, synced.
func writeTemp(dir *os.Root, name string, data []byte, mode os.FileMode) error {
	f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// remember records what the model knows a file holds.
func (w *workspace) remember(real string, s stamp) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]stamp{}
	}
	w.seen[real] = s
}

func (w *workspace) seenStamp(real string) (stamp, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.seen[real]
	return s, ok
}

func closeRoot(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}

func closeQuiet(f *os.File) {
	if err := f.Close(); err != nil {
		return
	}
}
