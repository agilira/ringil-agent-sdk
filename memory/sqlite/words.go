// words.go: finding memories by their words
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package sqlite

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/memory/internal/embedgate"
)

// Bounds of a search by words.
const (
	maxQuery    = 4096 // bytes of a question read for its words
	maxTerms    = 64   // words of a question looked for
	minWord     = 3    // shorter words are found by meaning only
	commonShare = 5    // a word in more than 1/commonShare of the records is common
	smallMemory = 20   // below this many records no word is common
	maxCommon   = 4    // common words required together
)

// byWords finds the records holding the words of the query that matter.
//
// The words are split and folded by SQLite's own tokenizer, the one of the
// index, so a question spells them as the memories do ("ς" and "σ", "İ",
// accents written as two characters). Words shorter than minWord ("la",
// "of", also "Go") are found by meaning only, as in memory/chroma: in a
// small memory nothing tells an article from a word that matters. A word
// in more than a fifth of the records weighs a sixth of a rare one or less
// in BM25: the rare words rank alone; when every word is common, the
// records holding them together come first, then the newest holding the
// two rarest. Every word is quoted: the query is text, never FTS syntax.
func (s *Store) byWords(ctx context.Context, query string, limit int) ([]agent.MemoryRecord, error) {
	terms, err := s.queryTerms(ctx, query)
	if err != nil || len(terms) == 0 {
		return nil, err
	}
	found, err := s.docFreqs(ctx, terms)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&total); err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	if rare := rareTerms(found, total); len(rare) > 0 {
		return s.match(ctx, strings.Join(rare, " OR "), `bm25(records_fts), r.created DESC`, limit)
	}
	return s.byCommonWords(ctx, found, limit)
}

// byCommonWords: the records holding the (up to maxCommon) rarest common
// words together, by BM25, then the newest holding the two rarest.
func (s *Store) byCommonWords(ctx context.Context, found []docFreq, limit int) ([]agent.MemoryRecord, error) {
	common := make([]string, 0, maxCommon)
	for _, d := range found[:min(maxCommon, len(found))] {
		common = append(common, d.term)
	}
	together, err := s.match(ctx, strings.Join(common, " AND "), `bm25(records_fts), r.created DESC`, limit)
	if err != nil || len(together) >= limit {
		return together, err
	}
	newest, err := s.match(ctx, strings.Join(common[:min(2, len(common))], " OR "), `f.rowid DESC`, limit)
	return unseen(together, newest, limit), err
}

func (s *Store) match(ctx context.Context, expr, order string, limit int) ([]agent.MemoryRecord, error) {
	return s.query(ctx, `SELECT r.id, r.content, r.created FROM records_fts f JOIN records r ON r.rowid = f.rowid
		WHERE records_fts MATCH ? ORDER BY `+order+` LIMIT ?`, expr, limit)
}

// unseen is a followed by the records of b it lacks, up to limit.
func unseen(a, b []agent.MemoryRecord, limit int) []agent.MemoryRecord {
	seen := make(map[string]bool, len(a))
	for _, r := range a {
		seen[r.ID] = true
	}
	for _, r := range b {
		if len(a) >= limit {
			break
		}
		if !seen[r.ID] {
			a = append(a, r)
		}
	}
	return a
}

// queryTerms are the words of query as the index spells them, quoted for
// MATCH, the longest first: SQLite tokenizes the question in a temporary
// full-text table of the connection, with the index's tokenizer.
func (s *Store) queryTerms(ctx context.Context, query string) ([]string, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `
		CREATE VIRTUAL TABLE IF NOT EXISTS temp.question USING fts5(x, tokenize="unicode61 remove_diacritics 0");
		CREATE VIRTUAL TABLE IF NOT EXISTS temp.question_vocab USING fts5vocab('temp', 'question', 'row');
		DELETE FROM temp.question;`); err != nil {
		return nil, ioError(err, "cannot read the question")
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `DELETE FROM temp.question`) }()
	if _, err := conn.ExecContext(ctx, `INSERT INTO temp.question(x) VALUES (?)`, embedgate.Clip(query, maxQuery)); err != nil {
		return nil, ioError(err, "cannot read the question")
	}
	rows, err := conn.QueryContext(ctx, `SELECT term FROM temp.question_vocab`)
	if err != nil {
		return nil, ioError(err, "cannot read the question")
	}
	defer func() { _ = rows.Close() }()
	var terms []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, ioError(err, "cannot read the question")
		}
		if utf8.RuneCountInString(t) >= minWord {
			terms = append(terms, t)
		}
	}
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	return terms[:min(maxTerms, len(terms))], rows.Err()
}

// rareTerms are the words, quoted, that are not common among total
// records; in a small memory, all.
func rareTerms(found []docFreq, total int64) []string {
	var kept []string
	for _, d := range found {
		if d.n*commonShare <= total || total < smallMemory {
			kept = append(kept, d.term)
		}
	}
	return kept
}

type docFreq struct {
	term string // quoted for MATCH
	n    int64
}

// docFreqs are the words of terms some record holds, quoted, with the
// number of records holding each, rarest first.
func (s *Store) docFreqs(ctx context.Context, terms []string) ([]docFreq, error) {
	args := make([]any, len(terms))
	for i, t := range terms {
		args[i] = t
	}
	rows, err := s.db.QueryContext(ctx, `SELECT term, doc FROM records_vocab WHERE term IN (?`+strings.Repeat(",?", len(terms)-1)+`)`, args...)
	if err != nil {
		return nil, ioError(err, "cannot read the memory")
	}
	defer func() { _ = rows.Close() }()
	var found []docFreq
	for rows.Next() {
		var d docFreq
		if err := rows.Scan(&d.term, &d.n); err != nil {
			return nil, ioError(err, "cannot read the memory")
		}
		d.term = `"` + strings.ReplaceAll(d.term, `"`, `""`) + `"`
		found = append(found, d)
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].n < found[j].n })
	return found, rows.Err()
}
