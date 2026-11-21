// mem0_test.go: the memory against a fake Mem0, or a real one
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mem0

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// target is the server the tests talk to: the fake, or a real Mem0 when
// RINGIL_MEM0_URL is set (with its key in RINGIL_MEM0_KEY). The tests that
// lean on the fake's model run on the fake only.
func target(t *testing.T) (string, *fakeMem0) {
	t.Helper()
	if url := os.Getenv("RINGIL_MEM0_URL"); url != "" {
		return url, nil
	}
	f, srv := newFake(t)
	return srv.URL, f
}

func keyHeaders(context.Context) (http.Header, error) {
	h := http.Header{}
	if k := os.Getenv("RINGIL_MEM0_KEY"); k != "" {
		h.Set("X-API-Key", k)
	}
	return h, nil
}

var nonUser = regexp.MustCompile(`[^A-Za-z0-9]+`)

// user is a user_id of this test's own, so that runs on a real server do
// not meet.
func user(t *testing.T, suffix string) string {
	t.Helper()
	return "t-" + nonUser.ReplaceAllString(t.Name(), "") + "-" + suffix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func open(t *testing.T, url, who string, raw bool) agent.Memory {
	t.Helper()
	m, err := New(t.Context(), Config{URL: url, User: who, Raw: raw, Headers: keyHeaders})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { forgetAll(m) })
	return m
}

func forgetAll(m agent.Memory) {
	ctx := context.Background()
	recs, _ := m.Search(ctx, "", maxResults)
	for _, r := range recs {
		_ = m.Delete(ctx, r.ID)
	}
}

func put(t *testing.T, m agent.Memory, text string) agent.MemoryRecord {
	t.Helper()
	rec, err := m.Put(t.Context(), agent.MemoryRecord{Content: text})
	if err != nil {
		t.Fatalf("Put %q: %v", text, err)
	}
	return rec
}

func contents(recs []agent.MemoryRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Content
	}
	return out
}

func fakeOnly(t *testing.T, f *fakeMem0) {
	t.Helper()
	if f == nil {
		t.Skip("leans on the fake's model")
	}
}

func TestRawKeepsTheTextAndFindsIt(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	rec := put(t, m, "Il magazzino di Torino apre alle otto")
	put(t, m, "La riunione con Acme è giovedì")
	if rec.Content != "Il magazzino di Torino apre alle otto" || rec.ID == "" || rec.Created.IsZero() {
		t.Fatalf("stored %+v", rec)
	}
	got, err := m.Search(t.Context(), "magazzino Torino", 5)
	if err != nil || len(got) == 0 || got[0].ID != rec.ID {
		t.Fatalf("search: %q %v", contents(got), err)
	}
}

// The server's model may split a text: Put returns the first fact, and
// all are kept.
func TestPutKeepsTheFactsTheModelFinds(t *testing.T) {
	url, f := target(t)
	fakeOnly(t, f)
	m := open(t, url, user(t, "a"), false)
	rec := put(t, m, "Antonio vive a Roma. Lavora in Go.")
	if rec.Content != "Antonio vive a Roma" {
		t.Fatalf("first fact: %q", rec.Content)
	}
	all, _ := m.Search(t.Context(), "", 10)
	if len(all) != 2 {
		t.Fatalf("kept %q", contents(all))
	}
}

// A fact known already is not kept twice, and Put says nothing new was
// kept: never a record that only resembles it as if it were stored.
func TestNothingNewKeptIsSaid(t *testing.T) {
	url, f := target(t)
	fakeOnly(t, f)
	m := open(t, url, user(t, "a"), false)
	put(t, m, "Il nome in codice del progetto è Bluefin")
	for _, text := range []string{"Il nome in codice del progetto è Bluefin", "Qual è il nome in codice del progetto?", "ciao"} {
		got, err := m.Put(t.Context(), agent.MemoryRecord{Content: text})
		if !errors.HasCode(err, agent.ErrMemoryInput) || !strings.Contains(err.Error(), "Nothing new") {
			t.Errorf("%q: %+v %v", text, got, err)
		}
	}
	if all, _ := m.Search(t.Context(), "", 10); len(all) != 1 {
		t.Fatalf("kept %q", contents(all))
	}
}

// Raw keeps the same text once, as the other memories do (Mem0's raw add
// would store it again).
func TestRawKeepsTheSameTextOnce(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	first := put(t, m, "il backup gira alle due")
	again := put(t, m, "il backup gira alle due")
	if again.ID != first.ID {
		t.Fatalf("again %+v, first %+v", again, first)
	}
	if all, _ := m.Search(t.Context(), "", 10); len(all) != 1 {
		t.Fatalf("kept %q", contents(all))
	}
}

