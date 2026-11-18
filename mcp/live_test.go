// live_test.go: a real model using an MCP server's tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package mcp

import (
	"context"
	"os"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

func TestLiveMCPTool(t *testing.T) {
	base, model := os.Getenv("RINGIL_BASE_URL"), os.Getenv("RINGIL_MODEL")
	if base == "" || model == "" {
		t.Skip("RINGIL_BASE_URL and RINGIL_MODEL not set")
	}
	p, err := openai.New(openai.Config{BaseURL: base, Model: model, APIKey: os.Getenv("RINGIL_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(inMemory(t).Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: p, Tools: tools, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), nil, agent.UserText("Usa il tool srv__add per sommare 1234 e 4321, e dimmi il risultato."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(res.Text, ".", ""), "5555") {
		t.Fatalf("answer %q", res.Text)
	}
}
