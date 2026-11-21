// live_test.go: typed tools against a real model
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/eventlog"
	"github.com/agilira/ringil-agent-sdk/memory"
	"github.com/agilira/ringil-agent-sdk/memory/local"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/provider/budget"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
	"github.com/agilira/ringil-agent-sdk/rules"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/todo"
	"github.com/agilira/ringil-agent-sdk/tools/bash"
	"github.com/agilira/ringil-agent-sdk/tools/files"
	"github.com/agilira/ringil-agent-sdk/workflow"
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
	spend, err := budget.New(budget.Limits{Calls: 40})
	if err != nil {
		t.Fatal(err)
	}
	var wrapped agent.Provider = retry.Wrap(spend.Wrap(p), retry.Config{})
	if os.Getenv("RINGIL_LIVE_TRACE") == "1" {
		wrapped = traced{wrapped, t}
	}
	return wrapped
}

// traced logs what the model did, for telling our faults from the
// model's: RINGIL_LIVE_TRACE=1 (live tests only; test data, no secrets).
type traced struct {
	agent.Provider
	t *testing.T
}

func (p traced) Generate(ctx context.Context, req *agent.Request, sink agent.TextSink) (*agent.Response, error) {
	first := len(req.Messages)
	for first > 0 && req.Messages[first-1].Result != nil {
		first--
	}
	for _, m := range req.Messages[first:] {
		p.t.Logf("  <- %.200q", m.Result.Content)
	}
	resp, err := p.Provider.Generate(ctx, req, sink)
	if err != nil {
		p.t.Logf("  !! %v", err)
		return resp, err
	}
	for _, c := range resp.Message.Calls {
		p.t.Logf("  -> %s %.200s", c.Name, c.Input)
	}
	if txt := resp.Message.Text(); txt != "" {
		p.t.Logf("  .. %.200q", txt)
	}
	return resp, nil
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

// TestLiveCompaction gives a real model more tool output than its (small,
// declared) window: old results must be stubbed and the task still finished.
func TestLiveCompaction(t *testing.T) {
	model := liveModel(t)
	page := 0
	fetch := agent.NewTool("read_page", "Read the next page of the report", func(context.Context, struct{}) (string, error) {
		page++
		return "Page " + strconv.Itoa(page) + " of 4. Key figure on this page: " + strconv.Itoa(page*111) + ". " +
			strings.Repeat("Filler text. ", 300), nil
	}, agent.ReadOnly())
	tools, err := agent.NewRegistry(fetch)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, ContextWindow: 3000, MaxTurns: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	compacted, answer := watch(t, a.Stream(ctx, nil, agent.UserText("Read all 4 pages of the report one at a time with read_page, then tell me the key figure of the last page.")))
	if compacted == 0 || page < 4 || !strings.Contains(answer, "444") {
		t.Fatalf("compacted %d pages %d answer %q", compacted, page, answer)
	}
}

// watch drains a run, logging compactions, and returns their count and the
// final answer.
func watch(t *testing.T, events iter.Seq2[agent.Event, error]) (int, string) {
	t.Helper()
	compacted, answer := 0, ""
	for ev, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.EventCompacted {
			compacted++
			t.Log("compacted:", ev.Text)
		}
		if ev.Kind == agent.EventDone {
			answer = ev.Result.Text
		}
	}
	return compacted, answer
}

// TestLiveFileTools: a real model finds and reads a file through the file
// tools, outside the first granted directory, and answers from it.
func TestLiveFileTools(t *testing.T) {
	model := liveModel(t)
	work, docs := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "contratto.txt"), []byte("Durata del contratto: 36 mesi.\nPenale di recesso: 4.200 euro.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := files.New(files.Config{Dirs: []string{work, docs}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(ws.ReadTools()...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, MaxTurns: 6})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), nil, agent.UserText("Nella cartella "+docs+" c'è un contratto. Qual è la penale di recesso?"))
	if err != nil {
		t.Fatal(err)
	}
	// 4.200, 4,200, 4200, 4 200: any way of writing it.
	if !strings.Contains(strings.NewReplacer(".", "", ",", "", " ", "", "\u00a0", "").Replace(res.Text), "4200") {
		t.Fatalf("answer %q", res.Text)
	}
}

