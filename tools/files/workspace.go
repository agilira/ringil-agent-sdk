// workspace.go: directories granted to the agent's file tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package files gives an agent tools for the directories the host grants:
// read_file, list_directory, glob and grep to read, write_file and
// edit_file to change.
//
// Changes follow three rules. A file is changed only if the model read it
// and it has not changed since (someone else's edit is never lost). A
// write is whole or nothing: a temporary file renamed over the old one.
// Protected paths (DefaultProtected: other agents' rules, editor and CI
// settings, .git) are written only with Config.ConfirmProtected's yes.
//
// Paths may be absolute host paths inside any granted directory, or
// relative to the first one, so "read /home/me/notes/todo.md" works as the
// user said it. A path outside every grant is either refused or, when the
// host sets Config.Ask, offered to the user.
//
// Security does not rest on the path the model asked for. Every file and
// directory is opened through os.Root and then asked where it really is
// (/proc/self/fd, F_GETPATH, GetFinalPathNameByHandle); that real path must
// lie inside a grant and outside the excluded secrets. A symlink with an
// innocent name, or a file swapped for a link mid-search, therefore reads
// nothing it should not.
package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ErrConfig is returned for an unusable workspace configuration.
const ErrConfig errors.ErrorCode = "FILES_CONFIG"

// Model-facing error codes.
const (
	ErrOutside  errors.ErrorCode = "FILE_OUTSIDE"
	ErrExcluded errors.ErrorCode = "FILE_EXCLUDED"
	ErrNotFound errors.ErrorCode = "FILE_NOT_FOUND"
	ErrInput    errors.ErrorCode = "FILE_INPUT"
	ErrBinary   errors.ErrorCode = "FILE_BINARY"
	ErrIO       errors.ErrorCode = "FILE_IO"
)

// Config grants directories to the file tools.
type Config struct {
	// Dirs are the granted directories; relative paths in tool calls are
	// resolved against the first one.
	Dirs []string
	// Exclude replaces DefaultExclude when not nil; an empty, non-nil
	// slice excludes nothing.
	Exclude []string
	// Ask, when set, is called for a path outside every granted directory
	// with the directory that would be granted; true grants it for the
	// workspace's life. The CLI asks in the terminal, an app shows a
	// dialog. Nil refuses such paths. Excluded directories are never
	// offered.
	Ask func(ctx context.Context, dir string) (bool, error)
	// Protected replaces DefaultProtected when not nil.
	Protected []string
	// ProtectedFiles are files protected as well, by their exact path, not
	// as patterns: a path with [ or * in it means itself. They are
	// matched where they really are (through symbolic links), as the
	// file system compares names (case, a Windows drive).
	ProtectedFiles []string
	// ConfirmProtected is asked before every write to a protected path,
	// whatever else was allowed; nil refuses them.
	ConfirmProtected func(ctx context.Context, path string) (bool, error)
}

// Workspace is a set of granted directories.
type Workspace interface {
	// ReadTools returns read_file, list_directory, glob and grep.
	ReadTools() []agent.Tool
	// WriteTools returns write_file and edit_file. Hosts usually confirm
	// these calls (the CLI does unless --accept-edits).
	WriteTools() []agent.Tool
	// Dirs returns the granted directories, resolved.
	Dirs() []string
}

// New checks the configuration and resolves the granted directories.
func New(cfg Config) (Workspace, error) {
	if len(cfg.Dirs) == 0 {
		return nil, errors.New(ErrConfig, "grant at least one directory")
	}
	patterns := cfg.Exclude
	if patterns == nil {
		patterns = DefaultExclude
	}
	pol, err := newPolicy(patterns)
	if err != nil {
		return nil, errors.New(ErrConfig, "bad exclude pattern")
	}
	protectedPatterns := cfg.Protected
	if protectedPatterns == nil {
		protectedPatterns = DefaultProtected
	}
	prot, err := newPolicy(protectedPatterns)
	if err != nil {
		return nil, errors.New(ErrConfig, "bad protected pattern")
	}
	w := &workspace{policy: pol, ask: cfg.Ask, protected: prot, files: exactFiles(cfg.ProtectedFiles), confirmProtected: cfg.ConfirmProtected}
	for _, d := range cfg.Dirs {
		real, err := resolveDir(d)
		if err != nil {
			return nil, err
		}
		if pol.excluded(real) {
			return nil, errors.New(ErrConfig, "granted directory is excluded by policy: "+d)
		}
		w.dirs = append(w.dirs, real)
	}
	return w, nil
}

