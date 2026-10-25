// Package tools holds tool implementations, schema definitions, the repair
// dispatch layer and the permission gate. All paths resolve against the
// workspace root, never the process CWD (models cannot see CWD).
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/audit"
	"kintsugi/internal/config"
	"kintsugi/internal/hub"
	"kintsugi/internal/lab"
	"kintsugi/internal/mcp"
	"kintsugi/internal/memory"
	"kintsugi/internal/mods"
	"kintsugi/internal/provider"
	"kintsugi/internal/redact"
	"kintsugi/internal/sandbox"
	"kintsugi/internal/scope"
	"kintsugi/internal/skills"
)

// TaskRequest is one subagent assignment (DESIGN §5.2).
type TaskRequest struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
	Agent  string `json:"agent,omitempty"`
	// Role selects the model role (and therefore the provider/model) the runner
	// uses for this worker (WP-4.1): coder, researcher or opsec. Empty means
	// ModelRole's default, coder.
	Role string `json:"role,omitempty"`
	// Context is the typed handoff for this worker (M8.5.5): what was already
	// decided or accepted for this task, carried as project context. It is
	// context, never permission — a worker's permissions come from policy, and an
	// empty handoff is not a refusal. Runners may fold it into the worker's
	// context bundle.
	Context string `json:"context,omitempty"`
	// Background (M10-D): when true, the task runs in the background and returns
	// an ID immediately. The caller uses agent_output to wait/status/kill.
	Background bool `json:"background,omitempty"`
}

// TaskRoles are the model roles a task item may name.
var TaskRoles = []string{"coder", "researcher", "opsec"}

// ModelRole returns the normalized model role for this task, defaulting to
// "coder" when the caller named none.
func (t TaskRequest) ModelRole() string {
	role := strings.ToLower(strings.TrimSpace(t.Role))
	if role == "" {
		return "coder"
	}
	return role
}

// ValidModelRole reports whether role is one of TaskRoles.
func ValidModelRole(role string) bool {
	for _, r := range TaskRoles {
		if r == role {
			return true
		}
	}
	return false
}

// TaskRunner fans tasks out to isolated workers; subagent.Manager implements it.
type TaskRunner interface {
	RunTasks(ctx context.Context, tasks []TaskRequest) (string, error)
}

type Executor struct {
	Workspace string
	Memory    *memory.Store
	SkillsDir string
	// OnOutput, when set, receives each line of a running tool's combined
	// output while the tool runs — the live stream a front end renders
	// before the tool returns. nil (the default) is byte-identical to the
	// pre-streaming behaviour: output arrives once, with the result.
	OnOutput func(ev ToolOutputEvent)
	// MemoryVerbs carries the wiring the memory verbs (M10-C) need beyond the
	// store: the write gate shared with the curator, the skills directory they
	// write to, and the curator client `reflect` synthesizes with. nil means
	// the defaults — the hard-gated classes only, the workspace skills
	// directory, and no curator model.
	MemoryVerbs *MemoryVerbConfig
	MCP         *mcp.Manager
	// Mods are the external-process plugins this workspace may run. They are
	// repository-controlled, so the manager only holds the trusted ones.
	Mods     *mods.Manager
	Policy   Policy
	Approver Approver
	Tasks    TaskRunner
	Log      func(kind string, v any)
	OnTodo   func([]TodoItem)
	Offline  bool
	// Sandbox is the OS-level isolation posture for shell commands (M11.1).
	// The zero value means "off": commands run unsandboxed, as before.
	// With a mode of readonly or workspace, a command runs inside the
	// sandbox, and an unavailable backend refuses the command instead of
	// running it unsandboxed.
	Sandbox sandbox.Config
	// Scope is the operator-declared engagement scope (M11.2). Empty rules
	// mean no scope was declared, so nothing is narrowed. A declared scope
	// denies paths, commands and hosts outside it before they are asked
	// about, and out_of_scope entries always win.
	Scope scope.Rules
	// Lab is the operator's lab configuration (M8 remainder, DESIGN §6.3).
	// It names the compose project, the compose file and the services the
	// `run_in_lab` tool may use. A nil value means no lab is configured:
	// `run_in_lab` then works from the lab lock alone and refuses when there
	// is no lock.
	Lab *lab.Config
	// Hub is the background run registry (M10-D): task(background=true) spawns
	// into it and agent_output collects from it. It is a distinct package from
	// subagent on purpose — subagent runs workers, workers use tools, and the
	// tool layer must not import the thing that imports it.
	Hub *hub.Hub
	// KBs are the knowledge bases this workspace may search (DESIGN §3.1
	// layer 3). The executor does not guess a layout: the caller names the
	// bases and where they live. Empty means "no knowledge base configured".
	KBs []KBSource
	// KBTopK is the default number of kb_search hits. Zero means 8.
	KBTopK int
	// Audit is the workspace's local audit log (M11.3). Every permission
	// decision the executor makes is recorded there: the mode baseline, the
	// rule that decided it, the operator's answer, a scope or sandbox
	// refusal. A nil log means no audit — the harness still works, it just
	// keeps no security record.
	Audit *audit.Log
	// SessionID names the session the audit events belong to.
	SessionID string
	// Classifier is the two-stage permission classifier (A1). nil disables the
	// model-backed stage; deterministic buckets still run. A classifier is
	// advisory: it may only tighten a decision, never widen it, and a call the
	// classifier blocks is refused before the permission matrix is consulted.
	Classifier Classifier
	// ClassifierMode gates the model-backed stage: off | soft | hard. Off is the
	// default and is byte-identical to the pre-classifier behaviour.
	ClassifierMode ClassifierMode

	mu       sync.Mutex
	approved map[string]bool
}

