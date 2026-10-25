package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/kb"
)

// kbBody mirrors the JSON a kb_search call returns.
type kbBody struct {
	Query string `json:"query"`
	Hits  []struct {
		KB      string  `json:"kb"`
		ID      string  `json:"id"`
		Title   string  `json:"title"`
		Score   float64 `json:"score"`
		Excerpt string  `json:"excerpt"`
		Source  string  `json:"source"`
		License string  `json:"license"`
	} `json:"hits"`
	Bases     []string `json:"bases"`
	Available []string `json:"available"`
	Note      string   `json:"note"`
	Warnings  []string `json:"warnings"`
}

type testChunk struct {
	id      string
	title   string
	tactic  string
	source  string
	license string
	prompt  string
	body    string
}

// writeTestKB builds a knowledge base on disk in the shape internal/kb
// expects: kb.yaml plus one Markdown file per chunk.
func writeTestKB(t *testing.T, dir, id string, chunks []testChunk) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, kb.ChunksDir), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf("id: %s\nversion: \"1.0\"\ntitle: \"Test KB %s\"\nsource: \"https://example.invalid/%s\"\nlicense: \"CC BY 4.0\"\n",
		id, id, id)
	if err := os.WriteFile(filepath.Join(dir, kb.MetaFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		doc := fmt.Sprintf("---\nid: %s\ntitle: %q\ntactic: %s\nsource: %q\nlicense: %q\nadversary_prompt: %s\n---\n\n%s\n",
			c.id, c.title, c.tactic, c.source, c.license, c.prompt, c.body)
		path := filepath.Join(dir, kb.ChunksDir, c.id+".md")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func defaultChunks() []testChunk {
	return []testChunk{
		{
			id: "T1059.001", title: "Command and Scripting Interpreter: PowerShell", tactic: "execution",
			source: "MITRE ATT&CK T1059.001", license: "CC BY 4.0",
			prompt: "A plan that shells out to powershell with an encoded command.",
			body:   "PowerShell is a Windows command shell. Adversaries abuse it with -EncodedCommand to stage a payload.",
		},
		{
			id: "T1027", title: "Obfuscated Files or Information", tactic: "defense-evasion",
			source: "MITRE ATT&CK T1027", license: "CC BY 4.0",
			prompt: "A plan that XORs a payload with a single-byte key.",
			body:   "Adversaries may encrypt, encode or otherwise obfuscate file contents to avoid static matching.",
		},
		{
			id: "T1055", title: "Process Injection", tactic: "privilege-escalation",
			source: "MITRE ATT&CK T1055", license: "CC BY 4.0",
			prompt: "A plan that injects code into a trusted process.",
			body:   "Adversaries inject code into live processes to evade defenses and gain persistence.",
		},
	}
}

func decodeKBBody(t *testing.T, res Result) kbBody {
	t.Helper()
	var body kbBody
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("result is not JSON (%v): %s", err, res.Content)
	}
	return body
}

func TestKBSearchSchema(t *testing.T) {
	def := kbSearchTool()
	if def.Type != "function" {
		t.Fatalf("type = %q, want function", def.Type)
	}
	if def.Function.Name != "kb_search" {
		t.Fatalf("name = %q, want kb_search", def.Function.Name)
	}
	if def.Function.Parameters.Type != "object" {
		t.Fatalf("parameters type = %q, want object", def.Function.Parameters.Type)
	}
	if len(def.Function.Parameters.Required) != 1 || def.Function.Parameters.Required[0] != "query" {
		t.Fatalf("required = %v, want [query]", def.Function.Parameters.Required)
	}
	props := def.Function.Parameters.Properties
	if len(props) != 3 {
		t.Fatalf("properties = %d, want 3: %v", len(props), props)
	}
	if props["query"].Type != "string" {
		t.Fatalf("query type = %q, want string", props["query"].Type)
	}
	if props["kb"].Type != "string" {
		t.Fatalf("kb type = %q, want string", props["kb"].Type)
	}
	if props["top_k"].Type != "integer" {
		t.Fatalf("top_k type = %q, want integer", props["top_k"].Type)
	}
	if def.Function.Description == "" {
		t.Fatal("tool has no description")
	}
}

func TestKBSearchRegisteredInDefinitions(t *testing.T) {
	ex := &Executor{Workspace: t.TempDir()}
	found := false
	for _, d := range ex.Definitions() {
		if d.Function.Name == "kb_search" {
			found = true
		}
	}
	if !found {
		t.Fatal("kb_search is not registered in Definitions()")
	}
}

func TestKBSearchDispatchReturnsHitsWithProvenance(t *testing.T) {
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", defaultChunks())
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "kb_search", `{"query":"powershell encoded command"}`)
	if res.Failed {
		t.Fatalf("kb_search failed: %+v", res)
	}
	body := decodeKBBody(t, res)
	if body.Query != "powershell encoded command" {
		t.Fatalf("query = %q", body.Query)
	}
	if len(body.Hits) == 0 {
		t.Fatalf("no hits: %s", res.Content)
	}
	hit := body.Hits[0]
	if hit.KB != "attack" {
		t.Fatalf("hit kb = %q, want attack", hit.KB)
	}
	if hit.ID != "T1059.001" {
		t.Fatalf("hit id = %q, want T1059.001", hit.ID)
	}
	if !strings.Contains(strings.ToLower(hit.Title), "powershell") {
		t.Fatalf("hit title = %q", hit.Title)
	}
	if hit.Source == "" || hit.License == "" {
		t.Fatalf("hit is missing provenance: %+v", hit)
	}
	if hit.License != "CC BY 4.0" {
		t.Fatalf("hit license = %q", hit.License)
	}
	if hit.Excerpt == "" {
		t.Fatal("hit has no excerpt")
	}
	if hit.Score >= 0 {
		t.Fatalf("bm25 score = %v, want a negative (better) value", hit.Score)
	}
	if len(body.Bases) != 1 || body.Bases[0] != "attack" {
		t.Fatalf("bases = %v, want [attack]", body.Bases)
	}
	if len(body.Warnings) != 0 {
		t.Fatalf("warnings = %v", body.Warnings)
	}
}

