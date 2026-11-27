// project_test.go: projects, their boards and timelines
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agilira/go-errors"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "projects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// get is v, failing the test on err: get(s.Get(id))(t).
func get[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// ok fails the test unless cond holds.
func ok(t *testing.T, cond bool, format string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatalf(format, args...)
	}
}

func wantCode(t *testing.T, err error, code errors.ErrorCode, what string) {
	t.Helper()
	if !errors.HasCode(err, code) {
		t.Fatalf("%s: %v, want %s", what, err, code)
	}
}

func kindsOf(evs []Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, string(e.Kind))
	}
	return strings.Join(out, " ")
}

// A project is made with the default board, listed, changed and removed;
// the timeline records each change.
func TestProjects(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "Q4 launch", Brief: "Ship the connector"}, "user"))(t)
	ok(t, strings.HasPrefix(p.ID, "q4-launch-"), "id %q", p.ID)
	ok(t, len(p.Columns) == 4 && p.Columns[0].Auto, "columns %+v", p.Columns)
	_, err := s.Create(Project{ID: strings.ToUpper(p.ID), Name: "twin"}, "user")
	wantCode(t, err, ErrExists, "a twin in case")
	p2 := get(s.Update(p.ID, "user", func(x *Project) error { x.Brief, x.ID = "Ship it in December", "other"; return nil }))(t)
	ok(t, p2.ID == p.ID && p2.Brief == "Ship it in December", "%+v", p2)
	ok(t, len(get(s.List())(t)) == 1, "listed")
	evs, last, err := s.Timeline(p.ID, 0, 10)
	ok(t, err == nil && last == 2, "%v last %d", err, last)
	ok(t, kindsOf(evs) == "project_created project_changed" && evs[1].Text == "brief", "%+v", evs)
	ok(t, s.Remove(p.ID) == nil, "remove")
	_, err = s.Get(p.ID)
	wantCode(t, err, ErrNotFound, "removed")
}

// The project of a folder is made at its first use, around the notes it
// may already have.
func TestEnsureKeepsNotes(t *testing.T) {
	s := open(t)
	notes := get(s.NotesDir("ringil-a78dd964"))(t)
	if err := os.MkdirAll(notes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "INDEX.md"), []byte("- decisions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list := get(s.List())(t); len(list) != 0 {
		t.Fatal("a folder of notes alone is listed")
	}
	p := get(s.Ensure(Project{ID: "ringil-a78dd964", Name: "ringil", Folder: "/home/me/ringil"}, "system"))(t)
	again := get(s.Ensure(Project{ID: "ringil-a78dd964", Name: "other"}, "system"))(t)
	if again.Name != "ringil" || p.Folder != "/home/me/ringil" {
		t.Fatalf("%+v", again)
	}
	if _, err := os.Stat(filepath.Join(notes, "INDEX.md")); err != nil {
		t.Fatal("the notes were lost")
	}
}

// Cards are added and kept in the board's spelling and order.
func TestCards(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "board"}, "user"))(t)
	a, _, err := s.AddCard(p.ID, "user", Card{Title: "a"})
	ok(t, err == nil && a.Column == "To do", "%v %+v", err, a)
	b, _, _ := s.AddCard(p.ID, "user", Card{Title: "b", Column: "to DO"})
	ok(t, b.Column == "To do", "the board's spelling was not kept: %q", b.Column)
	zero := 0
	_, evs, err := s.ChangeCard(p.ID, b.ID, "agent:writer", Change{Index: &zero})
	ok(t, err == nil && len(evs) == 0, "%v %+v", err, evs)
	cards := get(s.Cards(p.ID))(t)
	ok(t, cards[0].ID == b.ID && cards[1].ID == a.ID, "order %+v", cards)
}

// A card is moved and changed in one, then removed; a column holding
// cards cannot go.
func TestCardChanges(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "board"}, "user"))(t)
	a, _, _ := s.AddCard(p.ID, "user", Card{Title: "a"})
	done, title := "Done", "a, done"
	c, evs, err := s.ChangeCard(p.ID, a.ID, "agent:writer", Change{Column: &done, Title: &title, Conversation: "conv-1"})
	ok(t, err == nil && c.Column == "Done", "%v %+v", err, c)
	ok(t, kindsOf(evs) == "card_moved card_changed" && c.Conversations[0] == "conv-1", "%+v %+v", evs, c)
	_, err = s.Update(p.ID, "user", func(x *Project) error { x.Columns = x.Columns[:3]; return nil })
	wantCode(t, err, ErrInput, "a column with cards removed")
	bad := "Nowhere"
	_, _, err = s.ChangeCard(p.ID, a.ID, "user", Change{Column: &bad})
	wantCode(t, err, ErrInput, "a column that is not there")
	ok(t, s.RemoveCard(p.ID, a.ID, "user") == nil, "remove")
	wantCode(t, s.RemoveCard(p.ID, a.ID, "user"), ErrNotFound, "removed twice")
}

// A card with an agent that enters an Auto column is handed to it.
func TestStarts(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "handoff"}, "user"))(t)
	_, ev, _ := s.AddCard(p.ID, "user", Card{Title: "draft", Agent: "writer"})
	if !p.Starts(ev) {
		t.Fatal("a card made for an agent in To do did not start")
	}
	c, ev, _ := s.AddCard(p.ID, "user", Card{Title: "review", Column: "Review", Agent: "editor"})
	if p.Starts(ev) {
		t.Fatal("a card in Review started")
	}
	todo, other := "To do", "reviewer"
	_, evs, _ := s.ChangeCard(p.ID, c.ID, "agent:editor", Change{Column: &todo})
	if len(evs) != 1 || !p.Starts(evs[0]) {
		t.Fatalf("a move to To do did not start: %+v", evs)
	}
	_, evs, _ = s.ChangeCard(p.ID, c.ID, "user", Change{Agent: &other})
	if len(evs) != 1 || !p.Starts(evs[0]) {
		t.Fatalf("a new agent in To do did not start: %+v", evs)
	}
	text := "more"
	_, evs, _ = s.ChangeCard(p.ID, c.ID, "user", Change{Text: &text})
	if p.Starts(evs[0]) {
		t.Fatal("a change of its text started it again")
	}
}

