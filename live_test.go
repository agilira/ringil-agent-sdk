// live_test.go: typed tools against a real model
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package agent_test

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/memory"
	"github.com/agilira/ringil-agent-sdk/memory/local"
	"github.com/agilira/ringil-agent-sdk/memory/vector"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/provider/budget"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
	"github.com/agilira/ringil-agent-sdk/rules"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/todo"
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
	if !strings.Contains(strings.ReplaceAll(res.Text, ".", ""), "4200") {
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

// TestLiveEmbeddings: a real embedding model finds a memory by meaning.
// Cost: a handful of short texts, about $0.00002 with
// text-embedding-3-small.
func TestLiveEmbeddings(t *testing.T) {
	base, model := os.Getenv("RINGIL_BASE_URL"), os.Getenv("RINGIL_EMBED_MODEL")
	if base == "" || model == "" {
		t.Skip("RINGIL_EMBED_MODEL not set")
	}
	mem := liveVectorMemory(t, base, model)
	ctx := context.Background()
	for _, fact := range []string{"Il gatto di Antonio si chiama Lira.", "La riunione settimanale con Acme è il martedì alle 10.", "La fattura 42 va pagata entro venerdì."} {
		if _, err := mem.Put(ctx, agent.MemoryRecord{Content: fact}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mem.Search(ctx, "quando ci vediamo con il cliente?", 1)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Content, "Acme") {
		t.Fatalf("by meaning: %v %v", got, err)
	}
}

func liveVectorMemory(t *testing.T, base, model string) agent.Memory {
	t.Helper()
	emb, err := openai.NewEmbedder(openai.Config{BaseURL: base, Model: model, APIKey: os.Getenv("RINGIL_API_KEY"), EmbedInputType: os.Getenv("RINGIL_EMBED_INPUT_TYPE") == "1"})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := vector.New(filepath.Join(t.TempDir(), "mem"), emb)
	if err != nil {
		t.Fatal(err)
	}
	return mem
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
	crew, err := delegate.NewCrew(delegate.Team{Provider: model, Workers: []delegate.Worker{{Name: "warehouse", Description: "knows stock levels; it has the stock_level tool", Tools: wtools}}})
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
	res, err := a.Run(context.Background(), nil, agent.UserText("Quante unità di AX-7 abbiamo? Chiedi al worker warehouse."))
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
	crew, err := delegate.NewCrew(delegate.Team{Provider: model, Workers: []delegate.Worker{{Name: "writer", Description: "writes short texts", System: "Answer in one short sentence."}}})
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
