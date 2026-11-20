// chroma_test.go: the memory against a fake Chroma, or a real one
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chroma

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// conceptEmbedder puts words of one meaning on one axis, so that tests
// know what is near what.
type conceptEmbedder struct {
	model string
	fail  atomic.Bool
	calls atomic.Int32
}

var concepts = map[string]int{
	"riunione": 0, "meeting": 0, "incontro": 0,
	"cliente": 1, "acme": 1, "customer": 1,
	"fattura": 2, "invoice": 2, "pagamento": 2,
	"gatto": 3, "cat": 3,
}

func (e *conceptEmbedder) Model() string { return e.model }

func (e *conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	if e.fail.Load() {
		return nil, errors.New(agent.ErrEmbed, "down")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 5)
		v[4] = 0.01
		for _, w := range strings.Fields(strings.ToLower(t)) {
			if c, ok := concepts[strings.Trim(w, ".,:?")]; ok {
				v[c]++
			}
		}
		out[i] = v
	}
	return out, nil
}

// target is the server the tests use: RINGIL_CHROMA_URL when set (each
// test with collections of its own, removed after), else the fake.
func target(t *testing.T) (url string, fake *fakeChroma) {
	t.Helper()
	if u := os.Getenv("RINGIL_CHROMA_URL"); u != "" {
		return u, nil
	}
	f, srv := newFake(t)
	return srv.URL, f
}

// collectionName is unique to the test, and removed from a real server.
func collectionName(t *testing.T, url, suffix string) string {
	t.Helper()
	name := "ringil-test-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())) + "-" + suffix + "-" + itoa(int(time.Now().UnixNano()%1e9))
	if os.Getenv("RINGIL_CHROMA_URL") != "" {
		t.Cleanup(func() { dropCollection(url, name) })
	}
	return name
}

func dropCollection(url, name string) {
	req, err := http.NewRequest(http.MethodDelete, url+"/api/v2/tenants/default_tenant/databases/default_database/collections/"+name, nil)
	if err == nil {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
}

func open(t *testing.T, url, name string, emb agent.Embedder) agent.Memory {
	t.Helper()
	m, err := New(t.Context(), Config{URL: url, Collection: name, Embedder: emb})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func put(t *testing.T, m agent.Memory, text string) agent.MemoryRecord {
	t.Helper()
	r, err := m.Put(t.Context(), agent.MemoryRecord{Content: text})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func contents(recs []agent.MemoryRecord) string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Content)
	}
	return strings.Join(out, " | ")
}

func TestFoundByMeaningAndByWords(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, collectionName(t, url, "m"), &conceptEmbedder{model: "concepts"})
	put(t, m, "Riunione con Acme martedì alle 10")
	put(t, m, "La fattura di ottobre è pagata")
	put(t, m, "Il wifi di casa si chiama Nebbia")

	got, err := m.Search(t.Context(), "quando è l'incontro col cliente?", 3)
	if err != nil || len(got) == 0 || !strings.Contains(got[0].Content, "Acme") {
		t.Fatalf("by meaning: %q %v", contents(got), err)
	}
	got, err = m.Search(t.Context(), "NEBBIA", 3)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Content, "wifi") {
		t.Fatalf("by words, in any case: %q %v", contents(got), err)
	}
	if got, err := m.Search(t.Context(), "il gatto", 3); err != nil || len(got) != 0 {
		t.Fatalf("an unrelated question recalled: %q %v", contents(got), err)
	}
}

func TestRecentAndDelete(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, collectionName(t, url, "r"), &conceptEmbedder{model: "concepts"})
	var ids []string
	for _, text := range []string{"primo", "secondo", "terzo"} {
		ids = append(ids, put(t, m, text).ID)
		time.Sleep(5 * time.Millisecond)
	}
	got, err := m.Search(t.Context(), "", 2)
	if err != nil || contents(got) != "terzo | secondo" {
		t.Fatalf("most recent: %q %v", contents(got), err)
	}
	if err := m.Delete(t.Context(), ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), ids[2]); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
	if err := m.Delete(t.Context(), "../x"); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("a bad ID: %v", err)
	}
	if got, _ := m.Search(t.Context(), "", 5); contents(got) != "secondo | primo" {
		t.Fatalf("after delete: %q", contents(got))
	}
}