// Past the server's cap the most recent are said to be partial: its
// listing has no order, the newest may be left out.
func TestRecentPastTheCapIsPartial(t *testing.T) {
	f, srv := newFake(t)
	m, err := New(t.Context(), Config{URL: srv.URL, User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for range maxResults + 1 {
		f.keep("a", "nota", false)
	}
	f.mu.Unlock()
	got, err := m.Search(t.Context(), "", 5)
	if !errors.HasCode(err, agent.ErrMemoryPartial) || len(got) != 5 {
		t.Fatalf("%d records, %v", len(got), err)
	}
}

// An ID in capitals is the same UUID.
func TestIDsInEitherCase(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	rec := put(t, m, "una nota in maiuscolo")
	if _, err := m.Put(t.Context(), agent.MemoryRecord{ID: strings.ToUpper(rec.ID), Content: "una nota cambiata"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), strings.ToUpper(rec.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestRecentIsNewestFirst(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	for i := range 6 {
		put(t, m, "nota numero "+strconv.Itoa(i))
		time.Sleep(2 * time.Millisecond)
	}
	got, err := m.Search(t.Context(), "", 3)
	if err != nil || strings.Join(contents(got), ",") != "nota numero 5,nota numero 4,nota numero 3" {
		t.Fatalf("recent: %q %v", contents(got), err)
	}
}

func TestDelete(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	rec := put(t, m, "da dimenticare")
	if err := m.Delete(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), rec.ID); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
	if err := m.Delete(t.Context(), "not-a-mem0-id"); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("an ID Mem0 never makes: %v", err)
	}
}

// Put with an ID replaces that record's text and keeps its creation.
func TestReplaceKeepsCreation(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	rec := put(t, m, "la riunione è giovedì")
	got, err := m.Put(t.Context(), agent.MemoryRecord{ID: rec.ID, Content: "la riunione è venerdì"})
	if err != nil || got.ID != rec.ID || !got.Created.Equal(rec.Created) {
		t.Fatalf("replaced %+v from %+v: %v", got, rec, err)
	}
	all, _ := m.Search(t.Context(), "", 5)
	if len(all) != 1 || all[0].Content != "la riunione è venerdì" {
		t.Fatalf("after: %q", contents(all))
	}
}

// A record Put under an ID of the caller's (an import, a sync) is found,
// replaced and deleted by it, as in the other memories; also an ID shaped
// as Mem0's own.
func TestPutUnderTheCallersID(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), false) // extraction on: a named record is kept as given
	for _, id := range []string{"import-42", "00000000-0000-4000-8000-999999999999"} {
		underID(t, m, id)
	}
}

// underID makes, replaces, finds and deletes a record under id.
func underID(t *testing.T, m agent.Memory, id string) {
	t.Helper()
	made, err := m.Put(t.Context(), agent.MemoryRecord{ID: id, Content: "il fornitore delle viti è Bulloni Srl"})
	if err != nil || made.ID != id || made.Content != "il fornitore delle viti è Bulloni Srl" || made.Created.IsZero() {
		t.Fatalf("%s made %+v: %v", id, made, err)
	}
	again, err := m.Put(t.Context(), agent.MemoryRecord{ID: id, Content: "il fornitore delle viti è Dadi Spa"})
	if err != nil || again.ID != id || !again.Created.Equal(made.Created) {
		t.Fatalf("%s replaced %+v from %+v: %v", id, again, made, err)
	}
	foundAndDeleted(t, m, id, "il fornitore delle viti è Dadi Spa")
}

func foundAndDeleted(t *testing.T, m agent.Memory, id, content string) {
	t.Helper()
	all, _ := m.Search(t.Context(), "", 10)
	if len(all) != 1 || all[0].ID != id || all[0].Content != content {
		t.Fatalf("%s: %+v", id, all)
	}
	if got, _ := m.Search(t.Context(), "fornitore viti", 3); len(got) != 1 || got[0].ID != id {
		t.Fatalf("%s searched: %+v", id, got)
	}
	if err := m.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), id); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("%s deleted twice: %v", id, err)
	}
}

