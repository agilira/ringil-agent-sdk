// project.go: projects several agents share — a board, a timeline, notes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package project keeps projects that several agents, and people, work
// on in turn: each has a brief, a board of cards in columns, a timeline of
// what happened (who did what, when) and a folder of notes. An agent that
// works on a project gets its Context at every run, so the next agent
// knows what the last one did without being told.
//
// A project is a private directory of its own under the store's root:
//
//	<root>/<id>/project.json   name, brief, columns, working folder
//	<root>/<id>/cards.json     the board's cards, in order
//	<root>/<id>/timeline.jsonl what happened, one event a line, only added to
//	<root>/<id>/notes/         Markdown notes (package notes, scope "project")
//
// Every change is written whole (a reader never sees half of it), under
// the project's lock, so that several processes may share a store.
package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// Error codes.
const (
	// ErrInput: a project, card or event is not valid.
	ErrInput errors.ErrorCode = "PROJECT_INPUT"
	// ErrNotFound: no such project or card.
	ErrNotFound errors.ErrorCode = "PROJECT_NOT_FOUND"
	// ErrStore: the store cannot be read or written.
	ErrStore errors.ErrorCode = "PROJECT_STORE"
	// ErrExists: a project of that id is there already.
	ErrExists errors.ErrorCode = "PROJECT_EXISTS"
)

// Bounds.
const (
	MaxProjects = 1024
	MaxCards    = 1000
	MaxColumns  = 12
	maxName     = 120
	maxBrief    = 16 << 10
	maxTitle    = 200
	maxText     = 16 << 10
	maxFile     = 8 << 20 // project.json, cards.json
	lockWait    = 10 * time.Second
)

// Files of a project.
const (
	projectFile  = "project.json"
	cardsFile    = "cards.json"
	timelineFile = "timeline.jsonl"
	notesDir     = "notes"
	lockFile     = "lock"
)

