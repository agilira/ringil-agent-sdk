// cards.go: the board's cards
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	"crypto/rand"
	"slices"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// Card is a piece of work on the board.
type Card struct {
	// ID is made by the store: "c" and lowercase letters and digits.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Text is what the work is: the task, what done means, links.
	Text   string `json:"text,omitempty"`
	Column string `json:"column"`
	// Agent is the agent the card is for (the host's name for it), Person
	// the person; either, both or none.
	Agent  string `json:"agent,omitempty"`
	Person string `json:"person,omitempty"`
	// Conversations are those where the card was worked on, the latest
	// last (at most 32).
	Conversations []string  `json:"conversations,omitempty"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
}

const maxConversations = 32

// Change is what changes of a card: a nil field stays as it is.
type Change struct {
	Title  *string `json:"title,omitempty"`
	Text   *string `json:"text,omitempty"`
	Agent  *string `json:"agent,omitempty"`
	Person *string `json:"person,omitempty"`
	// Column and Index move it: to that column (its own when empty), at
	// that position among its cards (-1 or past the end: last).
	Column *string `json:"column,omitempty"`
	Index  *int    `json:"index,omitempty"`
	// Conversation adds a conversation the card was worked on.
	Conversation string `json:"conversation,omitempty"`
}

func (c Card) check(p Project) error {
	switch {
	case !validCard.MatchString(c.ID):
		return errors.New(ErrInput, "not a card's id: "+c.ID)
	case !text(c.Title, maxTitle) || strings.TrimSpace(c.Title) == "":
		return errors.New(ErrInput, "a card's title is 1-200 bytes of text")
	case !text(c.Text, maxText):
		return errors.New(ErrInput, "a card's text is at most 16 KiB")
	case !validWho.MatchString(c.Agent) || !validWho.MatchString(c.Person):
		return errors.New(ErrInput, "an agent's or a person's name is at most 64 letters, digits, spaces, _ . @ -")
	}
	if _, ok := p.column(c.Column); !ok {
		return errors.New(ErrInput, "no column "+c.Column+" on the board of "+p.ID)
	}
	return nil
}

// Cards are the board's cards, column by column in the board's order,
// each column's in its own order.
func (s *Store) Cards(id string) ([]Card, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	cards, err := s.cards(id)
	if err != nil {
		return nil, err
	}
	return byColumn(p, cards), nil
}

// cards are the cards as the file holds them; none when there is no file.
func (s *Store) cards(id string) ([]Card, error) {
	var cards []Card
	err := s.read(id, cardsFile, &cards)
	if errors.HasCode(err, ErrNotFound) {
		if _, gerr := s.Get(id); gerr != nil {
			return nil, gerr
		}
		return nil, nil
	}
	return cards, err
}

// byColumn orders cards by the board's columns, keeping each column's
// order.
func byColumn(p Project, cards []Card) []Card {
	out := make([]Card, 0, len(cards))
	for _, col := range p.Columns {
		for _, c := range cards {
			if strings.EqualFold(c.Column, col.Name) {
				out = append(out, c)
			}
		}
	}
	for _, c := range cards { // a column not on the board (a hand edit): last, never hidden
		if _, ok := p.column(c.Column); !ok {
			out = append(out, c)
		}
	}
	return out
}

// Card is one card.
func (s *Store) Card(id, card string) (Card, error) {
	cards, err := s.cards(id)
	if err != nil {
		return Card{}, err
	}
	for _, c := range cards {
		if c.ID == card {
			return c, nil
		}
	}
	return Card{}, errors.New(ErrNotFound, "no card "+card+" in "+id)
}

// AddCard puts a new card at the end of its column (the board's first
// when none), made by actor.
func (s *Store) AddCard(id, actor string, c Card) (Card, Event, error) {
	var ev Event
	err := s.locked(id, func() error {
		p, cards, err := s.board(id)
		if err != nil {
			return err
		}
		if err := writable(p); err != nil {
			return err
		}
		if len(cards) >= MaxCards {
			return errors.New(ErrInput, "a board holds at most 1000 cards")
		}
		if c.Column == "" {
			c.Column = p.Columns[0].Name
		}
		if col, ok := p.column(c.Column); ok {
			c.Column = col.Name // the board's spelling
		}
		c.ID, c.Created, c.Updated, c.Conversations = "c"+strings.ToLower(rand.Text()[:10]), s.now(), s.now(), nil
		if err := c.check(p); err != nil {
			return err
		}
		if err := s.write(id, cardsFile, append(cards, c)); err != nil {
			return err
		}
		ev, err = s.appendEvent(id, Event{Actor: actor, Kind: CardCreated, Card: c.ID, Text: c.Title, Column: c.Column, Agent: c.Agent,
			Start: p.handsOn(Card{}, c)})
		return err
	})
	return c, ev, err
}

// ChangeCard changes a card as ch says, by actor; a move is one event, a
// change of its words or people another.
func (s *Store) ChangeCard(id, card, actor string, ch Change) (Card, []Event, error) {
	var out Card
	var evs []Event
	err := s.locked(id, func() error {
		p, cards, err := s.board(id)
		if err != nil {
			return err
		}
		if err := writable(p); err != nil {
			return err
		}
		i := slices.IndexFunc(cards, func(c Card) bool { return c.ID == card })
		if i < 0 {
			return errors.New(ErrNotFound, "no card "+card+" in "+id)
		}
		was := cards[i]
		next := applyChange(was, ch, s.now())
		if col, ok := p.column(next.Column); ok {
			next.Column = col.Name
		}
		if err := next.check(p); err != nil {
			return err
		}
		cards = place(slices.Delete(cards, i, i+1), next, ch.Index)
		if err := s.write(id, cardsFile, cards); err != nil {
			return err
		}
		out = next
		evs, err = s.cardEvents(id, actor, was, next, p.handsOn(was, next))
		return err
	})
	return out, evs, err
}

// applyChange is c with ch made.
func applyChange(c Card, ch Change, now time.Time) Card {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.Title, ch.Title)
	set(&c.Text, ch.Text)
	set(&c.Agent, ch.Agent)
	set(&c.Person, ch.Person)
	set(&c.Column, ch.Column)
	if ch.Conversation != "" && !slices.Contains(c.Conversations, ch.Conversation) {
		c.Conversations = append(slices.Clone(c.Conversations), ch.Conversation)
		if len(c.Conversations) > maxConversations {
			c.Conversations = c.Conversations[len(c.Conversations)-maxConversations:]
		}
	}
	c.Updated = now
	return c
}

// place puts c among cards at index among those of its column (-1, nil
// or past the end: after the column's last).
func place(cards []Card, c Card, index *int) []Card {
	var at []int // positions of the column's cards in cards
	for i, x := range cards {
		if strings.EqualFold(x.Column, c.Column) {
			at = append(at, i)
		}
	}
	switch {
	case len(at) == 0:
		return append(cards, c)
	case index == nil || *index < 0 || *index >= len(at):
		return slices.Insert(cards, at[len(at)-1]+1, c)
	}
	return slices.Insert(cards, at[*index], c)
}

// cardEvents records what a change of a card did: a move, then a change
// of its words or people, each when it happened.
func (s *Store) cardEvents(id, actor string, was, now Card, start bool) ([]Event, error) {
	var evs []Event
	moved := !strings.EqualFold(was.Column, now.Column) // a column renamed in case is not a move
	if moved {
		ev, err := s.appendEvent(id, Event{Actor: actor, Kind: CardMoved, Card: now.ID, Text: was.Column + " → " + now.Column, Column: now.Column, Agent: now.Agent, Start: start})
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
	if what := cardChanges(was, now); what != "" {
		ev, err := s.appendEvent(id, Event{Actor: actor, Kind: CardChanged, Card: now.ID, Text: what, Column: now.Column, Agent: now.Agent, Start: start && !moved})
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

func cardChanges(a, b Card) string {
	var what []string
	if a.Title != b.Title {
		what = append(what, "title")
	}
	if a.Text != b.Text {
		what = append(what, "text")
	}
	if a.Agent != b.Agent {
		what = append(what, "agent: "+or(b.Agent, "none"))
	}
	if a.Person != b.Person {
		what = append(what, "person: "+or(b.Person, "none"))
	}
	return strings.Join(what, ", ")
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// RemoveCard takes a card off the board, by actor.
func (s *Store) RemoveCard(id, card, actor string) error {
	return s.locked(id, func() error {
		p, cards, err := s.board(id)
		if err != nil {
			return err
		}
		if err := writable(p); err != nil {
			return err
		}
		i := slices.IndexFunc(cards, func(c Card) bool { return c.ID == card })
		if i < 0 {
			return errors.New(ErrNotFound, "no card "+card+" in "+id)
		}
		gone := cards[i]
		if err := s.write(id, cardsFile, slices.Delete(cards, i, i+1)); err != nil {
			return err
		}
		_, err = s.appendEvent(id, Event{Actor: actor, Kind: CardRemoved, Card: card, Text: gone.Title})
		return err
	})
}

// board is the project and its cards, under its lock.
func (s *Store) board(id string) (Project, []Card, error) {
	p, err := s.Get(id)
	if err != nil {
		return Project{}, nil, err
	}
	cards, err := s.cards(id)
	return p, cards, err
}

// Starts says whether an event hands a card to its agent: the card, with
// an agent, entered an Auto column (made there, moved there, or given an
// agent there), once for a change however many events it makes. The host
// then starts that agent's run on it.
func (p Project) Starts(ev Event) bool { return ev.Start && ev.Agent != "" && ev.Card != "" }

// handsOn: the change from was to now hands the card to its agent: it
// has one, in an Auto column, and it was not there with that agent.
func (p Project) handsOn(was, now Card) bool {
	col, ok := p.column(now.Column)
	if !ok || !col.Auto || now.Agent == "" {
		return false
	}
	return !strings.EqualFold(was.Column, now.Column) || was.Agent != now.Agent
}
