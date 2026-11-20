// chroma.go: memory kept in a Chroma server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package chroma is an agent.Memory kept in a Chroma collection (the
// HTTP API v2 of Chroma 1.x): the memories of one person, or of one chat,
// in a collection of their own, on a server that serves many of them.
//
// The vectors are ours, made with an agent.Embedder, never the server's:
// the model is written in the collection's metadata, and a collection made
// with another model is refused rather than searched with vectors that
// cannot be compared. Search fuses meaning (cosine, from a minimum
// similarity up, so that an unrelated question recalls nothing) and words
// (BM25 over the records whose text holds a word of the question), as
// memory/sqlite does; with the embedder down it works by words and says
// so with agent.ErrMemoryPartial.
//
// Only https, or http to the loopback (Config.AllowHTTP for a trusted
// network); the credentials (Config.Headers) go with every request and
// never to another origin; responses are bounded.
package chroma

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
	"github.com/agilira/ringil-agent-sdk/memory/internal/embedgate"
	"github.com/agilira/ringil-agent-sdk/memory/local"
)

// Config is where the memory lives and how its vectors are made.
type Config struct {
	// URL is the server, e.g. http://127.0.0.1:8000.
	URL string
	// Tenant and Database default to Chroma's default_tenant and
	// default_database.
	Tenant, Database string
	// Collection holds this memory, and only it: one per person (or
	// chat). 3-512 letters, digits, ., _ or -, starting and ending with a
	// letter or digit.
	Collection string
	// Embedder makes the vectors; its Model is kept with them.
	Embedder agent.Embedder
	// MinSimilarity is the cosine a record found only by meaning must
	// reach (default 0.30, measured with text-embedding-3-small: related
	// pairs scored 0.31-0.40, small talk 0.24-0.27); it depends on the model.
	MinSimilarity float64
	// Headers are added to every request, such as the server's token
	// (Chroma reads "X-Chroma-Token" or "Authorization: Bearer").
	Headers func(ctx context.Context) (http.Header, error)
	// AllowHTTP allows plain http to a host that is not the loopback.
	AllowHTTP bool
	// Client is the HTTP client to start from (its timeout, proxy...).
	Client *http.Client
}

// Bounds of the conversation with the server.
const (
	maxResponse  = 32 << 20
	callTimeout  = 30 * time.Second
	page         = 1000 // records read in one call
	maxPerTerm   = 2000 // records read for one word of a question
	maxResults   = 1000 // records asked of a query by meaning
	maxWordTerms = 8
)

// DefaultMinSimilarity is memory/sqlite's.
const DefaultMinSimilarity = 0.30

const modelKey = "ringil_model" // in the collection's metadata

type store struct {
	cfg    Config
	base   string // .../collections/{id}
	client *http.Client
	gate   *embedgate.Gate
	now    func() time.Time
}

