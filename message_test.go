// message_test.go: tests for messages and usage
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTextConstructors(t *testing.T) {
	cases := []struct {
		msg  Message
		role Role
	}{
		{UserText("ciao"), RoleUser},
		{AssistantText("ciao"), RoleAssistant},
	}
	for _, c := range cases {
		if c.msg.Role != c.role || c.msg.Text() != "ciao" {
			t.Errorf("got role %q text %q, want %q ciao", c.msg.Role, c.msg.Text(), c.role)
		}
	}
}

func TestToolMessageCopiesResult(t *testing.T) {
	result := ToolResult{CallID: "c1", Content: "ok"}
	msg := ToolMessage(result)
	result.Content = "changed"
	if msg.Role != RoleTool || msg.Result.Content != "ok" {
		t.Fatalf("tool message shares or lost the result: %+v", msg.Result)
	}
}

func TestTextJoinsOnlyTextParts(t *testing.T) {
	msg := Message{Parts: []Part{
		{Type: PartText, Text: "a"},
		{Type: "image", Text: "ignored"},
		{Type: PartText, Text: "b"},
	}}
	if got := msg.Text(); got != "ab" {
		t.Fatalf("Text() = %q, want ab", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	orig := Message{
		Role:   RoleAssistant,
		Parts:  []Part{{Type: PartText, Text: "x"}},
		Calls:  []ToolCall{{ID: "1", Name: "t", Input: json.RawMessage(`{"a":1}`)}},
		Result: &ToolResult{CallID: "1", Content: "r"},
	}
	clone := orig.Clone()
	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("clone differs: %+v", clone)
	}
	orig.Parts[0].Text = "y"
	orig.Calls[0].Input[2] = 'b'
	orig.Calls[0].Name = "u"
	orig.Result.Content = "s"
	if clone.Parts[0].Text != "x" || string(clone.Calls[0].Input) != `{"a":1}` ||
		clone.Calls[0].Name != "t" || clone.Result.Content != "r" {
		t.Fatalf("clone shares memory with the original: %+v", clone)
	}
}

func TestCloneKeepsNil(t *testing.T) {
	clone := UserText("x").Clone()
	if clone.Calls != nil || clone.Result != nil {
		t.Fatalf("clone invented fields: %+v", clone)
	}
}

func TestUsageAdd(t *testing.T) {
	got := Usage{1, 2, 3, 4}.Add(Usage{10, 20, 30, 40})
	if got != (Usage{11, 22, 33, 44}) {
		t.Fatalf("Add = %+v", got)
	}
}

func TestMessageJSONRoundTrip(t *testing.T) {
	msg := Message{
		Role:  RoleAssistant,
		Parts: []Part{{Type: PartText, Text: "testo"}},
		Calls: []ToolCall{{ID: "c", Name: "meteo", Input: json.RawMessage(`{"citta":"Roma"}`), BadInput: true}},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(msg, back) {
		t.Fatalf("round trip changed the message:\n%s\n%+v", data, back)
	}
}

func TestToolSpecOmitsEmptyAnnotations(t *testing.T) {
	data, err := json.Marshal(ToolSpec{Name: "t", InputSchema: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"name":"t","description":"","input_schema":{}}` {
		t.Fatalf("unexpected encoding %s", data)
	}
}
