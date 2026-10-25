package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/persona"
	"kintsugi/internal/scope"
)

// fixture builds a pack on disk: shared base, three family personas with
// different shapes (notes, triggers), plus a capability persona.
func fixture(t *testing.T) (*persona.Pack, *adapters.Registry, string) {
	t.Helper()
	dir := t.TempDir()

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("_base.md", `---
name: _base
description: shared doctrine fixture
inherits: []
---
# Base doctrine

Finish the task. Do not relitigate scope.
`)

	write("deepseek-flash.md", `---
name: deepseek-flash
match:
  family: [deepseek]
  reasoning: false
inherits: [_base]
model_notes: |
  Flash-class model quirk note.
triggers:
  known:
    - term: "payload"
      instead: "the compiled artifact written to artifacts/"
      class: vocabulary
---
# Flash doctrine

One tool call per turn.
`)

	write("qwen.md", `---
name: qwen
match:
  family: [qwen]
inherits: [_base]
model_notes: |
  Qwen drift note.
---
# Qwen doctrine

Re-read files before editing.
`)

	write("glm.md", `---
name: glm
match:
  family: [glm]
inherits: [_base]
---
# GLM doctrine

Explicit ordered steps.
`)

	write("small.md", `---
name: small
match:
  capabilities: [no_tools]
inherits: [_base]
---
# Small-model doctrine

No tool calling; answer from context.
`)

	pack, err := persona.Load(dir, "_base")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	return pack, reg, dir
}

func compileFor(t *testing.T, pack *persona.Pack, reg *adapters.Registry, family, id string, reasoning bool, opts ...func(*Input)) *Compiled {
	t.Helper()
	ad, err := reg.Select(family, id, reasoning)
	if err != nil {
		t.Fatal(err)
	}
	caps := []string{"tools"}
	if !ad.Caps.Tools {
		caps = append(caps, "no_tools")
	}
	sel, err := pack.Select("", family, id, caps)
	if err != nil {
		t.Fatal(err)
	}
	in := Input{
		Provider: "test", ModelID: id, Family: family, Reasoning: reasoning,
		Adapter: ad, Persona: sel, BudgetTokens: 24000, WindowTokens: 128000,
	}
	for _, o := range opts {
		o(&in)
	}
	return Compile(in)
}

// M1 exit: same inputs → byte-identical manifest.
func TestCompileIsDeterministic(t *testing.T) {
	pack, reg, _ := fixture(t)
	a := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false)
	b := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false)

	if a.Text != b.Text {
		t.Fatal("compiled text differs between identical runs")
	}
	ja, _ := json.Marshal(a.Manifest)
	jb, _ := json.Marshal(b.Manifest)
	if string(ja) != string(jb) {
		t.Fatalf("manifest differs between identical runs:\n%s\n---\n%s", ja, jb)
	}
	if a.Manifest.StackHash == "" || a.Manifest.StackHash != b.Manifest.StackHash {
		t.Fatalf("stack hash unstable: %q vs %q", a.Manifest.StackHash, b.Manifest.StackHash)
	}
}

// M1 exit: three model families produce three visibly different manifests.
func TestFamiliesProduceDifferentManifests(t *testing.T) {
	pack, reg, _ := fixture(t)
	d := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false)
	q := compileFor(t, pack, reg, "qwen", "qwen3-coder", false)
	g := compileFor(t, pack, reg, "glm", "glm-4.6", false)

	hashes := map[string]bool{d.Manifest.StackHash: true, q.Manifest.StackHash: true, g.Manifest.StackHash: true}
	if len(hashes) != 3 {
		t.Fatalf("expected three distinct stack hashes, got %d", len(hashes))
	}
	if d.Manifest.Persona.Name == q.Manifest.Persona.Name || q.Manifest.Persona.Name == g.Manifest.Persona.Name {
		t.Fatal("families selected the same persona")
	}
	if d.Manifest.Adapter == q.Manifest.Adapter || q.Manifest.Adapter == g.Manifest.Adapter {
		t.Fatal("families selected the same adapter")
	}

	// Kernel is model-independent by definition.
	kernelHash := func(c *Compiled) string {
		for _, l := range c.Layers {
			if l.ID == "kernel" {
				return l.Hash
			}
		}
		return ""
	}
	if kernelHash(d) != kernelHash(q) || kernelHash(q) != kernelHash(g) {
		t.Fatal("kernel layer differs across models")
	}
}

