// appprojects_test.go: projects through the app's API and in the runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/project"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func timelineOf(t *testing.T, call appCall, id string) []project.Event {
	t.Helper()
	var out TimelineOut
	if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/projects/"+id+"/timeline?limit=1000", "", http.StatusOK)), &out); err != nil {
		t.Fatal(err)
	}
	return out.Events
}

func hasKind(evs []project.Event, kind project.EventKind) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// newProject makes a project from the app and returns it.
func newProject(t *testing.T, call appCall, body string) project.Project {
	t.Helper()
	var p project.Project
	if err := json.Unmarshal([]byte(must(t, call, "POST", "/v1/projects", body, http.StatusCreated)), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A project's board and timeline are changed and read from the app.
func TestAppProjects(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		p := newProject(t, call, `{"name":"Q4 launch","brief":"Ship it"}`)
		var card project.Card
		if err := json.Unmarshal([]byte(must(t, call, "POST", "/v1/projects/"+p.ID+"/cards", `{"title":"numbers","column":"Review"}`, http.StatusCreated)), &card); err != nil {
			t.Fatal(err)
		}
		must(t, call, "PATCH", "/v1/projects/"+p.ID+"/cards/"+card.ID, `{"column":"Done"}`, http.StatusOK)
		must(t, call, "POST", "/v1/projects/"+p.ID+"/cards", `{"title":"x","agent":"nobody"}`, http.StatusBadRequest)
		must(t, call, "POST", "/v1/projects/"+p.ID+"/timeline", `{"kind":"decision","text":"go in December"}`, http.StatusCreated)
		var got ProjectOut
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/projects/"+p.ID, "", http.StatusOK)), &got); err != nil || len(got.Cards) != 1 || got.Cards[0].Column != "Done" {
			t.Fatalf("%v %+v", err, got)
		}
		if evs := timelineOf(t, call, p.ID); len(evs) != 4 || evs[3].Kind != project.Decision {
			t.Fatalf("%+v", evs)
		}
	})
}

// A project's notes are written and read from the app; it is archived,
// then removed.
func TestAppProjectNotes(t *testing.T) {
	appAPI(t, "models: [openai:m]\n", nil, func(_ *harness, _ *serve.Server, call appCall) {
		p := newProject(t, call, `{"name":"notes"}`)
		must(t, call, "PUT", "/v1/projects/"+p.ID+"/notes/plan/dates.md", `{"text":"Dec 1"}`, http.StatusNoContent)
		if out := must(t, call, "GET", "/v1/projects/"+p.ID+"/notes/plan/dates.md", "", http.StatusOK); !strings.Contains(out, "Dec 1") {
			t.Fatalf("%s", out)
		}
		must(t, call, "GET", "/v1/projects/"+p.ID+"/notes/..%2Fx.md", "", http.StatusBadRequest)
		must(t, call, "PATCH", "/v1/projects/"+p.ID, `{"archived":true}`, http.StatusOK)
		if out := must(t, call, "GET", "/v1/projects", "", http.StatusOK); out != "[]\n" {
			t.Fatalf("an archived project listed: %s", out)
		}
		must(t, call, "DELETE", "/v1/projects/"+p.ID, "", http.StatusNoContent)
		must(t, call, "GET", "/v1/projects/"+p.ID, "", http.StatusNotFound)
	})
}

// endedRuns counts the runs the timeline saw end.
func endedRuns(t *testing.T, call appCall, id string) int {
	n := 0
	for _, e := range timelineOf(t, call, id) {
		if e.Kind == project.RunEnded {
			n++
		}
	}
	return n
}

