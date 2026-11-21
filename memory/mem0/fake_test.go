// fake_test.go: a Mem0 server in memory, the subset the memory uses
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mem0

import (
	"crypto/md5" // #nosec G501 -- as Mem0 does, to tell a known fact
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMem0 answers as the Mem0 2.x REST server does for the calls of this
// package. Its "model" keeps each sentence of a text as a fact, none of a
// greeting, and none it holds already; its search scores the share of the
// question's words a fact holds. As the real one, it deletes, reads and
// changes any ID, whoever it belongs to.
type fakeMem0 struct {
	mu    sync.Mutex
	recs  map[string]fakeRecord
	next  int
	key   string // when set, X-API-Key must carry it
	seen  http.Header
	calls int
	paths []string // every request's path and query
	clock time.Time
	// onAdd runs inside an add, before it keeps anything: another process
	// at the same time.
	onAdd func()
}

type fakeRecord struct {
	memory, user, hash string
	created            time.Time
	run                string // the run_id
}

func newFake(t *testing.T) (*fakeMem0, *httptest.Server) {
	t.Helper()
	f := &fakeMem0{recs: map[string]fakeRecord{}, clock: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeMem0) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = r.Header.Clone()
	f.paths = append(f.paths, r.URL.RequestURI())
	if f.key != "" && r.Header.Get("X-API-Key") != f.key {
		answer(w, http.StatusUnauthorized, map[string]string{"detail": "Invalid API key."})
		return
	}
	if id, one := strings.CutPrefix(r.URL.Path, "/memories/"); one {
		f.serveOne(w, r, id)
		return
	}
	route := map[string]func(http.ResponseWriter, *http.Request){
		"POST /memories": f.add, "GET /memories": f.list, "POST /search": f.search,
	}[r.Method+" "+r.URL.Path]
	if route == nil {
		answer(w, http.StatusNotFound, map[string]string{"detail": "Not Found"})
		return
	}
	route(w, r)
}

func (f *fakeMem0) serveOne(w http.ResponseWriter, r *http.Request, id string) {
	rec, ok := f.recs[id]
	switch r.Method {
	case http.MethodGet:
		if !ok {
			answer(w, http.StatusOK, nil)
			return
		}
		answer(w, http.StatusOK, f.item(id, rec, nil))
	case http.MethodDelete:
		if !ok {
			answer(w, http.StatusNotFound, map[string]string{"detail": "Memory with id " + id + " not found"})
			return
		}
		delete(f.recs, id)
		answer(w, http.StatusOK, map[string]string{"message": "Memory deleted successfully"})
	case http.MethodPut:
		var in struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if !ok {
			answer(w, http.StatusNotFound, map[string]string{"detail": "Memory with id " + id + " not found."})
			return
		}
		rec.memory = in.Text
		f.recs[id] = rec
		answer(w, http.StatusOK, map[string]string{"message": "Memory updated successfully!"})
	}
}

func (f *fakeMem0) item(id string, rec fakeRecord, score *float64) map[string]any {
	it := map[string]any{"id": id, "memory": rec.memory, "user_id": rec.user, "hash": rec.hash,
		"created_at": rec.created.Format("2006-01-02T15:04:05.000000-07:00"), "metadata": nil}
	if rec.run != "" {
		it["run_id"] = rec.run
	}
	if score != nil {
		it["score"] = *score
	}
	return it
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) // #nosec G401 -- as Mem0 does
	return hex.EncodeToString(sum[:])
}