type Result struct {
	Content string
	Failed  bool
	// Diff carries the structured before/after for file-authoring tools, so the
	// transcript can render a real diff instead of a one-line summary card. It
	// is populated by edit, write and apply_patch; every other tool leaves it
	// nil and pays no cost. The Before/After strings are whole-file contents
	// (or "" when there was no before, e.g. a new file), so the renderer owns
	// the line-diff and the cap policy.
	Diff []FileChange
}

// ToolOutputEvent is one line of a running tool's combined output, emitted
// while the tool runs. Line is already redacted: a secret must never stream
// to a front end unredacted, even transiently. Line-at-a-time redaction
// covers the single-line patterns; a multi-line construct (a PEM block) is
// fully redacted in the final Result, which is the authoritative persisted
// copy.
type ToolOutputEvent struct {
	Tool  string
	Label string // the command or path — the same label the final card shows
	Line  string
}

// emitOutput forwards one streamed line to the observer, if any.
func (e *Executor) emitOutput(ev ToolOutputEvent) {
	if e.OnOutput != nil {
		e.OnOutput(ev)
	}
}

// FileChange is one file's before/after for diff rendering. Kind names the
// operation that produced it, so a renderer can label the card ("edit",
// "new file", "deleted") without re-deriving it from the strings.
type FileChange struct {
	Path   string
	Kind   string // edit | write-new | write-replace | append | patch-add | patch-delete | patch-update | patch-move
	Before string
	After  string
}

func (e *Executor) Definitions() []provider.Tool {
	defs := []provider.Tool{}
	if e.Memory != nil {
		defs = append(defs, memorySearchTool(), retainTool(), recallTool(), reflectTool(), memoryEditTool(), learnTool(), manageSkillTool())
	}
	defs = append(defs, readTool(), listTool(), globTool(), grepTool(), editTool(), writeTool(), applyPatchTool(), bashTool(), todoTool(), webFetchTool(), webSearchTool(), extractTool(), cveLookupTool(), kbSearchTool(), repoMapTool(), astGrepTool(), runInLabTool())
	if e.SkillsDir != "" {
		defs = append(defs, skillTool())
	}
	if e.Tasks != nil {
		defs = append(defs, taskTool())
	}
	if e.Hub != nil {
		defs = append(defs, agentOutputTool())
	}
	if e.MCP != nil {
		defs = append(defs, e.MCP.Definitions()...)
	}
	if e.Mods != nil {
		defs = append(defs, e.Mods.Definitions()...)
	}
	return defs
}

// Dispatch repairs, authorizes and executes one tool call. A failed repair is
// a tool result (the model sees its own malformed call), not a harness crash.
func (e *Executor) Dispatch(tc provider.ToolCall) Result {
	return e.DispatchContext(context.Background(), tc)
}

// DispatchContext is Dispatch with the caller's cancellation/deadline, used
// by subagents and DAG nodes.
func (e *Executor) DispatchContext(ctx context.Context, tc provider.ToolCall) Result {
	rep, err := Repair(tc.Function.Name, tc.Function.Arguments)
	if err != nil {
		if e.Log != nil {
			e.Log("tool_repair_failed", map[string]any{"tool": tc.Function.Name, "error": err.Error()})
		}
		return Result{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), Failed: true}
	}
	if e.Log != nil && len(rep.Classes) > 0 {
		e.Log("tool_repair", map[string]any{
			"tool": tc.Function.Name, "classes": rep.Classes, "notes": rep.Notes,
		})
	}

	res := e.execute(ctx, rep.Tool, rep.Args)

	var annotations []string
	for _, n := range rep.Notes {
		annotations = append(annotations, "note: "+n)
	}
	if len(rep.Classes) > 0 && !res.Failed {
		annotations = append(annotations, "harness note: repaired tool arguments ("+strings.Join(rep.Classes, ", ")+")")
	}
	if len(annotations) > 0 {
		res.Content = strings.TrimRight(res.Content, "\n") + "\n\n" + strings.Join(annotations, "\n")
	}
	return res
}

