// Package session persists one session per SQLite/WAL store (DESIGN §10), with
// the legacy JSONL line format kept as an import source and an export
// projection. Secrets are redacted before anything touches disk (§1.2).
package session

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/redact"
)

type Session struct {
	ID string
	// Path is the session's SQLite store, <Dir()>/<ID>.sqlite. A legacy
	// <Dir()>/<ID>.jsonl may also exist; it is never rewritten.
	Path string
	db   *sql.DB
	// mu guards the db field. The agent goroutine logs events while the UI
	// thread may Close the session (a quit or a test cleanup mid-turn), so
	// the field itself is shared state: Close nils it under the lock and
	// every reader captures it under the lock. The SQL calls then run on the
	// captured handle outside the lock — database/sql is safe for concurrent
	// use, and after Close those calls return an error rather than racing.
	mu sync.Mutex
}

// handle returns the live store, or nil once the session is closed. It is the
// one sanctioned way to read the db field; every method that touches the
// store captures its handle through here.
func (s *Session) handle() *sql.DB {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db
}

// TitleMaxRunes bounds a derived session title.
const TitleMaxRunes = 60

// noTitle is what a session without a title shows. Older sessions have no
// title at all; they must keep working and stay readable in a table.
const noTitle = "—"

// entry is the in-memory event shape. On disk ts and kind are columns and the
// payload lives in the json column; reads rebuild an entry from all three.
type entry struct {
	TS   string `json:"ts"`
	Kind string `json:"kind"`
	V    any    `json:"v,omitempty"`
}

func Dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "kintsugi", "sessions")
	return d, os.MkdirAll(d, 0o755)
}

func New() (*Session, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	id := time.Now().Format("20060102-150405") + "-" + shortID()
	p := filepath.Join(dir, id+sqliteSuffix)
	db, err := openStore(p)
	if err != nil {
		return nil, err
	}
	return &Session{ID: id, Path: p, db: db}, nil
}

// shortID keeps two sessions started in the same second distinct.
func shortID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return fmt.Sprintf("%x", b)
}

// Log appends one redacted event. The store is the only writer, so events land
// in log order. A user turn also names the session: the first one gives it a
// title (M9.7).
//
// The agent goroutine calls this while the UI thread may Close the session
// (a quit mid-turn), so the db handle is captured under the session lock; a
// Log that loses the race to a Close simply drops the event.
func (s *Session) Log(kind string, v any) {
	db := s.handle()
	if db == nil {
		return
	}
	payload, err := json.Marshal(redactAny(v))
	if err != nil {
		return
	}
	if _, err := db.Exec(`INSERT INTO events (ts, kind, json) VALUES (?,?,?)`,
		nowTS(), kind, normalizePayload(payload)); err != nil {
		return
	}
	if kind == "user" {
		// The first operator turn names the session (M9.7).
		s.ensureTitle(v)
	}
}

// ensureTitle gives a session its title the first time an operator turn is
// recorded. The title is derived locally — a heuristic, never a model call —
// and stored in the session's meta event, so a later read sees it without
// replaying the transcript and `kintsugi session rename` can replace it.
func (s *Session) ensureTitle(v any) {
	task, ok := v.(string)
	if !ok {
		return
	}
	s.SetTitle(DeriveTitle(task))
}

// SetTitle stores a title for the session when it has none yet. It reports
// whether a title was written: a session that already has one keeps it,
// because a title belongs to the first task of a session, not to the newest
// one. Rename is the explicit override.
func (s *Session) SetTitle(title string) bool {
	if s.handle() == nil {
		return false
	}
	t := strings.TrimSpace(title)
	if t == "" {
		return false
	}
	if s.hasTitle() {
		return false
	}
	s.writeTitle(t)
	return true
}