// M1 exit: switching model mid-session shows a persona+adapter-only diff.
func TestModelSwapDiffIsPersonaAndAdapterOnly(t *testing.T) {
	pack, reg, _ := fixture(t)
	ws := t.TempDir()
	opts := func(in *Input) {
		in.WorkspaceDir = ws
		in.ContextPath = filepath.Join(ws, "CONTEXT.md")
		in.ContextText = "## Authorization & scope\n\nLab environment, own hardware.\n"
		in.BehaviorText = "## Behavior\n\nPlan, then execute.\n"
	}

	oldC := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)
	newC := compileFor(t, pack, reg, "qwen", "qwen3-coder", false, opts)

	diff := strings.Join(Diff(oldC.Manifest, newC.Manifest), "\n")
	if !strings.Contains(diff, "adapter") || !strings.Contains(diff, "persona") {
		t.Fatalf("diff missing adapter/persona changes:\n%s", diff)
	}
	for _, stable := range []string{"kernel", "workspace", "behavior"} {
		if strings.Contains(diff, stable) {
			t.Fatalf("layer %q changed on a model swap; diff:\n%s", stable, diff)
		}
	}
}

// M1 exit: projected cache stability — layers 0-5 are byte-stable across
// turns; only the task layer may change.
func TestCacheStabilityAcrossTurns(t *testing.T) {
	pack, reg, _ := fixture(t)
	ws := t.TempDir()
	opts := func(in *Input) {
		in.WorkspaceDir = ws
		in.ContextText = "## Domain context\n\nSecurity tooling lab.\n"
	}

	base := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)
	perTurn := []*Compiled{base}
	for _, task := range []string{"read a file", "now edit it", "run the tests"} {
		task := task
		perTurn = append(perTurn, compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
			opts(in)
			in.Task = task
		}))
	}

	stable := func(c *Compiled) string {
		var ids []string
		for _, l := range c.Layers {
			if l.ID == "task" {
				continue
			}
			ids = append(ids, l.ID+"="+l.Hash)
		}
		return strings.Join(ids, ";")
	}
	for i := 1; i < len(perTurn); i++ {
		if stable(base) != stable(perTurn[i]) {
			t.Fatalf("cached layers changed on turn %d", i)
		}
	}
}

// Budget policy: workspace drops before behavior, behavior before persona
// optionals, and the kernel never drops.
func TestBudgetDropOrder(t *testing.T) {
	pack, reg, _ := fixture(t)
	ws := t.TempDir()

	big := strings.Repeat("context line that is fairly long and repetitive\n", 200)
	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.WorkspaceDir = ws
		in.ContextText = "## Context\n\n" + big
		in.BehaviorText = "## Behavior\n\n" + strings.Repeat("behavior line\n", 120)
		in.BudgetTokens = 900
	})

	byID := map[string]Layer{}
	for _, l := range c.Layers {
		byID[l.ID] = l
	}
	if byID["workspace"].Dropped != true {
		t.Fatal("workspace should drop first under a tight budget")
	}
	if byID["kernel"].Dropped {
		t.Fatal("kernel must never drop")
	}
	if !strings.Contains(c.Text, "# Flash doctrine") {
		t.Fatal("persona core must survive budgeting")
	}
}

