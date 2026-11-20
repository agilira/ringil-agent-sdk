// sqlite.go: memory in a SQLite database
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package sqlite is an agent.Memory in one SQLite database file per memory
// (pure Go, modernc.org/sqlite: no cgo). Words are found through SQLite's
// own full-text index (FTS5, ranked by BM25), so a search no longer reads
// every record; with an agent.Embedder, records are also found by meaning,
// their vectors kept in the same database beside the content they were
// made from, and the two rankings fused by reciprocal rank. With the
// embedder down, the memory works by words and says so with
// agent.ErrMemoryPartial.
//
// Several processes may use one database: SQLite serializes the writes,
// and the same fact remembered twice stays one record.
package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
	_ "modernc.org/sqlite" // the driver

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
	"github.com/agilira/ringil-agent-sdk/memory/internal/embedgate"
	"github.com/agilira/ringil-agent-sdk/memory/local"
)

// File is the database's name in the memory's directory.
const File = "memory.db"

// DefaultMinSimilarity is the cosine a record found only by meaning must
// reach. It depends on the model: measured with text-embedding-3-small,
// related pairs scored 0.31-0.40, small talk 0.24-0.27; with
// qwen3-embedding:0.6b, 0.46 against 0.25.
const DefaultMinSimilarity = 0.30

const (
	schemaVersion = 1
	batch         = 32
	failedFor     = 10 * time.Minute // a text the model refused is not sent again before
)

const schema = `
CREATE TABLE IF NOT EXISTS records (
	rowid   INTEGER PRIMARY KEY,
	id      TEXT NOT NULL UNIQUE,
	content TEXT NOT NULL,
	hash    TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS records_hash ON records(hash);
CREATE INDEX IF NOT EXISTS records_created ON records(created);
CREATE VIRTUAL TABLE IF NOT EXISTS records_fts USING fts5(
	content, content='records', content_rowid='rowid', tokenize="unicode61 remove_diacritics 0"
);
CREATE TRIGGER IF NOT EXISTS records_ai AFTER INSERT ON records BEGIN
	INSERT INTO records_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS records_ad AFTER DELETE ON records BEGIN
	INSERT INTO records_fts(records_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS records_au AFTER UPDATE OF content ON records BEGIN
	INSERT INTO records_fts(records_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
	INSERT INTO records_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE VIRTUAL TABLE IF NOT EXISTS records_vocab USING fts5vocab(records_fts, 'row');
CREATE TABLE IF NOT EXISTS vectors (
	id    TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
	model TEXT NOT NULL,
	hash  TEXT NOT NULL,
	vec   BLOB NOT NULL,
	at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (id, model)
);
CREATE INDEX IF NOT EXISTS vectors_at ON vectors(at);
CREATE TABLE IF NOT EXISTS generation (k INTEGER PRIMARY KEY CHECK (k = 1), n INTEGER NOT NULL, kept INTEGER NOT NULL);
INSERT OR IGNORE INTO generation VALUES (1, 0, 0);
CREATE TABLE IF NOT EXISTS gone (id TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS gone_at ON gone(at);
CREATE TRIGGER IF NOT EXISTS vectors_ai AFTER INSERT ON vectors BEGIN
	UPDATE generation SET n = n + 1;
	UPDATE vectors SET at = (SELECT n FROM generation) WHERE id = new.id AND model = new.model;
END;
CREATE TRIGGER IF NOT EXISTS vectors_au AFTER UPDATE OF hash, vec ON vectors BEGIN
	UPDATE generation SET n = n + 1;
	UPDATE vectors SET at = (SELECT n FROM generation) WHERE id = new.id AND model = new.model;
END;
CREATE TRIGGER IF NOT EXISTS vectors_ad AFTER DELETE ON vectors BEGIN
	UPDATE generation SET n = n + 1;
	INSERT INTO gone VALUES (old.id, (SELECT n FROM generation));
END;
CREATE TRIGGER IF NOT EXISTS records_gen AFTER UPDATE OF content ON records BEGIN
	UPDATE generation SET n = n + 1;
	INSERT INTO gone VALUES (new.id, (SELECT n FROM generation));
END;
`

