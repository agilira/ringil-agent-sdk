// discovery.go: tools shown by name, loaded when the model needs them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Discovery decides whether deferrable tools (MCP servers, large
// catalogs) are sent with their schemas or only listed by name.
type Discovery int

const (
	// DiscoveryAuto defers when the deferrable tools' schemas exceed
	// Config.DiscoveryThreshold bytes.
	DiscoveryAuto Discovery = iota
	// DiscoveryOff always sends every schema.
	DiscoveryOff
	// DiscoveryAlways defers every deferrable tool.
	DiscoveryAlways
)

// Deferrable marks a tool the agent may list by name only, loading its
// schema through tool_search when the model asks for it.
func Deferrable() ToolOption { return func(s *ToolSpec) { s.Annotations.Deferrable = true } }

// SearchToolName is the tool the model calls to load deferred tools.
const SearchToolName = "tool_search"

const defaultDiscoveryThreshold = 16 << 10

// discovery is the state of one run: which tools are deferred and which
// of them the model has loaded so far in this conversation.
type discovery struct {
	deferred map[string]ToolSpec
	order    []string
	mu       sync.Mutex // tool_search is read-only, so it may run in parallel
	loaded   map[string]bool
}

// newDiscovery returns nil when every schema is sent as usual.
func newDiscovery(cfg Config, history []Message) *discovery {
	if cfg.Tools == nil || cfg.Discovery == DiscoveryOff {
		return nil
	}
	d := &discovery{deferred: map[string]ToolSpec{}, loaded: map[string]bool{}}
	size := d.collect(cfg.Tools.Specs())
	threshold := cfg.DiscoveryThreshold
	if threshold <= 0 {
		threshold = defaultDiscoveryThreshold
	}
	if len(d.deferred) == 0 || (cfg.Discovery == DiscoveryAuto && size <= threshold) {
		return nil
	}
	sort.Strings(d.order)
	d.restore(history)
	return d
}

// collect takes the deferrable tools and returns the size of their
// schemas and descriptions.
func (d *discovery) collect(specs []ToolSpec) int {
	size := 0
	for _, s := range specs {
		if s.Annotations.Deferrable && s.Name != SearchToolName {
			d.deferred[s.Name] = s
			d.order = append(d.order, s.Name)
			size += len(s.InputSchema) + len(s.Description)
		}
	}
	return size
}

// restore finds the tools loaded earlier in the conversation, from the
// results of tool_search calls in the stored history (compaction changes
// only what is sent, so they survive it).
func (d *discovery) restore(history []Message) {
	searches := map[string]bool{}
	for _, m := range history {
		for _, c := range m.Calls {
			if c.Name == SearchToolName {
				searches[c.ID] = true
			}
		}
		if m.Result != nil && searches[m.Result.CallID] && !m.Result.IsError {
			var out searchOut
			if json.Unmarshal([]byte(m.Result.Content), &out) == nil {
				d.mark(out.Loaded)
			}
		}
	}
}

func (d *discovery) mark(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range names {
		if _, ok := d.deferred[n]; ok {
			d.loaded[n] = true
		}
	}
}

// specs are what the model gets: every tool not deferred, the deferred
// ones it loaded, and tool_search.
func (d *discovery) specs(all []ToolSpec) []ToolSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]ToolSpec, 0, len(all)-len(d.deferred)+len(d.loaded)+1)
	for _, s := range all {
		if _, deferred := d.deferred[s.Name]; !deferred || d.loaded[s.Name] {
			out = append(out, s)
		}
	}
	return append(out, d.searchTool().Spec())
}

// Bounds of the deferred list in the system prompt: with hints while they
// fit, names only beyond, and a count past the end.
const (
	blockWithHints = 24 << 10
	blockMax       = 48 << 10
)

