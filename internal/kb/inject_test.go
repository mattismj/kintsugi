package kb

import (
	"path/filepath"
	"strings"
	"testing"
)

// renderFixture loads testdata/basic: the attack base with three injectable
// chunks in filename order (T1027, T1055, T1059.001).
func renderFixture(t *testing.T) *KB {
	t.Helper()
	k, err := Load(filepath.Join("testdata", "basic"))
	if err != nil {
		t.Fatalf("Load(testdata/basic): %v", err)
	}
	return k
}

// The tier-3 block format is fixed (DESIGN §3.1 layer 3): header with title and
// version, provenance comment, chunk title, body, source+license line.
func TestRenderBlockFormat(t *testing.T) {
	got := RenderBlock("attack", "MITRE ATT&CK — curated execution subset", "15.1", Chunk{
		ID:      "T1059.001",
		Title:   "Command and Scripting Interpreter: PowerShell",
		Body:    "PowerShell is a Windows shell.\n\n## Detection\n\nModule logging.",
		Source:  "MITRE ATT&CK T1059.001",
		License: "CC BY 4.0",
	})
	want := "## Knowledge: MITRE ATT&CK — curated execution subset v15.1\n" +
		"<!-- kb:attack chunk:T1059.001 -->\n" +
		"### Command and Scripting Interpreter: PowerShell\n" +
		"PowerShell is a Windows shell.\n\n## Detection\n\nModule logging.\n" +
		"\nSource: MITRE ATT&CK T1059.001 (CC BY 4.0)\n"
	if got != want {
		t.Fatalf("RenderBlock:\n%s\nwant:\n%s", got, want)
	}

	// A missing version drops the " v..." suffix rather than printing "v".
	if h := strings.SplitN(RenderBlock("x", "Base", "", Chunk{ID: "c", Title: "t"}), "\n", 2)[0]; h != "## Knowledge: Base" {
		t.Fatalf("versionless header = %q", h)
	}
	// A base without a title falls back to its id.
	if !strings.HasPrefix(RenderBlock("bare-id", "", "1", Chunk{ID: "c", Title: "t"}), "## Knowledge: bare-id v1\n") {
		t.Fatal("titleless base did not fall back to its id")
	}
}

// Every injected chunk gets its own self-describing block, blocks separated by a
// blank line, and a non-injectable chunk is never rendered.
func TestRenderChunksAndSkipsUnlicensed(t *testing.T) {
	k := renderFixture(t)
	chunks := k.InjectionCandidates()
	if len(chunks) != 3 {
		t.Fatalf("fixture injectable chunks = %d, want 3", len(chunks))
	}

	text := Render(k, chunks)
	if n := strings.Count(text, "## Knowledge: "); n != 3 {
		t.Fatalf("block headers = %d, want 3:\n%s", n, text)
	}
	for _, id := range []string{"T1027", "T1055", "T1059.001"} {
		if !strings.Contains(text, "<!-- kb:attack chunk:"+id+" -->") {
			t.Fatalf("provenance comment for %s missing:\n%s", id, text)
		}
		if !strings.Contains(text, "Source: MITRE ATT&CK "+id+" (CC BY 4.0)") {
			t.Fatalf("source line for %s missing:\n%s", id, text)
		}
	}
	if strings.Contains(text, "\n\n\n") {
		t.Fatalf("blocks are not separated by exactly one blank line:\n%s", text)
	}
	if strings.HasSuffix(text, "\n") {
		t.Fatal("rendered section has a trailing newline")
	}

	// Order is the order given (retrieval hands over best-first).
	swapped := Render(k, []Chunk{chunks[2], chunks[0]})
	if strings.Index(swapped, "chunk:T1059.001") > strings.Index(swapped, "chunk:T1027") {
		t.Fatal("Render did not preserve the given order")
	}

	// License policy wins: a chunk the license forbids never renders.
	mixed, err := Load(filepath.Join("testdata", "license"))
	if err != nil {
		t.Fatalf("Load(testdata/license): %v", err)
	}
	mixedText := Render(mixed, mixed.Chunks)
	if !strings.Contains(mixedText, "chunk:CC-1") {
		t.Fatalf("injectable chunk missing:\n%s", mixedText)
	}
	for _, banned := range []string{"chunk:PROP-1", "chunk:NONE-1", "proprietary chunk: readable"} {
		if strings.Contains(mixedText, banned) {
			t.Fatalf("non-injectable content %q was rendered:\n%s", banned, mixedText)
		}
	}
}