func TestKBSearchExcerptIsCapped(t *testing.T) {
	long := strings.Repeat("payload staging detail ", 80)
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", []testChunk{
		{id: "T1", title: "Long", tactic: "execution", source: "src", license: "CC BY 4.0",
			prompt: "prompt", body: long},
	})
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "kb_search", `{"query":"payload"}`)
	if res.Failed {
		t.Fatalf("kb_search failed: %+v", res)
	}
	body := decodeKBBody(t, res)
	if len(body.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(body.Hits))
	}
	if n := len([]rune(body.Hits[0].Excerpt)); n > kbExcerptMax+1 {
		t.Fatalf("excerpt is %d runes, want <= %d", n, kbExcerptMax+1)
	}
}

func TestKBSearchTopKIsHonored(t *testing.T) {
	chunks := defaultChunks()
	for i := range chunks {
		chunks[i].body = "PowerShell powershell powershell " + chunks[i].body
	}
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", chunks)

	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "kb_search", `{"query":"powershell","top_k":1}`)
	if res.Failed {
		t.Fatalf("kb_search failed: %+v", res)
	}
	if body := decodeKBBody(t, res); len(body.Hits) != 1 {
		t.Fatalf("top_k=1 returned %d hits", len(body.Hits))
	}

	ex.KBTopK = 2
	res = dispatch(t, ex, "kb_search", `{"query":"powershell"}`)
	if body := decodeKBBody(t, res); len(body.Hits) != 2 {
		t.Fatalf("KBTopK=2 returned %d hits", len(body.Hits))
	}
}

