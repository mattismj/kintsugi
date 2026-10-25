package session

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"kintsugi/internal/provider"
)

// withSessionDir points the sessions directory at a fresh temp dir (via the
// XDG cache root the existing tests already use) and returns it.
func withSessionDir(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustOpenRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStoreRoundTrip(t *testing.T) {
	dir := withSessionDir(t)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, s.ID+".sqlite"); s.Path != want {
		t.Fatalf("path = %q, want %q", s.Path, want)
	}
	s.Log("meta", map[string]string{"model": "p/m", "persona": "flash", "role": "coder", "stack": "abc"})
	s.Log("user", "do the thing")
	s.Log("assistant", "")
	tc := provider.ToolCall{
		ID: "call-1", Type: "function",
		Function: provider.Function{Name: "read", Arguments: `{"path":"README.md"}`},
	}
	s.Log("tool_call", tc)
	s.Log("tool_result", "hello")
	s.Log("assistant", "done")
	id := s.ID
	s.Close()

	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); err == nil {
		t.Fatal("new session wrote a JSONL file")
	}

	// Reopen through the public API: the store must stand on its own.
	s2, err := Open(id)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s2.entries()
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if len(entries) != 6 {
		t.Fatalf("entries = %d: %+v", len(entries), entries)
	}
	kinds := []string{"meta", "user", "assistant", "tool_call", "tool_result", "assistant"}
	for i, e := range entries {
		if e.Kind != kinds[i] {
			t.Fatalf("entry %d kind = %q, want %q", i, e.Kind, kinds[i])
		}
		if e.TS == "" {
			t.Fatalf("entry %d has no timestamp", i)
		}
		if _, err := time.Parse(time.RFC3339Nano, e.TS); err != nil {
			t.Fatalf("entry %d ts %q: %v", i, e.TS, err)
		}
	}

	hist, err := History(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 4 {
		t.Fatalf("history = %d messages: %+v", len(hist), hist)
	}
	if hist[0].Role != "user" || hist[0].Content != "do the thing" {
		t.Fatalf("user message wrong: %+v", hist[0])
	}
	if hist[1].Role != "assistant" || len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].ID != "call-1" ||
		hist[1].ToolCalls[0].Function.Name != "read" {
		t.Fatalf("assistant tool call not reattached: %+v", hist[1])
	}
	if hist[2].Role != "tool" || hist[2].ToolCallID != "call-1" || hist[2].Name != "read" || hist[2].Content != "hello" {
		t.Fatalf("tool result not paired: %+v", hist[2])
	}
	if hist[3].Role != "assistant" || hist[3].Content != "done" {
		t.Fatalf("final assistant wrong: %+v", hist[3])
	}

	// The file really is a WAL-mode SQLite database.
	var mode string
	if err := mustOpenRaw(t, s.Path).QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestStoreOpenMissing(t *testing.T) {
	withSessionDir(t)
	s, err := Open("20260101-000000-abcdef")
	if err == nil {
		s.Close()
		t.Fatal("opened a session that does not exist")
	}
	if !strings.Contains(err.Error(), "no session") {
		t.Fatalf("error = %v", err)
	}
}

// legacyFixture is a hand-written JSONL session in exactly the shape the old
// writer produced (json.Marshal of {ts, kind, v}).
var legacyFixture = []string{
	`{"ts":"2026-10-21T01:00:00Z","kind":"meta","v":{"model":"p/m","persona":"flash","role":"coder","stack":"abc"}}`,
	`{"ts":"2026-10-21T01:00:01Z","kind":"user","v":"do the thing"}`,
	`{"ts":"2026-10-21T01:00:02Z","kind":"assistant","v":""}`,
	`{"ts":"2026-10-21T01:00:03Z","kind":"tool_call","v":{"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"}}}`,
	`{"ts":"2026-10-21T01:00:04Z","kind":"tool_result","v":"hello"}`,
	`{"ts":"2026-10-21T01:00:05Z","kind":"assistant","v":"done"}`,
}

