// todo_test.go: the task list through the agent loop
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package todo

import (
	"context"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func TestListLivesInTheConversation(t *testing.T) {
	reg, err := agent.NewRegistry(Tool())
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(
		fake.Call(ToolName, `{"todos":[{"content":"Read the file","status":"in_progress","active_form":"Reading the file"},{"content":"Fix the bug","status":"pending"}]}`),
		fake.Call(ToolName, `{"todos":[{"content":"Read the file","status":"completed"},{"content":"Fix the bug","status":"in_progress"},{"content":"x","status":"in_progress"}]}`),
		fake.Call(ToolName, `{"todos":[{"content":"Read the file","status":"completed"},{"content":"Fix the bug","status":"in_progress"}]}`),
		fake.Reply("ok"),
	)
	a, err := agent.New(agent.Config{Provider: p, Tools: reg})
	if err != nil {
		t.Fatal(err)
	}
	conv := agent.NewConversation()
	if _, err := a.Run(context.Background(), conv, agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	got := Current(conv.Messages())
	if len(got) != 2 || got[0].Status != Completed || got[1].Status != InProgress {
		t.Fatalf("current %+v", got)
	}
	// The refused list (two in progress) did not count.
	msgs := p.Requests()[2].Messages
	if res := msgs[len(msgs)-1].Result; !res.IsError || !strings.Contains(res.Content, "Only one") {
		t.Fatalf("second call %+v", res)
	}
}

func TestSchemaHasStatusEnum(t *testing.T) {
	if s := string(Tool().Spec().InputSchema); !strings.Contains(s, `"enum":["pending","in_progress","completed"]`) {
		t.Fatalf("schema %s", s)
	}
}
