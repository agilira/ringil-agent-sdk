// vector.go: memory found by meaning, not only by words
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package vector is an agent.Memory that finds records by meaning: "the
// meeting with the client" finds "Acme, Tuesday at 10" though they share
// no word. Records live in a memory/local store; beside them, a file of
// vectors from an agent.Embedder, each tied to the content it was made
// from. Search fuses two rankings, words (BM25) and meaning (cosine), by
// reciprocal rank, so an exact name and a paraphrase both count; a record
// found only by meaning must reach a minimum similarity, so an unrelated
// question recalls nothing.
//
// Embedding is a remote call that can fail: then the memory still works
// by words, returns its results with agent.ErrMemoryPartial, and leaves
// the embedder alone for a while instead of retrying on every turn.
package vector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
	"github.com/agilira/ringil-agent-sdk/memory/local"
)

const (
	vectorFile = "vectors.json"
	vectorLock = "vectors.lock"
	maxVectors = 2 << 30
	rrfK       = 60 // the usual constant of reciprocal rank fusion
	allRecords = 1 << 30
	batchSize  = 32
	// maxEmbedChars keeps a text within what embedding models accept
	// (about 8k tokens); the start of a long memory carries its meaning.
	maxEmbedChars = 8000
	// DefaultMinSimilarity was measured with text-embedding-3-small:
	// related pairs scored 0.31-0.40, small talk 0.24-0.27.
	DefaultMinSimilarity = 0.30
	pause                = time.Minute
)

// Option configures the memory.
type Option func(*store)

// WithMinSimilarity sets the cosine a record found only by meaning must
// reach; it depends on the embedding model.
func WithMinSimilarity(v float64) Option { return func(s *store) { s.minSim = v } }

type store struct {
	dir    string
	words  agent.Memory
	emb    agent.Embedder
	minSim float64

	mu       sync.Mutex
	cache    *cached   // decoded vectors of the file last read
	resting  time.Time // no embedding calls before this, after a failure
	failures map[string]time.Time
}

type cached struct {
	mod  time.Time
	size int64
	vecs *vectors
	dec  map[string][]float32
}

