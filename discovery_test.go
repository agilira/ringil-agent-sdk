// discovery_test.go: deferred tools and tool_search
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type noIn struct{}

func bigTool(name, desc string, calls *int) agent.Tool {
	return agent.NewTool(name, desc+strings.Repeat(" padding", 400), func(context.Context, noIn) (string, error) {
		*calls++
		return name + " done", nil
	}, agent.Deferrable(), agent.ReadOnly()) // a fake business read
}

func names(specs []agent.ToolSpec) string {
	var n []string
	for _, s := range specs {
		n = append(n, s.Name)
	}
	return strings.Join(n, ",")
}

func TestDeferredToolsLoadOnSearch(t *testing.T) {
	calls := 0
	reg, err := agent.NewRegistry(bigTool("gmail_fetch", "Fetch Gmail messages", &calls), bigTool("slack_post", "Post to Slack", &calls), adder(&calls))
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(
		fake.Call(agent.SearchToolName, `{"query":"read my gmail messages"}`),
		fake.Call("gmail_fetch", `{}`),
		fake.Reply("ok"),
		fake.Reply("again"),
	)
	a := newAgent(t, agent.Config{Provider: p, Tools: reg, DiscoveryThreshold: 1000})
	conv := agent.NewConversation()
	if _, err := a.Run(context.Background(), conv, agent.UserText("leggi la posta")); err != nil {
		t.Fatal(err)
	}
	checkFirstRun(t, p.Requests(), calls)
	// A later run in the same conversation keeps what was loaded.
	if _, err := a.Run(context.Background(), conv, agent.UserText("ancora")); err != nil {
		t.Fatal(err)
	}
	if got := names(p.Requests()[3].Tools); got != names(p.Requests()[0].Tools) {
		t.Fatalf("next run: %s", got)
	}
}

func checkFirstRun(t *testing.T, reqs []*agent.Request, calls int) {
	t.Helper()
	// Deferred tools come as light entries: one line, any object.
	first := reqs[0].Tools
	if got := names(first); got != "add,gmail_fetch,slack_post,tool_search" {
		t.Fatalf("first request tools %s", got)
	}
	if g := first[1]; !strings.Contains(g.Description, "Parameters not shown") || string(g.InputSchema) != `{"type":"object"}` {
		t.Fatalf("light entry %+v", g)
	}
	sameListWithParameters(t, first, reqs[1])
	if calls != 1 {
		t.Fatalf("gmail_fetch ran %d times", calls)
	}
}

func TestSmallCatalogIsSentWhole(t *testing.T) {
	calls := 0
	reg, err := agent.NewRegistry(agent.NewTool("tiny", "small", func(context.Context, noIn) (string, error) { return "", nil }, agent.Deferrable()), adder(&calls))
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Reply("ok"))
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: reg}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	if got := names(p.Requests()[0].Tools); strings.Contains(got, agent.SearchToolName) || !strings.Contains(got, "tiny") {
		t.Fatalf("small catalog %s", got)
	}
}

func TestDiscoveryAlwaysAndOff(t *testing.T) {
	calls := 0
	for mode, want := range map[agent.Discovery]string{agent.DiscoveryAlways: "gmail_fetch,tool_search", agent.DiscoveryOff: "gmail_fetch"} {
		reg, err := agent.NewRegistry(bigTool("gmail_fetch", "Fetch", &calls))
		if err != nil {
			t.Fatal(err)
		}
		p := fake.New(fake.Reply("ok"))
		if _, err := newAgent(t, agent.Config{Provider: p, Tools: reg, Discovery: mode}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
			t.Fatal(err)
		}
		if got := names(p.Requests()[0].Tools); got != want {
			t.Errorf("mode %d: %s", mode, got)
		}
	}
}

// Audit 18/11: stopwords loaded arbitrary tools, plurals found nothing,
// and an unknown name brought back the whole catalog.
func TestSearchRankingAndUnknownTool(t *testing.T) {
	calls := 0
	var tools []agent.Tool
	for _, n := range []string{"gmail_send", "slack_post", "notion_page", "github_issue", "calendar_event"} {
		tools = append(tools, bigTool(n, "Use the "+strings.Split(n, "_")[0]+" API to do the "+strings.Split(n, "_")[1]+" task", &calls))
	}
	reg, err := agent.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(
		fake.Call(agent.SearchToolName, `{"query":"the"}`),
		fake.Call(agent.SearchToolName, `{"query":"send the emails with gmail"}`),
		fake.Call("nonexistent_tool", `{}`),
		fake.Reply("ok"),
	)
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: reg, Discovery: agent.DiscoveryAlways}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	results := toolResults(p.Requests()[3].Messages)
	if !strings.Contains(results[0], `"loaded":null`) || !strings.Contains(results[1], `"loaded":["gmail_send"]`) {
		t.Fatalf("search results %q", results[:2])
	}
	if strings.Contains(results[2], "slack_post") || !strings.Contains(results[2], agent.SearchToolName) {
		t.Fatalf("unknown tool result %q", results[2])
	}
}

func TestDeferredBlockIsBounded(t *testing.T) {
	calls := 0
	var tools []agent.Tool
	for i := range 2000 {
		tools = append(tools, bigTool("tool_"+strings.Repeat("x", 20)+"_"+strconv.Itoa(i), "A description long enough to matter in the list of deferred tools", &calls))
	}
	reg, err := agent.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Reply("ok"))
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: reg}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	req := p.Requests()[0]
	if len(req.Tools) != 301 || !strings.Contains(req.System, "1700 more tools") {
		t.Fatalf("%d tools offered; system %q", len(req.Tools), req.System)
	}
}

func toolResults(msgs []agent.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Result != nil {
			out = append(out, m.Result.Content)
		}
	}
	return out
}

// sameListWithParameters: 21/11, the list stays as it was, so the
// provider's cache keeps it; the loaded tool's parameters came in
// tool_search's result.
func sameListWithParameters(t *testing.T, first []agent.ToolSpec, next *agent.Request) {
	t.Helper()
	if !slices.EqualFunc(next.Tools, first, func(a, b agent.ToolSpec) bool {
		return a.Name == b.Name && a.Description == b.Description && string(a.InputSchema) == string(b.InputSchema)
	}) {
		t.Fatalf("the tools changed after a search: %s", names(next.Tools))
	}
	last := next.Messages[len(next.Messages)-1]
	if last.Result == nil || !strings.Contains(last.Result.Content, `"name":"gmail_fetch"`) || !strings.Contains(last.Result.Content, `"parameters"`) {
		t.Fatalf("the search gave no parameters: %+v", last)
	}
}