// Option configures the memory.
type Option func(*Store)

// WithMinSimilarity sets the cosine a record found only by meaning must
// reach.
func WithMinSimilarity(v float64) Option { return func(s *Store) { s.minSim = v } }

// Store is the memory of one directory. Close it when done; until then it
// holds the database open, shared with the other Stores of the same
// directory in this process.
type Store struct {
	db     *shared
	emb    agent.Embedder
	gate   *embedgate.Gate
	minSim float64
	now    func() time.Time
	once   sync.Once
}

// New opens the memory in dir (dir/memory.db), creating both privately.
// emb may be nil: the memory then finds by words only.
func New(dir string, emb agent.Embedder, opts ...Option) (*Store, error) {
	path, err := prepare(dir)
	if err != nil {
		return nil, err
	}
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, emb: emb, minSim: DefaultMinSimilarity, now: time.Now}
	if emb != nil {
		s.gate = embedgate.New(emb)
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// prepare makes the directory private and the database file a regular
// file of the user's, never a link.
func prepare(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.Wrap(err, agent.ErrMemoryIO, "cannot create the memory directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New(agent.ErrMemoryIO, "memory path is not a directory")
	}
	if lockfile.CheckPrivate(info) != nil {
		return "", errors.New(agent.ErrMemoryIO, "memory directory is not private to the current user; chmod 700 it")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", errors.Wrap(err, agent.ErrMemoryIO, "cannot resolve the memory directory")
	}
	return filepath.Join(abs, File), prepareFile(abs)
}

// prepareFile makes the database file in dir, 0600, or checks the one
// there: a regular file, never a link (os.Root keeps it inside dir).
func prepareFile(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory directory")
	}
	defer func() { _ = root.Close() }()
	if l, err := root.Lstat(File); err == nil && !l.Mode().IsRegular() {
		return errors.New(agent.ErrMemoryIO, "the memory database is not a regular file (a link?)")
	}
	_ = root.Chmod(File, 0o600) // one made by hand, or by a looser umask
	f, err := root.OpenFile(File, os.O_RDWR|os.O_CREATE|lockfile.NoFollow, 0o600)
	if err != nil {
		return errors.Wrap(err, agent.ErrMemoryIO, "cannot open the memory database")
	}
	st, err := f.Stat()
	_ = f.Close()
	if err != nil || !st.Mode().IsRegular() {
		return errors.New(agent.ErrMemoryIO, "the memory database is not a regular file")
	}
	return nil
}

// Close releases the database; the last Store of a directory closes it.
func (s *Store) Close() error {
	var err error
	s.once.Do(func() { err = s.db.release() })
	return err
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:16])
}

func newID() string { return strings.ToLower(rand.Text()[:10]) }

// Put stores rec. Without an ID, the same content stays one record; with
// one, the record is replaced and keeps its creation time. The vector is
// made now when it can be, else at the next search by meaning.
func (s *Store) Put(ctx context.Context, rec agent.MemoryRecord) (agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	rec.Content = agent.MemoryText(rec.Content)
	if err := agent.CheckMemoryRecord(rec); err != nil {
		return rec, err
	}
	hash := hashOf(rec.Content)
	stored, found, err := s.upsert(ctx, rec, hash)
	if err != nil {
		return rec, err
	}
	if s.gate != nil && !(found && s.hasVector(ctx, stored.ID, hash)) {
		if vecs, ok := s.gate.Embed(ctx, []string{stored.Content}, false); ok {
			s.saveVectors(ctx, map[string]vectorRow{stored.ID: {hash: hash, vec: vecs[0]}})
		}
	}
	return stored, nil
}

