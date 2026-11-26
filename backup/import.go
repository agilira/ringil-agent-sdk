// import.go: an archive back in place of the state, all or nothing
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// ErrBusy: the state is in use by a process, or being imported.
const ErrBusy errors.ErrorCode = "BACKUP_BUSY"

// In the directory the host names for the state's lock (not inside a
// source, which an import moves): the lock, and an import's journal.
const (
	lockName    = ".state.lock"
	journalName = ".import-journal.json"
)

// Use takes a shared lock on the state whose lock lives in dir, for as
// long as the process uses it; release gives it back. Any number of
// processes hold it at once; an import needs every one of them gone.
// An import cut short (a crash, power gone) is undone first: the state
// is as it was before it. ErrBusy while an import runs.
func Use(dir string) (release func() error, err error) {
	for range 3 {
		if hasJournal(dir) {
			if err := recoverImport(dir); err != nil {
				return nil, err
			}
		}
		release, err = lockState(dir, lockfile.TryLockShared, "an import of the state is running: try again when it ends")
		if err != nil {
			return nil, err
		}
		if !hasJournal(dir) {
			return release, nil
		}
		_ = release()
	}
	return nil, errors.New(ErrBusy, "an import of the state is running: try again when it ends")
}

func hasJournal(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, journalName))
	return err == nil
}

// recoverImport undoes an import cut short, under the exclusive lock.
func recoverImport(dir string) error {
	release, err := lockState(dir, lockfile.TryLock, "an import of the state is running: try again when it ends")
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	return recoverLocked(dir)
}

// recoverLocked finishes an import whose journal says Done, else undoes
// it; the caller holds the exclusive lock.
func recoverLocked(dir string) error {
	j, err := readJournal(dir)
	if err != nil || j == nil {
		return err
	}
	if j.Done {
		cleanUp(j.Moves) // it had finished: only the old state is left to remove
	} else if err := rollBack(j.Moves); err != nil {
		return errors.Wrap(err, ErrIO, "an import was cut short and could not be undone ("+err.Error()+"): remove "+filepath.Join(dir, journalName)+", then put the state back from the copy made before it (ringil state import <copy>)")
	}
	return removeJournal(dir)
}

// resolved is sources with each one that is a link replaced by what it
// points to: an import replaces the file or directory, never the link (a
// settings file kept with dotfiles), as an export reads through it.
func resolved(sources map[string]Source) (map[string]Source, error) {
	out := make(map[string]Source, len(sources))
	for n, s := range sources {
		if info, err := os.Lstat(s.Path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(s.Path)
			if err != nil {
				return nil, errors.Wrap(err, ErrIO, n+" is a link that leads nowhere")
			}
			s.Path = real
		}
		out[n] = s
	}
	return out, nil
}

// sweep removes what an import cut short while it extracted left beside
// the sources (before its journal existed).
func sweep(sources map[string]Source) {
	for _, s := range sources {
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(s.Path), "."+filepath.Base(s.Path)+".import-*"))
		for _, p := range left {
			_ = os.RemoveAll(p)
		}
	}
}

func lockState(dir string, try func(*os.File) (bool, error), busy string) (func() error, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot create the state directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot open the state directory")
	}
	defer closeRoot(root)
	f, err := lockfile.Open(root, lockName)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot open the state's lock")
	}
	ok, err := try(f)
	if err != nil || !ok {
		closeFile(f)
		if err != nil {
			return nil, errors.Wrap(err, ErrIO, "cannot lock the state")
		}
		return nil, errors.New(ErrBusy, busy).WithUserMessage(busy)
	}
	return func() error {
		uerr := lockfile.Unlock(f)
		if cerr := f.Close(); uerr == nil {
			uerr = cerr
		}
		return uerr
	}, nil
}