func (f *fakeMem0) add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Messages []struct{ Role, Content string }
		User     string `json:"user_id"`
		Infer    *bool
		Run      string `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.User == "" || len(in.Messages) != 1 {
		answer(w, http.StatusBadRequest, map[string]string{"detail": "At least one identifier (user_id, agent_id, run_id) is required."})
		return
	}
	if f.onAdd != nil {
		f.onAdd()
	}
	results := f.keep(in.User, in.Messages[0].Content, in.Infer == nil || *in.Infer)
	for _, r := range results {
		rec := f.recs[r["id"].(string)]
		rec.run = in.Run
		f.recs[r["id"].(string)] = rec
	}
	answer(w, http.StatusOK, map[string]any{"results": results})
}

// keep stores the facts of text (text itself without infer), and says
// which as the server does.
func (f *fakeMem0) keep(user, text string, infer bool) []map[string]any {
	facts := []string{text}
	if infer {
		facts = extract(text)
	}
	results := []map[string]any{}
	for _, fact := range facts {
		if infer && f.holds(user, md5hex(fact)) {
			continue
		}
		id := f.newID()
		f.clock = f.clock.Add(time.Second)
		f.recs[id] = fakeRecord{memory: fact, user: user, hash: md5hex(fact), created: f.clock}
		results = append(results, map[string]any{"id": id, "memory": fact, "event": "ADD"})
	}
	return results
}

// othersThan is a user other than user holding a record, or "".
func (f *fakeMem0) othersThan(user string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.recs {
		if rec.user != user {
			return rec.user
		}
	}
	return ""
}

// extract is the fake's model: one fact per sentence, none in a greeting
// or a question.
func extract(text string) []string {
	var facts []string
	for _, s := range strings.Split(text, ". ") {
		s = strings.TrimSuffix(strings.TrimSpace(s), ".")
		if s != "" && !strings.EqualFold(s, "ciao") && !strings.HasSuffix(s, "?") {
			facts = append(facts, s)
		}
	}
	return facts
}

func (f *fakeMem0) holds(user, hash string) bool {
	for _, rec := range f.recs {
		if rec.user == user && rec.hash == hash {
			return true
		}
	}
	return false
}

func (f *fakeMem0) newID() string {
	f.next++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.next)
}

// list answers in no order, as pgvector does.
func (f *fakeMem0) list(w http.ResponseWriter, r *http.Request) {
	filters := map[string]string{"user_id": r.URL.Query().Get("user_id"), "run_id": r.URL.Query().Get("run_id")}
	most, _ := strconv.Atoi(r.URL.Query().Get("top_k"))
	if most == 0 {
		most = 20
	}
	out := []map[string]any{}
	for id, rec := range f.recs { // map order: none
		if matches(rec, filters) && len(out) < most {
			out = append(out, f.item(id, rec, nil))
		}
	}
	answer(w, http.StatusOK, map[string]any{"results": out})
}

func (f *fakeMem0) search(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string
		Filters   map[string]string
		TopK      int     `json:"top_k"`
		Threshold float64 `json:"threshold"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	type hit struct {
		id    string
		score float64
	}
	var hits []hit
	words := strings.Fields(strings.ToLower(in.Query))
	for id, rec := range f.recs {
		if s, ok := f.score(rec, words, in.Filters, in.Threshold); ok {
			hits = append(hits, hit{id, s})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].score > hits[j].score || hits[i].score == hits[j].score && hits[i].id < hits[j].id
	})
	out := []map[string]any{}
	for _, h := range hits[:min(len(hits), in.TopK)] {
		out = append(out, f.item(h.id, f.recs[h.id], &h.score))
	}
	answer(w, http.StatusOK, map[string]any{"results": out})
}

// score is rec's for a search: the share of the words it holds, from the
// threshold up (with threshold 0, any record the filters let through).
func (f *fakeMem0) score(rec fakeRecord, words []string, filters map[string]string, threshold float64) (float64, bool) {
	if len(words) == 0 || !matches(rec, filters) {
		return 0, false
	}
	s := share(words, rec.memory)
	return s, s >= threshold && (s > 0 || threshold == 0)
}

// share is the share of words that text holds.
func share(words []string, text string) float64 {
	n := 0
	for _, w := range words {
		if strings.Contains(strings.ToLower(text), w) {
			n++
		}
	}
	return float64(n) / float64(len(words))
}

// matches is the server's filter on user_id and run_id.
func matches(rec fakeRecord, filters map[string]string) bool {
	return rec.user == filters["user_id"] && (filters["run_id"] == "" || rec.run == filters["run_id"])
}

// twin is a record of user under run, as another process would make it.
func (f *fakeMem0) twin(user, run, text string) {
	id := f.keep(user, text, false)[0]["id"].(string)
	rec := f.recs[id]
	rec.run = run
	f.recs[id] = rec
}

func (f *fakeMem0) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}
