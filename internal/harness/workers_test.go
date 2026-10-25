package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/mcp"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/redact"
	"kintsugi/internal/subagent"
)

func testWorkersApp(t *testing.T) *App {
	t.Helper()
	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.File{
		Core:    config.Core{Mode: "offline"},
		Agents:  config.Agents{Subagents: true, MaxSubagents: 2, TimeoutS: 45},
		Harness: config.Harness{MaxToolLoops: 4},
		Providers: map[string]config.Provider{
			"alpha": {BaseURL: "http://127.0.0.1:1", Wire: "openai-completions", Family: "deepseek", Offline: true},
		},
	}
	return &App{Cfg: cfg, Adapters: reg, Workspace: t.TempDir()}
}

func testParentResolved(t *testing.T) *Resolved {
	t.Helper()
	pc := config.Provider{BaseURL: "http://127.0.0.1:2", Wire: "openai-completions", Offline: true}
	c, err := provider.New(pc, "parent-model", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return &Resolved{ProviderName: "alpha", ProviderCfg: pc, Model: config.Model{ID: "parent-model"}, Client: c}
}

func TestTaskManagerWithRolesWiring(t *testing.T) {
	a := testWorkersApp(t)
	base := testParentResolved(t)
	mgr := &mcp.Manager{}

	m, err := a.TaskManagerWithRoles(base, &prompt.Compiled{Text: "parent stack"}, mgr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Cfg.MCP != mgr {
		t.Fatal("MCP manager not propagated to the worker config")
	}
	if m.Cfg.Client != base.Client {
		t.Fatal("parent client is not the worker fallback")
	}
	if m.Cfg.SystemPrompt != "parent stack" {
		t.Fatalf("system prompt = %q", m.Cfg.SystemPrompt)
	}
	if m.Cfg.Max != 2 || m.Cfg.Timeout != 45*time.Second || m.Cfg.MaxLoops != 4 {
		t.Fatalf("limits = max %d timeout %v loops %d", m.Cfg.Max, m.Cfg.Timeout, m.Cfg.MaxLoops)
	}
	if !m.Cfg.Offline {
		t.Fatal("offline mode not propagated")
	}
	if m.Cfg.ResolveClient == nil {
		t.Fatal("ResolveClient not wired")
	}

	// Unbound roles reuse the parent client: an explicit --model must keep
	// applying to every worker until the role is bound.
	if c, err := m.Cfg.ResolveClient("researcher"); err != nil || c != base.Client {
		t.Fatalf("unbound researcher = %v, %v", c, err)
	}

	// nil MCP manager disables MCP inside workers only.
	bare, err := a.TaskManagerWithRoles(base, &prompt.Compiled{Text: "x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Cfg.MCP != nil {
		t.Fatal("nil manager should leave worker MCP unset")
	}
}

func TestTaskManagerWithRolesResolvesBoundRoles(t *testing.T) {
	a := testWorkersApp(t)
	a.Cfg.Roles = config.Roles{
		Researcher: "alpha/small-model",
		Coder:      "alpha/parent-model", // same model as the parent
	}
	base := testParentResolved(t)
	m, err := a.TaskManagerWithRoles(base, &prompt.Compiled{Text: "x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	research, err := m.Cfg.ResolveClient("researcher")
	if err != nil {
		t.Fatal(err)
	}
	if research == base.Client {
		t.Fatal("bound researcher role reused the parent client")
	}
	if research.Model != "small-model" {
		t.Fatalf("researcher client model = %q", research.Model)
	}

	// A role bound to the parent's own model ref keeps the parent client so the
	// configured fallback chain is not lost.
	if coder, err := m.Cfg.ResolveClient("coder"); err != nil || coder != base.Client {
		t.Fatalf("coder = %v, %v", coder, err)
	}

	a.Cfg.Roles.Opsec = "missing/model"
	if _, err := m.Cfg.ResolveClient("opsec"); err == nil {
		t.Fatal("unresolvable role did not error")
	} else if !strings.Contains(err.Error(), `role "opsec"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestTaskManagerWithRolesGuards(t *testing.T) {
	a := testWorkersApp(t)
	base := testParentResolved(t)
	comp := &prompt.Compiled{Text: "x"}

	a.Cfg.Agents.Subagents = false
	if _, err := a.TaskManagerWithRoles(base, comp, nil, nil); err == nil {
		t.Fatal("subagents disabled accepted")
	}
	a.Cfg.Agents.Subagents = true

	if _, err := a.TaskManagerWithRoles(nil, comp, nil, nil); err == nil {
		t.Fatal("nil base accepted")
	}
	if _, err := a.TaskManagerWithRoles(&Resolved{}, comp, nil, nil); err == nil {
		t.Fatal("unresolved base accepted")
	}
	if _, err := a.TaskManagerWithRoles(base, nil, nil, nil); err == nil {
		t.Fatal("nil compiled prompt accepted")
	}
}

// ---------- M8.5.5 per-worker context bundles ----------

func testBundleApp(t *testing.T, contextDoc string) *App {
	t.Helper()
	a := testWorkersApp(t)
	a.Cfg.Context = config.Context{Enabled: true, Path: "CONTEXT.md"}
	if contextDoc != "" {
		if err := os.WriteFile(filepath.Join(a.Workspace, "CONTEXT.md"), []byte(contextDoc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

const testContextDoc = `# Project: kintsugi

## Authorization

- Authorized dual-use R&D in an isolated local sandbox with no egress.

## Scope boundaries

- Defensive development and lab validation only.

## Domain context

- AgentForge is a local-first coding harness for one operator.

## Stack

- Go.
`

func TestTaskManagerWithBundleWiring(t *testing.T) {
	a := testBundleApp(t, testContextDoc)
	base := testParentResolved(t)
	mgr := &mcp.Manager{}

	var seen []string
	calls := 0
	m, err := a.TaskManagerWithBundle(base, &prompt.Compiled{Text: "parent stack"}, mgr,
		func(task, role string) subagent.ContextBundle {
			calls++
			seen = append(seen, task+"/"+role)
			return subagent.ContextBundle{Domain: "domain for " + task}
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Cfg.BundleFor == nil {
		t.Fatal("TaskManagerWithBundle did not set BundleFor")
	}
	if m.Cfg.BundleTokens != subagent.BundleTokensDefault {
		t.Fatalf("bundle token cap = %d", m.Cfg.BundleTokens)
	}
	// The rest of the construction is shared with TaskManagerWithRoles.
	if m.Cfg.MCP != mgr || m.Cfg.Client != base.Client || m.Cfg.SystemPrompt != "parent stack" ||
		m.Cfg.Max != 2 || m.Cfg.Timeout != 45*time.Second || m.Cfg.MaxLoops != 4 || !m.Cfg.Offline {
		t.Fatalf("construction drifted: %+v", m.Cfg)
	}
	bundle := m.Cfg.BundleFor("alpha", "coder")
	if len(seen) != 1 || seen[0] != "alpha/coder" || calls != 1 {
		t.Fatalf("bundleFor calls = %v", seen)
	}
	if bundle.Domain != "domain for alpha" {
		t.Fatalf("bundle = %+v", bundle)
	}

	// A nil bundleFor falls back to the App's own bundle builder.
	auto, err := a.TaskManagerWithBundle(base, &prompt.Compiled{Text: "x"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auto.Cfg.BundleFor == nil {
		t.Fatal("nil bundleFor did not fall back to BuildWorkerBundle")
	}
	if b := auto.Cfg.BundleFor("alpha", "coder"); !strings.Contains(b.Authorization, "Authorized dual-use") {
		t.Fatalf("fallback bundle = %+v", b)
	}

	// TaskManagerWithRoles keeps its old signature and carries no bundle.
	plain, err := a.TaskManagerWithRoles(base, &prompt.Compiled{Text: "x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Cfg.BundleFor != nil {
		t.Fatal("TaskManagerWithRoles grew a bundle source")
	}
	if plain.Cfg.BundleTokens != 0 {
		t.Fatalf("TaskManagerWithRoles bundle cap = %d, want unset", plain.Cfg.BundleTokens)
	}
}

func TestTaskManagerWithBundleGuards(t *testing.T) {
	a := testBundleApp(t, testContextDoc)
	base := testParentResolved(t)
	comp := &prompt.Compiled{Text: "x"}
	a.Cfg.Agents.Subagents = false
	if _, err := a.TaskManagerWithBundle(base, comp, nil, nil, nil); err == nil {
		t.Fatal("subagents disabled accepted")
	}
	a.Cfg.Agents.Subagents = true
	if _, err := a.TaskManagerWithBundle(nil, comp, nil, nil, nil); err == nil {
		t.Fatal("nil base accepted")
	}
	if _, err := a.TaskManagerWithBundle(base, nil, nil, nil, nil); err == nil {
		t.Fatal("nil compiled prompt accepted")
	}
}

func TestBuildWorkerBundleFillsFrame(t *testing.T) {
	prev := SetWorkerPriorWork(nil)
	t.Cleanup(func() { SetWorkerPriorWork(prev) })

	a := testBundleApp(t, testContextDoc)
	b := a.BuildWorkerBundle("implement the bundle", "coder")
	if !strings.Contains(b.Authorization, "Authorized dual-use R&D") {
		t.Fatalf("authorization = %q", b.Authorization)
	}
	if !strings.Contains(b.Domain, "local-first coding harness") {
		t.Fatalf("domain = %q", b.Domain)
	}
	if !strings.Contains(b.Scope, "Defensive development") {
		t.Fatalf("scope = %q", b.Scope)
	}
	// Prior work and KB stay empty until the coordinator wires them.
	if len(b.PriorWork) != 0 || len(b.KB) != 0 || b.Handoff != "" {
		t.Fatalf("bundle = %+v", b)
	}
	rendered := subagent.RenderBundle(b, subagent.BundleTokensDefault)
	for _, want := range []string{"## Worker context bundle", subagent.BundleFraming, "### Authorization", "### Domain", "### Scope"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered bundle missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "## Prior accepted work") || strings.Contains(rendered, "## Knowledge") {
		t.Fatalf("empty sections rendered:\n%s", rendered)
	}
}

func TestBuildWorkerBundleWorksWithoutContextFile(t *testing.T) {
	prev := SetWorkerPriorWork(nil)
	t.Cleanup(func() { SetWorkerPriorWork(prev) })
	a := testBundleApp(t, "")
	if b := a.BuildWorkerBundle("task", "coder"); !b.Empty() {
		t.Fatalf("bundle without CONTEXT.md = %+v", b)
	}
	if got := subagent.RenderBundle(a.BuildWorkerBundle("task", "coder"), 0); got != "" {
		t.Fatalf("bundle without CONTEXT.md rendered %q", got)
	}
}

func TestBuildWorkerBundlePriorWorkHook(t *testing.T) {
	prev := SetWorkerPriorWork(nil)
	t.Cleanup(func() { SetWorkerPriorWork(prev) })
	a := testBundleApp(t, testContextDoc)

	var asked []string
	SetWorkerPriorWork(func(task string) []subagent.PriorWorkRef {
		asked = append(asked, task)
		return []subagent.PriorWorkRef{
			{ID: "L-1", Date: "2026-02-03", Kind: "plan", Summary: "accepted: " + task, Evidence: []string{"sha256:abc"}},
			{ID: "L-2", Date: "2026-02-04", Kind: "decision", Summary: "accepted: layers stay static"},
		}
	})
	if WorkerPriorWork() == nil {
		t.Fatal("hook not installed")
	}
	b := a.BuildWorkerBundle("wire bundles", "researcher")
	if len(asked) != 1 || asked[0] != "wire bundles" {
		t.Fatalf("hook calls = %v", asked)
	}
	if len(b.PriorWork) != 2 || b.PriorWork[0].ID != "L-1" {
		t.Fatalf("prior work = %+v", b.PriorWork)
	}
	rendered := subagent.RenderBundle(b, subagent.BundleTokensDefault)
	if !strings.Contains(rendered, "## Prior accepted work") || !strings.Contains(rendered, "accepted: wire bundles") {
		t.Fatalf("rendered bundle:\n%s", rendered)
	}

	// The hook's entries are capped at the retrieval bound.
	SetWorkerPriorWork(func(string) []subagent.PriorWorkRef {
		out := make([]subagent.PriorWorkRef, 0, 12)
		for i := 0; i < 12; i++ {
			out = append(out, subagent.PriorWorkRef{ID: fmt.Sprintf("L-%d", i)})
		}
		return out
	})
	if b := a.BuildWorkerBundle("t", "coder"); len(b.PriorWork) != maxBundlePriorWork {
		t.Fatalf("prior work = %d entries, want %d", len(b.PriorWork), maxBundlePriorWork)
	}

	// Clearing the hook stops prior work without touching the frame.
	SetWorkerPriorWork(nil)
	if b := a.BuildWorkerBundle("t", "coder"); len(b.PriorWork) != 0 || b.Authorization == "" {
		t.Fatalf("bundle after clearing hook = %+v", b)
	}
}

// TestBuildWorkerBundleIsBoundedAndRedacted proves a hostile-sized CONTEXT.md
// cannot blow the worker prompt budget and that secret-shaped text is scrubbed
// by the renderer.
func TestBuildWorkerBundleIsBoundedAndRedacted(t *testing.T) {
	prev := SetWorkerPriorWork(nil)
	t.Cleanup(func() { SetWorkerPriorWork(prev) })

	secret := "sk-" + strings.Repeat("z", 24)
	doc := "# Project: kintsugi\n\n## Authorization\n\n" +
		strings.Repeat("- authorization line\n", 500) +
		"\n## Domain context\n\n- token " + secret + "\n"
	a := testBundleApp(t, doc)
	b := a.BuildWorkerBundle("task", "coder")

	rendered := subagent.RenderBundle(b, subagent.BundleTokensDefault)
	if len(rendered) > subagent.BundleTokensDefault*4 {
		t.Fatalf("rendered bundle = %d bytes, want <= %d", len(rendered), subagent.BundleTokensDefault*4)
	}
	if !strings.Contains(rendered, subagent.BundleFraming) {
		t.Fatal("bounded bundle lost its framing line")
	}
	if redacted := redact.Redact(rendered); strings.Contains(redacted, secret) {
		t.Fatal("secret survived redaction")
	}
}
