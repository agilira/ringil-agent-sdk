// sqlite_test.go: the memory in a SQLite database
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type conceptEmbedder struct {
	model string
	fail  atomic.Bool
	calls atomic.Int32
	texts atomic.Int32
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
	e.texts.Add(int32(len(texts)))
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

func openIn(t *testing.T, dir string, emb agent.Embedder) *Store {
	t.Helper()
	s, err := New(dir, emb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
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
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Content)
	}
	return strings.Join(out, " | ")
}

func TestFoundByWords(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "the figures are in report_final.xlsx")
	put(t, m, "non so perché")
	put(t, m, "Il wifi di casa si chiama Nebbia")
	for q, want := range map[string]string{"report": "report_final", "PERCHÉ": "perché", "nebbia wifi": "Nebbia", "AND OR NEAR": ""} {
		got, err := m.Search(t.Context(), q, 3)
		if err != nil || (want == "") != (len(got) == 0) || (want != "" && !strings.Contains(got[0].Content, want)) {
			t.Errorf("%s: %q %v", q, contents(got), err)
		}
	}
	if got, _ := m.Search(t.Context(), "perche", 3); len(got) != 0 {
		t.Errorf("accents are kept, as in memory/local: %q", contents(got))
	}
}

func TestFoundByMeaning(t *testing.T) {
	m := openIn(t, t.TempDir(), &conceptEmbedder{model: "concepts"})
	put(t, m, "Riunione con Acme martedì alle 10")
	put(t, m, "La fattura di ottobre è pagata")
	got, err := m.Search(t.Context(), "quando è l'incontro col cliente?", 3)
	if err != nil || len(got) == 0 || !strings.Contains(got[0].Content, "Acme") {
		t.Fatalf("by meaning: %q %v", contents(got), err)
	}
	if got, err := m.Search(t.Context(), "il gatto", 3); err != nil || len(got) != 0 {
		t.Fatalf("an unrelated question recalled: %q %v", contents(got), err)
	}
}

func TestRecentPutAndDelete(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { now = now.Add(time.Second); return now }
	a, b := put(t, m, "primo"), put(t, m, "secondo")
	put(t, m, "terzo")
	if got, _ := m.Search(t.Context(), "", 2); contents(got) != "terzo | secondo" {
		t.Fatalf("recent: %q", contents(got))
	}
	if again := put(t, m, "  primo "); again.ID != a.ID || !again.Created.Equal(a.Created) {
		t.Fatalf("the same fact twice: %+v %+v", again, a)
	}
	upd, err := m.Put(t.Context(), agent.MemoryRecord{ID: b.ID, Content: "secondo, corretto"})
	if err != nil || !upd.Created.Equal(b.Created) {
		t.Fatalf("an update kept its creation: %+v %v", upd, err)
	}
	checkDelete(t, m, a.ID)
}

func checkDelete(t *testing.T, m *Store, id string) {
	t.Helper()
	if err := m.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(t.Context(), id); !errors.HasCode(err, agent.ErrMemoryNotFound) {
		t.Fatalf("twice: %v", err)
	}
	if got, _ := m.Search(t.Context(), "", 5); contents(got) != "terzo | secondo, corretto" {
		t.Fatalf("after: %q", contents(got))
	}
}

func TestBadRecordsAreRefused(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	for _, bad := range []agent.MemoryRecord{{Content: " "}, {Content: strings.Repeat("x", agent.MaxMemoryContent+1)}, {ID: "a b", Content: "x"}, {Content: "a\x00b"}} {
		if _, err := m.Put(t.Context(), bad); !errors.HasCode(err, agent.ErrMemoryInput) {
			t.Errorf("accepted %.20q: %v", bad.Content, err)
		}
	}
}

// With the embedder down, Put still keeps the fact, and Search goes by
// words and says so; once it is back, the missing vectors are made.
func TestEmbedderDown(t *testing.T) {
	emb := &conceptEmbedder{model: "concepts"}
	m := openIn(t, t.TempDir(), emb)
	emb.fail.Store(true)
	put(t, m, "Riunione con Acme martedì")
	got, err := m.Search(t.Context(), "acme", 3)
	if !errors.HasCode(err, agent.ErrMemoryPartial) || len(got) != 1 {
		t.Fatalf("by words while down: %q %v", contents(got), err)
	}
	calls := emb.calls.Load()
	_, _ = m.Search(t.Context(), "acme", 3)
	if emb.calls.Load() != calls {
		t.Error("the embedder was called during its pause")
	}
	emb.fail.Store(false)
	m.gate.Wake()
	got, err = m.Search(t.Context(), "un incontro col cliente", 3)
	if err != nil || len(got) != 1 {
		t.Fatalf("after recovery: %q %v", contents(got), err)
	}
}

