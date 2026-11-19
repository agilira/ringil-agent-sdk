// history.go: reading the original messages that compaction left out
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
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

// renderHistory writes one JSON object per message: a line of a tool
// result can never pose as another message, whatever text it holds. The
// first message is always shown, clipped if need be, so a read always
// moves forward.
func renderHistory(msgs []Message, from, to int) string {
	if from < 1 || from > len(msgs) {
		return "No message " + strconv.Itoa(from) + ": the conversation has " + strconv.Itoa(len(msgs)) + "."
	}
	to = min(to, len(msgs))
	var b strings.Builder
	for n := from; n <= to; n++ {
		line := encodeMessage(n, msgs[n-1], maxHistoryPart)
		if n > from && b.Len()+len(line) > maxHistory {
			b.WriteString(`{"stopped_at":` + strconv.Itoa(n) + `,"note":"ask again from this number"}` + "\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

type historyLine struct {
	N      int      `json:"n"`
	Role   Role     `json:"role"`
	Text   string   `json:"text,omitempty"`
	Images int      `json:"images,omitempty"`
	Calls  []string `json:"calls,omitempty"`
	Result string   `json:"result,omitempty"`
	Error  bool     `json:"error,omitempty"`
}

func encodeMessage(n int, m Message, part int) string {
	h := historyLine{N: n, Role: m.Role, Text: clipText(m.Text(), part), Images: m.Images()}
	for i, c := range m.Calls {
		if i == 16 {
			h.Calls = append(h.Calls, "... "+strconv.Itoa(len(m.Calls)-16)+" more calls")
			break
		}
		h.Calls = append(h.Calls, c.Name+" "+clipText(string(c.Input), 512))
	}
	if m.Result != nil {
		h.Result, h.Error = clipText(m.Result.Content, part), m.Result.IsError
	}
	data, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(data) + "\n"
}