func (e *Executor) execute(ctx context.Context, tool, args string) Result {
	// The classifier (A1) runs ahead of the permission matrix. It only ever
	// tightens a decision: a blocked call is refused here with the classifier's
	// reason, and a non-blocked call falls through to the matrix unchanged.
	if block, reason := e.classify(tool, SpecArg(tool, args), e.ClassifierMode); block {
		e.audit(tool, SpecArg(tool, args), audit.DecisionDeny, reason, "classifier")
		return Result{
			Failed:  true,
			Content: fmt.Sprintf(`{"error":%q,"permission":"deny","classifier":true}`, reason),
		}
	}
	// The permission matrix is the single gate (M9.4). A deny is final and is
	// answered here; an "ask" is answered by the tool's own authorize() call,
	// which has the resolved command/path in hand and can therefore match a
	// rule's spec (and so lets the built-in deny floors run first). A hook may
	// tighten this later, never widen it.
	d := e.Policy.Decide(tool, args)
	if d.Action == ActionDeny {
		e.audit(tool, SpecArg(tool, args), audit.DecisionDeny, d.Reason, d.Rule)
		return Result{
			Failed:  true,
			Content: fmt.Sprintf(`{"error":%q,"permission":"deny"}`, d.Reason),
		}
	}
	// A tool that consults the approver records its own decision in
	// authorize(), where the operator's answer is known. Every other tool is
	// decided by the matrix alone, so the gate records that decision here.
	if !e.consultsApprover(tool) {
		e.audit(tool, SpecArg(tool, args), d.Action, d.Reason, d.Rule)
	}
	switch tool {
	case "read":
		return e.read(args)
	case "list":
		return e.list(args)
	case "glob":
		return e.glob(args)
	case "grep":
		return e.grep(args)
	case "edit":
		return e.edit(args)
	case "write":
		return e.write(args)
	case "apply_patch":
		return e.applyPatch(args)
	case "todo":
		return e.todo(args)
	case "webfetch":
		if err := e.scopeURL(args); err != nil {
			e.audit("webfetch", "", audit.DecisionDeny, err.Error(), audit.RuleScope)
			return failJSON(err)
		}
		return e.webFetch(ctx, args)
	case "websearch":
		return e.webSearch(ctx, args)
	case "extract":
		return e.extract(ctx, args)
	case "cve_lookup":
		return e.cveLookup(ctx, args)
	case "bash":
		// The lab lock is a boundary: while a lab is up, a command may only
		// run inside it. The host shell is refused before it is asked about.
		if err := e.labHostGuard(); err != nil {
			e.audit("bash", "", audit.DecisionDeny, err.Error(), "lab")
			return failJSON(err)
		}
		return e.bash(ctx, args)
	case "run_in_lab":
		return e.runInLab(ctx, args)
	case "memory_search":
		return e.memorySearch(args)
	case "recall":
		return e.recall(args)
	case "retain":
		return e.retain(args)
	case "memory_edit":
		return e.memoryEdit(args)
	case "learn":
		return e.learn(args)
	case "manage_skill":
		return e.manageSkill(args)
	case "reflect":
		return e.reflect(ctx, args)
	case "kb_search":
		return e.kbSearch(args)
	case "repo_map":
		return e.repoMap(args)
	case "ast_grep":
		return e.runASTGrep(args)
	case "skill":
		return e.skill(args)
	case "task":
		return e.task(ctx, args)
	case "agent_output":
		return e.agentOutput(ctx, args)
	default:
		if e.Mods != nil && e.Mods.Has(tool) {
			if err := e.authorize(tool, "", "mod tool call: "+clip(args, 300)); err != nil {
				return Result{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), Failed: true}
			}
			content, isErr, err := e.Mods.Call(ctx, tool, args)
			if err != nil {
				return failJSON(err)
			}
			return Result{Content: content, Failed: isErr}
		}
		if e.MCP != nil && e.MCP.Has(tool) {
			if err := e.authorize(tool, "", "MCP tool call: "+clip(args, 300)); err != nil {
				return Result{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), Failed: true}
			}
			content, isErr, err := e.MCP.Call(ctx, tool, args)
			if err != nil {
				return failJSON(err)
			}
			return Result{Content: content, Failed: isErr}
		}
		return Result{
			Content: fmt.Sprintf(`{"error":"unknown tool %q"}`, tool),
			Failed:  true,
		}
	}
}

// ---------- argument parsing ----------

func decode(args string, dst any) error {
	return json.Unmarshal([]byte(args), dst)
}

// ---------- path resolution + policy ----------

// resolve resolves a workspace-relative path for a tool that does not name
// itself in the record. The path floors are recorded against "path" then.
func (e *Executor) resolve(p string) (string, error) {
	return e.resolveFor("", p)
}