// A run on a project is told its brief and board, has its tools, and is
// recorded on its timeline; a card the model hands to an agent in To do
// starts that agent's run on it.
func TestRunOnAProjectHandsOn(t *testing.T) {
	steps := []fake.Step{
		fake.Call("project_card", `{"action":"add","title":"write the draft","agent":"default"}`),
		fake.Reply("handed on"),
		fake.Reply("draft written"),
	}
	appAPI(t, "models: [openai:m]\n", steps, func(h *harness, srv *serve.Server, call appCall) {
		p := newProject(t, call, `{"name":"report","brief":"The monthly report"}`)
		snap, err := srv.StartFrom(serve.Request{Profile: "default", Conversation: "plan-1", Project: p.ID, Input: agent.UserText("plan it")})
		if err != nil {
			t.Fatal(err)
		}
		if end, err := srv.Wait(t.Context(), snap.ID); err != nil || end.Text != "handed on" {
			t.Fatalf("%v %+v", err, end)
		}
		first := h.script.Requests()[0]
		if !strings.Contains(contextOf(first), "The monthly report") || !hasTool(first, "project_board") {
			t.Fatal("the run was not told the project, or has not its tools")
		}
		waitFor(t, func() bool { return endedRuns(t, call, p.ID) == 2 }) // the planner's run and the one it handed the card to
		if evs := timelineOf(t, call, p.ID); !hasKind(evs, project.CardCreated) || !hasKind(evs, project.RunStarted) {
			t.Fatalf("%+v", evs)
		}
		if n := strings.Count(must(t, call, "GET", "/v1/conversations?project="+p.ID, "", http.StatusOK), `"id":`); n != 2 {
			t.Fatalf("%d conversations on the project", n)
		}
	})
}

