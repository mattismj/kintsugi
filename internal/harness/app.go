// Package kintsugi is the application glue: config + persona pack + adapters +
// compiler + provider client. CLI and TUI are thin layers over this.
package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"kintsugi/internal/adapters"
	"kintsugi/internal/advisor"
	"kintsugi/internal/agent"
	"kintsugi/internal/agentsctx"
	"kintsugi/internal/catalog"
	"kintsugi/internal/config"
	fctx "kintsugi/internal/context"
	"kintsugi/internal/continuity"
	"kintsugi/internal/credentials"
	"kintsugi/internal/gate"
	"kintsugi/internal/hooks"
	"kintsugi/internal/hub"
	"kintsugi/internal/intercept"
	"kintsugi/internal/kb"
	"kintsugi/internal/lab"
	"kintsugi/internal/ledger"
	"kintsugi/internal/mcp"
	"kintsugi/internal/memory"
	"kintsugi/internal/mods"
	"kintsugi/internal/persona"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/quota"
	"kintsugi/internal/rules"
	"kintsugi/internal/scope"
	"kintsugi/internal/session"
	"kintsugi/internal/skills"
	"kintsugi/internal/subagent"
	"kintsugi/internal/taste"
	"kintsugi/internal/tools"
	"kintsugi/pack"
)

type App struct {
	// SessionID is the current session, set when a session is opened. Hooks
	// and subagents use it to label their events.
	SessionID   string
	Cfg         config.File
	ConfigPaths []string
	Pack        *persona.Pack
	Adapters    *adapters.Registry
	Workspace   string

	// workerPriorWork is the App-scoped prior-accepted-work source handed to
	// worker bundles (M8.5.5). When nil, BuildWorkerBundle falls back to the
	// package-level WorkerPriorWork hook, so both wirings keep working.
	workerPriorWork WorkerPriorWorkFunc

	// refusalStats is a seam: nil means "aggregate the live session dir". The
	// preflight scorer only ever reads class counters from it.
	refusalStats func() ([]session.RefusalStat, error)

	// bgHub is the session's background run registry (M10-D), created on first
	// use so a session that never detaches a task allocates nothing. AttachTools
	// hands it to every executor and the front-end renders the roster from it,
	// so spawn and collect always see the same runs.
	bgHub *hub.Hub

	// Model-keyed context documents (I-80). resolved is the model the current
	// prompt is compiled for, kept so AgentsMD can key on it without threading
	// a Resolved through every call. modelDocOverride forces a document by name
	// (the TUI's "use this document"); empty means rule matching. The cached
	// docs and warnings back the TUI list without re-reading the directory on
	// every render.
	resolved         *Resolved
	modelDocOverride string
	modelDocs        []agentsctx.Doc
	modelDocWarnings []string
}

// Hub returns the session's background run registry, creating it on first use.
func (a *App) Hub() *hub.Hub {
	if a.bgHub == nil {
		a.bgHub = hub.NewHub()
	}
	return a.bgHub
}

func New(workspace string) (*App, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	cfg, paths, err := config.Load(abs)
	if err != nil {
		return nil, err
	}
	// A previously imported catalog only adds models to providers that are
	// already configured; it never invents endpoints or keys.
	catPath := cfg.Core.CatalogPath
	if strings.TrimSpace(catPath) == "" {
		catPath = config.ExpandHome("~/.config/kintsugi/catalog.json")
	}
	if b, err := os.ReadFile(catPath); err == nil {
		if cat, err := catalog.Parse(b); err == nil {
			catalog.MergeInto(&cfg, cat)
		}
	}
	reg, err := adapters.Load()
	if err != nil {
		return nil, err
	}

	app := &App{Cfg: cfg, ConfigPaths: paths, Adapters: reg, Workspace: abs}

	packDir := config.ExpandHome(cfg.PersonaPack.Dir)
	loaded := false
	if packDir != "" {
		if _, err := os.Stat(packDir); err == nil {
			pack, err := persona.Load(packDir, cfg.PersonaPack.Fallback)
			if err != nil {
				return nil, err
			}
			app.Pack = pack
			loaded = true
		}
	}
	if !loaded {
		// A fresh install has no config dir yet: use the embedded pack so
		// `kintsugi` works out of the box. `kintsugi setup` materializes it for
		// editing.
		p, err := persona.LoadFS(pack.FS, "agents/global", cfg.PersonaPack.Fallback)
		if err != nil {
			return nil, fmt.Errorf("persona pack: %w", err)
		}
		app.Pack = p
	}
	return app, nil
}

// PersonaPackDir is the absolute pack directory currently configured.
func (a *App) PersonaPackDir() string {
	return config.ExpandHome(a.Cfg.PersonaPack.Dir)
}

// ReloadPersona re-reads the pack from disk. On error the previous pack stays
// active: a malformed edit never takes a running session down.
func (a *App) ReloadPersona() error {
	dir := a.PersonaPackDir()
	if dir != "" {
		if _, err := os.Stat(dir); err == nil {
			loaded, err := persona.Load(dir, a.Cfg.PersonaPack.Fallback)
			if err != nil {
				return err
			}
			a.Pack = loaded
			return nil
		}
	}
	loaded, err := persona.LoadFS(pack.FS, "agents/global", a.Cfg.PersonaPack.Fallback)
	if err != nil {
		return err
	}
	a.Pack = loaded
	return nil
}

// Resolved is one model + provider + adapter, ready to compile or stream.
type Resolved struct {
	ProviderName string
	ProviderCfg  config.Provider
	Model        config.Model
	Adapter      *adapters.Adapter
	Client       *provider.Client
}

// Resolve turns a model ref into a provider + adapter + client. A ref of the
// form `chain/<name>` (or a bare configured chain name) resolves the chain's
// slots and returns the first one as the primary, with the rest attached as
// provider.Client.Fallbacks (WP-4.2).
func (a *App) Resolve(ref string) (*Resolved, error) {
	if ref == "" {
		ref = a.Cfg.Core.DefaultModel
	}
	if name, ok := a.chainRef(ref); ok {
		return a.resolveChain(name)
	}
	return a.resolveModel(ref)
}

// chainRef reports whether ref names a configured fallback chain. `chain/<name>`
// always means a chain (so a typo fails loudly instead of resolving as a
// provider); a bare word means a chain only when one is configured under it.
func (a *App) chainRef(ref string) (string, bool) {
	if name, ok := strings.CutPrefix(strings.TrimSpace(ref), "chain/"); ok {
		return name, true
	}
	if !strings.Contains(ref, "/") {
		if _, ok := a.Cfg.Chains[ref]; ok {
			return ref, true
		}
	}
	return "", false
}

// resolveChain builds one client per slot and attaches every slot after the
// first as a fallback of the primary. Adapter and model metadata always come
// from the primary: the compiled prompt is written for one model.
func (a *App) resolveChain(name string) (*Resolved, error) {
	ch, ok := a.Cfg.Chains[name]
	if !ok {
		names := make([]string, 0, len(a.Cfg.Chains))
		for n := range a.Cfg.Chains {
			names = append(names, n)
		}
		sort.Strings(names)
		list := "none"
		if len(names) > 0 {
			list = strings.Join(names, ", ")
		}
		return nil, fmt.Errorf("unknown chain %q (configured chains: %s)", name, list)
	}
	if len(ch.Providers) == 0 {
		return nil, fmt.Errorf("chain %q has no providers", name)
	}
	var primary *Resolved
	var fallbacks []*provider.Client
	for i, slot := range ch.Providers {
		slot = strings.TrimSpace(slot)
		if slot == "" {
			return nil, fmt.Errorf("chain %q slot %d is empty", name, i)
		}
		r, err := a.resolveModel(slot)
		if err != nil {
			return nil, fmt.Errorf("chain %q slot %d (%s): %w", name, i, slot, err)
		}
		if i == 0 {
			primary = r
			continue
		}
		fallbacks = append(fallbacks, r.Client)
	}
	primary.Client.SetFallbacks(fallbacks)
	return primary, nil
}

