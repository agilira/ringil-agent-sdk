// shared.go: one database per directory, shared by the Stores of a process
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sqlite

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// shared is one database: serve makes a Store for every run, and they
// share the connections and the decoded vectors. The connections close
// with the last Store (no file stays open: Windows could not remove it);
// the vectors stay for linger, for the next run to find them.
type shared struct {
	*sql.DB // nil while no Store holds it
	path    string
	file    os.FileInfo // the file opened, told from one put in its place
	refs    int         // under registry
	idle    *time.Timer // forgets the vectors, after the last Store (linger); under registry

	mu       sync.Mutex
	cache    map[string]vector // vectors of cacheFor at generation cacheGen
	cacheFor string
	cacheGen int64
	cached   bool
	failures map[string]time.Time // content hashes the model refused, until
}

// keepNotes is how many generations of changes are noted for the caches.
const keepNotes = 100000

var (
	registry   sync.Mutex
	registered = map[string]*shared{}
)

// open returns the database of path, opening it (and making or checking
// its schema) the first time.
func open(path string) (*shared, error) {
	registry.Lock()
	defer registry.Unlock()
	if db, ok := registered[path]; ok {
		if now, err := os.Stat(path); err == nil && os.SameFile(now, db.file) {
			return db, db.take()
		}
		db.forget() // the file was removed or replaced: a new one, nothing kept
	}
	file, err := os.Stat(path)
	if err != nil {
		return nil, ioError(err, "cannot open the memory database")
	}
	db := &shared{path: path, file: file, failures: map[string]time.Time{}}
	if err := db.take(); err != nil {
		return nil, err
	}
	registered[path] = db
	return db, nil
}

// take adds a Store, opening the connections for the first. registry held.
func (db *shared) take() error {
	if db.DB == nil {
		conn, err := connect(db.path)
		if err != nil {
			return err
		}
		db.DB = conn
	}
	db.refs++
	if db.idle != nil {
		db.idle.Stop()
		db.idle = nil
	}
	return nil
}

// connect opens path and makes or checks its schema.
func connect(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, ioError(err, "cannot open the memory database")
	}
	if err := prepareSchema(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// prepareSchema makes or checks the schema and tidies the notes, again
// while another process holds the new database (two opening one at the
// same moment: the first makes it, the second waits its turn).
func prepareSchema(conn *sql.DB) error {
	var err error
	for attempt := range 20 {
		if err = migrate(conn); err == nil {
			if err = prune(conn); err == nil {
				return nil
			}
			err = ioError(err, "cannot tidy the memory database")
		}
		if !busy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return err
}

func busy(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "SQLITE_BUSY"))
}

// migrate makes the schema of a new database, and refuses one written by
// a newer version.
func migrate(conn *sql.DB) error {
	var version int
	if err := conn.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return ioError(err, "cannot read the memory database")
	}
	switch {
	case version > schemaVersion:
		return errors.New(agent.ErrMemoryIO, "the memory database was written by a newer version (schema "+strconv.Itoa(version)+")")
	case version == schemaVersion:
		return nil
	}
	if _, err := conn.Exec(schema + `PRAGMA user_version = ` + strconv.Itoa(schemaVersion) + `;`); err != nil {
		return ioError(err, "cannot make the memory database")
	}
	return nil
}

// linger is how long the decoded vectors of a database stay after its
// last Store closes, for serve's next run. A variable for tests.
var linger = 2 * time.Minute

// forget takes db out of the registry, closing it when no Store holds it
// (else with its last). registry held.
func (db *shared) forget() {
	delete(registered, db.path)
	if db.idle != nil {
		db.idle.Stop()
		db.idle = nil
	}
	if db.refs == 0 && db.DB != nil {
		_ = db.Close()
		db.DB = nil
	}
}

// release drops a Store; with the last, the connections close and the
// vectors wait linger for the next.
func (db *shared) release() error {
	registry.Lock()
	defer registry.Unlock()
	db.refs--
	if db.refs > 0 {
		return nil
	}
	err := db.Close()
	db.DB = nil
	if registered[db.path] != db {
		return err // forgotten meanwhile
	}
	if linger <= 0 {
		delete(registered, db.path)
		return err
	}
	db.idle = time.AfterFunc(linger, func() {
		registry.Lock()
		defer registry.Unlock()
		if db.refs == 0 && registered[db.path] == db {
			delete(registered, db.path)
		}
	})
	return err
}