func TestPutKeepsOneRecordPerFact(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, collectionName(t, url, "p"), &conceptEmbedder{model: "concepts"})
	a := put(t, m, "Antonio preferisce risposte brevi")
	b := put(t, m, "  Antonio preferisce risposte brevi ")
	if a.ID != b.ID {
		t.Fatalf("the same fact twice made two records: %s %s", a.ID, b.ID)
	}
	time.Sleep(5 * time.Millisecond)
	c, err := m.Put(t.Context(), agent.MemoryRecord{ID: a.ID, Content: "Antonio preferisce risposte corte"})
	if err != nil || !c.Created.Equal(a.Created) {
		t.Fatalf("an update by ID changed its creation: %v %v %v", c.Created, a.Created, err)
	}
	if got, _ := m.Search(t.Context(), "", 5); contents(got) != "Antonio preferisce risposte corte" {
		t.Fatalf("records: %q", contents(got))
	}
	for _, bad := range []agent.MemoryRecord{{Content: " "}, {Content: strings.Repeat("x", agent.MaxMemoryContent+1)}, {ID: "a b", Content: "x"}, {Content: "a\x00b"}} {
		if _, err := m.Put(t.Context(), bad); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("accepted %.20q: %v", bad.Content, err)
		}
	}
}

func TestEmbedderDown(t *testing.T) {
	url, _ := target(t)
	emb := &conceptEmbedder{model: "concepts"}
	m := open(t, url, collectionName(t, url, "d"), emb)
	put(t, m, "Riunione con Acme martedì")
	emb.fail.Store(true)
	if _, err := m.Put(t.Context(), agent.MemoryRecord{Content: "La fattura è pagata"}); !errors.HasCode(err, agent.ErrEmbed) {
		t.Fatalf("stored without a vector: %v", err)
	}
	got, err := m.Search(t.Context(), "acme", 3)
	if !errors.HasCode(err, agent.ErrMemoryPartial) || len(got) != 1 {
		t.Fatalf("by words while down: %q %v", contents(got), err)
	}
	calls := emb.calls.Load()
	_, _ = m.Search(t.Context(), "acme", 3)
	if emb.calls.Load() != calls {
		t.Error("the embedder was called again during its pause")
	}
}

// Vectors of two models cannot be compared: a collection holding one
// model's is refused to another; an empty one is taken.
func TestOneModelPerCollection(t *testing.T) {
	url, _ := target(t)
	name := collectionName(t, url, "x")
	open(t, url, name, &conceptEmbedder{model: "first"})
	if _, err := New(t.Context(), Config{URL: url, Collection: name, Embedder: &conceptEmbedder{model: "second"}}); err != nil {
		t.Fatalf("an empty collection was refused to another model: %v", err)
	}
	m := open(t, url, name, &conceptEmbedder{model: "second"})
	put(t, m, "un fatto")
	_, err := New(t.Context(), Config{URL: url, Collection: name, Embedder: &conceptEmbedder{model: "first"}})
	if !errors.HasCode(err, agent.ErrMemoryInput) || !strings.Contains(err.Error(), "second") {
		t.Fatalf("a collection of another model: %v", err)
	}
}

func TestCollectionsAreApart(t *testing.T) {
	url, _ := target(t)
	a := open(t, url, collectionName(t, url, "a"), &conceptEmbedder{model: "concepts"})
	b := open(t, url, collectionName(t, url, "b"), &conceptEmbedder{model: "concepts"})
	put(t, a, "Riunione con Acme")
	if got, _ := b.Search(t.Context(), "acme riunione", 5); len(got) != 0 {
		t.Fatalf("another collection's memories: %q", contents(got))
	}
	if got, _ := b.Search(t.Context(), "", 5); len(got) != 0 {
		t.Fatalf("another collection's recent: %q", contents(got))
	}
}

