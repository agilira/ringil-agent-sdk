// file.go: sessions stored as files in a directory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package file stores sessions as append-only JSON Lines files, one per
// session, in a private directory.
//
// Every Append is written in one write and synced before it returns, so a
// message the agent recorded survives a crash. A session is held by one
// opener at a time; a second Open, in this process or another, fails with
// ErrSessionBusy. There is no size limit: a session grows as long as the
// conversation does.
package file

import (
	"context"
	stderrors "errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

const (
	dataExt = ".jsonl"
	lockExt = ".lock"
)

// New returns a store in dir, creating it with mode 0700 if needed. An
// existing directory must belong to the current user and not be writable by
// others: anyone who can write there can plant a history or hold a lock.
func New(dir string) (agent.Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, agent.ErrSessionIO, "cannot create the session directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New(agent.ErrSessionIO, "session path is not a directory")
	}
	if err := checkPrivate(info); err != nil {
		return nil, err
	}
	return &store{dir: dir}, nil
}

type store struct {
	dir string
}

// withRoot runs fn with the directory opened as an os.Root, which cannot
// reach outside dir whatever a name or a planted link says. The root is
// held only for the operation: a directory left open cannot be deleted or
// renamed on Windows.
func (s *store) withRoot(fn func(*os.Root) error) error {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return ioError(err, "cannot open the session directory")
	}
	err = fn(root)
	if cerr := root.Close(); err == nil && cerr != nil {
		err = ioError(cerr, "cannot close the session directory")
	}
	return err
}

func name(id, ext string) string { return id + ext }

func ioError(err error, msg string) error { return errors.Wrap(err, agent.ErrSessionIO, msg) }

// openFile opens a file of the store, refusing symlinks even inside the
// directory: session files are always regular files the store created.
func (s *store) openFile(name string, flag int) (f *os.File, err error) {
	err = s.withRoot(func(root *os.Root) error {
		f, err = openIn(root, name, flag)
		return err
	})
	return f, err
}

func openIn(root *os.Root, name string, flag int) (*os.File, error) {
	if info, err := root.Lstat(name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New(agent.ErrSessionIO, "session file is a symlink")
	}
	f, err := root.OpenFile(name, flag|openFlags, 0o600)
	if err != nil {
		return nil, ioError(err, "cannot open session file")
	}
	return f, nil
}

// lock takes the session's lock or reports it busy.
func (s *store) lock(id string) (*os.File, error) {
	f, err := s.openFile(name(id, lockExt), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err == nil && !ok {
		err = errors.New(agent.ErrSessionBusy, "session "+id+" is open elsewhere")
	} else if err != nil {
		err = ioError(err, "cannot lock session")
	}
	if err != nil {
		return nil, joinClose(err, f)
	}
	return f, nil
}

// joinClose closes f after a failure, keeping the failure's code.
func joinClose(err error, f *os.File) error {
	if cerr := f.Close(); cerr != nil {
		var coded *errors.Error
		if stderrors.As(err, &coded) {
			return coded.WithContext("close_error", cerr.Error())
		}
	}
	return err
}

func (s *store) Open(ctx context.Context, id string) (agent.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := agent.ValidateSessionID(id); err != nil {
		return nil, err
	}
	lock, err := s.lock(id)
	if err != nil {
		return nil, err
	}
	sess, err := s.load(id, lock)
	if err != nil {
		return nil, joinClose(err, lock)
	}
	return sess, nil
}

func (s *store) load(id string, lock *os.File) (*session, error) {
	f, err := s.openFile(name(id, dataExt), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	d, err := readSession(f)
	if err != nil {
		return nil, joinClose(err, f)
	}
	return &session{id: id, f: f, lock: lock, d: d}, nil
}

// readSession decodes f, writes the header of a new session, cuts a
// partial last line left by a crash and restores a stripped final newline.
func readSession(f *os.File) (*decoded, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, ioError(err, "cannot read session")
	}
	d, err := decode(data)
	if err != nil {
		return nil, err
	}
	switch {
	case d.valid == 0:
		head := headerLine()
		d.valid = int64(len(head))
		return d, rewrite(f, 0, head)
	case d.unterminated:
		d.valid++
		return d, writeAt(f, d.valid-1, []byte("\n"))
	case d.valid < int64(len(data)):
		return d, truncate(f, d.valid)
	}
	return d, nil
}

// rewrite replaces the whole file with data.
func rewrite(f *os.File, off int64, data []byte) error {
	if err := f.Truncate(off); err != nil {
		return ioError(err, "cannot reset session file")
	}
	return writeAt(f, off, data)
}

func truncate(f *os.File, size int64) error {
	if err := f.Truncate(size); err != nil {
		return ioError(err, "cannot cut a partial line")
	}
	return flush(f)
}

func writeAt(f *os.File, off int64, data []byte) error {
	if _, err := f.WriteAt(data, off); err != nil {
		return ioError(err, "cannot write session")
	}
	return flush(f)
}

func flush(f *os.File) error {
	if err := f.Sync(); err != nil {
		return ioError(err, "cannot sync session")
	}
	return nil
}

func (s *store) List(ctx context.Context) ([]agent.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, ioError(err, "cannot list sessions")
	}
	var out []agent.SessionInfo
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), dataExt)
		if !ok || !e.Type().IsRegular() || agent.ValidateSessionID(id) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed while listing
		}
		out = append(out, agent.SessionInfo{ID: id, Updated: info.ModTime(), Size: info.Size()})
	}
	slices.SortFunc(out, func(a, b agent.SessionInfo) int { return b.Updated.Compare(a.Updated) })
	return out, nil
}