func TestMigrateLegacyJSONL(t *testing.T) {
	dir := withSessionDir(t)
	const id = "20261021-010000-aaaaaa"
	legacyPath := filepath.Join(dir, id+".jsonl")
	original := []byte(strings.Join(legacyFixture, "\n") + "\n")
	if err := os.WriteFile(legacyPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	// A legacy-only session is visible before anyone touches it.
	ids, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("List = %v, want [%s]", ids, id)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".sqlite")); err == nil {
		t.Fatal("List migrated the legacy session")
	}

	// First touch imports it.
	s, err := Open(id)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	raw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatalf("legacy JSONL was modified:\n got %q\nwant %q", raw, original)
	}

	entries, err := readEntries(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(legacyFixture)+1 {
		t.Fatalf("entries = %d, want %d: %+v", len(entries), len(legacyFixture)+1, entries)
	}
	for i, line := range legacyFixture {
		var want legacyEntry
		if err := json.Unmarshal([]byte(line), &want); err != nil {
			t.Fatal(err)
		}
		got := entries[i]
		if got.TS != want.TS || got.Kind != want.Kind {
			t.Fatalf("entry %d = %s/%s, want %s/%s", i, got.Kind, got.TS, want.Kind, want.TS)
		}
		// Semantic comparison: object key order is not part of the event
		// stream, and the bytes themselves are checked by ExportJSONL below.
		gotJSON, err := json.Marshal(got.V)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(gotJSON, want.V) {
			t.Fatalf("entry %d payload = %s, want %s", i, gotJSON, want.V)
		}
	}

	// The import is recorded as a meta event.
	last := entries[len(entries)-1]
	if last.Kind != "meta" {
		t.Fatalf("last entry kind = %q, want meta", last.Kind)
	}
	var marker struct {
		MigratedFrom string `json:"migrated_from"`
		Events       int    `json:"events"`
	}
	if !remarshal(last.V, &marker) {
		t.Fatalf("migration marker unreadable: %+v", last.V)
	}
	if marker.MigratedFrom != "jsonl" || marker.Events != len(legacyFixture) {
		t.Fatalf("migration marker = %+v, want jsonl/%d", marker, len(legacyFixture))
	}

	// The imported stream round-trips back to the original bytes, followed by
	// the one meta event recording the import itself.
	var export bytes.Buffer
	if err := ExportJSONL(id, &export); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(export.Bytes(), original) {
		t.Fatalf("export prefix mismatch:\n got %q\nwant %q", export.Bytes(), original)
	}
	extra := strings.TrimSuffix(strings.TrimPrefix(export.String(), string(original)), "\n")
	var extraEvent legacyEntry
	if err := json.Unmarshal([]byte(extra), &extraEvent); err != nil {
		t.Fatalf("trailing export line %q: %v", extra, err)
	}
	if extraEvent.Kind != "meta" {
		t.Fatalf("trailing export event = %+v, want meta", extraEvent)
	}

	// A second open must not duplicate anything, and must not import again.
	s, err = Open(id)
	if err != nil {
		t.Fatal(err)
	}
	s.Log("user", "after migration")
	s.Close()
	rows := countRows(t, filepath.Join(dir, id+".sqlite"))
	if rows != len(legacyFixture)+2 {
		t.Fatalf("rows after second open = %d, want %d", rows, len(legacyFixture)+2)
	}
	metas := countKind(t, filepath.Join(dir, id+".sqlite"), "meta")
	if metas != 2 { // imported meta + migration marker
		t.Fatalf("meta rows = %d, want 2", metas)
	}

	// History/InfoOf work over the imported stream, including tool pairing.
	hist, err := History(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 5 { // user, assistant+tools, tool, assistant, user
		t.Fatalf("history = %d: %+v", len(hist), hist)
	}
	if hist[1].Role != "assistant" || len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("migrated tool call not reattached: %+v", hist[1])
	}
	if hist[2].ToolCallID != "call-1" || hist[2].Content != "hello" {
		t.Fatalf("migrated tool result not paired: %+v", hist[2])
	}
	info, err := InfoOf(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "p/m" || info.Persona != "flash" || info.Role != "coder" || info.Stack != "abc" {
		t.Fatalf("info = %+v", info)
	}
	if info.Turns != 2 { // the fixture's user turn plus the one logged after the import
		t.Fatalf("turns = %d, want 2", info.Turns)
	}
	if info.Started.IsZero() || info.LastAt.IsZero() {
		t.Fatalf("timestamps not derived: %+v", info)
	}
}

func TestForkStore(t *testing.T) {
	dir := withSessionDir(t)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("meta", map[string]string{"model": "p/m", "persona": "flash"})
	s.Log("user", "refactor the auth middleware")
	s.Log("assistant", "on it")
	srcID := s.ID
	s.Close()

	var before bytes.Buffer
	if err := ExportJSONL(srcID, &before); err != nil {
		t.Fatal(err)
	}
	srcEntries, err := readEntries(srcID)
	if err != nil {
		t.Fatal(err)
	}

	fork, err := Fork(srcID)
	if err != nil {
		t.Fatal(err)
	}
	forkID := fork.ID
	fork.Log("user", "continued on the branch")
	fork.Close()

	if forkID == srcID {
		t.Fatal("fork reused the source id")
	}
	if st, err := os.Stat(filepath.Join(dir, forkID+".sqlite")); err != nil || st.Size() == 0 {
		t.Fatalf("fork store missing: %v", err)
	}

	forkEntries, err := readEntries(forkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(forkEntries) != len(srcEntries)+2 { // + forked_from meta + the new user turn
		t.Fatalf("fork entries = %d, want %d: %+v", len(forkEntries), len(srcEntries)+2, forkEntries)
	}
	for i, want := range srcEntries {
		if forkEntries[i].TS != want.TS || forkEntries[i].Kind != want.Kind {
			t.Fatalf("fork entry %d = %s/%s, want %s/%s", i, forkEntries[i].Kind, forkEntries[i].TS, want.Kind, want.TS)
		}
	}
	marker := forkEntries[len(srcEntries)]
	var meta map[string]string
	if marker.Kind != "meta" || !remarshal(marker.V, &meta) || meta["forked_from"] != srcID {
		t.Fatalf("forked_from meta missing: %+v", marker)
	}

	hist, err := History(forkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 { // user + assistant copied, then the new user turn
		t.Fatalf("fork history = %d: %+v", len(hist), hist)
	}
	info, err := InfoOf(forkID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "p/m" {
		t.Fatalf("fork lost meta: %+v", info)
	}

	// The source is untouched by the fork.
	after, err := readEntries(srcID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(srcEntries) {
		t.Fatalf("source grew to %d entries", len(after))
	}
	var afterExport bytes.Buffer
	if err := ExportJSONL(srcID, &afterExport); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), afterExport.Bytes()) {
		t.Fatal("source event stream changed by fork")
	}
}

func TestInfoListAndSearchOnStore(t *testing.T) {
	dir := withSessionDir(t)

	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	a.Log("meta", map[string]string{"model": "p/a", "persona": "flash", "role": "coder"})
	a.Log("user", "refactor the auth middleware")
	a.Log("assistant", "I will preserve the refusal-proofing sections.")
	aID := a.ID
	a.Close()

	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b.Log("meta", map[string]string{"model": "p/b", "persona": "steady"})
	b.Log("user", "summarise the findings")
	bID := b.ID
	b.Close()

	infos, err := ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("ListInfo = %d sessions: %+v", len(infos), infos)
	}
	byID := map[string]Info{}
	for _, in := range infos {
		byID[in.ID] = in
	}
	if got := byID[aID]; got.Model != "p/a" || got.Persona != "flash" || got.Role != "coder" || got.Turns != 1 {
		t.Fatalf("info a = %+v", got)
	}
	if got := byID[bID]; got.Model != "p/b" || got.Turns != 1 {
		t.Fatalf("info b = %+v", got)
	}

	info, err := InfoOf(aID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(info.Path, aID+".sqlite") {
		t.Fatalf("info path = %q", info.Path)
	}
	if info.Bytes <= 0 {
		t.Fatalf("info bytes = %d", info.Bytes)
	}

	hits, err := Search("AUTH", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != aID || !strings.Contains(hits[0].Snippet, "auth") {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].TS.IsZero() {
		t.Fatalf("hit has no timestamp: %+v", hits[0])
	}
	limited, err := Search("the", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit ignored: %+v", limited)
	}
	if _, err := Search("  ", 10); err == nil {
		t.Fatal("empty query accepted")
	}

	// Nothing leaked a JSONL file for either store.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			t.Fatalf("unexpected JSONL file %s", e.Name())
		}
	}
}

func TestRedactionBeforeWrite(t *testing.T) {
	dir := withSessionDir(t)
	const secret = "AKIAIOSFODNN7EXAMPLE"
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("tool_call", map[string]any{
		"tool": "bash",
		"argv": []string{"env", secret},
		"nested": map[string]any{
			"aws": secret,
			"hdr": "Authorization: Bearer abcdefghijklmnop",
		},
	})
	id := s.ID
	s.Close()

	db := mustOpenRaw(t, filepath.Join(dir, id+".sqlite"))
	var payload string
	if err := db.QueryRow(`SELECT json FROM events WHERE kind = 'tool_call'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, secret) {
		t.Fatalf("secret reached the store: %s", payload)
	}
	if !strings.Contains(payload, "[REDACTED]") {
		t.Fatalf("payload not redacted: %s", payload)
	}

	// The redacted value is what the API surfaces too.
	entries, err := readEntries(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	raw, err := json.Marshal(entries[0].V)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("redacted secret survived read: %s", raw)
	}
}

func TestRepairAndRefusalStatsOverStore(t *testing.T) {
	dir := withSessionDir(t)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("meta", map[string]string{"model": "p/m", "persona": "flash"})
	s.Log("tool_repair", map[string]any{"tool": "read", "classes": []string{"alias-default", "null-optional"}})
	s.Log("tool_repair_failed", map[string]any{"tool": "bash", "error": "bad json"})
	s.Log("refusal", map[string]any{"class": "moralizing", "outcome": "resumed"})
	s.Close()

	repairs, err := RepairStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[RepairStat]int{
		{Model: "p/m", Tool: "read", Class: "alias-default"}: 1,
		{Model: "p/m", Tool: "read", Class: "null-optional"}: 1,
		{Model: "p/m", Tool: "bash", Class: "unrepaired"}:    1,
	}
	if len(repairs) != len(want) {
		t.Fatalf("repairs = %+v, want %d rows", repairs, len(want))
	}
	for _, r := range repairs {
		k := RepairStat{Model: r.Model, Tool: r.Tool, Class: r.Class}
		if want[k] != r.Count {
			t.Errorf("%+v count = %d, want %d", k, r.Count, want[k])
		}
	}

	refusals, err := RefusalStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v", refusals)
	}
	if r := refusals[0]; r.Model != "p/m" || r.Persona != "flash" || r.Class != "moralizing" || r.Outcome != "resumed" || r.Count != 1 {
		t.Fatalf("refusal row = %+v", r)
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func countRows(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := mustOpenRaw(t, path).QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countKind(t *testing.T, path, kind string) int {
	t.Helper()
	var n int
	if err := mustOpenRaw(t, path).QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
