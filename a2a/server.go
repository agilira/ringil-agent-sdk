// server.go: a served profile as an A2A agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package a2a

import (
	"context"
	"iter"
	"net/http"
	"sync"

	proto "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// Card describes the agent to its peers.
type Card struct {
	Name, Description, Version string
	// URL is where peers reach the agent: the endpoint this handler is
	// mounted on, as they see it.
	URL    string
	Skills []Skill
}

// Skill is one thing the agent offers to do.
type Skill struct {
	ID, Name, Description string
}

// Path is where Expose answers JSON-RPC; the card is at the well-known
// path.
const Path = "/a2a"

// Expose makes a profile of srv an A2A agent: its card at the well-known
// path and JSON-RPC at Path, behind srv's guard (loopback, bearer token).
// Each A2A task is a run of srv, so limits, approvals through the API,
// the event log and cancellation are the same; an A2A context is a
// conversation when srv keeps them.
func Expose(srv *serve.Server, profile string, card Card) (http.Handler, error) {
	if card.Name == "" || card.URL == "" {
		return nil, errors.New(ErrConfig, "the card needs a name and the URL peers use")
	}
	known := false
	for _, p := range srv.Profiles() {
		known = known || p.Name == profile
	}
	if !known {
		return nil, errors.New(ErrConfig, "no such profile: "+profile)
	}
	pc := &proto.AgentCard{Name: card.Name, Description: card.Description, Version: card.Version,
		DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		SupportedInterfaces: []*proto.AgentInterface{proto.NewAgentInterface(card.URL, proto.TransportProtocolJSONRPC)}}
	for _, s := range card.Skills {
		pc.Skills = append(pc.Skills, proto.AgentSkill{ID: s.ID, Name: s.Name, Description: s.Description})
	}
	ex := &executor{srv: srv, profile: profile, runs: map[proto.TaskID]string{}}
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(pc))
	mux.Handle(Path, a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(ex)))
	return srv.Guard(mux), nil
}

type executor struct {
	srv     *serve.Server
	profile string
	mu      sync.Mutex
	runs    map[proto.TaskID]string // A2A task -> run of srv
}

// Execute runs the message as a run of the profile and reports its end.
func (e *executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[proto.Event, error] {
	return func(yield func(proto.Event, error) bool) {
		if ec.StoredTask == nil && !yield(proto.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(proto.NewStatusUpdateEvent(ec, proto.TaskStateWorking, nil), nil) {
			return
		}
		snap, err := e.run(ctx, ec)
		for _, ev := range outcome(ec, snap, err) {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// run starts the run and waits for its end; cancelling ctx cancels it.
func (e *executor) run(ctx context.Context, ec *a2asrv.ExecutorContext) (serve.Snapshot, error) {
	conv := ""
	if id := "a2a-" + ec.ContextID; ec.ContextID != "" && agent.ValidateSessionID(id) == nil {
		conv = id
	}
	snap, err := e.srv.Start(e.profile, conv, messageText(ec.Message))
	if errors.HasCode(err, serve.ErrInput) && conv != "" {
		snap, err = e.srv.Start(e.profile, "", messageText(ec.Message)) // the server keeps no conversations
	}
	if err != nil {
		return serve.Snapshot{}, err
	}
	e.mu.Lock()
	e.runs[ec.TaskID] = snap.ID
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.runs, ec.TaskID)
		e.mu.Unlock()
	}()
	final, err := e.srv.Wait(ctx, snap.ID)
	if err != nil {
		_ = e.srv.Cancel(snap.ID) // the peer went: the run stops too
	}
	return final, err
}

// outcome is how a run's end reads in A2A.
func outcome(ec *a2asrv.ExecutorContext, s serve.Snapshot, err error) []proto.Event {
	say := func(text string) *proto.Message {
		return proto.NewMessage(proto.MessageRoleAgent, proto.NewTextPart(text))
	}
	switch {
	case err != nil:
		return []proto.Event{proto.NewStatusUpdateEvent(ec, proto.TaskStateFailed, say(agent.ModelMessage(err)))}
	case s.State == serve.Completed:
		return []proto.Event{proto.NewArtifactEvent(ec, proto.NewTextPart(s.Text)), proto.NewStatusUpdateEvent(ec, proto.TaskStateCompleted, nil)}
	case s.State == serve.Cancelled:
		return []proto.Event{proto.NewStatusUpdateEvent(ec, proto.TaskStateCanceled, nil)}
	}
	why := "the run failed"
	if s.Error != nil {
		why = s.Error.Message
	}
	return []proto.Event{proto.NewStatusUpdateEvent(ec, proto.TaskStateFailed, say(why))}
}

// Cancel stops the task's run.
func (e *executor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[proto.Event, error] {
	return func(yield func(proto.Event, error) bool) {
		e.mu.Lock()
		id, ok := e.runs[ec.TaskID]
		e.mu.Unlock()
		if ok {
			_ = e.srv.Cancel(id)
		}
		yield(proto.NewStatusUpdateEvent(ec, proto.TaskStateCanceled, nil), nil)
	}
}