// TestLiveMemory: a fact remembered through the tools in one run is
// recalled into a later, separate conversation.
func TestLiveMemory(t *testing.T) {
	model := liveModel(t)
	mem, err := local.New(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(memory.Tools(mem)...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, Memory: mem, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := a.Run(ctx, nil, agent.UserText("Ricordati per le prossime volte che il mio fornitore di fiducia è Ferramenta Rossi.")); err != nil {
		t.Fatal(err)
	}
	if recs, err := mem.Search(ctx, "fornitore", 5); err != nil || len(recs) == 0 {
		t.Fatalf("nothing remembered: %v", err)
	}
	res, err := a.Run(ctx, nil, agent.UserText("Qual è il mio fornitore di fiducia?"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Rossi") {
		t.Fatalf("answer %q", res.Text)
	}
}

// TestLiveSkill: a real model picks an installed skill from its
// description, loads it and runs its script in the sandbox.
func TestLiveSkill(t *testing.T) {
	model := liveModel(t)
	cat, sb := liveCatalog(t)
	tools, err := agent.NewRegistry(skills.Tools(cat, skills.Options{Sandbox: sb})...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, System: "Answer briefly." + cat.Instructions(), MaxTurns: 6})
	if err != nil {
		t.Fatal(err)
	}
	var ran bool
	for ev, err := range a.Stream(context.Background(), nil, agent.UserText("Quanti caratteri ha esattamente questo testo? Usa lo script della skill. Testo: ringil-agent-sdk")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.EventToolEnd && ev.Call.Name == "run_skill_script" && ev.Err == nil {
			ran = true
		}
	}
	if !ran {
		t.Fatal("the skill script did not run")
	}
}

func liveCatalog(t *testing.T) (*skills.Catalog, sandbox.Sandbox) {
	t.Helper()
	sb, err := sandbox.New()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Join(t.TempDir(), "skills")
	if _, err := skills.Install(context.Background(), "examples/skills/review", dir); err != nil {
		t.Fatal(err)
	}
	cat, err := skills.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cat, sb
}

// TestLiveDiscovery: with schemas deferred, a real model loads the tool
// it needs through tool_search and then calls it.
func TestLiveDiscovery(t *testing.T) {
	model := liveModel(t)
	type noArgs struct{}
	called := false
	tools := []agent.Tool{agent.NewTool("weather_today", "Today's weather in Milan.", func(context.Context, noArgs) (string, error) {
		called = true
		return "Milano: sole, 21 gradi.", nil
	}, agent.Deferrable())}
	for _, n := range []string{"crm_lookup", "invoice_create", "slack_post", "calendar_add"} {
		tools = append(tools, agent.NewTool(n, "Business tool "+n+".", func(context.Context, noArgs) (string, error) { return "", nil }, agent.Deferrable()))
	}
	reg, err := agent.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: reg, Discovery: agent.DiscoveryAlways, MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), nil, agent.UserText("Che tempo fa oggi a Milano? Usa lo strumento adatto."))
	if err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(res.Text, "21") {
		t.Fatalf("called %v, answer %q", called, res.Text)
	}
}

// TestLiveNotes: a real model writes a project note in one conversation,
// and a later one answers from the note's index in its context.
func TestLiveNotes(t *testing.T) {
	model := liveModel(t)
	nb, err := notes.New(notes.Scope{Name: "project", Dir: filepath.Join(t.TempDir(), "project")})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(nb.Tool(), todo.Tool())
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, Context: nb.Context, MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := a.Run(ctx, nil, agent.UserText("Salva nelle note di progetto, nel file /project/INDEX.md, la decisione: il database di produzione è PostgreSQL 17.")); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, nil, agent.UserText("Che database usiamo in produzione?"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "PostgreSQL") {
		t.Fatalf("answer %q", res.Text)
	}
}

// TestLiveRules: a real model follows rules written for other agents.
func TestLiveRules(t *testing.T) {
	model := liveModel(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Rispondi sempre in inglese, e chiudi ogni risposta con: -- ringil"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Context: rules.Context(rules.Config{Dir: dir})})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), nil, agent.UserText("Come si dice gatto?"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "-- ringil") {
		t.Fatalf("answer %q", res.Text)
	}
}

