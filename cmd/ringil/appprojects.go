// appprojects.go: projects several agents work on in turn
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/project"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// projectsDir holds the projects: those of the app, and the one of each
// folder the CLI runs in (its notes there since before, made a project
// at its first use from the app).
const projectsDir = "projects"

func (c *cli) projectStore() (*project.Store, error) {
	root, err := c.stateRoot()
	if err != nil {
		return nil, err
	}
	return project.Open(filepath.Join(root, projectsDir))
}

// actor is who this run acts as on a project's timeline.
func (c *cli) actor() string {
	return "agent:" + or(c.roleName, "default")
}

// projectTools are the tools of a run of serve on a project (ProjectOf):
// its board and timeline, the store open for the run.
func (c *cli) projectTools() []agent.Tool {
	if c.project == "" || c.restricted() {
		return nil
	}
	store, err := c.projectStore()
	if err != nil {
		return nil
	}
	c.closers = append(c.closers, store.Close)
	c.projects = store
	return store.Tools(c.project, project.ToolOptions{Actor: c.actor(), Conversation: c.runConversation, OnEvent: c.handOff})
}

// checkRunProject: the project a run is on is there and not archived.
func (c *cli) checkRunProject() error {
	if c.project == "" {
		return nil
	}
	s, err := c.projectStore()
	if err != nil {
		return err
	}
	defer closeQuietly(s)
	p, err := s.Get(c.project)
	switch {
	case err != nil:
		return errors.Wrap(err, ErrUsage, "no project "+c.project)
	case p.Archived:
		return errors.New(ErrUsage, "project "+c.project+" is archived: unarchive it to work on it")
	}
	return nil
}

// projectBlock is the project's context for the model, at each run: its
// brief, board and latest events.
func (c *cli) projectBlock() string {
	if c.projects == nil {
		return ""
	}
	text, err := c.projects.Context(c.project, 30)
	if err != nil {
		return ""
	}
	return text
}

// projectNotesDir is where a run's project notes are: its project's, in a
// run on one; else those of the start folder.
func (c *cli) projectNotesDir(root, cwd string) string {
	if c.project != "" {
		if s, err := c.projectStore(); err == nil {
			defer closeQuietly(s)
			if dir, err := s.NotesDir(c.project); err == nil {
				return dir
			}
		}
	}
	return filepath.Join(root, projectsDir, projectKey(cwd), "notes")
}

// projectFree: the project's tools run without asking, as the notes do:
// they change only what the project's agents share (a rule may still ask).
func (c *cli) projectFree(p *agent.Policy, call agent.ToolCall, spec agent.ToolSpec) bool {
	if c.project == "" || p.AskedByRule(call, spec) || mayHandOn(call) {
		return false
	}
	for _, n := range project.ToolNames {
		if call.Name == n {
			return true
		}
	}
	return false
}

// mayHandOn: a call of project_card that gives a card an agent or moves
// it: it may start another agent's run, which spends; it is asked as any
// change (audit 27/11).
func mayHandOn(call agent.ToolCall) bool {
	if call.Name != project.CardTool {
		return false
	}
	// Every key in any case, as the tool's decoding may take any of them
	// (audit 27/11: "AGENT":"" beside "agent":"bob" passed unasked).
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Input, &fields) != nil {
		return true
	}
	change := false
	for k, v := range fields {
		switch strings.ToLower(k) {
		case "agent":
			if string(v) != "null" && string(v) != `""` {
				return true
			}
		case "column":
			change = true
		}
	}
	return change && !onlyAdd(fields)
}

// onlyAdd: every action key says add (a new card goes where it is told,
// its agent asked above).
func onlyAdd(fields map[string]json.RawMessage) bool {
	seen := false
	for k, v := range fields {
		if strings.EqualFold(k, "action") {
			seen = true
			if string(v) != `"add"` {
				return false
			}
		}
	}
	return seen
}

// projectObserver records on the project's timeline what a run does that
// the others should know: that it ran, and what it changed.
func (c *cli) projectObserver() agent.Observer {
	if c.projects == nil {
		return nil
	}
	store, id, actor, conv := c.projects, c.project, c.actor(), c.runConversation
	return func(ev agent.Event) {
		e, ok := c.timelineEvent(ev)
		if !ok {
			return
		}
		e.Actor, e.Conversation, e.Run = actor, conv, ev.Run
		if ev.Worker != "" {
			e.Text = "(worker " + ev.Worker + ") " + e.Text
		}
		if _, err := store.Log(id, e); err != nil {
			c.note("project " + id + ": " + describe(err))
		}
	}
}

