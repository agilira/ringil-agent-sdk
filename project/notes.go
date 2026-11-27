// notes.go: a project's notes, for a host that shows them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	stderrors "errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
)

// Bounds of the notes.
const (
	maxNote  = 256 << 10
	maxNotes = 1000
)

// notePath is a note's path in the folder: words of letters, digits,
// spaces, . _ -, joined by /, ending .md.
var notePath = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9 ._-]{0,79}/){0,4}[A-Za-z0-9][A-Za-z0-9 ._-]{0,79}\.md$`)

func checkNote(p string) error {
	if !notePath.MatchString(p) || path.Clean(p) != p || strings.Contains(p, "..") {
		return errors.New(ErrInput, "a note's path is up to 5 names of letters, digits, spaces, . _ -, joined by /, ending .md")
	}
	return nil
}

// openNotesRead opens the project's notes folder if it exists; nil when
// the project has no notes folder yet.
func (s *Store) openNotesRead(id string) (*os.Root, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	r, err := s.root.OpenRoot(filepath.Join(id, notesDir))
	if stderrors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the notes of "+id)
	}
	return r, nil
}

// writableNotes checks that id is writable, then opens or creates its
// notes folder.
func (s *Store) writableNotes(id string) (*os.Root, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if err := writable(p); err != nil {
		return nil, err
	}
	return s.openNotes(id)
}

// openWritableNotesExisting checks that id is writable and opens its
// notes folder without creating it.
func (s *Store) openWritableNotesExisting(id string) (*os.Root, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if err := writable(p); err != nil {
		return nil, err
	}
	r, err := s.root.OpenRoot(filepath.Join(id, notesDir))
	if stderrors.Is(err, fs.ErrNotExist) {
		return nil, errors.New(ErrNotFound, "no notes in "+id)
	}
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the notes of "+id)
	}
	return r, nil
}

// openNotes opens the notes folder, made if it is not there.
func (s *Store) openNotes(id string) (*os.Root, error) {
	dir := filepath.Join(id, notesDir)
	if err := s.root.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot make the notes of "+id)
	}
	r, err := s.root.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the notes of "+id)
	}
	return r, nil
}

// listNotes walks an open notes root and returns sorted note paths.
func listNotes(r *os.Root) ([]string, error) {
	var out []string
	err := fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return collectNote(&out, p, d)
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Notes lists the paths of the project's notes, sorted.
func (s *Store) Notes(id string) ([]string, error) {
	r, err := s.openNotesRead(id)
	if err != nil || r == nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	out, err := listNotes(r)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot list the notes of "+id)
	}
	return out, nil
}

// collectNote adds p to out when it is a note; hidden folders are left.
func collectNote(out *[]string, p string, d fs.DirEntry) error {
	switch {
	case d.IsDir() && p != "." && strings.HasPrefix(d.Name(), "."):
		return fs.SkipDir
	case d.Type().IsRegular() && checkNote(p) == nil:
		*out = append(*out, p)
	}
	if len(*out) > maxNotes {
		return errors.New(ErrInput, "more than 1000 notes")
	}
	return nil
}

// Note reads a note.
func (s *Store) Note(id, p string) (string, error) {
	if err := checkNote(p); err != nil {
		return "", err
	}
	r, err := s.openNotesRead(id)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", errors.New(ErrNotFound, "no note "+p)
	}
	defer func() { _ = r.Close() }()
	if info, err := r.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return "", errors.New(ErrNotFound, "no note "+p+" (a folder)")
	}
	data, err := atomicfile.Read(r, p, maxNote)
	switch {
	case stderrors.Is(err, fs.ErrNotExist):
		return "", errors.New(ErrNotFound, "no note "+p)
	case err != nil:
		return "", errors.Wrap(err, ErrStore, "cannot read note "+p)
	}
	return string(data), nil
}

// noteEvent validates and returns the Note timeline event before disk edits.
func noteEvent(actor, body string) (Event, error) {
	if actor == "" {
		actor = "system"
	}
	ev := Event{Actor: actor, Kind: Note, Text: body}
	return ev, ev.check()
}

// WriteNote writes a note whole, by actor, and records it.
func (s *Store) WriteNote(id, p, body, actor string) error {
	if err := checkNote(p); err != nil {
		return err
	}
	if !text(body, maxNote) {
		return errors.New(ErrInput, "a note is UTF-8 text of at most 256 KiB")
	}
	ev, err := noteEvent(actor, "wrote "+p)
	if err != nil {
		return err
	}
	return s.locked(id, func() error {
		r, err := s.writableNotes(id)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		if err := writeNoteFile(r, p, body); err != nil {
			return err
		}
		_, err = s.appendEvent(id, ev)
		return err
	})
}

func writeNoteFile(r *os.Root, p, body string) error {
	if err := noteRoomRoot(r, p); err != nil {
		return err
	}
	if err := noteFits(r, p); err != nil {
		return err
	}
	if dir := path.Dir(p); dir != "." {
		if err := r.MkdirAll(dir, 0o700); err != nil {
			return errors.Wrap(err, ErrStore, "cannot make the folder of note "+p)
		}
	}
	if err := atomicfile.Write(r, p, []byte(body)); err != nil {
		return errors.Wrap(err, ErrStore, "cannot write note "+p)
	}
	return nil
}

// noteFits: no folder is where the note goes, no note where one of its
// folders goes.
func noteFits(r *os.Root, p string) error {
	if info, err := r.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return errors.New(ErrInput, p+" is a folder of notes, not a note")
	}
	for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
		if info, err := r.Lstat(dir); err == nil && !info.IsDir() {
			return errors.New(ErrInput, dir+" is a note, not a folder")
		}
	}
	return nil
}

// noteRoomRoot: p is a note there, or there is room for one more.
func noteRoomRoot(r *os.Root, p string) error {
	list, err := listNotes(r)
	if err != nil {
		return err
	}
	if len(list) >= maxNotes && !slices.Contains(list, p) {
		return errors.New(ErrInput, "a project holds at most 1000 notes")
	}
	return nil
}

// RemoveNote removes a note, by actor, and records it.
func (s *Store) RemoveNote(id, p, actor string) error {
	if err := checkNote(p); err != nil {
		return err
	}
	ev, err := noteEvent(actor, "removed "+p)
	if err != nil {
		return err
	}
	return s.locked(id, func() error {
		r, err := s.openWritableNotesExisting(id)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		if info, err := r.Lstat(p); err == nil && !info.Mode().IsRegular() {
			return errors.New(ErrNotFound, "no note "+p+" (a folder)")
		}
		if err := r.Remove(p); err != nil {
			if stderrors.Is(err, fs.ErrNotExist) {
				return errors.New(ErrNotFound, "no note "+p)
			}
			return errors.Wrap(err, ErrStore, "cannot remove note "+p)
		}
		_, err = s.appendEvent(id, ev)
		return err
	})
}
