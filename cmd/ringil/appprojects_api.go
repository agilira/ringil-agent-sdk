// appprojects_api.go: the projects' API (routes in appprojects.go)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"net/http"
	"strconv"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/project"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func (c *cli) mountProjects(srv *serve.Server) {
	routes := map[string]http.HandlerFunc{
		"GET /v1/projects":                         c.listProjects,
		"POST /v1/projects":                        c.createProject,
		"GET /v1/projects/{id}":                    c.getProject,
		"PATCH /v1/projects/{id}":                  c.patchProject,
		"DELETE /v1/projects/{id}":                 c.deleteProject,
		"POST /v1/projects/{id}/cards":             c.addCard,
		"PATCH /v1/projects/{id}/cards/{card}":     c.changeCard,
		"DELETE /v1/projects/{id}/cards/{card}":    c.removeCard,
		"GET /v1/projects/{id}/timeline":           c.getTimeline,
		"POST /v1/projects/{id}/timeline":          c.postTimeline,
		"GET /v1/projects/{id}/notes":              c.listNotes,
		"GET /v1/projects/{id}/notes/{path...}":    c.getNote,
		"PUT /v1/projects/{id}/notes/{path...}":    c.putNote,
		"DELETE /v1/projects/{id}/notes/{path...}": c.deleteNote,
	}
	for pattern, h := range routes {
		srv.Mount(pattern, h)
	}
}

// withProjects runs fn on the store, or says why it cannot be opened.
func (c *cli) withProjects(w http.ResponseWriter, fn func(*project.Store)) {
	s, err := c.projectStore()
	if err != nil {
		projectFail(w, err)
		return
	}
	defer closeQuietly(s)
	fn(s)
}

// projectFail answers a project package error with its status.
func projectFail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.HasCode(err, project.ErrNotFound):
		code = http.StatusNotFound
	case errors.HasCode(err, project.ErrInput):
		code = http.StatusBadRequest
	case errors.HasCode(err, project.ErrExists):
		code = http.StatusConflict
	}
	fail(w, code, "PROJECT", describe(err))
}

