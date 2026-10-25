// BM25 retrieval over knowledge-base chunks (WP-1.2).
//
// kb.go materializes the on-disk format and enforces the license policy; this
// file derives a disposable SQLite FTS5 index over it and answers two shapes of
// query: plain top-K retrieval (Search) and the same retrieval capped by an
// injected byte budget (SearchBudget). Markdown stays the source of truth: the
// index can be dropped and rebuilt at any time from KB.Chunks, exactly like the
// memory and skills indexes.
//
// # Ranking
//
// One FTS5 table holds the tier-3 rendering fields (id, title, tactic, body,
// source, license, adversary_prompt) tokenized with "porter unicode61", and
// results are ordered by bm25() ascending, so the best match comes first. The
// chunk id is indexed too, which makes a technique id ("T1059.001") a usable
// query: FTS5 tokenizes it as the phrase "t1059 001".
//
// # What is retrievable
//
// Only chunks whose license permits prompt injection are indexed. A chunk
// excluded by the license policy is skipped at Reindex time and recorded in
// Index.Warnings; it can never surface from Search, and MustInclude ids that
// name it are ignored (honoring them would inject content the license forbids).
//
// # Calling conventions
//
// topK <= 0 means the default of 8. mustInclude holds exact chunk ids,
// matched case-insensitively; those chunks are prepended in the order given,
// deduplicated against the BM25 hits, and returned even when they do not match
// the query at all. Unknown ids are ignored silently. All methods are safe on a
// nil *Index and return empty results.

package kb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// IndexFile is the derived index file created inside a KB directory by
// OpenIndex.
const IndexFile = "index.sqlite"

// defaultTopK is the result count used when a caller passes topK <= 0.
const defaultTopK = 8

// indexSchema creates the FTS5 table. It is virtual, so "IF NOT EXISTS" is all
// the migration machinery needed: the schema has no versioned columns and
// Reindex drops it before rebuilding.
const indexSchema = `CREATE VIRTUAL TABLE IF NOT EXISTS kb_chunks USING fts5(
	id, title, tactic, body, source, license, adversary_prompt,
	tokenize = 'porter unicode61'
)`

// Result is one ranked chunk. Score is the bm25 value (ascending is better, so
// values are negative) and is 0 for chunks forced in through mustInclude.
type Result struct {
	Chunk Chunk
	Score float64
}

// Index is a BM25 index over a loaded KB. DB is the SQLite handle and KB the
// source of truth the index was built from; both are exported so callers can
// pass the index around and inspect what it covers.
type Index struct {
	DB *sql.DB
	KB *KB
	// Warnings records what the most recent Reindex could not index, one line
	// per skipped chunk (non-injectable licenses). It is replaced, not
	// appended, on every Reindex.
	Warnings []string
}

// OpenIndex opens (creating if needed) the derived index for k at
// <k.Dir>/index.sqlite.
func OpenIndex(k *KB) (*Index, error) {
	if k == nil {
		return nil, fmt.Errorf("kb: open index: nil knowledge base")
	}
	if strings.TrimSpace(k.Dir) == "" {
		return nil, fmt.Errorf("kb: open index: knowledge base has no directory")
	}
	return OpenIndexAt(k, filepath.Join(k.Dir, IndexFile))
}

