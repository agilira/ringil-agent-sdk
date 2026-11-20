// store.go: the triggers and what became of them, on disk
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package trigger

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"os"
	"sort"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
)

// Error codes.
const (
	// ErrConfig: a trigger or the configuration is invalid.
	ErrConfig errors.ErrorCode = "TRIGGER_CONFIG"
	// ErrStore: the store cannot be read or written.
	ErrStore errors.ErrorCode = "TRIGGER_STORE"
	// ErrNotFound: no trigger by that name.
	ErrNotFound errors.ErrorCode = "TRIGGER_NOT_FOUND"
	// ErrBusy: another runner runs this store's triggers.
	ErrBusy errors.ErrorCode = "TRIGGER_BUSY"
)

// Bounds.
const (
	MaxTriggers = 256
	MaxInput    = 64 << 10
	maxFile     = 16 << 20
)

// Status is what became of a trigger.
type Status struct {
	// LastFire is when the last run was set off: the time on the
	// timetable, or when the change was seen, the message heard.
	LastFire time.Time `json:"last_fire,omitzero"`
	// LastRun is that run's ID (serve keeps runs for a while).
	LastRun string `json:"last_run,omitempty"`
	// LastEnd and LastError: when it ended, and how, "" for success.
	LastEnd   time.Time `json:"last_end,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	// LastText is the start of the last run's answer.
	LastText string `json:"last_text,omitempty"`
	Runs     int    `json:"runs,omitempty"`
	// Target is a digest of what sets the trigger off (kind, timetable,
	// tool and arguments, path): a trigger set off by something else, or
	// a new trigger by an old name, starts a status of its own.
	Target string `json:"target,omitempty"`
	// Seen is a digest of what a Tool or File trigger last saw: a change
	// is a change from it, across restarts.
	Seen string `json:"seen,omitempty"`
	// Skipped counts times a run was due while the last was still going.
	Skipped int `json:"skipped,omitempty"`
}

// Store keeps triggers in triggers.json, which people edit (through the CLI), and
// their status in status.json, which only the runner writes: a private
// directory, like every store of the SDK.
type Store struct {
	root *os.Root
}

const (
	triggersFile = "triggers.json"
	statusFile   = "status.json"
	lockName     = "runner.lock"
)

// OpenStore opens, creating it private, the store in dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot create the triggers' directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the triggers' directory")
	}
	if err := checkPrivate(root); err != nil {
		_ = root.Close() // #nosec G104 -- refusing it anyway
		return nil, err
	}
	return &Store{root: root}, nil
}

// Close releases the store.
func (s *Store) Close() error { return s.root.Close() }

type triggersDoc struct {
	Triggers []Trigger `json:"triggers"`
}

// Triggers returns the triggers, sorted by name. A missing file is no triggers.
func (s *Store) Triggers() ([]Trigger, error) {
	var doc triggersDoc
	if err := s.read(triggersFile, &doc, true); err != nil {
		return nil, err
	}
	sort.Slice(doc.Triggers, func(i, k int) bool { return doc.Triggers[i].Name < doc.Triggers[k].Name })
	return doc.Triggers, nil
}

// Put adds a trigger, or replaces the one with its name.
func (s *Store) Put(trigger Trigger) error {
	if _, err := trigger.Check(); err != nil {
		return err
	}
	return s.editTriggers(func(triggers []Trigger) ([]Trigger, error) {
		for i := range triggers {
			if triggers[i].Name == trigger.Name {
				triggers[i] = trigger
				return triggers, nil
			}
		}
		if len(triggers) >= MaxTriggers {
			return nil, errors.New(ErrConfig, "the triggers holds at most 256 triggers")
		}
		return append(triggers, trigger), nil
	})
}

// RemoveIf deletes a trigger if, as the store holds it now, ok says so:
// a change made meanwhile is judged, not the version read before.
func (s *Store) RemoveIf(name string, ok func(Trigger) bool) error {
	return s.editTriggers(func(triggers []Trigger) ([]Trigger, error) {
		for i := range triggers {
			if triggers[i].Name == name {
				if !ok(triggers[i]) {
					return triggers, nil
				}
				return append(triggers[:i], triggers[i+1:]...), nil
			}
		}
		return triggers, nil
	})
}

// Remove deletes a trigger.
func (s *Store) Remove(name string) error {
	return s.editTriggers(func(triggers []Trigger) ([]Trigger, error) {
		for i := range triggers {
			if triggers[i].Name == name {
				return append(triggers[:i], triggers[i+1:]...), nil
			}
		}
		return nil, errors.New(ErrNotFound, "no trigger "+name)
	})
}

// Pause stops a trigger running, or lets it run again.
func (s *Store) Pause(name string, paused bool) error {
	return s.editTriggers(func(triggers []Trigger) ([]Trigger, error) {
		for i := range triggers {
			if triggers[i].Name == name {
				triggers[i].Paused = paused
				return triggers, nil
			}
		}
		return nil, errors.New(ErrNotFound, "no trigger "+name)
	})
}

// editTriggers changes the triggers under the store's lock, so that two edits do
// not lose one another.
func (s *Store) editTriggers(change func([]Trigger) ([]Trigger, error)) error {
	unlock, err := s.lock("triggers.lock")
	if err != nil {
		return err
	}
	defer unlock()
	triggers, err := s.Triggers()
	if err != nil {
		return err
	}
	if triggers, err = change(triggers); err != nil {
		return err
	}
	return s.write(triggersFile, triggersDoc{Triggers: triggers})
}

// Status returns every trigger's status.
func (s *Store) Status() (map[string]Status, error) {
	st := map[string]Status{}
	if err := s.read(statusFile, &st, false); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Store) saveStatus(st map[string]Status) error { return s.write(statusFile, st) }

// read decodes a file; strict refuses unknown fields (triggers people write),
// lenient keeps going past them (a status a newer version wrote).
func (s *Store) read(name string, v any, strict bool) error {
	data, err := atomicfile.Read(s.root, name, maxFile)
	if stderrors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot read "+name)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return errors.Wrap(err, ErrStore, name+" is not valid")
	}
	return nil
}

func (s *Store) write(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errors.Wrap(err, ErrStore, "cannot encode "+name)
	}
	if err := atomicfile.Write(s.root, name, append(data, '\n')); err != nil {
		return errors.Wrap(err, ErrStore, "cannot write "+name)
	}
	return nil
}