// exactFiles indexes ProtectedFiles by where they are: their path and,
// when it differs, their real one.
func exactFiles(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		out[fileKey(abs)] = true
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			out[fileKey(real)] = true
		} else if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
			out[fileKey(filepath.Join(dir, filepath.Base(abs)))] = true // not made yet
		}
	}
	return out
}

func fileKey(abs string) string { return strings.Join(components(abs), "/") }

// protects: real is protected, by a pattern or as one of ProtectedFiles.
func (w *workspace) protects(real string) bool {
	return w.protected.excluded(real) || w.files[fileKey(real)]
}

func resolveDir(d string) (string, error) {
	abs, err := filepath.Abs(d)
	if err != nil {
		return "", errors.New(ErrConfig, "cannot resolve "+d)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New(ErrConfig, "granted directory does not exist: "+d)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", errors.New(ErrConfig, "granted path is not a directory: "+d)
	}
	return real, nil
}

type workspace struct {
	policy           policy
	ask              func(context.Context, string) (bool, error)
	protected        policy
	files            map[string]bool // ProtectedFiles, by their components
	confirmProtected func(context.Context, string) (bool, error)
	// mu guards dirs, which grows when the user grants a directory; it is
	// held while asking, so one request is never asked twice.
	mu   sync.Mutex
	dirs []string
	// seen is what the model knows each file holds: read or written last.
	seen map[string]stamp
	// locks hold a file during one write.
	locks map[string]*fileLock
}

func (w *workspace) Dirs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.dirs...)
}

func (w *workspace) first() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dirs[0]
}

// dirList describes the grants in tool descriptions.
func (w *workspace) dirList() string {
	dirs := w.Dirs()
	// The first is the working directory: said so, a model that is not
	// told searches the home first (measured 21/11: a minute lost).
	list := dirs[0] + " (the working directory: relative paths and searches start here)"
	if len(dirs) > 1 {
		list += ", " + strings.Join(dirs[1:], ", ")
	}
	if w.ask != nil {
		list += " (for any other directory use its absolute path: the user will be asked)"
	}
	return list
}

// display is how a real path is shown to the model: relative to the first
// grant when inside it, absolute otherwise, so it can be passed back as is.
func (w *workspace) display(real string) string {
	first := w.first()
	if real == first {
		return "."
	}
	if rel, err := filepath.Rel(first, real); err == nil && filepath.IsLocal(rel) {
		return quoteName(filepath.ToSlash(rel))
	}
	return quoteName(real)
}

// grantOf returns the granted directory holding the real path p.
func (w *workspace) grantOf(p string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.grantLocked(p)
}

func (w *workspace) grantLocked(p string) (string, bool) {
	for _, d := range w.dirs {
		if rel, err := filepath.Rel(d, p); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return d, true
		}
	}
	return "", false
}

// resolve turns a path from a tool call into an absolute candidate inside
// a grant, asking for a new grant when needed. The candidate is only a
// starting point: open checks where the opened file really is.
func (w *workspace) resolve(ctx context.Context, p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", agent.ToolError(ErrInput, "Give a file or directory path.")
	}
	abs := filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.first(), abs)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if w.policy.excluded(abs) {
		return "", excludedError()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.grantLocked(abs); ok {
		return abs, nil
	}
	return abs, w.request(ctx, abs)
}

// request asks the host to grant the directory holding p (p itself if it
// is a directory). Called with mu held.
func (w *workspace) request(ctx context.Context, p string) error {
	dir, err := nearestDir(p)
	if w.ask == nil || err != nil || w.policy.excluded(dir) {
		return w.outsideLocked()
	}
	ok, err := w.ask(ctx, dir)
	if err != nil {
		return errors.Wrap(err, ErrIO, "asking for access failed")
	}
	if !ok {
		return agent.ToolError(ErrOutside, "The user did not allow access to "+quoteName(dir)+".")
	}
	w.dirs = append(w.dirs, dir)
	return nil
}

// nearestDir is p if it is a directory, else its closest existing parent,
// with symlinks resolved so the grant is the real location.
func nearestDir(p string) (string, error) {
	for {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return filepath.EvalSymlinks(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", os.ErrNotExist
		}
		p = parent
	}
}

func (w *workspace) outside() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.outsideLocked()
}

func (w *workspace) outsideLocked() error {
	return agent.ToolError(ErrOutside, "That path is outside the directories you can access: "+strings.Join(w.dirs, ", ")+
		". Relative paths are resolved against "+w.dirs[0]+".")
}

func excludedError() error {
	return agent.ToolError(ErrExcluded, "That path is excluded by policy (it may hold secrets).")
}

// ReadTools implements Workspace.
func (w *workspace) ReadTools() []agent.Tool {
	return []agent.Tool{w.readTool(), w.listTool(), w.globTool(), w.grepTool()}
}