// contextOf is the host's context blocks of a request's last user message.
func contextOf(req *agent.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			if part.Type == agent.PartContext {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

func hasTool(req *agent.Request, name string) bool {
	for _, s := range req.Tools {
		if s.Name == name {
			return true
		}
	}
	return false
}

// Hand-offs are bounded per card, per project and along a chain.
func TestHandOffsAreBounded(t *testing.T) {
	for i := range maxHandOffs {
		if !takeHandOff("pb", "cbounded1") {
			t.Fatalf("refused at %d", i)
		}
	}
	if takeHandOff("pb", "cbounded1") {
		t.Fatal("an eleventh hand-off of a card in an hour was taken")
	}
	for i := range maxProjectHandOffs - maxHandOffs {
		if !takeHandOff("pb", "cfresh"+strconv.Itoa(i)) {
			t.Fatalf("a fresh card refused at %d", i)
		}
	}
	if takeHandOff("pb", "cfreshlast") {
		t.Fatal("fresh cards went past the project's bound")
	}
	if !takeHandOff("pother", "cother1") {
		t.Fatal("another project was held too")
	}
}

// A run's depth in a chain of hand-offs is read off its source.
func TestHandOffDepth(t *testing.T) {
	if handDepthOf("project:pb:3") != 3 || handDepthOf("trigger:x") != 0 || handDepthOf("project:pb:x") != maxHandOffDepth {
		t.Fatal("depth of a source")
	}
}

// A card call that may hand on is asked; one that only records is not.
func TestHandOnIsAsked(t *testing.T) {
	for in, want := range map[string]bool{
		`{"action":"add","title":"x","agent":"writer"}`:   true,
		`{"action":"add","title":"x"}`:                    false,
		`{"action":"change","card":"c1","column":"Done"}`: true,
		`{"action":"change","card":"c1","title":"y"}`:     false,
		`{"action":"remove","card":"c1"}`:                 false,
		`not json`:                                        true,
	} {
		if got := mayHandOn(agent.ToolCall{Name: project.CardTool, Input: []byte(in)}); got != want {
			t.Errorf("%s: %v", in, got)
		}
	}
}

// An archived project takes no card and no run; a project that is not
// there takes no run; an agent of the chats takes no card.
func TestProjectsRefuse(t *testing.T) {
	appAPI(t, "models: [openai:m]\nprofiles:\n  guest: {tools: [], memory: chat}\n", []fake.Step{fake.Reply("x")}, func(_ *harness, srv *serve.Server, call appCall) {
		p := newProject(t, call, `{"name":"old"}`)
		must(t, call, "POST", "/v1/projects/"+p.ID+"/cards", `{"title":"x","agent":"guest"}`, http.StatusBadRequest)
		must(t, call, "PATCH", "/v1/projects/"+p.ID, `{"archived":true}`, http.StatusOK)
		must(t, call, "POST", "/v1/projects/"+p.ID+"/cards", `{"title":"x"}`, http.StatusBadRequest)
		for _, id := range []string{p.ID, "not-there"} { // refused at once, as a bad request
			must(t, call, "POST", "/v1/runs", `{"profile":"default","project":"`+id+`","input":"x"}`, http.StatusBadRequest)
			if _, err := srv.StartFrom(serve.Request{Profile: "default", Project: id, Input: agent.UserText("x")}); err == nil {
				t.Fatalf("a run on %s started", id)
			}
		}
		must(t, call, "PATCH", "/v1/conversations/c1", `{"project":"not-there"}`, http.StatusBadRequest)
	})
}

// An agent makes a project of the conversation: the conversation moves
// into it, a first card given to an agent starts it; the tool is not in
// a run already on a project.
func TestStartProjectFromAChat(t *testing.T) {
	steps := []fake.Step{
		fake.Call("start_project", `{"name":"Q1 plan","brief":"Plan Q1","cards":[{"title":"numbers","agent":"default"}]}`),
		fake.Reply("made it"),
		fake.Reply("numbers done"),
	}
	appAPI(t, "models: [openai:m]\n", steps, func(h *harness, srv *serve.Server, call appCall) {
		snap, err := srv.StartFrom(serve.Request{Profile: "default", Conversation: "chat-1", Input: agent.UserText("this is getting big")})
		if err != nil {
			t.Fatal(err)
		}
		if end, err := srv.Wait(t.Context(), snap.ID); err != nil || end.Text != "made it" {
			t.Fatalf("%v %+v", err, end)
		}
		var list []project.Project
		if err := json.Unmarshal([]byte(must(t, call, "GET", "/v1/projects", "", http.StatusOK)), &list); err != nil || len(list) != 1 {
			t.Fatalf("%v %+v", err, list)
		}
		id := list[0].ID
		waitFor(t, func() bool { return endedRuns(t, call, id) == 1 }) // the card's run
		if out := must(t, call, "GET", "/v1/conversations?project="+id, "", http.StatusOK); !strings.Contains(out, `"id":"chat-1"`) {
			t.Fatalf("the conversation was not moved: %s", out)
		}
		if !hasTool(h.script.Requests()[0], "start_project") || hasTool(h.script.Requests()[2], "start_project") {
			t.Fatal("start_project offered where it should not, or not where it should")
		}
	})
}

// A key in another case cannot hide a hand-off from the approval.
func TestHandOnInAnyCase(t *testing.T) {
	for _, in := range []string{
		`{"action":"add","title":"t","agent":"worker","AGENT":""}`,
		`{"action":"add","title":"t","agent":"worker","Agent":null}`,
		`{"action":"change","card":"c1","Agent":"worker"}`,
		`{"action":"change","card":"c1","COLUMN":"To do"}`,
		`{"Action":"change","action":"add","card":"c1","column":"To do"}`,
	} {
		if !mayHandOn(agent.ToolCall{Name: project.CardTool, Input: []byte(in)}) {
			t.Errorf("%s passed unasked", in)
		}
	}
}

// A hand-off's conversation ID stays within the session ID limit even
// for 64-character agent names.
func TestHandOffConversationLength(t *testing.T) {
	if got := handOffConversation("c1234567890", "writer"); got != "p-c1234567890-writer" {
		t.Fatalf("short: %q", got)
	}
	long := strings.Repeat("a", 64)
	got := handOffConversation("c1234567890", long)
	if len(got) > 64 || agent.ValidateSessionID(got) != nil {
		t.Fatalf("long invalid (%d): %q", len(got), got)
	}
	if got != handOffConversation("c1234567890", long) {
		t.Fatal("handOffConversation not deterministic")
	}
}