// timelineEvent is what an agent event is on the timeline, if anything:
// a run's start and end (not its workers'), a call that changed something.
func (c *cli) timelineEvent(ev agent.Event) (project.Event, bool) {
	if ev.Kind == agent.EventToolEnd && ev.Call != nil && ev.Output != nil && !ev.Output.IsError {
		return c.actionEvent(*ev.Call)
	}
	if ev.Parent != "" {
		return project.Event{}, false // a worker's run: its parent's is the one
	}
	return runEvent(ev)
}

// runEvent is a run's start or end on the timeline.
func runEvent(ev agent.Event) (project.Event, bool) {
	switch ev.Kind {
	case agent.EventStarted:
		return project.Event{Kind: project.RunStarted}, true
	case agent.EventDone:
		text := "ended"
		if ev.Result != nil {
			text = "ended: " + oneLine(ev.Result.Text, 300)
		}
		return project.Event{Kind: project.RunEnded, Text: text}, true
	case agent.EventFailed:
		return project.Event{Kind: project.RunEnded, Text: "failed: " + oneLine(describe(ev.Err), 300)}, true
	}
	return project.Event{}, false
}

// actionEvent is a call that changed something: its tool and the path it
// names, never its content (it may be private); the project's own tools
// record themselves, reading changes nothing.
func (c *cli) actionEvent(call agent.ToolCall) (project.Event, bool) {
	for _, n := range project.ToolNames {
		if call.Name == n {
			return project.Event{}, false
		}
	}
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(call.Input, &in)
	if call.Name == notes.DefaultName {
		if in.Command == "view" || !strings.HasPrefix(in.Path, "/project/") {
			return project.Event{}, false
		}
		return project.Event{Kind: project.Note, Text: oneLine(in.Command+" "+in.Path, 300)}, true
	}
	spec, ok := specOf(c.finalTools, call.Name)
	if ok && spec.Annotations.ReadOnly {
		return project.Event{}, false
	}
	text := "used " + call.Name
	if in.Path != "" {
		text += " on " + in.Path
	}
	return project.Event{Kind: project.Action, Text: oneLine(text, 300)}, true
}

// Hand-offs: a card given to an agent in a column that starts work.

// handOffs bounds the runs cards start by themselves, so that agents
// handing cards on do not run for ever: per card, per project, and along
// a chain of hand-offs (a run started by one hands on at one level more).
var handOffs = struct {
	mu   sync.Mutex
	runs map[string][]time.Time // by card, and by "project:<id>"
}{runs: map[string][]time.Time{}}

const (
	maxHandOffs        = 10 // a card's runs started by hand-off in an hour
	maxProjectHandOffs = 30 // a project's
	maxHandOffDepth    = 8  // hand-offs in a chain
	handOffWindow      = time.Hour
	handOffSource      = "project:"
)

// handOff starts the agent's run on a card handed to it, when serve runs:
// in the card's own conversation with that agent, on the project.
func (c *cli) handOff(p project.Project, ev project.Event) {
	if !p.Starts(ev) || c.server == nil || c.workflowLife == nil || p.Archived {
		return
	}
	if why := c.handOffRefused(p, ev); why != "" {
		c.logSystem(p.ID, ev.Card, why)
		return
	}
	source := handOffSource + p.ID + ":" + strconv.Itoa(c.handDepth+1)
	r := serve.Request{Source: source, Profile: ev.Agent, Conversation: handOffConversation(ev.Card, ev.Agent), Project: p.ID, Input: agent.UserText(handOffInput(p, ev))}
	go c.runHandedOn(p.ID, ev, r)
}

// handOffConversation keeps a card+agent conversation ID within the 64-byte
// session ID limit.
func handOffConversation(card, agentName string) string {
	id := "p-" + card + "-" + agentName
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return id[:55] + "-" + hex.EncodeToString(sum[:4])
}

// runHandedOn runs a card's agent, and says on the timeline when the run
// did not start or failed before its first word (its own events say
// nothing then).
func (c *cli) runHandedOn(id string, ev project.Event, r serve.Request) {
	life := c.workflowLife
	snap, err := startWaiting(life, c.server, r)
	if err == nil {
		snap, err = c.server.Wait(life, snap.ID)
	}
	switch {
	case err != nil && life.Err() == nil:
		c.logSystem(id, ev.Card, "the run of "+ev.Agent+" did not start: "+oneLine(describe(err), 300))
	case snap.Error != nil && snap.Turns == 0:
		c.logSystem(id, ev.Card, "the run of "+ev.Agent+" failed: "+oneLine(snap.Error.Message, 300))
	}
}

