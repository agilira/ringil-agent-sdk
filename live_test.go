// live_test.go: typed tools against a real model
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package agent_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/budget"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
)

type convertIn struct {
	Amount float64 `json:"amount" desc:"Amount to convert" min:"0"`
	From   string  `json:"from" enum:"EUR,USD" desc:"Source currency"`
	To     string  `json:"to" enum:"EUR,USD" desc:"Target currency"`
}

func liveModel(t *testing.T) agent.Provider {
	t.Helper()
	base, model := os.Getenv("RINGIL_BASE_URL"), os.Getenv("RINGIL_MODEL")
	if base == "" || model == "" {
		t.Skip("RINGIL_BASE_URL and RINGIL_MODEL not set")
	}
	p, err := openai.New(openai.Config{BaseURL: base, Model: model, APIKey: os.Getenv("RINGIL_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	spend, err := budget.New(budget.Limits{Calls: 6})
	if err != nil {
		t.Fatal(err)
	}
	return retry.Wrap(spend.Wrap(p), retry.Config{})
}

// TestLiveTypedTool lets a real model call a NewTool through the registry:
// the generated schema must be one the model can follow, and the result
// must flow back into its answer.
func TestLiveTypedTool(t *testing.T) {
	model := liveModel(t)
	var seen []convertIn
	convert := agent.NewTool("convert_currency", "Convert an amount between EUR and USD at today's rate",
		func(_ context.Context, in convertIn) (string, error) {
			seen = append(seen, in)
			return "rate 1.25, result " + strconv.FormatFloat(in.Amount*1.25, 'f', -1, 64), nil
		}, agent.ReadOnly())
	tools, err := agent.NewRegistry(convert)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := &agent.Request{Messages: []agent.Message{agent.UserText("Convert 100 EUR to USD with the tool.")}, Tools: tools.Specs()}
	answer := runTools(ctx, t, model, tools, req)
	if len(seen) == 0 || seen[0].Amount != 100 || seen[0].From != "EUR" || seen[0].To != "USD" {
		t.Fatalf("tool saw %+v", seen)
	}
	if !strings.Contains(answer, "125") {
		t.Fatalf("result not used: %q", answer)
	}
}

// runTools is a minimal loop, enough to exercise the registry; the real
// agent loop comes with N3.
func runTools(ctx context.Context, t *testing.T, model agent.Provider, tools agent.Registry, req *agent.Request) string {
	t.Helper()
	for range 3 {
		resp, err := model.Generate(ctx, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Messages = append(req.Messages, resp.Message)
		if len(resp.Message.Calls) == 0 {
			return resp.Message.Text()
		}
		for _, call := range resp.Message.Calls {
			res, err := tools.Execute(ctx, call)
			t.Logf("call %s %s -> %q (err %v)", call.Name, call.Input, res.Content, err)
			req.Messages = append(req.Messages, agent.ToolMessage(res))
		}
	}
	t.Fatal("model did not answer within 3 turns")
	return ""
}