// upsert writes rec in one transaction (immediate: two processes take
// turns), finding the same content first when it has no ID.
func (s *Store) upsert(ctx context.Context, rec agent.MemoryRecord, hash string) (agent.MemoryRecord, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rec, false, ioError(err, "cannot write the memory")
	}
	defer func() { _ = tx.Rollback() }()
	if created, same := sameContent(ctx, tx, rec.ID, hash); same {
		rec.Created = created
		return rec, true, nil // the same content: nothing to write
	}
	if rec.ID == "" {
		var old agent.MemoryRecord
		var created int64
		err := tx.QueryRowContext(ctx, `SELECT id, content, created FROM records WHERE hash = ? AND content = ? LIMIT 1`, hash, rec.Content).
			Scan(&old.ID, &old.Content, &created)
		if err == nil {
			old.Created = time.UnixMilli(created).UTC()
			return old, true, nil // remembered already
		}
		if err != sql.ErrNoRows {
			return rec, false, ioError(err, "cannot read the memory")
		}
		rec.ID = newID()
	}
	created := s.now().UTC().Truncate(time.Millisecond).UnixMilli()
	err = tx.QueryRowContext(ctx, `INSERT INTO records(id, content, hash, created) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET content = excluded.content, hash = excluded.hash RETURNING created`,
		rec.ID, rec.Content, hash, created).Scan(&created)
	if err != nil {
		return rec, false, ioError(err, "cannot write the memory")
	}
	if err := tx.Commit(); err != nil {
		return rec, false, ioError(err, "cannot write the memory")
	}
	rec.Created = time.UnixMilli(created).UTC()
	return rec, false, nil
}

// sameContent: the record id already holds the content of hash; its
// creation time.
func sameContent(ctx context.Context, tx *sql.Tx, id, hash string) (time.Time, bool) {
	if id == "" {
		return time.Time{}, false
	}
	var oldHash string
	var created int64
	err := tx.QueryRowContext(ctx, `SELECT hash, created FROM records WHERE id = ?`, id).Scan(&oldHash, &created)
	return time.UnixMilli(created).UTC(), err == nil && oldHash == hash
}

