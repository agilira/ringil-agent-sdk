// compact_internal_test.go: internals of the compactor
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"strconv"
	"strings"
	"testing"
)

func TestSummaryCacheIsBounded(t *testing.T) {
	c := &compactor{}
	for i := range summaryCacheSize * 3 {
		c.store(cachedSummary{n: i + 1, hash: strconv.Itoa(i)})
	}
	if len(c.summaries) != summaryCacheSize || c.summaries[0].n != 2*summaryCacheSize+1 {
		t.Fatalf("cache %d entries, oldest n=%d", len(c.summaries), c.summaries[0].n)
	}
}

func TestRatioIsClamped(t *testing.T) {
	c := &compactor{}
	c.learn(1_000_000, 1)
	if c.tokens(1600) != 100 {
		t.Fatalf("ratio not capped: %d", c.tokens(1600))
	}
	c.learn(10, 1000)
	if c.tokens(100) != 100 {
		t.Fatalf("ratio not floored: %d", c.tokens(100))
	}
	c.learn(0, 5)
	c.learn(5, 0)
	if c.tokens(100) != 100 {
		t.Fatal("zero values changed the ratio")
	}
}

func TestClipAndFitTailKeepRunes(t *testing.T) {
	if got := clip("àààà", 3); got != "à [...]" {
		t.Fatalf("clip %q", got)
	}
	if got := fitTail("àààà", 3); got != "[earlier part omitted]\nà" {
		t.Fatalf("fitTail %q", got)
	}
	if got := fitTail("abc", 0); got != "[transcript omitted]" {
		t.Fatalf("fitTail zero %q", got)
	}
}

// A cut keeps the tools tool_search loaded in the part left out, and the
// application's notes are not the model's words in a summary (audit 21/11).
func TestCutKeepsLoadedTools(t *testing.T) {
	search := Message{Role: RoleAssistant, Calls: []ToolCall{{ID: "s1", Name: SearchToolName, Input: []byte(`{}`)}}}
	msgs := []Message{UserText("uno"), search, ToolMessage(ToolResult{CallID: "s1", Content: `{"tools":[{"name":"weather"}]}`}),
		AssistantText("ok"), UserText("due"), AssistantText("ok")}
	v := &sized{c: &compactor{}, msgs: msgs, sizes: make([]int, len(msgs))}
	v.drop(4, "[omitted]")
	if !strings.Contains(v.msgs[0].ModelText(), `"name":"weather"`) {
		t.Fatalf("%q", v.msgs[0].ModelText())
	}
	if got := transcript([]Message{SystemText("Your last reply was empty.")}); !strings.HasPrefix(got, "Note from the application") {
		t.Fatalf("%q", got)
	}
	if startsTurn(msgs, 2) || !startsTurn(msgs, 4) {
		t.Fatal("startsTurn")
	}
}
