// local.go: memory kept in a private file, searched with BM25
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package local is an agent.Memory kept in one JSON file in a private
// directory and searched with BM25, the ranking classic search engines
// use: no service, no embeddings, no download. A vector store is another
// implementation of agent.Memory, for when wording varies too much for
// words to match.
//
// Every operation takes an exclusive lock and every change is written to a
// temporary file, synced and renamed over the old one, so concurrent
// processes and crashes never leave a half-written memory.
package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	stderrors "errors"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// maxContent bounds one record: a memory is a fact, not a document.
const maxContent = 32 << 10

const (
	dataFile = "memory.json"
	lockName = "memory.lock"
	version  = 1
)

type document struct {
	Version int                  `json:"version"`
	Records []agent.MemoryRecord `json:"records"`
}

// New returns a memory stored in dir, creating it with mode 0700.
func New(dir string) (agent.Memory, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, agent.ErrMemoryIO, "cannot create the memory directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New(agent.ErrMemoryIO, "memory path is not a directory")
	}
	if err := lockfile.CheckPrivate(info); err != nil {
		return nil, errors.New(agent.ErrMemoryIO, "memory directory is not private to the current user; chmod 700 it")
	}
	return &store{dir: dir, now: time.Now}, nil
}

type store struct {
	dir string
	now func() time.Time
}

// locked runs fn on the current records under the store's lock and writes
// them back when fn reports a change. Every file access goes through an
// os.Root on the directory, held for this operation only.
func (s *store) locked(ctx context.Context, fn func(*document) (bool, error)) error {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory directory")
	}
	err = s.withRoot(ctx, root, fn)
	if cerr := root.Close(); err == nil && cerr != nil {
		err = errors.Wrap(cerr, agent.ErrMemoryIO, "cannot close the memory directory")
	}
	return err
}

func (s *store) withRoot(ctx context.Context, root *os.Root, fn func(*document) (bool, error)) error {
	lock, err := lockfile.Open(root, lockName)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory lock").WithContext("cause", err.Error())
	}
	err = s.withLock(ctx, root, lock, fn)
	if cerr := lock.Close(); err == nil && cerr != nil {
		err = errors.Wrap(cerr, agent.ErrMemoryIO, "cannot release the memory lock")
	}
	return err
}

func (s *store) withLock(ctx context.Context, root *os.Root, lock *os.File, fn func(*document) (bool, error)) error {
	if err := lockfile.Lock(ctx, lock); err != nil {
		return err
	}
	doc, err := read(root)
	if err != nil {
		return err
	}
	changed, err := fn(doc)
	if err != nil || !changed {
		return err
	}
	return write(root, doc)
}

func read(root *os.Root) (*document, error) {
	data, err := readFile(root)
	if os.IsNotExist(err) {
		return &document{Version: version}, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrMemoryIO, "cannot read the memory")
	}
	return decode(data)
}

// maxFile bounds the memory file read into memory; far above real use.
const maxFile = 1 << 30

func readFile(root *os.Root) ([]byte, error) {
	data, err := atomicfile.Read(root, dataFile, maxFile)
	if stderrors.Is(err, atomicfile.ErrNotRegular) {
		return nil, errors.New(agent.ErrMemoryIO, "the memory file is not a regular file of sane size")
	}
	return data, err
}

// decode parses the memory file and checks every record, keeping the last
// of any duplicated ID.
func decode(data []byte) (*document, error) {
	doc := &document{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(doc); err != nil || doc.Version != version {
		return nil, errors.New(agent.ErrMemoryIO, "the memory file is damaged or from another version")
	}
	seen := map[string]int{}
	var recs []agent.MemoryRecord
	for _, r := range doc.Records {
		if validate(r) != nil || r.ID == "" {
			return nil, errors.New(agent.ErrMemoryIO, "the memory file holds an invalid record")
		}
		if i, dup := seen[r.ID]; dup {
			recs[i] = r
			continue
		}
		seen[r.ID] = len(recs)
		recs = append(recs, r)
	}
	doc.Records = recs
	return doc, nil
}

// write replaces the memory file atomically.
func write(root *os.Root, doc *document) error {
	data, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot encode the memory")
	}
	if err := atomicfile.Write(root, dataFile, data); err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot write the memory")
	}
	return nil
}

func (s *store) Put(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	rec.Content = strings.TrimSpace(rec.Content)
	if err := validate(rec); err != nil {
		return rec, err
	}
	err := s.locked(ctx, func(doc *document) (bool, error) {
		rec = s.upsert(doc, rec)
		return true, nil
	})
	return rec, err
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validate(rec agent.MemoryRecord) error {
	switch {
	case rec.Content == "":
		return agent.ToolError(agent.ErrMemoryInput, "A memory needs content.")
	case len(rec.Content) > maxContent:
		return agent.ToolError(agent.ErrMemoryInput, "A memory is one fact: keep it under 32 KiB.")
	case !utf8.ValidString(rec.Content) || strings.ContainsRune(rec.Content, 0):
		return agent.ToolError(agent.ErrMemoryInput, "A memory must be text.")
	case rec.ID != "" && !validID.MatchString(rec.ID):
		return agent.ToolError(agent.ErrMemoryInput, "Memory IDs are 1-64 letters, digits, _ or -.")
	}
	return nil
}

// upsert replaces the record with the same ID, keeping its creation time,
// or appends a new one.
func (s *store) upsert(doc *document, rec agent.MemoryRecord) agent.MemoryRecord {
	if rec.ID == "" {
		// The same fact remembered twice stays one record: a model that
		// repeats itself does not grow the memory.
		for _, old := range doc.Records {
			if old.Content == rec.Content {
				return old
			}
		}
		rec.ID = newID()
	}
	for i, old := range doc.Records {
		if old.ID == rec.ID {
			rec.Created = old.Created
			doc.Records[i] = rec
			return rec
		}
	}
	rec.Created = s.now().UTC()
	doc.Records = append(doc.Records, rec)
	return rec
}

func newID() string { return strings.ToLower(rand.Text()[:10]) }

func (s *store) Delete(ctx context.Context, id string) error {
	return s.locked(ctx, func(doc *document) (bool, error) {
		for i, r := range doc.Records {
			if r.ID == id {
				doc.Records = append(doc.Records[:i], doc.Records[i+1:]...)
				return true, nil
			}
		}
		return false, agent.ToolError(agent.ErrMemoryNotFound, "No memory with that ID.")
	})
}

// Search reads a snapshot without taking the lock: writes replace the file
// by rename, so a reader always sees one whole version, and a slow search
// never holds up other processes.
func (s *store) Search(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory directory")
	}
	doc, err := read(root)
	if cerr := root.Close(); err == nil && cerr != nil {
		err = errors.Wrap(cerr, agent.ErrMemoryIO, "cannot close the memory directory")
	}
	if err != nil {
		return nil, err
	}
	return rank(doc.Records, query, limit), nil
}