// resolveModel resolves a plain `provider/model` ref.
func (a *App) resolveModel(ref string) (*Resolved, error) {
	pCfg, name, model, err := a.Cfg.ResolveModel(ref)
	if err != nil {
		return nil, err
	}
	ad, err := a.Adapters.Select(model.Family, model.ID, model.Reasoning)
	if err != nil {
		return nil, err
	}
	client, err := provider.New(pCfg, model.ID, a.Cfg.Core.Mode)
	if err != nil {
		return nil, err
	}
	// Stored credentials (`kintsugi connect <provider>`) are the fallback, never an
	// override: provider.New already read the environment, so a set env var wins
	// and a stored key only fills the gap it leaves. A failing credential store
	// is not fatal here — compilation and `doctor` must work without one — and
	// requireKey still produces the actionable message at request time.
	if client.APIKey == "" && pCfg.APIKeyEnv != "" {
		if store, err := credentials.Load(); err == nil {
			if key, ok := store.Get(name); ok {
				client.APIKey = key
			}
		}
	}
	client.MergeSystemMessages = ad.MergeSystemMessages()
	client.DeveloperRole = ad.Caps.DeveloperRole
	return &Resolved{
		ProviderName: name, ProviderCfg: pCfg, Model: model,
		Adapter: ad, Client: client,
	}, nil
}

// ResolveRole resolves the model bound to a role, falling back to the default
// model when the role is unset.
func (a *App) ResolveRole(role string) (*Resolved, error) {
	ref := a.Cfg.Roles.Model(role)
	if strings.TrimSpace(ref) == "" {
		ref = a.Cfg.Core.DefaultModel
	}
	return a.Resolve(ref)
}

// CompileRole applies the role's persona unless an explicit override is given,
// and selects the knowledge bases bound to that role (layer 3).
func (a *App) CompileRole(r *Resolved, role string, opts CompileOpts) (*prompt.Compiled, error) {
	if strings.TrimSpace(opts.Persona) == "" {
		opts.Persona = a.Cfg.Roles.Persona(role)
	}
	if strings.TrimSpace(opts.Role) == "" {
		opts.Role = role
	}
	return a.Compile(r, opts)
}

// Capabilities feeds the persona selector's capability rule.
func (r *Resolved) Capabilities() []string {
	caps := append([]string{}, r.Model.Capabilities...)
	if r.Adapter.Caps.Tools {
		caps = append(caps, "tools")
	} else {
		caps = append(caps, "no_tools")
	}
	if r.Model.Reasoning {
		caps = append(caps, "reasoning")
	}
	if smallModel(r.Model.ID) {
		caps = append(caps, "small")
	}
	if r.ProviderCfg.Offline {
		caps = append(caps, "local")
	}
	return caps
}

// smallModel reports whether a model id names a small parameter count
// (<= 14B), e.g. "qwen3.5-abliterated:9b", "llama3-8b", "phi:3.8b". Small
// models get the compact standalone persona instead of the full doctrine.
func smallModel(id string) bool {
	low := strings.ToLower(id)
	for i := 0; i < len(low); i++ {
		if low[i] < '0' || low[i] > '9' {
			continue
		}
		j := i
		for j < len(low) && ((low[j] >= '0' && low[j] <= '9') || low[j] == '.') {
			j++
		}
		if j < len(low) && low[j] == 'b' {
			n, err := strconv.ParseFloat(low[i:j], 64)
			if err == nil && n > 0 && n <= 14 {
				return true
			}
		}
		for j < len(low) && low[j] >= '0' && low[j] <= '9' {
			j++
		}
		i = j
	}
	return false
}

type CompileOpts struct {
	Persona string // explicit persona name overrides matching
	Profile string // behavior profile name (default from config)
	Task    string // included as the task layer when non-empty
	Budget  int    // 0 = config default
	Role    string // role used to select configured knowledge bases
}

// Compile builds the prompt stack for a resolved model.
func (a *App) Compile(r *Resolved, opts CompileOpts) (*prompt.Compiled, error) {
	if a.Pack == nil {
		return nil, fmt.Errorf("persona pack not found at %s (set persona_pack.dir)", config.ExpandHome(a.Cfg.PersonaPack.Dir))
	}
	// Remember the model this prompt is for: the model-keyed AGENTS.md tier
	// (I-80) keys on it, and so does the TUI's document list.
	a.resolved = r
	sel, err := a.Pack.Select(opts.Persona, r.Model.Family, r.Model.ID, r.Capabilities())
	if err != nil {
		return nil, err
	}

	profile := opts.Profile
	if profile == "" {
		profile = a.Cfg.Behavior.DefaultProfile
	}
	behaviorText, behaviorPath, err := a.behavior(profile, r)
	if err != nil {
		return nil, err
	}

	contextText, contextPath := a.context()
	// The operator attestation belongs to the static layer-5 frame: it is
	// appended before Compile so the manifest shows exactly what the model
	// sees. RenderAttestation is a pure function of the recorded fields, so the
	// cached prefix stays byte-identical turn to turn (DESIGN §3.6).
	if att := a.attestationText(); att != "" {
		if strings.TrimSpace(contextText) == "" {
			contextText = att
			if contextPath == "" {
				contextPath = fctx.AttestationPath(a.Workspace)
			}
		} else {
			contextText = strings.TrimRight(contextText, "\n") + "\n\n" + att
		}
	}
	skillsText, skillsPath := a.skillsIndex()
	kbText, kbMeta, kbWarnings := a.knowledge(opts.Role, opts.Task)

	// The declared scope is enforced in the tool layer, so it has to be visible
	// in the prompt: a model held to a boundary it cannot read proposes work that
	// will be refused and cannot explain the refusal. The rules come from
	// scope.FromWorkspace — the exact call tools.Executor.ApplySecurity makes —
	// so [scope] and the CONTEXT.md "Scope" section are merged the same way on
	// both sides. A renderer fed a different merge would produce a prompt that
	// disagrees with enforcement, which is worse than no prompt at all.
	scopeRules := scope.FromWorkspace(a.Cfg, a.Workspace)
	scopeText, scopePath := scopeRules.PromptBlock(), a.scopeSourcePath()

	budgetTokens := opts.Budget
	if budgetTokens == 0 {
		budgetTokens = a.Cfg.Budget.MaxPromptTokens
	}

	in := prompt.Input{
		Provider:     r.ProviderName,
		ModelID:      r.Model.ID,
		Family:       r.Model.Family,
		Reasoning:    r.Model.Reasoning,
		Capabilities: r.Capabilities(),
		WindowTokens: r.Model.Context,
		Adapter:      r.Adapter,
		Persona:      sel,
		KBText:       kbText,
		KBMeta:       kbMeta,
		BehaviorText: behaviorText,
		BehaviorPath: behaviorPath,
		SkillsText:   skillsText,
		SkillsPath:   skillsPath,
		WorkspaceDir: a.Workspace,
		ContextPath:  contextPath,
		ContextText:  contextText,
		TasteText:    a.tasteBlock(),
		ScopeText:    scopeText,
		ScopePath:    scopePath,
		Task:         opts.Task,
		BudgetTokens: budgetTokens,
	}
	comp := prompt.Compile(in)
	// Knowledge bases are content, not control flow: a base that cannot be
	// loaded or searched is reported and skipped, never fatal.
	if len(kbWarnings) > 0 {
		comp.Manifest.Warnings = append(comp.Manifest.Warnings, kbWarnings...)
	}
	return comp, nil
}

// kbDefaultTopK is the chunk cap used when a binding leaves top_k unset. It
// matches the index package's retrieval default on purpose.
const kbDefaultTopK = 8

// kbRetrievalMaxBytes is the per-base byte budget handed to SearchBudget: a
// soft cap over the optional (ranked) hits. Must-include chunks are mandatory
// and are always injected regardless of this number.
const kbRetrievalMaxBytes = 4000

