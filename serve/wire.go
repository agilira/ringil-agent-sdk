// wire.go: events as the API sends them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"encoding/json"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
)

// wireEvent is one event of a run on the API. ID numbers the run's
// events for resuming a stream (Last-Event-ID). Agent events keep their
// kind and carry their content: the API's caller owns the run. The
// server's own kinds are "approval", "approval_decided", "artifact" (a
// new version of one, its description) and "run_end".
type wireEvent struct {
	ID     int              `json:"id"`
	Kind   string           `json:"kind"`
	Run    string           `json:"run,omitzero"`
	Seq    int              `json:"seq,omitzero"`
	Time   time.Time        `json:"time,omitzero"`
	Parent string           `json:"parent,omitzero"`
	Worker string           `json:"worker,omitzero"`
	Task   int              `json:"task,omitzero"`
	MS     int64            `json:"ms,omitzero"`
	Text   string           `json:"text,omitzero"`
	Call   *wireCall        `json:"call,omitzero"`
	Output *wireOutput      `json:"output,omitzero"`
	Error  *ErrorInfo       `json:"error,omitzero"`
	Usage  agent.Usage      `json:"usage,omitzero"`
	Stop   agent.StopReason `json:"stop,omitzero"`
	// Model is the model that answered (a model event; with fallback, the
	// one that stood in).
	Model    string         `json:"model,omitzero"`
	Turns    int            `json:"turns,omitzero"`
	State    string         `json:"state,omitzero"`
	Approval *Approval      `json:"approval,omitzero"`
	Decision *Decision      `json:"decision,omitzero"`
	Artifact *artifact.Info `json:"artifact,omitzero"`
}

type wireCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitzero"`
}

type wireOutput struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitzero"`
}

func wire(ev agent.Event) wireEvent {
	w := wireEvent{Kind: string(ev.Kind), Run: ev.Run, Seq: ev.Seq, Time: ev.Time, Parent: ev.Parent,
		Worker: ev.Worker, Task: ev.Task, MS: ev.Duration.Milliseconds(), Text: ev.Text, Usage: ev.Usage, Stop: ev.Stop, Model: ev.Model}
	if ev.Call != nil {
		w.Call = &wireCall{ID: ev.Call.ID, Name: ev.Call.Name, Input: safeJSON(ev.Call.Input)}
	}
	if ev.Output != nil {
		w.Output = &wireOutput{Content: ev.Output.Content, IsError: ev.Output.IsError}
	}
	if ev.Err != nil {
		w.Error = errorInfo(ev.Err)
	}
	if ev.Kind == agent.EventDone && ev.Result != nil {
		w.Text, w.Turns, w.Usage = ev.Result.Text, ev.Result.Turns, ev.Result.Usage
	}
	return w
}