// writeTitle records the title in the session's meta event.
//
// The meta event is the session header — model, persona, role, stack — and a
// title is one more header field: it joins the header instead of adding a
// row to the transcript, so the event stream keeps the shape it had before
// titles existed. A session without a meta event yet gets one.
func (s *Session) writeTitle(title string) {
	t := Redact(title)
	id, payload, ok := s.firstMeta()
	if !ok {
		s.Log("meta", map[string]string{"title": t})
		return
	}
	merged, err := mergeJSONField(payload, "title", t)
	if err != nil {
		s.Log("meta", map[string]string{"title": t})
		return
	}
	db := s.handle()
	if db == nil {
		return
	}
	if _, err := db.Exec(`UPDATE events SET json = ? WHERE id = ?`, merged, id); err != nil {
		return
	}
}

// firstMeta returns the id and the payload of the session's first meta event.
func (s *Session) firstMeta() (int64, string, bool) {
	db := s.handle()
	if db == nil {
		return 0, "", false
	}
	var id int64
	var payload string
	err := db.QueryRow(`SELECT id, json FROM events WHERE kind = 'meta' ORDER BY id LIMIT 1`).Scan(&id, &payload)
	if err != nil {
		return 0, "", false
	}
	return id, payload, true
}

// hasTitle reports whether the session already carries a title.
func (s *Session) hasTitle() bool {
	db := s.handle()
	if db == nil {
		return false
	}
	rows, err := db.Query(`SELECT json FROM events WHERE kind = 'meta'`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var v any
		if json.Unmarshal([]byte(payload), &v) != nil {
			continue
		}
		if strings.TrimSpace(metaString(v, "title")) != "" {
			return true
		}
	}
	return false
}

// mergeJSONField adds a string field to a JSON object payload, keeping the
// order of the fields that are already there. The session header is written
// by several callers; rewriting it must not shuffle its fields.
func mergeJSONField(payload, key, value string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(payload))
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", fmt.Errorf("payload is not a JSON object")
	}
	var pairs []jsonPair
	seen := false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", err
		}
		k, ok := kt.(string)
		if !ok {
			return "", fmt.Errorf("payload key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", err
		}
		if k == key {
			seen = true
			pairs = append(pairs, jsonPair{key: k, val: value})
			continue
		}
		pairs = append(pairs, jsonPair{key: k, val: raw})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return "", err
	}
	if !seen {
		pairs = append(pairs, jsonPair{key: key, val: value})
	}
	b, err := marshalPairs(pairs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Rename sets a session's title explicitly, replacing whatever was there. The
// title is the operator's own words: renaming never calls a model.
func Rename(id, title string) error {
	t := strings.TrimSpace(title)
	if t == "" {
		return fmt.Errorf("title is empty")
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
	s.Log("meta", map[string]string{"title": t})
	return nil
}

// DeriveTitle turns an operator task into a short, single-line label.
//
// The rule is a local heuristic, never an API request:
//
//  1. every whitespace run — newlines included — collapses to one space;
//  2. the result is trimmed;
//  3. it is cut to TitleMaxRunes runes, at a word boundary when one is close
//     enough, and an ellipsis marks the cut.
//
// An empty task has no title: a session with nothing to name keeps showing an
// em dash.
func DeriveTitle(task string) string {
	s := strings.Join(strings.Fields(task), " ")
	if s == "" {
		return ""
	}
	return cutTitle(s, TitleMaxRunes)
}

// cutTitle cuts s to at most n runes, preferring a word boundary and marking
// the cut with an ellipsis.
func cutTitle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := r[:n]
	// Back up to the last space, but never below three quarters of the
	// budget: a long first word still yields a title.
	if i := lastSpace(cut); i >= n*3/4 {
		cut = cut[:i]
	}
	return strings.TrimRight(string(cut), " ") + "…"
}

// lastSpace returns the index of the last space in r, or -1.
func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == ' ' {
			return i
		}
	}
	return -1
}

// metaString reads one string field out of a meta event payload. A meta event
// may mix strings and numbers (a migration row does), so the payload is read
// as a generic map rather than a map of strings.
func metaString(v any, key string) string {
	switch m := v.(type) {
	case map[string]any:
		s, _ := m[key].(string)
		return s
	case map[string]string:
		return m[key]
	}
	return ""
}

