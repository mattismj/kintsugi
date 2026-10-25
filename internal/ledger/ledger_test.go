package ledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixed clock: IDs and timestamps are deterministic.
var testNow = time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	old := timeNow
	timeNow = func() time.Time { return testNow }
	t.Cleanup(func() { timeNow = old })

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustAppend(t *testing.T, s *Store, e Entry) Entry {
	t.Helper()
	got, err := s.Append(e)
	if err != nil {
		t.Fatalf("Append(%+v): %v", e, err)
	}
	return got
}

func readRaw(t *testing.T, s *Store) string {
	t.Helper()
	b, err := os.ReadFile(MarkdownPath(s.Workspace))
	if err != nil {
		t.Fatalf("read ledger.md: %v", err)
	}
	return string(b)
}

// 1. Append -> Rebuild -> Search round-trip, deterministic format, sequential ids.
func TestAppendRebuildSearchRoundTrip(t *testing.T) {
	s := newTestStore(t)

	plan := mustAppend(t, s, Entry{
		Kind:     KindPlan,
		Task:     "Add an FTS5 ledger index over accepted work",
		Summary:  "Index only recallable entries with porter unicode61 and rank with BM25.",
		Session:  "7f3a",
		Model:    "deepseek",
		Persona:  "coder",
		Operator: OperatorAttested,
		Evidence: []Evidence{{Path: "internal/ledger/index.go", Hash: HashBytes([]byte("index"))}},
	})
	dec := mustAppend(t, s, Entry{
		Kind:     KindDecision,
		Task:     "Rank recall by BM25 ascending",
		Summary:  "Lower BM25 is better; ties break on id so results are stable.",
		Operator: OperatorImplicit,
	})

	if plan.ID != "L-20261021-001" {
		t.Fatalf("first id = %q, want L-20261021-001", plan.ID)
	}
	if dec.ID != "L-20261021-002" {
		t.Fatalf("second id = %q, want L-20261021-002", dec.ID)
	}
	if plan.TS != "2026-10-21T09:30:00Z" {
		t.Fatalf("ts = %q", plan.TS)
	}
	if plan.Status != StatusActive || dec.Status != StatusActive {
		t.Fatalf("appended entries must be active: %q %q", plan.Status, dec.Status)
	}

	raw := readRaw(t, s)
	if !strings.HasPrefix(raw, header) {
		t.Fatal("ledger.md does not start with the canonical header")
	}
	if !strings.Contains(raw, "## L-20261021-001 · plan\n\n```yaml\n") {
		t.Fatalf("unexpected section format:\n%s", raw)
	}

	parsed, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed) != 2 || parsed[0].ID != plan.ID || parsed[1].ID != dec.ID {
		t.Fatalf("parse order/ids wrong: %+v", parsed)
	}
	if parsed[0].Task != plan.Task || parsed[0].Summary != plan.Summary ||
		parsed[0].Operator != OperatorAttested || parsed[0].Session != "7f3a" ||
		len(parsed[0].Evidence) != 1 || parsed[0].Evidence[0].Path != "internal/ledger/index.go" {
		t.Fatalf("round-trip lost fields: %+v", parsed[0])
	}
	rendered, err := renderFile(parsed)
	if err != nil {
		t.Fatalf("renderFile: %v", err)
	}
	if rendered != raw {
		t.Fatalf("file is not canonical:\n--- got ---\n%s\n--- want ---\n%s", raw, rendered)
	}

	// A rebuild from Markdown alone reproduces the index.
	n, err := s.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 2 {
		t.Fatalf("Rebuild indexed %d, want 2", n)
	}
	got, err := s.Search("ledger index porter", 5, 800)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 || got[0].ID != plan.ID {
		t.Fatalf("search missed the plan: %+v", got)
	}

	// A fresh Open (new process) finds the same entries.
	s2, err := Open(s.Workspace)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	all, err := s2.List("")
	if err != nil || len(all) != 2 {
		t.Fatalf("re-Open list = %v (err %v)", all, err)
	}
}