// kbBinding is one configured knowledge base that applies to the compiled role.
type kbBinding struct {
	name string
	cfg  config.KB
}

// kbBindings returns the configured bases that apply to role, ordered by name
// so the injected text — and therefore the stack hash — is deterministic.
func (a *App) kbBindings(role string) []kbBinding {
	if len(a.Cfg.KBs) == 0 {
		return nil
	}
	names := make([]string, 0, len(a.Cfg.KBs))
	for n := range a.Cfg.KBs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]kbBinding, 0, len(names))
	for _, n := range names {
		cfg := a.Cfg.KBs[n]
		if !kbApplies(cfg.Roles, role) {
			continue
		}
		out = append(out, kbBinding{name: n, cfg: cfg})
	}
	return out
}

// kbApplies reports whether a binding's roles cover role. An empty role list
// binds the base to every role; otherwise the comparison is case-insensitive
// and an empty role matches only the all-role bindings.
func kbApplies(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return false
	}
	for _, r := range roles {
		if strings.EqualFold(strings.TrimSpace(r), role) {
			return true
		}
	}
	return false
}

// knowledge resolves the configured knowledge bases for role and renders
// layer 3. Every base is best-effort: a base that cannot be loaded, indexed or
// searched becomes a warning and is skipped, and an empty base is skipped
// silently. Compilation never fails because of content.
func (a *App) knowledge(role, task string) (text string, meta []prompt.KBLayer, warnings []string) {
	bindings := a.kbBindings(role)
	if len(bindings) == 0 {
		return "", nil, nil
	}
	sections := make([]string, 0, len(bindings))
	for _, b := range bindings {
		section, m, warns := a.kbSection(b, task)
		warnings = append(warnings, warns...)
		if strings.TrimSpace(section) == "" {
			continue
		}
		sections = append(sections, section)
		if m != nil {
			meta = append(meta, *m)
		}
	}
	return kb.Join(sections...), meta, warnings
}

// kbSection renders one binding under its strategy.
func (a *App) kbSection(b kbBinding, task string) (string, *prompt.KBLayer, []string) {
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, `kb "`+b.name+`": `+fmt.Sprintf(format, args...))
	}

	strategy, ok := kb.Strategy(b.cfg.Strategy)
	if !ok {
		warn("unknown strategy %q (want retrieval|always|on-demand|summary); using %s",
			b.cfg.Strategy, strategy)
	}
	dir := a.kbDir(b.cfg.Path)
	if dir == "" {
		warn("path is empty")
		return "", nil, warnings
	}
	k, err := kb.Load(dir)
	if err != nil {
		warn("%v", err)
		return "", nil, warnings
	}

	topK := b.cfg.TopK
	if topK <= 0 {
		topK = kbDefaultTopK
	}

	switch strategy {
	case kb.StrategyAlways:
		// The operator said this whole base belongs in the prompt: no ranking,
		// just load order, capped so a big base cannot swallow the budget.
		chunks := kb.SelectCandidates(k, topK)
		return kb.Render(k, chunks), &prompt.KBLayer{ID: k.ID, Path: dir, Chunks: chunkIDs(chunks)}, warnings

	case kb.StrategyOnDemand:
		// No bodies: one line saying the base exists and how to search it. A
		// base with nothing injectable is skipped like an empty one.
		n := len(k.InjectionCandidates())
		if n == 0 {
			return "", nil, warnings
		}
		return kb.RenderOnDemand(k, n), &prompt.KBLayer{ID: k.ID, Path: dir}, warnings

	case kb.StrategySummary:
		// Titles only: what the base covers, with no retrieval and no bodies.
		chunks := k.InjectionCandidates()
		return kb.RenderSummary(k, chunks),
			&prompt.KBLayer{ID: k.ID, Path: dir, Chunks: chunkIDs(chunks)}, warnings

	default: // kb.StrategyRetrieval
		chunks, err := a.kbRetrieve(k, task, topK, b.cfg.MustInclude)
		if err != nil {
			warn("%v", err)
			return "", nil, warnings
		}
		return kb.Render(k, chunks), &prompt.KBLayer{ID: k.ID, Path: dir, Chunks: chunkIDs(chunks)}, warnings
	}
}

// kbRetrieve ranks the base against the task under the per-base byte budget.
// An empty task means "no query": only the must_include chunks are injected,
// which is how a base declares mandatory doctrine that needs no match.
func (a *App) kbRetrieve(k *kb.KB, task string, topK int, mustInclude []string) ([]kb.Chunk, error) {
	ix, err := kb.OpenIndex(k)
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	// A base that has never been indexed is still searchable: build the
	// derived index on first use (it is disposable by doctrine).
	if err := kb.EnsureIndex(ix); err != nil {
		return nil, err
	}
	results, err := ix.SearchBudget(strings.TrimSpace(task), topK, kbRetrievalMaxBytes, mustInclude)
	if err != nil {
		return nil, err
	}
	chunks := make([]kb.Chunk, 0, len(results))
	for _, r := range results {
		chunks = append(chunks, r.Chunk)
	}
	return chunks, nil
}

// kbDir resolves a configured KB path: ~ expanded, absolute as-is, anything
// else relative to the workspace.
func (a *App) kbDir(p string) string {
	p = strings.TrimSpace(config.ExpandHome(p))
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(a.Workspace, p)
}

func chunkIDs(chunks []kb.Chunk) []string {
	if len(chunks) == 0 {
		return nil
	}
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.ID)
	}
	return out
}

func (a *App) behavior(profile string, r *Resolved) (text, path string, err error) {
	var parts, paths []string
	if profile != "" {
		dir := config.ExpandHome(a.Cfg.Behavior.Dir)
		p := filepath.Join(dir, profile+".md")
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			// Fall back to the embedded behavior profiles shipped in the binary.
			if eb, eerr := pack.FS.ReadFile("agents/behavior/" + profile + ".md"); eerr == nil {
				b, p = eb, "agents/behavior/"+profile+".md"
			} else {
				return "", "", fmt.Errorf("behavior profile %q: %w", profile, rerr)
			}
		}
		parts = append(parts, stripFrontmatter(string(b)))
		paths = append(paths, p)
	}
	// Explicitly imported repository rules (kintsugi rules import --apply) are
	// operator-approved and become part of the behavior layer.
	if inherited, ipath := rules.LoadInherited(a.Workspace); inherited != "" {
		parts = append(parts, "# Imported repository rules (operator-approved)\n\n"+inherited)
		paths = append(paths, ipath)
	}
	// Operator-authored AGENTS.md, resolved in three tiers (I-80): global, then
	// the document keyed to *this model*, then the workspace. Most specific
	// last, so a per-model instruction about this model's behaviour wins over a
	// project-wide one — which is the point: a 9B local model and a 400B cloud
	// model need different instructions for the same repo.
	if t, p := a.agentsMD(r); t != "" {
		parts = append(parts, t)
		paths = append(paths, p)
	}
	if len(parts) == 0 {
		return "", "", nil
	}
	return strings.Join(parts, "\n\n"), strings.Join(paths, " + "), nil
}

// AgentsMD returns the resolved operator AGENTS.md text and its source path(s)
// for the currently resolved model, most-specific tier last.
func (a *App) AgentsMD() (text, path string) {
	return a.agentsMD(a.resolved)
}

// AgentsMDPath returns just the source path(s), or "" when none is loaded.
func (a *App) AgentsMDPath() string {
	_, p := a.agentsMD(a.resolved)
	return p
}