// Two processes making the same ID at once: one record stays, with the
// text of the Put that came last.
func TestTwoMakeTheSameID(t *testing.T) {
	f, srv := newFake(t)
	m, err := New(t.Context(), Config{URL: srv.URL, User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	f.onAdd = func() { // the other process got there first
		f.onAdd = nil
		f.twin("a", "import-9", "dall'altro processo")
	}
	got, err := m.Put(t.Context(), agent.MemoryRecord{ID: "import-9", Content: "da questo"})
	if err != nil || got.ID != "import-9" || got.Content != "da questo" {
		t.Fatalf("%+v %v", got, err)
	}
	if all, _ := m.Search(t.Context(), "", 5); len(all) != 1 || all[0].Content != "da questo" {
		t.Fatalf("%+v", all)
	}
}

// Twins a race left behind (a process that died between its add and its
// check) are one record: listed once, mended by the next Put, all gone
// with Delete.
func TestTwinsAreOneRecord(t *testing.T) {
	f, srv := newFake(t)
	m, err := New(t.Context(), Config{URL: srv.URL, User: "a"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.twin("a", "x", "vecchio A")
	f.twin("a", "x", "vecchio B")
	f.mu.Unlock()
	if all, _ := m.Search(t.Context(), "", 5); len(all) != 1 || all[0].Content != "vecchio A" {
		t.Fatalf("listed %+v", all)
	}
	if _, err := m.Put(t.Context(), agent.MemoryRecord{ID: "x", Content: "nuovo"}); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("%d records after Put", f.count())
	}
	f.mu.Lock()
	f.twin("a", "x", "ancora uno")
	f.mu.Unlock()
	if err := m.Delete(t.Context(), "x"); err != nil || f.count() != 0 {
		t.Fatalf("%d records after Delete: %v", f.count(), err)
	}
}

// The caller's IDs are kept exactly as written: two that differ in case
// are two records, also when shaped as a UUID.
func TestTheCallersIDsAsWritten(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	for _, id := range []string{"Nota", "nota", "AAAAAAAA-0000-4000-8000-000000000001"} {
		if got, err := m.Put(t.Context(), agent.MemoryRecord{ID: id, Content: "testo di " + id}); err != nil || got.ID != id {
			t.Fatalf("%s: %+v %v", id, got, err)
		}
	}
	if all, _ := m.Search(t.Context(), "", 10); len(all) != 3 {
		t.Fatalf("%+v", all)
	}
	if err := m.Delete(t.Context(), "AAAAAAAA-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
}

// Two users on one server: neither finds, changes or deletes the other's.
func TestUsersAreApart(t *testing.T) {
	url, _ := target(t)
	a := open(t, url, user(t, "a"), true)
	b := open(t, url, user(t, "b"), true)
	secret := put(t, a, "il codice del cancello è 4512")
	if got, _ := b.Search(t.Context(), "codice cancello", 5); len(got) != 0 {
		t.Fatalf("b found a's: %q", contents(got))
	}
	if got, _ := b.Search(t.Context(), "", 5); len(got) != 0 {
		t.Fatalf("b lists a's: %q", contents(got))
	}
	if err := b.Delete(t.Context(), secret.ID); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("b deleting a's: %v", err)
	}
	if _, err := b.Put(t.Context(), agent.MemoryRecord{ID: secret.ID, Content: "cambiato"}); err != nil {
		t.Fatalf("b putting under a's ID: %v", err) // b's own record, under that ID
	}
	named := put(t, a, "x")
	if _, err := a.Put(t.Context(), agent.MemoryRecord{ID: "shared-1", Content: "di a"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(t.Context(), "shared-1"); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("b deleting a's named record: %v", err)
	}
	_ = a.Delete(t.Context(), named.ID)
	_ = a.Delete(t.Context(), "shared-1")
	if got, _ := a.Search(t.Context(), "", 5); len(got) != 1 || got[0].Content != secret.Content {
		t.Fatalf("a's record after b: %q", contents(got))
	}
}

func TestAnUnrelatedQuestionFindsNothing(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	put(t, m, "Il magazzino di Torino apre alle otto")
	if got, err := m.Search(t.Context(), "ricetta della torta di mele", 5); err != nil || len(got) != 0 {
		t.Fatalf("unrelated: %q %v", contents(got), err)
	}
}

func TestConfigRefused(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no user":     {URL: "http://127.0.0.1:1"},
		"space":       {URL: "http://127.0.0.1:1", User: "a b"},
		"slash":       {URL: "http://127.0.0.1:1", User: "a/b"},
		"long":        {URL: "http://127.0.0.1:1", User: strings.Repeat("a", 129)},
		"http remote": {URL: "http://mem0.example.com", User: "a"},
		"score":       {URL: "http://127.0.0.1:1", User: "a", MinScore: 1.5},
	} {
		if _, err := New(t.Context(), cfg); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheKeyGoesWithEveryCall(t *testing.T) {
	f, srv := newFake(t)
	f.key = "s3cret"
	if _, err := New(t.Context(), Config{URL: srv.URL, User: "a"}); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("without the key: %v", err)
	}
	headers := func(context.Context) (http.Header, error) { return http.Header{"X-Api-Key": {"s3cret"}}, nil }
	m, err := New(t.Context(), Config{URL: srv.URL, User: "a", Raw: true, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	rec := put(t, m, "un fatto")
	if _, err := m.Search(t.Context(), "fatto", 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
}

func TestServerTroubleIsAnIOError(t *testing.T) {
	url := hostile(t, http.StatusBadGateway, `{"detail":"The LLM provider is unavailable.","code":"upstream_unavailable"}`)
	_, err := New(t.Context(), Config{URL: url, User: "a"})
	if !errors.HasCode(err, agent.ErrMemoryIO) || !strings.Contains(err.Error(), "LLM provider") {
		t.Fatalf("a 502: %v", err)
	}
	if _, err := New(t.Context(), Config{URL: "http://127.0.0.1:1", User: "a"}); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Fatalf("no server: %v", err)
	}
}

// Text is kept composed (NFC), as by every memory: either form of an
// accent finds it.
func TestEitherFormOfAnAccent(t *testing.T) {
	url, _ := target(t)
	m := open(t, url, user(t, "a"), true)
	rec := put(t, m, "caffè alle dieci")
	if rec.Content != "caffè alle dieci" {
		t.Fatalf("kept %q", rec.Content)
	}
	if got, _ := m.Search(t.Context(), "caffè", 3); len(got) != 1 {
		t.Fatalf("composed question: %q", contents(got))
	}
}