// Multiple configured bases join with one blank line; an empty base is skipped
// silently instead of leaving a gap in the prompt.
func TestJoinMultipleKBsAndSkipsEmpty(t *testing.T) {
	attack := renderFixture(t)
	minimal, err := Load(filepath.Join("testdata", "minimal"))
	if err != nil {
		t.Fatalf("Load(testdata/minimal): %v", err)
	}

	sectionA := Render(attack, attack.InjectionCandidates()[:1])
	sectionB := Render(minimal, minimal.InjectionCandidates())
	empty := Render(attack, nil)

	if empty != "" {
		t.Fatalf("empty chunk list rendered %q, want \"\"", empty)
	}

	joined := Join(sectionA, empty, sectionB)
	if strings.Contains(joined, "\n\n\n") {
		t.Fatalf("joined sections are not single-blank-line separated:\n%s", joined)
	}
	a := strings.Index(joined, "<!-- kb:attack chunk:T1027 -->")
	b := strings.Index(joined, "<!-- kb:minimal chunk:T1000 -->")
	if a < 0 || b < 0 {
		t.Fatalf("joined sections missing a base (attack=%d minimal=%d):\n%s", a, b, joined)
	}
	if a > b {
		t.Fatalf("join order not preserved (attack=%d minimal=%d)", a, b)
	}
	if strings.HasPrefix(joined, "\n") || strings.HasSuffix(joined, "\n") {
		t.Fatal("joined text has leading/trailing whitespace")
	}
	if got := Join("", "", ""); got != "" {
		t.Fatalf("Join of empties = %q, want \"\"", got)
	}
}

// On-demand injects one pointer line and no bodies.
func TestRenderOnDemand(t *testing.T) {
	k := renderFixture(t)
	got := RenderOnDemand(k, len(k.InjectionCandidates()))
	want := "## Knowledge: MITRE ATT&CK — curated execution subset v15.1 (on demand)\n" +
		"<!-- kb:attack -->\n" +
		"3 chunks; retrieval via the kb_search tool\n"
	if got != want {
		t.Fatalf("RenderOnDemand:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "Adversaries may attempt") {
		t.Fatal("on-demand section leaked a chunk body")
	}
}

// Summary injects the title list and no bodies.
func TestRenderSummary(t *testing.T) {
	k := renderFixture(t)
	got := RenderSummary(k, k.InjectionCandidates())
	if !strings.HasPrefix(got, "## Knowledge: MITRE ATT&CK — curated execution subset v15.1\n") {
		t.Fatalf("summary header wrong:\n%s", got)
	}
	if !strings.Contains(got, "<!-- kb:attack -->") {
		t.Fatalf("summary provenance comment missing:\n%s", got)
	}
	for _, line := range []string{
		"- T1027 · Obfuscated Files or Information",
		"- T1055 · Process Injection",
		"- T1059.001 · Command and Scripting Interpreter: PowerShell",
	} {
		if !strings.Contains(got, line) {
			t.Fatalf("summary line %q missing:\n%s", line, got)
		}
	}
	if strings.Contains(got, "Adversaries may attempt") || strings.Contains(got, "## Detection") {
		t.Fatalf("summary leaked a body:\n%s", got)
	}
	if RenderSummary(k, nil) != "" {
		t.Fatal("summary of an empty selection should render \"\"")
	}
}

// Strategy normalizes the configured names and flags typos.
func TestStrategyNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"":            StrategyRetrieval,
		"retrieval":   StrategyRetrieval,
		"ALWAYS":      StrategyAlways,
		"on-demand":   StrategyOnDemand,
		"on_demand":   StrategyOnDemand,
		"On Demand":   StrategyOnDemand,
		" summary ":   StrategySummary,
		"ondemand":    StrategyOnDemand,
		"nonsense":    StrategyRetrieval,
		"retrieval x": StrategyRetrieval,
	} {
		got, ok := Strategy(in)
		if got != want {
			t.Fatalf("Strategy(%q) = %q, want %q", in, got, want)
		}
		wantOK := in != "nonsense" && in != "retrieval x"
		if ok != wantOK {
			t.Fatalf("Strategy(%q) ok = %v, want %v", in, ok, wantOK)
		}
	}
}

// The always strategy caps at topK (default 8) in load order.
func TestSelectCandidates(t *testing.T) {
	k := renderFixture(t)
	if got := len(SelectCandidates(k, 2)); got != 2 {
		t.Fatalf("SelectCandidates(topK=2) = %d chunks", got)
	}
	if got := SelectCandidates(k, 0); len(got) != 3 {
		t.Fatalf("SelectCandidates(topK=0) = %d chunks, want all 3", len(got))
	}
	if ids := SelectCandidates(k, 0); ids[0].ID != "T1027" {
		t.Fatalf("first candidate = %q, want load order T1027", ids[0].ID)
	}
	if SelectCandidates(nil, 8) != nil {
		t.Fatal("nil KB should select nothing")
	}
}

// EnsureIndex makes a never-indexed base searchable on first use and leaves an
// already-built index alone.
func TestEnsureIndexBuildsOnce(t *testing.T) {
	k := renderFixture(t)
	ix, err := OpenIndexAt(k, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatalf("OpenIndexAt: %v", err)
	}
	defer ix.Close()

	if err := EnsureIndex(ix); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	var n int
	if err := ix.DB.QueryRow(`SELECT count(*) FROM kb_chunks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("indexed chunks = %d, want 3", n)
	}
	// Second call is a no-op even if the KB changed underneath.
	if err := EnsureIndex(ix); err != nil {
		t.Fatalf("EnsureIndex (again): %v", err)
	}
	if err := EnsureIndex(nil); err != nil {
		t.Fatalf("EnsureIndex(nil) = %v", err)
	}

	res, err := ix.SearchBudget("powershell", 8, 4000, nil)
	if err != nil {
		t.Fatalf("SearchBudget: %v", err)
	}
	if len(res) == 0 || res[0].Chunk.ID != "T1059.001" {
		t.Fatalf("search after EnsureIndex = %v", res)
	}
}