// OpenIndexAt opens the derived index for k at an explicit path. It exists for
// tests and for callers that keep derived state outside the KB directory.
func OpenIndexAt(k *KB, path string) (*Index, error) {
	if k == nil {
		return nil, fmt.Errorf("kb: open index: nil knowledge base")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("kb: open index: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("kb: open index %s: %w", path, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("kb: open index %s: %w", path, err)
	}
	// One writer/reader connection: this index is tiny, and a single
	// connection removes every lock-contention question (no WAL needed).
	db.SetMaxOpenConns(1)
	ix := &Index{DB: db, KB: k}
	if _, err := db.Exec(indexSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("kb: index schema: %w", err)
	}
	return ix, nil
}

// Close releases the database handle. It is a no-op on a nil index.
func (ix *Index) Close() error {
	if ix == nil || ix.DB == nil {
		return nil
	}
	return ix.DB.Close()
}

// Reindex rebuilds the index from ix.KB.Chunks(), dropping whatever was there
// first, and returns the number of indexed chunks. It is idempotent: running it
// twice over an unchanged KB yields the same count and the same rows.
//
// Chunks whose license forbids injection are skipped and reported through
// Index.Warnings. A nil index reports (0, nil).
func (ix *Index) Reindex() (int, error) {
	if ix == nil {
		return 0, nil
	}
	if ix.DB == nil {
		return 0, fmt.Errorf("kb: reindex: index has no database")
	}
	if ix.KB == nil {
		return 0, fmt.Errorf("kb: reindex: index has no knowledge base")
	}

	tx, err := ix.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Drop rather than DELETE: a rebuild is always a full rebuild, and this
	// also recovers from a stale schema.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS kb_chunks`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(indexSchema); err != nil {
		return 0, err
	}

	n := 0
	var warnings []string
	for _, c := range ix.KB.Chunks {
		if !c.Injectable {
			warnings = append(warnings, fmt.Sprintf("chunk %q not indexed: license %q forbids prompt injection", c.ID, c.License))
			continue
		}
		if _, err := tx.Exec(`INSERT INTO kb_chunks
			(id, title, tactic, body, source, license, adversary_prompt)
			VALUES (?,?,?,?,?,?,?)`,
			c.ID, c.Title, c.Tactic, c.Body, c.Source, c.License, c.AdversaryPrompt); err != nil {
			return n, fmt.Errorf("kb: index chunk %q: %w", c.ID, err)
		}
		n++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	ix.Warnings = warnings
	return n, nil
}

// Search returns the best BM25 matches for query, best first, limited to topK
// hits (topK <= 0 means 8). Chunks named in mustInclude are prepended in the
// order given, whether or not they match the query, and are never returned
// twice; they do not consume the topK count (but they do share its limit when
// the same chunk also ranks, so the caller still gets topK hits when possible).
func (ix *Index) Search(query string, topK int, mustInclude []string) ([]Result, error) {
	return ix.search(query, topK, 0, mustInclude)
}

// SearchBudget is Search with an injection budget: the must-include chunks come
// first, then BM25 hits are added while the cumulative byte length of their
// title+body+adversary_prompt stays within maxBytes. maxBytes <= 0 means "no
// cap". The byte count covers only the optional hits; must-include chunks are
// mandatory and always returned.
func (ix *Index) SearchBudget(query string, topK, maxBytes int, mustInclude []string) ([]Result, error) {
	return ix.search(query, topK, maxBytes, mustInclude)
}

// search is the shared implementation. maxBytes <= 0 disables the byte cap.
func (ix *Index) search(query string, topK, maxBytes int, mustInclude []string) ([]Result, error) {
	if ix == nil || ix.DB == nil {
		return nil, nil
	}
	if topK <= 0 {
		topK = defaultTopK
	}

	must := ix.mustChunks(mustInclude)
	// Over-fetch by the number of forced chunks so the caller still gets a
	// full topK after the duplicates are removed.
	hits, err := ix.bm25(query, topK+len(must))
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(must)+len(hits))
	out := make([]Result, 0, topK+len(must))
	for _, c := range must {
		key := foldID(c.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Result{Chunk: c})
	}

	optional := 0
	used := 0
	for _, h := range hits {
		if optional >= topK {
			break
		}
		key := foldID(h.Chunk.ID)
		if seen[key] {
			continue
		}
		if maxBytes > 0 {
			size := injectBytes(h.Chunk)
			if used+size > maxBytes {
				break
			}
			used += size
		}
		seen[key] = true
		optional++
		out = append(out, h)
	}
	return out, nil
}

// bm25 runs the FTS5 query and returns up to limit ranked hits.
func (ix *Index) bm25(query string, limit int) ([]Result, error) {
	match := kbMatchQuery(query)
	if match == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := ix.DB.Query(`
		SELECT id, title, tactic, body, source, license, adversary_prompt,
		       bm25(kb_chunks)
		FROM kb_chunks
		WHERE kb_chunks MATCH ?
		ORDER BY bm25(kb_chunks), id
		LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Result
	for rows.Next() {
		var c Chunk
		var score float64
		if err := rows.Scan(&c.ID, &c.Title, &c.Tactic, &c.Body, &c.Source,
			&c.License, &c.AdversaryPrompt, &score); err != nil {
			return nil, err
		}
		// Return the full chunk from the KB (platforms, path, retrieved_at,
		// injectability) rather than the reduced column set in the index.
		if ix.KB != nil {
			if full, ok := ix.KB.Find(c.ID); ok {
				c = full
			}
		}
		out = append(out, Result{Chunk: c, Score: score})
	}
	return out, rows.Err()
}

// mustChunks resolves must-include ids to chunks, preserving order, dropping
// blanks, duplicates, unknown ids, and chunks the license forbids injecting.
func (ix *Index) mustChunks(ids []string) []Chunk {
	if ix == nil || ix.KB == nil {
		return nil
	}
	var out []Chunk
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		c, ok := ix.KB.Find(id)
		if !ok || !c.Injectable {
			continue
		}
		key := foldID(c.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// injectBytes is the size a chunk contributes to a prompt budget.
func injectBytes(c Chunk) int {
	return len(c.Title) + len(c.Body) + len(c.AdversaryPrompt)
}

// foldID normalizes a chunk id for case-insensitive comparisons.
func foldID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// kbMatchQuery turns free text into a safe FTS5 MATCH expression: every term
// becomes a quoted string joined with AND, so quotes, operators, and other FTS
// metacharacters in user input can never break the query.
func kbMatchQuery(query string) string {
	terms := kbQueryTerms(query)
	if len(terms) == 0 {
		return ""
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	return strings.Join(parts, " AND ")
}

// kbQueryTerms splits a query into searchable terms. Punctuation at the edges
// is trimmed ("-EncodedCommand" -> "encodedcommand"); internal punctuation is
// kept because the tokenizer splits it into an adjacent phrase ("T1059.001" ->
// "t1059 001"), which is exactly what a technique id should match.
func kbQueryTerms(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, "\"'`.,;:!?()[]{}*^~-")
		if len(f) >= 2 && !kbStopword(f) {
			out = append(out, f)
		}
	}
	return out
}

// kbStopword drops high-frequency words that would only dilute an AND query.
func kbStopword(w string) bool {
	switch w {
	case "the", "and", "for", "with", "that", "this", "from", "does", "did",
		"was", "were", "are", "how", "why", "what", "into", "when", "then":
		return true
	}
	return false
}