// agentsMD resolves the three AGENTS.md tiers for one model (I-80): the global
// document, the document keyed to that model, and the workspace document. A
// nil Resolved (before a model is bound) resolves the global and workspace
// tiers only — the model tier needs a model to key on.
//
// These are operator-owned instructions, injected unconditionally: the operator
// wrote them for this workspace or this model, so they are neither imported nor
// gated.
func (a *App) agentsMD(r *Resolved) (text, path string) {
	var tiers []agentsctx.Selected

	global := filepath.Join(filepath.Dir(config.GlobalPath()), "AGENTS.md")
	if b, err := os.ReadFile(global); err == nil && strings.TrimSpace(string(b)) != "" {
		tiers = append(tiers, agentsctx.Selected{
			Tier: agentsctx.TierGlobal, Path: global, Body: strings.TrimSpace(string(b)),
		})
	}

	docs, warns, err := agentsctx.LoadDir(agentsctx.Dir(a.Workspace))
	if err == nil && len(docs) > 0 && r != nil {
		if sel, serr := agentsctx.Select(docs, a.modelDocOverride, r.Model.Family, r.Model.ID, r.Capabilities()); serr == nil {
			tiers = append(tiers, sel)
		}
	}
	a.modelDocWarnings = warns
	a.modelDocs = docs

	for _, name := range []string{"AGENTS.md", "AGENT.md"} {
		p := filepath.Join(a.Workspace, name)
		b, rerr := os.ReadFile(p)
		if rerr != nil || strings.TrimSpace(string(b)) == "" {
			continue
		}
		tiers = append(tiers, agentsctx.Selected{
			Tier: agentsctx.TierProject, Path: p, Body: strings.TrimSpace(string(b)),
		})
		break
	}

	text, labels := agentsctx.Compose(tiers...)
	return text, strings.Join(labels, " + ")
}

// ModelDocs returns the model-tier context documents in the workspace, the
// warnings from loading them, and the one selected for the resolved model. The
// TUI uses this to list and edit them.
func (a *App) ModelDocs() (docs []agentsctx.Doc, warns []string, selected agentsctx.Selected) {
	docs, warns, err := agentsctx.LoadDir(agentsctx.Dir(a.Workspace))
	if err != nil {
		return nil, append(warns, err.Error()), agentsctx.Selected{Absent: true}
	}
	if a.resolved == nil {
		return docs, warns, agentsctx.Selected{Tier: agentsctx.TierModel, Absent: true}
	}
	sel, serr := agentsctx.Select(docs, a.modelDocOverride, a.resolved.Model.Family, a.resolved.Model.ID, a.resolved.Capabilities())
	if serr != nil {
		warns = append(warns, serr.Error())
		sel = agentsctx.Selected{Tier: agentsctx.TierModel, Absent: true}
	}
	return docs, warns, sel
}

// ModelDocPath is where a model-tier document for a name lives.
func (a *App) ModelDocPath(name string) string {
	return filepath.Join(agentsctx.Dir(a.Workspace), strings.TrimSuffix(name, ".md")+".md")
}

// ResolvedModel is the model the current prompt was compiled for, or nil before
// the first compile. The TUI uses it to key a new model-context document.
func (a *App) ResolvedModel() *Resolved { return a.resolved }

// AgentsTiers returns the AGENTS.md tiers that reach the prompt right now, most
// specific last. It is the honest view for the TUI: it reports what the prompt
// carries, not what the match rules would say in the abstract.
func (a *App) AgentsTiers() []agentsctx.Selected {
	text, _ := a.agentsMD(a.resolved)
	if text == "" {
		return nil
	}
	var tiers []agentsctx.Selected
	global := filepath.Join(filepath.Dir(config.GlobalPath()), "AGENTS.md")
	if b, err := os.ReadFile(global); err == nil && strings.TrimSpace(string(b)) != "" {
		tiers = append(tiers, agentsctx.Selected{Tier: agentsctx.TierGlobal, Path: global, Body: strings.TrimSpace(string(b))})
	}
	if a.resolved != nil {
		docs, _, _ := a.ModelDocs()
		if sel, err := agentsctx.Select(docs, a.modelDocOverride, a.resolved.Model.Family, a.resolved.Model.ID, a.resolved.Capabilities()); err == nil && !sel.Absent {
			tiers = append(tiers, sel)
		}
	}
	for _, name := range []string{"AGENTS.md", "AGENT.md"} {
		p := filepath.Join(a.Workspace, name)
		b, err := os.ReadFile(p)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			continue
		}
		tiers = append(tiers, agentsctx.Selected{Tier: agentsctx.TierProject, Path: p, Body: strings.TrimSpace(string(b))})
		break
	}
	return tiers
}

// SetModelDocOverride forces a model-tier document by name; empty clears it and
// returns to rule matching. It is the TUI's "use this document" action.
func (a *App) SetModelDocOverride(name string) {
	a.modelDocOverride = strings.TrimSpace(name)
}

func (a *App) context() (text, path string) {
	if !a.Cfg.Context.Enabled {
		return "", ""
	}
	rel := a.Cfg.Context.Path
	if rel == "" {
		rel = "CONTEXT.md"
	}
	path = rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.Workspace, rel)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	t := strings.TrimSpace(string(b))

	// Soft context budget: drop whole trailing sections, never mid-sentence.
	limitTokens := a.Cfg.Context.InjectBudgetTokens
	if limitTokens > 0 {
		limitBytes := limitTokens * 4
		if len(t) > limitBytes {
			cut := t[:limitBytes]
			if idx := strings.LastIndex(cut, "\n## "); idx > 0 {
				cut = cut[:idx]
			} else if idx := strings.LastIndex(cut, "\n"); idx > 0 {
				cut = cut[:idx]
			}
			t = cut + "\n\n…[context truncated to budget]"
		}
	}
	return t, path
}

// scopeSourcePath names the sources the enforced scope rule set was merged
// from, so the manifest's scope layer carries a path an operator can follow.
// It mirrors scope.FromWorkspace exactly: the [scope] block of the loaded
// config file(s), plus the CONTEXT.md "Scope" section when the context layer is
// enabled and that section yields rules. A source that declared nothing is not
// named, so the path never claims a file the rules did not come from.
func (a *App) scopeSourcePath() string {
	var parts []string
	if !scope.FromConfig(a.Cfg).Empty() {
		switch len(a.ConfigPaths) {
		case 0:
			parts = append(parts, "config [scope]")
		default:
			parts = append(parts, strings.Join(a.ConfigPaths, " + ")+" [scope]")
		}
	}
	if !a.Cfg.Context.Enabled || strings.TrimSpace(a.Cfg.Context.Path) == "" {
		return strings.Join(parts, " + ")
	}
	if cr, err := scope.FromContextFile(a.ContextPath()); err == nil && !cr.Empty() {
		parts = append(parts, a.ContextPath())
	}
	return strings.Join(parts, " + ")
}

// SkillsDir is the absolute directory scanned for progressive-disclosure
// skills.
func (a *App) SkillsDir() string {
	return skills.Dir(a.Workspace, a.Cfg.Skills.Dir)
}

// Skills loads the current workspace skill set. Compilation and the skill tool
// both call this, so an edited skill is visible on the next turn.
func (a *App) Skills() []skills.Skill {
	return skills.Load(a.Workspace, a.SkillsDir())
}

func (a *App) skillsIndex() (text, path string) {
	list := a.Skills()
	if len(list) == 0 {
		return "", ""
	}
	return skills.Index(list), a.SkillsDir()
}

func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	rest := s[3:]
	if i := strings.Index(rest, "\n---"); i >= 0 {
		return strings.TrimPrefix(rest[i+4:], "\n")
	}
	return s
}

// PersonaSummary is used by `kintsugi pack list`.
type PersonaSummary struct {
	Name     string
	Path     string
	Family   []string
	IDGlob   []string
	Caps     []string
	Inherits []string
	Triggers int
}

func (a *App) Personas() []PersonaSummary {
	if a.Pack == nil {
		return nil
	}
	var out []PersonaSummary
	for _, name := range a.Pack.Names() {
		p, _ := a.Pack.Get(name)
		if p == nil {
			continue
		}
		out = append(out, PersonaSummary{
			Name: p.Name, Path: p.Path, Family: p.Match.Family,
			IDGlob: p.Match.IDGlob, Caps: p.Match.Capabilities,
			Inherits: p.Inherits, Triggers: len(p.Triggers.Known),
		})
	}
	return out
}

