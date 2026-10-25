// SQLite/WAL session store (DESIGN §10, WP-2.1).
//
// One session = one SQLite database file, <Dir()>/<id>.sqlite, in WAL journal
// mode. That mirrors the old JSONL model deliberately: retention and fork stay
// plain file operations, and two concurrent sessions never contend for the same
// write lock. Legacy <id>.jsonl files are imported once, on first touch, and are
// left exactly as they are (audit trail); see migrateJSONL.
//
// Event shape is unchanged from the JSONL days: an event is {ts, kind, v}. The
// table keeps ts and kind as columns and the payload (v) in the json column, so
// ExportJSONL can project the stream back to the legacy line format byte for
// byte. A missing/null payload is stored as the empty string and re-emitted
// without a "v" field, matching the omitempty behaviour of the old writer.
package session

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sqliteSuffix = ".sqlite"
	legacySuffix = ".jsonl"

	// migratingSuffix marks an in-flight legacy import. The finished file is
	// renamed into place so a crashed import never leaves a half-populated
	// store that would be mistaken for a complete one.
	migratingSuffix = ".migrating"
)

const (
	schemaEvents = `CREATE TABLE IF NOT EXISTS events (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	ts   TEXT NOT NULL,
	kind TEXT NOT NULL,
	json TEXT NOT NULL
)`
	schemaKindIndex = `CREATE INDEX IF NOT EXISTS events_kind ON events(kind)`
)

// storedEvent is one row as it lives on disk: the payload is the raw JSON text
// of the event's "v" field ("" when there was none).
type storedEvent struct {
	TS      string
	Kind    string
	Payload string
}

// legacyEntry is the JSONL line shape still used for migration and export.
type legacyEntry struct {
	TS   string          `json:"ts"`
	Kind string          `json:"kind"`
	V    json.RawMessage `json:"v"`
}

// openStore opens (creating if needed) one session database, pins it to a
// single connection (one writer per session), and ensures WAL + schema.
func openStore(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		db.Close()
		return nil, fmt.Errorf("journal_mode = %q, want wal", mode)
	}
	for _, stmt := range []string{`PRAGMA busy_timeout = 5000`, schemaEvents, schemaKindIndex} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// openSessionAt opens (or migrates then opens) the session id inside dir.
func openSessionAt(dir, id string) (*Session, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	p := filepath.Join(dir, id+sqliteSuffix)
	if _, err := os.Stat(p); err != nil {
		if _, legacyErr := os.Stat(filepath.Join(dir, id+legacySuffix)); legacyErr != nil {
			ids, _ := listIDs(dir)
			return nil, fmt.Errorf("no session %q (available: %s)", id, strings.Join(ids, ", "))
		}
		if err := migrateJSONL(dir, id); err != nil {
			return nil, fmt.Errorf("session %q: migrate: %w", id, err)
		}
	}
	db, err := openStore(p)
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", id, err)
	}
	return &Session{ID: id, Path: p, db: db}, nil
}

// migrateJSONL imports <id>.jsonl into a new <id>.sqlite, in file order, then
// records a meta event naming the source and the number of events imported.
// The JSONL file itself is only ever read: it stays on disk untouched as the
// audit copy of what was originally logged.
func migrateJSONL(dir, id string) error {
	legacyPath := filepath.Join(dir, id+legacySuffix)
	legacy, err := readLegacyFile(legacyPath)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, id+sqliteSuffix)
	tmp := target + migratingSuffix
	// Clear debris from an import that died mid-flight; only ever touches the
	// .migrating scratch paths, never the legacy file.
	for _, f := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
		os.Remove(f)
	}
	db, err := openStore(tmp)
	if err != nil {
		return err
	}
	abort := func(err error) error {
		db.Close()
		for _, f := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
			os.Remove(f)
		}
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return abort(err)
	}
	for _, e := range legacy {
		if _, err := tx.Exec(`INSERT INTO events (ts, kind, json) VALUES (?,?,?)`,
			e.TS, e.Kind, normalizePayload(e.V)); err != nil {
			tx.Rollback()
			return abort(err)
		}
	}
	meta, err := json.Marshal(map[string]any{"migrated_from": "jsonl", "events": len(legacy)})
	if err != nil {
		tx.Rollback()
		return abort(err)
	}
	if _, err := tx.Exec(`INSERT INTO events (ts, kind, json) VALUES (?,?,?)`,
		nowTS(), "meta", string(meta)); err != nil {
		tx.Rollback()
		return abort(err)
	}
	if err := tx.Commit(); err != nil {
		return abort(err)
	}
	// Close checkpoints the WAL back into the main file so the rename moves a
	// single, self-contained database.
	if err := db.Close(); err != nil {
		return abort(err)
	}
	if _, err := os.Stat(target); err == nil {
		// Another process won the race; keep its store and drop the duplicate.
		for _, f := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
			os.Remove(f)
		}
		return nil
	}
	return os.Rename(tmp, target)
}

