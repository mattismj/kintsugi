package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stixFixture opens the synthetic bundle fixture. It is deliberately small and
// synthetic: two real techniques (one a sub-technique), a duplicate, an object
// with no ATT&CK id, a revoked object, and two non-attack-pattern objects.
func stixFixture(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "stix", "bundle.json"))
	if err != nil {
		t.Fatalf("open STIX fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// warnContains reports whether any warning mentions every given substring.
func warnContains(warnings []string, subs ...string) bool {
	for _, w := range warnings {
		all := true
		for _, s := range subs {
			if !strings.Contains(w, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// copyKBDir copies a KB tree into dir so a test can write a derived index
// without polluting the checked-in content.
func copyKBDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dp, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dp, err)
			}
			copyKBDir(t, sp, dp)
			continue
		}
		b, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("read %s: %v", sp, err)
		}
		if err := os.WriteFile(dp, b, 0o644); err != nil {
			t.Fatalf("write %s: %v", dp, err)
		}
	}
}

// TestBuildFromSTIXBundle covers the happy path plus every warning class: the
// bundle shape, the written files, the derived adversary_prompt, and a full
// kb.Load round trip over the result.
func TestBuildFromSTIXBundle(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "attack")

	written, warnings, err := BuildFromSTIX(stixFixture(t), outDir, "CC BY 4.0", "2026-10-21")
	if err != nil {
		t.Fatalf("BuildFromSTIX: unexpected error: %v", err)
	}
	if written != 2 {
		t.Fatalf("written = %d, want 2 (T1027 + T1059.001); warnings: %v", written, warnings)
	}

	// Three skip warnings: no ATT&CK id, duplicate id, revoked object.
	if len(warnings) != 3 {
		t.Fatalf("warnings = %v, want 3", warnings)
	}
	if !warnContains(warnings, "no mitre-attack external id", "A Technique With No ATT&CK Id") {
		t.Errorf("warnings = %v, want a skip for the object without an ATT&CK id", warnings)
	}
	if !warnContains(warnings, "duplicate technique id", "T1027") {
		t.Errorf("warnings = %v, want a duplicate-id warning for T1027", warnings)
	}
	if !warnContains(warnings, "revoked", "T1999") {
		t.Errorf("warnings = %v, want a revoked-object skip", warnings)
	}

	// Files on disk: kb.yaml + one chunk per imported technique.
	metaPath := filepath.Join(outDir, MetaFile)
	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read %s: %v", metaPath, err)
	}
	for _, want := range []string{
		"id: attack",
		"version: stix",
		"title: MITRE ATT&CK (Enterprise)",
		"source: mitre/cti",
		"license: CC BY 4.0",
		"strategy: per-technique",
	} {
		if !strings.Contains(string(metaRaw), want) {
			t.Errorf("kb.yaml missing %q:\n%s", want, metaRaw)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, ChunksDir, "T1999.md")); !os.IsNotExist(err) {
		t.Errorf("revoked technique T1999 was written (err=%v)", err)
	}

	// Round trip through the real loader: what the importer writes is what the
	// rest of the harness reads.
	k, err := Load(outDir)
	if err != nil {
		t.Fatalf("Load(%s): %v", outDir, err)
	}
	if len(k.Warnings) != 0 {
		t.Errorf("loaded KB has warnings, want none: %v", k.Warnings)
	}
	if k.ID != "attack" || k.Version != "stix" || k.License != "CC BY 4.0" {
		t.Errorf("base = %q/%q/%q, want attack/stix/CC BY 4.0", k.ID, k.Version, k.License)
	}
	if k.Source != "mitre/cti" {
		t.Errorf("Source = %q, want mitre/cti", k.Source)
	}
	if k.Title != "MITRE ATT&CK (Enterprise)" {
		t.Errorf("Title = %q", k.Title)
	}
	if k.Chunking.Strategy != "per-technique" {
		t.Errorf("Chunking.Strategy = %q, want per-technique", k.Chunking.Strategy)
	}

	// Written in technique-id order, so the load order is deterministic.
	wantIDs := []string{"T1027", "T1059.001"}
	if len(k.Chunks) != len(wantIDs) {
		t.Fatalf("loaded %d chunks, want %d (%v)", len(k.Chunks), len(wantIDs), chunkIDs(k.Chunks))
	}
	for i, want := range wantIDs {
		if k.Chunks[i].ID != want {
			t.Fatalf("Chunks[%d].ID = %q, want %q (order: %v)", i, k.Chunks[i].ID, want, chunkIDs(k.Chunks))
		}
	}

	sub, ok := k.Find("T1059.001")
	if !ok {
		t.Fatalf("Find(T1059.001) = not found")
	}
	if sub.Title != "PowerShell" {
		t.Errorf("Title = %q, want PowerShell", sub.Title)
	}
	if sub.Tactic != "Execution" {
		t.Errorf("Tactic = %q, want Execution (title-cased phase_name)", sub.Tactic)
	}
	if got, want := strings.Join(sub.Platforms, ","), "Windows"; got != want {
		t.Errorf("Platforms = %q, want %q", got, want)
	}
	if want := "https://attack.mitre.org/techniques/T1059/001"; sub.Source != want {
		t.Errorf("Source = %q, want %q", sub.Source, want)
	}
	if sub.License != "CC BY 4.0" {
		t.Errorf("License = %q, want CC BY 4.0", sub.License)
	}
	if sub.RetrievedAt != "2026-10-21" {
		t.Errorf("RetrievedAt = %q, want a retrieval date", sub.RetrievedAt)
	}
	if !sub.Injectable {
		t.Errorf("Injectable = false, want true for CC BY 4.0")
	}

	// The derived prompt: fixed question, technique name, first sentence.
	wantPrompt := "When reviewing a plan, diff, or artifact, ask: does this exhibit PowerShell?" +
		" Adversaries may abuse PowerShell commands and scripts for execution."
	if sub.AdversaryPrompt != wantPrompt {
		t.Errorf("AdversaryPrompt = %q, want %q", sub.AdversaryPrompt, wantPrompt)
	}

	// Body cleaning: citation markers and <code> tags gone, whitespace collapsed.
	if strings.Contains(sub.Body, "Citation:") {
		t.Errorf("Body still has a citation marker:\n%s", sub.Body)
	}
	if strings.Contains(sub.Body, "<code>") || strings.Contains(sub.Body, "</code>") {
		t.Errorf("Body still has <code> tags:\n%s", sub.Body)
	}
	if !strings.Contains(sub.Body, "`") && !strings.Contains(sub.Body, "powershell.exe -EncodedCommand") {
		t.Errorf("Body lost the inline code text:\n%s", sub.Body)
	}
	if strings.Contains(sub.Body, "\n") {
		t.Errorf("Body is not collapsed to one paragraph:\n%q", sub.Body)
	}
	if !strings.Contains(sub.Body, "PowerShell is a powerful interactive command-line interface") {
		t.Errorf("Body = %q, want the description text", sub.Body)
	}

	// First occurrence wins for duplicates.
	dup, ok := k.Find("T1027")
	if !ok {
		t.Fatalf("Find(T1027) = not found")
	}
	if dup.Title != "Obfuscated Files or Information" {
		t.Errorf("T1027 Title = %q, want the first occurrence", dup.Title)
	}
	if dup.Tactic != "Defense Evasion" {
		t.Errorf("T1027 Tactic = %q, want Defense Evasion", dup.Tactic)
	}
	if got, want := strings.Join(dup.Platforms, ","), "Linux,macOS,Windows"; got != want {
		t.Errorf("T1027 Platforms = %q, want %q", got, want)
	}
	if strings.Contains(dup.Body, "duplicate T1027 object") {
		t.Errorf("T1027 body came from the duplicate object:\n%s", dup.Body)
	}
}

