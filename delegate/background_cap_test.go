// background_cap_test.go: background tasks are capped (regression: a burst
// of background:true spawned workers and provider calls without bound).
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package delegate

import (
	"context"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func TestBackgroundCapRefusesAtLimit(t *testing.T) {
	crew, err := NewCrew(Team{
		Workers:       []Worker{{Name: "w", Description: "w", Provider: fake.New(fake.Reply("ok"))}},
		MaxBackground: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()

	for range 2 {
		if !crew.reserve() {
			t.Fatal("two reserves within the cap should succeed")
		}
	}
	if crew.reserve() {
		t.Fatal("a third reserve past the cap must fail")
	}
	_, err = crew.call(bg, taskIn{Worker: "w", Task: "x", Background: true})
	if !errors.HasCode(err, agent.ErrToolInput) {
		t.Fatalf("a background task past the cap must be refused, got %v", err)
	}
}

func TestBackgroundSlotReleased(t *testing.T) {
	crew, err := NewCrew(Team{
		Workers:       []Worker{{Name: "w", Description: "w", Provider: fake.New(fake.Reply("ok"))}},
		MaxBackground: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()

	if _, err := crew.call(bg, taskIn{Worker: "w", Task: "x", Background: true}); err != nil {
		t.Fatalf("first background task should start: %v", err)
	}
	if _, ok := crew.Next(bg); !ok {
		t.Fatal("the background task should report")
	}
	// Its slot is free again: another background task starts.
	if _, err := crew.call(bg, taskIn{Worker: "w", Task: "y", Background: true}); err != nil {
		t.Fatalf("a background task should start after a slot freed: %v", err)
	}
	if _, ok := crew.Next(bg); !ok {
		t.Fatal("the second background task should report")
	}
}

// A refused background call at the cap must neither drop an existing task
// passed via continue_task nor run Prepare for a new task.
func TestBackgroundCapPreservesContinueAndSkipsPrepare(t *testing.T) {
	prep := 0
	crew, err := NewCrew(Team{
		Workers: []Worker{{
			Name: "w", Description: "w", Provider: fake.New(fake.Reply("ok"), fake.Reply("more")),
			Prepare: func(context.Context, string) (Setup, error) { prep++; return Setup{}, nil },
		}},
		MaxBackground: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()

	if _, err := crew.call(bg, taskIn{Worker: "w", Task: "one"}); err != nil {
		t.Fatal(err)
	}
	if !crew.reserve() {
		t.Fatal("reserve should succeed")
	}
	if _, err := crew.call(bg, taskIn{ContinueTask: 1, Task: "more", Background: true}); !errors.HasCode(err, agent.ErrToolInput) {
		t.Fatalf("want ErrToolInput at cap, got %v", err)
	}
	if _, err := crew.call(bg, taskIn{ContinueTask: 1, Task: "more"}); err != nil {
		t.Fatalf("refused background continue_task dropped task 1: %v", err)
	}
	if _, err := crew.call(bg, taskIn{Worker: "w", Task: "new", Background: true}); !errors.HasCode(err, agent.ErrToolInput) || prep != 1 {
		t.Fatalf("expected refusal without calling Prepare again: err=%v prep=%d", err, prep)
	}
}

func TestBackgroundFailureUnreserves(t *testing.T) {
	crew, err := NewCrew(Team{
		Workers:       []Worker{{Name: "w", Description: "w", Provider: fake.New(fake.Reply("ok"))}},
		MaxBackground: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()

	if _, err := crew.call(bg, taskIn{Worker: "missing", Task: "bad", Background: true}); !errors.HasCode(err, agent.ErrToolInput) {
		t.Fatalf("expected ErrToolInput for unknown worker, got %v", err)
	}
	if got := crew.Pending(); got != 0 {
		t.Fatalf("failed background start leaked a reserved slot: Pending=%d", got)
	}
}