// TestLiveDelegation: a real model hands a lookup to a sub-agent with a
// tool the parent does not have, and answers from its report.
func TestLiveDelegation(t *testing.T) {
	model := liveModel(t)
	type noArgs struct{}
	stock := agent.NewTool("stock_level", "Stock of item AX-7 in the warehouse.", func(context.Context, noArgs) (string, error) {
		return "AX-7: 314 units in Bologna.", nil
	}, agent.ReadOnly())
	wtools, err := agent.NewRegistry(stock)
	if err != nil {
		t.Fatal(err)
	}
	crew, err := delegate.NewCrew(delegate.Team{Config: agent.Config{Provider: model}, Workers: []delegate.Worker{{Name: "warehouse", Description: "knows stock levels; it has the stock_level tool", Tools: wtools}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	tools, err := agent.NewRegistry(crew.WorkerTools()...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	res, err := crew.Converse(context.Background(), a, nil, agent.UserText("Quante unità di AX-7 abbiamo? Chiedi al worker warehouse."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(res.Text, ".", ""), "314") {
		t.Fatalf("answer %q", res.Text)
	}
}

// TestLiveWorkflowAndBackground: a real model as workers of a two-step
// plan, then a background task whose report arrives later.
func TestLiveWorkflowAndBackground(t *testing.T) {
	model := liveModel(t)
	crew, err := delegate.NewCrew(delegate.Team{Config: agent.Config{Provider: model}, Workers: []delegate.Worker{{Name: "writer", Description: "writes short texts", System: "Answer in one short sentence."}}})
	if err != nil {
		t.Fatal(err)
	}
	defer crew.Close()
	r := &workflow.Runner{Crew: crew}
	plan := workflow.Plan{Steps: []workflow.Step{
		{Name: "number", Worker: "writer", Task: "Write only the number 314."},
		{Name: "sentence", Worker: "writer", Task: "Write one sentence that contains the number from the report you received.", Needs: []string{"number"}},
	}, Output: "sentence"}
	res, err := r.Run(context.Background(), "live", plan, "", false)
	if err != nil || !strings.Contains(res.Output, "314") {
		t.Fatalf("workflow %v %v", res, err)
	}
	raw := json.RawMessage(`{"worker":"writer","task":"Write the word ciao.","background":true}`)
	if _, err := crew.Tool().Call(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	f, ok := crew.Next(context.Background())
	if !ok || f.Err != nil || !strings.Contains(strings.ToLower(f.Report), "ciao") {
		t.Fatalf("background %+v %v", f, ok)
	}
}

// TestLiveReadHistory: a detail of an early result, left out of a small
// context, is still found by a real model (through read_history or by
// fetching again).
func TestLiveReadHistory(t *testing.T) {
	model := liveModel(t)
	type noArgs struct{}
	n := 0
	fetch := agent.NewTool("fetch_page", "Fetch the next page of the report.", func(context.Context, noArgs) (string, error) {
		n++
		body := strings.Repeat("filler text about nothing in particular. ", 120)
		if n == 1 {
			return "PAGE 1. The access code of the vault is 7391. " + body, nil
		}
		return "PAGE " + strconv.Itoa(n) + ". " + body, nil
	}, agent.ReadOnly())
	tools, err := agent.NewRegistry(fetch)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, ContextWindow: 7000, MaxTurns: 12})
	if err != nil {
		t.Fatal(err)
	}
	conv := agent.NewConversation()
	ctx := context.Background()
	if _, err := a.Run(ctx, conv, agent.UserText("Call fetch_page four times, one after the other, then say only 'done'.")); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, conv, agent.UserText("What is the access code of the vault written on page 1?"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "7391") {
		t.Fatalf("answer %q", res.Text)
	}
}

// TestLiveArtifacts: a real model publishes an artifact and another
// agent reads it back.
func TestLiveArtifacts(t *testing.T) {
	model := liveModel(t)
	store, err := artifact.NewLocal(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := agent.NewRegistry(artifact.Tools(store, artifact.Access{Scope: "proj", By: "writer", Publish: true})...)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := agent.New(agent.Config{Provider: model, Tools: pub, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := writer.Run(ctx, nil, agent.UserText("Publish an artifact named codes.md whose content is: 'vault code 7391'.")); err != nil {
		t.Fatal(err)
	}
	ro, err := agent.NewRegistry(artifact.Tools(store, artifact.Access{Scope: "proj", By: "reader"})...)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := agent.New(agent.Config{Provider: model, Tools: ro, MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	res, err := reader.Run(ctx, nil, agent.UserText("Read the artifact codes.md and tell me the vault code."))
	if err != nil || !strings.Contains(res.Text, "7391") {
		t.Fatalf("answer %q %v", res.Text, err)
	}
}

// TestLiveLoop: a real model asked to wait for a job that never finishes
// keeps checking; the detector warns it, refuses the next repeat, and the
// model ends with an answer instead of spending every turn.
func TestLiveLoop(t *testing.T) {
	model := liveModel(t)
	runs := 0
	status := agent.NewTool("job_status", "Status of the nightly export job.", func(context.Context, struct{}) (string, error) {
		runs++
		return "export job: still running", nil
	}, agent.ReadOnly())
	tools, err := agent.NewRegistry(status)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, MaxTurns: 12, Loop: agent.Loop{Action: agent.LoopBlock}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), nil, agent.UserText("Controlla lo stato del job di export con job_status finché non è finito, poi dimmi com'è andato."))
	if err != nil {
		t.Fatal(err)
	}
	if runs > 3 || res.Stop != agent.StopEnd {
		t.Fatalf("job_status ran %d times, stop %s, answer %q", runs, res.Stop, res.Text)
	}
	t.Logf("ran %d times, %d turns, answer %q", runs, res.Turns, res.Text)
}

// TestLiveEventLog: a real run logged through eventlog: every step is
// there, none of what was said.
func TestLiveEventLog(t *testing.T) {
	model := liveModel(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	type in struct {
		City string `json:"city"`
	}
	weather := agent.NewTool("weather", "Weather in a city.", func(_ context.Context, i in) (string, error) {
		return "Bologna-Meteo-Riservato: 18 gradi", nil
	})
	tools, err := agent.NewRegistry(weather)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, MaxTurns: 4, Observers: []agent.Observer{eventlog.Slog(logger)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("Che tempo fa a Bologna? Usa weather.")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, leak := range []string{"Bologna", "Riservato", "gradi", "tempo"} {
		if strings.Contains(out, leak) {
			t.Fatalf("content %q in the log:\n%s", leak, out)
		}
	}
	for _, want := range []string{"agent.started", "agent.model", "agent.tool_end", `"tool":"weather"`, "agent.done"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s:\n%s", want, out)
		}
	}
	t.Log("\n" + out)
}

// TestLiveFileEdit: a real model reads a file and changes one value with
// edit_file; the rest of the file stays as it was.
func TestLiveFileEdit(t *testing.T) {
	model := liveModel(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  host: localhost\n  port: 8080\n  timeout: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := files.New(files.Config{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(append(ws.ReadTools(), ws.WriteTools()...)...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, MaxTurns: 6})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("Nel file config.yaml cambia la porta da 8080 a 9090. Leggi il file e usa edit_file.")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil || string(data) != "server:\n  host: localhost\n  port: 9090\n  timeout: 30\n" {
		t.Fatalf("file now %q %v", data, err)
	}
}

// gitRepo makes a repository with one empty commit, an untracked
// notes.txt and a build tree.
func gitRepo(t *testing.T, env map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "start"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		for k, v := range env {
			cmd.Env = append(cmd.Environ(), k+"="+v)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "build", "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func bashAgent(t *testing.T, model agent.Provider, cfg bash.Config) agent.Agent {
	t.Helper()
	sh, err := bash.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })
	tools, err := agent.NewRegistry(sh.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Provider: model, Tools: tools, MaxTurns: 6})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestLiveBash: the model commits with git through bash in the sandbox,
// and a dangerous command waits for a yes that does not come.
func TestLiveBash(t *testing.T) {
	model := liveModel(t)
	if _, err := sandbox.New(); err != nil {
		t.Skip(err)
	}
	gitEnv := map[string]string{"GIT_AUTHOR_NAME": "T", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "T", "GIT_COMMITTER_EMAIL": "t@example.com"}
	dir := gitRepo(t, gitEnv)
	var asked atomic.Int32
	a := bashAgent(t, model, bash.Config{Dir: dir, Read: []string{}, Env: gitEnv, Confirm: func(context.Context, bash.Request) (bool, error) {
		asked.Add(1)
		return false, nil
	}})
	if _, err := a.Run(context.Background(), nil, agent.UserText("Nel repository git della cartella di lavoro aggiungi il file notes.txt e fai un commit con messaggio \"add notes\". Usa il tool bash.")); err != nil {
		t.Fatal(err)
	}
	log := exec.Command("git", "log", "--format=%s", "-1", "--", "notes.txt")
	log.Dir = dir
	if out, err := log.Output(); err != nil || !strings.Contains(strings.ToLower(string(out)), "add notes") {
		t.Fatalf("no commit of notes.txt: %q %v", out, err)
	}
	if _, err := a.Run(context.Background(), nil, agent.UserText("Cancella la cartella build con rm -rf build usando il tool bash.")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "out")); err != nil || asked.Load() == 0 {
		t.Fatalf("build: %v, asked %d", err, asked.Load())
	}
}
