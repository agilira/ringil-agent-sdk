// appagents.go: the agents an app makes, each with its own prompt
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/trigger"
	"github.com/agilira/ringil-agent-sdk/workflow"
)

// profilesDir holds the instructions of the agents made from the app,
// one file each, beside ringil.yaml: <config>/ringil/profiles/<name>.md.
const profilesDir = "profiles"

// The agents' API, mounted with the settings: each agent is a profile of
// ringil.yaml, with its own models, instructions, tools and memory.
//
//	GET    /v1/settings/profiles         every agent, with its instructions
//	PUT    /v1/settings/profiles/{name}  {"description","models"?,"instructions"?,"tools"?,"memory"?,"max_turns"?,"images"?,"embed_model"?} make or replace one
//	DELETE /v1/settings/profiles/{name}  remove one no trigger or chat uses
//
// serve takes a change at once: a new agent answers its next run, one
// removed takes none. "default" is the main agent, set by the other
// settings.
func (c *cli) mountProfiles(octx *orpheus.Context, srv *serve.Server) {
	srv.Mount("GET /v1/settings/profiles", http.HandlerFunc(c.getProfiles))
	srv.Mount("PUT /v1/settings/profiles/{name}", c.putProfile(octx))
	srv.Mount("DELETE /v1/settings/profiles/{name}", http.HandlerFunc(c.deleteProfile))
}

// ProfileOut is an agent as the app shows it.
type ProfileOut struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitzero"`
	Models      []string `json:"models"`
	// Instructions is the text of its instructions file; InstructionsPath
	// where it is.
	Instructions     string `json:"instructions"`
	InstructionsPath string `json:"instructions_path,omitzero"`
	// Tools nil: every tool; a list, only those.
	Tools      *[]string `json:"tools"`
	Memory     string    `json:"memory,omitzero"`
	MaxTurns   int       `json:"max_turns,omitzero"`
	Images     bool      `json:"images,omitzero"`
	EmbedModel string    `json:"embed_model,omitzero"`
}

func (c *cli) getProfiles(w http.ResponseWriter, _ *http.Request) {
	f := c.conf()
	out := make([]ProfileOut, 0, len(f.Profiles))
	for _, name := range names(f.Profiles) {
		p := f.Profiles[name]
		text := ""
		if p.Instructions != "" {
			data, err := readPrivate(p.Instructions)
			if err != nil && !os.IsNotExist(err) {
				fail(w, http.StatusInternalServerError, "SETTINGS_INSTRUCTIONS", "the instructions of "+name+": "+describe(err))
				return
			}
			text = string(data)
		}
		out = append(out, ProfileOut{Name: name, Description: p.Description, Models: nonNil(p.Models), Instructions: text,
			InstructionsPath: p.Instructions, Tools: p.Tools, Memory: p.Memory, MaxTurns: p.MaxTurns, Images: p.Images, EmbedModel: p.EmbedModel})
	}
	writeJSON(w, http.StatusOK, out)
}

// profileIn is an agent as the app sends it; Instructions nil keeps the
// file the agent has.
type profileIn struct {
	Description  string    `json:"description"`
	Models       []string  `json:"models,omitempty"`
	Instructions *string   `json:"instructions,omitempty"`
	Tools        *[]string `json:"tools,omitempty"`
	Memory       string    `json:"memory,omitempty"`
	MaxTurns     int       `json:"max_turns,omitempty"`
	Images       bool      `json:"images,omitempty"`
	EmbedModel   string    `json:"embed_model,omitempty"`
}

func (c *cli) putProfile(octx *orpheus.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var in profileIn
		if !decodeJSONLimit(w, r, &in, 8*agent.MaxInstructions) { // room for JSON's escapes
			return
		}
		if msg := c.checkProfileIn(octx, name, in); msg != "" {
			fail(w, http.StatusBadRequest, "SETTINGS_INPUT", msg)
			return
		}
		c.keepMu.Lock()
		defer c.keepMu.Unlock()
		if c.settings == nil || c.settings.path == "" {
			fail(w, http.StatusConflict, "SETTINGS_NO_FILE", "serve runs without a configuration directory: there is no ringil.yaml for the agents")
			return
		}
		c.writeProfile(w, name, in)
	}
}