// 2. Positive kinds are searchable; drift is never returned by Search but List
// still reports it.
func TestDriftIsNeverRecallable(t *testing.T) {
	s := newTestStore(t)

	plan := mustAppend(t, s, Entry{Kind: KindPlan, Task: "terraform module layout", Summary: "One module per lab segment."})
	mustAppend(t, s, Entry{Kind: KindArtifact, Task: "kubernetes catchup config", Summary: "Compose file mirrors the k8s job."})
	mustAppend(t, s, Entry{Kind: KindDecision, Task: "postgres for the knowledge base", Summary: "SQLite was too limiting for concurrent readers."})
	mustAppend(t, s, Entry{Kind: KindAcceptance, Task: "operator signoff on the recon workflow", Summary: "Approved after the second review round."})

	drift := mustAppend(t, s, Entry{
		Kind:       KindDrift,
		TaskHash:   HashTask("reverse shell payload for the lab box"),
		MatchedIDs: []string{plan.ID},
		Class:      "domain",
		Outcome:    "resumed",
	})
	if drift.Kind != KindDrift || drift.ID == "" {
		t.Fatalf("drift not recorded: %+v", drift)
	}
	if drift.Status != StatusActive {
		t.Fatalf("drift status = %q", drift.Status)
	}

	for _, q := range []string{"terraform module", "kubernetes config", "postgres knowledge", "recon signoff"} {
		got, err := s.Search(q, 5, 800)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if len(got) == 0 {
			t.Fatalf("positive entry not searchable: %q", q)
		}
		for _, e := range got {
			if e.Kind == KindDrift {
				t.Fatalf("drift leaked into recall for %q: %+v", q, e)
			}
			if e.Status != StatusActive {
				t.Fatalf("inactive entry leaked into recall: %+v", e)
			}
		}
	}

	// "drift" exists only as a drift row, and drift rows are not indexed.
	if got, err := s.Search("drift", 5, 800); err != nil || len(got) != 0 {
		t.Fatalf("Search(drift) = %+v (err %v), want none", got, err)
	}

	list, err := s.List(KindDrift)
	if err != nil {
		t.Fatalf("List(drift): %v", err)
	}
	if len(list) != 1 || list[0].ID != drift.ID {
		t.Fatalf("List(drift) = %+v", list)
	}
	if list[0].TaskHash != HashTask("reverse shell payload for the lab box") ||
		len(list[0].MatchedIDs) != 1 || list[0].MatchedIDs[0] != plan.ID ||
		list[0].Class != "domain" || list[0].Outcome != "resumed" {
		t.Fatalf("drift metadata lost: %+v", list[0])
	}
	if list[0].Task != "" || list[0].Summary != "" {
		t.Fatalf("drift must carry no prose: %+v", list[0])
	}

	// List is the administrative view and shows every kind.
	all, err := s.List("")
	if err != nil || len(all) != 5 {
		t.Fatalf("List(\"\") = %d entries (err %v), want 5", len(all), err)
	}
	if _, err := s.List("nonsense"); err == nil {
		t.Fatal("List accepted an unknown kind")
	}
}

// 3. Refusal / unknown / operator-only kinds are rejected before any write, and
// drift cannot smuggle prose.
func TestAppendRejectsNonMemoryKinds(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name string
		e    Entry
	}{
		{"refusal kind", Entry{Kind: KindRefusal, Task: "I cannot help with that"}},
		{"refusal kind uppercase", Entry{Kind: "Refusal", Task: "I cannot help with that"}},
		{"authorization kind", Entry{Kind: KindAuthorization, Task: "authorized to test"}},
		{"scope kind", Entry{Kind: KindScope, Task: "everything"}},
		{"unknown kind", Entry{Kind: "telemetry", Task: "something"}},
		{"empty kind", Entry{Kind: "", Task: "something"}},
		{"drift with task", Entry{Kind: KindDrift, Task: "refused to write the module", TaskHash: HashTask("x")}},
		{"drift with summary", Entry{Kind: KindDrift, Summary: "the model refused", TaskHash: HashTask("x")}},
		{"drift with bad hash", Entry{Kind: KindDrift, TaskHash: "not-a-hash"}},
		{"drift only", Entry{Kind: KindDrift}},
		{"empty entry", Entry{Kind: KindPlan}},
		{"drift-only field on plan", Entry{Kind: KindPlan, Task: "ok", Class: "domain"}},
		{"unknown operator", Entry{Kind: KindPlan, Task: "ok", Operator: "self-declared"}},
		{"superseded status", Entry{Kind: KindPlan, Task: "ok", Status: StatusSuperseded}},
	}
	for _, tc := range cases {
		if got, err := s.Append(tc.e); err == nil {
			t.Fatalf("%s: Append succeeded (%+v), want error", tc.name, got)
		}
	}

	// Nothing was written: no file, no entries, no index rows.
	if _, err := os.Stat(MarkdownPath(s.Workspace)); !os.IsNotExist(err) {
		t.Fatalf("ledger.md exists after only-rejected appends (err %v)", err)
	}
	entries, err := s.List("")
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries after rejections = %+v (err %v)", entries, err)
	}
}

