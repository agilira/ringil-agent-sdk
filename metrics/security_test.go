// security_test.go: no content, bounded labels, a valid format
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package metrics

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	stderrors "errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// What a run says and does (text, tool input and output, errors) never
// reaches the metrics.
func TestSecurityNoContent(t *testing.T) {
	m := New()
	obs := m.Observer("p")
	const secret = "sk-live-SECRET"
	obs(agent.Event{Kind: agent.EventStarted, Run: "r"})
	obs(agent.Event{Kind: agent.EventText, Text: secret})
	obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: "t", Input: json.RawMessage(`{"k":"` + secret + `"}`)}, Output: &agent.ToolResult{Content: secret}, Err: stderrors.New(secret)})
	obs(agent.Event{Kind: agent.EventLoop, Text: secret})
	obs(agent.Event{Kind: agent.EventFailed, Run: "r", Err: stderrors.New(secret)})
	if out := scrape(t, m); strings.Contains(out, secret) {
		t.Fatalf("content in the metrics:\n%s", out)
	}
}

// Tool names are the model's to choose and the servers' to list: their
// series are bounded.
func TestSecurityToolSeriesAreBounded(t *testing.T) {
	m := New()
	obs := m.Observer("p")
	for i := range 10 * MaxTools {
		obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: "tool_" + strconv.Itoa(i)}})
	}
	if n := strings.Count(scrape(t, m), "ringil_tool_calls_total{"); n > MaxTools+1 {
		t.Fatalf("%d tool series", n)
	}
}

// A hostile name cannot break the format: quotes, newlines, invalid UTF-8.
func TestSecurityLabelsAreEscaped(t *testing.T) {
	m := New()
	m.Observer("p")(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: "x\"}\nringil_fake 1\n\xff"}})
	for _, line := range strings.Split(strings.TrimSpace(scrape(t, m)), "\n") {
		if strings.HasPrefix(line, "ringil_fake") || !validLine(line) {
			t.Fatalf("broken line %q", line)
		}
	}
}

// validLine: a comment, or a name, optional labels and a number.
func validLine(line string) bool {
	if strings.HasPrefix(line, "# ") {
		return true
	}
	i := strings.LastIndexByte(line, ' ')
	if i < 0 {
		return false
	}
	_, err := strconv.ParseFloat(line[i+1:], 64)
	return err == nil && strings.HasPrefix(line, "ringil_")
}

func TestSecurityConcurrentObservers(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obs := m.Observer("p" + strconv.Itoa(i%2))
			for j := range 200 {
				run := strconv.Itoa(i) + "-" + strconv.Itoa(j)
				obs(agent.Event{Kind: agent.EventStarted, Run: run})
				obs(agent.Event{Kind: agent.EventDone, Run: run, Stop: agent.StopEnd})
				_ = scrape(t, m)
			}
		}()
	}
	wg.Wait()
	if out := scrape(t, m); !strings.Contains(out, `ringil_runs_started_total{profile="p0"} 800`) || !strings.Contains(out, `ringil_runs_active{profile="p0"} 0`) {
		t.Fatalf("counts:\n%s", out)
	}
}

func FuzzLabels(f *testing.F) {
	f.Add("read_file")
	f.Add("a\"b\\c\nd")
	f.Fuzz(func(t *testing.T, name string) {
		m := New()
		m.Observer(name)(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: name}})
		for _, line := range strings.Split(strings.TrimSpace(scrape(t, m)), "\n") {
			if !validLine(line) {
				t.Fatalf("broken line %q", line)
			}
		}
	})
}

func TestSecurityLabelsAndToolNamesAreBounded(t *testing.T) {
	m := New()
	profile := strings.Repeat("a", 63) + "€"
	longTool := strings.Repeat("t", 100)
	obs := m.Observer(profile)
	obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: ""}})
	obs(agent.Event{Kind: agent.EventToolEnd, Call: &agent.ToolCall{Name: longTool}})
	out := scrape(t, m)
	if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) || !strings.Contains(out, `profile="`+strings.Repeat("a", 63)+`"`) {
		t.Fatalf("broken UTF-8 in label:\n%s", out)
	}
	if !strings.Contains(out, `tool="(unknown)"`) {
		t.Fatalf("empty tool name not mapped to (unknown):\n%s", out)
	}
	if strings.Contains(out, longTool) || !strings.Contains(out, `tool="`+longTool[:64]+`"`) {
		t.Fatalf("long tool name not bounded:\n%s", out)
	}
}
