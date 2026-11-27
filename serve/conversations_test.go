// conversations_test.go: the stored conversations over the API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

func storedAPI(t *testing.T, steps ...fake.Step) *api {
	t.Helper()
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(steps...)})}, Store: store})
}

func (a *api) ran(conversation, input string) {
	a.t.Helper()
	body, _ := json.Marshal(map[string]string{"profile": "default", "conversation": conversation, "input": input})
	run := a.start(string(body))
	a.until(run.ID, func(s Snapshot) bool { return s.State != Running && s.State != Waiting })
}

func (a *api) json(method, path string, want int, v any) {
	a.t.Helper()
	code, out := a.do(method, path, "")
	if code != want {
		a.t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			a.t.Fatalf("%s %s: %v %s", method, path, err, out)
		}
	}
}

// A conversation is listed with the title of its first message.
func TestConversationsListed(t *testing.T) {
	a := storedAPI(t, fake.Reply("first answer"))
	a.ran("chat-1", "How do I   reset\nmy password?")
	var list []ConversationInfo
	a.json("GET", "/v1/conversations", http.StatusOK, &list)
	if len(list) != 1 || list[0].Title != "How do I reset my password?" || list[0].Running {
		t.Fatalf("%+v", list)
	}
}

// Its messages are read back in pages, the last by default.
func TestConversationPages(t *testing.T) {
	a := storedAPI(t, fake.Reply("first answer"), fake.Reply("second answer"))
	a.ran("chat-1", "one")
	a.ran("chat-1", "two")
	var h History
	a.json("GET", "/v1/conversations/chat-1/messages?limit=2", http.StatusOK, &h)
	if h.Total != 4 || h.From != 2 || len(h.Messages) != 2 || h.Messages[1].Text != "second answer" {
		t.Fatalf("last page %+v", h)
	}
	a.json("GET", "/v1/conversations/chat-1/messages?from=0&limit=1", http.StatusOK, &h)
	if h.Messages[0].Role != agent.RoleUser || h.Messages[0].Index != 0 {
		t.Fatalf("first page %+v", h)
	}
}

// It is renamed and deleted.
func TestConversationRenameDelete(t *testing.T) {
	a := storedAPI(t, fake.Reply("ok"))
	a.ran("chat-1", "hello")
	if code, out := a.do("PATCH", "/v1/conversations/chat-1", `{"title":"Password"}`); code != http.StatusNoContent {
		t.Fatalf("rename %d %s", code, out)
	}
	var list []ConversationInfo
	a.json("GET", "/v1/conversations", http.StatusOK, &list)
	if list[0].Title != "Password" {
		t.Fatalf("renamed %+v", list)
	}
	a.json("DELETE", "/v1/conversations/chat-1", http.StatusNoContent, nil)
	a.json("GET", "/v1/conversations", http.StatusOK, &list)
	if len(list) != 0 {
		t.Fatalf("after delete %+v", list)
	}
}

func TestConversationsRefusals(t *testing.T) {
	a := storedAPI(t)
	for _, c := range []struct {
		method, path, body string
		code               int
	}{
		{"GET", "/v1/conversations/nope/messages", "", http.StatusNotFound}, // never created by a read
		{"GET", "/v1/conversations/x/messages?from=-1", "", http.StatusBadRequest},
		{"GET", "/v1/conversations/x/messages?limit=0", "", http.StatusBadRequest},
		{"GET", "/v1/conversations/bad%20id/messages", "", http.StatusBadRequest},
		{"PATCH", "/v1/conversations/bad%20id", `{"title":"x"}`, http.StatusBadRequest},
		{"DELETE", "/v1/conversations/bad%20id", "", http.StatusBadRequest},
		{"PATCH", "/v1/conversations/nope", `{"title":"x"}`, http.StatusNotFound},
		{"PATCH", "/v1/conversations/x", `{}`, http.StatusBadRequest},
		{"PATCH", "/v1/conversations/x", `{"pinned":"yes"}`, http.StatusBadRequest},
		{"DELETE", "/v1/conversations/nope", "", http.StatusNotFound},
	} {
		if code, out := a.do(c.method, c.path, c.body); code != c.code {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, out)
		}
	}
}