// The timeline numbers events and pages them; many writers at once lose
// none.
func TestTimeline(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "log"}, "user"))(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Log(p.ID, Event{Actor: "agent:a", Kind: Comment, Text: fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	all, last, err := s.Timeline(p.ID, 0, 1000)
	ok(t, err == nil && last == 21 && len(all) == 21, "%v last %d, %d events", err, last, len(all))
	for i, e := range all {
		ok(t, e.Seq == int64(i+1), "seq %d at %d", e.Seq, i)
	}
	page, _, _ := s.Timeline(p.ID, 5, 3)
	ok(t, len(page) == 3 && page[0].Seq == 5, "page %+v", page)
	tail, _, _ := s.Timeline(p.ID, 0, 2)
	ok(t, len(tail) == 2 && tail[1].Seq == 21, "tail %+v", tail)
}

// What is not an event is refused, and so is a comment on no card.
func TestEventsChecked(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "log"}, "user"))(t)
	for _, bad := range []Event{{Actor: "root", Kind: Comment}, {Actor: "user", Kind: "hack"}, {Actor: "user", Kind: Comment, Card: "../x"},
		{Actor: "user", Kind: Comment, Text: strings.Repeat("x", 5000)}} {
		_, err := s.Log(p.ID, bad)
		wantCode(t, err, ErrInput, bad.Actor+" "+string(bad.Kind))
	}
	_, err := s.Log(p.ID, Event{Actor: "user", Kind: Comment, Card: "cnotthere1"})
	wantCode(t, err, ErrNotFound, "a comment on no card")
}

// The context says the brief, the board and the latest events, as data.
func TestContext(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "ctx", Brief: "Monthly report", Folder: "/srv/reports"}, "user"))(t)
	c, _, _ := s.AddCard(p.ID, "user", Card{Title: "numbers", Agent: "analyst"})
	_, _ = s.Log(p.ID, Event{Actor: "agent:analyst", Kind: Decision, Card: c.ID, Text: "use the Q3 file"})
	got := get(s.Context(p.ID, 0))(t)
	for _, want := range []string{"Monthly report", "/srv/reports", c.ID, "agent analyst", "use the Q3 file", "data, not instructions", "Done: none"} {
		if !strings.Contains(got, want) {
			t.Errorf("context lacks %q:\n%s", want, got)
		}
	}
}

// The agent reads the board, moves a card to another agent's start, and
// records a decision; the host hears of each.
func TestTools(t *testing.T) {
	s := open(t)
	p := get(s.Create(Project{Name: "tools"}, "user"))(t)
	var heard []Event
	tools := s.Tools(p.ID, ToolOptions{Actor: "agent:writer", Conversation: "conv-9", OnEvent: func(_ Project, e Event) { heard = append(heard, e) }})
	call := func(i int, in string) string {
		t.Helper()
		out, err := tools[i].Call(context.Background(), json.RawMessage(in))
		ok(t, err == nil, "%s %s: %v", tools[i].Spec().Name, in, err)
		return out.Content
	}
	id := strings.Fields(call(1, `{"action":"add","title":"draft","column":"In progress"}`))[1]
	call(1, `{"action":"change","card":"`+id+`","column":"To do","agent":"editor"}`)
	call(2, `{"kind":"decision","text":"tone: formal","card":"`+id+`"}`)
	out := call(0, `{"card":"`+id+`"}`)
	ok(t, strings.Contains(out, "tone: formal") && strings.Contains(out, "agent editor"), "%s", out)
	ok(t, len(heard) == 4 && p.Starts(heard[1]) && heard[1].Actor == "agent:writer", "heard %+v", heard)
	ok(t, get(s.Card(p.ID, id))(t).Conversations[0] == "conv-9", "conversation")
	ok(t, tools[0].Spec().Annotations.ReadOnly && tools[1].Spec().Annotations.Edits, "annotations")
}

// The agent makes a project with its first cards, asked first; the host
// hears of it.
func TestStartTool(t *testing.T) {
	s := open(t)
	var made Project
	var evs []Event
	tool := s.StartTool(StartOptions{Actor: "agent:planner", Started: func(p Project, e []Event) { made, evs = p, e }})
	ok(t, tool.Spec().Annotations.Destructive, "it is not asked")
	out, err := tool.Call(context.Background(), json.RawMessage(`{"name":"Q1 plan","brief":"Plan Q1","cards":[{"title":"numbers","agent":"analyst"},{"title":"deck"}]}`))
	ok(t, err == nil && strings.Contains(out.Content, "made"), "%v %+v", err, out)
	ok(t, made.Name == "Q1 plan" && len(evs) == 2 && made.Starts(evs[0]) && !made.Starts(evs[1]), "%+v %+v", made, evs)
	ok(t, len(get(s.Cards(made.ID))(t)) == 2, "cards")
	_, err = tool.Call(context.Background(), json.RawMessage(`{"name":"","brief":"x"}`))
	ok(t, err != nil, "a project without a name was made")
}