func TestPersonaTriggerMapRenders(t *testing.T) {
	pack, reg, _ := fixture(t)
	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false)
	if !strings.Contains(c.Text, "Trigger map for deepseek-flash") {
		t.Fatal("trigger map not rendered into persona layer")
	}
	if !strings.Contains(c.Text, "the compiled artifact written to artifacts/") {
		t.Fatal("trigger rephrase missing")
	}
}

func TestPrefixChangedSeparatesCacheFromTaskLayers(t *testing.T) {
	mk := func(kernel, adapter, persona, task string) Manifest {
		return Manifest{Layers: []ManifestLayer{
			{ID: "kernel", Hash: kernel},
			{ID: "adapter", Hash: adapter},
			{ID: "persona", Hash: persona},
			{ID: "workspace", Hash: "ws"},
			{ID: "task", Hash: task},
		}}
	}
	base := mk("k", "a", "p", "t1")
	if got := PrefixChanged(base, mk("k", "a", "p", "t2")); len(got) != 0 {
		t.Fatalf("task-only edit invalidated prefix: %v", got)
	}
	got := PrefixChanged(base, mk("k", "a2", "p", "t1"))
	if len(got) != 1 || got[0] != "adapter" {
		t.Fatalf("adapter change = %v, want [adapter]", got)
	}
	got = PrefixChanged(base, mk("k", "a", "p2", "t1"))
	if len(got) != 1 || got[0] != "persona" {
		t.Fatalf("persona change = %v, want [persona]", got)
	}
}

func TestSkillsLayerIsIndexOnlyAndOutsidePrefix(t *testing.T) {
	base := Compile(Input{ModelID: "m", Provider: "p", BudgetTokens: 24000})
	with := Compile(Input{
		ModelID: "m", Provider: "p", BudgetTokens: 24000,
		SkillsText: "## Skills\n\n- `deploy` — deploy the lab",
		SkillsPath: "/ws/.kintsugi/skills",
	})
	var layer *Layer
	for i := range with.Layers {
		if with.Layers[i].ID == "skills" {
			layer = &with.Layers[i]
		}
	}
	if layer == nil {
		t.Fatal("skills layer missing")
	}
	if strings.Contains(layer.Text, "make deploy") {
		t.Fatal("skill body must not be injected")
	}
	if len(PrefixChanged(base.Manifest, with.Manifest)) != 0 {
		t.Fatal("skills index must not invalidate the cache prefix")
	}
	if !strings.Contains(with.Text, "deploy the lab") {
		t.Fatal("skills index not assembled into the prompt")
	}
}

// ---------- layer 3: knowledge base (WP-1.3) ----------

// kbSample is a two-block knowledge section in the fixed tier-3 format.
func kbSample() (text string, meta []KBLayer) {
	block := func(id, title, body string) string {
		return "## Knowledge: ATT&CK v15.1\n" +
			"<!-- kb:attack chunk:" + id + " -->\n" +
			"### " + title + "\n" + body + "\n\n" +
			"Source: MITRE ATT&CK " + id + " (CC BY 4.0)"
	}
	text = block("T1027", "Obfuscated Files or Information", "Base64 or XOR hides the payload.") +
		"\n\n" +
		block("T1059.001", "Command and Scripting Interpreter: PowerShell", "EncodedCommand is the high-signal token.")
	meta = []KBLayer{{ID: "attack", Path: "/kb/attack", Chunks: []string{"T1027", "T1059.001"}}}
	return text, meta
}

