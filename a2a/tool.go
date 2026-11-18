// tool.go: the remote agent as one tool
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package a2a

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	proto "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type askIn struct {
	Message   string `json:"message" desc:"What to ask or tell the remote agent, with everything it needs to know"`
	ContextID string `json:"context_id,omitempty" desc:"context_id of an earlier answer, to continue that conversation"`
	TaskID    string `json:"task_id,omitempty" desc:"task_id of a task waiting for your answer"`
}

// Tool is the remote agent as a tool named after Remote.Name. It counts
// as destructive and open-world: a remote agent may act on what it is
// asked, so an approval hook sees every call.
func (a *Agent) Tool() agent.Tool {
	return agent.NewTool(a.remote.Name, a.description(), a.ask, agent.Destructive(), agent.OpenWorld(), agent.Deferrable())
}

func (a *Agent) ask(ctx context.Context, in askIn) (string, error) {
	if strings.TrimSpace(in.Message) == "" {
		return "", agent.ToolError(agent.ErrToolInput, "The message is empty.")
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	msg := proto.NewMessage(proto.MessageRoleUser, proto.NewTextPart(in.Message))
	// Only IDs the remote gave: a model that invents one (or copies a
	// placeholder) would get a refusal and stall; a new conversation
	// starts instead, and the model is told.
	note := a.vet(&in)
	msg.ContextID = in.ContextID
	msg.TaskID = proto.TaskID(in.TaskID)
	// Return at once with the task, then follow it: the wait is ours to
	// bound and to cancel, not held open on one request.
	req := &proto.SendMessageRequest{Message: msg, Config: &proto.SendMessageConfig{ReturnImmediately: true}}
	res, err := a.client.SendMessage(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return "", agent.ToolError(ErrCall, "The remote agent did not answer in time.")
		}
		return "", errors.Wrap(err, ErrCall, "the remote agent did not answer").WithUserMessage("The remote agent did not answer.")
	}
	switch v := res.(type) {
	case *proto.Message:
		return a.cap(orEmpty(messageText(v)) + a.footer(v.ContextID, string(v.TaskID), "") + note), nil
	case *proto.Task:
		out, err := a.finish(ctx, v)
		return out + note, err
	}
	return "", agent.ToolError(ErrCall, "The remote agent sent an answer of an unknown kind.")
}

// finish waits for the task to end or to need the user, polling, and
// cancels it if the wait runs out.
func (a *Agent) finish(ctx context.Context, task *proto.Task) (string, error) {
	for !settled(task.Status.State) {
		select {
		case <-ctx.Done():
			a.cancel(task.ID)
			return "", agent.ToolError(ErrCall, "The remote agent did not finish in time; the task was cancelled.")
		case <-time.After(a.poll()):
		}
		next, err := a.client.GetTask(ctx, &proto.GetTaskRequest{ID: task.ID})
		if err != nil && ctx.Err() != nil {
			a.cancel(task.ID)
			return "", agent.ToolError(ErrCall, "The remote agent did not finish in time; the task was cancelled.")
		}
		if err != nil || next == nil {
			return "", errors.Wrap(err, ErrCall, "cannot follow the remote task").WithUserMessage("The remote agent stopped answering.")
		}
		task = next
	}
	text := a.cap(orEmpty(taskText(task)) + a.footer(task.ContextID, string(task.ID), string(task.Status.State)))
	switch task.Status.State {
	case proto.TaskStateFailed, proto.TaskStateRejected, proto.TaskStateCanceled:
		return "", agent.ToolError(ErrCall, text)
	}
	return text, nil
}

// settled: the task ended, or waits for the user.
func settled(s proto.TaskState) bool {
	return s.Terminal() || s == proto.TaskStateInputRequired || s == proto.TaskStateAuthRequired
}

func (a *Agent) cancel(id proto.TaskID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.client.CancelTask(ctx, &proto.CancelTaskRequest{ID: id}); err != nil {
		return // best effort: the remote may have finished meanwhile
	}
}

func messageText(m *proto.Message) string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, p := range m.Parts {
		if p == nil {
			continue
		}
		if _, isText := p.Content.(proto.Text); isText {
			if t := p.Text(); t != "" {
				parts = append(parts, t)
			}
		} else {
			parts = append(parts, "[non-text content not shown]")
		}
	}
	return strings.Join(parts, "\n")
}

// taskText is the task's results, then its last status message.
func taskText(t *proto.Task) string {
	var out []string
	for _, art := range t.Artifacts {
		if art == nil {
			continue
		}
		if s := messageText(&proto.Message{Parts: art.Parts}); s != "" {
			out = append(out, s)
		}
	}
	if s := messageText(t.Status.Message); s != "" {
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(the remote agent answered with no text)"
	}
	return s
}

// safeID accepts the identifiers the remote sends back only in a plain
// form: they are written into the answer the model reads, so they must
// not carry lines or brackets of their own.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,128}$`)

// footer shows the IDs to the model and remembers them as the remote's.
func (a *Agent) footer(contextID, taskID, state string) string {
	a.mu.Lock()
	if a.seen == nil || len(a.seen) > 10000 {
		a.seen = map[string]bool{}
	}
	for _, id := range []string{contextID, taskID} {
		if safeID.MatchString(id) {
			a.seen[id] = true
		}
	}
	a.mu.Unlock()
	return footer(contextID, taskID, state)
}

// vet drops the IDs the remote never gave and says so.
func (a *Agent) vet(in *askIn) string {
	note := ""
	if in.ContextID != "" && !a.known(in.ContextID) {
		in.ContextID, note = "", "\n[context_id ignored: it does not come from an earlier answer; this is a new conversation]"
	}
	if in.TaskID != "" && !a.known(in.TaskID) {
		in.TaskID, note = "", note+"\n[task_id ignored: it does not come from an earlier answer]"
	}
	return note
}

func (a *Agent) known(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[id]
}

func footer(contextID, taskID, state string) string {
	var f []string
	if safeID.MatchString(contextID) {
		f = append(f, "context_id: "+contextID)
	}
	if safeID.MatchString(taskID) {
		f = append(f, "task_id: "+taskID)
	}
	if state != "" {
		f = append(f, "state: "+strings.ToLower(strings.TrimPrefix(state, "TASK_STATE_")))
	}
	if len(f) == 0 {
		return ""
	}
	return "\n[" + strings.Join(f, ", ") + "]"
}

func (a *Agent) cap(s string) string {
	n := a.remote.MaxOutput
	if n <= 0 {
		n = 256 << 10
	}
	if len(s) <= n {
		return s
	}
	return clip(s, n) + "\n[" + strconv.Itoa(len(s)-n) + " more bytes not shown]"
}

func (a *Agent) timeout() time.Duration {
	if a.remote.Timeout > 0 {
		return a.remote.Timeout
	}
	return 10 * time.Minute
}

func (a *Agent) poll() time.Duration {
	if a.remote.Poll > 0 {
		return a.remote.Poll
	}
	return time.Second
}
