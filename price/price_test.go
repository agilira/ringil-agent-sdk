// price_test.go: tests for the cost of a model call and the catalog
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package price

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// fixture is an extract of OpenRouter's real list (25/11/2026), with two
// vendors sharing a model name at different prices.
func fixture(t testing.TB) Table {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openrouter.json"))
	if err != nil {
		t.Fatal(err)
	}
	tab, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func lookup(t *testing.T, tab Table, name string) Price {
	t.Helper()
	p, ok := tab.Lookup(name)
	if !ok {
		t.Fatalf("%s not priced", name)
	}
	return p
}

// The worked example of Anthropic's pricing page: Opus 5.5, 50,000 input
// tokens of which 40,000 read from the cache, 15,000 output: $0.348.
func TestCostMatchesTheVendorExample(t *testing.T) {
	p := lookup(t, fixture(t), "claude-opus-5-5")
	if got := p.Cost(agent.Usage{Input: 50_000, CacheRead: 40_000, Output: 15_000}); !near(got, 0.348) {
		t.Fatalf("cost %v, want 0.348", got)
	}
	if got := p.Cost(agent.Usage{Input: 1_000_000, CacheWrite: 1_000_000}); !near(got, 5) {
		t.Fatalf("cache write %v, want 5", got)
	}
}

// Haiku 5.5: a prompt of 100,000 tokens or more pays the higher price on
// all of it, the cached part included.
func TestLongPrompt(t *testing.T) {
	p := lookup(t, fixture(t), "claude-haiku-5-5")
	if got := p.Cost(agent.Usage{Input: 99_999}); !near(got, 0.0099999) {
		t.Fatalf("short %v", got)
	}
	got := p.Cost(agent.Usage{Input: 200_000, CacheRead: 100_000, Output: 1_000_000})
	if want := 0.05 + 0.005 + 2.5; !near(got, want) {
		t.Fatalf("long %v, want %v", got, want)
	}
}

func TestTiersInOrder(t *testing.T) {
	b := []byte(`{"data":[{"id":"v/m","pricing":{"prompt":"0.000001","completion":"0.000001","overrides":[
		{"min_prompt_tokens":500000,"prompt":"0.000003"},{"min_prompt_tokens":100000,"prompt":"0.000002"}]}}]}`)
	tab, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	p := lookup(t, tab, "m")
	for input, perM := range map[int]float64{1000: 1, 100_000: 2, 499_999: 2, 500_000: 3} {
		if got := p.Cost(agent.Usage{Input: input}); !near(got, float64(input)*perM/1e6) {
			t.Errorf("%d tokens: %v", input, got)
		}
	}
	if p.Long.CacheRead != 2 || p.Long.Output != 1 {
		t.Errorf("a tier inherits what it does not set: %+v", *p.Long)
	}
}

// Usage that does not add up never makes a negative cost.
func TestInconsistentUsage(t *testing.T) {
	p := Price{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}
	for _, u := range []agent.Usage{{Input: 10, CacheRead: 20}, {Input: -5, Output: -5, CacheRead: -5, CacheWrite: -5}} {
		if c := p.Cost(u); c < 0 {
			t.Errorf("%+v cost %v", u, c)
		}
	}
}

func TestLookup(t *testing.T) {
	tab := fixture(t)
	for name, want := range map[string]string{
		"claude-opus-5-5":           "anthropic/claude-opus-5-5",
		"anthropic/claude-opus-5.5": "anthropic/claude-opus-5-5",
		"grok-4.7":                  "x-ai/grok-4-7",
		"glm-5.3":                   "z-ai/glm-5-3",
		"kimi-k3":                   "moonshotai/kimi-k3",
		"gemini-4-carbon":           "google/gemini-4-carbon",
		"GPT-5":                     "openai/gpt-5",
		"gpt-4o-2024-05-13":         "openai/gpt-4o-2024-05-13", // a snapshot priced as itself
		"gpt-4o-2024-08-06":         "openai/gpt-4o",
		"vendor-a/shared-7b":        "vendor-a/shared-7b",
	} {
		if got := lookup(t, tab, name); got != tab[want] {
			t.Errorf("%s: %+v, want the price of %s", name, got, want)
		}
	}
	// Never a model at another's price: unknown, routers, variants, and
	// a name two vendors share.
	for _, name := range []string{"claude-opus-5-7", "auto", "openrouter/auto", "claude-opus-5-5:batch", "shared-7b", "", "/", "-20251001"} {
		if p, ok := tab.Lookup(name); ok {
			t.Errorf("%q priced as %+v", name, p)
		}
	}
	if near(lookup(t, tab, "claude-opus-5-5").Input, 2) {
		t.Error("the batch variant replaced the model")
	}
}

func TestOver(t *testing.T) {
	tab := fixture(t)
	mine := Table{"gpt-5": {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}}
	got := tab.Over(mine)
	if lookup(t, got, "gpt-5").Input != 1 || lookup(t, tab, "gpt-5").Input == 1 {
		t.Fatal("the host's price must win, in a new table")
	}
}

func TestParseRejects(t *testing.T) {
	for _, b := range []string{``, `{}`, `{"data":[]}`, `{"data":[{"id":"a/b","pricing":{"prompt":"x","completion":"1"}}]}`,
		`{"data":[{"id":"a/b","pricing":{"prompt":"2","completion":"0.1"}}]}`} {
		if _, err := Parse([]byte(b)); !errors.HasCode(err, ErrCatalog) {
			t.Errorf("%s: %v", b, err)
		}
	}
}

func TestWrapPricesEachCall(t *testing.T) {
	pr := Price{Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5}
	p := Wrap(fake.New(
		fake.Step{Chunks: []string{"a"}, Usage: agent.Usage{Input: 1_000_000}},
		fake.Step{Chunks: []string{"b"}, Usage: agent.Usage{Input: 1_000_000, Cost: 0.01}},
		fake.Step{Chunks: []string{"c"}, Usage: agent.Usage{Input: 1_000_000, Cost: -3}},
	), pr)
	if p.Info() != fake.New().Info() {
		t.Fatal("Info not passed through")
	}
	for i, want := range []float64{2, 0.01, 2} {
		r, err := p.Generate(context.Background(), &agent.Request{}, nil)
		if err != nil || !near(r.Usage.Cost, want) {
			t.Fatalf("call %d: %+v %v, want %v", i, r, err, want)
		}
	}
	if resp, err := Wrap(fake.New(fake.Step{Err: context.Canceled}), pr).Generate(context.Background(), &agent.Request{}, nil); err == nil || resp != nil {
		t.Fatalf("%+v %v", resp, err)
	}
}

// server serves the fixture and counts the downloads.
func server(t *testing.T, status *atomic.Int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openrouter.json"))
	if err != nil {
		t.Fatal(err)
	}
	hits := &atomic.Int32{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if code := status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// The list is downloaded once, used from the file while it is fresh,
// downloaded again when old, and the old copy serves when the download
// fails.
func TestCatalogCaches(t *testing.T) {
	status := &atomic.Int32{}
	srv, hits := server(t, status)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	c := &Catalog{Path: filepath.Join(t.TempDir(), "p", "prices.json"), URL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }}
	load := func(wantHits int32, wantErr bool) {
		t.Helper()
		tab, err := c.Load(context.Background())
		if (err != nil) != wantErr || len(tab) == 0 || hits.Load() != wantHits {
			t.Fatalf("%d models, %v, %d downloads (want %d)", len(tab), err, hits.Load(), wantHits)
		}
	}
	load(1, false)
	load(1, false)
	if fi, err := os.Stat(c.Path); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 { // Windows has no such bits
		t.Fatalf("file %v %v", fi, err)
	}
	now = now.Add(25 * time.Hour)
	status.Store(http.StatusBadGateway)
	load(2, true) // the saved copy, with the error
	status.Store(0)
	load(3, false)
}

func TestCatalogNeedsHTTPS(t *testing.T) {
	c := &Catalog{Path: filepath.Join(t.TempDir(), "prices.json"), URL: "http://example.com/models"}
	if _, err := c.Load(context.Background()); !errors.HasCode(err, ErrCatalog) {
		t.Fatalf("plain HTTP: %v", err)
	}
}

// A file saved from another list is not used for this one.
func TestCatalogSourceChecked(t *testing.T) {
	status := &atomic.Int32{}
	srv, hits := server(t, status)
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := writeSaved(path, saved{Fetched: time.Now(), Source: "https://elsewhere.example/models", Models: Table{"x": {}}}); err != nil {
		t.Fatal(err)
	}
	c := &Catalog{Path: path, URL: srv.URL, Client: srv.Client()}
	if _, err := c.Load(context.Background()); err != nil || hits.Load() != 1 {
		t.Fatalf("%v, %d downloads", err, hits.Load())
	}
}

func TestOverNormalizesKeys(t *testing.T) {
	tab := fixture(t)
	mine := Table{"grok-4.7": {Input: 9, Output: 9, CacheRead: 9, CacheWrite: 9}}
	got := tab.Over(mine)
	for _, name := range []string{"grok-4.7", "grok-4-7", "GROK-4.7"} {
		if lookup(t, got, name).Input != 9 {
			t.Errorf("%s did not pick up normalized override", name)
		}
	}
}

func TestCostRejectsCyclesAndBadRates(t *testing.T) {
	cyc := Price{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, LongFrom: 1}
	cyc.Long = &cyc
	if got := cyc.Cost(agent.Usage{Input: 1_000_000}); !near(got, 1) {
		t.Fatalf("cyclic Long cost = %v, want 1", got)
	}
	for _, bad := range []Price{{Input: -1}, {Input: math.NaN()}, {Input: math.Inf(1)}} {
		if got := bad.Cost(agent.Usage{Input: 100}); got != 0 {
			t.Errorf("bad price %+v gave cost %v", bad, got)
		}
	}
	p := Wrap(fake.New(fake.Step{Chunks: []string{"x"}, Usage: agent.Usage{Input: 1_000_000, Cost: math.Inf(1)}}), Price{Input: 2})
	if r, err := p.Generate(context.Background(), &agent.Request{}, nil); err != nil || !near(r.Usage.Cost, 2) {
		t.Fatalf("Wrap kept +Inf cost: %+v %v", r, err)
	}
}

func TestCatalogRejectsInvalidSavedFile(t *testing.T) {
	status := &atomic.Int32{}
	srv, hits := server(t, status)
	path := filepath.Join(t.TempDir(), "prices.json")
	tampered := `{"fetched":"2099-01-01T00:00:00Z","source":"` + srv.URL + `","models":{"x":{"input":-1}}}`
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Catalog{Path: path, URL: srv.URL, Client: srv.Client()}
	if _, err := c.Load(context.Background()); err != nil || hits.Load() != 1 {
		t.Fatalf("tampered saved file not redownloaded: err=%v hits=%d", err, hits.Load())
	}
}

func TestCatalogRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := writeSaved(target, saved{Fetched: time.Now(), Source: "https://example.com", Models: Table{"x": {Input: 1}}}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "prices.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := readSaved(link); err == nil {
		t.Fatal("symlink accepted by readSaved")
	}
}

// A price built by hand that loops back on itself, or has rates that are
// not amounts, never hangs and never yields a bad cost.
func TestHandMadePriceIsSafe(t *testing.T) {
	loop := Price{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, LongFrom: 10}
	loop.Long = &loop
	if got := loop.Cost(agent.Usage{Input: 1_000_000}); !near(got, 1) {
		t.Fatalf("loop %v", got)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		if got := (Price{Input: bad, Output: 1}).Cost(agent.Usage{Input: 10, Output: 10}); got != 0 {
			t.Errorf("rate %v: cost %v", bad, got)
		}
	}
}
