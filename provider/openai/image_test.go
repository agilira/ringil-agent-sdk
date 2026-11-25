// image_test.go: user images on the wire
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"encoding/json"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
)

func TestUserImages(t *testing.T) {
	img := agent.Part{Type: agent.PartImage, MIME: "image/png", Data: []byte{1, 2, 3}}
	req := &agent.Request{Messages: []agent.Message{agent.UserMessage("what is it?", img)}}
	for _, images := range []bool{true, false} {
		data, err := (&provider{cfg: Config{Model: "m", Images: images}}).encode(req, false)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		got := string(wire.Messages[0].Content)
		if images {
			want := `[{"type":"text","text":"what is it?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AQID"}}]`
			if got != want {
				t.Fatalf("with images: %s", got)
			}
		} else if !strings.Contains(got, "what is it?") || !strings.Contains(got, "cannot see") || strings.Contains(got, "AQID") {
			t.Fatalf("without images: %s", got)
		}
	}
}

func TestTextOnlyUserMessageStaysAString(t *testing.T) {
	data, err := (&provider{cfg: Config{Model: "m", Images: true}}).encode(&agent.Request{Messages: []agent.Message{agent.UserText("hi")}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"content":"hi"`) {
		t.Fatalf("%s", data)
	}
}

// A PDF goes as a file where the API reads them, else it is named.
func TestDocuments(t *testing.T) {
	pdf := agent.Part{Type: agent.PartDocument, MIME: "application/pdf", Data: []byte("%PDF-1")}
	res := agent.ToolResult{CallID: "c1", Content: "text", Images: []agent.Part{pdf}}
	calls := agent.Message{Role: agent.RoleAssistant, Calls: []agent.ToolCall{{ID: "c1", Name: "read_document", Input: json.RawMessage(`{}`)}}}
	req := &agent.Request{Messages: []agent.Message{agent.UserText("leggi"), calls, agent.ToolMessage(res)}}
	for _, docs := range []bool{true, false} {
		data, err := (&provider{cfg: Config{Model: "m", Documents: docs}}).encode(req, false)
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		file := strings.Contains(got, `"type":"file"`) && strings.Contains(got, `"file_data":"data:application/pdf;base64,JVBERi0x"`)
		if docs != file || docs == strings.Contains(got, "cannot see") {
			t.Fatalf("documents %v: %s", docs, got)
		}
	}
}
