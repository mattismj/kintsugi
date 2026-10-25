package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) *KB {
	t.Helper()
	k, err := Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Load(testdata/%s): unexpected error: %v", name, err)
	}
	if k == nil {
		t.Fatalf("Load(testdata/%s): nil KB with nil error", name)
	}
	return k
}

// hasWarning reports whether any warning mentions every given substring.
func hasWarning(k *KB, subs ...string) bool {
	for _, w := range k.Warnings {
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

func TestLoadRoundTrip(t *testing.T) {
	k := loadFixture(t, "basic")

	if k.ID != "attack" {
		t.Errorf("ID = %q, want %q", k.ID, "attack")
	}
	if k.Version != "15.1" {
		t.Errorf("Version = %q, want %q", k.Version, "15.1")
	}
	if want := "MITRE ATT&CK — curated execution subset"; k.Title != want {
		t.Errorf("Title = %q, want %q", k.Title, want)
	}
	if want := "MITRE ATT&CK (https://attack.mitre.org)"; k.Source != want {
		t.Errorf("Source = %q, want %q", k.Source, want)
	}
	if k.License != "CC BY 4.0" {
		t.Errorf("License = %q, want %q", k.License, "CC BY 4.0")
	}
	if k.Chunking.Strategy != "per-technique" {
		t.Errorf("Chunking.Strategy = %q, want %q", k.Chunking.Strategy, "per-technique")
	}
	if got, want := strings.Join(k.Chunking.Fields, ","), "name,description,detection,platforms"; got != want {
		t.Errorf("Chunking.Fields = %q, want %q", got, want)
	}
	if want := ".kintsugi/kb/attack.sqlite"; k.Index != want {
		t.Errorf("Index = %q, want %q", k.Index, want)
	}
	if len(k.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", k.Warnings)
	}

	// Load order is filename order, so this is deterministic.
	wantIDs := []string{"T1027", "T1055", "T1059.001"}
	if len(k.Chunks) != len(wantIDs) {
		t.Fatalf("len(Chunks) = %d, want %d (%v)", len(k.Chunks), len(wantIDs), chunkIDs(k.Chunks))
	}
	for i, want := range wantIDs {
		if k.Chunks[i].ID != want {
			t.Fatalf("Chunks[%d].ID = %q, want %q (order: %v)", i, k.Chunks[i].ID, want, chunkIDs(k.Chunks))
		}
	}

	full, ok := k.Find("T1059.001")
	if !ok {
		t.Fatalf("Find(T1059.001) = not found")
	}
	if want := "Command and Scripting Interpreter: PowerShell"; full.Title != want {
		t.Errorf("Title = %q, want %q", full.Title, want)
	}
	if full.Tactic != "execution" {
		t.Errorf("Tactic = %q, want %q", full.Tactic, "execution")
	}
	if got, want := strings.Join(full.Platforms, ","), "Windows"; got != want {
		t.Errorf("Platforms = %q, want %q", got, want)
	}
	if full.Source != "MITRE ATT&CK T1059.001" {
		t.Errorf("Source = %q", full.Source)
	}
	if full.License != "CC BY 4.0" {
		t.Errorf("License = %q", full.License)
	}
	if full.RetrievedAt != "2026-10-20" {
		t.Errorf("RetrievedAt = %q, want %q", full.RetrievedAt, "2026-10-20")
	}
	if !strings.Contains(full.AdversaryPrompt, "powershell.exe -EncodedCommand") {
		t.Errorf("AdversaryPrompt = %q, want it to mention the encoded PowerShell shape", full.AdversaryPrompt)
	}
	if !full.Injectable {
		t.Errorf("Injectable = false, want true for CC BY 4.0")
	}
	if !strings.HasSuffix(filepath.ToSlash(full.Path), "testdata/basic/chunks/T1059.001.md") {
		t.Errorf("Path = %q, want the chunk file", full.Path)
	}

	// Body is everything after the closing delimiter, trimmed.
	if strings.Contains(full.Body, "adversary_prompt:") || strings.HasPrefix(full.Body, "---") {
		t.Errorf("Body still contains frontmatter:\n%s", full.Body)
	}
	if !strings.HasPrefix(full.Body, "PowerShell is a Windows interactive command shell") {
		t.Errorf("Body = %q, want the Markdown after the frontmatter", full.Body)
	}
	if !strings.HasSuffix(full.Body, "whether the host is a lab fixture.") {
		t.Errorf("Body is not trimmed at the end: %q", full.Body)
	}
	if strings.Contains(full.Body, "\n\n\n") {
		t.Errorf("Body should be trimmed, got %q", full.Body)
	}

	// Multi-platform lists and a chunk with no retrieved_at/adversary_prompt.
	multi, ok := k.Find("T1027")
	if !ok {
		t.Fatalf("Find(T1027) = not found")
	}
	if got, want := strings.Join(multi.Platforms, ","), "Windows,Linux,macOS"; got != want {
		t.Errorf("T1027 Platforms = %q, want %q", got, want)
	}
	if multi.RetrievedAt != "2026-10-20" {
		t.Errorf("T1027 RetrievedAt = %q", multi.RetrievedAt)
	}
	if !strings.HasPrefix(multi.AdversaryPrompt, "A plan step that XORs a payload") {
		t.Errorf("T1027 AdversaryPrompt = %q", multi.AdversaryPrompt)
	}

	min, ok := k.Find("T1055")
	if !ok {
		t.Fatalf("Find(T1055) = not found")
	}
	if min.RetrievedAt != "" || min.AdversaryPrompt != "" {
		t.Errorf("T1055 optional fields = (%q, %q), want empty", min.RetrievedAt, min.AdversaryPrompt)
	}
	if !strings.Contains(min.Body, "inject code into processes") {
		t.Errorf("T1055 Body = %q", min.Body)
	}

	if got := len(k.InjectionCandidates()); got != len(wantIDs) {
		t.Errorf("len(InjectionCandidates()) = %d, want %d (CC BY 4.0 allows injection)", got, len(wantIDs))
	}
}

func TestLicensePolicy(t *testing.T) {
	k := loadFixture(t, "license")
	if len(k.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", k.Warnings)
	}

	// All three chunks are loaded, including the un-injectable ones.
	if len(k.Chunks) != 3 {
		t.Fatalf("len(Chunks) = %d, want 3 (%v)", len(k.Chunks), chunkIDs(k.Chunks))
	}
	wantInjectable := map[string]bool{
		"CC-1":   true,
		"PROP-1": false,
		"NONE-1": false,
	}
	if len(k.Chunks) != len(wantInjectable) {
		t.Fatalf("chunks = %v, want %d", chunkIDs(k.Chunks), len(wantInjectable))
	}
	for _, c := range k.Chunks {
		want, ok := wantInjectable[c.ID]
		if !ok {
			t.Fatalf("unexpected chunk id %q", c.ID)
		}
		if c.Injectable != want {
			t.Errorf("chunk %s (license %q): Injectable = %v, want %v", c.ID, c.License, c.Injectable, want)
		}
		// Excluded chunks stay readable: bodies are loaded either way.
		if c.Body == "" {
			t.Errorf("chunk %s has empty body", c.ID)
		}
	}

	cands := k.InjectionCandidates()
	if len(cands) != 1 {
		t.Fatalf("InjectionCandidates() = %v, want only CC-1", chunkIDs(cands))
	}
	if cands[0].ID != "CC-1" {
		t.Errorf("InjectionCandidates()[0].ID = %q, want CC-1", cands[0].ID)
	}
	if cands[0].License != "CC BY 4.0" {
		t.Errorf("candidate license = %q", cands[0].License)
	}

	// The excluded chunks are still addressable by id.
	for _, id := range []string{"PROP-1", "NONE-1"} {
		c, ok := k.Find(id)
		if !ok {
			t.Fatalf("Find(%s) = not found; excluded chunks must still be loaded", id)
		}
		if c.Injectable {
			t.Errorf("%s: Injectable = true, want false", id)
		}
	}
}

func TestLicenseAllowsInjection(t *testing.T) {
	cases := []struct {
		license string
		want    bool
	}{
		{"CC BY 4.0", true},
		{"cc by-sa 4.0", true},
		{"MIT", true},
		{"Apache-2.0", true},
		{"", true},
		{"Public Domain", true},
		{"Proprietary", false},
		{"Proprietary — internal use only", false},
		{"PROPRIETARY-vendor", false},
		{"no-redistribution", false},
		{"No-Redistribution", false},
		{"No Redistribution", false},
		{"CC BY-ND 4.0 (no-redistribution)", false},
		{"non-distributable", false},
		{"non_distributable", false},
		{"Non Distributable", false},
		{"none", false},
		{"NONE", false},
		{" none. ", false},
	}
	for _, c := range cases {
		if got := LicenseAllowsInjection(c.license); got != c.want {
			t.Errorf("LicenseAllowsInjection(%q) = %v, want %v", c.license, got, c.want)
		}
	}
}

func TestMalformedChunksSkipped(t *testing.T) {
	k := loadFixture(t, "malformed")

	// The good chunk still loads; nothing else does.
	if len(k.Chunks) != 1 {
		t.Fatalf("len(Chunks) = %d, want 1 (%v)", len(k.Chunks), chunkIDs(k.Chunks))
	}
	if k.Chunks[0].ID != "T1059.001" {
		t.Errorf("Chunks[0].ID = %q, want T1059.001", k.Chunks[0].ID)
	}
	if !strings.Contains(k.Chunks[0].Body, "This chunk is fine") {
		t.Errorf("good chunk body = %q", k.Chunks[0].Body)
	}
	if k.ID != "malformed-fixture" {
		t.Errorf("KB metadata lost: ID = %q", k.ID)
	}

	// Each bad chunk is reported by path, with a reason.
	checks := []struct {
		name string
		subs []string
	}{
		{"bad frontmatter", []string{"bad-frontmatter.md", "bad frontmatter"}},
		{"missing frontmatter", []string{"no-frontmatter.md", "missing YAML frontmatter"}},
		{"missing required fields", []string{"partial.md", "missing required frontmatter field(s): source, license"}},
		{"duplicate id", []string{"z-duplicate.md", "duplicate chunk id", "T1059.001"}},
	}
	for _, c := range checks {
		if !hasWarning(k, c.subs...) {
			t.Errorf("%s: no warning containing %v; warnings = %v", c.name, c.subs, k.Warnings)
		}
	}
	if len(k.Warnings) != len(checks) {
		t.Errorf("len(Warnings) = %d, want %d: %v", len(k.Warnings), len(checks), k.Warnings)
	}
}

func TestMissingOptionalFrontmatter(t *testing.T) {
	k := loadFixture(t, "minimal")
	if len(k.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", k.Warnings)
	}
	if len(k.Chunks) != 1 {
		t.Fatalf("len(Chunks) = %d, want 1", len(k.Chunks))
	}
	c := k.Chunks[0]
	if c.ID != "T1000" || c.Title != "Only the required fields" {
		t.Errorf("chunk identity = (%q, %q)", c.ID, c.Title)
	}
	if c.Source != "MITRE ATT&CK T1000" || c.License != "CC BY 4.0" {
		t.Errorf("chunk provenance = (%q, %q)", c.Source, c.License)
	}
	if c.Tactic != "" || len(c.Platforms) != 0 || c.RetrievedAt != "" || c.AdversaryPrompt != "" {
		t.Errorf("optional fields = (%q, %v, %q, %q), want all empty", c.Tactic, c.Platforms, c.RetrievedAt, c.AdversaryPrompt)
	}
	if !c.Injectable {
		t.Errorf("Injectable = false, want true")
	}
	if got, want := c.Body, "A chunk with no optional frontmatter at all."; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
	// Version given as a YAML number is tolerated.
	if k.Version != "2" {
		t.Errorf("Version = %q, want %q", k.Version, "2")
	}
}

func TestLoadRejectsBadMeta(t *testing.T) {
	t.Run("missing required field", func(t *testing.T) {
		k, err := Load(filepath.Join("testdata", "badmeta"))
		if err == nil {
			t.Fatalf("Load succeeded (%v), want an error for a kb.yaml without id", k)
		}
		if k != nil {
			t.Errorf("Load returned a KB alongside the error: %v", k)
		}
		if !strings.Contains(err.Error(), "missing required field(s): id") {
			t.Errorf("err = %v, want it to name the missing field(s)", err)
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		_, err := Load(filepath.Join("testdata", "badmeta-yaml"))
		if err == nil {
			t.Fatalf("Load succeeded, want an error for invalid kb.yaml YAML")
		}
		if !strings.Contains(err.Error(), "invalid YAML") {
			t.Errorf("err = %v, want it to mention invalid YAML", err)
		}
	})

	t.Run("missing meta file", func(t *testing.T) {
		_, err := Load(t.TempDir())
		if err == nil {
			t.Fatalf("Load succeeded, want an error for a directory without kb.yaml")
		}
		if !strings.Contains(err.Error(), MetaFile) {
			t.Errorf("err = %v, want it to name %s", err, MetaFile)
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatalf("Load succeeded, want an error for a missing directory")
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "kb.yaml")
		if err := os.WriteFile(file, []byte("id: x\nversion: 1\ntitle: t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(file); err == nil {
			t.Fatalf("Load succeeded, want an error when dir is a file")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := Load("  "); err == nil {
			t.Fatalf("Load succeeded, want an error for an empty path")
		}
	})
}

func TestMissingChunksDirIsTolerated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MetaFile), []byte("id: empty\nversion: \"1\"\ntitle: Empty KB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	k, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if len(k.Chunks) != 0 {
		t.Errorf("Chunks = %v, want none", chunkIDs(k.Chunks))
	}
	if !hasWarning(k, ChunksDir) {
		t.Errorf("Warnings = %v, want one naming the missing %s/ directory", k.Warnings, ChunksDir)
	}
	if cands := k.InjectionCandidates(); len(cands) != 0 {
		t.Errorf("InjectionCandidates() = %v, want none", chunkIDs(cands))
	}
}

func TestNilKBSafety(t *testing.T) {
	var k *KB
	if got := k.InjectionCandidates(); got != nil {
		t.Errorf("nil KB InjectionCandidates() = %v, want nil", got)
	}
	if _, ok := k.Find("T1059"); ok {
		t.Errorf("nil KB Find() = found, want not found")
	}
}

func chunkIDs(list []Chunk) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}