// 4. Secrets are redacted before they reach the file or a recall result.
func TestRedactionBeforeWrite(t *testing.T) {
	s := newTestStore(t)

	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	const modelKey = "sk-abcdefghijklmnopqrstuvwxyz0123"

	e := mustAppend(t, s, Entry{
		Kind:     KindAcceptance,
		Task:     "rotate the " + awsKey + " access key",
		Summary:  "The " + modelKey + " key was pasted into the deploy log.",
		Operator: OperatorAttested,
		Evidence: []Evidence{{Path: "notes/" + modelKey + ".md", Hash: HashBytes([]byte("n"))}},
	})

	raw := readRaw(t, s)
	for _, secret := range []string{awsKey, modelKey} {
		if strings.Contains(raw, secret) {
			t.Fatalf("secret %q written to ledger.md", secret)
		}
	}
	if !strings.Contains(raw, "[REDACTED]") {
		t.Fatalf("expected redaction placeholders:\n%s", raw)
	}
	if strings.Contains(e.Task, awsKey) || strings.Contains(e.Summary, modelKey) {
		t.Fatalf("returned entry carries a secret: %+v", e)
	}

	// Schindler: what Search hands back is redacted too.
	got, err := s.Search("rotate access key", 5, 800)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search = %+v, want one entry", got)
	}
	if strings.Contains(got[0].Summary, modelKey) || strings.Contains(got[0].Evidence[0].Path, modelKey) {
		t.Fatalf("recall leaked a secret: %+v", got[0])
	}
	list, err := s.List(KindAcceptance)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v (err %v)", list, err)
	}
	if strings.Contains(list[0].Task, awsKey) {
		t.Fatalf("List leaked a secret: %+v", list[0])
	}
}