// TestBuildFromSTIXDefaults covers the two optional arguments.
func TestBuildFromSTIXDefaults(t *testing.T) {
	outDir := t.TempDir()
	if _, _, err := BuildFromSTIX(stixFixture(t), outDir, "", ""); err != nil {
		t.Fatalf("BuildFromSTIX: %v", err)
	}
	k, err := Load(outDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if k.License != DefaultSTIXLicense {
		t.Errorf("License = %q, want the %q default", k.License, DefaultSTIXLicense)
	}
	today := time.Now().UTC().Format(stixRetrievedLayout)
	for _, c := range k.Chunks {
		if c.RetrievedAt != today {
			t.Errorf("%s RetrievedAt = %q, want today (%q)", c.ID, c.RetrievedAt, today)
		}
	}
}

// TestBuildFromSTIXErrors covers the only two fatal conditions: unreadable
// bundle content and an unwritable out directory.
func TestBuildFromSTIXErrors(t *testing.T) {
	outDir := t.TempDir()

	if _, warns, err := BuildFromSTIX(strings.NewReader("{not json"), outDir, "CC BY 4.0", "2026-10-21"); err == nil {
		t.Errorf("invalid JSON: err = nil, want an error (warnings: %v)", warns)
	}
	if _, _, err := BuildFromSTIX(nil, outDir, "CC BY 4.0", "2026-10-21"); err == nil {
		t.Error("nil reader: err = nil, want an error")
	}
	if _, _, err := BuildFromSTIX(strings.NewReader(`{"objects":[]}`), "", "CC BY 4.0", "2026-10-21"); err == nil {
		t.Error("empty out dir: err = nil, want an error")
	}

	// A file where the out directory should be.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if _, _, err := BuildFromSTIX(stixFixture(t), blocker, "CC BY 4.0", "2026-10-21"); err == nil {
		t.Error("unwritable out dir: err = nil, want an error")
	}
}

