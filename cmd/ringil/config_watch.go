// config_watch.go: following ringil.yaml, its instructions and the skills
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"cmp"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/agilira/argus"
	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
)

// settings is the configuration in force, shared by serve's runs: the
// file, the instructions it names and the skills, read together by Argus
// as one revision.
//
// One goroutine, loop, owns everything that changes after the start: it
// reads the revision Argus holds, moves to a new Argus setup when the file
// names other instructions (Argus follows the documents named at its
// start), checks and applies. It looks at every wake from Argus and at
// every tick, so that what was refused for a reason that has since gone
// (a file made private, an instructions file written at last) is applied
// without waiting for another edit.
type settings struct {
	path    string // "" without a user config directory: no file
	current atomic.Pointer[config]
	started *config // as read at the start: what serve set up once
	// noApprovals is the owners' chats' no_approvals for a file: the
	// flag's, when given; instructions the --instructions file, made
	// absolute.
	noApprovals  func(*config) bool
	instructions string
	skills       string // the skills directory
	every        time.Duration
	audit        *argus.AuditLogger
	// served are the profiles serve offers, fixed at its start.
	served atomic.Pointer[map[string]bool]

	watching *watch // loop's, after the start
	applied  string // digest of the revision in force (loop's)
	told     string // the last refusal told (loop's)
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

// watch is one Argus setup: the file and the instructions files it named
// when it was made.
type watch struct {
	argus *argus.Settings
	bound *argus.Bound[config]
	paths []string
}

func (s *settings) get() *config {
	if s == nil {
		return &config{}
	}
	if f := s.current.Load(); f != nil {
		return f
	}
	return &config{}
}

// pinned holds the revision in force now, and only it: what a run of
// serve reads, so that all of it comes from one revision.
func (s *settings) pinned() *settings {
	if s == nil {
		return nil
	}
	p := &settings{path: s.path, started: s.started, noApprovals: s.noApprovals, instructions: s.instructions, skills: s.skills}
	p.served.Store(s.served.Load())
	p.current.Store(s.get())
	return p
}

func (c *cli) configPath(ctx *orpheus.Context) (path string, explicit bool) {
	if p := ctx.GetGlobalFlagString("config"); p != "" {
		return p, true
	}
	base, err := c.configDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(base, "ringil", "ringil.yaml"), false
}

// configName is the configuration file, for messages.
func (c *cli) configName() string {
	if c.settings != nil && c.settings.path != "" {
		return c.settings.path
	}
	return "ringil.yaml"
}

// conf is the configuration in force: empty before loadConfig.
func (c *cli) conf() *config { return c.settings.get() }

// loadConfig reads ringil.yaml with the instructions it names and the
// skills, then follows their edits: one that passes every check is
// applied to what starts next (a run, a chat), one that does not is told
// and the last good one stays. A missing default file is no configuration
// until it is written. Every revision is recorded in the audit file.
func (c *cli) loadConfig(ctx *orpheus.Context) error {
	if c.settings != nil {
		return nil
	}
	path, explicit := c.configPath(ctx)
	if _, err := os.Lstat(path); path != "" && (err == nil || explicit) {
		if _, err := readPrivate(path); err != nil {
			return errors.Wrap(err, ErrUsage, "cannot read the configuration in "+path)
		}
	}
	s, err := c.newSettings(ctx, path)
	if err != nil {
		return err
	}
	if err := s.start(c); err != nil {
		s.close()
		return err
	}
	c.settings = s
	c.closers = append(c.closers, func() error { s.close(); return nil })
	return nil
}

func (c *cli) newSettings(ctx *orpheus.Context, path string) (*settings, error) {
	s := &settings{path: path, every: cmp.Or(c.configEvery, 2*time.Second), noApprovals: func(f *config) bool {
		if ctx.GlobalFlagChanged("no-approvals") {
			return ctx.GetGlobalFlagBool("no-approvals")
		}
		return f.NoApprovals
	}, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	if p := ctx.GetGlobalFlagString("instructions"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, errors.Wrap(err, ErrUsage, "cannot find "+p)
		}
		s.instructions = abs
	}
	if dir, err := c.skillsDir(); err == nil {
		s.skills = dir
	}
	return s, nil
}

// start makes the first setup: the file is read once to learn the
// instructions it names and the audit file, then followed with them.
func (s *settings) start(c *cli) error {
	var paths []string
	for range 3 { // the file may change while it is being read
		w, f, err := s.open(c, paths)
		if err != nil {
			return errors.Wrap(err, ErrUsage, cmp.Or(s.path, "the instructions")+": "+err.Error())
		}
		if s.audit == nil && !f.NoAudit && s.startAudit(c, f) {
			_ = w.argus.Close()
			paths = s.pathsOf(f)
			continue // read again, recorded this time
		}
		if slices.Equal(w.paths, s.pathsOf(f)) {
			s.watching, s.started, s.applied = w, f, f.digest
			s.current.Store(f)
			go s.loop(c)
			return nil
		}
		_ = w.argus.Close()
		paths = s.pathsOf(f)
	}
	return errors.New(ErrUsage, s.path+" kept changing while it was read")
}

// startAudit opens the audit file f names (JSONL by default; .db is
// SQLite, in a build with cgo); false when there is none to open.
func (s *settings) startAudit(c *cli, f *config) bool {
	file := f.AuditFile
	if file == "" {
		base, err := c.configDir()
		if err != nil {
			return false // nowhere to keep it
		}
		file = filepath.Join(base, "ringil", "audit.jsonl")
	}
	err := os.MkdirAll(filepath.Dir(file), 0o700)
	var logger *argus.AuditLogger
	if err == nil {
		logger, err = argus.NewAuditLogger(argus.AuditConfig{Enabled: true, OutputFile: file, MinLevel: argus.AuditInfo,
			BufferSize: 100, FlushInterval: time.Second})
	}
	if err != nil {
		c.note("no audit of the configuration: cannot open " + file + ": " + err.Error())
		return false
	}
	s.audit = logger
	return true
}

// pathsOf are the instructions files f and --instructions name, sorted.
func (s *settings) pathsOf(f *config) []string {
	paths := []string{s.instructions, f.Instructions}
	for _, p := range f.Profiles {
		paths = append(paths, p.Instructions)
	}
	paths = slices.DeleteFunc(paths, func(p string) bool { return p == "" })
	slices.Sort(paths)
	return slices.Compact(paths)
}

// errMoved: the file names other instructions than the setup follows.
var errMoved = stderrors.New("the instructions files changed")

// open makes a setup following the file, paths (each a required document,
// grouped by its own path) and the skills, and reads it.
func (s *settings) open(c *cli, paths []string) (*watch, *config, error) {
	b, err := s.setup(c, paths)
	if err != nil {
		return nil, nil, err
	}
	w := &watch{paths: paths}
	watched, err := b.Start()
	if err != nil {
		return nil, nil, err
	}
	w.argus = watched
	if w.bound, err = argus.Bind[config](watched, argus.Strict()); err != nil {
		_ = watched.Close()
		return nil, nil, err
	}
	f, err := s.read(w)
	if err != nil && !stderrors.Is(err, errMoved) {
		_ = watched.Close()
		return nil, nil, err
	}
	return w, f, nil
}

// setup is the Argus setup of open, not started.
func (s *settings) setup(c *cli, paths []string) (*argus.SetupBuilder, error) {
	for _, p := range paths {
		if strings.ContainsAny(p, "*?[") {
			return nil, errors.New(ErrUsage, "an instructions file's path cannot hold * ? or [: "+p)
		}
	}
	b := argus.Setup("ringil").MaxStaleness(s.every).
		DocumentLimits(agent.MaxInstructions+1, 32<<20, 2000).
		OnReload(func(*argus.Settings, argus.Change) { s.signal() }).
		OnError(func(err error) {
			c.note(cmp.Or(s.path, "the configuration") + " not applied, the last good one stays: " + err.Error())
		})
	if s.path != "" {
		b = b.FileIfPresent(s.path)
	}
	if s.skills != "" {
		b = b.Documents("skills", filepath.Join(s.skills, "**", "SKILL.md"))
	}
	if s.audit != nil {
		b = b.AuditTo(s.audit)
	}
	for _, p := range paths {
		b = b.RequiredDocuments(p, p)
	}
	return b, nil
}

// read is the configuration of the revision w holds: the file with its
// paths made absolute, the instructions' text and the revision's digest,
// all of one revision. errMoved, with the configuration, when the file
// names other instructions than w follows.
func (s *settings) read(w *watch) (*config, error) {
	for range 3 {
		digest := w.argus.Digest()
		next, err := s.readOnce(w)
		if w.argus.Digest() == digest { // no swap in between
			if next != nil {
				next.digest = digest
			}
			return next, err
		}
	}
	return nil, errors.New(ErrUsage, "the configuration kept changing while it was read")
}

func (s *settings) readOnce(w *watch) (*config, error) {
	next := w.bound.Value().from(filepath.Dir(s.path))
	if !slices.Equal(w.paths, s.pathsOf(next)) {
		return next, errMoved
	}
	next.texts = map[string]string{}
	for _, p := range w.paths {
		text, err := instructionsText(w.argus, p)
		if err != nil {
			return nil, err
		}
		next.texts[p] = text
	}
	return next, nil
}

// instructionsText is an instructions file as the revision holds it: a
// private regular file of UTF-8 text, within the bound.
func instructionsText(watched *argus.Settings, path string) (string, error) {
	docs := watched.Docs(path)
	if len(docs) != 1 {
		return "", errors.New(ErrUsage, "cannot read the instructions in "+path)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || lockfile.CheckPrivate(info) != nil {
		return "", errors.New(ErrUsage, path+": instructions must be a regular file of yours, not writable by others")
	}
	text := docs[0].String()
	if len(text) > agent.MaxInstructions || !utf8.ValidString(text) {
		return "", errors.New(ErrUsage, path+": instructions are UTF-8 text of at most 64 KiB")
	}
	return strings.TrimSpace(text), nil
}

// signal wakes the loop: Argus published a revision.
func (s *settings) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *settings) loop(c *cli) {
	defer close(s.done)
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-tick.C:
		}
		s.step(c)
	}
}

