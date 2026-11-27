// start.go: the agent proposes a project, the user says yes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	"context"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// StartToolName is the tool that makes a project of the work at hand.
const StartToolName = "start_project"

// StartOptions configure StartTool.
type StartOptions struct {
	// Actor is who makes it: "agent:<name>".
	Actor string
	// Started hears of the project made, and of the events of its first
	// cards: the host moves the conversation into it and hands the cards
	// on (Project.Starts).
	Started func(Project, []Event)
}

// maxFirstCards bounds the cards a project is made with.
const maxFirstCards = 20

type startIn struct {
	Name  string      `json:"name" desc:"The project's name"`
	Brief string      `json:"brief" desc:"What it is for and what done means, for every agent who will work on it"`
	Cards []firstCard `json:"cards,omitempty" desc:"The first pieces of work, at most 20, in order"`
}

type firstCard struct {
	Title string `json:"title"`
	Text  string `json:"text,omitempty" desc:"What the work is and what done means"`
	Agent string `json:"agent,omitempty" desc:"The agent it is for, by name: given in the first column, it starts at once"`
}

// StartTool lets the agent make a project of the work at hand, when it
// has grown into several steps, sessions or agents. It is destructive: an
// approval shows the whole project (name, brief, first cards) before it
// is made, so that nothing becomes a project without the user's yes.
func (s *Store) StartTool(opts StartOptions) agent.Tool {
	return agent.NewTool(StartToolName, "Make a project of the work at hand: a board, a timeline and notes that several agents and people share in turn. "+
		"Most tasks are not projects: propose one in words only when the work grows into several steps, sessions or agents, "+
		"and call this once the user agrees (they see it all before it is made). This conversation then belongs to it.",
		func(_ context.Context, in startIn) (string, error) {
			if err := validateStart(in); err != nil {
				return "", agent.ToolError(ErrInput, "Not made: "+agent.ModelMessage(err))
			}
			p, err := s.Create(Project{Name: in.Name, Brief: in.Brief}, opts.Actor)
			if err != nil {
				return "", agent.ToolError(ErrInput, "Not made: "+agent.ModelMessage(err))
			}
			var evs []Event
			for _, c := range in.Cards {
				_, ev, err := s.AddCard(p.ID, opts.Actor, Card{Title: c.Title, Text: c.Text, Agent: c.Agent})
				if err != nil {
					return "", agent.ToolError(ErrInput, "Project "+p.ID+" made, but a card was refused: "+agent.ModelMessage(err))
				}
				evs = append(evs, ev)
			}
			if opts.Started != nil {
				opts.Started(p, evs)
			}
			return "Project " + quote(p.Name) + " made (" + p.ID + ") with " + strconv.Itoa(len(evs)) + " cards; this conversation belongs to it from the next message. " +
				"The board: " + strings.Join(columnNames(p), " → ") + ".", nil
		}, agent.Destructive())
}

func validateStart(in startIn) error {
	if len(in.Cards) > maxFirstCards {
		return errors.New(ErrInput, "at most 20 first cards: the rest can come later")
	}
	p := Project{ID: "p0", Name: in.Name, Brief: in.Brief, Columns: DefaultColumns()}
	if err := p.Check(); err != nil {
		return err
	}
	for _, c := range in.Cards {
		card := Card{ID: "c0000000000", Title: c.Title, Text: c.Text, Column: p.Columns[0].Name, Agent: c.Agent}
		if err := card.check(p); err != nil {
			return err
		}
	}
	return nil
}

func columnNames(p Project) []string {
	out := make([]string, len(p.Columns))
	for i, c := range p.Columns {
		out[i] = c.Name
	}
	return out
}