// checkProfileIn says what is wrong with an agent the app sends, "" for
// nothing: its name, its description, its instructions, its tools.
func (c *cli) checkProfileIn(octx *orpheus.Context, name string, in profileIn) string {
	switch {
	case !profileName.MatchString(name) || name == "default":
		return "an agent's name is 1-64 letters, digits, - and _, and not default"
	case len(in.Description) > 1024:
		return "the description is at most 1 KiB"
	case in.Instructions != nil && (len(*in.Instructions) > agent.MaxInstructions || !utf8.ValidString(*in.Instructions)):
		return "the instructions are UTF-8 text of at most " + strconv.Itoa(agent.MaxInstructions>>10) + " KiB"
	case caseTwin(c.conf().Profiles, name) != "":
		return "agent " + caseTwin(c.conf().Profiles, name) + " differs only in case: its instructions file would be the same on some systems"
	case in.Tools != nil:
		return c.unknownTools(octx, *in.Tools)
	}
	return ""
}

// caseTwin is a profile whose name differs from name only in case.
func caseTwin(all map[string]profile, name string) string {
	for n := range all {
		if n != name && strings.EqualFold(n, name) {
			return n
		}
	}
	return ""
}

// writeProfile writes the agent's instructions file, then its entry; the
// caller holds keepMu. A file written for an entry that is then refused
// is put back as it was.
func (c *cli) writeProfile(w http.ResponseWriter, name string, in profileIn) {
	cur := c.conf().Profiles[name]
	p := profile{Description: in.Description, Models: in.Models, Instructions: cur.Instructions, Tools: in.Tools,
		Memory: in.Memory, MaxTurns: in.MaxTurns, Images: in.Images, EmbedModel: in.EmbedModel}
	rel, undo := "", func() {}
	if in.Instructions != nil {
		rel = filepath.ToSlash(filepath.Join(profilesDir, name+".md"))
		p.Instructions = filepath.Join(filepath.Dir(c.settings.path), profilesDir, name+".md")
		var err error
		if undo, err = replaceFile(p.Instructions, []byte(*in.Instructions)); err != nil {
			fail(w, http.StatusInternalServerError, "SETTINGS_INSTRUCTIONS", describe(err))
			return
		}
	}
	if err := c.changeLocked(func(next *config) {
		if next.Profiles == nil {
			next.Profiles = map[string]profile{}
		}
		next.Profiles[name] = p
	}, func(ed *edit) {
		at := "profiles." + name
		ed.set(at, map[string]interface{}{})
		ed.set(at+".description", p.Description)
		ed.set(at+".models", listOrNil(p.Models))
		if rel != "" {
			ed.set(at+".instructions", rel)
		} else {
			ed.set(at+".instructions", p.Instructions)
		}
		if p.Tools != nil {
			ed.set(at+".tools", *p.Tools)
		}
		ed.set(at+".memory", p.Memory)
		ed.set(at+".max_turns", zeroIntAsNone(p.MaxTurns))
		ed.set(at+".images", trueOrNone(p.Images))
		ed.set(at+".embed_model", p.EmbedModel)
	}); err != nil {
		undo()
		fail(w, http.StatusBadRequest, "SETTINGS_INPUT", describe(err))
		return
	}
	c.settings.signal()
	w.WriteHeader(http.StatusNoContent)
}

// deleteProfile removes an agent no trigger and no chat uses, and the
// instructions file the app made for it.
func (c *cli) deleteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !profileName.MatchString(name) || name == "default" {
		fail(w, http.StatusNotFound, "SETTINGS_NOT_FOUND", "no agent "+name)
		return
	}
	if users := c.profileUsers(name); len(users) > 0 {
		fail(w, http.StatusConflict, "SETTINGS_IN_USE", "agent "+name+" is used by "+joinNames(users)+": change them first")
		return
	}
	own := ""
	if c.settings != nil && c.settings.path != "" {
		own = filepath.Join(filepath.Dir(c.settings.path), profilesDir, name+".md")
	}
	removed := false
	c.change(statusOf(w, &removed), func(next *config) *refusal {
		if _, ok := next.Profiles[name]; !ok {
			return notFound("no agent " + name)
		}
		delete(next.Profiles, name)
		return nil
	}, func(ed *edit) { ed.set("profiles."+name, nil) })
	if removed && own != "" {
		go c.removeWhenUnnamed(name, own)
	}
}

