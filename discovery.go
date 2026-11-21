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

// specs are what the model gets: every tool not deferred, a light entry
// (name, one line, any object) for each deferred one, and tool_search. A
// loaded tool keeps its entry: its parameters come in tool_search's
// result, as mature agents do, so the list never changes and the
// provider's cache keeps it (21/11: each load swapped the entry for the
// full tool, and the list, the system prompt and the history went out of
// the cache with it). The entries matter: a provider may drop a call to a
// name missing from the list, and models call tools by name. Calling an
// entry runs the real tool, whose schema still checks the arguments. Past
// maxEntries, a tool is listed only once loaded.
func (d *discovery) specs(all []ToolSpec) []ToolSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]ToolSpec, 0, len(all)+1)
	entries := 0
	for _, s := range all {
		_, deferred := d.deferred[s.Name]
		switch {
		case !deferred:
			out = append(out, s)
		case entries < maxEntries:
			out = append(out, lightEntry(s))
			entries++
		case d.loaded[s.Name]:
			out = append(out, s)
		}
	}
	return append(out, d.searchTool().Spec())
}

// maxEntries bounds the light entries; tools past it are found only by
// tool_search.
const maxEntries = 300

func lightEntry(s ToolSpec) ToolSpec {
	return ToolSpec{
		Name:        s.Name,
		Description: firstLine(s.Description, 100) + " [Parameters not shown: call " + SearchToolName + " with this name first.]",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: s.Annotations,
	}
}

// block explains the light entries in the system prompt.
func (d *discovery) block() string {
	text := "\n\n<deferred-tools>\nSome tools are listed with one line only and no parameters. Before using one, call " + SearchToolName + " with its exact name: its result gives the tool's full parameters, to call it with."
	if n := len(d.order) - maxEntries; n > 0 {
		text += " " + strconv.Itoa(n) + " more tools are not listed at all: find them with " + SearchToolName + " by describing the task."
	}
	return text + "\n</deferred-tools>"
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
	Loaded []string     `json:"loaded"`
	Tools  []loadedTool `json:"tools,omitempty"`
	Note   string       `json:"note"`
}

// loadedTool is a loaded tool's definition, for the model to call it by.
type loadedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (d *discovery) searchTool() Tool {
	return NewTool(SearchToolName, "Load deferred tools so you can call them: by exact name, or by a description of the task.",
		func(_ context.Context, in searchIn) (string, error) {
			names := d.find(in)
			d.mark(names)
			out := searchOut{Loaded: names, Tools: d.definitions(names), Note: "Call these tools with the parameters given here."}
			if len(names) == 0 {
				out.Note = "No deferred tool matches; try other words or the exact names listed in <deferred-tools>."
			}
			data, err := json.Marshal(out)
			return string(data), err
		}, ReadOnly())
}

// definitions are the full definitions of names.
func (d *discovery) definitions(names []string) []loadedTool {
	out := make([]loadedTool, 0, len(names))
	for _, n := range names {
		s := d.deferred[n]
		schema := s.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, loadedTool{Name: n, Description: s.Description, Parameters: schema})
	}
	return out
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
