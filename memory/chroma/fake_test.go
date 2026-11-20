// fake_test.go: a Chroma server in memory, the subset the memory uses
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package chroma

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeChroma answers as Chroma 1.x does for the calls of this package. The
// same tests run against a real server when RINGIL_CHROMA_URL is set, which
// is how the fake is kept honest.
type fakeChroma struct {
	mu    sync.Mutex
	byID  map[string]*fakeCollection
	names map[string]string // name -> id
	token string            // when set, X-Chroma-Token must carry it
	seen  http.Header       // the last request's headers
	calls int
}

type fakeCollection struct {
	id, name, space string
	meta            map[string]any
	recs            map[string]fakeRecord
	order           []string // insertion order, as a real server answers get
}

type fakeRecord struct {
	vec  []float64
	doc  string
	meta map[string]any
}

func newFake(t *testing.T) (*fakeChroma, *httptest.Server) {
	t.Helper()
	f := &fakeChroma{byID: map[string]*fakeCollection{}, names: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeChroma) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = r.Header.Clone()
	if f.token != "" && r.Header.Get("X-Chroma-Token") != f.token {
		answer(w, http.StatusUnauthorized, map[string]string{"error": "AuthError", "message": "unauthorized"})
		return
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v2/tenants/default_tenant/databases/default_database/collections"), "/")
	switch {
	case len(parts) == 1 && r.Method == http.MethodPost:
		f.create(w, body)
	case len(parts) >= 2:
		c := f.byID[parts[1]]
		if c == nil {
			answer(w, http.StatusNotFound, map[string]string{"error": "NotFoundError", "message": "collection not found"})
			return
		}
		f.onCollection(w, r.Method, strings.Join(parts[2:], "/"), c, body)
	default:
		answer(w, http.StatusNotFound, nil)
	}
}

func (f *fakeChroma) create(w http.ResponseWriter, body map[string]any) {
	name, _ := body["name"].(string)
	if id, ok := f.names[name]; ok {
		answer(w, http.StatusOK, f.byID[id].json())
		return
	}
	space := "l2"
	if cfg, ok := body["configuration"].(map[string]any); ok {
		if h, ok := cfg["hnsw"].(map[string]any); ok {
			space, _ = h["space"].(string)
		}
	}
	meta, _ := body["metadata"].(map[string]any)
	c := &fakeCollection{id: fakeUUID(len(f.names) + 1), name: name, space: space, meta: meta, recs: map[string]fakeRecord{}}
	f.byID[c.id], f.names[name] = c, c.id
	answer(w, http.StatusOK, c.json())
}

func fakeUUID(n int) string {
	s := strings.Repeat("0", 12) + itoa(n)
	return "00000000-0000-4000-8000-" + s[len(s)-12:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func (c *fakeCollection) json() map[string]any {
	return map[string]any{"id": c.id, "name": c.name, "metadata": c.meta,
		"configuration_json": map[string]any{"hnsw": map[string]any{"space": c.space}}}
}

func (f *fakeChroma) onCollection(w http.ResponseWriter, method, op string, c *fakeCollection, body map[string]any) {
	switch method + " " + op {
	case "GET count":
		answer(w, http.StatusOK, len(c.recs))
	case "PUT ":
		c.meta, _ = body["new_metadata"].(map[string]any)
		answer(w, http.StatusOK, map[string]any{})
	case "DELETE ":
		delete(f.byID, c.id)
		delete(f.names, c.name)
		answer(w, http.StatusOK, map[string]any{})
	default:
		c.records(w, op, body)
	}
}

// records answers the calls on a collection's records.
func (c *fakeCollection) records(w http.ResponseWriter, op string, body map[string]any) {
	switch op {
	case "upsert":
		c.upsert(body)
		answer(w, http.StatusOK, map[string]any{})
	case "get":
		answer(w, http.StatusOK, c.get(body))
	case "query":
		answer(w, http.StatusOK, c.query(body))
	case "delete":
		for _, id := range strs(body["ids"]) {
			delete(c.recs, id)
		}
		answer(w, http.StatusOK, map[string]any{"deleted": 1})
	default:
		answer(w, http.StatusNotFound, nil)
	}
}

func (c *fakeCollection) upsert(body map[string]any) {
	ids, docs := strs(body["ids"]), strs(body["documents"])
	vecs, _ := body["embeddings"].([]any)
	metas, _ := body["metadatas"].([]any)
	for i, id := range ids {
		var v []float64
		for _, x := range vecs[i].([]any) {
			v = append(v, x.(float64))
		}
		m, _ := metas[i].(map[string]any)
		if _, ok := c.recs[id]; !ok {
			c.order = append(c.order, id)
		}
		c.recs[id] = fakeRecord{vec: v, doc: docs[i], meta: m}
	}
}

func (c *fakeCollection) matching(body map[string]any) []string {
	if want := strs(body["ids"]); len(want) > 0 {
		return slices.DeleteFunc(want, func(id string) bool { _, ok := c.recs[id]; return !ok })
	}
	ids := slices.DeleteFunc(slices.Clone(c.order), func(id string) bool { _, ok := c.recs[id]; return !ok })
	wdoc, _ := body["where_document"].(map[string]any)
	if p, ok := wdoc["$regex"].(string); ok {
		re := regexp.MustCompile(p)
		ids = slices.DeleteFunc(ids, func(id string) bool { return !re.MatchString(c.recs[id].doc) })
	}
	return ids
}

func (c *fakeCollection) get(body map[string]any) map[string]any {
	ids := c.matching(body)
	if off, ok := body["offset"].(float64); ok {
		ids = ids[min(int(off), len(ids)):]
	}
	if lim, ok := body["limit"].(float64); ok {
		ids = ids[:min(int(lim), len(ids))]
	}
	docs, metas := []any{}, []any{}
	for _, id := range ids {
		docs = append(docs, c.recs[id].doc)
		metas = append(metas, c.recs[id].meta)
	}
	return map[string]any{"ids": ids, "documents": docs, "metadatas": metas}
}

func (c *fakeCollection) query(body map[string]any) map[string]any {
	q := body["query_embeddings"].([]any)[0].([]any)
	type hit struct {
		id   string
		dist float64
	}
	var hits []hit
	for id, rec := range c.recs {
		hits = append(hits, hit{id, 1 - cosineOf(q, rec.vec)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].dist < hits[j].dist })
	n := int(body["n_results"].(float64))
	hits = hits[:min(n, len(hits))]
	ids, docs, metas, dists := []string{}, []any{}, []any{}, []any{}
	for _, h := range hits {
		ids = append(ids, h.id)
		docs = append(docs, c.recs[h.id].doc)
		metas = append(metas, c.recs[h.id].meta)
		dists = append(dists, h.dist)
	}
	return map[string]any{"ids": [][]string{ids}, "documents": []any{docs}, "metadatas": []any{metas}, "distances": []any{dists}}
}

func cosineOf(q []any, v []float64) float64 {
	var dot, nq, nv float64
	for i := range v {
		x := q[i].(float64)
		dot, nq, nv = dot+x*v[i], nq+x*x, nv+v[i]*v[i]
	}
	if nq == 0 || nv == 0 {
		return 0
	}
	return dot / math.Sqrt(nq*nv)
}

func strs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func answer(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
