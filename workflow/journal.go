// journal.go: a journal of workflow runs in a private directory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

const maxJournal = 64 << 20

// Remove deletes a finished or abandoned run's journal and lock.
func (j *FileJournal) Remove(ctx context.Context, id string) error {
	unlock, err := j.Lock(ctx, id)
	if err != nil {
		return err
	}
	defer release(unlock)
	root, err := os.OpenRoot(j.dir)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	if err := root.Remove(id + ".json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return root.Remove(id + ".lock")
}

// FileJournal keeps each run as <id>.json in a private directory, written
// atomically, and holds <id>.lock for the whole run: a second process on
// the same run is refused rather than interleaved.
type FileJournal struct {
	dir string
}

var runID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ErrLocked: another process is running the workflow.
const ErrLocked errors.ErrorCode = "WORKFLOW_LOCKED"

// NewFileJournal opens dir, creating it private.
func NewFileJournal(dir string) (*FileJournal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrJournal, "cannot create the journal directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || lockfile.CheckPrivate(info) != nil {
		return nil, errors.New(ErrJournal, "the journal directory must be a real directory of yours, not writable by others")
	}
	return &FileJournal{dir: dir}, nil
}

func checkID(id string) error {
	if !runID.MatchString(id) {
		return errors.New(ErrJournal, "workflow ids are 1-64 letters, digits, . _ -")
	}
	return nil
}

// Load implements Journal.
func (j *FileJournal) Load(_ context.Context, id string) (*State, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(j.dir)
	if err != nil {
		return nil, err
	}
	defer closeRoot(root)
	data, err := atomicfile.Read(root, id+".json", maxJournal)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil || st.Version != 1 || st.Steps == nil {
		return nil, errors.New(ErrJournal, "the journal of "+id+" is damaged")
	}
	return &st, nil
}

// Save implements Journal.
func (j *FileJournal) Save(_ context.Context, id string, st *State) error {
	if err := checkID(id); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // reports full of code would grow sixfold
	if err := enc.Encode(st); err != nil {
		return err
	}
	data := buf.Bytes()
	if len(data) > maxJournal {
		return errors.New(ErrJournal, "the journal of "+id+" would exceed its size bound")
	}
	root, err := os.OpenRoot(j.dir)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	return atomicfile.Write(root, id+".json", data)
}

// Lock implements Locker: the run is held until unlock or the process
// ends.
func (j *FileJournal) Lock(_ context.Context, id string) (func() error, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(j.dir)
	if err != nil {
		return nil, err
	}
	defer closeRoot(root)
	f, err := lockfile.Open(root, id+".lock")
	if err != nil {
		return nil, err
	}
	ok, err := lockfile.TryLock(f)
	if err != nil || !ok {
		if cerr := f.Close(); cerr != nil {
			return nil, cerr
		}
		return nil, errors.New(ErrLocked, "workflow "+id+" is running in another process")
	}
	return f.Close, nil
}

// List returns the runs in the journal.
func (j *FileJournal) List() ([]string, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if name := e.Name(); len(name) > 5 && name[len(name)-5:] == ".json" && name[0] != '.' {
			ids = append(ids, name[:len(name)-5])
		}
	}
	return ids, nil
}

func closeRoot(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}