// Close releases the store. It is safe against an in-flight Log on another
// goroutine: the field is nilled under the session lock, so a concurrent
// writer either captured the handle before the close (its SQL then returns
// an error) or sees nil and drops the event. Either way, no race.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
}

// Redact delegates to the shared redactor.
func Redact(s string) string { return redact.Redact(s) }

func redactAny(v any) any {
	switch t := v.(type) {
	case string:
		return Redact(t)
	case []string:
		out := make([]string, len(t))
		for i := range t {
			out[i] = Redact(t[i])
		}
		return out
	case map[string]string:
		out := map[string]string{}
		for k, val := range t {
			out[k] = Redact(val)
		}
		return out
	default:
		// Structured payloads (tool calls) go through JSON to reach nested
		// strings, but only when they plausibly contain secrets.
		b, err := json.Marshal(v)
		if err != nil {
			return v
		}
		var generic any
		if json.Unmarshal(b, &generic) != nil {
			return v
		}
		return redactTree(generic)
	}
}

func redactTree(v any) any {
	switch t := v.(type) {
	case string:
		return Redact(t)
	case []any:
		for i := range t {
			t[i] = redactTree(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = redactTree(t[k])
		}
		return t
	default:
		return v
	}
}

// TranscriptText renders a session for the curator: a bounded, role-tagged
// transcript rather than raw event JSON.
func TranscriptText(id string, maxLines int) (string, error) {
	entries, err := readEntries(id)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, e := range entries {
		s, _ := e.V.(string)
		if s == "" {
			continue
		}
		switch e.Kind {
		case "user":
			lines = append(lines, "OPERATOR: "+s)
		case "assistant":
			lines = append(lines, "ASSISTANT: "+truncate(s, 4000))
		case "tool_call":
			lines = append(lines, "TOOL CALL: "+truncate(s, 500))
		case "tool_result":
			lines = append(lines, "TOOL RESULT: "+truncate(s, 800))
		case "error":
			lines = append(lines, "ERROR: "+s)
		}
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n"), nil
}

// LatestID returns the most recent session id.
func LatestID() (string, error) {
	ids, err := List()
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no sessions found")
	}
	max := ids[0]
	for _, id := range ids[1:] {
		if id > max {
			max = id
		}
	}
	return max, nil
}

// Show replays a session.
func Show(id string) error {
	entries, err := readEntries(id)
	if err != nil {
		return err
	}
	title := ""
	for _, e := range entries {
		if e.Kind != "meta" {
			continue
		}
		if t := strings.TrimSpace(metaString(e.V, "title")); t != "" {
			title = t
		}
	}
	fmt.Printf("session %s\n", id)
	fmt.Printf("title   %s\n", dash(title))
	for _, e := range entries {
		v := ""
		if s, ok := e.V.(string); ok {
			v = s
		} else {
			b, _ := json.Marshal(e.V)
			v = string(b)
		}
		switch e.Kind {
		case "user":
			fmt.Printf("you ** %s\n", v)
		case "thinking":
			fmt.Printf("  ◇ thinking  %s\n", truncate(v, 800))
		case "assistant":
			fmt.Printf("kintsugi  %s\n", v)
		case "tool_call":
			fmt.Printf("  ⚙ %s\n", truncate(v, 400))
		case "tool_result":
			fmt.Printf("  → %s\n", truncate(v, 600))
		case "error":
			fmt.Printf("  ✗ %s\n", v)
		case "meta":
			fmt.Printf("  (%s)\n", v)
		default:
			fmt.Printf("  (%s) %s\n", e.Kind, truncate(v, 300))
		}
	}
	return nil
}

// List returns every session id in Dir(), whether it owns a store or only a
// legacy JSONL file.
func List() ([]string, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	return listIDs(d)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// dash returns s, or an em dash when s is empty. A session without a title
// shows the dash instead of an empty column.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return noTitle
	}
	return s
}

// fitText truncates s to at most n runes, marking the cut with an ellipsis.
// It counts runes, not bytes, so a multi-byte title is never cut in half.
func fitText(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}