// Options are an import's.
type Options struct {
	// Sources are the state's parts, as for Create.
	Sources map[string]Source
	// LockDir holds the state's lock (see Use): the import runs only when
	// no process holds it.
	LockDir string
	// Before runs once the import has the state to itself, before
	// anything changes: the host exports the state as it is. An error
	// stops the import.
	Before func() error
}

// Imported is what an import did.
type Imported struct {
	Manifest *Manifest
	// Kept are the sources the archive does not hold (it is older than
	// they are): left as they were.
	Kept []string
}

// Import replaces the sources with the archive's, all or nothing: each
// is extracted beside its place, written to disk, and the whole archive
// checked against its manifest (each source once, of its kind, nothing
// outside them); then every source is swapped in, as a journal records,
// and if one swap fails those done are put back. A crash mid-way is
// undone by the next Use. ErrBusy when a process holds the state. A
// source in the archive the host does not name is refused: it comes
// from a version that knows more.
func Import(ctx context.Context, r io.Reader, opts Options) (*Imported, error) {
	for n := range opts.Sources {
		if !sourceName.MatchString(n) {
			return nil, errors.New(ErrInput, "source names are lowercase words: "+n)
		}
	}
	release, err := lockState(opts.LockDir, lockfile.TryLock, "ringil is running (a chat, serve, a trigger): close it, or import from its app")
	if err != nil {
		return nil, err
	}
	defer func() { _ = release() }()
	sources, err := prepare(opts)
	if err != nil {
		return nil, err
	}
	st := &staging{sources: sources, staged: map[string]string{}, tag: strings.ToLower(rand.Text()[:8])}
	m, err := walk(ctx, r, st.write)
	if err == nil {
		err = st.complete(m)
	}
	if err == nil {
		err = st.swap(m, opts.LockDir)
	}
	if err != nil {
		st.discard()
		return nil, err
	}
	return &Imported{Manifest: m, Kept: kept(opts.Sources, m)}, nil
}

// prepare readies the state for an import, under the exclusive lock: an
// import cut short undone first (its old state must not be lost), the
// sources through their links, what an extraction cut short left swept,
// then the host's Before.
func prepare(opts Options) (map[string]Source, error) {
	if err := recoverLocked(opts.LockDir); err != nil {
		return nil, err
	}
	sources, err := resolved(opts.Sources)
	if err != nil {
		return nil, err
	}
	sweep(sources)
	if opts.Before != nil {
		if err := opts.Before(); err != nil {
			return nil, err
		}
	}
	return sources, nil
}

// staging is an import's extracted sources, each beside its place.
type staging struct {
	sources map[string]Source
	staged  map[string]string // source -> where it was extracted
	tag     string
}

// beside is a hidden name next to p, for what an import puts there.
func (st *staging) beside(p, what string) string {
	return filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+"."+what+"-"+st.tag)
}

// write extracts one archive entry: name/<path> into a directory source,
// name alone as a file source; anything else is refused.
func (st *staging) write(name string, body io.Reader) error {
	src, rel, inDir := strings.Cut(name, "/")
	s, ok := st.sources[src]
	switch {
	case !ok:
		return errors.New(ErrInput, "the archive holds "+strconv.Quote(src)+", which this version does not know").
			WithUserMessage("The archive comes from a newer version: update, then import it.")
	case s.File == inDir:
		return errors.New(ErrDamaged, "the archive holds "+strconv.Quote(src)+" as a "+kindOf(inDir)+", which here is a "+s.kind())
	}
	staged, err := st.stagePath(src, s.Path, inDir)
	if err != nil {
		return err
	}
	if !inDir {
		return writeFile(staged, body)
	}
	return writeIn(staged, src, rel, name, body)
}

func (st *staging) stagePath(src, target string, inDir bool) (string, error) {
	if staged := st.staged[src]; staged != "" {
		return staged, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot prepare "+src)
	}
	staged := st.beside(target, "import")
	if inDir {
		if err := os.Mkdir(staged, 0o700); err != nil {
			return "", errors.Wrap(err, ErrIO, "cannot prepare "+src)
		}
	}
	st.staged[src] = staged
	return staged, nil
}