func TestKBSearchTopKIsCappedAt25(t *testing.T) {
	var chunks []testChunk
	for i := 0; i < 30; i++ {
		chunks = append(chunks, testChunk{
			id: fmt.Sprintf("T%03d", i), title: fmt.Sprintf("Chunk %d", i), tactic: "execution",
			source: "src", license: "CC BY 4.0", prompt: "prompt",
			body: "loader loader loader technique detail",
		})
	}
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", chunks)
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "kb_search", `{"query":"loader","top_k":100}`)
	if res.Failed {
		t.Fatalf("kb_search failed: %+v", res)
	}
	if body := decodeKBBody(t, res); len(body.Hits) != kbMaxTopK {
		t.Fatalf("hits = %d, want the cap of %d", len(body.Hits), kbMaxTopK)
	}
}

func TestKBSearchUnknownBaseReturnsEmptyHits(t *testing.T) {
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", defaultChunks())
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "kb_search", `{"query":"powershell","kb":"nosuchbase"}`)
	if res.Failed {
		t.Fatalf("unknown base must not fail the turn: %+v", res)
	}
	body := decodeKBBody(t, res)
	if len(body.Hits) != 0 {
		t.Fatalf("hits = %d, want 0", len(body.Hits))
	}
	if !strings.Contains(body.Note, "nosuchbase") {
		t.Fatalf("note = %q, want it to name the unknown base", body.Note)
	}
	if len(body.Available) != 1 || body.Available[0] != "attack" {
		t.Fatalf("available = %v, want [attack]", body.Available)
	}
}

func TestKBSearchOnlyConfiguredBasesAreSearched(t *testing.T) {
	root := t.TempDir()
	configured := writeTestKB(t, filepath.Join(root, "attack"), "attack", defaultChunks())
	hidden := writeTestKB(t, filepath.Join(root, "secret"), "secret", []testChunk{
		{id: "S1", title: "Secret", tactic: "execution", source: "src", license: "CC BY 4.0",
			prompt: "prompt", body: "powershell powershell secret material"},
	})
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: configured}}}

	// An unconfigured base cannot be reached by naming it.
	res := dispatch(t, ex, "kb_search", `{"query":"powershell","kb":"secret"}`)
	if res.Failed {
		t.Fatalf("unexpected failure: %+v", res)
	}
	if body := decodeKBBody(t, res); len(body.Hits) != 0 {
		t.Fatalf("hits = %d, want 0 for an unconfigured base", len(body.Hits))
	}

	// A path in the kb argument is not a path: it matches no base.
	for _, arg := range []string{"../../etc", "../secret", filepath.Join(root, "secret"), "/etc"} {
		res := dispatch(t, ex, "kb_search", fmt.Sprintf(`{"query":"powershell","kb":%q}`, arg))
		if res.Failed {
			t.Fatalf("unexpected failure for kb=%q: %+v", arg, res)
		}
		body := decodeKBBody(t, res)
		if len(body.Hits) != 0 {
			t.Fatalf("kb=%q returned %d hits, want 0", arg, len(body.Hits))
		}
		if body.Note == "" {
			t.Fatalf("kb=%q returned no note", arg)
		}
	}

	// The unconfigured base was never opened: no derived index was written.
	if _, err := os.Stat(filepath.Join(hidden, kb.IndexFile)); !os.IsNotExist(err) {
		t.Fatalf("unconfigured base was indexed: %v", err)
	}
}

func TestKBSearchNoBaseConfiguredExplainsItself(t *testing.T) {
	ex := &Executor{Workspace: t.TempDir()}

	res := dispatch(t, ex, "kb_search", `{"query":"powershell"}`)
	if res.Failed {
		t.Fatalf("no-KB result must not be a failure: %+v", res)
	}
	body := decodeKBBody(t, res)
	if len(body.Hits) != 0 {
		t.Fatalf("hits = %d, want 0", len(body.Hits))
	}
	if !strings.Contains(strings.ToLower(body.Note), "no knowledge base") {
		t.Fatalf("note = %q, want it to explain that no base is configured", body.Note)
	}
}

