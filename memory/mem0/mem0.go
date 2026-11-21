// mem0.go: memory kept by a Mem0 server
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package mem0 is an agent.Memory kept by a self-hosted Mem0 server (its
// REST API, mem0ai 2.x): the memories of one person, or of one chat, under
// a user_id of their own, on a server that keeps many.
//
// Mem0 is not a store of what it is given: its own model reads the text and
// keeps the facts it finds there, rewritten, split in several, or none when
// they are known already. Put returns the first fact kept, and when none
// was kept an ErrMemoryInput that says so, for the model to read: Mem0
// does not tell a fact it knows from text without one. Config.Raw keeps
// the text as it is (the same text twice is one record). The vectors, the
// model that extracts and the search are the server's: Ringil's embedder
// is not used.
//
// Mem0 makes its IDs (UUIDs); a record Put under an ID of the caller's
// keeps it as its run_id, and is found, replaced and deleted by it. Its
// listing has no order and a cap of 1000, so past 1000 records the most
// recent (an empty Search) may miss the newest, and says so with
// agent.ErrMemoryPartial.
//
// Every record is checked to be this memory's before it is changed or
// deleted, and every answer is checked to hold only this memory's records:
// the server deletes any ID it is given, whoever it belongs to.
//
// Only https, or http to the loopback (Config.AllowHTTP for a trusted
// network); the credentials (Config.Headers) go with every request and
// never to another origin; responses are bounded.
package mem0

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
	"github.com/agilira/ringil-agent-sdk/memory/internal/embedgate"
)

// Config is where the memory lives and whose it is.
type Config struct {
	// URL is the server, e.g. http://127.0.0.1:8888.
	URL string
	// User is the user_id the memories are kept under, and only this
	// memory's: one per person (or chat). 1-128 letters, digits, . _ : @ -,
	// starting with a letter or digit.
	User string
	// Raw keeps each text as it is given, without Mem0's extraction.
	Raw bool
	// MinScore is the similarity of meaning (cosine, 0-1) a record must
	// reach to be found, before Mem0 ranks by its blend with words and
	// entities; default DefaultMinScore. It depends on the server's
	// embedding model.
	MinScore float64
	// Headers are added to every request, such as the server's key
	// ("X-API-Key", or "Authorization: Bearer").
	Headers func(ctx context.Context) (http.Header, error)
	// AllowHTTP allows plain http to a host that is not the loopback.
	AllowHTTP bool
	// Client is the HTTP client to start from (its timeout, proxy...).
	Client *http.Client
}

// DefaultMinScore leaves out what shares neither meaning nor words with
// the question: measured on Mem0 2.2.1 with text-embedding-3-small, the
// record a question asked for scored 0.42-0.68, unrelated questions at
// most 0.31 (with qwen3-embedding:0.6b, 0.44-0.77 and up to 0.53).
const DefaultMinScore = 0.4

// Bounds of the conversation with the server.
const (
	maxResponse = 2*maxResults*agent.MaxMemoryContent + 1<<20 // a full listing, escaped
	callTimeout = 30 * time.Second
	putTimeout  = 3 * time.Minute
	maxResults  = 1000 // the server's own cap on a listing
)

