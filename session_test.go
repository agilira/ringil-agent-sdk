// session_test.go: tests for history validation and in-memory conversations
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agilira/go-errors"
)

func call(id string) ToolCall { return ToolCall{ID: id, Name: "t", Input: json.RawMessage(`{}`)} }

func calls(ids ...string) Message {
	m := Message{Role: RoleAssistant}
	for _, id := range ids {
		m.Calls = append(m.Calls, call(id))
	}
	return m
}

func result(id string) Message { return ToolMessage(ToolResult{CallID: id, Content: "ok"}) }

func TestValidateHistoryAccepts(t *testing.T) {
	good := [][]Message{
		nil,
		{UserText("a"), AssistantText("b")},
		{UserText("a"), calls("1", "2"), result("2"), result("1"), AssistantText("c"), UserText("d")},
		{UserText("a"), calls("1", "2"), result("1")}, // interrupted at the end
	}
	for i, h := range good {
		if err := ValidateHistory(h); err != nil {
			t.Errorf("case %d refused: %v", i, err)
		}
	}
}

func TestValidateHistoryRefuses(t *testing.T) {
	withResult := UserText("x")
	withResult.Result = &ToolResult{CallID: "1"}
	bad := [][]Message{
		{result("1")},
		{UserText("a"), calls("1"), result("1"), result("1")},
		{UserText("a"), calls("1"), UserText("b")},
		{UserText("a"), calls("1"), AssistantText("b")},
		{UserText("a"), calls("1"), result("1"), calls("1")},
		{UserText("a"), calls("")},
		{{Role: "system"}},
		{{Role: RoleTool}},
		{{Role: RoleUser, Calls: []ToolCall{call("1")}}},
		{withResult},
		{{Role: RoleTool, Result: &ToolResult{CallID: "1"}, Calls: []ToolCall{call("2")}}},
	}
	for i, h := range bad {
		if err := ValidateHistory(h); !errors.HasCode(err, ErrSessionCorrupt) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
}

func TestPendingCalls(t *testing.T) {
	cases := []struct {
		h    []Message
		want string
	}{
		{nil, ""},
		{[]Message{UserText("a")}, ""},
		{[]Message{UserText("a"), calls("1", "2"), result("1")}, "2"},
		{[]Message{UserText("a"), calls("1", "2")}, "1,2"},
		{[]Message{UserText("a"), calls("1"), result("1")}, ""},
		{[]Message{UserText("a"), AssistantText("b")}, ""},
	}
	for i, c := range cases {
		var ids []string
		for _, p := range pendingCalls(c.h) {
			ids = append(ids, p.ID)
		}
		if strings.Join(ids, ",") != c.want {
			t.Errorf("case %d: %v, want %s", i, ids, c.want)
		}
	}
}

func TestValidateSessionID(t *testing.T) {
	for _, id := range []string{"a", "chat-1_X", strings.Repeat("a", 64)} {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", "../x", "a/b", `a\b`, "a.b", "con ", strings.Repeat("a", 65), "é"} {
		if err := ValidateSessionID(id); !errors.HasCode(err, ErrSessionInput) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestMemoryConversationCopies(t *testing.T) {
	orig := UserText("a")
	conv := NewConversation(orig)
	orig.Parts[0].Text = "changed"
	got := conv.Messages()
	got[0].Parts[0].Text = "also changed"
	if conv.Messages()[0].Text() != "a" {
		t.Fatal("conversation shares memory with callers")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := conv.Append(ctx, UserText("b")); err == nil || len(conv.Messages()) != 1 {
		t.Fatal("append after cancel")
	}
}

func TestMemoryConversationConcurrent(t *testing.T) {
	conv := NewConversation()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := conv.Append(context.Background(), UserText("x")); err != nil {
				t.Error(err)
			}
			conv.Messages()
		}()
	}
	wg.Wait()
	if len(conv.Messages()) != 20 {
		t.Fatalf("got %d", len(conv.Messages()))
	}
}

func TestHistoryCheckerIsAllOrNothing(t *testing.T) {
	h := NewHistoryChecker()
	if err := h.Add(UserText("a"), calls("1")); err != nil {
		t.Fatal(err)
	}
	// The batch answers 1, opens 2, then breaks: nothing of it may stick.
	if err := h.Add(result("1"), calls("2"), UserText("bad")); !errors.HasCode(err, ErrSessionCorrupt) {
		t.Fatalf("bad batch accepted: %v", err)
	}
	if err := h.Add(result("1"), calls("2"), result("2")); err != nil {
		t.Fatalf("checker kept part of the refused batch: %v", err)
	}
}

// Allowed rules are checked as a store keeps them, and count as labels.
func TestLabelsAllowed(t *testing.T) {
	ok := Labels{Allowed: map[string][]string{"": {"write_file(notes/**)"}, "researcher": {"bash(git status*)"}}}
	if ok.Check() != nil || ok.IsZero() {
		t.Fatalf("%v %v", ok.Check(), ok.IsZero())
	}
	for _, bad := range []map[string][]string{{"": {"not a rule ("}}, {"bad\nname": {"read_file"}}} {
		if (Labels{Allowed: bad}).Check() == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if !(Labels{}).IsZero() {
		t.Fatal("zero labels")
	}
}