// New returns the memory in dir, embedding with emb.
func New(dir string, emb agent.Embedder, opts ...Option) (agent.Memory, error) {
	if emb == nil {
		return nil, errors.New(agent.ErrMemoryInput, "a vector memory needs an embedder")
	}
	words, err := local.New(dir)
	if err != nil {
		return nil, err
	}
	s := &store{dir: dir, words: words, emb: emb, minSim: DefaultMinSimilarity, failures: map[string]time.Time{}}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// vectors is the file beside the records. Every vector is tied to the
// hash of the content it was made from, and all come from one model with
// one length: a change of either rebuilds them.
type vectors struct {
	Model   string           `json:"model"`
	Dims    int              `json:"dims"`
	Vectors map[string]entry `json:"vectors"`
}

type entry struct {
	Hash string `json:"h"`
	Vec  string `json:"v"` // base64 of little-endian float32
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}

func (s *store) Put(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	stored, err := s.words.Put(ctx, rec)
	if err != nil {
		return stored, err
	}
	// Without a vector now, Search embeds it later: the hash check sees
	// that any older vector no longer matches the content.
	vecs, ok := s.embed(ctx, []string{stored.Content}, false)
	if !ok {
		return stored, nil
	}
	return stored, s.update(ctx, len(vecs[0]), func(v *vectors) {
		v.Vectors[stored.ID] = entry{Hash: hashOf(stored.Content), Vec: encode(vecs[0])}
	})
}

func (s *store) Delete(ctx context.Context, id string) error {
	if err := s.words.Delete(ctx, id); err != nil {
		return err
	}
	return s.update(ctx, 0, func(v *vectors) { delete(v.Vectors, id) })
}

// Search ranks by words and by meaning and fuses the two. An empty query
// returns the most recent records, as the word store does.
func (s *store) Search(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	if query == "" || limit <= 0 {
		return s.words.Search(ctx, query, limit)
	}
	byWords, err := s.words.Search(ctx, query, max(limit*4, 20))
	if err != nil {
		return nil, err
	}
	byMeaning, err := s.byMeaning(ctx, query, max(limit*4, 20))
	if err != nil {
		return trim(byWords, limit), errors.Wrap(err, agent.ErrMemoryPartial, "found by words only: "+err.Error())
	}
	return fuse(byWords, byMeaning, limit), nil
}

// byMeaning ranks the records by cosine similarity to the query, keeping
// those above the minimum, after embedding the ones without a vector.
func (s *store) byMeaning(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	all, err := s.words.Search(ctx, "", allRecords)
	if err != nil {
		return nil, err
	}
	q, ok := s.embed(ctx, []string{query}, true)
	if !ok {
		return nil, errors.New(agent.ErrEmbed, "embeddings are unavailable")
	}
	have, err := s.complete(ctx, all, len(q[0]))
	if err != nil {
		return nil, err
	}
	return rankByMeaning(all, have, q[0], s.minSim, limit), nil
}

func rankByMeaning(all []agent.MemoryRecord, have map[string][]float32, q []float32, minSim float64, limit int) []agent.MemoryRecord {
	type hit struct {
		rec   agent.MemoryRecord
		score float64
	}
	var hits []hit
	for _, r := range all {
		if v, ok := have[r.ID]; ok {
			if score, ok := cosine(q, v); ok && score >= minSim {
				hits = append(hits, hit{r, score})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]agent.MemoryRecord, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, h.rec)
	}
	return out
}

// embed calls the embedder unless it failed recently; a failure starts a
// pause, so a broken or misnamed model costs one call a minute, not one
// per turn. A query goes through EmbedQuery when the model is asymmetric.
func (s *store) embed(ctx context.Context, texts []string, query bool) ([][]float32, bool) {
	s.mu.Lock()
	resting := time.Now().Before(s.resting)
	s.mu.Unlock()
	if resting {
		return nil, false
	}
	clipped := make([]string, len(texts))
	for i, t := range texts {
		clipped[i] = clip(t, maxEmbedChars)
	}
	embed := s.emb.Embed
	if qe, ok := s.emb.(agent.QueryEmbedder); ok && query {
		embed = qe.EmbedQuery
	}
	vecs, err := embed(ctx, clipped)
	if err != nil {
		if ctx.Err() == nil {
			s.mu.Lock()
			s.resting = time.Now().Add(pause)
			s.mu.Unlock()
		}
		return nil, false
	}
	return vecs, true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// complete returns a usable vector for every record it can, embedding the
// missing ones in batches: a batch the model refuses is skipped (and not
// retried for a while), the others are saved.
func (s *store) complete(ctx context.Context, all []agent.MemoryRecord, dims int) (map[string][]float32, error) {
	file, dec, err := s.read()
	if err != nil {
		return nil, err
	}
	have, missing := s.split(file, dec, all, dims)
	if len(missing) == 0 {
		return have, nil
	}
	fresh := s.embedMissing(ctx, missing, have)
	if len(fresh) == 0 && len(missing) > 0 {
		return have, s.prune(ctx, file, all, dims)
	}
	live := idSet(all)
	return have, s.update(ctx, dims, func(v *vectors) {
		for id, e := range fresh {
			v.Vectors[id] = e
		}
		for id := range v.Vectors {
			if !live[id] {
				delete(v.Vectors, id) // forgotten records leave no vector behind
			}
		}
	})
}

// split separates records whose vector matches their content, model and
// length from the others; records that failed recently are left out.
func (s *store) split(file *vectors, dec map[string][]float32, all []agent.MemoryRecord, dims int) (map[string][]float32, []agent.MemoryRecord) {
	have := map[string][]float32{}
	var missing []agent.MemoryRecord
	sameModel := file.Model == s.emb.Model() && file.Dims == dims
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range all {
		e, ok := file.Vectors[r.ID]
		if v := dec[r.ID]; sameModel && ok && e.Hash == hashOf(r.Content) && len(v) == dims {
			have[r.ID] = v
			continue
		}
		if until, failed := s.failures[hashOf(r.Content)]; failed && time.Now().Before(until) {
			continue
		}
		missing = append(missing, r)
	}
	return have, missing
}

func (s *store) embedMissing(ctx context.Context, missing []agent.MemoryRecord, have map[string][]float32) map[string]entry {
	fresh := map[string]entry{}
	for start := 0; start < len(missing); start += batchSize {
		batch := missing[start:min(start+batchSize, len(missing))]
		texts := make([]string, len(batch))
		for i, r := range batch {
			texts[i] = r.Content
		}
		vecs, ok := s.embed(ctx, texts, false)
		if !ok {
			s.markFailed(batch)
			continue
		}
		for i, r := range batch {
			have[r.ID] = vecs[i]
			fresh[r.ID] = entry{Hash: hashOf(r.Content), Vec: encode(vecs[i])}
		}
	}
	return fresh
}

func (s *store) markFailed(batch []agent.MemoryRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until := time.Now().Add(10 * pause)
	for _, r := range batch {
		s.failures[hashOf(r.Content)] = until
	}
}

// prune drops vectors of records that no longer exist, when nothing else
// is being written.
func (s *store) prune(ctx context.Context, file *vectors, all []agent.MemoryRecord, dims int) error {
	live := idSet(all)
	stale := false
	for id := range file.Vectors {
		stale = stale || !live[id]
	}
	if !stale {
		return nil
	}
	return s.update(ctx, dims, func(v *vectors) {
		for id := range v.Vectors {
			if !live[id] {
				delete(v.Vectors, id)
			}
		}
	})
}

func idSet(all []agent.MemoryRecord) map[string]bool {
	m := make(map[string]bool, len(all))
	for _, r := range all {
		m[r.ID] = true
	}
	return m
}

// fuse combines two rankings by reciprocal rank: each record scores
// 1/(k+rank) in each list it appears in.
func fuse(a, b []agent.MemoryRecord, limit int) []agent.MemoryRecord {
	score := map[string]float64{}
	recs := map[string]agent.MemoryRecord{}
	var order []string
	for _, list := range [][]agent.MemoryRecord{a, b} {
		for rank, r := range list {
			if _, seen := recs[r.ID]; !seen {
				recs[r.ID] = r
				order = append(order, r.ID)
			}
			score[r.ID] += 1 / float64(rrfK+rank+1)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	out := make([]agent.MemoryRecord, 0, min(limit, len(order)))
	for _, id := range order[:min(limit, len(order))] {
		out = append(out, recs[id])
	}
	return out
}

func trim(recs []agent.MemoryRecord, n int) []agent.MemoryRecord {
	return recs[:min(n, len(recs))]
}

// cosine reports false for vectors that cannot be compared (other length,
// zero, not finite): they are left out rather than ranked.
func cosine(a, b []float32) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	c := dot / math.Sqrt(na*nb)
	if na == 0 || nb == 0 || math.IsNaN(c) || math.IsInf(c, 0) {
		return 0, false
	}
	return c, true
}

func encode(v []float32) string {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decode(s string) ([]float32, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) == 0 || len(b)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, true
}

// read loads the vectors without a lock (writes replace the file by
// rename, so a reader sees one whole version), decoding them once per
// version of the file: recall searches on every turn.
func (s *store) read() (*vectors, map[string][]float32, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, nil, errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory directory")
	}
	defer closeQuiet(root)
	info, statErr := root.Lstat(vectorFile)
	if c := s.cached(info, statErr); c != nil {
		return c.vecs, c.dec, nil
	}
	v, err := readIn(root)
	if err != nil {
		return nil, nil, err
	}
	dec := make(map[string][]float32, len(v.Vectors))
	for id, e := range v.Vectors {
		if vec, ok := decode(e.Vec); ok {
			dec[id] = vec
		}
	}
	if statErr == nil {
		s.mu.Lock()
		s.cache = &cached{mod: info.ModTime(), size: info.Size(), vecs: v, dec: dec}
		s.mu.Unlock()
	}
	return v, dec, nil
}

// cached returns the decoded vectors if the file is the one last read.
func (s *store) cached(info os.FileInfo, statErr error) *cached {
	s.mu.Lock()
	defer s.mu.Unlock()
	if statErr != nil || s.cache == nil || !s.cache.mod.Equal(info.ModTime()) || s.cache.size != info.Size() {
		return nil
	}
	return s.cache
}

func readIn(root *os.Root) (*vectors, error) {
	v := &vectors{Vectors: map[string]entry{}}
	data, err := atomicfile.Read(root, vectorFile, maxVectors)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrMemoryIO, "cannot read the vectors")
	}
	if json.Unmarshal(data, v) != nil || v.Vectors == nil {
		return &vectors{Vectors: map[string]entry{}}, nil // rebuilt on the next search
	}
	return v, nil
}

// update changes the vectors under their own lock. dims, when known,
// starts the file over if the model or the vector length changed.
func (s *store) update(ctx context.Context, dims int, fn func(*vectors)) error {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory directory")
	}
	defer closeQuiet(root)
	lock, err := lockfile.Open(root, vectorLock)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the vectors lock")
	}
	defer closeFile(lock)
	if err := lockfile.Lock(ctx, lock); err != nil {
		return err
	}
	return s.rewrite(root, dims, fn)
}

func (s *store) rewrite(root *os.Root, dims int, fn func(*vectors)) error {
	v, err := readIn(root)
	if err != nil {
		return err
	}
	if dims > 0 && (v.Model != s.emb.Model() || v.Dims != dims) {
		v = &vectors{Model: s.emb.Model(), Dims: dims, Vectors: map[string]entry{}}
	}
	fn(v)
	data, err := json.Marshal(v)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot encode the vectors")
	}
	if err := atomicfile.Write(root, vectorFile, data); err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot write the vectors")
	}
	return nil
}

func closeQuiet(r *os.Root) {
	if err := r.Close(); err != nil {
		return
	}
}

func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		return // the lock is released on close or exit either way
	}
}