// hasVector: id has a vector of this model for the content hash.
func (s *Store) hasVector(ctx context.Context, id, hash string) bool {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM vectors WHERE id = ? AND model = ? AND hash = ?`, id, s.emb.Model(), hash).Scan(&one)
	return err == nil
}

// Delete removes a record and its vector.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, id)
	if err != nil {
		return ioError(err, "cannot write the memory")
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return agent.ToolError(agent.ErrMemoryNotFound, "No memory with that ID.")
	}
	return nil
}

// Search ranks by words and, with an embedder, by meaning, and fuses the
// two. An empty query returns the most recent records.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	if err := ctx.Err(); err != nil || limit <= 0 {
		return nil, err
	}
	query = agent.MemoryText(query)
	if query == "" {
		return s.recent(ctx, limit)
	}
	pool := max(limit*4, 20)
	byWords, err := s.byWords(ctx, query, pool)
	if err != nil || s.gate == nil {
		return byWords[:min(limit, len(byWords))], err
	}
	byMeaning, err := s.byMeaning(ctx, query, pool)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.HasCode(err, agent.ErrEmbed):
		return byWords[:min(limit, len(byWords))], errors.Wrap(err, agent.ErrMemoryPartial, "found by words only: "+err.Error())
	case err != nil:
		return nil, err
	}
	return local.Fuse(byWords, byMeaning, limit), nil
}

func (s *Store) recent(ctx context.Context, limit int) ([]agent.MemoryRecord, error) {
	return s.query(ctx, `SELECT id, content, created FROM records ORDER BY created DESC, rowid DESC LIMIT ?`, limit)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]agent.MemoryRecord, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	defer func() { _ = rows.Close() }()
	var out []agent.MemoryRecord
	for rows.Next() {
		var r agent.MemoryRecord
		var created int64
		if err := rows.Scan(&r.ID, &r.Content, &created); err != nil {
			return nil, ioError(err, "cannot read the memory")
		}
		r.Created = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	return out, nil
}

func ioError(err error, msg string) error {
	return errors.Wrap(err, agent.ErrMemoryIO, msg+": "+err.Error())
}

// vectorRow is a vector and the hash of the content it was made from.
type vectorRow struct {
	hash string
	vec  []float32
}

// saveVectors keeps vectors, each only while its record still holds the
// content it was made from.
func (s *Store) saveVectors(ctx context.Context, vecs map[string]vectorRow) {
	model := s.emb.Model()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	for id, v := range vecs {
		_, err := tx.ExecContext(ctx, `INSERT INTO vectors(id, hash, model, vec)
			SELECT id, hash, ?, ? FROM records WHERE id = ? AND hash = ?
			ON CONFLICT(id, model) DO UPDATE SET hash = excluded.hash, vec = excluded.vec`,
			model, encode(v.vec), id, v.hash)
		if err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decode(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// byMeaning embeds the query, then the records without a vector of this
// model and content, and ranks by cosine from the minimum up.
func (s *Store) byMeaning(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	q, ok := s.gate.Embed(ctx, []string{query}, true)
	if !ok {
		return nil, errors.New(agent.ErrEmbed, "embeddings are unavailable")
	}
	vecs, err := s.current(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.byIDs(ctx, nearest(vecs, q[0], s.minSim, limit))
}

// current are the vectors of every record, made first for the records
// without one: looked for only when there are fewer vectors than records,
// not counting those the model refused lately. query, just embedded,
// tells the embedder still answers when a batch fails.
func (s *Store) current(ctx context.Context, query string) (map[string]vector, error) {
	model := s.emb.Model()
	vecs, err := s.db.vectors(ctx, model)
	if err != nil {
		return nil, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&total); err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	if len(vecs)+s.db.failedCount(model) >= total {
		return vecs, nil
	}
	if err := s.complete(ctx, query); err != nil {
		return nil, err
	}
	return s.db.vectors(ctx, model)
}

// vector is a stored vector with its norm, computed once.
type vector struct {
	v    []float32
	norm float64
}

func newVector(v []float32) vector {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	return vector{v: v, norm: math.Sqrt(n)}
}

// nearest are the ids of the limit vectors nearest q, from minSim up.
func nearest(vecs map[string]vector, q []float32, minSim float64, limit int) []string {
	type hit struct {
		id    string
		score float64
	}
	qv := newVector(q)
	var hits []hit
	for id, v := range vecs {
		if score, ok := cosine(qv, v); ok && score >= minSim {
			hits = append(hits, hit{id, score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].score > hits[j].score || (hits[i].score == hits[j].score && hits[i].id < hits[j].id)
	})
	ids := make([]string, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		ids = append(ids, h.id)
	}
	return ids
}

// byIDs returns the records of ids, in that order.
func (s *Store) byIDs(ctx context.Context, ids []string) ([]agent.MemoryRecord, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	got, err := s.query(ctx, `SELECT id, content, created FROM records WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]agent.MemoryRecord, len(got))
	for _, r := range got {
		byID[r.ID] = r
	}
	out := make([]agent.MemoryRecord, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// complete embeds, a batch at a time, the records whose vector is missing
// or stale (another model, other content). A batch that fails is either a
// text the model will not take, or the embedder gone down: the query,
// embedded again, tells which. Down, the pass stops and the gate rests;
// up, the batch is halved down to the texts refused, which are skipped
// for a while, so one of them never holds up the others.
func (s *Store) complete(ctx context.Context, query string) error {
	todo, err := s.missing(ctx)
	if err != nil {
		return err
	}
	for start := 0; start < len(todo) && ctx.Err() == nil; start += batch {
		part := todo[start:min(start+batch, len(todo))]
		if s.embedBatch(ctx, part) {
			continue
		}
		if _, err := s.emb.Embed(ctx, []string{embedgate.Clip(query, embedgate.MaxChars)}); err != nil {
			s.gate.Rest()
			return errors.Wrap(err, agent.ErrEmbed, "the embedder stopped answering")
		}
		s.bisect(ctx, part)
	}
	return ctx.Err()
}

// pending is a record without a current vector.
type pending struct {
	id, hash, content string
}

// missing are the records without a current vector of this model, but
// those refused lately; their contents are read only for those.
func (s *Store) missing(ctx context.Context) ([]pending, error) {
	model := s.emb.Model()
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.hash FROM records r
		LEFT JOIN vectors v ON v.id = r.id AND v.model = ? AND v.hash = r.hash WHERE v.id IS NULL`, model)
	if err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.hash); err != nil {
			_ = rows.Close()
			return nil, ioError(err, "cannot read the memory")
		}
		if !s.db.failedRecently(model, p.hash) {
			todo = append(todo, p)
		}
	}
	_ = rows.Close()
	return s.withContents(ctx, todo)
}

// withContents fills in the contents of todo.
func (s *Store) withContents(ctx context.Context, todo []pending) ([]pending, error) {
	for start := 0; start < len(todo); start += 500 {
		part := todo[start:min(start+500, len(todo))]
		ids := make([]string, len(part))
		for i, p := range part {
			ids[i] = p.id
		}
		recs, err := s.byIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		content := make(map[string]string, len(recs))
		for _, r := range recs {
			content[r.ID] = r.Content
		}
		for i := range part {
			part[i].content = content[part[i].id]
		}
	}
	return todo, nil
}

// embedBatch embeds todo straight (the gate would rest after one refused
// text and keep the good ones waiting) and keeps the vectors; false when
// the model would not.
func (s *Store) embedBatch(ctx context.Context, todo []pending) bool {
	texts := make([]string, len(todo))
	for i, p := range todo {
		texts[i] = embedgate.Clip(p.content, embedgate.MaxChars)
	}
	vecs, err := s.emb.Embed(ctx, texts)
	if err != nil || len(vecs) != len(todo) {
		return false
	}
	rows := make(map[string]vectorRow, len(todo))
	for i, p := range todo {
		rows[p.id] = vectorRow{hash: p.hash, vec: vecs[i]}
	}
	s.saveVectors(ctx, rows)
	return true
}

// bisect halves a refused batch down to the texts the model will not
// take, and marks them.
func (s *Store) bisect(ctx context.Context, todo []pending) {
	if ctx.Err() != nil {
		return
	}
	if len(todo) == 1 {
		s.db.markFailed(s.emb.Model(), []string{todo[0].hash})
		return
	}
	for _, half := range [][]pending{todo[:len(todo)/2], todo[len(todo)/2:]} {
		if !s.embedBatch(ctx, half) {
			s.bisect(ctx, half)
		}
	}
}

// cosine reports false for vectors that cannot be compared (other length,
// zero, not finite): they are left out rather than ranked.
func cosine(a, b vector) (float64, bool) {
	if len(a.v) != len(b.v) || len(a.v) == 0 || a.norm == 0 || b.norm == 0 {
		return 0, false
	}
	var dot float64
	for i := range a.v {
		dot += float64(a.v[i]) * float64(b.v[i])
	}
	c := dot / (a.norm * b.norm)
	if math.IsNaN(c) || math.IsInf(c, 0) {
		return 0, false
	}
	return c, true
}

// dsn opens path with WAL (readers do not wait for the writer), a wait
// for a busy database, foreign keys (a record's vector goes with it) and
// transactions that take the write lock at their start.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	return fileURI(filepath.ToSlash(path)) + "?" + q.Encode()
}

// fileURI is a path written with / as SQLite's URI wants it: three slashes
// and the absolute path, "file:///home/u/m.db", and on Windows the drive
// after the third, "file:///C:/Users/u/m.db".
func fileURI(slashed string) string {
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed // C:/Users/...
	}
	return (&url.URL{Scheme: "file", Path: slashed, OmitHost: false}).String()
}
