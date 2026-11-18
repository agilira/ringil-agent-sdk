// main.go: a complete agent in a few lines
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Command agent is the smallest complete agent: a model, a typed tool, a
// conversation saved to disk, streamed output. Run it twice: the second run
// remembers the first.
//
//	export RINGIL_BASE_URL=http://localhost:11434/v1 RINGIL_MODEL=qwen2.5:3b
//	go run ./examples/agent "Ricorda che il mio colore preferito è il verde"
//	go run ./examples/agent "Qual è il mio colore preferito?"
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

type clockIn struct {
	Zone string `json:"zone" desc:"IANA time zone, e.g. Europe/Rome"`
}

func clock(_ context.Context, in clockIn) (string, error) {
	loc, err := time.LoadLocation(in.Zone)
	if err != nil {
		return "", agent.ToolError("UNKNOWN_ZONE", "Unknown time zone.")
	}
	return time.Now().In(loc).Format(time.RFC1123), nil
}

func main() {
	ctx := context.Background()
	model, err := openai.New(openai.Config{
		BaseURL: os.Getenv("RINGIL_BASE_URL"),
		Model:   os.Getenv("RINGIL_MODEL"),
		KeyFunc: secrets.OptionalKeyFunc(secrets.Environment{}, "RINGIL_API_KEY"),
	})
	check(err)
	tools, err := agent.NewRegistry(agent.NewTool("clock", "Current time in a time zone", clock, agent.ReadOnly()))
	check(err)
	bot, err := agent.New(agent.Config{Provider: model, Tools: tools})
	check(err)

	store, err := file.New(os.TempDir() + "/ringil-example")
	check(err)
	sess, err := store.Open(ctx, "demo")
	check(err)
	defer func() { check(sess.Close()) }()

	for ev, err := range bot.Stream(ctx, sess, agent.UserText(strings.Join(os.Args[1:], " "))) {
		check(err)
		if ev.Kind == agent.EventText {
			fmt.Print(ev.Text)
		}
	}
	fmt.Println()
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}
