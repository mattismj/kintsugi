package harness

import (
	"fmt"
	"strings"
	"sync"
	"time"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/mcp"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/subagent"
)

// TaskManagerWithRoles is TaskManager plus per-role worker models (WP-4.1) and
// MCP propagation into workers (WP-4.5). It is additive: app.TaskManager is
// untouched and stays the plain, single-model fan-out.
//
// base is the resolved parent model (identity, fallbacks and — importantly —
// the client every worker falls back to). comp is the parent compiled prompt
// stack used as house rules. mgr is the already-started MCP manager the parent
// is using: pass the parent's ex.MCP after App.AttachTools so workers share one
// set of MCP child processes instead of starting a second one. A nil mgr
// disables MCP inside workers only; the parent keeps its tools. Ownership never
// moves: the parent still closes mgr exactly once via AttachTools' close func.
//
// Role routing resolves through the [roles] table. A role with no configured
// model resolves to base.Client rather than core.default_model: an explicit
// --model on the parent must keep applying to workers until the operator binds
// that role to a different model.
//
// This entry point carries no per-worker context bundle (M8.5.5); use
// TaskManagerWithBundle when workers should inherit the project frame.
func (a *App) TaskManagerWithRoles(base *Resolved, comp *prompt.Compiled, mgr *mcp.Manager, log func(kind string, v any)) (*subagent.Manager, error) {
	return a.taskManager(base, comp, mgr, nil, log)
}

// TaskManagerWithBundle is TaskManagerWithRoles plus per-worker context bundles
// (M8.5.5): the same construction, with subagent.Config.BundleFor set, so every
// worker receives its own authorization/domain/scope frame, prior accepted work,
// role-bound knowledge and handoff.
//
// bundleFor is called once per worker with the task title and the resolved model
// role. A nil bundleFor means a.BuildWorkerBundle, which fills the frame from the
// workspace CONTEXT.md and (when the prior-work hook is installed) attaches the
// relevant accepted work. The manager still bounds and redacts the rendered
// bundle, so a bundle can never widen a worker's permissions or leak a secret.
func (a *App) TaskManagerWithBundle(base *Resolved, comp *prompt.Compiled, mgr *mcp.Manager, bundleFor func(task, role string) subagent.ContextBundle, log func(kind string, v any)) (*subagent.Manager, error) {
	if bundleFor == nil {
		bundleFor = a.BuildWorkerBundle
	}
	return a.taskManager(base, comp, mgr, bundleFor, log)
}

