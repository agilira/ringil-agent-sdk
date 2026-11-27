// tracing_test.go: the runs' traces
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agilira/go-errors"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agent "github.com/agilira/ringil-agent-sdk"
	ringilotel "github.com/agilira/ringil-agent-sdk/otel"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

// collector is an OTLP endpoint that keeps what it is sent.
func collector(t *testing.T) (*httptest.Server, func() [][]byte, func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if r.URL.Path == "/v1/traces" {
			bodies, headers = append(bodies, b), append(headers, r.Header.Clone())
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]byte { mu.Lock(); defer mu.Unlock(); return bodies }, func() []http.Header { mu.Lock(); defer mu.Unlock(); return headers }
}

// With an endpoint set, a run's spans reach it at exit, with the headers
// asked, and no word of the conversation.
func TestTracesReachTheEndpoint(t *testing.T) {
	srv, bodies, headers := collector(t)
	h := newHarness(t, "", fake.Reply("the secret answer"))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k",
		"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL, "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20tok,x-team=a"})
	if code := h.run("run", "the secret question"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	got := bodies()
	if len(got) == 0 {
		t.Fatal("no traces sent")
	}
	all := bytes.Join(got, nil)
	if !bytes.Contains(all, []byte("invoke_agent")) || !bytes.Contains(all, []byte("chat fake")) || !bytes.Contains(all, []byte("ringil")) {
		t.Fatalf("spans missing from %q", all)
	}
	if bytes.Contains(all, []byte("secret")) {
		t.Fatal("conversation text in the traces")
	}
	if hd := headers()[0]; hd.Get("Authorization") != "Bearer tok" || hd.Get("X-Team") != "a" {
		t.Fatalf("headers %v", hd)
	}
}

// Without an endpoint nothing is sent and nothing is said.
func TestNoEndpointNoTraces(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	if code := h.run("run", "x"); code != exitOK || strings.Contains(h.err.String(), "traces") {
		t.Fatalf("exit %d %q", code, h.err)
	}
}

// A bad endpoint is said once, and the run goes on.
func TestBadEndpointIsSaid(t *testing.T) {
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k", "OTEL_EXPORTER_OTLP_ENDPOINT": "ftp://x"})
	if code := h.run("run", "x"); code != exitOK || strings.Count(h.err.String(), "* traces:") != 1 {
		t.Fatalf("exit %d %q", code, h.err)
	}
}

func TestParseHeaders(t *testing.T) {
	got := parseHeaders(" a = 1 ,b=x%3Dy,,=skip,c,auth=Basic%20dXNlcjpw+YQ==")
	if len(got) != 3 || got["a"] != "1" || got["b"] != "x=y" || got["auth"] != "Basic dXNlcjpw+YQ==" {
		t.Fatalf("%v", got)
	}
}

// The base address gets /v1/traces, path and all; the traces' own address
// is used as it is.
func TestTraceEndpointVariables(t *testing.T) {
	for vars, want := range map[[2]string]string{
		{"https://otel.example.com:4318", ""}:                        "https://otel.example.com:4318/v1/traces",
		{"https://gw.example.com/otlp/", ""}:                         "https://gw.example.com/otlp/v1/traces",
		{"https://base.example.com", "https://t.example.com/ingest"}: "https://t.example.com/ingest",
		{"https://gw.example.com/a%2Fb", ""}:                         "https://gw.example.com/a%2Fb/v1/traces",
		{"", ""}:                                                     "",
	} {
		h := newHarness(t, "")
		env(h, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": vars[0], "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": vars[1]})
		if got, _ := h.c.traceEndpoint(); got != want {
			t.Errorf("%v: %s, want %s", vars, got, want)
		}
	}
}

// A revision reaches the traces as its digests, the keys that changed and
// what needs a restart; a refusal as its code. No value of the file, no
// message: they name secrets, headers and addresses.
func TestConfigRevisionsTraced(t *testing.T) {
	srv, bodies, _ := collector(t)
	h := newHarness(t, "")
	env(h, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL})
	before := &config{Models: []string{"openai:m"}, digest: "aaaaaaaaaaaaaaaa", Providers: map[string]providerEntry{"gw": {Base: "https://gw.example.com/v1", KeyEnv: "GW_KEY"}}}
	after := &config{Models: []string{"openai:m", "secret-provider:secret-model"}, digest: "bbbbbbbbbbbbbbbb", MaxCost: 2,
		Providers: map[string]providerEntry{"gw": {Base: "https://secret.example.com/v1", KeyEnv: "GW_KEY"}}}
	h.c.traceConfig(configApplied, before, after, "allow_http, provider and providers for the chats' voice", nil)
	h.c.traceConfig(configRefused, after, nil, "", errors.New(ErrUsage, "models secret-provider:x: no provider secret-provider"))
	h.c.flushTraces()
	all := bytes.Join(bodies(), nil)
	for _, want := range []string{"ringil.config applied", "ringil.config refused", "aaaaaaaaaaaa", "bbbbbbbbbbbb", "models", "providers", "max_cost", "USAGE"} {
		if !bytes.Contains(all, []byte(want)) {
			t.Errorf("missing %q", want)
		}
	}
	if bytes.Contains(all, []byte("secret")) {
		t.Fatal("a value or a message of the file reached the traces")
	}
}

// Only what differs is named, the instructions' text included.
func TestChangedKeys(t *testing.T) {
	a := &config{Models: []string{"openai:m"}, texts: map[string]string{"x.md": "one"}}
	b := &config{Models: []string{"openai:m"}, MaxTurns: 3, texts: map[string]string{"x.md": "two"}}
	if got := strings.Join(changedKeys(a, b), ","); got != "max_turns,instructions" {
		t.Fatalf("%s", got)
	}
	if got := changedKeys(a, a); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// OTEL_SDK_DISABLED and OTEL_TRACES_EXPORTER=none turn the traces off
// over an endpoint set elsewhere; a protocol other than http/protobuf is
// said, not sent nowhere.
func TestTracesOffAndProtocol(t *testing.T) {
	for _, off := range []map[string]string{{"OTEL_SDK_DISABLED": "TRUE"}, {"OTEL_TRACES_EXPORTER": "none"}} {
		h := newHarness(t, "")
		off["OTEL_EXPORTER_OTLP_ENDPOINT"] = "https://otel.example.com"
		env(h, off)
		if got, _ := h.c.traceEndpoint(); got != "" {
			t.Errorf("%v: %s", off, got)
		}
	}
	h := newHarness(t, "", fake.Reply("ok"))
	env(h, map[string]string{"RINGIL_PROVIDER": "openai", "RINGIL_MODEL": "m", "OPENAI_API_KEY": "k",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"})
	if code := h.run("run", "x"); code != exitOK || !strings.Contains(h.err.String(), "http/protobuf") {
		t.Fatalf("exit %d %q", code, h.err)
	}
}

// Every observer of the process records on one Recorder: a worker's run,
// seen through the crew's observers, goes under the chat's call.
func TestObserversShareTheRecorder(t *testing.T) {
	srv, _, _ := collector(t)
	h := newHarness(t, "")
	env(h, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL})
	exp := tracetest.NewInMemoryExporter()
	if tp, err := h.c.tracer(context.Background()); tp == nil || err != nil {
		t.Fatalf("%v %v", tp, err)
	}
	h.c.traces.rec = ringilotel.New(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("t"))
	lead, crew := last(h.c.observers(context.Background())), last(h.c.observers(context.Background()))
	lead(agent.Event{Kind: agent.EventStarted, Run: "p", Time: time.Now()})
	lead(agent.Event{Kind: agent.EventToolStart, Run: "p", Call: &agent.ToolCall{ID: "c", Name: "delegate"}, Time: time.Now()})
	crew(agent.Event{Kind: agent.EventStarted, Run: "w", Parent: "p", ParentCall: "c", Worker: "explore", Time: time.Now()})
	crew(agent.Event{Kind: agent.EventDone, Run: "w", Time: time.Now()})
	lead(agent.Event{Kind: agent.EventToolEnd, Run: "p", Call: &agent.ToolCall{ID: "c", Name: "delegate"}, Time: time.Now()})
	lead(agent.Event{Kind: agent.EventDone, Run: "p", Time: time.Now()})
	var call, worker tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "execute_tool delegate":
			call = s
		case "invoke_agent explore":
			worker = s
		}
	}
	if worker.Parent.SpanID() != call.SpanContext.SpanID() || !call.SpanContext.IsValid() {
		t.Fatalf("worker under %v, call %v", worker.Parent.SpanID(), call.SpanContext.SpanID())
	}
}

func last(obs []agent.Observer) agent.Observer { return obs[len(obs)-1] }

// An endpoint with a query is refused: the exporter would drop it.
func TestTraceEndpointQueryRefused(t *testing.T) {
	if _, err := tracesURL("https://gw.example.com/v1/traces?token=x"); err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("%v", err)
	}
}
