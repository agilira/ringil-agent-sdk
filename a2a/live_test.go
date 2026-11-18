// live_test.go: two agents with a real model, talking over A2A
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package a2a

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	proto "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

func liveProvider(t *testing.T) agent.Provider {
	t.Helper()
	base, model := os.Getenv("RINGIL_BASE_URL"), os.Getenv("RINGIL_MODEL")
	if base == "" || model == "" {
		t.Skip("RINGIL_BASE_URL and RINGIL_MODEL not set")
	}
	p, err := openai.New(openai.Config{BaseURL: base, Model: model, APIKey: os.Getenv("RINGIL_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// agentExecutor serves an SDK agent over A2A: each message is one run.
type agentExecutor struct{ a agent.Agent }

func (e agentExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[proto.Event, error] {
	return func(yield func(proto.Event, error) bool) {
		res, err := e.a.Run(ctx, nil, agent.UserText(ec.Message.Parts[0].Text()))
		if err != nil {
			yield(nil, err)
			return
		}
		yield(proto.NewMessage(proto.MessageRoleAgent, proto.NewTextPart(res.Text)), nil)
	}
}

func (agentExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[proto.Event, error] {
	return func(func(proto.Event, error) bool) {}
}

func TestLiveAgentToAgent(t *testing.T) {
	p := liveProvider(t)
	// The remote agent knows a fact the asking agent does not.
	remote, err := agent.New(agent.Config{Provider: p, System: "You are the warehouse agent. Stock of item AX-7: 314 units in Bologna. Answer briefly."})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	card := &proto.AgentCard{Name: "Warehouse", Description: "Knows stock levels of every item.", Version: "1",
		SupportedInterfaces: []*proto.AgentInterface{proto.NewAgentInterface(srv.URL, proto.TransportProtocolJSONRPC)}}
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(agentExecutor{remote})))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	wh, err := Connect(ctx, Remote{Name: "warehouse", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := wh.Close(); err != nil {
			t.Log(err)
		}
	}()
	tools, err := agent.NewRegistry(wh.Tool())
	if err != nil {
		t.Fatal(err)
	}
	asker, err := agent.New(agent.Config{Provider: p, Tools: tools, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	res, err := asker.Run(ctx, nil, agent.UserText("Quante unità dell'articolo AX-7 abbiamo in magazzino? Chiedi all'agente del magazzino."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(res.Text, ".", ""), "314") {
		t.Fatalf("answer %q", res.Text)
	}
}