// resolveFor is resolve with the tool that asked for the path, so the audit
// record names the tool whose call was refused.
func (e *Executor) resolveFor(tool, p string) (string, error) {
	if e.Workspace == "" {
		return "", fmt.Errorf("no workspace configured")
	}
	p = strings.TrimSpace(p)
	if p == "" {
		p = "."
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(e.Workspace, full)
	}
	clean := filepath.Clean(full)
	ws := filepath.Clean(e.Workspace)
	if clean != ws && !strings.HasPrefix(clean, ws+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	rel, _ := filepath.Rel(ws, clean)
	if err := e.scopePath(p, rel); err != nil {
		e.audit(pathTool(tool), p, audit.DecisionDeny, err.Error(), audit.RuleScope)
		return "", err
	}
	if e.Policy.PathDenied(rel) {
		err := fmt.Errorf("path %q is denied by policy", p)
		e.audit(pathTool(tool), p, audit.DecisionDeny, err.Error(), audit.RuleDenyPaths)
		return "", err
	}
	return clean, nil
}

// pathTool names the tool in a path-floor record. A call that did not say
// which tool it was is recorded as "path": the decision was about the path.
func pathTool(tool string) string {
	if t := strings.TrimSpace(tool); t != "" {
		return t
	}
	return "path"
}

// ---------- scope + sandbox floors (M11.1, M11.2) ----------

// ApplySecurity wires the operator's sandbox posture and declared scope
// into the executor: the [sandbox] block from the config, and the scope
// rules from [scope] plus the CONTEXT.md "Scope" section.
//
// The caller owns this call: the executor cannot know the operator's
// configuration on its own, and a default that silently isolated (or
// silently did not) would be worse than an explicit wiring.
func (e *Executor) ApplySecurity(c config.Config) {
	e.Sandbox = sandbox.FromConfig(c.Sandbox)
	e.Scope = scope.FromWorkspace(c, e.Workspace)
}

// scopePath refuses a path outside the declared scope. The scope is a deny
// list, not a prompt: an out-of-scope path never reaches the operator.
func (e *Executor) scopePath(requested, rel string) error {
	if e.Scope.Empty() {
		return nil
	}
	if !e.Scope.AllowsPath(rel) {
		return fmt.Errorf("out of scope: path %q is not in the declared scope", requested)
	}
	return nil
}

// scopeCommand refuses a command outside the declared scope.
func (e *Executor) scopeCommand(cmd string) error {
	if e.Scope.Empty() {
		return nil
	}
	if !e.Scope.AllowsCommand(cmd) {
		return fmt.Errorf("out of scope: command %q is not in the declared scope", cmd)
	}
	return nil
}

// scopeURL refuses a fetch whose host is outside the declared scope.
func (e *Executor) scopeURL(args string) error {
	if e.Scope.Empty() && !e.sandboxAllowlistActive() {
		return nil
	}
	var a struct {
		URL string `json:"url"`
	}
	if err := decode(args, &a); err != nil {
		return nil // the webfetch tool reports the malformed argument
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil {
		return nil // the webfetch tool reports the malformed URL
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	if !e.Scope.Empty() && !e.Scope.AllowsHost(host) {
		return fmt.Errorf("out of scope: host %q is not in the declared scope", host)
	}
	// bubblewrap cannot filter by host, so the sandbox allowlist is enforced
	// here, in the fetch layer.
	if e.sandboxAllowlistActive() {
		allow := scope.Rules{Hosts: e.Sandbox.Allowlist}
		if !allow.AllowsHost(host) {
			return fmt.Errorf("out of scope: host %q is not in the sandbox allowlist", host)
		}
	}
	return nil
}

// sandboxAllowlistActive reports whether the sandbox declares a host
// allowlist that the fetch layer has to honour.
func (e *Executor) sandboxAllowlistActive() bool {
	return e.Sandbox.Enabled() &&
		strings.EqualFold(strings.TrimSpace(string(e.Sandbox.Network)), string(sandbox.NetworkAllowlist)) &&
		len(e.Sandbox.Allowlist) > 0
}

// sandboxUsable reports whether the configured sandbox can be used. When the
// operator asked for a sandbox, a missing backend is a refusal, not a
// fallback: the harness never pretends isolation it does not have.
func (e *Executor) sandboxUsable() error {
	if !e.Sandbox.Enabled() {
		return nil
	}
	res := sandbox.Detect(e.Sandbox)
	if !res.Available {
		return fmt.Errorf("sandbox unavailable: %s", res.Reason)
	}
	return nil
}

// authorize asks the operator when the permission matrix says "ask". The
// matrix is consulted first with the call's spec argument: a rule that allows
// the call answers the prompt, a rule that denies it refuses outright.
//
// Every outcome leaves one event in the audit log (M11.3): the rule that
// allowed it, the operator's answer, or the refusal.
// authorize asks the operator when the permission matrix says "ask". changes
// carries the proposed file changes for the authoring tools, so an informed
// approval is possible; every other tool passes none.
func (e *Executor) authorize(tool, spec, detail string, changes ...FileChange) error {
	d := e.Policy.DecideSpec(tool, spec)
	switch d.Action {
	case ActionAllow:
		e.audit(tool, detail, audit.DecisionAllow, d.Reason, d.Rule)
		return nil
	case ActionDeny:
		e.audit(tool, detail, audit.DecisionDeny, d.Reason, d.Rule)
		return fmt.Errorf("%s denied by policy: %s", tool, d.Reason)
	}
	e.mu.Lock()
	ok := e.approved[tool]
	e.mu.Unlock()
	if ok {
		e.audit(tool, detail, audit.DecisionAllow,
			"ask: "+d.Reason+" → approved earlier in this session", d.Rule)
		return nil
	}
	if e.Approver == nil {
		e.audit(tool, detail, audit.DecisionDeny,
			"ask: "+d.Reason+" → no approver is attached, so the call is refused", d.Rule)
		return fmt.Errorf("%s requires approval, and no approver is attached (denied)", tool)
	}
	if !e.Approver.Approve(ApprovalRequest{Tool: tool, Detail: detail, Changes: changes}) {
		e.audit(tool, detail, audit.DecisionDeny,
			"ask: "+d.Reason+" → denied by the operator", d.Rule)
		return fmt.Errorf("%s denied by operator", tool)
	}
	e.audit(tool, detail, audit.DecisionAllow,
		"ask: "+d.Reason+" → approved by the operator", d.Rule)
	e.mu.Lock()
	if e.approved == nil {
		e.approved = map[string]bool{}
	}
	e.approved[tool] = true
	e.mu.Unlock()
	return nil
}

// ---------- audit (M11.3) ----------

// approverTools are the tools whose own implementation asks the operator.
// They record their decision in authorize(), where the operator's answer is
// known; every other tool is decided by the matrix alone, so the gate
// records it.
//
// A tool added here without an authorize() call would record twice, and one
// added the other way round would record nothing. The list is the price of
// keeping one event per call.
var approverTools = map[string]bool{
	"apply_patch": true, "bash": true, "cve_lookup": true, "edit": true,
	"run_in_lab": true, "task": true, "webfetch": true, "websearch": true, "write": true,
}

// consultsApprover reports whether the tool's own implementation asks the
// operator for this call.
func (e *Executor) consultsApprover(tool string) bool {
	if approverTools[strings.ToLower(strings.TrimSpace(tool))] {
		return true
	}
	return e.MCP != nil && e.MCP.Has(tool)
}

// EnableAudit opens the workspace's audit log and records every permission
// decision in it (M11.3). The log is local and append-only; the caller owns
// the handle (ex.Audit.Close()).
func (e *Executor) EnableAudit(workspace, sessionID string) error {
	l, err := audit.Open(workspace)
	if err != nil {
		return err
	}
	e.Audit = l
	e.SessionID = sessionID
	return nil
}

// audit records one permission decision in the workspace audit log.
//
// A failed write is telemetry, never a failure of the tool call: the call
// itself is what the operator is waiting on.
func (e *Executor) audit(tool, detail, decision, reason, rule string) {
	if e == nil || e.Audit == nil {
		return
	}
	_ = e.Audit.Record(audit.Event{
		SessionID: e.SessionID,
		Tool:      tool,
		Detail:    detail,
		Decision:  decision,
		Reason:    reason,
		Rule:      rule,
	})
}

// ---------- definitions ----------

func readTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "read",
			Description: "Read a UTF-8 text file from the workspace and return its contents. Paths are workspace-relative strings.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"path"},
				Properties: map[string]provider.ToolProp{
					"path": {Type: "string", Description: "Workspace-relative path string of the text file to read, e.g. \"README.md\"."},
				},
			},
		},
	}
}

func listTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "list",
			Description: "List directory entries in the workspace. Directories end with \"/\".",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: nil,
				Properties: map[string]provider.ToolProp{
					"path": {Type: "string", Description: "Workspace-relative directory path string (default: workspace root)."},
				},
			},
		},
	}
}

func globTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "glob",
			Description: "Find files by glob pattern (supports \"**/\" prefixes). Returns workspace-relative paths.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"pattern"},
				Properties: map[string]provider.ToolProp{
					"pattern": {Type: "string", Description: "Glob pattern string, e.g. \"**/*.go\" or \"internal/*.go\"."},
					"root":    {Type: "string", Description: "Optional workspace-relative directory to search from."},
				},
			},
		},
	}
}

func grepTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "grep",
			Description: "Search file contents with a regular expression. Returns path:line:content matches.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"pattern"},
				Properties: map[string]provider.ToolProp{
					"pattern": {Type: "string", Description: "Regular expression string."},
					"path":    {Type: "string", Description: "Optional workspace-relative file or directory path string to search."},
					"glob":    {Type: "string", Description: "Optional file glob filter, e.g. \"*.md\"."},
					"max":     {Type: "integer", Description: "Maximum matches (default 100)."},
				},
			},
		},
	}
}

func editTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "edit",
			Description: "Replace one exact occurrence of a string in a workspace file. Fails if the old string is missing or appears more than once — read the file first and use a unique anchor.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"path", "old", "new"},
				Properties: map[string]provider.ToolProp{
					"path": {Type: "string", Description: "Workspace-relative file path string."},
					"old":  {Type: "string", Description: "Exact text to replace; must occur exactly once."},
					"new":  {Type: "string", Description: "Replacement text (may be empty to delete)."},
				},
			},
		},
	}
}

func bashTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "bash",
			Description: "Run a shell command in the workspace root and return its combined output and exit code. Requires operator approval.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"command"},
				Properties: map[string]provider.ToolProp{
					"command":   {Type: "string", Description: "Shell command string (runs via bash -lc)."},
					"timeout_s": {Type: "integer", Description: "Timeout in seconds (default 60, max 120)."},
				},
			},
		},
	}
}

func memorySearchTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "memory_search",
			Description: "Search the project's durable memory (learned lessons and skills). Use when prior project knowledge might change the answer.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"query"},
				Properties: map[string]provider.ToolProp{
					"query": {Type: "string", Description: "Keywords describing what you need to recall."},
					"k":     {Type: "integer", Description: "Maximum results (default 8)."},
				},
			},
		},
	}
}

// ---------- implementations ----------

const readCap = 96 << 10

func (e *Executor) read(args string) Result {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Path) == "" {
		return Result{Content: `{"error":"read: missing required property 'path'"}`, Failed: true}
	}
	p, err := e.resolveFor("read", a.Path)
	if err != nil {
		return failJSON(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return failJSON(err)
	}
	if len(data) > readCap {
		return Result{Content: fmt.Sprintf("%s\n…[truncated at %d bytes of %d]", data[:readCap], readCap, len(data))}
	}
	return Result{Content: string(data)}
}

