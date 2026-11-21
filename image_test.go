// image_test.go: images in messages
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/agilira/go-errors"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCheckImage(t *testing.T) {
	data := pngBytes(t)
	if err := CheckImage("image/png", data); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mime string
		data []byte
	}{
		"label lies":   {"image/jpeg", data},
		"not an image": {"image/png", []byte("<svg onload=x>")},
		"svg":          {"image/svg+xml", []byte("<svg/>")},
		"empty":        {"image/png", nil},
		"too large":    {"image/png", append(data, make([]byte, MaxImageBytes)...)},
	} {
		if err := CheckImage(c.mime, c.data); !errors.HasCode(err, ErrImage) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestImageAndUserMessage(t *testing.T) {
	p, err := Image(pngBytes(t))
	if err != nil || p.MIME != "image/png" || p.Type != PartImage {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := Image([]byte("plain text")); err == nil {
		t.Fatal("text taken as an image")
	}
}

func TestUserMessage(t *testing.T) {
	p, err := Image(pngBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	m := UserMessage("look", p, p)
	if m.Text() != "look" || m.Images() != 2 || m.Role != RoleUser {
		t.Fatalf("%+v", m)
	}
	if only := UserMessage("", p); len(only.Parts) != 1 {
		t.Fatalf("empty text kept: %+v", only.Parts)
	}
	c := m.Clone()
	c.Parts[1].Data[0] ^= 0xff
	if m.Parts[1].Data[0] == c.Parts[1].Data[0] {
		t.Fatal("Clone shares image bytes")
	}
}

func TestImagesAreCountedNotTheirBytes(t *testing.T) {
	big := append(pngBytes(t), make([]byte, 1<<20)...)
	img := Part{Type: PartImage, MIME: "image/png", Data: big}
	with := requestBytes(&Request{Messages: []Message{UserMessage("hi", img)}})
	without := requestBytes(&Request{Messages: []Message{UserText("hi")}})
	if d := with - without; d < imageTokens*bytesPerToken || d > imageTokens*bytesPerToken+100 {
		t.Fatalf("an image of 1 MiB counted as %d bytes", d)
	}
	if d := messageBytes(UserMessage("hi", img)) - messageBytes(UserText("hi")); d > imageTokens*bytesPerToken+100 {
		t.Fatalf("message: %d", d)
	}
	if !strings.Contains(transcript([]Message{UserMessage("hi", img, img)}), "[2 images]") {
		t.Fatal("the transcript does not say there were images")
	}
}

// Every image is sent by default, as mature agents do (21/11: all but the
// latest were always dropped); KeepImages keeps the latest few, for the
// servers that take one per request.
func TestImagesAreKeptUnlessToldOtherwise(t *testing.T) {
	img := Part{Type: PartImage, MIME: "image/png", Data: []byte{1}}
	msgs := []Message{UserMessage("a", img, img), AssistantText("ok"), UserMessage("b", img), AssistantText("ok"), UserText("and?")}
	if all := latestImages(msgs, 0); all[0].Images() != 2 || all[2].Images() != 1 {
		t.Fatalf("%+v", all)
	}
	if two := latestImages(msgs, 2); two[0].Images() != 2 {
		t.Fatalf("%+v", two)
	}
	out := latestImages(msgs, 1)
	if out[0].Images() != 0 || !strings.Contains(out[0].Text(), "2 images") || out[2].Images() != 1 {
		t.Fatalf("%+v", out)
	}
	if msgs[0].Images() != 2 {
		t.Fatal("the conversation was changed")
	}
	if got := latestImages([]Message{UserText("x")}, 1); got[0].Text() != "x" {
		t.Fatal(got)
	}
}

type shotTool struct{ out ToolOutput }

func (s shotTool) Spec() ToolSpec {
	return ToolSpec{Name: "shot", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (s shotTool) Call(context.Context, json.RawMessage) (ToolOutput, error) {
	return s.out, nil
}

// A tool's images reach the result (21/11: tool results were text only);
// one a model API would refuse does not.
func TestToolImages(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	tool := shotTool{ToolOutput{Content: "screen", Images: []Part{{Type: PartImage, MIME: "image/png", Data: png}, {Type: PartImage, MIME: "image/png", Data: []byte("x")}}}}
	reg, err := NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Execute(context.Background(), ToolCall{ID: "1", Name: "shot", Input: json.RawMessage(`{}`)})
	if err != nil || len(res.Images) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	msg := ToolMessage(res)
	if msg.Images() != 1 || messageBytes(msg) > len(`{"role":"tool"}`)+200+imageTokens*bytesPerToken {
		t.Fatalf("counted %d images, %d bytes", msg.Images(), messageBytes(msg))
	}
	if noted := latestImages([]Message{msg, UserMessage("again", Part{Type: PartImage, MIME: "image/png", Data: png})}, 1); noted[0].Images() != 0 || !strings.Contains(noted[0].Result.Content, "no longer shown") {
		t.Fatalf("%+v", noted[0].Result)
	}
}