// Layer 2 → 3 → 4: the kb layer sits between persona and behavior, both in the
// layer list and in the assembled bytes.
func TestKBLayerSitsBetweenPersonaAndBehavior(t *testing.T) {
	pack, reg, _ := fixture(t)
	kbText, kbMeta := kbSample()
	opts := func(in *Input) {
		in.KBText = kbText
		in.KBMeta = kbMeta
		in.BehaviorText = "## Behavior\n\nPlan, then execute.\n"
	}

	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)

	var ids []string
	for _, l := range c.Layers {
		ids = append(ids, l.ID)
	}
	want := []string{"kernel", "adapter", "persona", "kb", "behavior"}
	if len(ids) < len(want) {
		t.Fatalf("layer ids = %v, want prefix %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("layer %d = %q, want %q (full order %v)", i, ids[i], id, ids)
		}
	}

	var kb *Layer
	for i := range c.Layers {
		if c.Layers[i].ID == "kb" {
			kb = &c.Layers[i]
		}
	}
	if kb == nil {
		t.Fatal("kb layer missing")
	}
	if kb.Kind != "kb" {
		t.Fatalf("kb layer kind = %q, want kb", kb.Kind)
	}
	if want := "attack/T1027,attack/T1059.001"; kb.Path != want {
		t.Fatalf("kb layer path = %q, want %q", kb.Path, want)
	}
	if kb.Dropped {
		t.Fatal("kb layer dropped under a 24000-token budget")
	}

	personaAt := strings.Index(c.Text, "# Flash doctrine")
	kbAt := strings.Index(c.Text, "## Knowledge: ATT&CK v15.1")
	behaviorAt := strings.Index(c.Text, "## Behavior")
	if personaAt < 0 || kbAt < 0 || behaviorAt < 0 {
		t.Fatalf("assembled prompt missing a layer (persona=%d kb=%d behavior=%d)", personaAt, kbAt, behaviorAt)
	}
	if !(personaAt < kbAt && kbAt < behaviorAt) {
		t.Fatalf("assemble order wrong: persona=%d kb=%d behavior=%d", personaAt, kbAt, behaviorAt)
	}

	// No KB content means no kb layer at all.
	bare := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false)
	for _, l := range bare.Layers {
		if l.ID == "kb" {
			t.Fatal("kb layer emitted without KBText")
		}
	}
}

// Same inputs → same bytes, same hashes, same manifest; the manifest carries the
// injected chunk ids.
func TestKBManifestIsDeterministicAndCarriesChunks(t *testing.T) {
	pack, reg, _ := fixture(t)
	kbText, kbMeta := kbSample()
	kbMeta = append(kbMeta, KBLayer{ID: "cwe", Path: "/kb/cwe", Chunks: []string{"CWE-78"}})
	opts := func(in *Input) {
		in.KBText = kbText
		in.KBMeta = kbMeta
	}

	a := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)
	b := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)

	if a.Text != b.Text {
		t.Fatal("compiled text differs between identical runs")
	}
	ja, _ := json.Marshal(a.Manifest)
	jb, _ := json.Marshal(b.Manifest)
	if string(ja) != string(jb) {
		t.Fatalf("manifest differs between identical runs:\n%s\n---\n%s", ja, jb)
	}
	if a.Manifest.StackHash == "" || a.Manifest.StackHash != b.Manifest.StackHash {
		t.Fatalf("stack hash unstable: %q vs %q", a.Manifest.StackHash, b.Manifest.StackHash)
	}

	var layer *ManifestLayer
	for i := range a.Manifest.Layers {
		if a.Manifest.Layers[i].ID == "kb" {
			layer = &a.Manifest.Layers[i]
		}
	}
	if layer == nil {
		t.Fatal("kb manifest layer missing")
	}
	want := []string{"T1027", "T1059.001", "CWE-78"}
	if strings.Join(layer.Chunks, ",") != strings.Join(want, ",") {
		t.Fatalf("manifest chunks = %v, want %v", layer.Chunks, want)
	}
	if !strings.Contains(string(ja), `"chunks":["T1027","T1059.001","CWE-78"]`) {
		t.Fatalf("manifest JSON does not carry chunks: %s", ja)
	}
}