// Delete removes a session that nobody has open.
func (s *store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agent.ValidateSessionID(id); err != nil {
		return err
	}
	lock, err := s.lock(id)
	if err != nil {
		return err
	}
	err = s.withRoot(func(root *os.Root) error { return root.Remove(name(id, dataExt)) })
	if err != nil && !os.IsNotExist(errors.RootCause(err)) {
		return joinClose(ioError(err, "cannot delete session"), lock)
	}
	if err := lock.Close(); err != nil {
		return ioError(err, "cannot release session lock")
	}
	// The lock file is left behind on purpose: removing it while another
	// opener waits on it would let two holders lock different files.
	return nil
}

type session struct {
	mu      sync.Mutex
	id      string
	f       *os.File
	lock    *os.File
	d       *decoded
	running bool
	// failed is set after a write error: what is on disk past the last
	// acknowledged line is unknown, so the session must be reopened (which
	// re-verifies the file) before anything else is written.
	failed bool
}

func (s *session) BeginRun() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil, errors.New(agent.ErrSessionBusy, "another run is using session "+s.id)
	}
	s.running = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.running = false
	}, nil
}

func (s *session) ID() string { return s.id }

func (s *session) Messages() []agent.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Message, len(s.d.msgs))
	for i, msg := range s.d.msgs {
		out[i] = msg.Clone()
	}
	return out
}

// Append validates msgs as the continuation of the history, then writes
// them in one write and syncs. Several messages in one call are not atomic
// across a crash: complete lines that reached the disk are kept. The agent
// loop appends one message at a time.
func (s *session) Append(ctx context.Context, msgs ...agent.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	data, seq, prev, err := encode(msgs, s.d.seq, s.d.prev)
	if err != nil {
		return err
	}
	if err := s.d.checker.Add(msgs...); err != nil {
		return err
	}
	if err := writeAt(s.f, s.d.valid, data); err != nil {
		s.failed = true
		return withTruncate(err, s.f, s.d.valid)
	}
	for _, msg := range msgs {
		s.d.msgs = append(s.d.msgs, msg.Clone())
	}
	s.d.seq, s.d.prev, s.d.valid = seq, prev, s.d.valid+int64(len(data))
	return nil
}

func (s *session) writable() error {
	switch {
	case s.f == nil:
		return errors.New(agent.ErrSessionIO, "session is closed")
	case s.failed:
		return errors.New(agent.ErrSessionIO, "an earlier write failed; reopen the session")
	}
	return nil
}

// withTruncate tries to remove a partly written tail, so a reopen finds the
// file as it was before the failed Append.
func withTruncate(err error, f *os.File, size int64) error {
	if terr := truncate(f, size); terr != nil {
		var coded *errors.Error
		if stderrors.As(err, &coded) {
			return coded.WithContext("truncate_error", terr.Error())
		}
	}
	return err
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	if lerr := s.lock.Close(); err == nil {
		err = lerr
	}
	s.f, s.lock = nil, nil
	if err != nil {
		return ioError(err, "cannot close session")
	}
	return nil
}