var (
	validUser = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	// Mem0 makes its IDs as UUIDs; any other is not one of its records
	// (and its database would fail on it rather than say so).
	validID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ValidUser reports whether name is a user_id this memory takes.
func ValidUser(name string) bool { return validUser.MatchString(name) }

type store struct {
	cfg    Config
	client *http.Client
}

// New checks the server answers for User.
func New(ctx context.Context, cfg Config) (agent.Memory, error) {
	if err := check(&cfg); err != nil {
		return nil, err
	}
	s := &store{cfg: cfg,
		client: safehttp.Client(cfg.Client, cfg.Headers, maxResponse, false)}
	if _, _, err := s.list(ctx, 1); err != nil {
		return nil, err
	}
	return s, nil
}

func check(cfg *Config) error {
	if !ValidUser(cfg.User) {
		return errors.New(agent.ErrMemoryInput, "a Mem0 user is 1-128 letters, digits, . _ : @ -, starting with a letter or digit")
	}
	if cfg.MinScore < 0 || cfg.MinScore > 1 {
		return errors.New(agent.ErrMemoryInput, "the Mem0 minimum score is between 0 and 1")
	}
	if err := safehttp.CheckURL(cfg.URL, cfg.AllowHTTP); err != nil {
		return errors.Wrap(err, agent.ErrMemoryInput, "mem0: "+err.Error())
	}
	if cfg.MinScore == 0 {
		cfg.MinScore = DefaultMinScore
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return nil
}

// item is a record as the server writes it.
type item struct {
	ID      string `json:"id"`
	Memory  string `json:"memory"`
	User    string `json:"user_id"`
	Run     string `json:"run_id"`
	Created string `json:"created_at"`
	Event   string `json:"event"`
}

type items struct {
	Results []item `json:"results"`
}

// kept is a record of this memory with the ID Mem0 knows it by.
type kept struct {
	rec agent.MemoryRecord
	at  string // Mem0's ID
}

// record is it as a record of this memory; not ok when it is not one
// (another user's, a malformed ID, no text). A record made under an ID
// of the caller's holds it as its run_id: Mem0 matches that exactly, in
// SQL, and never changes it.
func (s *store) record(it item) (kept, bool) {
	content := agent.MemoryText(it.Memory)
	id := cmp.Or(it.Run, it.ID)
	if it.User != s.cfg.User || !validID.MatchString(it.ID) || agent.CheckMemoryRecord(agent.MemoryRecord{ID: id, Content: content}) != nil {
		return kept{}, false
	}
	created, _ := time.Parse(time.RFC3339Nano, it.Created)
	return kept{rec: agent.MemoryRecord{ID: id, Content: content, Created: created.UTC()}, at: it.ID}, true
}

// records is the list as this memory's records, one per ID (the oldest,
// should a race have left two).
func (s *store) records(list []item) []agent.MemoryRecord {
	all := make([]kept, 0, len(list))
	for _, it := range list {
		if k, ok := s.record(it); ok {
			all = append(all, k)
		}
	}
	oldest := map[string]kept{}
	for _, k := range all {
		if o, seen := oldest[k.rec.ID]; !seen || before(k, o) {
			oldest[k.rec.ID] = k
		}
	}
	out := make([]agent.MemoryRecord, 0, len(oldest))
	for _, k := range all {
		if oldest[k.rec.ID].at == k.at {
			out = append(out, k.rec)
		}
	}
	return out
}

// before orders the records of one ID the same way in every process:
// the oldest first, Mem0's ID breaking a tie.
func before(a, b kept) bool {
	if !a.rec.Created.Equal(b.rec.Created) {
		return a.rec.Created.Before(b.rec.Created)
	}
	return a.at < b.at
}

// Put gives rec to the server. Without an ID the server's model extracts
// the facts in it (unless Config.Raw). With one, the record with that ID
// is replaced, or made: as given, never extracted (the caller named this
// text).
func (s *store) Put(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	rec.Content = agent.MemoryText(rec.Content)
	if err := agent.CheckMemoryRecord(rec); err != nil {
		return rec, err
	}
	if rec.ID == "" {
		return s.add(ctx, rec)
	}
	all, err := s.find(ctx, rec.ID)
	if err != nil {
		return rec, err
	}
	if len(all) == 0 {
		return s.create(ctx, rec)
	}
	return s.replace(ctx, rec, all)
}

// replace writes rec over the oldest of all (its records), and removes
// the others.
func (s *store) replace(ctx context.Context, rec agent.MemoryRecord, all []kept) (agent.MemoryRecord, error) {
	rec.ID = all[0].rec.ID
	if err := s.call(ctx, http.MethodPut, "/memories/"+all[0].at, map[string]any{"text": rec.Content}, nil); err != nil {
		return rec, err
	}
	rec.Created = all[0].rec.Created
	return rec, s.remove(ctx, all[1:])
}

func (s *store) remove(ctx context.Context, extra []kept) error {
	for _, k := range extra {
		if err := s.call(ctx, http.MethodDelete, "/memories/"+k.at, nil, nil); err != nil && !errors.HasCode(err, agent.ErrMemoryNotFound) {
			return err
		}
	}
	return nil
}

// add gives the server a new text: its first fact kept, or the record
// holding the same text for Raw.
func (s *store) add(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	if s.cfg.Raw {
		if same, ok, err := s.same(ctx, rec.Content); ok || err != nil {
			return same, err
		}
	}
	got, err := s.post(ctx, rec.Content, !s.cfg.Raw, "")
	if err != nil {
		return rec, err
	}
	for _, it := range got {
		if it.Event == "ADD" || it.Event == "UPDATE" {
			k, err := s.byMem0ID(ctx, it.ID) // as kept, with its creation
			return k.rec, err
		}
	}
	return rec, agent.ToolError(agent.ErrMemoryInput, "Nothing new was kept: Mem0 found no fact in this it did not know already. If it is a new fact, say it as one fact about the user or the work.")
}

// create makes the record rec names, under its ID. Should another process
// make the same ID at once, one record stays, the same for both (before),
// with the text of the Put that came last.
func (s *store) create(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	got, err := s.post(ctx, rec.Content, false, rec.ID)
	if err != nil {
		return rec, err
	}
	all, err := s.tagged(ctx, rec.ID)
	switch {
	case err != nil:
		return rec, err
	case len(all) == 0:
		return rec, errors.New(agent.ErrMemoryIO, "mem0: the record made is not there")
	case len(got) == 1 && all[0].at == got[0].ID:
		return all[0].rec, s.remove(ctx, all[1:])
	}
	return s.replace(ctx, rec, all) // another's came first: ours goes over it
}

// post adds text for this memory, under run (the caller's ID) when set.
func (s *store) post(ctx context.Context, text string, infer bool, run string) ([]item, error) {
	body := map[string]any{"messages": []map[string]string{{"role": "user", "content": text}},
		"user_id": s.cfg.User, "infer": infer}
	if run != "" {
		body["run_id"] = run
	}
	var got items
	err := s.call(ctx, http.MethodPost, "/memories", body, &got)
	return got.Results, err
}

// same is the record holding exactly content, when there is one: Raw
// keeps what Mem0's raw add would store twice.
func (s *store) same(ctx context.Context, content string) (agent.MemoryRecord, bool, error) {
	found, err := s.Search(ctx, content, 5)
	if err != nil {
		return agent.MemoryRecord{}, false, err
	}
	for _, r := range found {
		if r.Content == content {
			return r, true, nil
		}
	}
	return agent.MemoryRecord{}, false, nil
}

var errNotFound = agent.ToolError(agent.ErrMemoryNotFound, "No memory with that ID.")

// find is this memory's records with id, oldest first: one Mem0 made
// (its UUID in either case), or those kept under an ID the caller chose
// (exactly as written). Another memory's is not found, so that an ID
// never tells whether another memory holds it.
func (s *store) find(ctx context.Context, id string) ([]kept, error) {
	if low := strings.ToLower(id); validID.MatchString(low) {
		k, err := s.byMem0ID(ctx, low)
		if err == nil && k.rec.ID == low {
			return []kept{k}, nil
		}
		if err != nil && !errors.HasCode(err, agent.ErrMemoryNotFound) {
			return nil, err
		}
	}
	return s.tagged(ctx, id)
}

// byMem0ID is the record Mem0 knows as id, when it is this memory's.
func (s *store) byMem0ID(ctx context.Context, id string) (kept, error) {
	if !validID.MatchString(id) {
		return kept{}, errNotFound
	}
	var it *item
	if err := s.call(ctx, http.MethodGet, "/memories/"+id, nil, &it); err != nil {
		return kept{}, err
	}
	if it == nil {
		return kept{}, errNotFound
	}
	k, ok := s.record(*it)
	if !ok || k.at != id {
		return kept{}, errNotFound
	}
	return k, nil
}

// tagged is this memory's records kept under the caller's ID, oldest
// first: the server's listing filtered on it, exact and without vectors.
func (s *store) tagged(ctx context.Context, id string) ([]kept, error) {
	q := url.Values{"user_id": {s.cfg.User}, "run_id": {id}, "top_k": {"20"}}
	var got items
	if err := s.call(ctx, http.MethodGet, "/memories?"+q.Encode(), nil, &got); err != nil {
		return nil, err
	}
	var out []kept
	for _, it := range got.Results {
		if k, ok := s.record(it); ok && it.Run == id {
			out = append(out, k)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return before(out[i], out[j]) })
	return out, nil
}

// Delete removes one of this memory's records (and any twin a race left).
func (s *store) Delete(ctx context.Context, id string) error {
	if id == "" || agent.CheckMemoryRecord(agent.MemoryRecord{ID: id, Content: "x"}) != nil {
		return errNotFound
	}
	all, err := s.find(ctx, id)
	switch {
	case err != nil:
		return err
	case len(all) == 0:
		return errNotFound
	}
	return s.remove(ctx, all)
}

// Search is the server's search within this memory; an empty query
// returns the most recent.
func (s *store) Search(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil || limit <= 0 {
		return nil, err
	}
	limit = min(limit, maxResults)
	query = agent.MemoryText(query)
	if query == "" {
		return s.recent(ctx, limit)
	}
	body := map[string]any{"query": query, "filters": map[string]string{"user_id": s.cfg.User},
		"top_k": limit, "threshold": s.cfg.MinScore}
	var got items
	if err := s.call(ctx, http.MethodPost, "/search", body, &got); err != nil {
		return nil, err
	}
	recs := s.records(got.Results)
	return recs[:min(limit, len(recs))], nil
}

// recent is the newest limit records: the server lists them in no order,
// so all of them (up to its cap) are read and sorted here. At the cap the
// newest may be among those it left out.
func (s *store) recent(ctx context.Context, limit int) ([]agent.MemoryRecord, error) {
	recs, full, err := s.list(ctx, maxResults)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Created.After(recs[j].Created) })
	recs = recs[:min(limit, len(recs))]
	if full {
		return recs, errors.New(agent.ErrMemoryPartial, "mem0 lists at most 1000 memories, in no order: the newest may be missing")
	}
	return recs, nil
}

