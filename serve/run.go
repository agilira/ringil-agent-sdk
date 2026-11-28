// run.go: one run of the API, its event log and its approvals
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
	"github.com/agilira/ringil-agent-sdk/delegate"
)

// State is where a run is.
type State string

// Run states.
const (
	Running   State = "running"
	Waiting   State = "waiting" // a tool call waits for approval
	Completed State = "completed"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

// Snapshot describes a run.
type Snapshot struct {
	ID           string           `json:"id"`
	Profile      string           `json:"profile"`
	Conversation string           `json:"conversation,omitzero"`
	Project      string           `json:"project,omitzero"`
	State        State            `json:"state"`
	Started      time.Time        `json:"started"`
	Finished     time.Time        `json:"finished,omitzero"`
	Text         string           `json:"text,omitzero"`
	Stop         agent.StopReason `json:"stop,omitzero"`
	Turns        int              `json:"turns,omitzero"`
	Usage        agent.Usage      `json:"usage,omitzero"`
	Error        *ErrorInfo       `json:"error,omitzero"`
	Pending      []Approval       `json:"pending,omitzero"`
	// Events is how many events the run has produced; Dropped how many of
	// the oldest are no longer kept.
	Events  int `json:"events"`
	Dropped int `json:"dropped,omitzero"`
}

// ErrorInfo is why a run failed: its code and the message meant for the
// user, never the internal detail.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Approval is a tool call waiting for a decision. ID, given by the
// server, is what a decision names: call IDs come from the model's
// provider and two waiting calls (the agent's and a worker's) may share
// one.
type Approval struct {
	ID     string          `json:"id"`
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	// Worker, when a worker of the agent asks, is its name and task, as
	// "researcher 2".
	Worker string `json:"worker,omitzero"`
	// Rule, when there is one, is the allow rule a "yes, this session" or
	// "yes, always" keeps (as bash(git status*)); without it the answer
	// is this call's only.
	Rule string `json:"rule,omitzero"`
	// Session: "yes, in this conversation" applies (a rule, and a run in a
	// conversation: a one-off run has none to hold it).
	Session bool `json:"session,omitzero"`
	// Explain: a refusal's message reaches the agent (a tool call's; a
	// tool's own question, as a sandbox's, takes yes or no).
	Explain bool `json:"explain,omitzero"`
}

type jobKey struct{}

// ConversationOf is the conversation a run of a server continues, from
// the context its profile's New gets; "" for a run without one.
func ConversationOf(ctx context.Context) string {
	if j, ok := ctx.Value(jobKey{}).(*job); ok {
		return j.conversation
	}
	return ""
}

// ProjectOf is the project a run works on (Request.Project, or its
// conversation's), from the context its profile's New gets; "" for none.
func ProjectOf(ctx context.Context) string {
	if j, ok := ctx.Value(jobKey{}).(*job); ok {
		return j.project
	}
	return ""
}

// SourceOf is what started a run, as the host labelled it with
// StartFrom ("trigger:mail", "telegram"); "" for a run of the API, which
// never sets it.
func SourceOf(ctx context.Context) string {
	if j, ok := ctx.Value(jobKey{}).(*job); ok {
		return j.source
	}
	return ""
}

type entry struct {
	id   int
	data []byte
}

type pending struct {
	approval Approval
	decide   chan agent.Answer // buffered: the decision never blocks the API
	decided  bool
}

type job struct {
	id, profile, conversation string
	source                    string // who started it, the host's label
	project                   string // the project it works on
	srv                       *Server

	mu      sync.Mutex
	state   State
	started time.Time
	ended   time.Time
	result  *agent.Result
	err     *ErrorInfo
	log     []entry
	next    int // id of the next event
	bytes   int
	dropped int
	changed chan struct{} // closed and replaced on every change
	cancel  context.CancelFunc
	waiting map[string]*pending // by approval ID
	asked   int                 // approvals so far, for their IDs
}

func newJob(id, profile, conversation string, s *Server) *job {
	return &job{id: id, profile: profile, conversation: conversation, srv: s, state: Running,
		started: s.now(), next: 1, changed: make(chan struct{}), waiting: map[string]*pending{}}
}

func (j *job) cancelRun() {
	j.mu.Lock()
	c := j.cancel
	j.mu.Unlock()
	if c != nil {
		c()
	}
}

// add appends an agent event to the log.
func (j *job) add(ev agent.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ev.Kind == agent.EventDone && ev.Result != nil {
		r := *ev.Result
		j.result = &r
	}
	j.append(wire(ev))
}

// append stores one event; j.mu held.
func (j *job) append(w wireEvent) {
	w.ID = j.next
	j.next++
	data, err := json.Marshal(w)
	if err != nil { // never silent: the event is there, without its content
		data, _ = json.Marshal(wireEvent{ID: w.ID, Kind: w.Kind, Run: w.Run, Seq: w.Seq, Error: &ErrorInfo{Code: "UNENCODABLE", Message: "this event could not be encoded"}})
	}
	j.log = append(j.log, entry{w.ID, data})
	j.bytes += len(data)
	for j.bytes > j.srv.maxLog() && len(j.log) > 1 {
		j.bytes -= len(j.log[0].data)
		j.log = j.log[1:]
		j.dropped++
	}
	j.notify()
}

// notify wakes the readers; j.mu held.
func (j *job) notify() {
	close(j.changed)
	j.changed = make(chan struct{})
}

// end records how the run ended.
func (j *job) end(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ended = j.srv.now()
	switch {
	case err == nil:
		j.state = Completed
	case stderrors.Is(err, context.Canceled):
		j.state = Cancelled
	default:
		j.state = Failed
	}
	if err != nil {
		j.err = errorInfo(err)
	}
	for _, p := range j.waiting { // the run is over: nobody waits now
		close(p.decide)
	}
	j.waiting = map[string]*pending{}
	j.append(wireEvent{Kind: "run_end", State: string(j.state)})
}

func errorInfo(err error) *ErrorInfo {
	code := "RUN_FAILED"
	var e *errors.Error
	if stderrors.As(err, &e) {
		code = string(e.Code)
	} else if stderrors.Is(err, context.Canceled) {
		code = "CANCELLED"
	}
	return &ErrorInfo{Code: code, Message: userMessage(err)}
}

// userMessage is the first message meant for the user in err's chain;
// without one, a plain sentence: internal detail never leaves.
func userMessage(err error) string {
	for e := err; e != nil; e = stderrors.Unwrap(e) {
		if coded, ok := e.(*errors.Error); ok && coded.UserMsg != "" {
			return coded.UserMsg
		}
	}
	if stderrors.Is(err, context.Canceled) {
		return "The run was cancelled."
	}
	return "The run failed."
}

// safeJSON is raw when it is valid JSON, else raw as a JSON string: a
// provider that does not check its tool input must not break the log.
func safeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || json.Valid(raw) {
		return raw
	}
	q, _ := json.Marshal(string(raw))
	return q
}

