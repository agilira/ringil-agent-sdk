// serve.go: agents behind a local HTTP API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package serve runs agents for other programs: a local HTTP API that
// starts runs on named profiles, streams their events (Server-Sent
// Events, resumable), asks the caller to approve risky tool calls, lets
// it cancel a run, and keeps finished runs for a while.
//
// The API listens on loopback only and wants a bearer token on every
// request; requests from a browser (an Origin header, a Host other than
// the loopback) are refused, so a web page cannot drive the agent.
//
//	srv, _ := serve.New(serve.Config{Token: token, Profiles: []serve.Profile{{Name: "default", New: serve.Fixed(a)}}})
//	defer srv.Close()
//	srv.Serve(ctx, listener)
//
// To have tool calls approved through the API, build the profile's agent
// with Approve: srv.Approve (or a policy that calls it for the calls it
// wants confirmed).
package serve

import (
	"context"
	"crypto/rand"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
)

// Error codes.
const (
	ErrConfig   errors.ErrorCode = "SERVE_CONFIG"
	ErrInput    errors.ErrorCode = "SERVE_INPUT"
	ErrNotFound errors.ErrorCode = "SERVE_NOT_FOUND"
	ErrBusy     errors.ErrorCode = "SERVE_BUSY"
	ErrClosed   errors.ErrorCode = "SERVE_CLOSED"
)

// Profile is an agent the API can run: a role, its provider, its tools,
// as the host builds it.
type Profile struct {
	// Name is how requests choose it: 1-64 letters, digits, - and _.
	Name        string
	Description string
	// New makes the agent for one run. A run gets its own: its workers,
	// their background tasks and anything else with state are never
	// shared with another run. Fixed serves one agent to every run.
	New func(ctx context.Context) (Instance, error)
}

// Instance is the agent of one run.
type Instance struct {
	Agent agent.Agent
	// Crew, when the agent delegates, is its workers: the run waits for
	// the tasks it started in the background and gives each report to the
	// agent before it ends. The server closes it at the end of the run.
	Crew *delegate.Crew
	// Close, when set, releases what New made, at the end of the run.
	Close func()
}

// Fixed serves a every run: for an agent without state of its own (a
// delegating agent needs a crew per run: build it in New).
func Fixed(a agent.Agent) func(context.Context) (Instance, error) {
	return func(context.Context) (Instance, error) { return Instance{Agent: a}, nil }
}

// Config sets up a Server.
type Config struct {
	Profiles []Profile
	// Token must be sent as "Authorization: Bearer <token>"; at least 32
	// characters.
	Token string
	// Store keeps conversations: a run that names one continues it. Nil
	// means every run starts fresh and naming a conversation is refused.
	Store agent.Store
	// MaxRuns bounds the runs going at once; default 4.
	MaxRuns int
	// Keep is how long a finished run stays readable; default 1 hour.
	Keep time.Duration
	// MaxLog bounds the bytes of events kept per run; past it the oldest
	// go, and a reader is told how many it missed. Default 16 MiB.
	MaxLog int
	// ApprovalTimeout is how long a call waits for a decision before it is
	// refused; default 10 minutes.
	ApprovalTimeout time.Duration
}

// Server runs the profiles' agents for the API.
type Server struct {
	cfg      Config
	profiles map[string]Profile
	ctx      context.Context
	stop     context.CancelFunc
	mu       sync.Mutex
	runs     map[string]*job
	active   int
	mounts   map[string]http.Handler
	wg       sync.WaitGroup
	now      func() time.Time
}

// fail is an error whose message the API's caller sees.
func fail(code errors.ErrorCode, message string) error {
	return errors.New(code, message).WithUserMessage(message)
}

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// New checks cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if len(cfg.Token) < 32 {
		return nil, fail(ErrConfig, "the token must be at least 32 characters")
	}
	if cfg.MaxRuns < 0 || cfg.Keep < 0 || cfg.MaxLog < 0 || cfg.ApprovalTimeout < 0 {
		return nil, fail(ErrConfig, "limits cannot be negative")
	}
	profiles, err := byName(cfg.Profiles)
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Server{cfg: cfg, profiles: profiles, ctx: ctx, stop: stop, runs: map[string]*job{}, now: time.Now}, nil
}