func TestKBSearchBuildsDerivedIndexOnFirstUse(t *testing.T) {
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", defaultChunks())
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	if _, err := os.Stat(filepath.Join(dir, kb.IndexFile)); !os.IsNotExist(err) {
		t.Fatalf("index exists before the first search: %v", err)
	}
	res := dispatch(t, ex, "kb_search", `{"query":"powershell"}`)
	if res.Failed {
		t.Fatalf("kb_search failed: %+v", res)
	}
	if len(decodeKBBody(t, res).Hits) == 0 {
		t.Fatal("no hits on a base that was never indexed")
	}
	if _, err := os.Stat(filepath.Join(dir, kb.IndexFile)); err != nil {
		t.Fatalf("derived index not created: %v", err)
	}
	// A second search reuses the index and still answers.
	res = dispatch(t, ex, "kb_search", `{"query":"powershell"}`)
	if res.Failed || len(decodeKBBody(t, res).Hits) == 0 {
		t.Fatalf("second search failed: %+v", res)
	}
}

func TestKBSearchSkipsLicenseForbiddenChunks(t *testing.T) {
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", []testChunk{
		{id: "T1", title: "Allowed", tactic: "execution", source: "src", license: "CC BY 4.0",
			prompt: "prompt", body: "powershell technique"},
		{id: "T2", title: "Forbidden", tactic: "execution", source: "src", license: "proprietary",
			prompt: "prompt", body: "powershell technique"},
	})
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "kb_search", `{"query":"powershell"}`)
	if res.Failed {
		t.Fatalf("kb_search failed: %+v", res)
	}
	for _, h := range decodeKBBody(t, res).Hits {
		if h.ID == "T2" {
			t.Fatal("a chunk whose license forbids injection was returned")
		}
	}
}

func TestKBSearchMissingQueryFails(t *testing.T) {
	ex := &Executor{Workspace: t.TempDir()}
	res := dispatch(t, ex, "kb_search", `{}`)
	if !res.Failed {
		t.Fatalf("missing query must fail: %+v", res)
	}
}

func TestRepairAliasMapsToKBSearch(t *testing.T) {
	for _, name := range []string{"kb search", "search_kb", "knowledge_search", "kb-search", "knowledge base search"} {
		got, err := Repair(name, `{"query":"powershell"}`)
		if err != nil {
			t.Fatalf("%s: repair failed: %v", name, err)
		}
		if got.Tool != "kb_search" {
			t.Fatalf("%s: canonical tool = %q, want kb_search", name, got.Tool)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(got.Args), &obj); err != nil {
			t.Fatalf("%s: repaired args not JSON: %v", name, err)
		}
		if obj["query"] != "powershell" {
			t.Fatalf("%s: repaired args = %v", name, obj)
		}
	}
}

func TestRepairAliasDefaultsKBSearchArguments(t *testing.T) {
	got, err := Repair("kb_search", `{"q":"powershell","base":"attack","limit":3}`)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got.Args), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["query"] != "powershell" {
		t.Fatalf("query = %v", obj["query"])
	}
	if obj["kb"] != "attack" {
		t.Fatalf("kb = %v", obj["kb"])
	}
	if obj["top_k"] != float64(3) {
		t.Fatalf("top_k = %v, want 3", obj["top_k"])
	}
}

func TestKBSearchDispatchThroughAlias(t *testing.T) {
	dir := writeTestKB(t, filepath.Join(t.TempDir(), "attack"), "attack", defaultChunks())
	ex := &Executor{Workspace: t.TempDir(), KBs: []KBSource{{ID: "attack", Dir: dir}}}

	res := dispatch(t, ex, "search_kb", `{"query":"powershell"}`)
	if res.Failed {
		t.Fatalf("aliased dispatch failed: %+v", res)
	}
	if len(decodeKBBody(t, res).Hits) == 0 {
		t.Fatalf("aliased dispatch returned no hits: %s", res.Content)
	}
}