func (j *job) finished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.ended.IsZero()
}

func (j *job) finishedAt() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.ended
}

func (j *job) snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{ID: j.id, Profile: j.profile, Conversation: j.conversation, Project: j.project, State: j.state,
		Started: j.started, Finished: j.ended, Error: j.err, Events: j.next - 1, Dropped: j.dropped}
	if j.result != nil {
		s.Text, s.Stop, s.Turns, s.Usage = j.result.Text, j.result.Stop, j.result.Turns, j.result.Usage
	}
	for _, p := range j.waiting {
		s.Pending = append(s.Pending, p.approval)
	}
	sort.Slice(s.Pending, func(a, b int) bool { // in the order asked: "a9" before "a10"
		x, y := s.Pending[a].ID, s.Pending[b].ID
		return len(x) < len(y) || (len(x) == len(y) && x < y)
	})
	if len(s.Pending) > 0 && j.state == Running {
		s.State = Waiting
	}
	return s
}

// since returns the events after id, a channel closed at the next
// change, how many asked-for events were dropped, and whether the run is
// over.
func (j *job) since(id int) (out []entry, changed <-chan struct{}, missed int, over bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.log) > 0 && j.log[0].id > id+1 {
		missed = j.log[0].id - id - 1
	}
	i := sort.Search(len(j.log), func(k int) bool { return j.log[k].id > id })
	out = append(out, j.log[i:]...)
	return out, j.changed, missed, !j.ended.IsZero()
}

// Approve is an approval hook for agent.Config.Approve: the caller says
// yes or no, and why not (no rule is kept: that is a Policy's, see Ask).
func (s *Server) Approve(ctx context.Context, call agent.ToolCall, _ agent.ToolSpec) (agent.Decision, error) {
	a, err := s.ask(ctx, call, "", true)
	return agent.Decision{Allow: a.Allow, Message: a.Message}, err
}