// Budget policy for layer 3: after warranting the workspace drop, the KB layer
// is trimmed from the tail (worst-ranked block first) and must_include survives;
// skills and behavior are untouched because kb is dropped before them.
func TestBudgetTrimsKBTailBeforeSkillsAndBehavior(t *testing.T) {
	pack, reg, _ := fixture(t)
	ws := t.TempDir()

	filler := strings.Repeat("retrieved knowledge body line long enough to cost real tokens\n", 8)
	block := func(id, title string) string {
		return "## Knowledge: ATT&CK v15.1\n" +
			"<!-- kb:attack chunk:" + id + " -->\n" +
			"### " + title + "\n" + filler + "\n\n" +
			"Source: MITRE ATT&CK " + id + " (CC BY 4.0)"
	}
	// must_include is first (the app always puts it first), then ranked hits.
	kbText := strings.Join([]string{
		block("T-MUST", "Must include doctrine"),
		block("T-SECOND", "Second ranked hit"),
		block("T-THIRD", "Third ranked hit"),
	}, "\n\n")

	opts := func(budget int) func(*Input) {
		return func(in *Input) {
			in.WorkspaceDir = ws
			in.ContextText = "## Context\n\n" + strings.Repeat("workspace line\n", 40)
			in.SkillsText = "## Skills\n\n- `deploy` — deploy the lab"
			in.BehaviorText = "## Behavior\n\nPlan, then execute.\n"
			in.KBText = kbText
			in.KBMeta = []KBLayer{{ID: "attack", Path: "/kb/attack",
				Chunks: []string{"T-MUST", "T-SECOND", "T-THIRD"}}}
			in.BudgetTokens = budget
		}
	}

	base := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts(0))
	byID := map[string]ManifestLayer{}
	for _, l := range base.Manifest.Layers {
		byID[l.ID] = l
	}
	// Squeeze hard enough that dropping the workspace alone is not enough, but
	// dropping a single kb block is.
	limit := base.Manifest.Totals.Tokens - byID["workspace"].Tokens - byID["kb"].Tokens/6 - 1

	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts(limit))
	got := map[string]Layer{}
	for _, l := range c.Layers {
		got[l.ID] = l
	}

	if !got["workspace"].Dropped {
		t.Fatal("workspace should drop before the kb layer")
	}
	if got["behavior"].Dropped || got["skills"].Dropped {
		t.Fatalf("behavior/skills dropped before the kb tail was trimmed (behavior=%v skills=%v)",
			got["behavior"].Dropped, got["skills"].Dropped)
	}
	if got["kb"].Dropped {
		t.Fatal("kb layer dropped whole; it must be trimmed from the tail")
	}
	if strings.Join(got["kb"].DroppedSections, ",") != "T-THIRD" {
		t.Fatalf("dropped sections = %v, want exactly the last block T-THIRD", got["kb"].DroppedSections)
	}
	if !strings.Contains(c.Text, "chunk:T-MUST") || !strings.Contains(c.Text, "chunk:T-SECOND") {
		t.Fatal("must_include / better-ranked blocks were trimmed")
	}
	if strings.Contains(c.Text, "chunk:T-THIRD") || strings.Contains(c.Text, "### Third ranked hit") {
		t.Fatal("worst-ranked block survived the trim")
	}
	if got["kb"].Tokens >= byID["kb"].Tokens {
		t.Fatalf("kb layer tokens not recomputed after trim: %d >= %d", got["kb"].Tokens, byID["kb"].Tokens)
	}
}

// A single-block kb layer is never trimmed to nothing: it degrades to its best
// chunk instead of vanishing.
func TestBudgetKeepsLastKBBlock(t *testing.T) {
	pack, reg, _ := fixture(t)
	oneBlock := "## Knowledge: ATT&CK v15.1\n<!-- kb:attack chunk:T-MUST -->\n### Only doctrine\n" +
		strings.Repeat("mandatory line\n", 20) + "\n\nSource: MITRE ATT&CK T-MUST (CC BY 4.0)"

	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.KBText = oneBlock
		in.KBMeta = []KBLayer{{ID: "attack", Path: "/kb/attack", Chunks: []string{"T-MUST"}}}
		in.BudgetTokens = 2000
	})
	var kb *Layer
	for i := range c.Layers {
		if c.Layers[i].ID == "kb" {
			kb = &c.Layers[i]
		}
	}
	if kb == nil {
		t.Fatal("kb layer missing")
	}
	if kb.Text != oneBlock {
		t.Fatalf("single-block kb layer was trimmed:\n%s", kb.Text)
	}
}