// list is up to most of this memory's records; full when the server gave
// as many as asked (there may be more).
func (s *store) list(ctx context.Context, most int) ([]agent.MemoryRecord, bool, error) {
	list, err := s.listing(ctx, most)
	if err != nil {
		return nil, false, err
	}
	return s.records(list), len(list) >= most, nil
}

// listing is the server's listing of this memory, up to most, in no order.
func (s *store) listing(ctx context.Context, most int) ([]item, error) {
	q := url.Values{"user_id": {s.cfg.User}, "top_k": {strconv.Itoa(most)}}
	var got items
	err := s.call(ctx, http.MethodGet, "/memories?"+q.Encode(), nil, &got)
	return got.Results, err
}

// call sends body as JSON to the server's path and decodes the answer
// into out (when not nil).
func (s *store) call(ctx context.Context, method, path string, body, out any) error {
	timeout := callTimeout
	if method == http.MethodPost && path == "/memories" {
		timeout = putTimeout // the server's model reads the text
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := request(ctx, method, s.cfg.URL+path, body)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "mem0: the server cannot be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(path, "/memories/") {
		return agent.ToolError(agent.ErrMemoryNotFound, "No memory with that ID.") // gone meanwhile
	}
	if resp.StatusCode/100 != 2 {
		return failure(resp)
	}
	return decode(resp.Body, out)
}

func request(ctx context.Context, method, target string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, errors.Wrap(err, agent.ErrMemoryIO, "mem0: cannot encode the request")
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrMemoryIO, "mem0: bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// decode reads the answer into out, or drains it when out is nil.
func decode(body io.Reader, out any) error {
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "mem0: unreadable answer")
	}
	return nil
}

// failure is a non-2xx answer: its status and the server's detail,
// clipped.
func failure(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Detail any `json:"detail"`
	}
	msg := resp.Status
	if json.Unmarshal(data, &e) == nil {
		if d, ok := e.Detail.(string); ok && d != "" {
			msg += ": " + embedgate.Clip(d, 300)
		}
	}
	return errors.New(agent.ErrMemoryIO, "mem0: "+msg)
}