// Ask is a Policy's Ask (agent.Policy.Ask): the API caller answers a
// tool call as people answer the coding agents: yes, yes for this
// conversation, yes always (Remember: the Policy keeps the rule, its host
// where the next session reads it), or no with what the agent should do
// instead. It waits until the caller decides (POST
// /v1/runs/{id}/approvals/{approval}), the run ends, or ApprovalTimeout
// passes, which refuses it. Outside a run of this server it refuses.
func (s *Server) Ask(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (agent.Answer, error) {
	rule := ""
	if r, ok := agent.RuleFor(call, spec); ok && spec.Name != "" {
		rule = r.String()
	}
	return s.ask(ctx, call, rule, true)
}

// Confirm asks the API caller a tool's own question about call (a command
// outside the sandbox, a domain): yes or no, nothing kept.
func (s *Server) Confirm(ctx context.Context, call agent.ToolCall) (bool, error) {
	a, err := s.ask(ctx, call, "", false)
	return a.Allow, err
}

func (s *Server) ask(ctx context.Context, call agent.ToolCall, rule string, explain bool) (agent.Answer, error) {
	j, ok := ctx.Value(jobKey{}).(*job)
	if !ok || j.srv != s {
		return agent.Answer{}, fail(ErrInput, "not a run of this server")
	}
	p := j.ask(call, worker(ctx), rule, explain)
	timer := time.NewTimer(s.approvalTimeout())
	defer timer.Stop()
	var answer agent.Answer
	how := "caller"
	select {
	case answer = <-p.decide:
	case <-timer.C:
		how = "timeout"
	case <-ctx.Done():
		how = "cancelled"
	}
	return j.settle(p, answer, how), nil
}

// worker names the worker asking, "" for the agent itself.
func worker(ctx context.Context) string {
	if info, ok := delegate.Current(ctx); ok {
		return info.Worker + " " + strconv.Itoa(info.Task)
	}
	return ""
}

// ask records a call waiting for approval.
func (j *job) ask(call agent.ToolCall, worker, rule string, explain bool) *pending {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.asked++
	p := &pending{approval: Approval{ID: "a" + strconv.Itoa(j.asked), CallID: call.ID, Tool: call.Name, Input: safeJSON(call.Input), Worker: worker,
		Rule: rule, Session: rule != "" && j.conversation != "", Explain: explain}, decide: make(chan agent.Answer, 1)}
	j.waiting[p.approval.ID] = p
	a := p.approval
	j.append(wireEvent{Kind: "approval", Approval: &a})
	return p
}

// settle closes a call's wait and returns the decision: a caller's
// decision accepted while the wait was ending still counts.
func (j *job) settle(p *pending, answer agent.Answer, how string) agent.Answer {
	j.mu.Lock()
	defer j.mu.Unlock()
	if how != "caller" && p.decided {
		answer, how = <-p.decide, "caller" // buffered: already there
	}
	p.decided = true // no decision is accepted after this
	if j.waiting[p.approval.ID] == p {
		delete(j.waiting, p.approval.ID)
	}
	if j.ended.IsZero() {
		j.append(wireEvent{Kind: "approval_decided", Decision: &Decision{ID: p.approval.ID, CallID: p.approval.CallID, Allowed: answer.Allow, By: how,
			Remember: rememberName[answer.Remember], Message: answer.Message}})
	}
	if !answer.Allow && answer.Message != "" { // the person's words, said as theirs
		answer.Message = "The user did not approve this call, and said: " + answer.Message
	}
	return answer
}

// MaxExplain bounds what a refusal tells the agent.
const MaxExplain = 4000

var rememberName = map[agent.Scope]string{agent.RememberSession: "session", agent.RememberProject: "always"}

// decide gives the caller's answer on a waiting call: a rule is kept
// only of a call that has one, a message only with a refusal that
// reaches the agent.
func (j *job) decide(approval string, a agent.Answer) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	p, ok := j.waiting[approval]
	if !ok || p.decided {
		return fail(ErrNotFound, "no call of this run waits for that decision")
	}
	if err := checkAnswer(p.approval, a); err != nil {
		return err
	}
	p.decided = true
	p.decide <- a
	return nil
}

func checkAnswer(ap Approval, a agent.Answer) error {
	if err := checkRemember(ap, a); err != nil {
		return err
	}
	switch {
	case a.Message != "" && (a.Allow || !ap.Explain):
		return fail(ErrInput, "only a no to a tool call says why")
	case len(a.Message) > MaxExplain || !utf8.ValidString(a.Message):
		return fail(ErrInput, "what the agent is told is UTF-8 text of at most 4000 bytes")
	}
	return nil
}

func checkRemember(ap Approval, a agent.Answer) error {
	switch {
	case a.Remember != agent.RememberNone && (!a.Allow || ap.Rule == ""):
		return fail(ErrInput, "only a yes to a call with a rule is remembered")
	case a.Remember == agent.RememberSession && !ap.Session:
		return fail(ErrInput, "a run with no conversation has no session to remember it in")
	}
	return nil
}

// Decide answers a run's waiting tool call, named by its approval ID.
func (s *Server) Decide(id, approval string, a agent.Answer) error {
	j, err := s.job(id)
	if err != nil {
		return err
	}
	return j.decide(approval, a)
}

// Decision is how a waiting call was decided: by the caller, a timeout,
// or the run's end.
type Decision struct {
	ID      string `json:"id"`
	CallID  string `json:"call_id"`
	Allowed bool   `json:"allowed"`
	By      string `json:"by"`
	// Remember is "session" or "always" when the yes keeps a rule.
	Remember string `json:"remember,omitzero"`
	// Message is what the person told the agent with a no.
	Message string `json:"message,omitzero"`
}

// artifact records a new version an artifact tool made in the run.
// One made after the run ended (by a background task the run did not
// wait for) is not an event: the stream is over; the app lists it.
func (j *job) artifact(info artifact.Info) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.ended.IsZero() {
		return
	}
	j.append(wireEvent{Kind: "artifact", Artifact: &info})
}
