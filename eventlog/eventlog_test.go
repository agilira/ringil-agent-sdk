// eventlog_test.go: metadata in, content never
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package eventlog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/eventlog"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

const secret = "SECRET-7f3a"

func TestLogsMetadataNeverContent(t *testing.T) {
	type in struct {
		Q string `json:"q"`
	}
	tool := agent.NewTool("search", "s", func(context.Context, in) (string, error) {
		return "", agent.ToolError("SEARCH_DOWN", "the index at /srv/"+secret+" is down")
	}, agent.ReadOnly())
	reg, err := agent.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := fake.New(fake.Step{Chunks: []string{"thinking " + secret}, Calls: []agent.ToolCall{{Name: "search", Input: json.RawMessage(`{"q":"` + secret + `"}`)}}},
		fake.Call(secret+" bad name!", `{}`), fake.Reply("answer "+secret))
	a, err := agent.New(agent.Config{Provider: p, Tools: reg, Observers: []agent.Observer{eventlog.Slog(l)}, System: "system " + secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("question "+secret)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("content in the log:\n%s", out)
	}
	for _, want := range []string{`"msg":"agent.started"`, `"msg":"agent.tool_end"`, `"tool":"search"`, `"error":"SEARCH_DOWN"`, `"is_error":true`, `"tool":"(unknown tool)"`, `"msg":"agent.done"`, `"turns":3`, `"seq":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "agent.text") {
		t.Error("text deltas logged")
	}
}

func TestFailureIsAnErrorRecord(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Fail(errors.New(agent.ErrProviderAuth, "key "+secret+" refused"))), Observers: []agent.Observer{eventlog.Slog(l)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("x")); err == nil {
		t.Fatal("no error")
	}
	out := buf.String()
	if !strings.Contains(out, `"level":"ERROR","msg":"agent.failed"`) || !strings.Contains(out, `"error":"PROVIDER_AUTH"`) || strings.Contains(out, secret) {
		t.Fatalf("%s", out)
	}
}

// Audit 18/11: a stop reason a provider passed through is not logged as
// is; a known one is.
func TestStopReasonsAreTheAgents(t *testing.T) {
	text := func(ev agent.Event) string {
		var b strings.Builder
		for _, a := range eventlog.Attrs(ev) {
			b.WriteString(a.String() + " ")
		}
		return b.String()
	}
	if got := text(agent.Event{Kind: agent.EventModel, Stop: agent.StopReason("key=" + secret)}); strings.Contains(got, secret) || !strings.Contains(got, "stop=(other)") {
		t.Fatal(got)
	}
	if got := text(agent.Event{Kind: agent.EventModel, Stop: agent.StopLoop, Parent: "P1"}); !strings.Contains(got, "stop=loop") || !strings.Contains(got, "parent=P1") {
		t.Fatal(got)
	}
}
