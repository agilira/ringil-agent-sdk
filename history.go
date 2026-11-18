// history.go: reading the original messages that compaction left out
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"strconv"
	"strings"
)

// HistoryToolName is the tool the model calls to read messages that
// compaction summarized or shortened: they stay whole in the
// conversation, only the request leaves them out.
const HistoryToolName = "read_history"

// maxHistory bounds one read: the point is a precise look, not the whole
// past back into the context.
const (
	maxHistory     = 32 << 10
	maxHistoryPart = 4 << 10
)

type historyIn struct {
	From int `json:"from" min:"1" desc:"First message number, as given by the summary or an omitted result"`
	To   int `json:"to,omitempty" min:"1" desc:"Last message number; default from"`
}

// historyTool reads the run's own conversation.
func (r *run) historyTool() Tool {
	return NewTool(HistoryToolName, "Read earlier messages of this conversation that were summarized or omitted to fit the context, by their numbers.",
		func(_ context.Context, in historyIn) (string, error) {
			return renderHistory(r.history(), in.From, max(in.To, in.From)), nil
		}, ReadOnly())
}

// history is a copy of the messages so far, safe to read while the run
// goes on.
func (r *run) history() []Message {
	r.histMu.Lock()
	defer r.histMu.Unlock()
	return append([]Message(nil), r.msgs...)
}

func renderHistory(msgs []Message, from, to int) string {
	if from < 1 || from > len(msgs) {
		return "No message " + strconv.Itoa(from) + ": the conversation has " + strconv.Itoa(len(msgs)) + "."
	}
	to = min(to, len(msgs))
	var b strings.Builder
	for n := from; n <= to; n++ {
		line := "#" + strconv.Itoa(n) + " " + describeMessage(msgs[n-1]) + "\n"
		if b.Len()+len(line) > maxHistory {
			b.WriteString("[stopped at message " + strconv.Itoa(n) + ": ask for the rest from there]\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func describeMessage(m Message) string {
	var parts []string
	if t := m.Text(); t != "" {
		parts = append(parts, clipText(t, maxHistoryPart))
	}
	for _, c := range m.Calls {
		parts = append(parts, "[called "+c.Name+" "+clipText(string(c.Input), 512)+"]")
	}
	if m.Result != nil {
		mark := "result"
		if m.Result.IsError {
			mark = "error"
		}
		parts = append(parts, "["+mark+"] "+clipText(m.Result.Content, maxHistoryPart))
	}
	return string(m.Role) + ": " + strings.Join(parts, " ")
}
