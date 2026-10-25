// Architect/editor split (M10.2, DESIGN §16.7).
//
// `kintsugi run --architect` is two ordinary turns: a read-only planning turn on
// the architect model, then the edit turn on the coder model with the plan
// injected as context. Both go through the same agent loop, so interception,
// continuity, hooks and permissions apply to both, and the plan itself is a
// real artifact under .kintsugi/plans/.
//
// The split is a composition of the roles the harness already has, not a new
// subsystem: the architect model is the one bound to the opsec role — a
// reviewer model — unless the operator names another one, and the coder model
// is the model the run would have used anyway. A strong reasoning model
// plans; a cheap fast model edits.
//
// This file also carries the harness compaction settings (M10.3): the
// configuration package belongs to another workstream, so the two harness keys
// are read here from the very files the configuration loader read, in the
// same precedence order.
package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"kintsugi/internal/agent"
	"kintsugi/internal/plan"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/tools"
)

// TurnRunner is one agent turn. The agent loop satisfies it, and so does the
// compaction runner that wraps it, so the edit phase keeps whatever the caller
// wired around it.
type TurnRunner interface {
	RunConversation(ctx context.Context, systemPrompt string, history []provider.Message, task string) (string, []provider.Message, error)
}

// ArchitectOptions is one architect/editor run.
type ArchitectOptions struct {
	// Task is the operator's request. Both phases work on it.
	Task string
	// ArchitectModel names the planner model (provider/model). Empty: the
	// model bound to the opsec role, else the default model.
	ArchitectModel string
	// Coder is the model that edits: the model the run would have used
	// anyway.
	Coder *Resolved
	// CoderLoop runs the edit phase.
	CoderLoop TurnRunner
	// Wiring is the loop the coder runs on. The plan phase inherits its
	// wiring — interception, continuity, hooks — so both phases are
	// ordinary turns. It may be nil.
	Wiring *agent.Loop
	// Compiled is the coder's compiled prompt stack.
	Compiled *prompt.Compiled
	// History is the conversation the run continues.
	History []provider.Message
	// Persona and Profile override the compiled stacks.
	Persona string
	Profile string
	// Note prints an operator-facing line: the phase boundaries and the
	// plan's path.
	Note func(format string, a ...any)
	// OnTool reports the plan phase's tool calls.
	OnTool func(call provider.ToolCall, res tools.Result)
}

// ArchitectResult reports what the two phases did.
type ArchitectResult struct {
	// ArchitectModel and CoderModel are the two models, as provider/model.
	ArchitectModel string
	CoderModel     string
	// PlanPath is where the plan was written ("" when there is no plan).
	PlanPath string
	// Plan is the plan the architect produced ("" when there is none).
	Plan string
	// PlanNote explains what happened to the plan phase: empty when a plan
	// was produced, the explicit "no plan produced" note when it was not.
	PlanNote string
	// Final is the coder's answer.
	Final string
	// History is the conversation after the edit phase.
	History []provider.Message
}

// approvedPlanHeading opens the block the architect's plan is injected as.
// The plan is context, not a system instruction, so it rides in its own
// system message behind the compiled prompt.
const approvedPlanHeading = "## Approved plan (from the architect)\n"

// noPlanNote is what the edit phase gets when the architect produced
// nothing usable: an explicit note, never an invented plan.
const noPlanNote = "(no plan produced — the architect phase returned nothing; plan the work yourself from the task)"

// architectInstruction is the brief the planning turn runs under. It names
// the tools the turn may use and the shape of the document it must produce.
const architectInstruction = `You are the architect: you plan the work, you do not perform it. This turn is READ-ONLY.

Rules:
- Use only the read, list, glob and grep tools. Do not edit, write, patch or run commands.
- Do not change anything in the workspace.
- Investigate before you propose: read the files the task touches.

Write a Markdown plan and nothing else:
- open with a level-one heading: "# <short title>"
- then the steps, each one concrete and each naming the files it touches
- then a short "## Risks" section
- no preamble and no closing remarks.`

// ResolveArchitect resolves the planner model: the explicit ref when the
// operator gave one, else the model bound to the opsec role, else the default
// model. The opsec role is the reviewer role of this harness, so an operator
// who bound a strong reasoning model there gets it as the architect for free.
func (a *App) ResolveArchitect(ref string) (*Resolved, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = strings.TrimSpace(a.Cfg.Roles.Model("opsec"))
	}
	if ref == "" {
		ref = strings.TrimSpace(a.Cfg.Core.DefaultModel)
	}
	if ref == "" {
		return nil, fmt.Errorf("architect: no model to plan with (set roles.opsec, core.default_model, or pass --architect-model)")
	}
	r, err := a.Resolve(ref)
	if err != nil {
		return nil, fmt.Errorf("architect: %w", err)
	}
	return r, nil
}

