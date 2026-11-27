// serve.go: agents behind a local HTTP API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package serve runs agents for other programs: a local HTTP API that
// starts runs on named profiles, streams their events (Server-Sent
// Events, resumable), asks the caller to approve risky tool calls, lets
// it cancel a run, and keeps finished runs for a while.
//
// The API listens on loopback only (in a container on its own network,
// with Config.Container) and wants a bearer token on every request; requests from a browser (an Origin header, a Host other than
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
	"io/fs"
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/dirs"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

// Error codes.
const (
	ErrConfig   errors.ErrorCode = "SERVE_CONFIG"
	ErrInput    errors.ErrorCode = "SERVE_INPUT"
	ErrNotFound errors.ErrorCode = "SERVE_NOT_FOUND"
	ErrBusy     errors.ErrorCode = "SERVE_BUSY"
	// ErrSignedOut: the app's own page, without a live session.
	ErrSignedOut errors.ErrorCode = "SERVE_SIGNED_OUT"
	ErrClosed    errors.ErrorCode = "SERVE_CLOSED"
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
	// keeps them in files in the program's state (dirs.Program().State,
	// "sessions"), as coding agents keep theirs.
	Store agent.Store
	// NoStore keeps no conversations: every run starts fresh and naming
	// a conversation is refused.
	NoStore bool
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
	// App, when set, is a browser app served at "/" (its index.html and
	// files, such as a build of the community app). A browser signs in
	// with a link of SignInPath and then calls the API with its session
	// cookie, from the app's own pages only; other pages stay refused.
	App fs.FS
	// CheckProject, when set, is asked of the project a run is started on
	// or a conversation is moved to: an error refuses it at once (a
	// project that is not there, or archived), as a bad request.
	CheckProject func(ctx context.Context, project string) error
	// Sessions, when set, is a file where the app's sessions are kept (by
	// their hashes, private to the user), so that a restart of the server
	// does not sign the browser out; "" keeps them in memory.
	Sessions string
	// Voice gives the app speech to text and text to speech; zero, the
	// voice routes say it is off.
	Voice Voice
	// Artifacts is where the profiles' artifact tools keep what they make
	// (artifact.Tools, on this same store): each run works in its
	// conversation's scope only, its workers too (a run without one has
	// none), each new version is an "artifact" event, and the app reads
	// them under /v1/conversations/{id}/artifacts. A store of its own:
	// every scope in it is a conversation's.
	Artifacts artifact.Store
	// Container says the server runs in a container, whose network is its
	// own: it may listen on every interface, as the container's port must
	// be reached from the host. Then a request is answered only from the
	// loopback or from Peers (the host, through the container's gateway),
	// refusing other containers on the bridge and direct LAN peers before
	// the token is looked at. Still publish the port on the host's loopback
	// only (docker run -p 127.0.0.1:8765:8765): Docker's userland proxy and
	// Docker Desktop masquerade LAN connections behind the gateway address.
	Container bool
	// Peers are the networks a server in a container answers besides the
	// loopback: the host's address as the container sees it (its default
	// gateway). Empty, only the loopback.
	Peers []netip.Prefix
}

// Server runs the profiles' agents for the API.
type Server struct {
	cfg       Config
	profileMu sync.RWMutex
	profiles  map[string]Profile
	ctx       context.Context
	stop      context.CancelFunc
	mu        sync.Mutex
	runs      map[string]*job
	held      map[string]int // conversations read or changed outside a run, by lower-case ID: readers, or -1 for one that deletes
	active    int
	paused    int // Pause holds: no run starts while one is held
	mounts    map[string]http.Handler
	wg        sync.WaitGroup
	now       func() time.Time
	app       *appAuth
	uploads   chan struct{} // large bodies read at once: MaxRuns
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
	if cfg, err = withStore(cfg); err != nil {
		return nil, err
	}
	cfg.Peers = slices.Clone(cfg.Peers)
	ctx, stop := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, profiles: profiles, ctx: ctx, stop: stop, runs: map[string]*job{}, held: map[string]int{}, now: time.Now, app: newAppAuth(cfg.Sessions)}
	s.uploads = make(chan struct{}, s.maxRuns())
	return s, nil
}

