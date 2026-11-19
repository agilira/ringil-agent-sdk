// serve_test.go: the API, end to end over HTTP
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
	"github.com/agilira/ringil-agent-sdk/session/file"
)

const token = "test-token-0123456789abcdef0123456789"

type api struct {
	t   *testing.T
	srv *Server
	url string
}

func newAPI(t *testing.T, cfg Config) *api {
	t.Helper()
	cfg.Token = token
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { h.Close(); s.Close() })
	return &api{t: t, srv: s, url: h.URL}
}

func profile(t *testing.T, name string, cfg agent.Config) Profile {
	t.Helper()
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Profile{Name: name, Description: "test", New: Fixed(a)}
}

// do sends a request with the token and returns status and body.
func (a *api) do(method, path, body string, headers ...string) (int, string) {
	a.t.Helper()
	req, err := http.NewRequest(method, a.url+path, strings.NewReader(body))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer closeQuietly(resp.Body)
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (a *api) start(body string) Snapshot {
	a.t.Helper()
	code, out := a.do("POST", "/v1/runs", body)
	if code != http.StatusCreated {
		a.t.Fatalf("start: %d %s", code, out)
	}
	var s Snapshot
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		a.t.Fatal(err)
	}
	return s
}

// until polls a run until cond holds.
func (a *api) until(id string, cond func(Snapshot) bool) Snapshot {
	a.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, out := a.do("GET", "/v1/runs/"+id, "")
		var s Snapshot
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			a.t.Fatal(err)
		}
		if cond(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.t.Fatalf("run %s never got there", id)
	return Snapshot{}
}

func over(s Snapshot) bool { return !s.Finished.IsZero() }

// events reads a run's SSE stream to its end: kinds in order, the ids.
func (a *api) events(id string, headers ...string) (kinds []string, ids []string) {
	a.t.Helper()
	req, _ := http.NewRequest("GET", a.url+"/v1/runs/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer closeQuietly(resp.Body)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: dropped"):
			kinds = append(kinds, "DROPPED")
		case strings.HasPrefix(line, "data: {\"id\""):
			var w wireEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &w); err != nil {
				a.t.Fatal(err)
			}
			kinds = append(kinds, w.Kind)
		}
	}
	return kinds, ids
}

func TestRunAndItsEvents(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ciao"))})}})
	if code, out := a.do("GET", "/v1/profiles", ""); code != 200 || !strings.Contains(out, `"name":"default"`) {
		t.Fatalf("%d %s", code, out)
	}
	s := a.start(`{"profile":"default","input":"hi"}`)
	done := a.until(s.ID, over)
	if done.State != Completed || done.Text != "ciao" || done.Turns != 1 {
		t.Fatalf("%+v", done)
	}
	kinds, ids := a.events(s.ID)
	if strings.Join(kinds, ",") != "started,text,model,done,run_end" {
		t.Fatalf("kinds %v", kinds)
	}
	// Resuming after event 3 gives the rest only.
	kinds, _ = a.events(s.ID, "Last-Event-ID", ids[2])
	if strings.Join(kinds, ",") != "done,run_end" {
		t.Fatalf("resumed %v", kinds)
	}
}