// ---------- declared scope (M11.2) ----------

// scopeBlock is the real rendered scope block, produced by the same renderer
// the application calls: the prompt-layer tests then pin the bytes the model
// actually receives, not a hand-copied approximation of them.
func scopeBlock() string {
	return scope.Rules{
		Paths:      []string{"src/**"},
		Commands:   []string{"go test"},
		Hosts:      []string{"example.com"},
		OutOfScope: []string{"production"},
	}.PromptBlock()
}

// layerIndex returns the position of a layer id, or -1.
func layerIndex(layers []Layer, id string) int {
	for i, l := range layers {
		if l.ID == id {
			return i
		}
	}
	return -1
}

// The scope layer is a real, addressable layer: own id, own path, own hash, so
// `/prompt` lists it and `/prompt diff` can tell the operator a rule changed.
func TestScopeLayerIsAddressable(t *testing.T) {
	pack, reg, _ := fixture(t)
	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.ScopeText = scopeBlock()
		in.ScopePath = "/ws/kintsugi.toml [scope]"
	})

	i := layerIndex(c.Layers, "scope")
	if i < 0 {
		t.Fatalf("no scope layer in the stack: %v", layerIDs(c.Layers))
	}
	if got := c.Layers[i].Kind; got != "context" {
		t.Fatalf("scope layer kind = %q, want context", got)
	}
	if c.Layers[i].Hash == "" || c.Layers[i].Tokens == 0 {
		t.Fatalf("scope layer has no hash/tokens: %+v", c.Layers[i])
	}
	if !strings.Contains(c.Text, "## Declared scope") {
		t.Fatal("the scope block is not assembled into the prompt text")
	}

	var ml *ManifestLayer
	for k := range c.Manifest.Layers {
		if c.Manifest.Layers[k].ID == "scope" {
			ml = &c.Manifest.Layers[k]
		}
	}
	if ml == nil {
		t.Fatal("the manifest has no scope layer")
	}
	if ml.Path != "/ws/kintsugi.toml [scope]" {
		t.Fatalf("manifest scope path = %q, want the declaring source", ml.Path)
	}
	if ml.Hash != c.Layers[i].Hash {
		t.Fatalf("manifest hash %q != layer hash %q", ml.Hash, c.Layers[i].Hash)
	}
	// A scope edit shows up in the diff an operator reads.
	changed := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.ScopeText = strings.Replace(scopeBlock(), `paths = ["src/**"]`, `paths = ["docs/**"]`, 1)
		in.ScopePath = "/ws/kintsugi.toml [scope]"
	})
	diff := strings.Join(Diff(c.Manifest, changed.Manifest), "\n")
	if !strings.Contains(diff, "~ scope (") {
		t.Fatalf("/prompt diff does not report the scope change:\n%s", diff)
	}
}

// Position: the scope layer sits below the workspace context it is partly
// derived from and above the task. It is never the task layer.
func TestScopeLayerSitsBelowWorkspaceAndAboveTask(t *testing.T) {
	pack, reg, _ := fixture(t)
	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.ContextText = "## Context\n\nproject memory\n"
		in.BehaviorText = "## Behavior\n\nplan first\n"
		in.ScopeText = scopeBlock()
		in.Task = "do the thing"
	})
	ws, sc, task := layerIndex(c.Layers, "workspace"), layerIndex(c.Layers, "scope"), layerIndex(c.Layers, "task")
	if ws < 0 || sc < 0 || task < 0 {
		t.Fatalf("missing a layer: %v", layerIDs(c.Layers))
	}
	if !(ws < sc && sc < task) {
		t.Fatalf("layer order workspace=%d scope=%d task=%d", ws, sc, task)
	}
	if c.Layers[task].Kind != "user" || c.Layers[sc].Kind == "user" {
		t.Fatal("the scope layer must not be the task layer")
	}
}