// handOffRefused says why a card is not handed on now, "" when it is.
func (c *cli) handOffRefused(p project.Project, ev project.Event) string {
	switch {
	case !c.isAgent(ev.Agent):
		return "no agent " + ev.Agent + " to hand the card to (an agent for the chats' members or the public cannot work on projects)"
	case c.handDepth >= maxHandOffDepth:
		return "this card came at the end of a chain of " + strconv.Itoa(maxHandOffDepth) + " hand-offs: not started; move it by hand to go on"
	case !takeHandOff(p.ID, ev.Card):
		return "too many hand-offs in an hour (10 a card, 30 a project): not started; move it by hand to go on"
	}
	return ""
}

// isAgent: name is an agent serve runs that may work on a project (not
// one for the chats' members or the public: it has no project tools).
func (c *cli) isAgent(name string) bool {
	if p, ok := c.conf().Profiles[name]; ok && p.Tools != nil {
		return false
	}
	for _, p := range c.server.Profiles() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// takeHandOff counts a hand-off of card on project, if both have room.
func takeHandOff(projectID, card string) bool {
	handOffs.mu.Lock()
	defer handOffs.mu.Unlock()
	now := time.Now()
	pk := handOffSource + projectID
	cards, projects := recent(handOffs.runs[card], now), recent(handOffs.runs[pk], now)
	if len(cards) >= maxHandOffs || len(projects) >= maxProjectHandOffs {
		handOffs.runs[card], handOffs.runs[pk] = cards, projects
		return false
	}
	handOffs.runs[card], handOffs.runs[pk] = append(cards, now), append(projects, now)
	for k, v := range handOffs.runs { // nothing kept of cards gone quiet
		if len(recent(v, now)) == 0 {
			delete(handOffs.runs, k)
		}
	}
	return true
}

// recent are the times within the window.
func recent(times []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range times {
		if now.Sub(t) < handOffWindow {
			out = append(out, t)
		}
	}
	return out
}

// handDepthOf is how deep in a chain of hand-offs a run's source is.
func handDepthOf(source string) int {
	rest, ok := strings.CutPrefix(source, handOffSource)
	if !ok {
		return 0
	}
	_, n, ok := strings.Cut(rest, ":")
	if !ok {
		return 0
	}
	d, err := strconv.Atoi(n)
	if err != nil || d < 0 {
		return maxHandOffDepth
	}
	return d
}

// handOffInput is what the agent is told: the card is its to work on.
func handOffInput(p project.Project, ev project.Event) string {
	return "The card " + ev.Card + " of the project " + p.Name + " was handed to you (" + ev.Column + "). " +
		"Read it with project_board, move it on as you work (project_card), record what you decide (project_log), " +
		"and when you are done move it to the column that follows, giving it to whoever goes next if the card or the brief says so."
}

// startWaiting starts a run, waiting while serve is full, a while.
func startWaiting(ctx context.Context, srv *serve.Server, r serve.Request) (serve.Snapshot, error) {
	deadline := time.Now().Add(busyFor)
	for {
		snap, err := srv.StartFrom(r)
		switch {
		case err == nil:
			return snap, nil
		case !errors.HasCode(err, serve.ErrBusy):
			return snap, err
		case time.Now().After(deadline):
			return snap, errors.Wrap(err, ErrUsage, "serve stayed full for "+busyFor.String()+": raise serve.max_runs, or run fewer things at once")
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-time.After(busyWait):
		}
	}
}

// logSystem records on a project's timeline what the system says of a
// card.
func (c *cli) logSystem(id, card, text string) {
	s, err := c.projectStore()
	if err != nil {
		return
	}
	defer closeQuietly(s)
	if _, err := s.Log(id, project.Event{Actor: "system", Kind: project.Comment, Card: card, Text: text}); err != nil {
		c.note("project " + id + ": " + describe(err))
	}
}

// The projects' API, mounted with the settings:
//
//	GET    /v1/projects                         the projects, the latest changed first (?archived=true with the archived)
//	POST   /v1/projects                         {"name","brief"?,"columns"?,"folder"?} make one: 201 with it
//	GET    /v1/projects/{id}                    one, with its board
//	PATCH  /v1/projects/{id}                    {"name"?,"brief"?,"columns"?,"folder"?,"archived"?}
//	DELETE /v1/projects/{id}                    remove it, with its cards, timeline and notes
//	POST   /v1/projects/{id}/cards              {"title","text"?,"column"?,"agent"?,"person"?} a card: 201
//	PATCH  /v1/projects/{id}/cards/{card}       {"title"?,"text"?,"column"?,"index"?,"agent"?,"person"?} change or move it
//	DELETE /v1/projects/{id}/cards/{card}       take it off the board
//	GET    /v1/projects/{id}/timeline           ?from=&limit= a page of what happened (from 0: the latest)
//	POST   /v1/projects/{id}/timeline           {"kind":"comment"|"decision","text","card"?} the user's word
//	GET    /v1/projects/{id}/notes              the notes' paths
//	GET    /v1/projects/{id}/notes/{path...}    one note's text
//	PUT    /v1/projects/{id}/notes/{path...}    {"text"} write one
//	DELETE /v1/projects/{id}/notes/{path...}    remove one
//
// The user acts as "user" on the timeline. A card given to an agent in a
// column that starts work starts its run, as when an agent hands it on.
// A chat on a project is POST /v1/runs with "project"; its conversations
// are GET /v1/conversations?project=<id>.

// startProjectTool is start_project, in a run of serve of the owner's that
// is on no project (not a trigger's, not a chat member's): it asks before
// it makes one, and moves the conversation into it.
func (c *cli) startProjectTool() []agent.Tool {
	if c.server == nil || c.project != "" || c.inTrigger || c.restricted() {
		return nil
	}
	store, err := c.projectStore()
	if err != nil {
		return nil
	}
	c.closers = append(c.closers, store.Close)
	return []agent.Tool{store.StartTool(project.StartOptions{Actor: c.actor(), Started: c.projectStarted})}
}

// projectStarted moves the run's conversation into the project just made
// and hands its first cards on.
func (c *cli) projectStarted(p project.Project, evs []project.Event) {
	if c.runConversation != "" {
		id := p.ID
		if err := c.server.ChangeConversation(context.Background(), c.runConversation, serve.ConversationChange{Project: &id}); err != nil {
			c.note("project " + p.ID + ": the conversation was not moved into it: " + describe(err))
		}
	}
	for _, ev := range evs {
		c.handOff(p, ev)
	}
}

// projectNote tells a run started with /project what it is for.
const projectNote = "This run makes a project the user asked for with /project: work that several agents and people will share in turn. " +
	"Make it with start_project: a name, a brief that says what it is for and what done means, and the first cards " +
	"(give a card to an agent only when the user said who does it). Ask the user only what you cannot infer; the approval shows them all of it."

// projectSystem adds, in a run started with /project, what it is for.
func (c *cli) projectSystem(system string) string {
	switch {
	case !c.projecting:
		return system
	case c.project != "":
		return system + "\n\n" + "The user asked with /project to make a project, but this conversation belongs to project " + c.project +
			" already: tell them /new starts a fresh conversation where they can make another."
	}
	return system + "\n\n" + projectNote
}

// cmdProjects lists the projects: name, id, cards by column.
func (c *cli) cmdProjects(*orpheus.Context) error {
	s, err := c.projectStore()
	if err != nil {
		return err
	}
	defer closeQuietly(s)
	list, err := s.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return c.line("No projects. Make one with: ringil project new \"Q1 plan\" --brief \"what it is for\" (or from the app, or /project in a chat)")
	}
	for _, p := range list {
		if err := c.line(projectLine(s, p)); err != nil {
			return err
		}
	}
	return nil
}