// Only the token's holder, by loopback name, not from a web page.
func TestGuard(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	for _, c := range []struct {
		headers []string
		want    int
	}{
		{[]string{"Authorization", ""}, 401},
		{[]string{"Authorization", "Bearer " + token + "x"}, 401},
		{[]string{"Origin", "https://evil.example"}, 403},
		{[]string{"Origin", "null"}, 403},
	} {
		if code, _ := a.do("GET", "/v1/profiles", "", c.headers...); code != c.want {
			t.Errorf("%v: %d, want %d", c.headers, code, c.want)
		}
	}
	// DNS rebinding: a name other than the loopback's.
	req, _ := http.NewRequest("GET", a.url+"/v1/profiles", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeQuietly(resp.Body)
	if resp.StatusCode != 403 {
		t.Fatalf("foreign host: %d", resp.StatusCode)
	}
}

func TestBadRequests(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	for body, want := range map[string]int{
		`{"profile":"default","input":"x","extra":1}`:           400,
		`{"profile":"nope","input":"x"}`:                        404,
		`{"profile":"default","input":""}`:                      400,
		`{"profile":"default","input":"x"} {}`:                  400,
		`{"profile":"default","input":"x","conversation":"c1"}`: 400, // no store
	} {
		if code, out := a.do("POST", "/v1/runs", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, _ := a.do("POST", "/v1/runs", `{"profile":"default","input":"x"}`, "Content-Type", "text/plain"); code != 415 {
		t.Errorf("text/plain: %d", code)
	}
	if code, _ := a.do("GET", "/v1/runs/nope", ""); code != 404 {
		t.Errorf("unknown run: %d", code)
	}
}

// waiter is a tool that changes something: it needs approval.
func waiter(ran *bool) agent.Tool {
	return agent.NewTool("delete_file", "deletes", func(context.Context, struct{}) (string, error) {
		*ran = true
		return "deleted", nil
	}, agent.Destructive())
}

func approvalProfile(t *testing.T, s **Server, ran *bool, steps ...fake.Step) Profile {
	t.Helper()
	reg, err := agent.NewRegistry(waiter(ran))
	if err != nil {
		t.Fatal(err)
	}
	approve := func(ctx context.Context, c agent.ToolCall, sp agent.ToolSpec) (bool, error) {
		return (*s).Approve(ctx, c, sp)
	}
	return profile(t, "default", agent.Config{Provider: fake.New(steps...), Tools: reg, Approve: approve})
}

func TestApprovalThroughTheAPI(t *testing.T) {
	for _, allow := range []bool{true, false} {
		var s *Server
		ran := false
		p := approvalProfile(t, &s, &ran, fake.Call("delete_file", `{}`), fake.Reply("ok"))
		a := newAPI(t, Config{Profiles: []Profile{p}})
		s = a.srv
		run := a.start(`{"profile":"default","input":"x"}`)
		w := a.until(run.ID, func(s Snapshot) bool { return s.State == Waiting })
		if len(w.Pending) != 1 || w.Pending[0].Tool != "delete_file" {
			t.Fatalf("%+v", w)
		}
		body := `{"allow":false}`
		if allow {
			body = `{"allow":true}`
		}
		if code, out := a.do("POST", "/v1/runs/"+run.ID+"/approvals/"+w.Pending[0].ID, body); code != 204 {
			t.Fatalf("%d %s", code, out)
		}
		a.until(run.ID, over)
		kinds, _ := a.events(run.ID)
		if ran != allow || !strings.Contains(strings.Join(kinds, ","), "tool_start,approval,approval_decided,tool_end") {
			t.Fatalf("allow %v ran %v kinds %v", allow, ran, kinds)
		}
		if code, _ := a.do("POST", "/v1/runs/"+run.ID+"/approvals/"+w.Pending[0].ID, body); code != 404 {
			t.Fatalf("decided twice: %d", code)
		}
	}
}

func TestApprovalTimesOut(t *testing.T) {
	var s *Server
	ran := false
	p := approvalProfile(t, &s, &ran, fake.Call("delete_file", `{}`), fake.Reply("ok"))
	a := newAPI(t, Config{Profiles: []Profile{p}, ApprovalTimeout: 50 * time.Millisecond})
	s = a.srv
	run := a.start(`{"profile":"default","input":"x"}`)
	if done := a.until(run.ID, over); done.State != Completed || ran {
		t.Fatalf("%+v ran %v", done, ran)
	}
}

// blocker waits until its run is cancelled.
func blocker(started chan<- struct{}) agent.Tool {
	return agent.NewTool("wait", "waits", func(ctx context.Context, _ struct{}) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
}

func TestCancelAndLimits(t *testing.T) {
	started := make(chan struct{}, 4)
	reg, err := agent.NewRegistry(blocker(started))
	if err != nil {
		t.Fatal(err)
	}
	p := profile(t, "default", agent.Config{Provider: fake.New(fake.Call("wait", `{}`), fake.Call("wait", `{}`)), Tools: reg})
	a := newAPI(t, Config{Profiles: []Profile{p}, MaxRuns: 1})
	run := a.start(`{"profile":"default","input":"x"}`)
	<-started
	if code, _ := a.do("POST", "/v1/runs", `{"profile":"default","input":"y"}`); code != 409 {
		t.Fatalf("second run over the limit: %d", code)
	}
	if code, _ := a.do("DELETE", "/v1/runs/"+run.ID, ""); code != 409 {
		t.Fatalf("forgot a running run: %d", code)
	}
	if code, _ := a.do("POST", "/v1/runs/"+run.ID+"/cancel", ""); code != 202 {
		t.Fatalf("cancel: %d", code)
	}
	if done := a.until(run.ID, over); done.State != Cancelled {
		t.Fatalf("%+v", done)
	}
	if code, _ := a.do("DELETE", "/v1/runs/"+run.ID, ""); code != 204 {
		t.Fatalf("forget: %d", code)
	}
}

// A conversation continues across runs; it admits one run at a time.
func TestConversations(t *testing.T) {
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := fake.New(fake.Reply("first"), fake.Reply("second"))
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: script})}, Store: store})
	r1 := a.start(`{"profile":"default","input":"one","conversation":"c1"}`)
	a.until(r1.ID, over)
	r2 := a.start(`{"profile":"default","input":"two","conversation":"c1"}`)
	if done := a.until(r2.ID, over); done.State != Completed {
		t.Fatalf("%+v", done)
	}
	if msgs := script.Requests()[1].Messages; len(msgs) != 3 || msgs[1].Text() != "first" {
		t.Fatalf("history %d", len(msgs))
	}
}

