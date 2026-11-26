// conversations_test.go: the stored conversations over the API
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
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