// step brings the configuration in force to the revision on disk, or
// tells, once, why it cannot.
func (s *settings) step(c *cli) {
	next, err := s.read(s.watching)
	if stderrors.Is(err, errMoved) {
		if err = s.move(c, next); err == nil {
			next, err = s.read(s.watching)
		}
	}
	if err == nil && next.digest == s.applied {
		return
	}
	if err == nil {
		err = c.applyConfig(s, next)
	}
	if err != nil {
		s.refuse(c, err)
		return
	}
	s.applied, s.told = next.digest, ""
}

// move replaces the setup with one following the instructions f names;
// on failure the old one stays, and the next step tries again.
func (s *settings) move(c *cli, f *config) error {
	w, got, err := s.open(c, s.pathsOf(f))
	if err != nil {
		return err
	}
	if !slices.Equal(w.paths, s.pathsOf(got)) {
		_ = w.argus.Close()
		return errors.New(ErrUsage, "the configuration changed while it was read")
	}
	_ = s.watching.argus.Close()
	s.watching = w
	return nil
}

// refuse tells why the revision on disk is not in force, once for each
// reason, in the terminal or log and in the audit file.
func (s *settings) refuse(c *cli, err error) {
	why := err.Error()
	if why == s.told {
		return
	}
	s.told = why
	c.note(cmp.Or(s.path, "the configuration") + " not applied, the last good one stays: " + why)
	if s.audit != nil {
		s.audit.Log(argus.AuditWarn, "ringil_refused", "ringil.config", s.path, nil, nil,
			map[string]interface{}{"in_force": s.applied, "reason": why})
	}
}