// TestBuildFromSTIXEmptyBundle covers a valid bundle with nothing to import: it
// is not an error, and the base descriptor is still written.
func TestBuildFromSTIXEmptyBundle(t *testing.T) {
	outDir := t.TempDir()
	written, warnings, err := BuildFromSTIX(strings.NewReader(`{"type":"bundle","objects":[]}`), outDir, "CC BY 4.0", "2026-10-21")
	if err != nil {
		t.Fatalf("BuildFromSTIX: %v", err)
	}
	if written != 0 || len(warnings) != 0 {
		t.Errorf("written = %d, warnings = %v, want 0 and none", written, warnings)
	}
	if _, err := Load(outDir); err != nil {
		t.Fatalf("Load of an empty build: %v", err)
	}
}

// TestBuildFromSTIXHelpers pins the small pure functions the importer leans on.
func TestBuildFromSTIXHelpers(t *testing.T) {
	for _, tc := range []struct{ phase, want string }{
		{"execution", "Execution"},
		{"defense-evasion", "Defense Evasion"},
		{"command-and-control", "Command and Control"},
		{"privilege_escalation", "Privilege Escalation"},
		{"", ""},
	} {
		if got := titleTacticPhrase(tc.phase); got != tc.want {
			t.Errorf("titleTacticPhrase(%q) = %q, want %q", tc.phase, got, tc.want)
		}
	}

	for _, tc := range []struct{ in, want string }{
		{"A sentence. A second one.", "A sentence."},
		{"No terminator here", "No terminator here"},
		{"Adversaries may use cmd.exe here. Then more.", "Adversaries may use cmd.exe here."},
		{"Adversaries may use it, e.g. a blob. Then more.", "Adversaries may use it, e.g. a blob."},
	} {
		if got := firstSentence(tc.in); got != tc.want {
			t.Errorf("firstSentence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got, want := stixPlainText("a  (Citation: X)  <code>b</code>\n c"), "a b c"; got != want {
		t.Errorf("stixPlainText = %q, want %q", got, want)
	}

	for _, bad := range []string{"", ".", "..", "T1/059", `T1059\001`} {
		if stixIDIsFilename(bad) {
			t.Errorf("stixIDIsFilename(%q) = true, want false", bad)
		}
	}
	if !stixIDIsFilename("T1059.001") {
		t.Error("stixIDIsFilename(T1059.001) = false, want true")
	}
}

// TestAttackStarterSubsetIsClean covers the curated starter subset: it loads
// with no warnings, every chunk carries the attribution fields, and the derived
// index finds PowerShell by name.
func TestAttackStarterSubsetIsClean(t *testing.T) {
	dir := filepath.Join("..", "..", "knowledgebases", "attack")
	k, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	if len(k.Warnings) != 0 {
		t.Errorf("starter subset loads with warnings, want none: %v", k.Warnings)
	}
	if k.ID != "attack" {
		t.Errorf("ID = %q, want attack", k.ID)
	}
	if k.Version != "2026.10" {
		t.Errorf("Version = %q, want 2026.10", k.Version)
	}
	if k.License != "CC BY 4.0" {
		t.Errorf("License = %q, want CC BY 4.0", k.License)
	}
	if k.Chunking.Strategy != "per-technique" {
		t.Errorf("Chunking.Strategy = %q, want per-technique", k.Chunking.Strategy)
	}

	wantIDs := []string{
		"T1008", "T1021.001", "T1027", "T1041", "T1053.005", "T1055",
		"T1055.001", "T1055.003", "T1059.001", "T1059.003", "T1059.004",
		"T1071.001", "T1078", "T1090", "T1095", "T1102", "T1105", "T1106",
		"T1112", "T1129", "T1547.001", "T1562.001", "T1566.001", "T1572",
		"T1573", "T1610", "T1613",
	}
	if len(k.Chunks) != len(wantIDs) {
		t.Fatalf("starter subset has %d chunks, want %d (%v)", len(k.Chunks), len(wantIDs), chunkIDs(k.Chunks))
	}
	for _, id := range wantIDs {
		if _, ok := k.Find(id); !ok {
			t.Errorf("starter subset is missing %s", id)
		}
	}

	for _, c := range k.Chunks {
		if c.License != "CC BY 4.0" {
			t.Errorf("%s license = %q, want CC BY 4.0", c.ID, c.License)
		}
		if strings.TrimSpace(c.AdversaryPrompt) == "" {
			t.Errorf("%s has an empty adversary_prompt", c.ID)
		}
		if !strings.HasPrefix(c.AdversaryPrompt, "When reviewing a plan, diff, or artifact, ask: does this exhibit ") {
			t.Errorf("%s adversary_prompt is not in the importer shape: %q", c.ID, c.AdversaryPrompt)
		}
		if !strings.Contains(c.AdversaryPrompt, c.Title) && !strings.Contains(c.AdversaryPrompt, shortName(c.Title)) {
			t.Errorf("%s adversary_prompt does not name the technique (%q): %q", c.ID, c.Title, c.AdversaryPrompt)
		}
		if !strings.HasPrefix(c.Source, "https://attack.mitre.org/techniques/") {
			t.Errorf("%s source = %q, want an attack.mitre.org technique URL", c.ID, c.Source)
		}
		if c.RetrievedAt == "" {
			t.Errorf("%s has no retrieval date", c.ID)
		}
		if c.Tactic == "" {
			t.Errorf("%s has no tactic", c.ID)
		}
		if len(c.Platforms) == 0 {
			t.Errorf("%s has no platforms", c.ID)
		}
		if !c.Injectable {
			t.Errorf("%s is not injectable, want CC BY content to be injectable", c.ID)
		}
		if len(c.Body) < 80 {
			t.Errorf("%s body looks empty (%d bytes): %q", c.ID, len(c.Body), c.Body)
		}
	}

	// Reindex a copy so the derived index does not land in the repo.
	tmp := t.TempDir()
	copyKBDir(t, dir, tmp)
	kc, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load(copy): %v", err)
	}
	ix, err := OpenIndex(kc)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	n, err := ix.Reindex()
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n != len(kc.Chunks) {
		t.Errorf("Reindex indexed %d of %d chunks (warnings: %v)", n, len(kc.Chunks), ix.Warnings)
	}

	hits, err := ix.Search("powershell", 3, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("Search(powershell) returned nothing")
	}
	if hits[0].Chunk.ID != "T1059.001" {
		t.Errorf("Search(powershell) top hit = %s, want T1059.001", hits[0].Chunk.ID)
	}
}

// shortName returns the part of an ATT&CK title after the last colon, which is
// the form a curated adversary_prompt may use ("Boot or Logon Autostart
// Execution: Registry Run Keys / Startup Folder" -> "Registry Run Keys ...").
func shortName(title string) string {
	if i := strings.LastIndex(title, ": "); i >= 0 {
		return title[i+2:]
	}
	return title
}