// withStore gives cfg the default store, unless it has one or wants none.
func withStore(cfg Config) (Config, error) {
	if cfg.NoStore {
		cfg.Store = nil
		return cfg, nil
	}
	if cfg.Store != nil {
		return cfg, nil
	}
	d, err := dirs.Program()
	if err != nil {
		return cfg, errors.Wrap(err, ErrConfig, "no place for the conversations: set Store, or NoStore")
	}
	store, err := file.New(filepath.Join(d.State, "sessions"))
	if err != nil {
		return cfg, errors.Wrap(err, ErrConfig, "cannot open the conversations' store")
	}
	cfg.Store = store
	return cfg, nil
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

// SetProfiles replaces the profiles, checked as New checks them: the
// next runs choose among these; the runs going keep theirs.
func (s *Server) SetProfiles(list []Profile) error {
	profiles, err := byName(list)
	if err != nil {
		return err
	}
	s.profileMu.Lock()
	s.profiles = profiles
	s.profileMu.Unlock()
	return nil
}

// profile is the profile name, if there is one.
func (s *Server) profile(name string) (Profile, bool) {
	s.profileMu.RLock()
	defer s.profileMu.RUnlock()
	p, ok := s.profiles[name]
	return p, ok
}

// Profiles lists the profiles by name.
func (s *Server) Profiles() []ProfileInfo {
	s.profileMu.RLock()
	defer s.profileMu.RUnlock()
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
	return s.StartMessage(profile, conversation, agent.UserText(input))
}

// StartMessage is Start with a user message that may hold images.
func (s *Server) StartMessage(profile, conversation string, input agent.Message) (Snapshot, error) {
	return s.StartFrom(Request{Profile: profile, Conversation: conversation, Input: input})
}

// Request is a run the host starts (StartFrom).
type Request struct {
	// Source labels what started it (SourceOf): a trigger, a chat. The
	// API never labels a run.
	Source       string
	Profile      string
	Conversation string
	// Project is the project the run works on (ProjectOf), the host's
	// name for it; the conversation keeps it (Labels.Project). Empty: the
	// conversation's, if it has one.
	Project string
	Input   agent.Message
}

// StartFrom is StartMessage with all a run can be told: labelled with
// what started it (SourceOf), and the project it works on (ProjectOf).
// The profile's New reads them and may set the run up for them (the tools
// a trigger was allowed, the project's context).
func (s *Server) StartFrom(r Request) (Snapshot, error) {
	source, profile, conversation, input := r.Source, r.Profile, r.Conversation, r.Input
	p, ok := s.profile(profile)
	if !ok {
		return Snapshot{}, fail(ErrNotFound, "no such profile")
	}
	if err := checkInput(input); err != nil {
		return Snapshot{}, err
	}
	switch {
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
	// Under the lock, once no other run has the conversation: the project
	// it keeps cannot change before this run labels it (audit 27/11).
	project, err := s.projectOf(context.Background(), conversation, r.Project)
	if err != nil {
		return Snapshot{}, err
	}
	j := newJob(rand.Text()[:20], p.Name, conversation, s)
	j.source, j.project = source, project
	// The run's context exists before Start returns: a Cancel right away
	// reaches it.
	ctx, cancel := context.WithCancel(context.WithValue(s.ctx, jobKey{}, j))
	j.cancel = cancel
	s.runs[j.id] = j
	s.active++
	s.wg.Add(1)
	go s.execute(ctx, cancel, j, p, input.Clone())
	return j.snapshot(), nil
}

// checkInput accepts a user message with text, images, or PDFs, its
// attachments as the model APIs would take them.
func checkInput(input agent.Message) error {
	switch {
	case input.Role != agent.RoleUser || len(input.Calls) > 0 || input.Result != nil:
		return fail(ErrInput, "the input must be a user message")
	case input.Text() == "" && input.Images() == 0:
		return fail(ErrInput, "the input is empty")
	}
	return checkAttachments(input)
}

// checkAttachments checks the input's images and documents as the model APIs would.
func checkAttachments(m agent.Message) error {
	for _, p := range m.Parts {
		switch p.Type {
		case agent.PartImage:
			if agent.CheckImage(p.MIME, p.Data) != nil {
				return fail(ErrInput, "an image is too large or not JPEG, PNG, GIF or WebP")
			}
		case agent.PartDocument:
			if agent.CheckDocument(p.MIME, p.Data) != nil {
				return fail(ErrInput, "a document must be a PDF up to 20 MiB")
			}
		}
	}
	return nil
}

func (s *Server) admit(conversation string) error {
	if s.ctx.Err() != nil {
		return fail(ErrClosed, "the server is closing")
	}
	if s.paused > 0 {
		return fail(ErrBusy, "the server is pausing its runs (an export or an import): try again in a moment")
	}
	if s.active >= s.maxRuns() {
		return fail(ErrBusy, "too many runs going")
	}
	if conversation != "" && s.held[strings.ToLower(conversation)] != 0 {
		return fail(ErrBusy, "the conversation is being read or changed: try again")
	}
	if s.runningLocked(conversation) {
		return fail(ErrBusy, "the conversation has a run going")
	}
	return nil
}

// runningLocked: a run of conversation is going. Compared without case:
// a store on a case-insensitive file system holds "abc" and "ABC" in one
// file. The caller holds mu.
func (s *Server) runningLocked(conversation string) bool {
	for _, j := range s.runs {
		if conversation != "" && strings.EqualFold(j.conversation, conversation) && !j.finished() {
			return true
		}
	}
	return false
}

// Kinds of hold on a conversation outside a run.
type holdKind int

const (
	holdRead    holdKind = iota // its history: shared with other reads
	holdLabel                   // its labels, beside the history: shared with reads and with a run
	holdExclude                 // deleting it: alone
)

// hold keeps runs off conversation while the caller reads or changes it
// outside a run: reads and labels share it, a delete has it alone. The
// caller calls release.
func (s *Server) hold(conversation string, kind holdKind) (release func(), err error) {
	key := strings.ToLower(conversation)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.held[key]
	switch {
	case s.paused > 0:
		return nil, fail(ErrBusy, "the server is pausing its runs (an export or an import): try again in a moment")
	case n < 0 || (kind == holdExclude && n > 0):
		return nil, fail(ErrBusy, "the conversation is being read or changed: try again")
	case kind != holdLabel && s.runningLocked(conversation):
		return nil, fail(ErrBusy, "the conversation has a run going")
	}
	if kind == holdExclude {
		s.held[key] = -1
	} else {
		s.held[key]++
	}
	return func() { s.releaseHold(key) }, nil
}

func (s *Server) releaseHold(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[key] < 0 {
		delete(s.held, key)
	} else if s.held[key]--; s.held[key] <= 0 {
		delete(s.held, key)
	}
}

// execute runs one job to its end.
func (s *Server) execute(ctx context.Context, cancel context.CancelFunc, j *job, p Profile, input agent.Message) {
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
func (s *Server) work(ctx context.Context, j *job, p Profile, input agent.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fail(ErrConfig, "the profile's agent panicked")
		}
	}()
	ctx = artifact.WithPublished(artifact.WithScope(ctx, j.conversation), j.artifact) // no conversation, no artifacts
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
	s.labelRun(ctx, j.conversation, p.Name, j.project, conv, input)
	err = turn(ctx, j, inst.Agent, conv, input)
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

// Watch calls see with the run's snapshot now and after its changes,
// until the run ends or ctx does. Changes close together may come as one
// snapshot; the last call of a run that ended sees its end. It returns
// ctx's error when ctx ends first.
func (s *Server) Watch(ctx context.Context, id string, see func(Snapshot)) error {
	j, err := s.job(id)
	if err != nil {
		return err
	}
	for {
		_, changed, _, over := j.since(1 << 62)
		see(j.snapshot())
		if over {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
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
// Call it before Serve. /health is the server's own: a mount there is
// never reached.
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

// Pause stops new runs and waits up to wait for those going (and any
// conversation reads or changes) to end: a host exports or imports the
// state while nothing touches it. resume lets runs start again; calling it
// more than once is safe. With an error nothing is held.
func (s *Server) Pause(ctx context.Context, wait time.Duration) (resume func(), err error) {
	s.mu.Lock()
	s.paused++
	s.mu.Unlock()
	var once sync.Once
	resume = func() {
		once.Do(func() {
			s.mu.Lock()
			s.paused--
			s.mu.Unlock()
		})
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		going := s.active + len(s.held)
		s.mu.Unlock()
		if going == 0 {
			return resume, nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			resume()
			return nil, fail(ErrBusy, strconv.Itoa(going)+" runs or active requests are still going: wait for them, or cancel them, and try again")
		case <-ctx.Done():
			resume()
			return nil, ctx.Err()
		}
	}
}