func kindOf(inDir bool) string {
	if inDir {
		return "directory"
	}
	return "regular file"
}

// writeIn writes rel inside a staged directory, through its root.
func writeIn(staged, src, rel, name string, body io.Reader) error {
	root, err := os.OpenRoot(staged)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot prepare "+src)
	}
	defer closeRoot(root)
	if d := filepath.Dir(filepath.FromSlash(rel)); d != "." {
		if err := root.MkdirAll(d, 0o700); err != nil {
			return errors.Wrap(err, ErrIO, "cannot import "+strconv.Quote(name))
		}
	}
	f, err := root.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Wrap(err, ErrDamaged, "the archive repeats or misplaces "+strconv.Quote(name))
	}
	return copyClose(f, body, name)
}

func writeFile(p string, body io.Reader) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- beside a source the host named
	if err != nil {
		return errors.Wrap(err, ErrDamaged, "the archive repeats "+strconv.Quote(filepath.Base(p)))
	}
	return copyClose(f, body, filepath.Base(p))
}

// copyClose writes body to f and to disk, before anything is swapped.
func copyClose(f *os.File, body io.Reader, name string) error {
	_, err := io.Copy(f, body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot import "+strconv.Quote(name))
	}
	return nil
}

// complete checks the manifest's sources: each named once, each known,
// a file source with its file, every source extracted among them. A
// directory source without a file in it is staged empty: it replaces
// what is there.
func (st *staging) complete(m *Manifest) error {
	listed := map[string]bool{}
	for _, src := range m.Sources {
		if err := st.listed(src, listed); err != nil {
			return err
		}
	}
	for src := range st.staged {
		if !listed[src] {
			return errors.New(ErrDamaged, "the archive holds "+strconv.Quote(src)+" without naming it")
		}
	}
	return nil
}

// listed checks one source the manifest names, and stages it empty when
// it is a directory without a file in it.
func (st *staging) listed(src string, listed map[string]bool) error {
	s, ok := st.sources[src]
	switch {
	case listed[src]:
		return errors.New(ErrDamaged, "the archive names "+strconv.Quote(src)+" twice")
	case !ok:
		return errors.New(ErrInput, "the archive holds "+strconv.Quote(src)+", which this version does not know")
	case s.File && st.staged[src] == "":
		return errors.New(ErrDamaged, "the archive names "+strconv.Quote(src)+" without its file")
	}
	listed[src] = true
	_, err := st.stagePath(src, s.Path, true)
	return err
}

// move is one source swapped in: its place, where the old state goes
// (HadOld: there was one), where the new one was staged.
type move struct {
	Target string `json:"target"`
	Old    string `json:"old"`
	HadOld bool   `json:"had_old"`
	Staged string `json:"staged"`
}

// journal is an import's swap, written before it: Done once every source
// is in place.
type journal struct {
	Moves []move `json:"moves"`
	Done  bool   `json:"done"`
}

// swap moves every staged source into place, the old state aside, as the
// journal says; if one fails, those done are put back. The old states go
// once all are in.
func (st *staging) swap(m *Manifest, lockDir string) error {
	names := append([]string(nil), m.Sources...)
	sort.Strings(names)
	j := journal{}
	for _, src := range names {
		target := st.sources[src].Path
		_, err := os.Lstat(target)
		j.Moves = append(j.Moves, move{Target: target, Old: st.beside(target, "old"), HadOld: err == nil, Staged: st.staged[src]})
	}
	if err := writeJournal(lockDir, j); err != nil {
		return err
	}
	for _, mv := range j.Moves {
		if err := swapOne(mv); err != nil {
			if rerr := rollBack(j.Moves); rerr != nil {
				return errors.Wrap(rerr, ErrIO, "an import failed ("+err.Error()+") and could not be undone: the state before it is in the copy made first")
			}
			_ = removeJournal(lockDir)
			return err
		}
	}
	syncParents(j.Moves) // the renames on disk before Done says they are
	j.Done = true
	if err := writeJournal(lockDir, j); err != nil {
		return err
	}
	cleanUp(j.Moves)
	return removeJournal(lockDir)
}