func (e *Executor) list(args string) Result {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(args, &a)
	p, err := e.resolveFor("list", a.Path)
	if err != nil {
		return failJSON(err)
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return failJSON(err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var b strings.Builder
	for _, ent := range ents {
		name := ent.Name()
		if ent.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}
	return Result{Content: redact.Redact(strings.TrimRight(b.String(), "\n"))}
}

func (e *Executor) glob(args string) Result {
	var a struct {
		Pattern string `json:"pattern"`
		Root    string `json:"root"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return Result{Content: `{"error":"glob: missing required property 'pattern'"}`, Failed: true}
	}
	root := a.Root
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	base, err := e.resolveFor("glob", root)
	if err != nil {
		return failJSON(err)
	}

	pattern := filepath.ToSlash(strings.TrimSpace(a.Pattern))
	var out []string
	filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(e.Workspace, p)
		rel = filepath.ToSlash(rel)
		if matchGlob(pattern, rel) && e.Scope.AllowsPath(rel) {
			out = append(out, rel)
			if len(out) >= 300 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if len(out) == 0 {
		return Result{Content: "no files matched " + pattern}
	}
	return Result{Content: strings.Join(out, "\n")}
}

func matchGlob(pattern, rel string) bool { return globMatch(pattern, rel) }

func (e *Executor) grep(args string) Result {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
		Max     int    `json:"max"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return Result{Content: `{"error":"grep: missing required property 'pattern'"}`, Failed: true}
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return Result{Content: fmt.Sprintf(`{"error":%q}`, "invalid regex: "+err.Error()), Failed: true}
	}
	start := a.Path
	if strings.TrimSpace(start) == "" {
		start = "."
	}
	base, err := e.resolveFor("grep", start)
	if err != nil {
		return failJSON(err)
	}
	limit := a.Max
	if limit <= 0 {
		limit = 100
	}

	var matches []string
	filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(e.Workspace, p)
		rel = filepath.ToSlash(rel)
		if !e.Scope.AllowsPath(rel) {
			return nil
		}
		if a.Glob != "" && !matchGlob(a.Glob, rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2<<20 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(512, len(data))], 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, i+1, strings.TrimRight(line, "\r")))
				if len(matches) >= limit {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if len(matches) == 0 {
		return Result{Content: "no matches"}
	}
	return Result{Content: redact.Redact(strings.Join(matches, "\n"))}
}

func (e *Executor) edit(args string) Result {
	var a struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	p, err := e.resolveFor("edit", a.Path)
	if err != nil {
		return failJSON(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return failJSON(err)
	}
	content := string(data)
	count := strings.Count(content, a.Old)
	if count == 0 {
		return Result{Content: fmt.Sprintf(`{"error":"edit anchor not found in %s; re-read the file and use an exact anchor"}`, a.Path), Failed: true}
	}
	if count > 1 {
		return Result{Content: fmt.Sprintf(`{"error":"edit anchor occurs %d times in %s; add surrounding context to make it unique"}`, count, a.Path), Failed: true}
	}
	updated := strings.Replace(content, a.Old, a.New, 1)
	// The approval is asked with the proposed change in hand: the operator
	// answers y/n with the diff in front of them, not a clipped preview line.
	preview := a.Path + "  " + clip(a.Old, 60) + " → " + clip(a.New, 60)
	if err := e.authorize("edit", a.Path, preview, FileChange{Path: a.Path, Kind: "edit", Before: content, After: updated}); err != nil {
		return failJSON(err)
	}
	if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
		return failJSON(err)
	}
	line := 1 + strings.Count(content[:strings.Index(content, a.Old)], "\n")
	return Result{
		Content: fmt.Sprintf(`{"ok":true,"path":%q,"line":%d,"bytes_before":%d,"bytes_after":%d}`, a.Path, line, len(content), len(updated)),
		Diff:    []FileChange{{Path: a.Path, Kind: "edit", Before: content, After: updated}},
	}
}

func (e *Executor) bash(ctx context.Context, args string) Result {
	var a struct {
		Command  string `json:"command"`
		TimeoutS int    `json:"timeout_s"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Command) == "" {
		return Result{Content: `{"error":"bash: missing required property 'command'"}`, Failed: true}
	}
	// Scope and sandbox are hard floors, and they are checked before the
	// permission gate: an out-of-scope command never reaches the operator
	// prompt, and a promised sandbox is never silently dropped.
	if err := e.scopeCommand(a.Command); err != nil {
		e.audit("bash", a.Command, audit.DecisionDeny, err.Error(), audit.RuleScope)
		return failJSON(err)
	}
	if err := e.sandboxUsable(); err != nil {
		e.audit("bash", a.Command, audit.DecisionDeny, err.Error(), audit.RuleSandbox)
		return failJSON(err)
	}
	if denied, needsApproval := e.Policy.CommandDenied(a.Command); denied {
		e.audit("bash", a.Command, audit.DecisionDeny,
			"command denied by policy", audit.RuleDenyCommands)
		return Result{Content: `{"error":"command denied by policy"}`, Failed: true}
	} else if needsApproval {
		if err := e.authorize("bash", a.Command, a.Command); err != nil {
			return failJSON(err)
		}
	}

	timeout := a.TimeoutS
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 120 {
		timeout = 120
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// The command runs inside the sandbox when one is configured. With the
	// sandbox off, this is the plain "bash -lc" it always was.
	argv := []string{"bash", "-lc", a.Command}
	if e.Sandbox.Enabled() {
		wrapped, err := sandbox.Wrap(a.Command, e.Sandbox, e.Workspace)
		if err != nil {
			return failJSON(err)
		}
		argv = wrapped
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = e.Workspace
	var out []byte
	if e.OnOutput == nil {
		// The unstreamed path, exactly as it always was: output arrives once,
		// with the result.
		var err error
		out, err = cmd.CombinedOutput()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return bashResult(string(out), ee.ExitCode())
			}
			return Result{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), Failed: true}
		}
		return bashResult(string(out), 0)
	}
	out, err := e.runStreamed(cmd, a.Command)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return bashResult(string(out), ee.ExitCode())
		}
		return Result{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), Failed: true}
	}
	return bashResult(string(out), 0)
}

// bashResult shapes the final content: redacted, capped, with the exit line.
// It is the single shaping step for both the streamed and unstreamed paths,
// so the persisted copy is identical whichever way the output arrived.
func bashResult(body string, code int) Result {
	body = redact.Redact(body)
	if len(body) > 32<<10 {
		body = body[:32<<10] + "\n…[truncated]"
	}
	return Result{
		Content: fmt.Sprintf("%s\n[exit %d]", strings.TrimRight(body, "\n"), code),
		Failed:  code != 0,
	}
}

// runStreamed runs cmd with its combined output streamed line-by-line
// through OnOutput while it is also accumulated for the final result. The
// reader goroutine owns the pipe and the buffer: the main goroutine only
// reads the buffer after the reader signals done, so no lock is needed.
// The pipe's write side is closed after Wait — both stdout and stderr copy
// goroutines inside exec are finished by then — which unblocks the scanner's
// final read and lets the drain terminate.
func (e *Executor) runStreamed(cmd *exec.Cmd, label string) ([]byte, error) {
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		// Long lines: the scanner's default 64 KiB token is small for some
		// tool output; 1 MiB is generous without being unbounded (the final
		// result caps at 32 KiB anyway).
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			buf.WriteString(line)
			buf.WriteByte('\n')
			e.emitOutput(ToolOutputEvent{Tool: "bash", Label: label, Line: redact.Redact(line)})
		}
		_ = sc.Err()
	}()
	if err := cmd.Start(); err != nil {
		pw.Close()
		<-done
		return nil, err
	}
	waitErr := cmd.Wait()
	pw.Close()
	<-done
	return buf.Bytes(), waitErr
}

func (e *Executor) memorySearch(args string) Result {
	if e.Memory == nil {
		return Result{Content: `{"error":"memory store not configured for this workspace"}`, Failed: true}
	}
	var a struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Query) == "" {
		return Result{Content: `{"error":"memory_search: missing required property 'query'"}`, Failed: true}
	}
	entries, err := e.Memory.Search(a.Query, a.K)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: memory.Format(entries)}
}

func skillTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "skill",
			Description: "Load a workspace skill by name. The prompt contains only names and descriptions; call this before following a skill so its full procedure is in context. Omit name to list available skills.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: nil,
				Properties: map[string]provider.ToolProp{
					"name": {Type: "string", Description: "Skill name from the injected Skills index."},
				},
			},
		},
	}
}

func (e *Executor) skill(args string) Result {
	var in struct {
		Name string `json:"name"`
	}
	if strings.TrimSpace(args) != "" {
		if err := decode(args, &in); err != nil {
			return failJSON(err)
		}
	}
	list := skills.Load(e.Workspace, e.SkillsDir)
	if len(list) == 0 {
		return Result{Content: `{"error":"no skills are installed under the configured skills directory"}`, Failed: true}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Result{Content: skills.Index(list)}
	}
	sk, ok := skills.Find(list, name)
	if !ok {
		var names []string
		for _, s := range list {
			names = append(names, s.Name)
		}
		return Result{Content: fmt.Sprintf(`{"error":"unknown skill %q","available":%q}`, name, names), Failed: true}
	}
	return Result{Content: fmt.Sprintf("# Skill: %s\n\nSource: %s\n\n%s", sk.Name, sk.Rel, sk.Body)}
}

func taskTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "task",
			Description: "Fan out 1-4 bounded subagents into isolated git worktrees. Each returns a schema-validated result with findings. Use for independent parallel work, not for one small edit. Each item may name a model role (coder, researcher, opsec) to run that worker on the role's model, and may carry a context handoff: the decisions already made for that worker.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"tasks"},
				Properties: map[string]provider.ToolProp{
					"tasks": {
						Type:        "array",
						Description: "Array of independent assignments.",
						Items: &provider.ToolProp{
							Type: "object",
							Properties: map[string]provider.ToolProp{
								"title":  {Type: "string", Description: "Short label for the task."},
								"prompt": {Type: "string", Description: "Self-contained instructions; the subagent cannot see this conversation."},
								"agent":  {Type: "string", Description: "Optional worker role: coder (default), researcher, opsec."},
								"role": {
									Type:        "string",
									Enum:        []any{"coder", "researcher", "opsec"},
									Description: "model role for this worker",
								},
								"context": {
									Type:        "string",
									Description: "Optional typed handoff: what was already decided, accepted or constrained for this worker (e.g. an accepted plan or the files it must not touch). The worker receives it as project context, never as a permission grant, and omitting it means 'nothing to hand over', not 'refused'.",
								},
								"background": {
									Type:        "boolean",
									Description: "Optional: run this worker detached and return its id immediately instead of waiting. Collect it later with agent_output(id, action=wait). Use for work the answer does not depend on; omit or false to wait.",
								},
							},
							Required: []string{"title", "prompt"},
						},
					},
				},
			},
		},
	}
}

func (e *Executor) task(ctx context.Context, args string) Result {
	if e.Tasks == nil {
		return Result{Content: `{"error":"task tool is not enabled in this profile"}`, Failed: true}
	}
	// Delegation is a mutating action: the matrix may ask or deny it.
	if err := e.authorize("task", "", "fan out to subagents: "+clip(args, 120)); err != nil {
		return failJSON(err)
	}
	reqs, err := decodeTaskRequests(args)
	if err != nil {
		return failJSON(err)
	}
	if len(reqs) == 0 {
		return Result{Content: `{"error":"task: at least one task is required"}`, Failed: true}
	}
	if len(reqs) > 4 {
		return Result{Content: `{"error":"task: at most 4 subagents per call"}`, Failed: true}
	}
	detached := false
	for i := range reqs {
		reqs[i].Title = strings.TrimSpace(reqs[i].Title)
		reqs[i].Prompt = strings.TrimSpace(reqs[i].Prompt)
		reqs[i].Context = normalizeTaskContext(reqs[i].Context)
		if reqs[i].Title == "" || reqs[i].Prompt == "" {
			return Result{Content: fmt.Sprintf(`{"error":"task %d: title and prompt are required"}`, i), Failed: true}
		}
		role := reqs[i].ModelRole()
		if !ValidModelRole(role) {
			return Result{Content: fmt.Sprintf(`{"error":"task %d: unknown role %q (allowed: %s)"}`, i, reqs[i].Role, strings.Join(TaskRoles, ", ")), Failed: true}
		}
		// Surface the resolved role so the runner never has to re-derive it.
		reqs[i].Role = role
		if reqs[i].Background {
			detached = true
		}
	}
	// Detaching is a request the harness must be able to honour: without a hub
	// there is nowhere for a background result to land, so it says so rather
	// than silently running the work in the foreground.
	if detached && e.Hub == nil {
		return Result{Content: `{"error":"task: background runs need a hub, which this session does not have; drop background or use kintsugi run"}`, Failed: true}
	}
	if !detached {
		out, err := e.Tasks.RunTasks(ctx, reqs)
		if err != nil {
			return failJSON(err)
		}
		return Result{Content: out}
	}
	return e.spawnBackground(ctx, reqs)
}

// spawnBackground hands every detached request to the hub and returns the run
// ids immediately. Foreground requests in the same call still run to completion,
// so a mixed call reports both: the ids to collect later, and the finished
// results now.
//
// The spawn carries a fresh context, not the caller's: a background run that
// died when the model's turn ended would not be background work at all. The run
// is still bounded by the runner's own limits and by the session's life.
func (e *Executor) spawnBackground(ctx context.Context, reqs []TaskRequest) Result {
	var ids []string
	var foreground []TaskRequest
	for i := range reqs {
		req := reqs[i] // each goroutine closes over its own request
		if !req.Background {
			foreground = append(foreground, req)
			continue
		}
		id := e.Hub.Spawn(req.Title, req.Role, "", func(ctx context.Context) (*hub.Result, int, error) {
			out, err := e.Tasks.RunTasks(ctx, []TaskRequest{req})
			if err != nil {
				return nil, 0, err
			}
			return &hub.Result{Content: out}, 0, nil
		})
		ids = append(ids, id)
		if e.Log != nil {
			e.Log("task_background", map[string]any{"id": id, "title": req.Title, "role": req.Role})
		}
	}
	payload := map[string]any{
		"background": ids,
		"note":       "started in the background; collect each with agent_output(id, action=wait|status|kill)",
	}
	if len(foreground) > 0 {
		out, err := e.Tasks.RunTasks(ctx, foreground)
		if err != nil {
			return failJSON(err)
		}
		// The call mixed detached and awaited work: hand back what finished and
		// name what is still running, so the model can act on both. A runner
		// that answers with something other than JSON is carried as a string
		// rather than failing the whole call — the detached runs are already
		// going, and their ids are the part the model cannot recover.
		if json.Valid([]byte(out)) {
			payload["foreground"] = json.RawMessage(out)
		} else {
			payload["foreground"] = out
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: string(b)}
}

// taskContextMaxBytes bounds the per-task handoff. The worker bundle is bounded
// again at render time; this only stops a runaway handoff from filling the tool
// call itself.
const taskContextMaxBytes = 4000

// normalizeTaskContext trims a task handoff and caps its size without splitting
// a multi-byte rune. An empty result simply means "nothing to hand over".
func normalizeTaskContext(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= taskContextMaxBytes {
		return s
	}
	cut := s[:taskContextMaxBytes]
	return strings.TrimSpace(strings.ToValidUTF8(cut, "")) + "\n…[handoff truncated]"
}

func decodeTaskRequests(args string) ([]TaskRequest, error) {
	var raw struct {
		Tasks json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(args), &raw); err != nil {
		return nil, err
	}
	body := bytes.TrimSpace(raw.Tasks)
	if len(body) == 0 {
		return nil, fmt.Errorf("task: missing tasks")
	}
	if body[0] == '"' {
		var inner string
		if err := json.Unmarshal(body, &inner); err != nil {
			return nil, err
		}
		body = []byte(inner)
	}
	var reqs []TaskRequest
	if err := json.Unmarshal(body, &reqs); err != nil {
		return nil, fmt.Errorf("task: tasks must be an array of objects: %w", err)
	}
	return reqs, nil
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func failJSON(err error) Result {
	return Result{Content: fmt.Sprintf(`{"error":%q}`, err.Error()), Failed: true}
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".venv", "target", "dist":
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