// RunArchitect runs the two phases of an architect/editor run: the read-only
// plan phase on the architect model, then the edit phase on the coder model
// with the plan injected as context.
//
// A plan phase that produces nothing is not a failure: the edit phase runs
// with an explicit "no plan produced" note, so the operator sees what
// happened and the coder plans the work itself. A planner that could not
// answer at all is treated the same way — only a model that cannot be
// resolved is a hard error.
//
// The plan phase reads the conversation as it is; the edit phase runs on what
// the compaction left, so the coder sees the thread inside the window.
func (a *App) RunArchitect(ctx context.Context, opts ArchitectOptions) (*ArchitectResult, error) {
	if a == nil {
		return nil, fmt.Errorf("architect: no app")
	}
	task := strings.TrimSpace(opts.Task)
	if task == "" {
		return nil, fmt.Errorf("architect: no task")
	}
	if opts.CoderLoop == nil {
		return nil, fmt.Errorf("architect: no coder loop")
	}
	if opts.Compiled == nil {
		return nil, fmt.Errorf("architect: no compiled stack for the coder")
	}
	arch, err := a.ResolveArchitect(opts.ArchitectModel)
	if err != nil {
		return nil, err
	}
	archComp, err := a.CompileRole(arch, "opsec", CompileOpts{
		Persona: opts.Persona,
		Profile: opts.Profile,
		Task:    task,
	})
	if err != nil {
		return nil, fmt.Errorf("architect: compile: %w", err)
	}

	res := &ArchitectResult{ArchitectModel: arch.ProviderName + "/" + arch.Model.ID}
	if opts.Coder != nil {
		res.CoderModel = opts.Coder.ProviderName + "/" + opts.Coder.Model.ID
	}
	say(opts.Note, "· architect: %s", res.ArchitectModel)

	body, path, planErr := a.runPlanPhase(ctx, arch, archComp, opts)
	if planErr != nil {
		// A planner that could not answer is not a failed run: the edit
		// phase plans the work itself and says so.
		say(opts.Note, "· architect failed: %v", planErr)
	}
	res.Plan = body
	res.PlanPath = path
	if body == "" {
		res.PlanNote = noPlanNote
		say(opts.Note, "· no plan produced — the edit phase plans from the task")
	} else {
		say(opts.Note, "· plan: %s", path)
	}

	say(opts.Note, "· coder: %s", res.CoderModel)

	// The plan rides as a system message right before the task: it is the
	// current directive, and the compiled system prompt in front of it is
	// untouched, so the cached prefix stays byte-identical.
	history := make([]provider.Message, 0, len(opts.History)+1)
	history = append(history, opts.History...)
	history = append(history, approvedPlanMessage(body, path))

	final, updated, err := opts.CoderLoop.RunConversation(ctx, opts.Compiled.Text, history, task)
	if err != nil {
		return res, err
	}
	res.Final = final
	res.History = updated
	return res, nil
}

// runPlanPhase runs the read-only planning turn and returns the plan and the
// artifact it was written to. A plan that cannot be saved is still a plan:
// the edit phase gets it and the operator is told the artifact is missing.
func (a *App) runPlanPhase(ctx context.Context, arch *Resolved, comp *prompt.Compiled, opts ArchitectOptions) (string, string, error) {
	loop := a.planLoop(opts, arch)
	final, _, err := loop.RunConversation(ctx, comp.Text, opts.History, architectInstruction+"\n\nTask:\n"+opts.Task)
	if err != nil {
		return "", "", fmt.Errorf("architect: plan phase: %w", err)
	}
	body := strings.TrimSpace(final)
	if body == "" {
		return "", "", nil
	}
	art, err := plan.Save(a.Workspace, architectPlanName(opts.Task), body)
	if err != nil {
		say(opts.Note, "· warn: the plan was not saved: %v", err)
		return body, "", nil
	}
	return body, art.Path, nil
}

// planLoop builds the loop the plan phase runs on. It inherits the wiring of
// the edit loop — interception, continuity, hooks — so both phases are
// ordinary turns, and swaps in the architect's client and a read-only
// executor.
//
// The plan phase does not write to the session log: the plan is an artifact,
// not a turn of the operator's conversation, and a replay must not carry the
// architect's brief.
func (a *App) planLoop(opts ArchitectOptions, arch *Resolved) *agent.Loop {
	l := &agent.Loop{
		Client:   arch.Client,
		Tools:    a.readOnlyExecutor(),
		MaxLoops: 8,
		OnTool:   opts.OnTool,
		Model:    arch.ProviderName + "/" + arch.Model.ID,
		OnUsage: func(p, c int) {
			// The architect's tokens are the architect's provider's tokens.
			a.RecordUsage(arch.ProviderName, p, c)
		},
	}
	wiring := opts.Wiring
	if wiring == nil {
		return l
	}
	l.MaxLoops = wiring.MaxLoops
	l.Interceptor = wiring.Interceptor
	l.Reminder = wiring.Reminder
	l.OnIntercept = wiring.OnIntercept
	l.MaxRefusalRetries = wiring.MaxRefusalRetries
	l.Advisor = wiring.Advisor
	l.OnAdvisor = wiring.OnAdvisor
	l.MaxAdvisorRevisions = wiring.MaxAdvisorRevisions
	l.PriorWork = wiring.PriorWork
	l.GuardRefusal = wiring.GuardRefusal
	l.OnDrift = wiring.OnDrift
	l.Hooks = wiring.Hooks
	l.SessionID = wiring.SessionID
	l.CWD = wiring.CWD
	l.Persona = wiring.Persona
	return l
}