func swapOne(mv move) error {
	if mv.HadOld {
		if err := os.Rename(mv.Target, mv.Old); err != nil {
			return errors.Wrap(err, ErrIO, "cannot move "+filepath.Base(mv.Target)+" aside (is it open somewhere?)")
		}
	}
	if err := os.Rename(mv.Staged, mv.Target); err != nil {
		return errors.Wrap(err, ErrIO, "cannot put "+filepath.Base(mv.Target)+" in place")
	}
	return nil
}

// rollBack puts every source back as it was before the swap, whatever
// part of it was done: the first error, after trying them all.
func rollBack(moves []move) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for i := len(moves) - 1; i >= 0; i-- {
		for _, err := range rollBackOne(moves[i]) {
			keep(err)
		}
	}
	return first
}

// rollBackOne puts one source back; the errors met on the way.
func rollBackOne(mv move) []error {
	var errs []error
	_, oldErr := os.Lstat(mv.Old)
	_, stagedErr := os.Lstat(mv.Staged)
	for _, err := range []error{oldErr, stagedErr} {
		if err != nil && !os.IsNotExist(err) {
			errs = append(errs, err) // cannot tell what was done: said, not taken as undone
		}
	}
	switch {
	case mv.HadOld && oldErr == nil: // moved aside: what is in place is new, or nothing
		errs = append(errs, os.RemoveAll(mv.Target), os.Rename(mv.Old, mv.Target))
	case !mv.HadOld && os.IsNotExist(stagedErr): // the new one went where nothing was
		errs = append(errs, os.RemoveAll(mv.Target))
	}
	return append(errs, os.RemoveAll(mv.Staged))
}

// syncParents writes the directories the renames changed to disk, where
// the system allows it (not Windows: its renames are on disk already).
func syncParents(moves []move) {
	seen := map[string]bool{}
	for _, mv := range moves {
		dir := filepath.Dir(mv.Target)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if d, err := os.Open(dir); err == nil { // #nosec G304 -- a source's parent the host named
			_ = d.Sync()
			_ = d.Close()
		}
	}
}

// cleanUp removes the old states and what was staged: the import is in.
func cleanUp(moves []move) {
	for _, mv := range moves {
		_ = os.RemoveAll(mv.Old) // the host exported it before: a leftover is only space
		_ = os.RemoveAll(mv.Staged)
	}
}

func writeJournal(dir string, j journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the import's journal")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the import's journal")
	}
	defer closeRoot(root)
	if err := atomicfile.Write(root, journalName, data); err != nil {
		return errors.Wrap(err, ErrIO, "cannot write the import's journal")
	}
	return nil
}

func readJournal(dir string) (*journal, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot read the import's journal")
	}
	defer closeRoot(root)
	data, err := atomicfile.Read(root, journalName, 1<<20)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot read the import's journal")
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, errors.New(ErrDamaged, "the import's journal is damaged: remove "+filepath.Join(dir, journalName)+", then put the state back from the copy made before it (ringil state import <copy>)")
	}
	return &j, nil
}

func removeJournal(dir string) error {
	if err := os.Remove(filepath.Join(dir, journalName)); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, ErrIO, "cannot remove the import's journal")
	}
	return nil
}

// discard removes what was staged and not swapped in.
func (st *staging) discard() {
	for _, p := range st.staged {
		_ = os.RemoveAll(p)
	}
}

func kept(sources map[string]Source, m *Manifest) []string {
	in := map[string]bool{}
	for _, s := range m.Sources {
		in[s] = true
	}
	var out []string
	for n := range sources {
		if !in[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