// The scope block states the boundary and the consequence; a layer that lists
// rules without saying a call outside them is refused leaves the model exactly
// where it was — proposing work that will be refused.
func TestScopeLayerStatesTheRefusalConsequence(t *testing.T) {
	pack, reg, _ := fixture(t)
	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.ScopeText = scopeBlock()
	})
	flat := strings.Join(strings.Fields(c.Text), " ")
	for _, want := range []string{"Paths in scope", "Commands in scope", "Hosts in scope", "Out of scope",
		"refused", "before the operator is asked", "always wins"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("prompt does not state %q:\n%s", want, c.Text)
		}
	}
}

// A workspace with no declared scope adds no layer, and its manifest is
// byte-identical to the one this compiler produced before the scope layer
// existed. The golden below is that pre-change manifest; regenerate it
// deliberately, never casually — it is the proof that the scope machinery is
// inert when no scope is declared.
func TestEmptyScopeAddsNoLayerAndKeepsTheManifestByteIdentical(t *testing.T) {
	in := Input{Provider: "p", ModelID: "m", Family: "f", BudgetTokens: 24000,
		ContextText: "## Context\n\nhello\n", BehaviorText: "b\n", TasteText: "t\n", Task: "do it"}

	// Regenerated for the kintsugi rename: the kernel text changed ("forge" →
	// "kintsugi"), so its hash and token count moved by one token. Still the
	// proof that the scope machinery is inert when no scope is declared: the
	// layer list and every other hash are unchanged from the pre-scope golden.
	const goldenStack = "sha256:0bff222afcc3868c"
	const goldenManifest = `{"model":{"id":"m","family":"f","provider":"p"},` +
		`"persona":{"name":"","rule":"","path":""},"adapter":"",` +
		`"layers":[` +
		`{"id":"kernel","kind":"builtin","hash":"sha256:d62b905fb7eae544","tokens":296},` +
		`{"id":"behavior","kind":"file","hash":"sha256:0263829989b6fd95","tokens":1},` +
		`{"id":"workspace","kind":"context","hash":"sha256:e2512d0561b72ec1","tokens":5},` +
		`{"id":"taste","kind":"context","path":"operator taste (local)","hash":"sha256:e3b98a4da31a127d","tokens":1},` +
		`{"id":"task","kind":"user","hash":"sha256:1891f3f61eada38a","tokens":4}],` +
		`"totals":{"tokens":307,"budget":24000,"window_used_pct":0},"stack_hash":"` + goldenStack + `"}`

	c := Compile(in)
	if i := layerIndex(c.Layers, "scope"); i >= 0 {
		t.Fatalf("an empty scope emitted a layer: %+v", c.Layers[i])
	}
	b, _ := json.Marshal(c.Manifest)
	if string(b) != goldenManifest {
		t.Fatalf("manifest with no declared scope changed:\n got %s\nwant %s", b, goldenManifest)
	}
	if c.Manifest.StackHash != goldenStack {
		t.Fatalf("stack hash with no declared scope = %q, want %q", c.Manifest.StackHash, goldenStack)
	}

	// An explicitly empty render is the same input: no layer, same bytes.
	empty := Compile(func() Input { i := in; i.ScopeText = ""; i.ScopePath = ""; return i }())
	be, _ := json.Marshal(empty.Manifest)
	if string(be) != goldenManifest || empty.Text != c.Text {
		t.Fatal("an empty scope render did not leave the manifest untouched")
	}
}