// readLegacyFile parses a JSONL session file. Unparseable lines are skipped,
// exactly as the old reader did.
func readLegacyFile(path string) ([]legacyEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []legacyEntry
	for _, raw := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var e legacyEntry
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// normalizePayload stores "no payload" as the empty string. A JSON null and a
// missing "v" are the same thing to every reader in this package, and the old
// writer omitted both (omitempty).
func normalizePayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if s == "null" || !json.Valid([]byte(s)) {
		return ""
	}
	return s
}

func nowTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// readEntriesAt reads a session's event stream, importing a legacy JSONL file
// on first touch. All reads go through the store so there is exactly one
// decode path.
func readEntriesAt(dir, id string) ([]entry, error) {
	s, err := openSessionAt(dir, id)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.entries()
}

// entries returns the session's events in log order.
func (s *Session) entries() ([]entry, error) {
	rows, err := s.stored()
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(rows))
	for _, r := range rows {
		e := entry{TS: r.TS, Kind: r.Kind}
		if r.Payload != "" {
			var v any
			if json.Unmarshal([]byte(r.Payload), &v) == nil {
				e.V = v
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// stored returns the raw rows in log order.
func (s *Session) stored() ([]storedEvent, error) {
	db := s.handle()
	if db == nil {
		return nil, fmt.Errorf("session: closed")
	}
	rows, err := db.Query(`SELECT ts, kind, json FROM events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedEvent
	for rows.Next() {
		var r storedEvent
		if err := rows.Scan(&r.TS, &r.Kind, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// copyEvents appends every row of src to dst, preserving timestamps and
// payload bytes; the source store is only read.
func copyEvents(src, dst *Session) error {
	events, err := src.stored()
	if err != nil {
		return err
	}
	if dst == nil || dst.db == nil {
		return fmt.Errorf("session: closed")
	}
	tx, err := dst.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if _, err := tx.Exec(`INSERT INTO events (ts, kind, json) VALUES (?,?,?)`,
			e.TS, e.Kind, e.Payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ExportJSONL streams a session's events to w as legacy JSONL lines, in log
// order. It is the projection `kintsugi session export --jsonl` is wired to: the
// DB stays the source of truth, the file is a view of it.
func ExportJSONL(id string, w io.Writer) error {
	if w == nil {
		return fmt.Errorf("export session %q: nil writer", id)
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	s, err := openSessionAt(dir, id)
	if err != nil {
		return err
	}
	defer s.Close()
	events, err := s.stored()
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	for _, e := range events {
		line, err := marshalEventLine(e)
		if err != nil {
			return err
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// marshalEventLine renders one event in the legacy JSONL shape
// {"ts":…,"kind":…,"v":…}, emitting the stored payload bytes verbatim.
func marshalEventLine(e storedEvent) ([]byte, error) {
	ts, err := json.Marshal(e.TS)
	if err != nil {
		return nil, err
	}
	kind, err := json.Marshal(e.Kind)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(`{"ts":`)
	b.Write(ts)
	b.WriteString(`,"kind":`)
	b.Write(kind)
	if e.Payload != "" {
		b.WriteString(`,"v":`)
		b.WriteString(e.Payload)
	}
	b.WriteString(`}`)
	return []byte(b.String()), nil
}

// listIDs returns every session id in dir, whether it lives in the new store or
// still only as a legacy JSONL file.
func listIDs(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, sqliteSuffix):
			seen[strings.TrimSuffix(name, sqliteSuffix)] = true
		case strings.HasSuffix(name, legacySuffix):
			seen[strings.TrimSuffix(name, legacySuffix)] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// storeSize is the on-disk footprint of a session store, including the WAL that
// holds not-yet-checkpointed events.
func storeSize(p string) int64 {
	var n int64
	for _, f := range []string{p, p + "-wal"} {
		if st, err := os.Stat(f); err == nil {
			n += st.Size()
		}
	}
	return n
}

// eachSession visits every session in dir (newest id last), importing legacy
// JSONL files as it goes. Best effort: unreadable sessions are skipped, which
// is what the telemetry aggregators want.
func eachSession(dir string, fn func(entries []entry)) {
	ids, err := listIDs(dir)
	if err != nil {
		return
	}
	for _, id := range ids {
		entries, err := readEntriesAt(dir, id)
		if err != nil {
			continue
		}
		fn(entries)
	}
}
