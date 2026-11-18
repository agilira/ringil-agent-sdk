// audit_test.go: regressions for the serve audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// funcAgent is an agent whose Stream is a function.
type funcAgent func(ctx context.Context, yield func(agent.Event, error) bool)

func (f funcAgent) Run(context.Context, agent.Conversation, agent.Message) (*agent.Result, error) {
	return nil, nil
}

func (f funcAgent) Stream(ctx context.Context, _ agent.Conversation, _ agent.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) { f(ctx, yield) }
}

func mustGet(s *Server, id string) Snapshot {
	g, _ := s.Get(id)
	return g
}

// (1) A Cancel right after Start was lost: the cancel func did not exist
// yet.
func TestCancelRightAfterStart(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	block := funcAgent(func(ctx context.Context, yield func(agent.Event, error) bool) {
		select {
		case <-ctx.Done():
			yield(agent.Event{}, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	})
	s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(block)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Start("p", "", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(snap.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Wait(context.Background(), snap.ID); got.State != Cancelled {
		t.Fatalf("run ended %q", got.State)
	}
}

// (2) A tool input that is not JSON lost its events and broke every
// snapshot and the listing.
func TestInvalidCallInput(t *testing.T) {
	var s *Server
	ran := false
	a := newAPI(t, Config{Profiles: []Profile{approvalProfile(t, &s, &ran, fake.Call("delete_file", "{"), fake.Reply("ok"))}})
	s = a.srv
	run := a.start(`{"profile":"default","input":"x"}`)
	w := a.until(run.ID, func(s Snapshot) bool { return s.State == Waiting })
	if string(w.Pending[0].Input) != `"{"` {
		t.Fatalf("input %s", w.Pending[0].Input)
	}
	if code, out := a.do("GET", "/v1/runs", ""); code != 200 || !strings.Contains(out, run.ID) {
		t.Fatalf("%d %s", code, out)
	}
}

// (3) Two waiting calls with one call ID: one was lost. Approvals have IDs
// of the server's.
func TestSameCallIDTwice(t *testing.T) {
	var s *Server
	done := make(chan bool, 2)
	ag := funcAgent(func(ctx context.Context, yield func(agent.Event, error) bool) {
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, _ := s.Approve(ctx, agent.ToolCall{ID: "call_0", Name: "rm", Input: json.RawMessage(`{}`)}, agent.ToolSpec{})
				done <- ok
			}()
		}
		wg.Wait()
	})
	var err error
	s, err = New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(ag)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, _ := s.Start("p", "", "x")
	pend := pendingOf(s, snap.ID, 2)
	if len(pend) != 2 || pend[0].ID == pend[1].ID {
		t.Fatalf("pending %+v", pend)
	}
	for _, p := range pend {
		if err := s.Decide(snap.ID, p.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if !<-done {
			t.Fatal("a decided call was refused")
		}
	}
}

// pendingOf waits until n calls of a run wait for approval.
func pendingOf(s *Server, id string, n int) []Approval {
	var pend []Approval
	for deadline := time.Now().Add(5 * time.Second); len(pend) < n && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		pend = mustGet(s, id).Pending
	}
	return pend
}

// (4) An idle stream died at the first heartbeat 30s after the last
// event: every write has its own deadline now.
func TestIdleStreamStaysOpen(t *testing.T) {
	oldBeat, oldWrite := heartbeat, writeTimeout
	heartbeat, writeTimeout = 100*time.Millisecond, 300*time.Millisecond
	defer func() { heartbeat, writeTimeout = oldBeat, oldWrite }()
	ag := funcAgent(func(ctx context.Context, yield func(agent.Event, error) bool) {
		yield(agent.Event{Kind: agent.EventText, Text: "one"}, nil)
		<-ctx.Done()
	})
	s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(ag)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	snap, _ := s.Start("p", "", "x")
	req, _ := http.NewRequest("GET", h.URL+"/v1/runs/"+snap.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(resp.Body)
	beats := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":") {
			beats++
		}
	}
	if beats < 10 {
		t.Fatalf("stream ended after %d heartbeats: %v", beats, sc.Err())
	}
}

// (6) An Instance without an agent was never released.
func TestInstanceReleasedOnError(t *testing.T) {
	closed := make(chan struct{})
	s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: func(context.Context) (Instance, error) {
		return Instance{Close: func() { close(closed) }}, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start("p", "", "x"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	select {
	case <-closed:
	default:
		t.Fatal("Instance.Close not called")
	}
}

// (5) Start racing Close could start a run after Close returned.
func TestStartVersusClose(t *testing.T) {
	for range 500 {
		s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(funcAgent(func(context.Context, func(agent.Event, error) bool) {}))}}})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = s.Start("p", "", "x") }()
		s.Close()
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active > 0 {
			t.Fatal("a run is active after Close returned")
		}
	}
}

// (7) Every error read "Tool failed [CODE]."; each says what happened.
func TestErrorBodiesSayWhy(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{{Name: "p", New: Fixed(funcAgent(func(context.Context, func(agent.Event, error) bool) {}))}}})
	for path, want := range map[string]string{"/v1/runs/nope": "no such run"} {
		if _, out := a.do("GET", path, ""); !strings.Contains(out, want) {
			t.Fatalf("%s: %s", path, out)
		}
	}
	if _, out := a.do("GET", "/v1/runs", "", "Authorization", ""); !strings.Contains(out, "a valid bearer token is required") {
		t.Fatal(out)
	}
	s, err := New(Config{Token: token, Profiles: []Profile{{Name: "p", New: Fixed(funcAgent(func(_ context.Context, y func(agent.Event, error) bool) {
		y(agent.Event{}, context.DeadlineExceeded)
	}))}}})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Start("p", "", "x")
	s.Close()
	if g := mustGet(s, snap.ID); g.Error == nil || g.Error.Message != "The run failed." {
		t.Fatalf("%+v", g.Error)
	}
}