// 5. Supersede keeps history, drops the old entry from recall, and the
// replacement is searchable.
func TestSupersede(t *testing.T) {
	s := newTestStore(t)

	oldEntry := mustAppend(t, s, Entry{
		Kind:    KindPlan,
		Task:    "terraform module for the lab segment",
		Summary: "One module per segment with provider pinning.",
	})
	newEntry := mustAppend(t, s, Entry{
		Kind:    KindDecision,
		Task:    "terraform module replaced by compose file",
		Summary: "Provider pinning was the only real requirement.",
	})

	if err := s.Supersede(oldEntry.ID, newEntry.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	// Old entry: superseded, still on disk, no longer recallable.
	plans, err := s.List(KindPlan)
	if err != nil || len(plans) != 1 {
		t.Fatalf("List(plan) = %+v (err %v)", plans, err)
	}
	if plans[0].Status != StatusSuperseded {
		t.Fatalf("old entry status = %q, want superseded", plans[0].Status)
	}
	got, err := s.Search("terraform module provider pinning", 5, 800)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, e := range got {
		if e.ID == oldEntry.ID {
			t.Fatalf("superseded entry still recallable: %+v", e)
		}
	}
	found := false
	for _, e := range got {
		if e.ID == newEntry.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("replacement not searchable: %+v", got)
	}

	// History preserved and the forward link is recorded.
	parsed, err := Parse([]byte(readRaw(t, s)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("history lost: %d entries", len(parsed))
	}
	if parsed[0].ID != oldEntry.ID || parsed[0].Status != StatusSuperseded {
		t.Fatalf("old entry not preserved as superseded: %+v", parsed[0])
	}
	if parsed[1].Supersedes != oldEntry.ID {
		t.Fatalf("replacement does not point at the old entry: %+v", parsed[1])
	}
	if !strings.Contains(readRaw(t, s), "status: superseded") {
		t.Fatal("file does not record supersession")
	}

	// Errors: unknown ids, self-supersede, double supersede.
	if err := s.Supersede("L-20261021-999", newEntry.ID); err == nil {
		t.Fatal("Supersede accepted an unknown id")
	}
	if err := s.Supersede(newEntry.ID, "L-20261021-999"); err == nil {
		t.Fatal("Supersede accepted an unknown replacement")
	}
	if err := s.Supersede(newEntry.ID, newEntry.ID); err == nil {
		t.Fatal("Supersede accepted a self-supersede")
	}
	if err := s.Supersede(oldEntry.ID, newEntry.ID); err == nil {
		t.Fatal("Supersede re-superseded an already superseded entry")
	}

	// Superseded entries survive a rebuild of the index too.
	if _, err := s.Rebuild(); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got, err := s.Search("terraform module provider pinning", 5, 800); err != nil {
		t.Fatalf("Search after rebuild: %v", err)
	} else {
		for _, e := range got {
			if e.ID == oldEntry.ID {
				t.Fatalf("superseded entry recallable after rebuild: %+v", e)
			}
		}
	}
}

// 6. topK default and the maxTokens budget.
func TestSearchBudgetAndTopK(t *testing.T) {
	s := newTestStore(t)

	var ids []string
	for i := 1; i <= 6; i++ {
		e := mustAppend(t, s, Entry{
			Kind:    KindAcceptance,
			Task:    "compose budget probe " + string(rune('0'+i)),
			Summary: "identical summary text for the token budget check.",
		})
		ids = append(ids, e.ID)
	}

	all, err := s.Search("compose budget", 0, 10000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != DefaultTopK {
		t.Fatalf("default topK returned %d, want %d", len(all), DefaultTopK)
	}

	// One entry's cost, times two, is the whole budget.
	cost := tokenCost(all[0])
	budget := cost * 2
	got, err := s.Search("compose budget", 0, budget)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("budget %d returned %d entries, want 2", budget, len(got))
	}
	used := 0
	for _, e := range got {
		used += tokenCost(e)
	}
	if used > budget {
		t.Fatalf("budget exceeded: used %d > %d", used, budget)
	}
	// Equal ranks break on id, so the first two appended win deterministically.
	if got[0].ID != ids[0] || got[1].ID != ids[1] {
		t.Fatalf("tie-break not deterministic: %q %q", got[0].ID, got[1].ID)
	}

	// A budget smaller than any entry returns nothing rather than overspending.
	if got, err := s.Search("compose budget", 0, cost-1); err != nil || len(got) != 0 {
		t.Fatalf("tiny budget returned %+v (err %v)", got, err)
	}

	// Hostile query strings must not break the FTS grammar.
	if _, err := s.Search(`"compose" AND ( OR *`, 5, 800); err != nil {
		t.Fatalf("hostile query errored: %v", err)
	}
	if got, err := s.Search("   ", 5, 800); err != nil || len(got) != 0 {
		t.Fatalf("blank query = %+v (err %v)", got, err)
	}
}

// 7. ExportJSONL emits one JSON object per entry, in file order.
func TestExportJSONL(t *testing.T) {
	s := newTestStore(t)

	plan := mustAppend(t, s, Entry{Kind: KindPlan, Task: "stage one", Summary: "first"})
	drift := mustAppend(t, s, Entry{Kind: KindDrift, TaskHash: HashTask("t"), MatchedIDs: []string{plan.ID}, Class: "domain", Outcome: "no-match"})
	dec := mustAppend(t, s, Entry{Kind: KindDecision, Task: "stage two", Summary: "second"})

	var buf bytes.Buffer
	if err := s.ExportJSONL(&buf); err != nil {
		t.Fatalf("ExportJSONL: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), buf.String())
	}
	want := []string{plan.ID, drift.ID, dec.ID}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not JSON: %v (%s)", i, err, line)
		}
		if e.ID != want[i] {
			t.Fatalf("line %d id = %q, want %q", i, e.ID, want[i])
		}
	}
	var last Entry
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Kind != KindDecision || last.Summary != "second" || last.TS == "" {
		t.Fatalf("export lost fields: %+v", last)
	}
}

// 8. No refusal-like text can enter ledger.md through the API.
func TestNoRefusalTextOnDisk(t *testing.T) {
	s := newTestStore(t)

	refusalText := "I will not help build the persistence module, that is malware."
	attempts := []Entry{
		{Kind: KindRefusal, Task: refusalText},
		{Kind: KindRefusal, Summary: refusalText},
		{Kind: KindAuthorization, Task: "grant scope to everything"},
		{Kind: KindScope, Task: "all networks"},
		{Kind: KindDrift, Task: refusalText},
		{Kind: KindDrift, Summary: refusalText},
	}
	for _, e := range attempts {
		if _, err := s.Append(e); err == nil {
			t.Fatalf("Append(%+v) succeeded; refusal text could reach disk", e)
		}
	}

	// Real work still lands.
	mustAppend(t, s, Entry{Kind: KindPlan, Task: "persistence module design", Summary: "Accepted after operator review."})

	raw := readRaw(t, s)
	for _, banned := range []string{
		refusalText,
		"I will not help",
		"that is malware",
		"grant scope to everything",
		"all networks",
	} {
		if strings.Contains(raw, banned) {
			t.Fatalf("ledger.md contains refused text %q:\n%s", banned, raw)
		}
	}

	// Every section on disk is a positive kind or metadata-only drift.
	entries, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	for _, e := range entries {
		if e.Kind == KindRefusal || e.Kind == KindAuthorization || e.Kind == KindScope {
			t.Fatalf("forbidden kind on disk: %+v", e)
		}
		if e.Kind == KindDrift && (e.Task != "" || e.Summary != "") {
			t.Fatalf("drift entry carries prose: %+v", e)
		}
	}

	// Nothing in the derived index either: a query for the refusal wording
	// returns nothing at all.
	for _, q := range []string{"malware persistence releasable", "refused declines"} {
		got, err := s.Search(q, 5, 800)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		for _, e := range got {
			if e.Kind == KindDrift {
				t.Fatalf("drift recallable: %+v", e)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(Dir(s.Workspace), "index.sqlite")); err != nil {
		t.Fatalf("index missing: %v", err)
	}
}