func (s *settings) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		if s.watching != nil { // the loop runs
			<-s.done
			_ = s.watching.argus.Close()
		}
		if s.audit != nil {
			_ = s.audit.Close()
		}
	})
}

// applyConfig puts a revision in force, if the file is still a private
// file of the user's and the chats' people take it.
func (c *cli) applyConfig(s *settings, next *config) error {
	if err := s.acceptable(next); err != nil {
		return err
	}
	last := s.get()
	s.current.Store(next)
	if b := c.bridge.Load(); b != nil {
		if err := b.SetPeople(next.Serve.people(s.noApprovals(next))); err != nil {
			s.current.Store(last)
			return err
		}
	}
	if why := restartFor(s.started, next); why != "" {
		c.note(s.path + ": " + why + ": changed, in force from the next start of serve")
	}
	c.note(cmp.Or(s.path, "the configuration") + " applied (config " + next.shortDigest() + ")")
	if s.audit != nil {
		s.audit.Log(argus.AuditInfo, "ringil_applied", "ringil.config", s.path, nil, nil,
			map[string]interface{}{"digest": next.digest})
	}
	return nil
}

// acceptable checks what a revision needs beyond the file's own rules.
func (s *settings) acceptable(next *config) error {
	if _, err := os.Lstat(s.path); s.path != "" && err == nil {
		if _, err := readPrivate(s.path); err != nil {
			return err
		}
	}
	served := s.served.Load()
	for _, name := range []string{next.Serve.MemberProfile, next.Serve.PublicProfile} {
		if served != nil && name != "" && !(*served)[name] {
			return errors.New(ErrUsage, "profile "+name+" is not served yet: restart serve to give it to the chats")
		}
	}
	return nil
}