// Project is what a project is about and how its board is laid out.
type Project struct {
	// ID names it: 1-80 letters, digits, . _ -, from a letter or digit.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Brief is what every agent of the project is told it is for.
	Brief   string   `json:"brief,omitempty"`
	Columns []Column `json:"columns"`
	// Folder, when set, is the absolute path of the files the project
	// works on (a repository, a shared folder).
	Folder   string    `json:"folder,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Archived bool      `json:"archived,omitempty"`
}

// Column is a column of the board.
type Column struct {
	Name string `json:"name"`
	// Auto: a card with an agent that enters it (made there, moved there,
	// or given an agent there) is that agent's to start on: the host
	// starts its run (OnChange tells it).
	Auto bool `json:"auto,omitempty"`
}

// DefaultColumns are a board's columns when none are given: work handed
// to an agent in "To do" starts by itself.
func DefaultColumns() []Column {
	return []Column{{Name: "To do", Auto: true}, {Name: "In progress"}, {Name: "Review"}, {Name: "Done"}}
}

var (
	validID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	validCard = regexp.MustCompile(`^c[a-z0-9]{6,32}$`)
	validWho  = regexp.MustCompile(`^[A-Za-z0-9_.@ -]{0,64}$`)
)

// Check validates a project as the store keeps it.
func (p Project) Check() error {
	switch {
	case !validID.MatchString(p.ID):
		return errors.New(ErrInput, "a project's id is 1-80 letters, digits, . _ -, from a letter or digit")
	case !text(p.Name, maxName) || strings.TrimSpace(p.Name) == "":
		return errors.New(ErrInput, "a project's name is 1-120 bytes of text")
	case !text(p.Brief, maxBrief):
		return errors.New(ErrInput, "a project's brief is at most 16 KiB of text")
	case p.Folder != "" && !filepath.IsAbs(p.Folder):
		return errors.New(ErrInput, "a project's folder is an absolute path")
	}
	return checkColumns(p.Columns)
}

func checkColumns(cols []Column) error {
	if len(cols) == 0 || len(cols) > MaxColumns {
		return errors.New(ErrInput, "a board has 1 to 12 columns")
	}
	seen := map[string]bool{}
	for _, c := range cols {
		key := strings.ToLower(strings.TrimSpace(c.Name))
		if key == "" || !text(c.Name, 40) || seen[key] {
			return errors.New(ErrInput, "columns have distinct names of 1-40 bytes")
		}
		seen[key] = true
	}
	return nil
}

// column is the board's column named name (any case), and whether there
// is one.
func (p Project) column(name string) (Column, bool) {
	for _, c := range p.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Column{}, false
}

// text: s is UTF-8 of at most max bytes, without NUL.
func text(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// Store keeps projects in a private directory, one directory each.
type Store struct {
	root *os.Root
	dir  string
	now  func() time.Time
}

// Open opens, creating it private, the store in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot create the projects' directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the projects' directory")
	}
	info, err := root.Stat(".")
	if err == nil {
		err = lockfile.CheckPrivate(info)
	}
	if err != nil {
		_ = root.Close() // #nosec G104 -- refusing it anyway
		return nil, errors.Wrap(err, ErrStore, "the projects' directory must be private to you")
	}
	return &Store{root: root, dir: dir, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Close releases the store.
func (s *Store) Close() error { return s.root.Close() }

// NotesDir is the folder of a project's notes, for package notes (a
// scope named "project").
func (s *Store) NotesDir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New(ErrNotFound, "no project "+id)
	}
	return filepath.Join(s.dir, id, notesDir), nil
}

// NewID is a new project's id from its name: its words, and a random part.
func NewID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case b.Len() >= 40:
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "project"
	}
	return slug + "-" + strings.ToLower(rand.Text()[:6])
}

// Create makes a project: its id from its name when empty, the default
// columns when none. ErrExists when a project of that id is there.
func (s *Store) Create(p Project, actor string) (Project, error) {
	p = s.fresh(p)
	if err := p.Check(); err != nil {
		return Project{}, err
	}
	unlock, err := s.storeLock()
	if err != nil {
		return Project{}, err
	}
	defer unlock()
	if err := s.room(p.ID); err != nil {
		return Project{}, err
	}
	// Its directory may be there with notes alone: the folder's project
	// of before, made a project at its first use.
	if err := s.root.Mkdir(p.ID, 0o700); err != nil && !stderrors.Is(err, fs.ErrExist) {
		return Project{}, errors.Wrap(err, ErrStore, "cannot make project "+p.ID)
	}
	err = s.locked(p.ID, func() error {
		if _, err := s.root.Stat(filepath.Join(p.ID, projectFile)); err == nil {
			return errors.New(ErrExists, "a project "+p.ID+" is there already") // made meanwhile
		}
		if err := s.write(p.ID, projectFile, p); err != nil {
			return err
		}
		_, err := s.appendEvent(p.ID, Event{Actor: actor, Kind: ProjectCreated, Text: p.Name})
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

// fresh is p as it is made: its id from its name when it has none, the
// default columns when none, the times now.
func (s *Store) fresh(p Project) Project {
	if p.ID == "" {
		p.ID = NewID(p.Name)
	}
	if len(p.Columns) == 0 {
		p.Columns = DefaultColumns()
	}
	p.Created, p.Updated = s.now(), s.now()
	return p
}

// writable: p takes changes (an archived project takes none but its
// unarchiving, through Update).
func writable(p Project) error {
	if p.Archived {
		return errors.New(ErrInput, "project "+p.ID+" is archived: unarchive it first")
	}
	return nil
}

// room: no project has the id, in any case, and the store has room.
func (s *Store) room(id string) error {
	ids, err := s.ids()
	if err != nil {
		return err
	}
	for _, other := range ids {
		if strings.EqualFold(other, id) {
			return errors.New(ErrExists, "a project "+other+" is there already")
		}
	}
	if err := s.dirCaseClash(id); err != nil {
		return err
	}
	if len(ids) >= MaxProjects {
		return errors.New(ErrInput, "the store holds at most 1024 projects")
	}
	return nil
}

// dirCaseClash refuses an id that matches an existing directory in
// another case (for instance a folder of notes alone).
func (s *Store) dirCaseClash(id string) error {
	f, err := s.root.Open(".")
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot list the projects")
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot list the projects")
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != id && strings.EqualFold(e.Name(), id) {
			return errors.New(ErrExists, "a project "+e.Name()+" is there already")
		}
	}
	return nil
}

// Ensure is the project id, made with p when it is not there: the
// project of a folder, made at its first use.
func (s *Store) Ensure(p Project, actor string) (Project, error) {
	got, err := s.Get(p.ID)
	if !errors.HasCode(err, ErrNotFound) {
		return got, err
	}
	got, err = s.Create(p, actor)
	if errors.HasCode(err, ErrExists) {
		return s.Get(p.ID) // made meanwhile, by another process
	}
	return got, err
}

// Get reads a project.
func (s *Store) Get(id string) (Project, error) {
	if !validID.MatchString(id) {
		return Project{}, errors.New(ErrNotFound, "no project "+id)
	}
	var p Project
	if err := s.read(id, projectFile, &p); err != nil {
		return Project{}, err
	}
	switch {
	case strings.EqualFold(p.ID, id) && p.ID != id: // another case of its id, on a system that does not tell them apart
		return Project{}, errors.New(ErrNotFound, "no project "+id+" (there is "+p.ID+")")
	case p.ID != id:
		return Project{}, errors.New(ErrStore, "project "+id+" is not valid: fix or remove "+filepath.Join(id, projectFile))
	}
	return p, nil
}

// List returns the projects, the latest changed first. One that cannot be
// read is an error: a list that left it out would hide it.
func (s *Store) List() ([]Project, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(ids))
	for _, id := range ids {
		p, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Update changes a project through change, which may edit its name,
// brief, columns, folder and archived flag; the rest stays. A column
// removed must hold no card.
func (s *Store) Update(id, actor string, change func(*Project) error) (Project, error) {
	var out Project
	err := s.locked(id, func() error {
		p, err := s.Get(id)
		if err != nil {
			return err
		}
		next := p
		next.Columns = append([]Column(nil), p.Columns...)
		if err := change(&next); err != nil {
			return err
		}
		next.ID, next.Created, next.Updated = p.ID, p.Created, s.now()
		if err := s.validateUpdate(id, p, next); err != nil {
			return err
		}
		if err := s.write(id, projectFile, next); err != nil {
			return err
		}
		out = next
		_, err = s.appendEvent(id, Event{Actor: actor, Kind: ProjectChanged, Text: changes(p, next)})
		return err
	})
	return out, err
}

func (s *Store) validateUpdate(id string, was, next Project) error {
	if was.Archived && next.Archived {
		return writable(was)
	}
	if err := next.Check(); err != nil {
		return err
	}
	return s.columnsHoldCards(id, next)
}

// columnsHoldCards: every card is in a column of next.
func (s *Store) columnsHoldCards(id string, next Project) error {
	cards, err := s.cards(id)
	if err != nil {
		return err
	}
	for _, c := range cards {
		if _, ok := next.column(c.Column); !ok {
			return errors.New(ErrInput, "column "+c.Column+" still holds cards: move them first")
		}
	}
	return nil
}

// changes says in a line what an update changed.
func changes(a, b Project) string {
	var what []string
	if a.Name != b.Name {
		what = append(what, "name")
	}
	if a.Brief != b.Brief {
		what = append(what, "brief")
	}
	if a.Folder != b.Folder {
		what = append(what, "folder")
	}
	if a.Archived != b.Archived {
		what = append(what, map[bool]string{true: "archived", false: "unarchived"}[b.Archived])
	}
	if len(a.Columns) != len(b.Columns) || !sameColumns(a.Columns, b.Columns) {
		what = append(what, "columns")
	}
	if len(what) == 0 {
		return "nothing"
	}
	return strings.Join(what, ", ")
}

func sameColumns(a, b []Column) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Remove deletes a project with everything in it: its cards, timeline
// and notes.
func (s *Store) Remove(id string) error {
	unlock, err := s.storeLock()
	if err != nil {
		return err
	}
	defer unlock()
	// Its lock taken and let go: no change of it is halfway. Its folder is
	// then moved aside whole (a name no project has), and only then
	// removed: a move that fails (Windows, a file held open) leaves the
	// project as it was; one that works leaves nothing a project made
	// again with its id would take (audit 27/11).
	if err := s.locked(id, func() error { _, err := s.Get(id); return err }); err != nil {
		return err
	}
	aside := ".removed-" + id + "-" + strings.ToLower(rand.Text()[:6])
	if err := s.root.Rename(id, aside); err != nil {
		return errors.Wrap(err, ErrStore, "cannot remove project "+id+" now (a file of it is in use?): try again")
	}
	if err := s.root.RemoveAll(aside); err != nil {
		return errors.Wrap(err, ErrStore, "project "+id+" is removed; its files are left in "+aside)
	}
	return nil
}

// storeLock holds the store's own lock: projects are made and removed one
// at a time (two ids differing only in case are one on some systems).
func (s *Store) storeLock() (func(), error) {
	f, err := lockfile.Open(s.root, lockFile)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the projects' lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	if err := lockfile.Lock(ctx, f); err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, ErrStore, "the projects are being changed elsewhere")
	}
	return func() { _ = lockfile.Unlock(f); _ = f.Close() }, nil
}

// ids are the projects' ids, from their directories, sorted.
func (s *Store) ids() ([]string, error) {
	f, err := s.root.Open(".")
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot list the projects")
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot list the projects")
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !validID.MatchString(e.Name()) {
			continue
		}
		if _, err := s.root.Stat(filepath.Join(e.Name(), projectFile)); err == nil {
			out = append(out, e.Name()) // a folder of notes alone is not a project yet
		}
	}
	sort.Strings(out)
	return out, nil
}

// locked runs fn holding the project's lock.
func (s *Store) locked(id string, fn func() error) error {
	if !validID.MatchString(id) {
		return errors.New(ErrNotFound, "no project "+id)
	}
	if _, err := s.root.Stat(id); err != nil {
		return errors.New(ErrNotFound, "no project "+id)
	}
	f, err := lockfile.Open(s.root, filepath.Join(id, lockFile))
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot open the lock of project "+id)
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	if err := lockfile.Lock(ctx, f); err != nil {
		return errors.Wrap(err, ErrStore, "project "+id+" is being changed elsewhere")
	}
	defer func() { _ = lockfile.Unlock(f) }()
	return fn()
}

// read decodes a project's file strictly; a file not there is ErrNotFound.
func (s *Store) read(id, name string, v any) error {
	data, err := atomicfile.Read(s.root, filepath.Join(id, name), maxFile)
	switch {
	case stderrors.Is(err, fs.ErrNotExist):
		return errors.New(ErrNotFound, "no project "+id)
	case err != nil:
		return errors.Wrap(err, ErrStore, "cannot read "+filepath.Join(id, name))
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New(ErrStore, filepath.Join(id, name)+" is not valid: fix or remove it")
	}
	return nil
}

func (s *Store) write(id, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot encode "+name)
	}
	if len(data) >= maxFile {
		return errors.New(ErrInput, name+" would be larger than 8 MiB")
	}
	if err := atomicfile.Write(s.root, filepath.Join(id, name), append(data, '\n')); err != nil {
		return errors.Wrap(err, ErrStore, "cannot write "+filepath.Join(id, name))
	}
	return nil
}