// The scope block is a boundary, not content: a budget squeeze drops the
// workspace, behaviour and persona optionals, but never the rules the tool layer
// is enforcing. Dropping it would recreate the invisible-scope bug under load.
func TestBudgetNeverDropsTheScopeLayer(t *testing.T) {
	pack, reg, _ := fixture(t)
	ws := t.TempDir()

	c := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.WorkspaceDir = ws
		in.ContextText = "## Context\n\n" + strings.Repeat("context line that is fairly long\n", 200)
		in.BehaviorText = "## Behavior\n\n" + strings.Repeat("behavior line\n", 120)
		in.ScopeText = scopeBlock()
		in.ScopePath = "kintsugi.toml [scope]"
		in.BudgetTokens = 700
	})

	byID := map[string]Layer{}
	for _, l := range c.Layers {
		byID[l.ID] = l
	}
	if byID["workspace"].Dropped != true {
		t.Fatal("the workspace layer should drop under a tight budget")
	}
	scope, ok := byID["scope"]
	if !ok {
		t.Fatal("the scope layer vanished under a tight budget")
	}
	if scope.Dropped || strings.TrimSpace(scope.Text) == "" {
		t.Fatalf("the scope layer was dropped: %+v", scope)
	}
	if !strings.Contains(c.Text, "## Declared scope") || !strings.Contains(c.Text, "`src/**`") {
		t.Fatal("the enforced boundary is missing from the assembled prompt")
	}
}

// Cache stability, both directions: an unchanged scope is byte-identical turn to
// turn (the cached prefix holds), and a changed one moves the hash (so the
// drill-down works and the invalidation is reported rather than hidden).
func TestScopeLayerCacheStability(t *testing.T) {
	pack, reg, _ := fixture(t)
	opts := func(in *Input) {
		in.ScopeText = scopeBlock()
		in.ScopePath = "kintsugi.toml [scope]"
	}

	base := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)
	scopeHash := func(c *Compiled) string { return layerHash(c.Manifest, "scope") }
	if scopeHash(base) == "" {
		t.Fatal("no scope hash to compare")
	}

	// Across turns: a new task changes the task layer only.
	for _, task := range []string{"read a file", "now edit it", "run the tests"} {
		turn := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
			opts(in)
			in.Task = task
		})
		if scopeHash(turn) != scopeHash(base) {
			t.Fatalf("the scope layer moved on a task-only turn %q", task)
		}
		if got := PrefixChanged(base.Manifest, turn.Manifest); len(got) != 0 {
			t.Fatalf("a task-only turn invalidated the cache prefix: %v", got)
		}
	}

	// Changing nothing at all: identical manifest, identical bytes.
	again := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, opts)
	if again.Text != base.Text || again.Manifest.StackHash != base.Manifest.StackHash {
		t.Fatal("recompiling the same scope is not byte-stable")
	}

	// Changing the rule set: the hash moves, and the prefix report says so.
	edited := compileFor(t, pack, reg, "deepseek", "deepseek-chat", false, func(in *Input) {
		in.ScopeText = strings.Replace(scopeBlock(), `hosts = ["example.com"]`, `hosts = ["example.com", "lab.example"]`, 1)
		in.ScopePath = "kintsugi.toml [scope]"
	})
	if scopeHash(edited) == scopeHash(base) {
		t.Fatal("a changed [scope] did not change the scope layer hash")
	}
	if edited.Manifest.StackHash == base.Manifest.StackHash {
		t.Fatal("a changed [scope] did not change the stack hash")
	}
	if got := PrefixChanged(base.Manifest, edited.Manifest); len(got) != 1 || got[0] != "scope" {
		t.Fatalf("scope change reported as %v, want [scope]", got)
	}
}

// layerIDs lists the layer ids in stack order, for failure messages.
func layerIDs(layers []Layer) []string {
	var out []string
	for _, l := range layers {
		out = append(out, l.ID)
	}
	return out
}

// layerHash returns the manifest hash of one layer id, or "".
func layerHash(m Manifest, id string) string {
	for _, l := range m.Layers {
		if l.ID == id {
			return l.Hash
		}
	}
	return ""
}