func TestOldRunsAreDropped(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("x"))})}, Keep: time.Minute})
	run := a.start(`{"profile":"default","input":"x"}`)
	a.until(run.ID, over)
	a.srv.mu.Lock()
	a.srv.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	a.srv.mu.Unlock()
	if code, _ := a.do("GET", "/v1/runs/"+run.ID, ""); code != 404 {
		t.Fatalf("kept past Keep: %d", code)
	}
}

func TestLogIsBounded(t *testing.T) {
	chunks := make([]string, 200)
	for i := range chunks {
		chunks[i] = strings.Repeat("x", 100)
	}
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Stream(chunks...))})}, MaxLog: 8 << 10})
	run := a.start(`{"profile":"default","input":"x"}`)
	done := a.until(run.ID, over)
	kinds, _ := a.events(run.ID)
	if done.Dropped == 0 || kinds[0] != "DROPPED" || kinds[len(kinds)-1] != "run_end" {
		t.Fatalf("dropped %d kinds %v", done.Dropped, kinds[:3])
	}
}

func TestServeOnLoopbackOnly(t *testing.T) {
	s, err := New(Config{Token: token, Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skip(err)
	}
	defer closeQuietly(l)
	if err := s.Serve(context.Background(), l); err == nil {
		t.Fatal("served on every interface")
	}
}

func TestConfigRefused(t *testing.T) {
	good := profile(t, "default", agent.Config{Provider: fake.New()})
	for _, c := range []Config{
		{Token: "short", Profiles: []Profile{good}},
		{Token: token},
		{Token: token, Profiles: []Profile{good, good}},
		{Token: token, Profiles: []Profile{{Name: "bad name", New: good.New}}},
		{Token: token, Profiles: []Profile{good}, MaxRuns: -1},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

// A run waits for the work it left in the background, and the agent sees
// the report before the run ends; each run has its own crew.
func TestBackgroundWorkIsAwaited(t *testing.T) {
	var made atomic.Int32
	p := Profile{Name: "default", New: func(context.Context) (Instance, error) {
		made.Add(1)
		crew, err := delegate.NewCrew(delegate.Team{Workers: []delegate.Worker{{Name: "w", Description: "w", Provider: fake.New(fake.Reply("314 units"))}}})
		if err != nil {
			return Instance{}, err
		}
		reg, err := agent.NewRegistry(crew.WorkerTools()...)
		if err != nil {
			return Instance{}, err
		}
		parent := fake.New(fake.Call("w", `{"task":"stock?","background":true}`), fake.Reply("waiting"), fake.Reply("AX-7: 314"))
		a, err := agent.New(agent.Config{Provider: parent, Tools: reg})
		return Instance{Agent: a, Crew: crew}, err
	}}
	a := newAPI(t, Config{Profiles: []Profile{p}})
	r1 := a.start(`{"profile":"default","input":"x"}`)
	r2 := a.start(`{"profile":"default","input":"x"}`)
	for _, r := range []Snapshot{r1, r2} {
		if done := a.until(r.ID, over); done.State != Completed || done.Text != "AX-7: 314" {
			t.Fatalf("%+v", done)
		}
	}
	if made.Load() != 2 {
		t.Fatalf("instances %d", made.Load())
	}
}

func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}

func TestWatch(t *testing.T) {
	var s *Server
	var ran bool
	a := newAPI(t, Config{Profiles: []Profile{approvalProfile(t, &s, &ran, fake.Call("delete_file", `{}`), fake.Reply("done"))}})
	s = a.srv
	if err := s.Watch(context.Background(), "nope", func(Snapshot) {}); err == nil {
		t.Fatal("watched a run that does not exist")
	}
	run, err := s.Start("default", "", "go")
	if err != nil {
		t.Fatal(err)
	}
	var seen []Snapshot
	err = s.Watch(context.Background(), run.ID, func(sn Snapshot) {
		seen = append(seen, sn)
		if len(sn.Pending) == 1 {
			if err := s.Decide(run.ID, sn.Pending[0].ID, true); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if last := seen[len(seen)-1]; last.State != Completed || last.Text != "done" || !ran {
		t.Fatalf("last %+v ran %v", last, ran)
	}
}

func TestWatchEndsWithItsContext(t *testing.T) {
	var s *Server
	var ran bool
	a := newAPI(t, Config{Profiles: []Profile{approvalProfile(t, &s, &ran, fake.Call("delete_file", `{}`), fake.Reply("done"))}})
	s = a.srv
	run, err := s.Start("default", "", "go")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = s.Watch(ctx, run.ID, func(sn Snapshot) {
		if sn.State == Waiting {
			cancel()
		}
	})
	if err != context.Canceled {
		t.Fatalf("err %v", err)
	}
}

func TestPendingInTheOrderAsked(t *testing.T) {
	j := newJob("r", "p", "", &Server{now: time.Now, cfg: Config{}})
	for range 10 {
		j.ask(agent.ToolCall{Name: "x"}, "")
	}
	p := j.snapshot().Pending
	if p[8].ID != "a9" || p[9].ID != "a10" {
		t.Fatalf("order %s %s", p[8].ID, p[9].ID)
	}
}

func TestStartMessageChecksTheInput(t *testing.T) {
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}})
	img := agent.Part{Type: agent.PartImage, MIME: "image/png", Data: []byte("not a png")}
	bad := []agent.Message{
		agent.UserMessage("look", img),
		{Role: agent.RoleUser},
		agent.AssistantText("hi"),
	}
	for i, m := range bad {
		if _, err := a.srv.StartMessage("default", "", m); err == nil {
			t.Errorf("input %d accepted", i)
		}
	}
}

func TestConversationOf(t *testing.T) {
	seen := make(chan string, 1)
	p := Profile{Name: "default", New: func(ctx context.Context) (Instance, error) {
		seen <- ConversationOf(ctx)
		return Instance{}, nil
	}}
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t, Config{Profiles: []Profile{p}, Store: store})
	if _, err := a.srv.Start("default", "telegram-5", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "telegram-5" {
		t.Fatalf("conversation %q", got)
	}
	if ConversationOf(context.Background()) != "" {
		t.Fatal("a conversation outside a run")
	}
}

func TestConversationsListsTheStore(t *testing.T) {
	store, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}, Store: store})
	run, err := a.srv.Start("default", "telegram-5", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.Wait(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := a.srv.Conversations(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != "telegram-5" {
		t.Fatalf("%v %v", ids, err)
	}
	none := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if ids, err := none.srv.Conversations(context.Background()); ids != nil || err != nil {
		t.Fatalf("without a store: %v %v", ids, err)
	}
}

func TestSourceOf(t *testing.T) {
	got := make(chan string, 2)
	a, err := agent.New(agent.Config{Provider: fake.New(fake.Reply("ok"), fake.Reply("ok"))})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Token: strings.Repeat("t", 32), Profiles: []Profile{{Name: "default", New: func(ctx context.Context) (Instance, error) {
		got <- SourceOf(ctx)
		return Instance{Agent: a}, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := srv.StartFrom("trigger:mail", "default", "", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != "trigger:mail" {
		t.Fatalf("%q", s)
	}
	if _, err := srv.Start("default", "", "hi"); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != "" {
		t.Fatalf("an API run labelled %q", s)
	}
}
