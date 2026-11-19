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
