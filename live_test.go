// live_test.go: tools the host changes while the agent runs
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type noArgs struct{}

func named(name string, ran *[]string) agent.Tool {
	return agent.NewTool(name, "the "+name+" tool", func(context.Context, noArgs) (string, error) {
		*ran = append(*ran, name)
		return name + " done", nil
	}, agent.ReadOnly())
}

func mustLive(t *testing.T, tools ...agent.Tool) *agent.LiveRegistry {
	t.Helper()
	reg, err := agent.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	return agent.NewLiveRegistry(reg)
}

// A tool installed by a call is offered at the next model call of the
// same run, and runs.
func TestLiveToolAddedDuringARun(t *testing.T) {
	var ran []string
	live := mustLive(t)
	install := agent.NewTool("install", "install a server", func(context.Context, noArgs) (string, error) {
		return "installed", live.Add(named("fresh", &ran))
	}, agent.ReadOnly())
	if err := live.Add(install); err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Call("install", `{}`), fake.Call("fresh", `{}`), fake.Reply("ok"))
	res, err := newAgent(t, agent.Config{Provider: p, Tools: live}).Run(context.Background(), nil, agent.UserText("installa e usa"))
	if err != nil || res.Text != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
	reqs := p.Requests()
	if names(reqs[0].Tools) != "install" || names(reqs[1].Tools) != "fresh,install" {
		t.Fatalf("tools offered: %s then %s", names(reqs[0].Tools), names(reqs[1].Tools))
	}
	if strings.Join(ran, ",") != "fresh" {
		t.Fatalf("ran %v", ran)
	}
}

// A removed tool is no longer offered, and a call to it is refused.
func TestLiveToolRemovedDuringARun(t *testing.T) {
	var ran []string
	live := mustLive(t, named("old", &ran))
	drop := agent.NewTool("drop", "remove old", func(context.Context, noArgs) (string, error) {
		return "dropped", live.Remove("old")
	}, agent.ReadOnly())
	if err := live.Add(drop); err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Call("drop", `{}`), fake.Call("old", `{}`), fake.Reply("ok"))
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: live}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	if got := names(p.Requests()[1].Tools); got != "drop" {
		t.Fatalf("after remove: %s", got)
	}
	if len(ran) != 0 {
		t.Fatalf("a removed tool ran: %v", ran)
	}
}

// With many schemas, a tool added during a run joins the tool_search
// catalog at the next call, and a later run starts from it.
func TestLiveToolsRebuildTheCatalog(t *testing.T) {
	calls := 0
	live := mustLive(t, bigTool("gmail_fetch", "Fetch Gmail messages", &calls))
	install := agent.NewTool("install", "install a server", func(context.Context, noArgs) (string, error) {
		return "installed", live.Add(bigTool("drive_list", "List Drive files", &calls))
	}, agent.ReadOnly())
	if err := live.Add(install); err != nil {
		t.Fatal(err)
	}
	p := fake.New(fake.Call("install", `{}`), fake.Reply("one"), fake.Reply("two"))
	a := newAgent(t, agent.Config{Provider: p, Tools: live, DiscoveryThreshold: 1000})
	conv := agent.NewConversation()
	for _, text := range []string{"a", "b"} {
		if _, err := a.Run(context.Background(), conv, agent.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	reqs := p.Requests()
	want := "drive_list,gmail_fetch,install," + agent.SearchToolName
	if names(reqs[0].Tools) != "gmail_fetch,install,"+agent.SearchToolName || names(reqs[1].Tools) != want || names(reqs[2].Tools) != want {
		t.Fatalf("tools: %s / %s / %s", names(reqs[0].Tools), names(reqs[1].Tools), names(reqs[2].Tools))
	}
	if reqs[1].System != reqs[0].System {
		t.Fatal("the system prompt changed though the catalog block is the same")
	}
}

// Without a change the system prompt stays the same at every call, so
// the provider's cache keeps it.
func TestLiveToolsUnchangedKeepTheSystemPrompt(t *testing.T) {
	var ran []string
	live := mustLive(t, named("a", &ran))
	p := fake.New(fake.Call("a", `{}`), fake.Call("a", `{}`), fake.Reply("ok"))
	if _, err := newAgent(t, agent.Config{Provider: p, Tools: live}).Run(context.Background(), nil, agent.UserText("x")); err != nil {
		t.Fatal(err)
	}
	reqs := p.Requests()
	for _, r := range reqs[1:] {
		if r.System != reqs[0].System || names(r.Tools) != names(reqs[0].Tools) {
			t.Fatal("the request changed without a change of tools")
		}
	}
}

func TestLiveRegistryOperations(t *testing.T) {
	var ran []string
	live := mustLive(t, named("a", &ran))
	if err := live.Add(named("a", &ran)); err == nil {
		t.Fatal("a duplicate name was added")
	}
	fixed, err := live.With(named("b", &ran))
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Remove("a", "missing"); err != nil {
		t.Fatal(err)
	}
	if live.Has("a") || !fixed.Has("a") || !fixed.Has("b") {
		t.Fatalf("live %s, fixed %s", names(live.Specs()), names(fixed.Specs()))
	}
	live.Set(nil)
	if len(live.Specs()) != 0 {
		t.Fatal("Set(nil) kept tools")
	}
	if _, err := agent.NewLiveRegistry(nil).Select(); err != nil {
		t.Fatal(err)
	}
}

// Changes while runs read the tools: no race, no torn registry.
func TestLiveRegistryConcurrent(t *testing.T) {
	var ran []string
	live := mustLive(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			name := "t" + string(rune('a'+i))
			_ = live.Add(agent.NewTool(name, "x", func(context.Context, noArgs) (string, error) { return "", nil }))
			_ = live.Remove(name)
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				_ = live.Specs()
				_ = live.Has("ta")
			}
		}()
	}
	wg.Wait()
	_ = ran
}