// vectors are model's vectors of the records whose content they were made
// from. Every change of a vector or of a record's content, in any process,
// moves the generation and notes what changed: the cache reads only that,
// or everything when it is for another model or older than the notes kept.
// The map returned is the cache's own: read it, never write it.
func (db *shared) vectors(ctx context.Context, model string) (map[string]vector, error) {
	var gen, kept int64
	if err := db.QueryRowContext(ctx, `SELECT n, kept FROM generation`).Scan(&gen, &kept); err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	switch {
	case db.cached && db.cacheFor == model && db.cacheGen == gen:
		return db.cache, nil
	case db.cached && db.cacheFor == model && db.cacheGen >= kept:
		next, err := db.refresh(ctx, model, db.cacheGen)
		if err != nil {
			return nil, err
		}
		db.cache, db.cacheGen = next, gen
		return next, nil
	}
	return db.loadAll(ctx, model, gen)
}

// loadAll reads every current vector of model into the cache. db.mu held.
func (db *shared) loadAll(ctx context.Context, model string, gen int64) (map[string]vector, error) {
	vecs, err := db.loadVectors(ctx, model, `SELECT v.id, v.vec FROM vectors v JOIN records r ON r.id = v.id AND r.hash = v.hash WHERE v.model = ?`, model)
	if err != nil {
		return nil, err
	}
	db.cache, db.cacheFor, db.cacheGen, db.cached = vecs, model, gen, true
	return vecs, nil
}

// refresh is the cache with what changed after generation since read
// again: each id that changed is dropped, then taken back if it still has
// a current vector. A new map: one handed out earlier is never written.
func (db *shared) refresh(ctx context.Context, model string, since int64) (map[string]vector, error) {
	// From the few ids that changed to their rows (CROSS JOIN keeps that
	// order), never through every vector.
	changed, err := db.loadVectors(ctx, model, `WITH c(id) AS (SELECT id FROM vectors INDEXED BY vectors_at WHERE at > ? UNION ALL SELECT id FROM gone WHERE at > ?)
		SELECT v.id, v.vec FROM c CROSS JOIN vectors v ON v.id = c.id CROSS JOIN records r ON r.id = v.id AND r.hash = v.hash
		WHERE v.model = ?`, since, since, model)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM gone WHERE at > ?`, since)
	if err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	next := maps.Clone(db.cache)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, ioError(err, "cannot read the memory")
		}
		delete(next, id)
	}
	_ = rows.Close()
	maps.Copy(next, changed)
	return next, rows.Err()
}

func (db *shared) loadVectors(ctx context.Context, _ string, query string, args ...any) (map[string]vector, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	defer func() { _ = rows.Close() }()
	vecs := map[string]vector{}
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, ioError(err, "cannot read the memory")
		}
		vecs[id] = newVector(decode(blob))
	}
	return vecs, rows.Err()
}

// prune forgets the notes of what changed beyond the last keepNotes
// generations: a cache older than that reads everything again.
func prune(conn *sql.DB) error {
	_, err := conn.Exec(`UPDATE generation SET kept = max(kept, n - ?);
		DELETE FROM gone WHERE at <= (SELECT kept FROM generation);`, keepNotes)
	return err
}

// failedRecently: model refused the text of hash lately.
func (db *shared) failedRecently(model, hash string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	until, ok := db.failures[model+"\x00"+hash]
	return ok && time.Now().Before(until)
}

// failedCount is how many texts model refused lately.
func (db *shared) failedCount(model string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	n, now := 0, time.Now()
	for k, until := range db.failures {
		if strings.HasPrefix(k, model+"\x00") && now.Before(until) {
			n++
		}
	}
	return n
}

func (db *shared) markFailed(model string, hashes []string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.failures) > 10000 {
		clear(db.failures)
	}
	until := time.Now().Add(failedFor)
	for _, h := range hashes {
		db.failures[model+"\x00"+h] = until
	}
}