// readOnlyExecutor builds the executor the plan phase runs on.
//
// The turn may only read. Every tool that would change the workspace — edit,
// write, apply_patch, bash, webfetch — is refused: the permission matrix
// runs in plan mode, which is read-only by construction, every command is
// denied by the policy before the approver is even asked, and the approver
// itself refuses. The MCP servers are left off, so the plan turn has no way
// to reach anything but the read tools.
func (a *App) readOnlyExecutor() *tools.Executor {
	policy := a.ToolPolicy()
	policy.Yolo = false
	policy.Mode = string(tools.ModePlan)
	// Every command is refused by the policy itself.
	policy.DenyCommands = append(policy.DenyCommands, ".*")

	ex := &tools.Executor{
		Workspace: a.Workspace,
		Policy:    policy,
		Offline:   a.Cfg.Core.Mode == "offline",
		SkillsDir: a.SkillsDir(),
	}
	ex.ApplySecurity(a.Cfg)
	ex.Approver = tools.ApproverFunc(func(tools.ApprovalRequest) bool { return false })
	// The knowledge bases are read-only, so the plan phase may search them.
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
	return ex
}

// approvedPlanMessage is the system message the edit phase carries: the
// architect's plan, the artifact it was written to, or the explicit note that
// there is no plan.
func approvedPlanMessage(planText, path string) provider.Message {
	var b strings.Builder
	b.WriteString(approvedPlanHeading)
	if strings.TrimSpace(planText) == "" {
		b.WriteString("\n")
		b.WriteString(noPlanNote)
	} else {
		b.WriteString(strings.TrimSpace(planText))
	}
	if path != "" {
		b.WriteString("\n\nPlan file: ")
		b.WriteString(path)
	}
	return provider.Message{Role: "system", Content: b.String()}
}

// architectPlanName is the name the plan is filed under: the task's first
// line, trimmed.
func architectPlanName(task string) string {
	t := strings.TrimSpace(task)
	if i := strings.IndexAny(t, "\r\n"); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	if len(t) > 60 {
		t = strings.TrimSpace(t[:60])
	}
	if t == "" {
		t = "plan"
	}
	return t
}

// say prints an operator-facing line when the caller asked for one.
func say(fn func(format string, a ...any), format string, a ...any) {
	if fn == nil {
		return
	}
	fn(format, a...)
}

// ---- harness compaction settings (M10.3) ----------------------------------
//
// The two harness keys live in the [harness] table:
//
//	[harness]
//	compact_after = 24
//	compact_keep_ratio = 0.4
//
// The configuration package is owned by another workstream, so the keys are
// read here from the very files the configuration loader read, in the same
// precedence order (global → workspace → KINTSUGI_CONFIG). A file that cannot be
// read is skipped: the documented defaults still apply.

// harnessCompact mirrors the two harness compaction keys.
type harnessCompact struct {
	Harness struct {
		CompactAfter     int     `toml:"compact_after"`
		CompactKeepRatio float64 `toml:"compact_keep_ratio"`
	} `toml:"harness"`
}

// CompactSettings returns the mid-session compaction settings: the operator's
// keys when they are set, the documented defaults otherwise.
func (a *App) CompactSettings() agent.CompactOptions {
	opts := agent.CompactOptions{
		After:     agent.DefaultCompactAfter,
		KeepRatio: agent.DefaultCompactKeepRatio,
	}
	if a == nil {
		return opts
	}
	var raw harnessCompact
	for _, path := range a.ConfigPaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		var f harnessCompact
		if _, err := toml.DecodeFile(path, &f); err != nil {
			continue
		}
		if f.Harness.CompactAfter != 0 {
			raw.Harness.CompactAfter = f.Harness.CompactAfter
		}
		if f.Harness.CompactKeepRatio != 0 {
			raw.Harness.CompactKeepRatio = f.Harness.CompactKeepRatio
		}
	}
	if raw.Harness.CompactAfter != 0 {
		opts.After = raw.Harness.CompactAfter
	}
	if raw.Harness.CompactKeepRatio != 0 {
		opts.KeepRatio = raw.Harness.CompactKeepRatio
	}
	return opts
}
