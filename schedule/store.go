// store.go: the jobs and what became of them, on disk
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package schedule

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
)

// Error codes.
const (
	// ErrConfig: a job or the configuration is invalid.
	ErrConfig errors.ErrorCode = "SCHEDULE_CONFIG"
	// ErrStore: the store cannot be read or written.
	ErrStore errors.ErrorCode = "SCHEDULE_STORE"
	// ErrNotFound: no job by that name.
	ErrNotFound errors.ErrorCode = "SCHEDULE_NOT_FOUND"
	// ErrBusy: another scheduler runs this store's jobs.
	ErrBusy errors.ErrorCode = "SCHEDULE_BUSY"
)

// Bounds.
const (
	MaxJobs  = 256
	MaxInput = 64 << 10
	maxFile  = 16 << 20
)

// Job is one scheduled run of an agent.
type Job struct {
	// Name identifies the job: 1-64 letters, digits, - and _.
	Name string `json:"name"`
	// When is the timetable (see Parse).
	When string `json:"when"`
	// TimeZone is an IANA zone ("Europe/Rome") for When; empty is local.
	TimeZone string `json:"time_zone,omitempty"`
	// Profile is the role that runs it; empty is "default".
	Profile string `json:"profile,omitempty"`
	// Input is the message the agent receives.
	Input string `json:"input"`
	// Conversation, when set, carries on one conversation across runs
	// (the agent remembers what it did last time); empty starts afresh.
	Conversation string `json:"conversation,omitempty"`
	// CatchUp runs a job once when the scheduler starts if a run was
	// missed while it was down; off, missed runs are skipped.
	CatchUp bool `json:"catch_up,omitempty"`
	// Paused jobs keep their place but do not run.
	Paused bool `json:"paused,omitempty"`
}

var jobName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Check validates the job and returns its parsed timetable.
func (j Job) Check() (When, error) {
	if !jobName.MatchString(j.Name) {
		return nil, errors.New(ErrConfig, "job names are 1-64 letters, digits, - and _")
	}
	if strings.TrimSpace(j.Input) == "" || len(j.Input) > MaxInput {
		return nil, errors.New(ErrConfig, "job "+j.Name+": the input is empty or larger than 64 KiB")
	}
	if j.Conversation != "" && !jobName.MatchString(j.Conversation) {
		return nil, errors.New(ErrConfig, "job "+j.Name+": conversation names are 1-64 letters, digits, - and _")
	}
	loc, err := zone(j.TimeZone)
	if err != nil {
		return nil, errors.New(ErrConfig, "job "+j.Name+": unknown time zone "+j.TimeZone)
	}
	return Parse(j.When, loc)
}

func zone(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	return time.LoadLocation(name)
}

// Status is what became of a job.
type Status struct {
	// LastFire is the scheduled time of the last run started.
	LastFire time.Time `json:"last_fire,omitzero"`
	// LastRun is that run's ID (serve keeps runs for a while).
	LastRun string `json:"last_run,omitempty"`
	// LastEnd and LastError: when it ended, and how, "" for success.
	LastEnd   time.Time `json:"last_end,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	// LastText is the start of the last run's answer.
	LastText string `json:"last_text,omitempty"`
	Runs     int    `json:"runs,omitempty"`
	// Skipped counts times a run was due while the last was still going.
	Skipped int `json:"skipped,omitempty"`
}

// Store keeps jobs in jobs.json, which people edit (through the CLI), and
// their status in status.json, which only the scheduler writes: a private
// directory, like every store of the SDK.
type Store struct {
	root *os.Root
}

const (
	jobsFile   = "jobs.json"
	statusFile = "status.json"
	lockName   = "scheduler.lock"
)

// OpenStore opens, creating it private, the store in dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot create the schedule's directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot open the schedule's directory")
	}
	if err := checkPrivate(root); err != nil {
		_ = root.Close() // #nosec G104 -- refusing it anyway
		return nil, err
	}
	return &Store{root: root}, nil
}

// Close releases the store.
func (s *Store) Close() error { return s.root.Close() }

type jobsDoc struct {
	Jobs []Job `json:"jobs"`
}

// Jobs returns the jobs, sorted by name. A missing file is no jobs.
func (s *Store) Jobs() ([]Job, error) {
	var doc jobsDoc
	if err := s.read(jobsFile, &doc, true); err != nil {
		return nil, err
	}
	sort.Slice(doc.Jobs, func(i, k int) bool { return doc.Jobs[i].Name < doc.Jobs[k].Name })
	return doc.Jobs, nil
}

// Put adds a job, or replaces the one with its name.
func (s *Store) Put(job Job) error {
	if _, err := job.Check(); err != nil {
		return err
	}
	return s.editJobs(func(jobs []Job) ([]Job, error) {
		for i := range jobs {
			if jobs[i].Name == job.Name {
				jobs[i] = job
				return jobs, nil
			}
		}
		if len(jobs) >= MaxJobs {
			return nil, errors.New(ErrConfig, "the schedule holds at most 256 jobs")
		}
		return append(jobs, job), nil
	})
}

// Remove deletes a job.
func (s *Store) Remove(name string) error {
	return s.editJobs(func(jobs []Job) ([]Job, error) {
		for i := range jobs {
			if jobs[i].Name == name {
				return append(jobs[:i], jobs[i+1:]...), nil
			}
		}
		return nil, errors.New(ErrNotFound, "no job "+name)
	})
}

// Pause stops a job running, or lets it run again.
func (s *Store) Pause(name string, paused bool) error {
	return s.editJobs(func(jobs []Job) ([]Job, error) {
		for i := range jobs {
			if jobs[i].Name == name {
				jobs[i].Paused = paused
				return jobs, nil
			}
		}
		return nil, errors.New(ErrNotFound, "no job "+name)
	})
}

// editJobs changes the jobs under the store's lock, so that two edits do
// not lose one another.
func (s *Store) editJobs(change func([]Job) ([]Job, error)) error {
	unlock, err := s.lock("jobs.lock")
	if err != nil {
		return err
	}
	defer unlock()
	jobs, err := s.Jobs()
	if err != nil {
		return err
	}
	if jobs, err = change(jobs); err != nil {
		return err
	}
	return s.write(jobsFile, jobsDoc{Jobs: jobs})
}

// Status returns every job's status.
func (s *Store) Status() (map[string]Status, error) {
	st := map[string]Status{}
	if err := s.read(statusFile, &st, false); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Store) saveStatus(st map[string]Status) error { return s.write(statusFile, st) }

// read decodes a file; strict refuses unknown fields (jobs people write),
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