// A vector made from older content, or by another model, is not used.
func TestStaleVectorsAreMadeAgain(t *testing.T) {
	dir := t.TempDir()
	emb := &conceptEmbedder{model: "first"}
	m := openIn(t, dir, emb)
	r := put(t, m, "Riunione con Acme")
	if _, err := m.Put(t.Context(), agent.MemoryRecord{ID: r.ID, Content: "La fattura è pagata"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Search(t.Context(), "incontro", 3); len(got) != 0 {
		t.Fatalf("found by the old content's vector: %q", contents(got))
	}
	other := &conceptEmbedder{model: "second"}
	m2 := openIn(t, dir, other)
	if got, _ := m2.Search(t.Context(), "pagamento", 3); len(got) != 1 || other.texts.Load() < 2 {
		t.Fatalf("another model: %q, %d texts embedded", contents(got), other.texts.Load())
	}
}

// Two Stores of one directory share the database; it closes with the last.
func TestStoresShareTheDatabase(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.db != b.db {
		t.Fatal("two databases for one directory")
	}
	put(t, a, "un fatto")
	_ = a.Close()
	_ = a.Close() // twice is once
	if got, err := b.Search(t.Context(), "fatto", 3); err != nil || len(got) != 1 {
		t.Fatalf("after the first close: %q %v", contents(got), err)
	}
	_ = b.Close()
	registry.Lock()
	_, open := registered[filepath.Join(dir, File)]
	registry.Unlock()
	if open {
		t.Fatal("the database stayed open")
	}
}

// Another process writing the database is seen: a raw connection plays it.
func TestAnotherProcessIsSeen(t *testing.T) {
	dir := t.TempDir()
	emb := &conceptEmbedder{model: "concepts"}
	m := openIn(t, dir, emb)
	put(t, m, "Riunione con Acme")
	if got, _ := m.Search(t.Context(), "incontro", 3); len(got) != 1 {
		t.Fatal("not found")
	}
	other, err := sql.Open("sqlite", dsn(filepath.Join(dir, File)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.Exec(`UPDATE records SET content = 'La fattura è pagata', hash = 'x'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Search(t.Context(), "incontro", 3); len(got) != 0 {
		t.Fatalf("the cached vector of a record another process changed: %q", contents(got))
	}
}

// Many at once remember the same fact: one record.
func TestConcurrentPutsOfOneFact(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := New(dir, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = m.Close() }()
			r, err := m.Put(context.Background(), agent.MemoryRecord{Content: "Antonio usa Go"})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = r.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("two records: %v", ids)
		}
	}
}

func TestFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	m := openIn(t, dir, nil)
	put(t, m, "un fatto")
	if runtime.GOOS == "windows" {
		return
	}
	if info, err := os.Stat(filepath.Join(dir, File)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v %v", info.Mode(), err)
	}
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := New(open, nil); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Errorf("a directory others can write: %v", err)
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, File), filepath.Join(linked, File)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(linked, nil); !errors.HasCode(err, agent.ErrMemoryIO) {
		t.Errorf("a database that is a link: %v", err)
	}
}

func TestANewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	m := openIn(t, dir, nil)
	put(t, m, "x")
	_ = m.Close()
	raw, err := sql.Open("sqlite", dsn(filepath.Join(dir, File)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := New(dir, nil); !errors.HasCode(err, agent.ErrMemoryIO) || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer schema: %v", err)
	}
}

// A word in most records ("il") does not decide a search: the rarer words
// do; a question made only of common words still finds.
func TestCommonWordsDoNotDecide(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	for i := range 10 {
		put(t, m, "il progetto "+strings.Repeat("x", i+1))
	}
	put(t, m, "il magazzino apre alle otto")
	got, _ := m.Search(t.Context(), "il magazzino", 3)
	if len(got) != 1 || !strings.Contains(got[0].Content, "magazzino") {
		t.Fatalf("%q", contents(got))
	}
	if got, _ := m.Search(t.Context(), "il progetto", 3); len(got) == 0 {
		t.Fatal("only common words found nothing")
	}
}

// The cache follows every change, in this process or another, without
// reading everything again.
func TestTheVectorCacheFollowsChanges(t *testing.T) {
	dir := t.TempDir()
	emb := &conceptEmbedder{model: "concepts"}
	m := openIn(t, dir, emb)
	a := put(t, m, "Riunione con Acme")
	b := put(t, m, "La fattura è pagata")
	search := func(q string) string {
		got, _ := m.Search(t.Context(), q, 5)
		return contents(got)
	}
	if search("incontro") != "Riunione con Acme" {
		t.Fatal("not found")
	}
	other, err := sql.Open("sqlite", dsn(filepath.Join(dir, File)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.Exec(`DELETE FROM records WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := search("incontro"); got != "" {
		t.Fatalf("a record another process deleted: %q", got)
	}
	if _, err := m.Put(t.Context(), agent.MemoryRecord{ID: b.ID, Content: "Un incontro con il gatto"}); err != nil {
		t.Fatal(err)
	}
	if got := search("pagamento"); got != "" {
		t.Fatalf("the old vector of a changed record: %q", got)
	}
	if got := search("incontro"); got != "Un incontro con il gatto" {
		t.Fatalf("the new vector: %q", got)
	}
	m.db.mu.Lock()
	n := len(m.db.cache)
	m.db.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d vectors cached, want 1", n)
	}
}

// A cache older than the notes kept reads everything again.
func TestAnOldCacheReadsAll(t *testing.T) {
	m := openIn(t, t.TempDir(), &conceptEmbedder{model: "concepts"})
	put(t, m, "Riunione con Acme")
	_, _ = m.Search(t.Context(), "incontro", 3)
	if _, err := m.db.Exec(`UPDATE generation SET n = n + 5, kept = n + 5`); err != nil {
		t.Fatal(err)
	}
	put(t, m, "Un altro incontro")
	if got, _ := m.Search(t.Context(), "incontro", 3); len(got) != 2 {
		t.Fatalf("%q", contents(got))
	}
}

func TestMain(m *testing.M) {
	linger = 0 // the tests see a database close with its last Store
	os.Exit(m.Run())
}

// Audit 20/11: serve's runs, one after another, find the decoded vectors
// of the last.
func TestTheDatabaseLingers(t *testing.T) {
	linger = time.Hour
	defer func() { linger = 0 }()
	dir := t.TempDir()
	a, err := New(dir, &conceptEmbedder{model: "concepts"})
	if err != nil {
		t.Fatal(err)
	}
	put(t, a, "Riunione con Acme")
	_, _ = a.Search(t.Context(), "incontro", 3)
	_ = a.Close()
	b, err := New(dir, &conceptEmbedder{model: "concepts"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	b.db.mu.Lock()
	cached := b.db.cached
	b.db.mu.Unlock()
	if b.db != a.db || !cached {
		t.Fatal("the next run started cold")
	}
}

// Audit 20/11: a text the model refuses does not keep the others from
// their vectors.
func TestOneRefusedTextDoesNotBlockTheRest(t *testing.T) {
	dir := t.TempDir()
	plain := openIn(t, dir, nil)
	put(t, plain, "POISON")
	for i := range 80 {
		put(t, plain, "un incontro numero "+strconv.Itoa(i))
	}
	emb := &pickyEmbedder{}
	m := openIn(t, dir, emb)
	if _, err := m.Search(t.Context(), "incontro", 3); err != nil {
		t.Fatal(err)
	}
	vecs, _ := m.db.vectors(t.Context(), emb.Model())
	if len(vecs) != 80 {
		t.Fatalf("%d vectors, want the 80 the model takes", len(vecs))
	}
}

type pickyEmbedder struct{ conceptEmbedder }

func (p *pickyEmbedder) Model() string { return "picky" }

func (p *pickyEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, t := range texts {
		if t == "POISON" {
			return nil, errors.New(agent.ErrEmbed, "refused")
		}
	}
	return p.conceptEmbedder.Embed(ctx, texts)
}

// Audit 20/11: two models on one memory keep a vector each.
func TestTwoModelsKeepTheirVectors(t *testing.T) {
	dir := t.TempDir()
	a, b := &conceptEmbedder{model: "a"}, &conceptEmbedder{model: "b"}
	ma, mb := openIn(t, dir, a), openIn(t, dir, b)
	for i := range 20 {
		put(t, ma, "riunione "+strconv.Itoa(i))
	}
	_, _ = mb.Search(t.Context(), "incontro", 3)
	before := a.texts.Load() + b.texts.Load()
	for range 3 {
		_, _ = ma.Search(t.Context(), "incontro", 3)
		_, _ = mb.Search(t.Context(), "incontro", 3)
	}
	if extra := a.texts.Load() + b.texts.Load() - before; extra != 6 { // the six questions only
		t.Fatalf("%d texts embedded again", extra-6)
	}
}

// Audit 20/11: words SQLite folds otherwise than Go are found; a question
// of countless words is bounded.
func TestWordsAsSQLiteFoldsThem(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "Ο δρόμος είναι κλειστός")
	if got, err := m.Search(t.Context(), "δρόμος", 3); err != nil || len(got) != 1 {
		t.Fatalf("%q %v", contents(got), err)
	}
	var b strings.Builder
	for i := range 40000 {
		b.WriteString("w" + strconv.Itoa(i) + "x ")
	}
	if _, err := m.Search(t.Context(), b.String(), 3); err != nil {
		t.Fatalf("40000 words: %v", err)
	}
}

// Audit 20/11: the same fact again costs no embedding.
func TestTheSameFactIsNotEmbeddedAgain(t *testing.T) {
	emb := &conceptEmbedder{model: "concepts"}
	m := openIn(t, t.TempDir(), emb)
	for range 3 {
		put(t, m, "Riunione con Acme")
	}
	if n := emb.calls.Load(); n != 1 {
		t.Fatalf("%d embedding calls", n)
	}
}

// Audit 20/11: processes opening a new database together all get it.
func TestOpeningANewDatabaseTogether(t *testing.T) {
	for round := range 5 {
		dir := t.TempDir()
		if err := prepareFile(dir); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := sql.Open("sqlite", dsn(filepath.Join(dir, File)))
				if err == nil {
					err = prepareSchema(conn)
					_ = conn.Close()
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}

// A small memory has no common words: a question naming its only record
// finds it, also with a word nothing holds.
func TestASmallMemoryHasNoCommonWords(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "Il budget 2027 è 50000 euro")
	if got, _ := m.Search(t.Context(), "qual è il budget 2027?", 3); len(got) != 1 {
		t.Fatalf("%q", contents(got))
	}
}

// Second audit 20/11.

// Two common words: the record holding both comes first.
func TestCommonWordsTogether(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "riunione con acme martedì")
	for i := range 100 {
		switch i % 4 {
		case 0:
			put(t, m, "riunione numero "+strconv.Itoa(i))
		case 1:
			put(t, m, "acme ordine "+strconv.Itoa(i))
		default:
			put(t, m, "altro "+strconv.Itoa(i))
		}
	}
	got, _ := m.Search(t.Context(), "riunione acme", 5)
	if len(got) == 0 || got[0].Content != "riunione con acme martedì" {
		t.Fatalf("%q", contents(got))
	}
}

// Words as SQLite spells them: "ς" folded to "σ" beside a common word,
// "İ", accents in two characters (kept and asked composed, MemoryText).
func TestWordsSpelledAsTheIndex(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	put(t, m, "Il δρόμος per il magazzino è chiuso")
	put(t, m, "Riunione a İstanbul")
	put(t, m, "non so perché")
	for i := range 40 {
		put(t, m, "magazzino nota "+strconv.Itoa(i))
	}
	for q, want := range map[string]string{"magazzino δρόμος": "δρόμος", "İstanbul": "İstanbul", "perche\u0301": "perché"} {
		got, err := m.Search(t.Context(), q, 3)
		if err != nil || len(got) == 0 || !strings.Contains(got[0].Content, want) {
			t.Errorf("%s: %q %v", q, contents(got), err)
		}
	}
}

// A text one model refuses is not skipped by another.
func TestRefusalsArePerModel(t *testing.T) {
	dir := t.TempDir()
	plain := openIn(t, dir, nil)
	put(t, plain, "POISON")
	put(t, plain, "un incontro")
	a := openIn(t, dir, &pickyEmbedder{})
	_, _ = a.Search(t.Context(), "incontro", 3)
	b := &conceptEmbedder{model: "b"}
	mb := openIn(t, dir, b)
	_, _ = mb.Search(t.Context(), "incontro", 3)
	if vecs, _ := mb.db.vectors(t.Context(), "b"); len(vecs) != 2 {
		t.Fatalf("model b has %d vectors, want 2", len(vecs))
	}
}

// An embedder that stops answering after the question: a few calls, the
// gate rests, and the search says it went by words.
func TestAnEmbedderGoneDownMidway(t *testing.T) {
	dir := t.TempDir()
	plain := openIn(t, dir, nil)
	for i := range 64 {
		put(t, plain, "incontro "+strconv.Itoa(i))
	}
	emb := &dyingEmbedder{}
	m := openIn(t, dir, emb)
	_, err := m.Search(t.Context(), "incontro", 3)
	if !errors.HasCode(err, agent.ErrMemoryPartial) {
		t.Fatalf("%v", err)
	}
	if n := emb.calls.Load(); n > 3 {
		t.Fatalf("%d calls", n)
	}
	if m.db.failedCount("dying") != 0 {
		t.Fatal("texts marked refused while the embedder was down")
	}
}

type dyingEmbedder struct{ conceptEmbedder }

func (d *dyingEmbedder) Model() string { return "dying" }

func (d *dyingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if d.calls.Load() > 0 {
		d.calls.Add(1)
		return nil, errors.New(agent.ErrEmbed, "503")
	}
	return d.conceptEmbedder.Embed(ctx, texts)
}

// A memory directory removed and made again, in the same process: the
// new database is the one written.
func TestARecreatedDirectoryIsANewDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not remove a file that is open")
	}
	linger = time.Hour
	defer func() { linger = 0 }()
	dir := filepath.Join(t.TempDir(), "mem")
	a, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	put(t, a, "vecchio")
	_ = a.Close()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	b, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	put(t, b, "nuovo")
	raw, err := sql.Open("sqlite", dsn(filepath.Join(dir, File)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM records`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the file on disk: %d records, %v", n, err)
	}
}

// The same content under its own ID again: no write, no embedding.
func TestTheSameContentByIDIsNotEmbeddedAgain(t *testing.T) {
	emb := &conceptEmbedder{model: "concepts"}
	m := openIn(t, t.TempDir(), emb)
	r := put(t, m, "Riunione con Acme")
	for range 3 {
		got, err := m.Put(t.Context(), agent.MemoryRecord{ID: r.ID, Content: "Riunione con Acme"})
		if err != nil || !got.Created.Equal(r.Created) {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if n := emb.calls.Load(); n != 1 {
		t.Fatalf("%d embedding calls", n)
	}
}

func TestACancelledSearchIsTheContextsError(t *testing.T) {
	m := openIn(t, t.TempDir(), &conceptEmbedder{model: "concepts"})
	put(t, m, "Riunione con Acme")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Search(ctx, "incontro", 3); err != context.Canceled {
		t.Fatalf("%v", err)
	}
}

// An accent written as one character finds it written as two, and back.
func TestEitherFormOfAnAccent(t *testing.T) {
	m := openIn(t, t.TempDir(), nil)
	stored := put(t, m, "non so perché")
	if stored.Content != "non so perché" {
		t.Fatalf("stored %q", stored.Content)
	}
	put(t, m, "il caffè è pronto")
	for _, q := range []string{"perché", "perché", "caffè"} {
		if got, _ := m.Search(t.Context(), q, 3); len(got) != 1 {
			t.Errorf("%q: %q", q, contents(got))
		}
	}
}

// The CI on Windows: a drive letter is no URI authority.
func TestFileURIs(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/m.db":                "file:///home/u/m.db",
		"C:/Users/RUNNER~1/Temp/m.db": "file:///C:/Users/RUNNER~1/Temp/m.db",
		"/tmp/a b/m#1?.db":            "file:///tmp/a%20b/m%231%3F.db",
	} {
		if got := fileURI(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
