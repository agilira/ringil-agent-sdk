// main.go: streaming chat against any OpenAI-compatible server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Command chat sends one prompt and streams the answer.
//
//	go run ./examples/chat -base http://localhost:11434/v1 -model qwen2.5:3b "ciao"
//
// The API key, if any, is read from OPENAI_API_KEY.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chat:", err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("base", "http://localhost:11434/v1", "API base URL")
	model := flag.String("model", "qwen2.5:3b", "model name")
	withTool := flag.Bool("tool", false, "offer a weather tool and print the call")
	flag.Parse()
	p, err := openai.New(openai.Config{BaseURL: *base, Model: *model, APIKey: os.Getenv("OPENAI_API_KEY")})
	if err != nil {
		return err
	}
	req := &agent.Request{Messages: []agent.Message{agent.UserText(strings.Join(flag.Args(), " "))}}
	if *withTool {
		req.Tools = []agent.ToolSpec{{Name: "weather", Description: "Current temperature in a city",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}
	}
	resp, err := retry.Wrap(p, retry.Config{}).Generate(context.Background(), req, func(d string) error {
		_, err := fmt.Print(d)
		return err
	})
	if err != nil {
		return err
	}
	for _, call := range resp.Message.Calls {
		fmt.Printf("tool call %s %s %s\n", call.ID, call.Name, call.Input)
	}
	fmt.Printf("\n[stop=%s input=%d output=%d]\n", resp.Stop, resp.Usage.Input, resp.Usage.Output)
	return nil
}