func (c *cli) listProjects(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("archived") == "true"
	c.withProjects(w, func(s *project.Store) {
		list, err := s.List()
		if err != nil {
			projectFail(w, err)
			return
		}
		out := []project.Project{}
		for _, p := range list {
			if all || !p.Archived {
				out = append(out, p)
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
}

type projectIn struct {
	Name     *string           `json:"name,omitempty"`
	Brief    *string           `json:"brief,omitempty"`
	Columns  *[]project.Column `json:"columns,omitempty"`
	Folder   *string           `json:"folder,omitempty"`
	Archived *bool             `json:"archived,omitempty"`
}

func (in projectIn) apply(p *project.Project) {
	if in.Name != nil {
		p.Name = *in.Name
	}
	if in.Brief != nil {
		p.Brief = *in.Brief
	}
	if in.Columns != nil {
		p.Columns = *in.Columns
	}
	if in.Folder != nil {
		p.Folder = *in.Folder
	}
	if in.Archived != nil {
		p.Archived = *in.Archived
	}
}

func (c *cli) createProject(w http.ResponseWriter, r *http.Request) {
	var in projectIn
	if !decodeJSON(w, r, &in) {
		return
	}
	var p project.Project
	in.apply(&p)
	c.withProjects(w, func(s *project.Store) {
		made, err := s.Create(p, "user")
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, made)
	})
}

// ProjectOut is a project with its board.
type ProjectOut struct {
	project.Project
	Cards []project.Card `json:"cards"`
}

func (c *cli) getProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c.withProjects(w, func(s *project.Store) {
		p, err := s.Get(id)
		if err != nil {
			projectFail(w, err)
			return
		}
		cards, err := s.Cards(id)
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ProjectOut{Project: p, Cards: append([]project.Card{}, cards...)})
	})
}

func (c *cli) patchProject(w http.ResponseWriter, r *http.Request) {
	var in projectIn
	if !decodeJSON(w, r, &in) {
		return
	}
	c.withProjects(w, func(s *project.Store) {
		p, err := s.Update(r.PathValue("id"), "user", func(p *project.Project) error { in.apply(p); return nil })
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
}

func (c *cli) deleteProject(w http.ResponseWriter, r *http.Request) {
	c.withProjects(w, func(s *project.Store) {
		if err := s.Remove(r.PathValue("id")); err != nil {
			projectFail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

type cardIn struct {
	Title  *string `json:"title,omitempty"`
	Text   *string `json:"text,omitempty"`
	Column *string `json:"column,omitempty"`
	Index  *int    `json:"index,omitempty"`
	Agent  *string `json:"agent,omitempty"`
	Person *string `json:"person,omitempty"`
}

// checkAgent: a card's agent is one serve runs, or none.
func (c *cli) checkAgent(w http.ResponseWriter, name *string) bool {
	if name == nil || *name == "" || *name == "default" {
		return true
	}
	p, ok := c.conf().Profiles[*name]
	switch {
	case !ok:
		fail(w, http.StatusBadRequest, "PROJECT", "no agent "+*name)
		return false
	case p.Tools != nil:
		fail(w, http.StatusBadRequest, "PROJECT", "agent "+*name+" is for the chats' members or the public: it cannot work on projects")
		return false
	}
	return true
}

func (c *cli) addCard(w http.ResponseWriter, r *http.Request) {
	var in cardIn
	if !decodeJSONLimit(w, r, &in, 128<<10) || !c.checkAgent(w, in.Agent) {
		return
	}
	id := r.PathValue("id")
	card := project.Card{Title: deref(in.Title), Text: deref(in.Text), Column: deref(in.Column), Agent: deref(in.Agent), Person: deref(in.Person)}
	c.withProjects(w, func(s *project.Store) {
		made, ev, err := s.AddCard(id, "user", card)
		if err != nil {
			projectFail(w, err)
			return
		}
		c.handedOn(s, id, ev)
		writeJSON(w, http.StatusCreated, made)
	})
}

func (c *cli) changeCard(w http.ResponseWriter, r *http.Request) {
	var in cardIn
	if !decodeJSONLimit(w, r, &in, 128<<10) || !c.checkAgent(w, in.Agent) {
		return
	}
	id := r.PathValue("id")
	ch := project.Change{Title: in.Title, Text: in.Text, Column: in.Column, Index: in.Index, Agent: in.Agent, Person: in.Person}
	c.withProjects(w, func(s *project.Store) {
		card, evs, err := s.ChangeCard(id, r.PathValue("card"), "user", ch)
		if err != nil {
			projectFail(w, err)
			return
		}
		for _, ev := range evs {
			c.handedOn(s, id, ev)
		}
		writeJSON(w, http.StatusOK, card)
	})
}

// handedOn starts the run of a card the user handed to an agent.
func (c *cli) handedOn(s *project.Store, id string, ev project.Event) {
	if p, err := s.Get(id); err == nil {
		c.handOff(p, ev)
	}
}

func (c *cli) removeCard(w http.ResponseWriter, r *http.Request) {
	c.withProjects(w, func(s *project.Store) {
		if err := s.RemoveCard(r.PathValue("id"), r.PathValue("card"), "user"); err != nil {
			projectFail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// TimelineOut is a page of the timeline and the last Seq there is.
type TimelineOut struct {
	Events []project.Event `json:"events"`
	Last   int64           `json:"last"`
}

func (c *cli) getTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := strconv.ParseInt(or(q.Get("from"), "0"), 10, 64)
	limit, err2 := strconv.Atoi(or(q.Get("limit"), "100"))
	if err1 != nil || err2 != nil || from < 0 || limit < 1 || limit > 1000 {
		fail(w, http.StatusBadRequest, "PROJECT", "from is an event's number (0: the latest), limit 1 to 1000")
		return
	}
	c.withProjects(w, func(s *project.Store) {
		evs, last, err := s.Timeline(r.PathValue("id"), from, limit)
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, TimelineOut{Events: evs, Last: last})
	})
}

type timelineIn struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Card string `json:"card,omitempty"`
}

func (c *cli) postTimeline(w http.ResponseWriter, r *http.Request) {
	var in timelineIn
	if !decodeJSON(w, r, &in) {
		return
	}
	kind := map[string]project.EventKind{"comment": project.Comment, "decision": project.Decision}[in.Kind]
	if kind == "" {
		fail(w, http.StatusBadRequest, "PROJECT", "kind is comment or decision")
		return
	}
	c.withProjects(w, func(s *project.Store) {
		ev, err := s.Log(r.PathValue("id"), project.Event{Actor: "user", Kind: kind, Text: in.Text, Card: in.Card})
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, ev)
	})
}

func (c *cli) listNotes(w http.ResponseWriter, r *http.Request) {
	c.withProjects(w, func(s *project.Store) {
		list, err := s.Notes(r.PathValue("id"))
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nonNil(list))
	})
}

func (c *cli) getNote(w http.ResponseWriter, r *http.Request) {
	c.withProjects(w, func(s *project.Store) {
		text, err := s.Note(r.PathValue("id"), r.PathValue("path"))
		if err != nil {
			projectFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"text": text})
	})
}

type noteIn struct {
	Text string `json:"text"`
}

func (c *cli) putNote(w http.ResponseWriter, r *http.Request) {
	var in noteIn
	if !decodeJSONLimit(w, r, &in, 2<<20) {
		return
	}
	c.withProjects(w, func(s *project.Store) {
		if err := s.WriteNote(r.PathValue("id"), r.PathValue("path"), in.Text, "user"); err != nil {
			projectFail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (c *cli) deleteNote(w http.ResponseWriter, r *http.Request) {
	c.withProjects(w, func(s *project.Store) {
		if err := s.RemoveNote(r.PathValue("id"), r.PathValue("path"), "user"); err != nil {
			projectFail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