// What was attached is named in the history, never sent again.
func TestHistoryNamesAttachments(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	m := historyMessage(3, agent.UserMessage("look", agent.Part{Type: agent.PartImage, MIME: "image/png", Data: png}))
	b, _ := json.Marshal(m)
	if len(m.Attachments) != 1 || m.Attachments[0].Bytes != len(png) || strings.Contains(string(b), "iVBOR") {
		t.Fatalf("%s", b)
	}
}

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"  hello \n\t world ":         "hello world",
		"a\x1b[31mb":                  "a[31mb",
		"bad\xfftitle":                "badtitle",
		"tab\there":                   "tab here",
		strings.Repeat("parola ", 40): strings.TrimSpace(strings.Repeat("parola ", 16)) + "…",
	} {
		if got := agent.CleanTitle(in); got != want || len(got) > agent.MaxTitle {
			t.Errorf("%q: %q (%d bytes)", in, got, len(got))
		}
	}
}

// Images and PDFs sent with the input reach the model as parts of the
// user's message; what is not one, or too many, is refused.
func TestRunAttachments(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	pdf := []byte("%PDF-1.4\nscan")
	model := fake.New(fake.Reply("seen"))
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: model})}, Store: store})
	body, _ := json.Marshal(map[string]any{"profile": "default", "conversation": "c1", "attachments": []map[string]any{
		{"mime": "image/png", "data": png}, {"mime": "application/pdf", "data": pdf}}})
	run := a.start(string(body))
	a.until(run.ID, func(s Snapshot) bool { return s.State != Running && s.State != Waiting })
	reqs := model.Requests()
	if len(reqs) != 1 || reqs[0].Messages[0].Images() != 2 {
		t.Fatalf("the model saw %d requests", len(reqs))
	}
	var list []ConversationInfo
	a.json("GET", "/v1/conversations", http.StatusOK, &list)
	if list[0].Title != "Image" {
		t.Fatalf("title %+v", list)
	}
	many := make([]map[string]any, maxAttachments+1)
	for i := range many {
		many[i] = map[string]any{"mime": "image/png", "data": png}
	}
	for name, atts := range map[string][]map[string]any{
		"fake png":  {{"mime": "image/png", "data": []byte("not a png")}},
		"text file": {{"mime": "text/plain", "data": []byte("hello")}},
		"fake pdf":  {{"mime": "application/pdf", "data": []byte("hello")}},
		"too many":  many,
	} {
		body, _ := json.Marshal(map[string]any{"profile": "default", "input": "x", "attachments": atts})
		if code, out := a.do("POST", "/v1/runs", string(body)); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
}

// conversations lists the conversations afresh: a field left out would
// keep the value of a list decoded before.
func (a *api) conversations() []ConversationInfo {
	a.t.Helper()
	var list []ConversationInfo
	a.json("GET", "/v1/conversations", http.StatusOK, &list)
	return list
}

func (a *api) pin(id string, on bool) {
	a.t.Helper()
	body := `{"pinned":false}`
	if on {
		body = `{"pinned":true}`
	}
	if code, out := a.do("PATCH", "/v1/conversations/"+id, body); code != http.StatusNoContent {
		a.t.Fatalf("pin %s %v: %d %s", id, on, code, out)
	}
}

// first is the list's first conversation, checked to be id with pinned.
func first(t *testing.T, list []ConversationInfo, id string, pinned bool) ConversationInfo {
	t.Helper()
	if len(list) == 0 || list[0].ID != id || list[0].Pinned != pinned {
		t.Fatalf("want %s first (pinned %v): %+v", id, pinned, list)
	}
	return list[0]
}