// projectLine is a project in a line: name, id, its cards by column.
func projectLine(s *project.Store, p project.Project) string {
	cards, _ := s.Cards(p.ID)
	counts := map[string]int{}
	for _, card := range cards {
		counts[strings.ToLower(card.Column)]++
	}
	var cols []string
	for _, col := range p.Columns {
		cols = append(cols, clean(col.Name)+" "+strconv.Itoa(counts[strings.ToLower(col.Name)]))
	}
	line := clean(p.Name) + "  (" + p.ID + ")  " + strings.Join(cols, " · ")
	if p.Archived {
		line += "  archived"
	}
	return line
}

// cmdProjectNew makes a project from the terminal.
func (c *cli) cmdProjectNew(ctx *orpheus.Context) error {
	name := strings.TrimSpace(strings.Join(ctx.Positional(), " "))
	if name == "" {
		return errors.New(ErrUsage, "usage: ringil project new <name> [--brief TEXT]")
	}
	s, err := c.projectStore()
	if err != nil {
		return err
	}
	defer closeQuietly(s)
	p, err := s.Create(project.Project{Name: name, Brief: ctx.GetFlagString("brief")}, "user")
	if err != nil {
		return err
	}
	return c.line("Project " + clean(p.Name) + " made (" + p.ID + "): open it in the app to fill its board and give its cards to the agents.")
}