// removeWhenUnnamed removes the instructions file the app made for an
// agent once the revision without it is in force: the one in force still
// follows the file, and would be refused without it. It looks at the file
// on disk too, under keepMu as the agents' writes: an agent of the same
// name made again meanwhile keeps its file (audit 27/11).
func (c *cli) removeWhenUnnamed(name, path string) {
	for range 500 {
		if _, named := c.conf().Profiles[name]; !named {
			c.removeIfUnnamedOnDisk(name, path)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *cli) removeIfUnnamedOnDisk(name, path string) {
	c.keepMu.Lock()
	defer c.keepMu.Unlock()
	disk, err := c.settings.onDisk(c)
	if err != nil {
		return // left: a file too many is harmless, one too few stops the watcher
	}
	if _, named := disk.Profiles[name]; !named {
		_ = os.Remove(path) // #nosec G703 -- the app's own file for the agent, its name checked by profileName
	}
}

// profileUsers are the triggers, workflows and chat settings that name
// an agent.
func (c *cli) profileUsers(name string) []string {
	var users []string
	f := c.conf()
	if f.Serve.MemberProfile == name {
		users = append(users, "serve.member_profile")
	}
	if f.Serve.PublicProfile == name {
		users = append(users, "serve.public_profile")
	}
	if c.serveCtx != nil && a2aProfile(c.serveCtx) == name {
		users = append(users, "serve --profile")
	}
	users = append(append(users, c.triggersOf(func(t trigger.Trigger) bool { return t.Profile == name })...), c.workflowsWith(name)...)
	return append(users, c.cardsFor(name)...)
}

// cardsFor names the projects' cards given to an agent.
func (c *cli) cardsFor(agent string) []string {
	s, err := c.projectStore()
	if err != nil {
		return []string{"the projects (" + describe(err) + ")"}
	}
	defer closeQuietly(s)
	list, err := s.List()
	if err != nil {
		return []string{"the projects, one of which cannot be read (" + describe(err) + ")"}
	}
	var out []string
	for _, p := range list {
		cards, err := s.Cards(p.ID)
		if err != nil {
			out = append(out, "project "+p.ID+" (its board cannot be read)")
		}
		for _, card := range cards {
			if card.Agent == agent {
				out = append(out, "card "+card.ID+" of project "+p.ID)
			}
		}
	}
	return out
}

// triggersOf names the triggers that match.
func (c *cli) triggersOf(match func(trigger.Trigger) bool) []string {
	store, err := c.triggerStore()
	if err != nil {
		return nil
	}
	defer closeQuietly(store)
	list, _ := store.Triggers()
	var out []string
	for _, t := range list {
		if match(t) {
			out = append(out, "trigger "+t.Name)
		}
	}
	return out
}

// workflowsWith names the saved workflows with a step on worker.
func (c *cli) workflowsWith(worker string) []string {
	lib, err := c.savedWorkflows()
	if err != nil {
		return nil
	}
	defer closeQuietly(lib)
	list, _ := lib.List()
	var out []string
	for _, s := range list {
		if slices.ContainsFunc(s.Plan.Steps, func(st workflow.Step) bool { return st.Worker == worker }) {
			out = append(out, "workflow "+s.Name)
		}
	}
	return out
}

// unknownTools says which of names an agent cannot list: tools there are
// not, or tools that reach the user's own artifacts; "" when all are
// fine.
func (c *cli) unknownTools(octx *orpheus.Context, list []string) string {
	r := c.forRun()
	defer r.shutdown()
	reg, err := r.tools(octx, nil, nil)
	if err != nil {
		return "the tools cannot be listed: " + describe(err)
	}
	for _, n := range list {
		switch {
		case ownersOnly[n]:
			return "an agent that lists its tools cannot have " + n + ": it reaches the user's own artifacts"
		case !reg.Has(n):
			return "no tool " + n
		}
	}
	return ""
}

// replaceFile writes data to path, private, and returns how to put back
// what was there before.
func replaceFile(path string, data []byte) (undo func(), err error) {
	old, rerr := readPrivate(path)
	if err := writePrivateFile(path, data); err != nil {
		return nil, err
	}
	return func() {
		if rerr == nil {
			_ = writePrivateFile(path, old)
		} else {
			_ = os.Remove(path) // #nosec G703 -- the file just written, by a path of the app's own
		}
	}, nil
}

// statusOf wraps w to note whether the change succeeded (204).
func statusOf(w http.ResponseWriter, ok *bool) http.ResponseWriter {
	return &statusWriter{ResponseWriter: w, ok: ok}
}

type statusWriter struct {
	http.ResponseWriter
	ok *bool
}

func (s *statusWriter) WriteHeader(code int) {
	*s.ok = code == http.StatusNoContent
	s.ResponseWriter.WriteHeader(code)
}

func listOrNil(list []string) interface{} {
	if len(list) == 0 {
		return nil
	}
	return list
}

func zeroIntAsNone(v int) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func trueOrNone(v bool) interface{} {
	if !v {
		return nil
	}
	return true
}

func joinNames(list []string) string {
	slices.Sort(list)
	return strings.Join(list, ", ")
}