// block lists the deferred tools for the system prompt, one JSON object
// per line, so that no description can break out of the block.
func (d *discovery) block() string {
	var b strings.Builder
	b.WriteString("\n\n<deferred-tools>\nMore tools exist, listed by name only. To use one, first call " + SearchToolName +
		" with its exact names (or with words describing the task); its schema then becomes available.\n")
	hints := d.listSize(true) <= blockWithHints
	for i, n := range d.order {
		hint := ""
		if hints {
			hint = firstLine(d.deferred[n].Description, 100)
		}
		line, err := json.Marshal(struct {
			Name string `json:"name"`
			Hint string `json:"hint,omitempty"`
		}{n, hint})
		if err != nil {
			continue
		}
		if b.Len()+len(line) > blockMax {
			b.WriteString(`{"more":` + strconv.Itoa(len(d.order)-i) + `,"note":"search by words to find them"}` + "\n")
			break
		}
		b.Write(line)
		b.WriteString("\n")
	}
	b.WriteString("</deferred-tools>")
	return b.String()
}

// listSize estimates the list with or without hints.
func (d *discovery) listSize(hints bool) int {
	n := 0
	for _, name := range d.order {
		n += len(name) + 14
		if hints {
			n += len(firstLine(d.deferred[name].Description, 100)) + 10
		}
	}
	return n
}

// unknown is the answer to a call of a tool that does not exist: a
// pointer to tool_search, not the whole deferred catalog.
func (d *discovery) unknown(call ToolCall) ToolResult {
	return ToolResult{CallID: call.ID, IsError: true,
		Content: "No tool named " + strconv.Quote(clipText(call.Name, 64)) + ". Use " + SearchToolName + " to find the tool you need. [" + string(ErrToolUnknown) + "]"}
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clipText(s, n)
}

type searchIn struct {
	Names []string `json:"names,omitempty" desc:"Exact names of tools to load, as listed in <deferred-tools>"`
	Query string   `json:"query,omitempty" desc:"Words describing what you need, when you do not know the names"`
}

type searchOut struct {
	Loaded []string `json:"loaded"`
	Note   string   `json:"note"`
}

func (d *discovery) searchTool() Tool {
	return NewTool(SearchToolName, "Load deferred tools so you can call them: by exact name, or by a description of the task.",
		func(_ context.Context, in searchIn) (string, error) {
			names := d.find(in)
			d.mark(names)
			out := searchOut{Loaded: names, Note: "These tools are now available to call."}
			if len(names) == 0 {
				out.Note = "No deferred tool matches; try other words or the exact names listed in <deferred-tools>."
			}
			data, err := json.Marshal(out)
			return string(data), err
		}, ReadOnly())
}

// find returns the exact names asked for, or the five tools whose names
// and descriptions share the most words with the query.
func (d *discovery) find(in searchIn) []string {
	var names []string
	for _, n := range in.Names {
		if _, ok := d.deferred[n]; ok {
			names = append(names, n)
		}
	}
	if len(names) > 0 || in.Query == "" {
		return names
	}
	return d.rank(words(in.Query), 5)
}

// rank scores tools by the query words they contain, rare words counting
// more; words found in most tools ("the", "tool") count for nothing, and
// a plural matches its singular. Only tools scoring at least half the
// best one are returned.
func (d *discovery) rank(query []string, limit int) []string {
	docs, freq := d.index()
	type hit struct {
		name  string
		score float64
	}
	var hits []hit
	best := 0.0
	for i, n := range d.order {
		if score := d.score(docs[i], freq, uniqueWords(query)); score > 0 {
			hits = append(hits, hit{n, score})
			best = max(best, score)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []string
	for _, h := range hits {
		if len(out) == limit || h.score < best/2 {
			break
		}
		out = append(out, h.name)
	}
	return out
}

// index returns each tool's stemmed words and how many tools hold each.
func (d *discovery) index() ([]map[string]bool, map[string]int) {
	docs := make([]map[string]bool, len(d.order))
	freq := map[string]int{}
	for i, n := range d.order {
		docs[i] = map[string]bool{}
		for _, w := range words(n + " " + d.deferred[n].Description) {
			docs[i][stem(w)] = true
		}
		for w := range docs[i] {
			freq[w]++
		}
	}
	return docs, freq
}

func (d *discovery) score(doc map[string]bool, freq map[string]int, query []string) float64 {
	score := 0.0
	for _, w := range query {
		if f := freq[w]; doc[w] && f*2 <= len(d.order) {
			score += 1 / float64(f)
		}
	}
	return score
}

func uniqueWords(ws []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range ws {
		if w = stem(w); !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// stem drops a plural ending, enough for "emails" to find "email".
func stem(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

// words splits text into lower-cased words; names split at _ and -.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}
