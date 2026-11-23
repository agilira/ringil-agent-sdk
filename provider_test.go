// provider_test.go: tests for requests
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRequestCloneIsDeep(t *testing.T) {
	temperature := 0.2
	format, err := FormatFor[struct {
		A string `json:"a"`
	}]("a")
	if err != nil {
		t.Fatal(err)
	}
	orig := &Request{
		System:      "sys",
		Messages:    []Message{UserText("a")},
		Tools:       []ToolSpec{{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:   10,
		Temperature: &temperature,
		ToolChoice:  ToolChoiceAuto,
		Format:      format,
	}
	clone := orig.Clone()
	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("clone differs: %+v", clone)
	}
	orig.Messages[0].Parts[0].Text = "b"
	orig.Tools[0].InputSchema[2] = 'X'
	*orig.Temperature = 0.9
	orig.Format.Schema[0] = 'X'
	if clone.Messages[0].Text() != "a" || clone.Tools[0].InputSchema[2] != 't' || *clone.Temperature != 0.2 || clone.Format.Schema[0] != '{' {
		t.Fatalf("clone shares memory with the original: %+v", clone)
	}
}

func TestRequestCloneKeepsNil(t *testing.T) {
	clone := (&Request{System: "s"}).Clone()
	if clone.Messages != nil || clone.Tools != nil || clone.Temperature != nil || clone.Format != nil || clone.System != "s" {
		t.Fatalf("clone changed empty fields: %+v", clone)
	}
}
