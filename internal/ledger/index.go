// Derived index for the Context Ledger (M8.5.1).
//
// index.sqlite is never authoritative: it is rebuilt from ledger.md by Rebuild
// (and on Open). Two tables:
//
//   - ledger_fts  — FTS5 (porter unicode61) over id, kind, task, summary,
//     evidence, ts, status. BM25 weights zero the metadata columns so ranking
//     only reflects task, summary, and evidence paths.
//   - ledger_meta — the full entry as JSON, so Search needs nothing but the
//     index to reconstruct a result.
//
// Only recallable, active entries are inserted. Drift rows and superseded rows
// are never indexed, which is the second line of defence behind Search's own
// filter.
package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// errClosed is returned by index-backed operations after Close.
var errClosed = errors.New("ledger: store is closed")

const (
	ftsSchema = `CREATE VIRTUAL TABLE IF NOT EXISTS ledger_fts USING fts5(
		id, kind, task, summary, evidence, ts, status,
		tokenize = 'porter unicode61'
	)`
	metaSchema = `CREATE TABLE IF NOT EXISTS ledger_meta (
		id TEXT PRIMARY KEY,
		json TEXT NOT NULL
	)`
	// bm25 weights: id, kind, task, summary, evidence, ts, status.
	bm25Weights = `bm25(ledger_fts, 0.0, 0.0, 1.0, 1.0, 0.6, 0.0, 0.0)`
)

func openIndex(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(ftsSchema); err != nil {
		return err
	}
	_, err := s.db.Exec(metaSchema)
	return err
}

// insertEntry adds one entry to the derived index.
func (s *Store) insertEntry(e Entry) error {
	if s.db == nil {
		return errClosed
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertTx(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

func insertTx(tx *sql.Tx, e Entry) error {
	js, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ledger_fts (id, kind, task, summary, evidence, ts, status)
		VALUES (?,?,?,?,?,?,?)`,
		e.ID, e.Kind, e.Task, e.Summary, evidenceText(e), e.TS, e.Status); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO ledger_meta (id, json) VALUES (?,?)`, e.ID, string(js))
	return err
}

// reindexAll rebuilds the index from the in-memory entries. Callers hold the
// store mutex.
func (s *Store) reindexAll() (int, error) {
	if s.db == nil {
		return 0, errClosed
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM ledger_fts`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM ledger_meta`); err != nil {
		return 0, err
	}

	n := 0
	for _, e := range s.entries {
		if !indexable(e) {
			continue
		}
		if err := insertTx(tx, e); err != nil {
			return n, err
		}
		n++
	}
	return n, tx.Commit()
}

func evidenceText(e Entry) string {
	if len(e.Evidence) == 0 {
		return ""
	}
	paths := make([]string, 0, len(e.Evidence))
	for _, ev := range e.Evidence {
		paths = append(paths, ev.Path)
	}
	return strings.Join(paths, " ")
}

// Search returns recallable, active entries ranked by BM25 (ascending, best
// first, ties broken by id). Drift and superseded entries are never returned.
// topK defaults to DefaultTopK; maxTokens caps the cumulative estimate
// (len(task)+len(summary)+len(evidence paths))/4, and results are truncated at
// the first entry that would exceed it.
func (s *Store) Search(query string, topK, maxTokens int) ([]Entry, error) {
	if s.db == nil {
		return nil, errClosed
	}
	terms := sanitizeTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = DefaultTopK
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}

	rows, err := s.db.Query(`
		SELECT ledger_fts.id, ledger_meta.json
		FROM ledger_fts JOIN ledger_meta ON ledger_meta.id = ledger_fts.id
		WHERE ledger_fts MATCH ?
		ORDER BY `+bm25Weights+` ASC, ledger_fts.id ASC
		LIMIT ?`, strings.Join(quoted, " AND "), topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	used := 0
	for rows.Next() {
		var id, js string
		if err := rows.Scan(&id, &js); err != nil {
			return nil, err
		}
		var e Entry
		if err := json.Unmarshal([]byte(js), &e); err != nil {
			return nil, fmt.Errorf("ledger: index entry %s: %w", id, err)
		}
		if !recallable(e) {
			continue // defensive: the index only holds recallable rows
		}
		cost := tokenCost(e)
		if used+cost > maxTokens {
			break
		}
		used += cost
		out = append(out, e)
	}
	return out, rows.Err()
}

// tokenCost is the cheap 4-chars-per-token estimate used for the recall budget.
func tokenCost(e Entry) int {
	n := len(e.Task) + len(e.Summary)
	for _, ev := range e.Evidence {
		n += len(ev.Path)
	}
	return n / 4
}

// sanitizeTerms lowers and trims a free-text query into safe FTS terms, so
// hostile input ("\" AND ( OR *") cannot break the query grammar.
func sanitizeTerms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.Trim(f, "\"'`.,;:!?()[]{}*^~-\\/")
		if len(f) < 2 || stopword(f) || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func stopword(w string) bool {
	switch w {
	case "the", "and", "for", "with", "that", "this", "from", "does", "did", "was", "were", "are", "how", "why", "what", "into", "over", "than", "then", "them", "they", "our", "its":
		return true
	}
	return false
}