func TestConfigRefused(t *testing.T) {
	emb := &conceptEmbedder{model: "m"}
	for name, cfg := range map[string]Config{
		"no embedder":      {URL: "http://127.0.0.1:1", Collection: "abc"},
		"short name":       {URL: "http://127.0.0.1:1", Collection: "ab", Embedder: emb},
		"bad name":         {URL: "http://127.0.0.1:1", Collection: "a/b/c", Embedder: emb},
		"dots":             {URL: "http://127.0.0.1:1", Collection: "a..b", Embedder: emb},
		"http not local":   {URL: "http://chroma.example.com", Collection: "abc", Embedder: emb},
		"credentials":      {URL: "https://u:p@chroma.example.com", Collection: "abc", Embedder: emb},
		"not a URL scheme": {URL: "ftp://chroma.example.com", Collection: "abc", Embedder: emb},
	} {
		if _, err := New(t.Context(), cfg); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The fake only: what a real server is not made to do.

func TestTheTokenGoesWithEveryCall(t *testing.T) {
	f, srv := newFake(t)
	f.token = "s3cret"
	headers := func(context.Context) (http.Header, error) { return http.Header{"X-Chroma-Token": {"s3cret"}}, nil }
	m, err := New(t.Context(), Config{URL: srv.URL, Collection: "tok", Embedder: &conceptEmbedder{model: "m"}, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	put(t, m, "un fatto")
	if _, err := New(t.Context(), Config{URL: srv.URL, Collection: "tok", Embedder: &conceptEmbedder{model: "m"}}); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("without the token: %v", err)
	}
}

func TestServerTroubleIsAnIOError(t *testing.T) {
	_, err := New(t.Context(), Config{URL: "http://127.0.0.1:1", Collection: "abc", Embedder: &conceptEmbedder{model: "m"}})
	if !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("unreachable: %v", err)
	}
	f, fsrv := newFake(t)
	m := open(t, fsrv.URL, "abc", &conceptEmbedder{model: "m"})
	f.mu.Lock()
	f.byID = map[string]*fakeCollection{} // the collection vanished
	f.mu.Unlock()
	if _, err := m.Search(t.Context(), "", 3); !errors.HasCode(err, agent.ErrMemoryIO) || !strings.Contains(err.Error(), "collection not found") {
		t.Fatalf("a server error: %v", err)
	}
}

// Audit 20/11: a rare word is not crowded out by a common one, whatever
// the order the server answers in.
func TestARareWordIsFound(t *testing.T) {
	url, _ := target(t)
	emb := &conceptEmbedder{model: "concepts"}
	m := open(t, url, collectionName(t, url, "rare"), emb)
	for i := range 250 {
		put(t, m, "project note "+itoa(i))
	}
	put(t, m, "project zebra")
	emb.fail.Store(true) // by words only
	got, _ := m.Search(t.Context(), "project zebra", 3)
	if len(got) == 0 || got[0].Content != "project zebra" {
		t.Fatalf("the rare word: %q", contents(got))
	}
}

// Audit 20/11: words are split as Terms splits them: "_" is not a letter.
func TestWordsAroundUnderscores(t *testing.T) {
	url, _ := target(t)
	emb := &conceptEmbedder{model: "concepts"}
	m := open(t, url, collectionName(t, url, "us"), emb)
	put(t, m, "the figures are in report_final.xlsx")
	put(t, m, "non so perché")
	emb.fail.Store(true)
	for _, q := range []string{"report", "PERCHÉ"} {
		if got, _ := m.Search(t.Context(), q, 3); len(got) != 1 {
			t.Errorf("%s: %q", q, contents(got))
		}
	}
}

// Audit 20/11: two memories on one collection remember the same fact as
// one record.
func TestTwoProcessesOneFact(t *testing.T) {
	url, _ := target(t)
	name := collectionName(t, url, "two")
	a := open(t, url, name, &conceptEmbedder{model: "concepts"})
	b := open(t, url, name, &conceptEmbedder{model: "concepts"})
	ra, rb := put(t, a, "Antonio usa Go"), put(t, b, "Antonio usa Go")
	if ra.ID != rb.ID {
		t.Fatalf("two records: %s %s", ra.ID, rb.ID)
	}
	if got, _ := a.Search(t.Context(), "", 5); len(got) != 1 {
		t.Fatalf("records: %q", contents(got))
	}
}

// Audit 20/11: the newest are found past any page.
func TestRecentPastAPage(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, collectionName(t, url, "many"), &conceptEmbedder{model: "concepts"})
	for i := range page + 5 {
		put(t, m, "old "+itoa(i))
	}
	time.Sleep(5 * time.Millisecond)
	put(t, m, "the newest")
	if got, err := m.Search(t.Context(), "", 1); err != nil || len(got) != 1 || got[0].Content != "the newest" {
		t.Fatalf("%q %v", contents(got), err)
	}
}

// An accent written as one character finds it written as two, and back.
func TestEitherFormOfAnAccent(t *testing.T) {
	url, _ := target(t)
	emb := &conceptEmbedder{model: "concepts"}
	m := open(t, url, collectionName(t, url, "nfc"), emb)
	put(t, m, "non so perché")
	emb.fail.Store(true) // by words
	for _, q := range []string{"perché", "perché"} {
		if got, _ := m.Search(t.Context(), q, 3); len(got) != 1 {
			t.Errorf("%q: %q", q, contents(got))
		}
	}
}
