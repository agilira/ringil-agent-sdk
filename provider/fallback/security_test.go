// security_test.go: the fallback at its limits
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package fallback

import (
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// FuzzNeverLosesACall: whatever the first model does, call after call,
// and however time moves, a call is answered while the last model works,
// and a request error is never handed over.
func FuzzNeverLosesACall(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 0, 0, 1})
	f.Add([]byte{3, 3, 3, 3})
	f.Fuzz(func(t *testing.T, plan []byte) {
		if len(plan) > 64 {
			plan = plan[:64]
		}
		last := make([]fake.Step, len(plan))
		for i := range last {
			last[i] = fake.Reply("b")
		}
		clk := &clock{t: time.Unix(0, 0)}
		a := named("a", planSteps(plan)...)
		p := mustNew(t, []agent.Provider{a, named("b", last...)}, Config{Now: clk.now})
		for i, b := range plan {
			clk.t = clk.t.Add(time.Duration(b) * 10 * time.Second)
			before := a.Remaining()
			got, err := call(p)
			checkCall(t, i, got, err, a.Remaining() < before)
		}
	})
}

// planSteps: what the first model does, call after call.
func planSteps(plan []byte) []fake.Step {
	steps := make([]fake.Step, len(plan))
	for i, b := range plan {
		switch b % 4 {
		case 0:
			steps[i] = fake.Reply("a")
		case 1:
			steps[i] = fake.Fail(down)
		case 2:
			steps[i] = fake.Fail(errors.New(agent.ErrProviderAuth, "key"))
		default:
			steps[i] = fake.Fail(errors.New(agent.ErrProviderInput, "bad"))
		}
	}
	return steps
}

// checkCall: answered, or the first model's own request error.
func checkCall(t *testing.T, i int, got string, err error, firstAsked bool) {
	t.Helper()
	switch {
	case err != nil && !errors.HasCode(err, agent.ErrProviderInput):
		t.Fatalf("call %d lost: %v", i, err)
	case err != nil && !firstAsked:
		t.Fatalf("call %d: a request error not from the first model", i)
	case err == nil && got == "":
		t.Fatalf("call %d: empty answer", i)
	}
}
