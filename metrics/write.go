// write.go: the counts in Prometheus's text format
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package metrics

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Handler serves the counts in Prometheus's text format (0.0.4). Mount
// it behind the host's own authentication: the counts tell what the
// agents are doing.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = m.WriteTo(w) // #nosec G104 -- a scrape cut short is retried by Prometheus
	})
}

// WriteTo writes the counts in Prometheus's text format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	var b bytes.Buffer
	m.mu.Lock()
	m.write(&b)
	m.mu.Unlock()
	return b.WriteTo(w)
}

func (m *Metrics) write(b *bytes.Buffer) {
	family(b, "ringil_start_time_seconds", "gauge", "When the process started, as a Unix time.")
	sample(b, "ringil_start_time_seconds", nil, float64(m.start.UnixNano())/float64(time.Second))
	names := make([]string, 0, len(m.profiles))
	for p := range m.profiles {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, f := range families {
		family(b, f.name, f.kind, f.help)
		for _, p := range names {
			f.write(b, p, m.profiles[p])
		}
	}
}

type metricFamily struct {
	name, kind, help string
	write            func(b *bytes.Buffer, profile string, c *counts)
}

var families = []metricFamily{
	{"ringil_runs_started_total", "counter", "Agent runs started: one per turn of a conversation (delegated tasks apart).", func(b *bytes.Buffer, p string, c *counts) {
		sample(b, "ringil_runs_started_total", []string{"profile", p}, float64(c.started))
	}},
	{"ringil_runs_active", "gauge", "Runs going on.", func(b *bytes.Buffer, p string, c *counts) {
		sample(b, "ringil_runs_active", []string{"profile", p}, float64(len(c.active)))
	}},
	{"ringil_runs_finished_total", "counter", "Runs ended: done with its stop reason, or failed with its error code.", func(b *bytes.Buffer, p string, c *counts) {
		keys := make([]outcome, 0, len(c.finished))
		for o := range c.finished {
			keys = append(keys, o)
		}
		sort.Slice(keys, func(i, j int) bool {
			return keys[i].status+"\x00"+keys[i].reason < keys[j].status+"\x00"+keys[j].reason
		})
		for _, o := range keys {
			sample(b, "ringil_runs_finished_total", []string{"profile", p, "status", o.status, "reason", o.reason}, float64(c.finished[o]))
		}
	}},
	{"ringil_delegated_tasks_total", "counter", "Worker runs of delegated tasks (a task continued runs again).", func(b *bytes.Buffer, p string, c *counts) {
		sample(b, "ringil_delegated_tasks_total", []string{"profile", p}, float64(c.delegated))
	}},
	{"ringil_model_calls_total", "counter", "Model calls answered (a failed call ends its run: ringil_runs_finished_total).", func(b *bytes.Buffer, p string, c *counts) {
		sample(b, "ringil_model_calls_total", []string{"profile", p}, float64(c.modelCalls))
	}},
	{"ringil_model_call_seconds", "histogram", "How long model calls took.", func(b *bytes.Buffer, p string, c *counts) {
		writeHistogram(b, "ringil_model_call_seconds", p, &c.model)
	}},
	{"ringil_tokens_total", "counter", "Tokens: input (the whole prompt, cache included), output, cache_read, cache_write.", func(b *bytes.Buffer, p string, c *counts) {
		for _, t := range []struct {
			kind string
			n    int
		}{{"input", c.tokens.Input}, {"output", c.tokens.Output}, {"cache_read", c.tokens.CacheRead}, {"cache_write", c.tokens.CacheWrite}} {
			sample(b, "ringil_tokens_total", []string{"profile", p, "kind", t.kind}, float64(t.n))
		}
	}},
	{"ringil_cost_usd_total", "counter", "What the model calls cost in US dollars, for the models with a price (package price).", func(b *bytes.Buffer, p string, c *counts) {
		sample(b, "ringil_cost_usd_total", []string{"profile", p}, c.tokens.Cost)
	}},
	{"ringil_compactions_total", "counter", "Model calls sent a shortened conversation.", func(b *bytes.Buffer, p string, c *counts) {
		sample(b, "ringil_compactions_total", []string{"profile", p}, float64(c.compactions))
	}},
	{"ringil_loops_total", "counter", "Repetitions caught, by what was done.", func(b *bytes.Buffer, p string, c *counts) {
		for _, a := range sortedKeys(c.loops) {
			sample(b, "ringil_loops_total", []string{"profile", p, "action", a}, float64(c.loops[a]))
		}
	}},
	{"ringil_tool_calls_total", "counter", "Tool calls, by tool and status (error: the call failed or was refused).", func(b *bytes.Buffer, p string, c *counts) {
		keys := make([]toolOutcome, 0, len(c.tools))
		for k := range c.tools {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].name+"\x00"+keys[i].status < keys[j].name+"\x00"+keys[j].status })
		for _, k := range keys {
			sample(b, "ringil_tool_calls_total", []string{"profile", p, "tool", k.name, "status", k.status}, float64(c.tools[k]))
		}
	}},
	{"ringil_tool_call_seconds", "histogram", "How long tool calls took.", func(b *bytes.Buffer, p string, c *counts) {
		writeHistogram(b, "ringil_tool_call_seconds", p, &c.tool)
	}},
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func family(b *bytes.Buffer, name, kind, help string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteString("\n# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(kind)
	b.WriteByte('\n')
}

func writeHistogram(b *bytes.Buffer, name, profile string, h *histogram) {
	var cum int64
	for i, bound := range h.bounds {
		cum += h.counts[i]
		sample(b, name+"_bucket", []string{"profile", profile, "le", strconv.FormatFloat(bound, 'g', -1, 64)}, float64(cum))
	}
	sample(b, name+"_bucket", []string{"profile", profile, "le", "+Inf"}, float64(h.n))
	sample(b, name+"_sum", []string{"profile", profile}, h.sum)
	sample(b, name+"_count", []string{"profile", profile}, float64(h.n))
}

// sample writes one line; labels are name, value pairs.
func sample(b *bytes.Buffer, name string, labels []string, v float64) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escape(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	b.WriteByte('\n')
}

// escape a label value as the format wants: \\, \" and \n; and invalid
// UTF-8 replaced, which a scraper would refuse.
func escape(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