// ContextPath returns the absolute path of the workspace CONTEXT.md.
func (a *App) ContextPath() string {
	rel := a.Cfg.Context.Path
	if rel == "" {
		rel = "CONTEXT.md"
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(a.Workspace, rel)
}

// LoadContext reads and parses the workspace CONTEXT.md.
func (a *App) LoadContext() (*fctx.Document, []string, error) {
	doc, warns, err := fctx.LoadFile(a.ContextPath())
	if err != nil {
		return nil, nil, fmt.Errorf("context: %w (run `kintsugi context init`)", err)
	}
	return doc, warns, nil
}

type CurateResult struct {
	ProposalPath string
	Proposals    int
	Apply        fctx.ApplyResult
}

// Curate runs one curator pass over a transcript and writes the proposal file.
// Auto-accept classes are applied immediately; gated classes are never
// applied here.
func (a *App) Curate(modelRef, sessionID, transcript string) (*CurateResult, error) {
	if modelRef == "" {
		modelRef = a.Cfg.Context.CuratorModel
	}
	if modelRef == "" {
		return nil, fmt.Errorf("no curator model configured (context.curator_model or --model)")
	}
	doc, _, err := a.LoadContext()
	if err != nil {
		return nil, err
	}
	r, err := a.Resolve(modelRef)
	if err != nil {
		return nil, err
	}

	system, user := fctx.CuratorPrompts(doc.Render(), transcript)
	resp, err := r.Client.Stream(context.Background(), []provider.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, nil, provider.StreamCallbacks{})
	if err != nil {
		return nil, err
	}
	props, err := fctx.ParseProposals(resp.Content)
	if err != nil {
		return nil, err
	}

	f := &fctx.File{Session: sessionID, Model: modelRef, Proposals: props}
	path, err := f.Save(a.Workspace)
	if err != nil {
		return nil, err
	}
	res, err := fctx.Apply(doc, props, fctx.ApplyMode{
		AutoClasses: a.Cfg.Context.AutoAcceptClasses,
		SkillsDir:   a.SkillsDir(),
	})
	if err != nil {
		return nil, err
	}
	// A skill proposal is applied by writing a SKILL.md, not by touching the
	// document, so the document is rewritten only when a section actually moved.
	if docTouched(res.Applied) {
		if err := fctx.WriteDocument(a.ContextPath(), doc); err != nil {
			return nil, err
		}
	}
	return &CurateResult{ProposalPath: path, Proposals: len(props), Apply: res}, nil
}

// docTouched reports whether an applied set changed CONTEXT.md. Skill proposals
// write their own file and leave the document byte-identical.
func docTouched(applied []fctx.Proposal) bool {
	for _, p := range applied {
		if p.Class != fctx.ClassSkill {
			return true
		}
	}
	return false
}

// MemoryStore opens the tier-2 recall index for this workspace.
func (a *App) MemoryStore() (*memory.Store, error) {
	return memory.Open(a.Workspace)
}

// ContextSections returns section bodies from the workspace CONTEXT.md by
// class, used to compose stream-level reminder injections.
func (a *App) ContextSections(classes ...string) map[string]string {
	out := map[string]string{}
	_, path := a.context()
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	doc, _ := fctx.Parse(b)
	if doc == nil {
		return out
	}
	for _, class := range classes {
		if sec, ok := doc.Find(class); ok {
			out[class] = strings.TrimSpace(sec.Body)
		}
	}
	return out
}

// ScopeText is the declared scope a reviewer judges an artifact against: the
// workspace's authorization and domain sections, in that order. It lives here
// rather than in the CLI so every surface — the gate command, the DAG and the
// TUI workbench — asks the same question.
func (a *App) ScopeText() string {
	secs := a.ContextSections("authorization", "domain")
	var b strings.Builder
	for _, class := range []string{"authorization", "domain"} {
		if body := strings.TrimSpace(secs[class]); body != "" {
			b.WriteString(body + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// ContextProvenance describes why a refusal interception had (or lacked) the
// authorization/domain context it would have needed. The refusal evidence card
// (A3) turns "we caught a refusal" into "here is what to fix", which is the
// on-mission version of a plain intercept notice.
func (a *App) ContextProvenance() (authorization, domain string, truncated bool) {
	secs := a.ContextSections(fctx.ClassAuthorization, fctx.ClassDomain)
	authorization = strings.TrimSpace(secs[fctx.ClassAuthorization])
	domain = strings.TrimSpace(secs[fctx.ClassDomain])
	// Truncation is a whole-file soft budget, not a per-section cut, so a
	// non-empty section is still usable; the flag tells the card the file was
	// cut and a trailing section may not have been injected.
	if limit := a.Cfg.Context.InjectBudgetTokens; limit > 0 {
		if b, err := os.ReadFile(a.ContextPath()); err == nil {
			truncated = len(b) > limit*4
		}
	}
	return authorization, domain, truncated
}

// RefusalLexicon loads the operator lexicon override, falling back to the
// workspace-local file and then to the built-in patterns.
func (a *App) RefusalLexicon() (*intercept.Lexicon, error) {
	path := config.ExpandHome(a.Cfg.Intercept.LexiconPath)
	if path == "" {
		candidate := filepath.Join(a.Workspace, ".kintsugi", "refusal_patterns.toml")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		}
	}
	return intercept.Load(path)
}

// EffectiveLexiconPath is the file `kintsugi refusal` edits. It is the configured
// intercept.lexicon_path (expanded), or the workspace-local default the loader
// falls back to, or the workspace-local default when neither exists yet (so a
// first `kintsugi refusal add` creates it there rather than refusing for want of a
// path). It never returns "": an editor always has a target.
//
// The path the loader actually uses and the path this returns are the same
// whenever the operator has not set lexicon_path: both are the workspace-local
// file. When the operator has set lexicon_path, this honours it — the CLI and
// the loader read the same file.
func (a *App) EffectiveLexiconPath() string {
	if p := strings.TrimSpace(config.ExpandHome(a.Cfg.Intercept.LexiconPath)); p != "" {
		return p
	}
	return filepath.Join(a.Workspace, ".kintsugi", "refusal_patterns.toml")
}

// StreamRules loads the operator stream rules (I-70), falling back to the
// workspace-local file. A missing file is not an error: the zero rule set is the
// default path, and a session without rules behaves exactly as it did before
// they existed. A file that is present but partly invalid still returns the
// valid rules alongside the error — one bad entry must not disable the rest.
func (a *App) StreamRules() (*intercept.Rules, error) {
	path := config.ExpandHome(a.Cfg.Intercept.RulesPath)
	if path == "" {
		candidate := filepath.Join(a.Workspace, ".kintsugi", "stream_rules.toml")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		}
	}
	if path == "" {
		return &intercept.Rules{}, nil
	}
	rules, err := intercept.LoadRules(path)
	if err != nil && rules == nil {
		return &intercept.Rules{}, err
	}
	return rules, err
}

// ReminderBuilder composes the injected reminder from the operator's
// authorization/domain sections and the selected persona's trigger map.
func (a *App) ReminderBuilder(comp *prompt.Compiled) func(level int, hit intercept.Hit) string {
	secs := a.ContextSections(fctx.ClassAuthorization, fctx.ClassDomain)

	var triggers []struct {
		class string
		line  string
	}
	if comp != nil && comp.Persona != nil && comp.Persona.Persona != nil {
		for _, t := range comp.Persona.Persona.Triggers.Known {
			if t.Term == "" || t.Instead == "" {
				continue
			}
			triggers = append(triggers, struct {
				class string
				line  string
			}{t.Class, t.Term + " → " + t.Instead})
		}
	}

	return func(level int, hit intercept.Hit) string {
		var rephr []string
		for _, t := range triggers {
			if t.class == "" || t.class == hit.Class {
				rephr = append(rephr, t.line)
			}
		}
		return intercept.BuildReminder(level, hit, secs[fctx.ClassAuthorization], secs[fctx.ClassDomain], rephr)
	}
}

// FrameText is the one-shot authorization frame used by `kintsugi run --frame`
// and the TUI /frame command. The operator attestation, when the workspace has
// one, follows the authorization/domain text; without one the output is exactly
// what it was before continuity existed.
func (a *App) FrameText() string {
	secs := a.ContextSections(fctx.ClassAuthorization, fctx.ClassDomain)
	var b strings.Builder
	for _, class := range []string{fctx.ClassAuthorization, fctx.ClassDomain} {
		body := strings.TrimSpace(secs[class])
		if body != "" {
			b.WriteString(body + "\n\n")
		}
	}
	body := strings.TrimRight(b.String(), "\n")
	att := a.AttestationBlock()
	if body == "" && att == "" {
		return ""
	}
	const prefix = "Operator frame (one-shot authorization context):\n\n"
	if att == "" {
		return prefix + body
	}
	if body == "" {
		return prefix + att
	}
	return prefix + body + "\n\n" + att
}

// attestationText is the rendered operator attestation for the workspace, or ""
// when the workspace has never been attested (a normal state, not a failure) or
// the file cannot be read. The render carries no timestamps of its own, which is
// what keeps the compiled prefix byte-stable.
func (a *App) attestationText() string {
	att, err := fctx.LoadAttestation(a.Workspace)
	if err != nil || att == nil {
		return ""
	}
	return strings.TrimRight(fctx.RenderAttestation(att), "\n")
}

// AttestationBlock is attestationText plus a stale warning line when the review
// has lapsed or an expiry cannot be trusted. The warning is deliberately kept
// out of the compiled layer (see attestationText): a wall-clock sentence would
// change the cached prefix, and a stale review is an operator prompt, not model
// context.
func (a *App) AttestationBlock() string {
	block := a.attestationText()
	if block == "" {
		return ""
	}
	att, err := fctx.LoadAttestation(a.Workspace)
	if err != nil || att == nil {
		return block
	}
	if !att.Stale(time.Now()) {
		return block
	}
	return block + "\n\nStale: this attestation predates the current frame or its review-by date has passed. " +
		"It is still context, not a permission grant — re-review the authorization and scope sections."
}

// AdvisorEnabled resolves the profile default.
func (a *App) AdvisorEnabled() bool {
	return a.Cfg.Advisor.Active(a.Cfg.Workspace.Profile)
}

// AdvisorFor builds the secondary reviewer. The advisor model resolution is:
// the configured advisor model, then the model bound to the opsec role, then
// the working model. The advisor reviews the same work the worker does, so it
// defaults to the model the operator bound to the reviewer role rather than
// to whatever model happens to be driving the session.
func (a *App) AdvisorFor(base *Resolved) (*advisor.Advisor, error) {
	ref := strings.TrimSpace(a.Cfg.Advisor.Model)
	if ref == "" {
		ref = strings.TrimSpace(a.Cfg.Roles.Model("opsec"))
	}
	if ref == "" && base != nil {
		ref = base.ProviderName + "/" + base.Model.ID
	}
	r, err := a.Resolve(ref)
	if err != nil {
		return nil, err
	}
	return advisor.New(r.Client, a.Cfg.Advisor.MaxTokens), nil
}

// GateReviewer builds the read-only reviewer. The gate model resolution is:
// the configured gate model, then the model bound to the opsec role, then the
// default model. A gate that reviews opsec must run on the model the operator
// bound to that role, not on the working model.
//
// The reviewer is also handed the chunk ids of the knowledge bases bound to the
// reviewing role, so a finding can cite the material that informed it (WP-1.6).
// With no binding the hook reports nothing and the reviewer prompt is exactly
// what it was before: no base, no citation machinery.
func (a *App) GateReviewer() (*gate.Reviewer, error) {
	ref := strings.TrimSpace(a.Cfg.Gates.Model)
	if ref == "" {
		ref = strings.TrimSpace(a.Cfg.Roles.Model("opsec"))
	}
	if ref == "" {
		ref = strings.TrimSpace(a.Cfg.Core.DefaultModel)
	}
	r, err := a.Resolve(ref)
	if err != nil {
		return nil, err
	}
	return &gate.Reviewer{Client: r.Client, Knowledge: a.GateKnowledge(gateReviewRole)}, nil
}

// gateReviewRole is the role whose knowledge-base bindings a gate may cite. A
// gate is opsec review work, so it reads the opsec role's bases; a binding with
// no role list applies to every role and is therefore included too.
const gateReviewRole = "opsec"

// kbGateQueryBytes bounds the artifact text used as the retrieval query.
// Ranking wants the intent of the artifact, not all of it.
const kbGateQueryBytes = 4000

// kbGateCitationMax caps the citations offered to a reviewer. A diff that
// matches half a base is not served by twenty chunk ids in the prompt.
const kbGateCitationMax = 16

// GateKnowledge returns the citation hook for a gate reviewer: given the
// artifact under review it reports the chunk references the reviewer may cite,
// as "<base>/<chunk>" — the same form the compiled prompt's kb layer records.
//
// It reuses knowledge(), so the citations come from exactly the bases and
// strategies the agent's own layer-3 prompt would use for this role: a base that
// cannot be loaded or indexed contributes nothing rather than failing the
// review, which is layer 3's best-effort contract.
func (a *App) GateKnowledge(role string) func(artifact string) []string {
	return func(artifact string) []string {
		if len(a.kbBindings(role)) == 0 {
			return nil
		}
		query := artifact
		if len(query) > kbGateQueryBytes {
			query = query[:kbGateQueryBytes]
		}
		_, meta, _ := a.knowledge(role, query)
		return kbCitations(meta, kbGateCitationMax)
	}
}

// kbCitations flattens compiled kb-layer metadata into "<base>/<chunk>"
// references, capped at limit. A layer with no base id degrades to the bare
// chunk id, matching how the prompt records it.
func kbCitations(meta []prompt.KBLayer, limit int) []string {
	if len(meta) == 0 {
		return nil
	}
	var out []string
	for _, layer := range meta {
		id := strings.TrimSpace(layer.ID)
		for _, c := range layer.Chunks {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if id != "" {
				c = id + "/" + c
			}
			out = append(out, c)
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// AttachTools wires the workspace skill directory and configured MCP servers
// into an executor. The returned function closes MCP child processes and is
// safe to call multiple times.
func (a *App) AttachTools(ex *tools.Executor, log func(kind string, v any)) func() {
	if ex == nil {
		return func() {}
	}
	ex.SkillsDir = a.SkillsDir()
	// The two-stage permission classifier (A1). off is the default and implies
	// no classifier; soft/hard are wired from the config knob. The classifier's
	// stage-2 review (when built) is the only caller of a model here.
	ex.ClassifierMode = tools.ClassifierMode(strings.ToLower(a.Cfg.Permissions.ClassifierMode))
	// The background hub (M10-D): task(background=true) spawns into it and
	// agent_output collects from it. It exists only where subagents do, since a
	// detached run is a subagent run that returns early. One hub per session,
	// shared with the front-end that renders the roster.
	if a.Cfg.Agents.Subagents {
		ex.Hub = a.Hub()
	}
	// The lab posture travels with the executor: while a lab is up, host
	// commands are refused and work runs inside the compose project.
	labCfg := lab.FromConfig(a.Cfg.Lab)
	ex.Lab = &labCfg
	// Mods are external processes from the workspace: they start only behind
	// the workspace trust marker, and one broken mod never takes the session
	// down.
	if a.Cfg.Mods.Enabled {
		manifests, err := mods.Load(mods.Dir(a.Workspace, a.Cfg.Mods.Dir))
		if err != nil && log != nil {
			log("mods_error", map[string]any{"error": err.Error()})
		}
		mgr := mods.NewManager(30*time.Second, log)
		if err := mgr.Discover(manifests, mods.Trusted(a.Workspace)); err != nil && log != nil {
			log("mods_error", map[string]any{"error": err.Error()})
		}
		ex.Mods = mgr
	}
	// Knowledge bases the agent may search with kb_search (WP-1.4). Only
	// configured bases are exposed, so the tool can never reach a path the
	// operator did not declare.
	names := make([]string, 0, len(a.Cfg.KBs))
	for name := range a.Cfg.KBs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := a.kbDir(a.Cfg.KBs[name].Path)
		if dir == "" {
			continue
		}
		ex.KBs = append(ex.KBs, tools.KBSource{ID: name, Dir: dir})
	}
	if !a.Cfg.MCP.Enabled || len(a.Cfg.MCP.Servers) == 0 {
		return func() {}
	}
	mgr := mcp.Start(a.Cfg.MCP, log)
	ex.MCP = mgr
	return mgr.Close
}

// ToolPolicy is the configured permission set shared by CLI, TUI and workers.
func (a *App) ToolPolicy() tools.Policy {
	p := a.Cfg.Permissions
	return tools.Policy{
		AllowPaths:    p.AllowPaths,
		DenyPaths:     p.DenyPaths,
		AllowCommands: p.AllowCommands,
		DenyCommands:  p.DenyCommands,
		Yolo:          p.Yolo,
		Mode:          p.Mode,
		Rules:         p.Rules,
	}
}

// Summarizer returns the compaction summarizer (M10.3): the curator model when
// one is configured, otherwise the default model. A nil client means no
// summarizer, and an unsummarized conversation is still a conversation.
func (a *App) Summarizer() agent.Summarizer {
	if a == nil {
		return nil
	}
	ref := strings.TrimSpace(a.Cfg.Context.CuratorModel)
	if ref == "" {
		ref = strings.TrimSpace(a.Cfg.Core.DefaultModel)
	}
	if ref == "" {
		return nil
	}
	r, err := a.Resolve(ref)
	if err != nil || r == nil || r.Client == nil {
		return nil
	}
	return agent.NewSummarizer(r.Client)
}

// HooksRunner builds the lifecycle hook runner (M9.1). Operator hooks always
// run; project hooks — those in the workspace's `.kintsugi/hooks.toml` and
// those in the workspace's own `kintsugi.toml` — are repository-controlled
// and only run behind the trust marker (`kintsugi hooks trust`).
func (a *App) HooksRunner(log func(kind string, v any)) *hooks.Runner {
	if a == nil {
		return nil
	}
	rules := hooks.FromConfig(a.Cfg.Hooks)
	project, trusted, err := hooks.LoadProjectHooks(a.Workspace)
	if err != nil {
		if log != nil {
			log("hook_project_invalid", map[string]any{"error": err.Error()})
		}
		project = nil
	}
	// The workspace's own kintsugi.toml is repository-controlled too: its
	// [hooks] section rides the same trust gate as the project hook file.
	for event, list := range hooks.FromConfig(a.Cfg.WorkspaceHooks) {
		if project == nil {
			project = map[string][]config.HookRule{}
		}
		project[event] = append(project[event], list...)
	}
	if len(rules) == 0 && len(project) == 0 {
		return nil
	}
	r := hooks.New(rules, log)
	r.Dir = a.Workspace
	r.WithProject(project, trusted)
	return r
}

// QuotaStore opens the global quota ledger with configured limits.
func (a *App) QuotaStore() (*quota.Store, error) {
	limits := map[string]quota.Limit{}
	for name, q := range a.Cfg.Quota {
		limits[name] = quota.Limit{LimitTokens: q.LimitTokens, Reset: q.Reset, Note: q.Note}
	}
	return quota.Open("", limits)
}

// RecordUsage adds approximate or exact token counts to the ledger. Failures
// are ignored: quota accounting must never fail a model turn.
func (a *App) RecordUsage(provider string, promptTokens, completionTokens int) {
	st, err := a.QuotaStore()
	if err != nil {
		return
	}
	_ = st.Add(provider, promptTokens+completionTokens)
}

// TaskManager builds the subagent fan-out manager with a parent-stack system
// prompt and the workspace policy (worker deny rules still apply).
func (a *App) TaskManager(base *Resolved, comp *prompt.Compiled, log func(kind string, v any)) (*subagent.Manager, error) {
	if !a.Cfg.Agents.Subagents {
		return nil, fmt.Errorf("subagents are disabled in config")
	}
	timeout := time.Duration(a.Cfg.Agents.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	pol := a.ToolPolicy()
	return &subagent.Manager{Cfg: subagent.Config{
		Workspace:    a.Workspace,
		Client:       base.Client,
		SystemPrompt: comp.Text,
		Policy:       pol,
		Log:          log,
		Max:          a.Cfg.Agents.MaxSubagents,
		Timeout:      timeout,
		WorktreeRoot: a.Cfg.Agents.WorktreeDir,
		MaxLoops:     a.Cfg.Harness.MaxToolLoops,
		Offline:      a.Cfg.Core.Mode == "offline",
	}}, nil
}

// ---- continuity (M8.5) -----------------------------------------------------
//
// These four entry points are the whole integration surface of the context
// ledger and the continuity guard:
//
//   - LedgerStore opens the workspace ledger.
//   - RecallPriorWork renders the prior *accepted* work for a task, for the
//     fresh-session injection point in the agent loop.
//   - PreflightFor decides locally whether the frame is worth injecting before
//     the first task, and builds it without calling a provider.
//   - RecordDrift appends non-recallable drift metadata.
//
// The invariant is the one the ledger and continuity packages state: a refusal
// is never memory. None of these methods takes refusal text, none of them writes
// prose to the ledger, and nothing recallable can be built out of a drift event.

// LedgerDir is the absolute continuity-ledger directory for this workspace. The
// configured value is workspace-relative by default (config.Continuity.LedgerDir,
// ".kintsugi/ledger").
func (a *App) LedgerDir() string {
	return continuity.LedgerDir(a.Workspace, config.ExpandHome(a.Cfg.Continuity.LedgerDir))
}

// LedgerStore opens (creating if needed) the workspace context ledger. Callers
// own the returned store and must Close it; continuity treats every failure as
// best-effort.
func (a *App) LedgerStore() (*ledger.Store, error) {
	// The ledger package derives its paths from a workspace root, so a custom
	// continuity.ledger directory takes effect only when it is the workspace's
	// own .kintsugi/ledger. Anything else falls back to the workspace default
	// rather than silently writing somewhere the operator did not configure.
	ws := strings.TrimSpace(a.Workspace)
	if ws == "" {
		return nil, fmt.Errorf("continuity: no workspace configured for the ledger")
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return nil, fmt.Errorf("continuity: workspace: %w", err)
	}
	if root, ok := ledgerWorkspaceFor(a.LedgerDir()); ok {
		rootAbs, rerr := filepath.Abs(root)
		if rerr == nil && rootAbs == abs {
			ws = root
		}
	}
	return ledger.Open(ws)
}

// ledgerWorkspaceFor maps "<root>/.kintsugi/ledger" back to root.
func ledgerWorkspaceFor(dir string) (string, bool) {
	p := filepath.ToSlash(filepath.Clean(dir))
	suffix := "/" + continuity.DefaultLedgerDir
	if !strings.HasSuffix(p, suffix) {
		return "", false
	}
	root := strings.TrimSuffix(p, suffix)
	if root == "" || root == "." {
		return "", false
	}
	return filepath.FromSlash(root), true
}

// RecallPriorWork returns the rendered "prior accepted work" block for task and
// the ledger ids it cites. It is a no-op when continuity is disabled, when the
// task is empty, or when nothing matches. The block is positive material only:
// Search returns recallable active entries, so superseded work and drift events
// cannot appear in it.
func (a *App) RecallPriorWork(task string) (block string, matched []string, err error) {
	if !a.Cfg.Continuity.Enabled {
		return "", nil, nil
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return "", nil, nil
	}
	st, err := a.LedgerStore()
	if err != nil {
		return "", nil, err
	}
	defer st.Close()

	entries, err := st.Search(task, a.Cfg.Continuity.PriorWorkTopK, a.Cfg.Continuity.PriorWorkBudgetTokens)
	if err != nil {
		return "", nil, err
	}
	items := continuity.PriorWorkFromEntries(entries)
	if len(items) == 0 {
		return "", nil, nil
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return continuity.RenderPriorWork(items, a.Cfg.Continuity.PriorWorkBudgetTokens), ids, nil
}

// PreflightFor scores a first task against the selected persona's approved
// trigger terms and the historical class counters, and returns the frame block
// when the score warrants injecting it. It never calls a provider and never
// looks at a transcript: the inputs are operator-approved terms and integers.
//
// A low-risk task returns ("", risk) so the caller can show the score without
// injecting anything.
func (a *App) PreflightFor(r *Resolved, task string) (block string, risk continuity.Risk) {
	if !a.Cfg.Continuity.Enabled || !a.Cfg.Continuity.Preflight {
		return "", continuity.Risk{}
	}
	triggers := a.personaTriggers(r)
	terms := make([]string, 0, len(triggers))
	for _, t := range triggers {
		terms = append(terms, t.Term)
	}
	risk = continuity.ScoreTask(task, terms, a.classCounts(r))
	if !continuity.ShouldPreflight(risk, continuity.DefaultPreflightThreshold) {
		return "", risk
	}

	var parts []string
	if frame := a.FrameText(); frame != "" {
		parts = append(parts, frame)
	}
	if section := preflightRephrasings(triggers, risk.Classes); section != "" {
		parts = append(parts, section)
	}
	return strings.Join(parts, "\n\n"), risk
}

// personaTriggers returns the selected persona's operator-approved trigger map
// for a resolved model. An unresolvable selection yields no terms: preflight is
// best-effort and must never fail a task.
func (a *App) personaTriggers(r *Resolved) []persona.Trigger {
	if a.Pack == nil || r == nil {
		return nil
	}
	caps := r.Model.Capabilities
	if r.Adapter != nil {
		caps = r.Capabilities()
	}
	sel, err := a.Pack.Select("", r.Model.Family, r.Model.ID, caps)
	if err != nil || sel == nil || sel.Persona == nil {
		return nil
	}
	return sel.Persona.Triggers.Known
}

// classCounts aggregates refusal telemetry by class for the preflight scorer.
// Counters are filtered to the model's family when the family can be inferred
// from the recorded model ref; when it cannot (or nothing matches, which means
// the family naming differs from the recorded refs), every counter is used.
//
// A class is a label and the count is an integer: the phrase that produced it
// was dropped at the telemetry boundary, so nothing here can be replayed.
func (a *App) classCounts(r *Resolved) map[string]int {
	stats, err := a.refusalStatsFor()
	if err != nil || len(stats) == 0 {
		return nil
	}
	family, modelID := "", ""
	if r != nil {
		family, modelID = r.Model.Family, r.Model.ID
	}

	matched := map[string]int{}
	all := map[string]int{}
	for _, s := range stats {
		class := strings.TrimSpace(s.Class)
		if class == "" {
			continue
		}
		if s.Count > all[class] {
			all[class] = s.Count
		}
		if statMatchesModel(s.Model, family, modelID) && s.Count > matched[class] {
			matched[class] = s.Count
		}
	}
	if len(matched) > 0 {
		return matched
	}
	return all
}

// statMatchesModel reports whether a recorded model ref belongs to the resolved
// model. An empty family and model id mean "cannot infer", which matches
// everything (the caller then uses all counters).
func statMatchesModel(model, family, modelID string) bool {
	if strings.TrimSpace(family) == "" && strings.TrimSpace(modelID) == "" {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || m == "unknown" {
		return false
	}
	if id := strings.ToLower(strings.TrimSpace(modelID)); id != "" {
		if m == id || strings.HasSuffix(m, "/"+id) {
			return true
		}
	}
	if f := strings.ToLower(strings.TrimSpace(family)); f != "" && strings.Contains(m, f) {
		return true
	}
	return false
}

// refusalStatsFor reads the refusal counters through the test seam, or from the
// live session directory.
func (a *App) refusalStatsFor() ([]session.RefusalStat, error) {
	if a.refusalStats != nil {
		return a.refusalStats()
	}
	dir, err := session.Dir()
	if err != nil {
		return nil, err
	}
	return session.RefusalStats(dir)
}

// preflightRephrasings renders the deterministic "term → instead" section for
// the persona's trigger map. classes is the class list the scorer flagged: when
// it is non-empty the section keeps those classes (plus unclassified entries),
// and when it is empty every trigger is listed. Output order is sorted, so the
// same persona and the same score always produce the same bytes.
func preflightRephrasings(triggers []persona.Trigger, classes []string) string {
	want := map[string]bool{}
	for _, c := range classes {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			want[c] = true
		}
	}

	type line struct{ class, term, instead string }
	var lines []line
	seen := map[line]bool{}
	for _, t := range triggers {
		term := flattenField(t.Term)
		instead := flattenField(t.Instead)
		if term == "" || instead == "" {
			continue
		}
		class := flattenField(t.Class)
		if len(want) > 0 && class != "" && !want[strings.ToLower(class)] {
			continue
		}
		l := line{class: class, term: term, instead: instead}
		if seen[l] {
			continue
		}
		seen[l] = true
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].class != lines[j].class {
			return lines[i].class < lines[j].class
		}
		if lines[i].term != lines[j].term {
			return lines[i].term < lines[j].term
		}
		return lines[i].instead < lines[j].instead
	})

	var b strings.Builder
	b.WriteString("### Trigger rephrasings for this persona\n\n")
	b.WriteString("Operator-recorded terms that trip this model's filters, with the honest rephrasing to use instead:\n\n")
	for _, l := range lines {
		if l.class != "" {
			fmt.Fprintf(&b, "- %s → %s (class: %s)\n", l.term, l.instead, l.class)
			continue
		}
		fmt.Fprintf(&b, "- %s → %s\n", l.term, l.instead)
	}
	return strings.TrimRight(b.String(), "\n")
}

// flattenField collapses whitespace so one trigger entry cannot inject list
// structure or a heading into the preflight section.
func flattenField(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// RecordDrift appends one metadata-only drift event to the workspace ledger: a
// task hash, the matched prior-work ids, the class and the outcome. There is no
// text field and the ledger rejects prose on drift entries, so a refusal can
// never become recallable memory. Errors are returned for the caller to log;
// they never fail a turn.
func (a *App) RecordDrift(task, class, outcome string, matched []string) error {
	if !a.Cfg.Continuity.Enabled {
		return nil
	}
	st, err := a.LedgerStore()
	if err != nil {
		return err
	}
	defer st.Close()

	_, err = st.Append(ledger.Entry{
		Kind:       ledger.KindDrift,
		TaskHash:   ledger.HashTask(task),
		MatchedIDs: matched,
		Class:      strings.TrimSpace(class),
		Outcome:    strings.TrimSpace(outcome),
	})
	return err
}

// tasteBlock returns the operator taste block for injection into the prompt
// stack. Returns empty string if taste is disabled or no bullets exist.
func (a *App) tasteBlock() string {
	block, ok := taste.Block(a.Workspace)
	if !ok {
		return ""
	}
	return block
}

// MentalModel returns the session's orientation digest: the work this workspace
// has already accepted plus its newest lessons, bounded and labelled by the
// renderer. It is empty when there is nothing accepted yet.
//
// A refusal is never part of it. The digest reads the ledger through
// memory.DigestFromEntries, which keeps positive kinds only — drift events are
// metadata and are skipped, and a refusal is not a ledger kind at all.
func (a *App) MentalModel() string {
	// Get ledger entries (accepted work)
	st, err := a.LedgerStore()
	if err != nil {
		return ""
	}
	defer st.Close()

	entries, err := st.List("")
	if err != nil || len(entries) == 0 {
		return ""
	}

	// Get lesson digest
	lessons := memory.LessonDigest(a.Workspace, 5)
	if len(lessons) == 0 && len(entries) == 0 {
		return ""
	}

	return memory.DigestFromEntries(entries, lessons, 200)
}