func byName(list []Profile) (map[string]Profile, error) {
	profiles := map[string]Profile{}
	for _, p := range list {
		if _, dup := profiles[p.Name]; !validName.MatchString(p.Name) || p.New == nil || dup {
			return nil, fail(ErrConfig, "every profile needs a distinct name (1-64 letters, digits, - _) and New")
		}
		profiles[p.Name] = p
	}
	if len(profiles) == 0 {
		return nil, fail(ErrConfig, "at least one profile is required")
	}
	return profiles, nil
}

func (s *Server) maxRuns() int        { return positive(s.cfg.MaxRuns, 4) }
func (s *Server) keep() time.Duration { return positiveDur(s.cfg.Keep, time.Hour) }
func (s *Server) maxLog() int         { return positive(s.cfg.MaxLog, 16<<20) }
func (s *Server) approvalTimeout() time.Duration {
	return positiveDur(s.cfg.ApprovalTimeout, 10*time.Minute)
}

func positive(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func positiveDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// ProfileInfo describes a profile to the API.
type ProfileInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

// Profiles lists the profiles by name.
func (s *Server) Profiles() []ProfileInfo {
	out := make([]ProfileInfo, 0, len(s.profiles))
	for _, p := range s.profiles {
		out = append(out, ProfileInfo{p.Name, p.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Start begins a run of profile on input, continuing conversation when
// it is not empty. It fails at once when the server is full or the
// conversation is in use: there is no queue.
func (s *Server) Start(profile, conversation, input string) (Snapshot, error) {
	p, ok := s.profiles[profile]
	switch {
	case !ok:
		return Snapshot{}, fail(ErrNotFound, "no such profile")
	case input == "":
		return Snapshot{}, fail(ErrInput, "the input is empty")
	case conversation != "" && s.cfg.Store == nil:
		return Snapshot{}, fail(ErrInput, "this server keeps no conversations")
	case conversation != "" && agent.ValidateSessionID(conversation) != nil:
		return Snapshot{}, fail(ErrInput, "bad conversation ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if err := s.admit(conversation); err != nil {
		return Snapshot{}, err
	}
	j := newJob(rand.Text()[:20], p.Name, conversation, s)
	// The run's context exists before Start returns: a Cancel right away
	// reaches it.
	ctx, cancel := context.WithCancel(context.WithValue(s.ctx, jobKey{}, j))
	j.cancel = cancel
	s.runs[j.id] = j
	s.active++
	s.wg.Add(1)
	go s.execute(ctx, cancel, j, p, input)
	return j.snapshot(), nil
}

func (s *Server) admit(conversation string) error {
	if s.ctx.Err() != nil {
		return fail(ErrClosed, "the server is closing")
	}
	if s.active >= s.maxRuns() {
		return fail(ErrBusy, "too many runs going")
	}
	for _, j := range s.runs {
		// Compared without case: a store on a case-insensitive file
		// system holds "abc" and "ABC" in one file.
		if conversation != "" && strings.EqualFold(j.conversation, conversation) && !j.finished() {
			return fail(ErrBusy, "the conversation has a run going")
		}
	}
	return nil
}

// execute runs one job to its end.
func (s *Server) execute(ctx context.Context, cancel context.CancelFunc, j *job, p Profile, input string) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	defer cancel()
	j.end(s.work(ctx, j, p, input))
}

// work makes the run's agent and runs it, then on each background
// report.
func (s *Server) work(ctx context.Context, j *job, p Profile, input string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fail(ErrConfig, "the profile's agent panicked")
		}
	}()
	inst, err := p.New(ctx)
	defer release(inst) // whatever New made, even with an error
	if err != nil {
		return err
	}
	if inst.Agent == nil {
		return fail(ErrConfig, "the profile made no agent")
	}
	conv, closeConv, err := s.open(ctx, j.conversation)
	if err != nil {
		return err
	}
	defer closeConv()
	err = turn(ctx, j, inst.Agent, conv, agent.UserText(input))
	for err == nil && inst.Crew != nil {
		f, ok := inst.Crew.Next(ctx)
		if !ok {
			break
		}
		err = turn(ctx, j, inst.Agent, conv, delegate.Notice(f))
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// turn runs the agent once, logging its events.
func turn(ctx context.Context, j *job, a agent.Agent, conv agent.Conversation, msg agent.Message) error {
	var last error
	for ev, err := range a.Stream(ctx, conv, msg) {
		if err != nil {
			last = err
		}
		j.add(ev)
	}
	return last
}

func release(inst Instance) {
	if inst.Crew != nil {
		inst.Crew.Close()
	}
	if inst.Close != nil {
		inst.Close()
	}
}

// open returns the run's conversation: a stored one, or one in memory
// for its turns.
func (s *Server) open(ctx context.Context, id string) (agent.Conversation, func(), error) {
	if id == "" {
		return agent.NewConversation(), func() {}, nil
	}
	sess, err := s.cfg.Store.Open(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return sess, func() { _ = sess.Close() }, nil
}

// Get returns a run's snapshot.
func (s *Server) Get(id string) (Snapshot, error) {
	j, err := s.job(id)
	if err != nil {
		return Snapshot{}, err
	}
	return j.snapshot(), nil
}

// List returns every run kept, newest first.
func (s *Server) List() []Snapshot {
	s.mu.Lock()
	s.prune()
	out := make([]Snapshot, 0, len(s.runs))
	for _, j := range s.runs {
		out = append(out, j.snapshot())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Wait blocks until the run ends, or ctx does, and returns its snapshot.
func (s *Server) Wait(ctx context.Context, id string) (Snapshot, error) {
	j, err := s.job(id)
	if err != nil {
		return Snapshot{}, err
	}
	for {
		_, changed, _, over := j.since(1 << 62)
		if over {
			return j.snapshot(), nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return j.snapshot(), ctx.Err()
		}
	}
}

// Cancel stops a run; it ends as cancelled once its tools return.
func (s *Server) Cancel(id string) error {
	j, err := s.job(id)
	if err != nil {
		return err
	}
	j.cancelRun()
	return nil
}

// Forget removes a finished run.
func (s *Server) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.runs[id]
	switch {
	case !ok:
		return fail(ErrNotFound, "no such run")
	case !j.finished():
		return fail(ErrBusy, "the run is still going; cancel it first")
	}
	delete(s.runs, id)
	return nil
}

func (s *Server) job(id string) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	j, ok := s.runs[id]
	if !ok {
		return nil, fail(ErrNotFound, "no such run")
	}
	return j, nil
}

// maxKept bounds the finished runs kept, each up to MaxLog of events.
const maxKept = 256

// prune drops finished runs older than Keep, and the oldest finished ones
// past maxKept; s.mu held.
func (s *Server) prune() {
	cut := s.now().Add(-s.keep())
	var done []*job
	for id, j := range s.runs {
		f := j.finishedAt()
		switch {
		case f.IsZero():
		case f.Before(cut):
			delete(s.runs, id)
		default:
			done = append(done, j)
		}
	}
	if extra := len(done) - maxKept; extra > 0 {
		sort.Slice(done, func(a, b int) bool { return done[a].finishedAt().Before(done[b].finishedAt()) })
		for _, j := range done[:extra] {
			delete(s.runs, j.id)
		}
	}
}

// Mount adds h under pattern (an http.ServeMux pattern) to what Serve
// answers, behind the same guard: for an A2A endpoint next to the API.
// Call it before Serve.
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mounts == nil {
		s.mounts = map[string]http.Handler{}
	}
	s.mounts[pattern] = h
}

// Close cancels every run and waits for them to end.
func (s *Server) Close() {
	s.mu.Lock()
	s.stop() // under the lock: no Start passes admission after this
	s.mu.Unlock()
	s.wg.Wait()
}