// taskManager is the single construction path behind TaskManagerWithRoles and
// TaskManagerWithBundle. bundleFor nil means "no bundles", which keeps the
// pre-M8.5.5 worker prompt byte-identical.
func (a *App) taskManager(base *Resolved, comp *prompt.Compiled, mgr *mcp.Manager, bundleFor func(task, role string) subagent.ContextBundle, log func(kind string, v any)) (*subagent.Manager, error) {
	if !a.Cfg.Agents.Subagents {
		return nil, fmt.Errorf("subagents are disabled in config")
	}
	if base == nil || base.Client == nil {
		return nil, fmt.Errorf("subagents need a resolved parent model")
	}
	if comp == nil {
		return nil, fmt.Errorf("subagents need a compiled parent prompt")
	}
	timeout := time.Duration(a.Cfg.Agents.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	pol := a.ToolPolicy()
	cfg := subagent.Config{
		Workspace:     a.Workspace,
		Client:        base.Client,
		ResolveClient: a.roleClient(base),
		MCP:           mgr,
		SystemPrompt:  comp.Text,
		Hooks:         a.HooksRunner(log),
		SessionID:     a.SessionID,
		Policy:        pol,
		Log:           log,
		Max:           a.Cfg.Agents.MaxSubagents,
		Timeout:       timeout,
		WorktreeRoot:  a.Cfg.Agents.WorktreeDir,
		MaxLoops:      a.Cfg.Harness.MaxToolLoops,
		Offline:       a.Cfg.Core.Mode == "offline",
	}
	if bundleFor != nil {
		cfg.BundleFor = bundleFor
		cfg.BundleTokens = subagent.BundleTokensDefault
	}
	return &subagent.Manager{Cfg: cfg}, nil
}

// WorkerPriorWorkFunc returns the prior *accepted* work relevant to one worker
// task. It is the wiring point for the continuity ledger: it must return
// acceptances only. Refusal turns, rejections and drift events are telemetry and
// are never recallable, so they must never reach a bundle (DESIGN §16.23).
type WorkerPriorWorkFunc func(task string) []subagent.PriorWorkRef

// workerPriorHookMu guards the package-level prior-work hook. The hook is
// package-level rather than an App field on purpose: the coordinator installs
// the ledger-backed source once, without every App construction (CLI, TUI, RPC)
// having to know about it, and without touching App's shape.
var (
	workerPriorHookMu sync.RWMutex
	workerPriorHook   WorkerPriorWorkFunc
)

// SetWorkerPriorWork installs the prior-work source used by BuildWorkerBundle
// and returns the previous one, so a caller (or a test) can restore it. A nil fn
// clears the hook, which leaves bundles with an empty PriorWork section.
func SetWorkerPriorWork(fn WorkerPriorWorkFunc) WorkerPriorWorkFunc {
	workerPriorHookMu.Lock()
	defer workerPriorHookMu.Unlock()
	prev := workerPriorHook
	workerPriorHook = fn
	return prev
}

// WorkerPriorWork returns the installed hook, or nil when none is installed.
func WorkerPriorWork() WorkerPriorWorkFunc {
	workerPriorHookMu.RLock()
	defer workerPriorHookMu.RUnlock()
	return workerPriorHook
}

// SetWorkerPriorWork (a method on App) installs the prior-work source on this
// App only and returns the previous App-level one (nil when unset). It takes
// precedence over the package-level kintsugi.SetWorkerPriorWork, which stays as the
// process-wide fallback for callers that build Apps elsewhere.
func (a *App) SetWorkerPriorWork(fn WorkerPriorWorkFunc) WorkerPriorWorkFunc {
	if a == nil {
		return nil
	}
	prev := a.workerPriorWork
	a.workerPriorWork = fn
	return prev
}

// workerPriorSource resolves the prior-work source for this App: the App-level
// hook wins, then the package-level hook, then nothing.
func (a *App) workerPriorSource() WorkerPriorWorkFunc {
	if a != nil && a.workerPriorWork != nil {
		return a.workerPriorWork
	}
	return WorkerPriorWork()
}

// maxBundlePriorWork bounds the prior-work entries handed to one worker. It
// mirrors subagent.BundleMaxEntries and DESIGN §16.23's ≤5-entry retrieval bound;
// RenderBundle enforces the same cap on the rendered text.
const maxBundlePriorWork = 5

// BuildWorkerBundle fills one worker's context bundle (M8.5.5). It is
// deliberately dependency-free: the authorization, domain and scope frame come
// from the workspace CONTEXT.md, prior work comes from the installed
// WorkerPriorWork hook (the coordinator wires the ledger later), and role-bound
// knowledge excerpts are left to that same coordinator. Everything it returns is
// bounded and redacted by subagent.RenderBundle before a worker sees it.
//
// task is the task title as it appears in the task tool; role is the resolved
// model role. Bundles are context, never policy: nothing here can widen a
// worker's permissions, and an empty bundle means "nothing to hand over", never
// "the operator refused".
func (a *App) BuildWorkerBundle(task, role string) subagent.ContextBundle {
	secs := a.ContextSections(fctx.ClassAuthorization, fctx.ClassDomain, fctx.ClassScope)
	bundle := subagent.ContextBundle{
		Authorization: strings.TrimSpace(secs[fctx.ClassAuthorization]),
		Domain:        strings.TrimSpace(secs[fctx.ClassDomain]),
		Scope:         strings.TrimSpace(secs[fctx.ClassScope]),
	}
	if fn := a.workerPriorSource(); fn != nil {
		refs := fn(task)
		if len(refs) > maxBundlePriorWork {
			refs = refs[:maxBundlePriorWork]
		}
		bundle.PriorWork = refs
	}
	// role selects which knowledge bases apply; KB excerpts stay empty here until
	// the coordinator binds role→base retrieval (M8.5.6).
	return bundle
}

// roleClient builds the per-role resolver handed to subagent.Config. Unbound
// roles and roles bound to the parent's own model ref reuse base.Client, so a
// fan-out never constructs a duplicate client (and never loses the parent's
// fallback chain). Resolution errors are returned as-is; the subagent manager
// logs subagent_role_fallback and degrades to the parent client.
func (a *App) roleClient(base *Resolved) func(role string) (*provider.Client, error) {
	parentRef := ""
	if base != nil {
		parentRef = base.ProviderName + "/" + base.Model.ID
	}
	return func(role string) (*provider.Client, error) {
		ref := strings.TrimSpace(a.Cfg.Roles.Model(role))
		if ref == "" || (parentRef != "" && ref == parentRef) {
			return base.Client, nil
		}
		r, err := a.ResolveRole(role)
		if err != nil {
			return nil, fmt.Errorf("role %q (%s): %w", role, ref, err)
		}
		return r.Client, nil
	}
}