// A pinned conversation comes first, whatever its date, and keeps its
// title through a run; each names the agent that answered it last.
func TestConversationPinAndProfile(t *testing.T) {
	a := storedAPI(t, fake.Reply("a"), fake.Reply("b"), fake.Reply("c"))
	a.ran("old", "first")
	a.ran("new", "second")
	a.pin("old", true)
	list := a.conversations()
	if got := first(t, list, "old", true); got.Title != "first" || got.Profile != "default" {
		t.Fatalf("%+v", got)
	}
	if list[1].Pinned || list[1].Profile != "default" {
		t.Fatalf("%+v", list[1])
	}
	a.ran("old", "third") // a run does not unpin it nor retitle it
	if got := first(t, a.conversations(), "old", true); got.Title != "first" {
		t.Fatalf("after a run %+v", got)
	}
	a.pin("old", false)
	first(t, a.conversations(), "old", false) // the latest: the run just now
}

// A run on a project marks its conversation: the next runs of it keep
// the project, another is refused, and the list is filtered by it.
func TestConversationProject(t *testing.T) {
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	p := Profile{Name: "default", New: func(ctx context.Context) (Instance, error) {
		seen = append(seen, ProjectOf(ctx))
		a, err := agent.New(agent.Config{Provider: fake.New(fake.Reply("ok"))})
		return Instance{Agent: a}, err
	}}
	a := newAPI(t, Config{Profiles: []Profile{p}, Store: store})
	a.runBody(`{"profile":"default","conversation":"c1","project":"launch-q4","input":"hi"}`, http.StatusCreated)
	a.runBody(`{"profile":"default","conversation":"c1","input":"again"}`, http.StatusCreated)
	a.runBody(`{"profile":"default","conversation":"c1","project":"other","input":"x"}`, http.StatusBadRequest)
	a.runBody(`{"profile":"default","conversation":"c2","input":"none"}`, http.StatusCreated)
	a.runBody(`{"profile":"default","project":"../x","input":"x"}`, http.StatusBadRequest)
	if strings.Join(seen, ",") != "launch-q4,launch-q4," {
		t.Fatalf("ProjectOf %q", seen)
	}
	a.onlyConversation("?project=launch-q4", "c1")
	a.onlyConversation("?project=", "c2")
}

// runBody posts a run and waits for its end when it starts.
func (a *api) runBody(body string, want int) {
	a.t.Helper()
	code, out := a.do("POST", "/v1/runs", body)
	if code != want {
		a.t.Fatalf("%s: %d %s", body, code, out)
	}
	if code != http.StatusCreated {
		return
	}
	var s Snapshot
	_ = json.Unmarshal([]byte(out), &s)
	a.until(s.ID, func(s Snapshot) bool { return s.State != Running && s.State != Waiting })
}

// onlyConversation: the list filtered by query holds only id.
func (a *api) onlyConversation(query, id string) {
	a.t.Helper()
	var list []ConversationInfo
	a.json("GET", "/v1/conversations"+query, http.StatusOK, &list)
	if len(list) != 1 || list[0].ID != id {
		a.t.Fatalf("%s: %+v", query, list)
	}
}

// Two runs on one new conversation, each with its own project, at once:
// one of them is refused, never both run.
func TestConversationProjectRace(t *testing.T) {
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("a"), fake.Reply("b"))})}, Store: store})
	first, err := a.srv.StartFrom(Request{Profile: "default", Conversation: "race", Project: "x", Input: agent.UserText("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.Wait(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.StartFrom(Request{Profile: "default", Conversation: "race", Project: "y", Input: agent.UserText("hi")}); status(err) != http.StatusBadRequest {
		t.Fatalf("a run of another project on it: %v", err)
	}
}

// A conversation is moved to a project, and out of it.
func TestConversationMovedToAProject(t *testing.T) {
	a := storedAPI(t, fake.Reply("ok"))
	a.ran("c1", "hi")
	if code, out := a.do("PATCH", "/v1/conversations/c1", `{"project":"q1-plan"}`); code != http.StatusNoContent {
		t.Fatalf("%d %s", code, out)
	}
	a.onlyConversation("?project=q1-plan", "c1")
	if code, _ := a.do("PATCH", "/v1/conversations/c1", `{"project":"../x"}`); code != http.StatusBadRequest {
		t.Fatal("a bad project was taken")
	}
	if code, _ := a.do("PATCH", "/v1/conversations/c1", `{"project":""}`); code != http.StatusNoContent {
		t.Fatal("out of the project")
	}
	a.onlyConversation("?project=", "c1")
}