var validCollection = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,510}[A-Za-z0-9]$`)

// ValidCollection reports whether name is a collection name Chroma takes.
func ValidCollection(name string) bool {
	return validCollection.MatchString(name) && !strings.Contains(name, "..")
}

// New opens the collection, making it when missing.
func New(ctx context.Context, cfg Config) (agent.Memory, error) {
	if err := check(&cfg); err != nil {
		return nil, err
	}
	s := &store{cfg: cfg, gate: embedgate.New(cfg.Embedder), now: time.Now,
		client: safehttp.Client(cfg.Client, cfg.Headers, maxResponse, false)}
	if err := s.open(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func check(cfg *Config) error {
	switch {
	case cfg.Embedder == nil:
		return errors.New(agent.ErrMemoryInput, "a Chroma memory needs an embedder")
	case !ValidCollection(cfg.Collection):
		return errors.New(agent.ErrMemoryInput, "a Chroma collection name is 3-512 letters, digits, . _ -, starting and ending with a letter or digit")
	}
	if err := safehttp.CheckURL(cfg.URL, cfg.AllowHTTP); err != nil {
		return errors.Wrap(err, agent.ErrMemoryInput, "chroma: "+err.Error())
	}
	if cfg.Tenant == "" {
		cfg.Tenant = "default_tenant"
	}
	if cfg.Database == "" {
		cfg.Database = "default_database"
	}
	if cfg.MinSimilarity == 0 {
		cfg.MinSimilarity = DefaultMinSimilarity
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return nil
}

type collection struct {
	ID            string         `json:"id"`
	Metadata      map[string]any `json:"metadata"`
	Configuration struct {
		HNSW  *struct{ Space string } `json:"hnsw"`
		SPANN *struct{ Space string } `json:"spann"`
	} `json:"configuration_json"`
}

// open makes or finds the collection, with cosine distance and our model.
func (s *store) open(ctx context.Context) error {
	model := s.cfg.Embedder.Model()
	body := map[string]any{
		"name": s.cfg.Collection, "get_or_create": true,
		"metadata":      map[string]any{modelKey: model},
		"configuration": map[string]any{"hnsw": map[string]any{"space": "cosine"}},
	}
	var c collection
	path := "/api/v2/tenants/" + url.PathEscape(s.cfg.Tenant) + "/databases/" + url.PathEscape(s.cfg.Database) + "/collections"
	if err := s.call(ctx, http.MethodPost, s.cfg.URL+path, body, &c); err != nil {
		return err
	}
	if space := c.space(); space != "cosine" {
		return errors.New(agent.ErrMemoryInput, "chroma: collection "+s.cfg.Collection+" measures distance by "+space+", not cosine")
	}
	s.base = s.cfg.URL + path + "/" + url.PathEscape(c.ID)
	if got, _ := c.Metadata[modelKey].(string); got != model {
		return s.adopt(ctx, got, model)
	}
	return nil
}

func (c collection) space() string {
	switch {
	case c.Configuration.HNSW != nil:
		return c.Configuration.HNSW.Space
	case c.Configuration.SPANN != nil:
		return c.Configuration.SPANN.Space
	}
	return "an unstated measure"
}

// adopt takes an empty collection made with another model (or none) for
// ours; one holding records is refused: its vectors are not comparable.
func (s *store) adopt(ctx context.Context, got, model string) error {
	var n int
	if err := s.call(ctx, http.MethodGet, s.base+"/count", nil, &n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New(agent.ErrMemoryInput, "chroma: collection "+s.cfg.Collection+" holds vectors of model "+cmpOr(got, "unknown")+", not "+model+": use another collection")
	}
	return s.call(ctx, http.MethodPut, s.base, map[string]any{"new_metadata": map[string]any{modelKey: model}}, nil)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// records is Chroma's answer to get (one list) or query (lists of lists,
// one per query: we send one).
type records struct {
	IDs       []string         `json:"ids"`
	Documents []*string        `json:"documents"`
	Metadatas []map[string]any `json:"metadatas"`
}

type queried struct {
	IDs       [][]string         `json:"ids"`
	Documents [][]*string        `json:"documents"`
	Metadatas [][]map[string]any `json:"metadatas"`
	Distances [][]*float64       `json:"distances"`
}

func (r records) list() []agent.MemoryRecord {
	out := make([]agent.MemoryRecord, 0, len(r.IDs))
	for i, id := range r.IDs {
		rec := agent.MemoryRecord{ID: id}
		if i < len(r.Documents) && r.Documents[i] != nil {
			rec.Content = *r.Documents[i]
		}
		if i < len(r.Metadatas) {
			rec.Created = created(r.Metadatas[i])
		}
		out = append(out, rec)
	}
	return out
}

func created(meta map[string]any) time.Time {
	if v, ok := meta["created"].(float64); ok {
		return time.UnixMilli(int64(v)).UTC() // milliseconds: exact in a JSON number
	}
	return time.Time{}
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:16])
}

// Put stores rec with its vector: a memory without one could not be found
// by meaning, so an embedder down fails the Put. Without an ID the ID is
// made from the content, so the same fact stays one record also when two
// processes remember it at once; a known ID keeps its creation time.
func (s *store) Put(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	rec.Content = agent.MemoryText(rec.Content)
	if err := agent.CheckMemoryRecord(rec); err != nil {
		return rec, err
	}
	byContent := rec.ID == ""
	if byContent {
		rec.ID = "h" + hashOf(rec.Content)[:20]
	}
	old, err := s.existing(ctx, rec.ID)
	if err != nil {
		return rec, err
	}
	if byContent && old != nil {
		return *old, nil // remembered already
	}
	rec.Created = s.now().UTC().Truncate(time.Millisecond)
	if old != nil {
		rec.Created = old.Created
	}
	vecs, ok := s.gate.Embed(ctx, []string{rec.Content}, false)
	if !ok {
		return rec, agent.ToolError(agent.ErrEmbed, "The memory cannot be stored now (its embedding failed): try again later.")
	}
	body := map[string]any{"ids": []string{rec.ID}, "embeddings": vecs, "documents": []string{rec.Content},
		"metadatas": []map[string]any{{"created": rec.Created.UnixMilli()}}}
	return rec, s.call(ctx, http.MethodPost, s.base+"/upsert", body, nil)
}

// existing is the record with id; nil when there is none.
func (s *store) existing(ctx context.Context, id string) (*agent.MemoryRecord, error) {
	body := map[string]any{"include": []string{"documents", "metadatas"}, "ids": []string{id}}
	var r records
	if err := s.call(ctx, http.MethodPost, s.base+"/get", body, &r); err != nil {
		return nil, err
	}
	if list := r.list(); len(list) > 0 {
		return &list[0], nil
	}
	return nil, nil
}

// Delete removes a record; one that is not there is an error, as in the
// other memories (Chroma's own delete does not say).
func (s *store) Delete(ctx context.Context, id string) error {
	if id == "" || agent.CheckMemoryRecord(agent.MemoryRecord{ID: id, Content: "x"}) != nil {
		return agent.ToolError(agent.ErrMemoryNotFound, "No memory with that ID.")
	}
	old, err := s.existing(ctx, id)
	if err != nil {
		return err
	}
	if old == nil {
		return agent.ToolError(agent.ErrMemoryNotFound, "No memory with that ID.")
	}
	return s.call(ctx, http.MethodPost, s.base+"/delete", map[string]any{"ids": []string{id}}, nil)
}

// Search fuses meaning and words; an empty query returns the most recent.
func (s *store) Search(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil || limit <= 0 {
		return nil, err
	}
	query = agent.MemoryText(query)
	if query == "" {
		return s.recent(ctx, limit)
	}
	byWords, err := s.byWords(ctx, query, max(limit*4, 20))
	if err != nil {
		return nil, err
	}
	byMeaning, err := s.byMeaning(ctx, query, max(limit*4, 20))
	if err != nil {
		return byWords[:min(limit, len(byWords))], errors.Wrap(err, agent.ErrMemoryPartial, "found by words only: "+err.Error())
	}
	return local.Fuse(byWords, byMeaning, limit), nil
}

// byMeaning asks the server for the nearest records, keeping those whose
// cosine reaches the minimum.
func (s *store) byMeaning(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	q, ok := s.gate.Embed(ctx, []string{query}, true)
	if !ok {
		return nil, errors.New(agent.ErrEmbed, "embeddings are unavailable")
	}
	var r queried
	body := map[string]any{"query_embeddings": q, "n_results": min(limit, maxResults), "include": []string{"documents", "metadatas", "distances"}}
	if err := s.call(ctx, http.MethodPost, s.base+"/query", body, &r); err != nil {
		return nil, err
	}
	if len(r.IDs) == 0 || len(r.Distances) == 0 {
		return nil, nil
	}
	var out []agent.MemoryRecord
	got := records{IDs: r.IDs[0], Documents: first(r.Documents), Metadatas: first(r.Metadatas)}.list()
	for i, rec := range got {
		if i < len(r.Distances[0]) && r.Distances[0][i] != nil && 1-*r.Distances[0][i] >= s.cfg.MinSimilarity {
			out = append(out, rec) // cosine distance is 1 - similarity
		}
	}
	return out, nil
}

func first[T any](lists [][]T) []T {
	if len(lists) == 0 {
		return nil
	}
	return lists[0]
}

// byWords takes the records whose text holds a word of the query and
// ranks them by BM25. Each word is asked apart, so that a rare word is not
// crowded out by a common one; BM25's weights come from the records read.
func (s *store) byWords(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	seen := map[string]bool{}
	var found []agent.MemoryRecord
	for _, pattern := range wordRegexes(query) {
		body := map[string]any{"where_document": map[string]any{"$regex": pattern}, "include": []string{"documents", "metadatas"}}
		got, err := s.pages(ctx, body, maxPerTerm)
		if err != nil {
			return nil, err
		}
		for _, rec := range got {
			if !seen[rec.ID] {
				seen[rec.ID] = true
				found = append(found, rec)
			}
		}
	}
	return local.Rank(found, query, limit), nil
}

// wordRegexes match, one each, the longest words of the query of three
// letters or more, as whole words (between characters that are not
// letters or digits, as Terms splits them) in any case. Every word is
// quoted: the query is text, never a pattern. (Chroma's regex has no \b
// for this: there, _ is a letter.)
func wordRegexes(query string) []string {
	terms := slices.DeleteFunc(local.Terms(query), func(t string) bool { return utf8.RuneCountInString(t) < 3 })
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	out := make([]string, 0, min(maxWordTerms, len(terms)))
	for _, t := range terms[:min(maxWordTerms, len(terms))] {
		out = append(out, `(?i)(^|[^\p{L}\p{N}])`+regexp.QuoteMeta(t)+`($|[^\p{L}\p{N}])`)
	}
	return out
}

// pages reads the records body selects, a page at a time, up to most.
func (s *store) pages(ctx context.Context, body map[string]any, most int) ([]agent.MemoryRecord, error) {
	var all []agent.MemoryRecord
	for offset := 0; offset < most; offset += page {
		body["limit"], body["offset"] = min(page, most-offset), offset
		var r records
		if err := s.call(ctx, http.MethodPost, s.base+"/get", body, &r); err != nil {
			return nil, err
		}
		all = append(all, r.list()...)
		if len(r.IDs) < page {
			break
		}
	}
	return all, nil
}

// recent are the newest records: the dates of all are read a page at a
// time (Chroma does not sort), then the texts of the newest. Its cost grows
// with the memory; it serves listings, not every turn.
func (s *store) recent(ctx context.Context, limit int) ([]agent.MemoryRecord, error) {
	all, err := s.pages(ctx, map[string]any{"include": []string{"metadatas"}}, math.MaxInt)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Created.After(all[j].Created) })
	ids := make([]string, 0, min(limit, len(all)))
	for _, rec := range all[:min(limit, len(all))] {
		ids = append(ids, rec.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var r records
	body := map[string]any{"ids": ids, "include": []string{"documents", "metadatas"}}
	if err := s.call(ctx, http.MethodPost, s.base+"/get", body, &r); err != nil {
		return nil, err
	}
	got := r.list()
	sort.SliceStable(got, func(i, j int) bool { return got[i].Created.After(got[j].Created) })
	return got, nil
}

// call sends body as JSON and decodes the answer into out (when not nil).
func (s *store) call(ctx context.Context, method, target string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return errors.Wrap(err, agent.ErrMemoryIO, "chroma: cannot encode the request")
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "chroma: bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "chroma: the server cannot be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return failure(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "chroma: unreadable answer")
	}
	return nil
}

// failure is a non-2xx answer: its status and Chroma's message, clipped.
func failure(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct{ Error, Message string }
	msg := resp.Status
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		msg += ": " + embedgate.Clip(e.Message, 300)
	}
	return errors.New(agent.ErrMemoryIO, "chroma: "+msg)
}
