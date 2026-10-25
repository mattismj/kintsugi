// Package subagent fans bounded workers out into isolated git worktrees
// (DESIGN §5.2). Workers never see the parent conversation; they get a
// self-contained prompt and return a schema-validated result plus their diff.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/agent"
	"kintsugi/internal/config"
	"kintsugi/internal/hooks"
	"kintsugi/internal/mcp"
	"kintsugi/internal/memory"
	"kintsugi/internal/provider"
	"kintsugi/internal/redact"
	"kintsugi/internal/sandbox"
	"kintsugi/internal/scope"
	"kintsugi/internal/tools"
)

type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Note     string `json:"note"`
}

type Result struct {
	Title        string    `json:"title"`
	Status       string    `json:"status"` // done | failed | invalid
	Summary      string    `json:"summary,omitempty"`
	Findings     []Finding `json:"findings,omitempty"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
	DiffStat     string    `json:"diff_stat,omitempty"`
	Diff         string    `json:"diff,omitempty"`
	Worktree     string    `json:"worktree,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// ---------- M8.5.5 per-worker context bundles ----------
//
// A bundle is the continuity a worker inherits from the parent session: the
// operator's authorization/domain/scope frame, prior *accepted* work, role-bound
// knowledge excerpts and a typed handoff for this task.
//
// CRITICAL INVARIANT: a refusal is never memory. The bundle types below have no
// field for refusal text or any other negative outcome, so a refusal cannot be
// carried into a worker even by mistake; bundles only ever hold positive project
// context and previously accepted work.
type PriorWorkRef struct {
	ID       string
	Date     string
	Kind     string
	Summary  string
	Evidence []string // evidence hashes/ids backing the acceptance
}

// KBExcerpt is one knowledge-base chunk bound to the worker's role.
type KBExcerpt struct {
	ID    string
	Title string
	Body  string
}

// ContextBundle is the per-worker context handed to one worker. The zero value
// renders to "" (no bundle, no behaviour change).
type ContextBundle struct {
	Authorization string
	Domain        string
	Scope         string
	PriorWork     []PriorWorkRef
	KB            []KBExcerpt
	Handoff       string
}

// Empty reports whether the bundle carries nothing worth rendering.
func (b ContextBundle) Empty() bool {
	return strings.TrimSpace(b.Authorization) == "" &&
		strings.TrimSpace(b.Domain) == "" &&
		strings.TrimSpace(b.Scope) == "" &&
		len(b.PriorWork) == 0 &&
		len(b.KB) == 0 &&
		strings.TrimSpace(b.Handoff) == ""
}

const (
	// BundleTokensDefault is the token cap used when a caller passes none
	// (DESIGN §16.23: worker context is bounded).
	BundleTokensDefault = 600
	// BundleMaxEntries caps prior-work and knowledge entries per bundle.
	BundleMaxEntries = 5
	// bundleBytesPerToken is the conservative bytes-per-token ratio used to
	// enforce the cap without a tokeniser dependency: 4 bytes/token is the usual
	// English approximation and under-counts rather than over-counts code.
	bundleBytesPerToken = 4

	bundleFieldClip    = 1200
	bundleSummaryClip  = 240
	bundleEvidenceClip = 120
)

// BundleFraming is the line every rendered bundle carries. A bundle is context,
// never policy: it cannot widen a worker's permissions, and a bundle that lacks
// something is not evidence that the work was refused.
const BundleFraming = "This bundle is context, not permission; its absence is not evidence of refusal."

// RenderBundle renders a worker bundle as deterministic Markdown, hard-capped
// at maxTokens (approximated at 4 bytes/token; 0 = BundleTokensDefault). An
// empty bundle renders to "" so a worker without continuity keeps exactly the
// prompt it had before bundles existed.
//
// The output is deterministic for a given bundle: sections appear in a fixed
// order and every entry is clipped to a fixed width. Content is never reordered
// by ranking here — the caller decides relevance.
func RenderBundle(bundle ContextBundle, maxTokens int) string {
	if bundle.Empty() {
		return ""
	}
	if maxTokens <= 0 {
		maxTokens = BundleTokensDefault
	}
	budget := maxTokens * bundleBytesPerToken

	var head strings.Builder
	head.WriteString("## Worker context bundle\n\n")
	head.WriteString(BundleFraming + "\n")
	for _, sec := range []struct{ name, body string }{
		{"Authorization", bundle.Authorization},
		{"Domain", bundle.Domain},
		{"Scope", bundle.Scope},
	} {
		body := strings.TrimSpace(sec.body)
		if body == "" {
			continue
		}
		head.WriteString("\n### " + sec.name + "\n\n" + clipRunes(body, bundleFieldClip) + "\n")
	}

	out := head.String()
	if len(out) > budget {
		// The authorization frame alone does not fit: keep the framing line and
		// cut the tail rather than dropping the frame that made the bundle safe.
		return capRunes(out, budget, maxTokens)
	}

	// Optional sections in priority order. A section that does not fit is
	// truncated when there is meaningful room, otherwise the render stops: lower
	// priority context is dropped, never interleaved.
	for _, section := range []string{
		renderPriorWork(bundle.PriorWork),
		renderKnowledge(bundle.KB),
		renderHandoff(bundle.Handoff),
	} {
		if section == "" {
			continue
		}
		room := budget - len(out)
		if room <= 0 {
			break
		}
		if len(section) <= room {
			out += section
			continue
		}
		if trimmed, ok := truncateTo(section, room, maxTokens); ok {
			out += trimmed
		}
		break
	}
	return out
}

func renderPriorWork(refs []PriorWorkRef) string {
	var b strings.Builder
	n := 0
	for _, r := range refs {
		if n >= BundleMaxEntries {
			break
		}
		n++
		line := "- "
		if d := strings.TrimSpace(r.Date); d != "" {
			line += d + " "
		}
		if k := strings.TrimSpace(r.Kind); k != "" {
			line += k + " "
		}
		if id := strings.TrimSpace(r.ID); id != "" {
			line += id + " "
		}
		line = strings.TrimSpace(line)
		if s := oneLine(r.Summary); s != "" {
			line += " — " + clipRunes(s, bundleSummaryClip)
		}
		b.WriteString(line + "\n")
		if len(r.Evidence) > 0 {
			ev := make([]string, 0, len(r.Evidence))
			for _, e := range r.Evidence {
				if e = strings.TrimSpace(e); e != "" {
					ev = append(ev, clipRunes(e, bundleEvidenceClip))
				}
				if len(ev) >= 4 {
					break
				}
			}
			if len(ev) > 0 {
				b.WriteString("  evidence: " + strings.Join(ev, ", ") + "\n")
			}
		}
	}
	if n == 0 {
		return ""
	}
	return "\n## Prior accepted work\n\n" + b.String()
}

func renderKnowledge(excerpts []KBExcerpt) string {
	var b strings.Builder
	n := 0
	for _, e := range excerpts {
		if n >= BundleMaxEntries {
			break
		}
		body := strings.TrimSpace(e.Body)
		title := strings.TrimSpace(e.Title)
		if body == "" && title == "" {
			continue
		}
		n++
		heading := title
		if id := strings.TrimSpace(e.ID); id != "" {
			if heading == "" {
				heading = id
			} else {
				heading += " (" + id + ")"
			}
		}
		if heading == "" {
			heading = "excerpt"
		}
		b.WriteString("\n### " + heading + "\n")
		if body != "" {
			b.WriteString("\n" + clipRunes(body, bundleFieldClip) + "\n")
		}
	}
	if n == 0 {
		return ""
	}
	return "\n## Knowledge\n" + b.String()
}

func renderHandoff(handoff string) string {
	handoff = strings.TrimSpace(handoff)
	if handoff == "" {
		return ""
	}
	return "\n## Handoff\n\n" + clipRunes(handoff, bundleFieldClip) + "\n"
}

// capRunes hard-truncates already-rendered text to budget bytes, cutting at the
// last line boundary so a bounded bundle never ends mid-line or mid-rune.
func capRunes(s string, budget, maxTokens int) string {
	if len(s) <= budget {
		return s
	}
	marker := fmt.Sprintf("\n…[bundle truncated to %d tokens]", maxTokens)
	if budget <= len(marker) {
		return strings.ToValidUTF8(s[:budget], "")
	}
	cut := s[:budget-len(marker)]
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.ToValidUTF8(cut, "") + marker
}

// truncateTo cuts one optional section down to room bytes and reports whether
// there was enough room for a useful fragment.
func truncateTo(section string, room, maxTokens int) (string, bool) {
	marker := fmt.Sprintf("\n…[bundle truncated to %d tokens]", maxTokens)
	if room <= len(marker)+64 {
		return "", false
	}
	cut := section[:room-len(marker)]
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.ToValidUTF8(cut, "") + marker, true
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

type Config struct {
	Workspace    string
	Client       *provider.Client
	SystemPrompt string // parent compiled stack, for house rules
	Policy       tools.Policy
	// Security carries the operator's sandbox and scope configuration so a
	// worker is isolated exactly like the parent (M11).
	Security     config.Config
	Memory       *memory.Store
	Log          func(kind string, v any)
	Max          int
	Timeout      time.Duration
	WorktreeRoot string // empty = os.TempDir()/kintsugi-worktrees
	Keep         bool
	MaxLoops     int
	Offline      bool

	// ResolveClient picks the provider client for one worker role (WP-4.1).
	// nil means every worker uses Client. An error, or a nil client, falls back
	// to Client and logs subagent_role_fallback: role routing is an
	// optimisation, never a reason to fail a task.
	ResolveClient func(role string) (*provider.Client, error)

	// MCP is the parent-owned MCP manager shared with workers (WP-4.5). Workers
	// get the same tool routes but never own the child processes: the manager
	// stays open and is closed once, by whoever started it. nil disables MCP in
	// workers.
	MCP *mcp.Manager

	// ContextBundle is the bundle every worker receives (M8.5.5). It is rendered
	// per worker, redacted, and bounded before it reaches a system prompt. The
	// zero value means "no bundle" and leaves the worker prompt unchanged.
	ContextBundle ContextBundle

	// BundleFor, when set, resolves the bundle for one task and role, overriding
	// ContextBundle. It is called once per worker, so a fan-out gets per-task
	// bundles with no cross-worker leakage. It may be called concurrently from
	// worker goroutines and must be safe for concurrent use. A nil BundleFor uses
	// ContextBundle for every worker.
	BundleFor func(task, role string) ContextBundle

	// BundleTokens caps the rendered bundle in tokens (0 = BundleTokensDefault).
	BundleTokens int

	// Hooks fires the SubagentStart/SubagentStop lifecycle events (M9.1).
	// Optional: a nil runner means no events fire and the manager behaves
	// exactly as it did before hooks existed.
	Hooks *hooks.Runner
	// SessionID and CWD are hook metadata: the session the fan-out belongs
	// to and the workspace it runs in. CWD defaults to Workspace.
	SessionID string
	CWD       string
	// ResolveModel returns the provider/model ref a role resolves to, for
	// the SubagentStart/SubagentStop events. Optional: the worker's own
	// client model id is reported when it is unset.
	ResolveModel func(role string) string
}

// DefaultRole is the model role used when a task item does not name one.
const DefaultRole = "coder"

type Manager struct {
	Cfg Config
}

const workerContract = `You are a bounded subagent working in an isolated git worktree.
You cannot see the parent conversation and you must not reach outside this worktree.
Do the assigned task, then return ONLY JSON, no prose and no code fences:
{"status":"done|failed","summary":"one paragraph","findings":[{"severity":"info|low|medium|high","file":"path","line":0,"note":"..."}],"changed_files":["path"]}
List only findings that matter to the parent; use an empty list when there are none.`

// RunTasks implements tools.TaskRunner.
func (m *Manager) RunTasks(ctx context.Context, reqs []tools.TaskRequest) (string, error) {
	if len(reqs) == 0 {
		return "", fmt.Errorf("subagent: no tasks")
	}
	max := m.Cfg.Max
	if max <= 0 {
		max = 3
	}
	if len(reqs) > max {
		return "", fmt.Errorf("subagent: %d tasks exceeds max %d", len(reqs), max)
	}
	if err := m.requireGit(ctx); err != nil {
		return "", err
	}

	root := m.Cfg.WorktreeRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "kintsugi-worktrees")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	runRoot, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return "", err
	}

	results := make([]Result, len(reqs))
	sem := make(chan struct{}, max)
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(i int, req tools.TaskRequest) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = m.runOne(ctx, runRoot, i, req)
		}(i, req)
	}
	wg.Wait()

	report := struct {
		Results  []Result `json:"results"`
		Findings string   `json:"findings_markdown"`
	}{Results: results, Findings: Merge(results)}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// worktreeMu serializes the git worktree operations of a fan-out.
//
// Two workers that add and remove worktrees in the same repository at the
// same time race on .git/worktrees, and the loser of that race leaves a stale
// registration behind: the directory is gone, the metadata is not, and the
// repository keeps listing a worktree that no longer exists. The git calls
// are short, so serializing them costs nothing and keeps the worktree list
// honest.
var worktreeMu sync.Mutex

func (m *Manager) runOne(ctx context.Context, root string, index int, req tools.TaskRequest) (res Result) {
	res = Result{Title: req.Title, Status: "failed"}
	role := req.ModelRole()
	client := m.clientFor(role)
	model := m.modelFor(role, client)

	// SubagentStart (M9.1): the event fires before the worker runs. A deny
	// stops the worker: no worktree, no model request, and the parent gets
	// a failed result that names the reason.
	if dec := m.fireWorkerHook(ctx, hooks.SubagentStart, req, role, model, ""); dec.Decision == "deny" {
		res.Error = "subagent blocked by hook: " + hookDenyReason(dec)
		return res
	}
	// SubagentStop (M9.1): fires once the worker has finished, whatever the
	// outcome. It observes; it cannot undo work that already ran.
	defer func() {
		m.fireWorkerHook(context.WithoutCancel(ctx), hooks.SubagentStop, req, role, model, res.Status)
	}()

	worktree := filepath.Join(root, fmt.Sprintf("%02d-%s", index+1, slug(req.Title)))
	res.Worktree = worktree

	worktreeMu.Lock()
	out, err := m.git(ctx, m.Cfg.Workspace, "worktree", "add", "--detach", worktree, "HEAD")
	worktreeMu.Unlock()
	if err != nil {
		res.Error = "worktree add: " + err.Error() + ": " + truncate(string(out), 500)
		return res
	}
	defer func() {
		if m.Cfg.Keep {
			return
		}
		worktreeMu.Lock()
		_, _ = m.git(context.Background(), m.Cfg.Workspace, "worktree", "remove", "--force", worktree)
		// A removal that lost a race leaves the registration behind; prune
		// drops it (it only removes entries whose directory is gone).
		_, _ = m.git(context.Background(), m.Cfg.Workspace, "worktree", "prune")
		worktreeMu.Unlock()
		_ = os.RemoveAll(worktree)
		res.Worktree = ""
	}()

	timeout := m.Cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	policy := workerPolicy(m.Cfg.Policy, req.Agent)
	ex := newWorkerExecutor(m.Cfg, worktree, policy)
	loop := &agent.Loop{
		Client: client, Tools: ex, Log: loggerFunc(m.Cfg.Log),
		MaxLoops: m.Cfg.MaxLoops,
	}
	if loop.MaxLoops <= 0 {
		loop.MaxLoops = 10
	}
	system := m.workerSystem(req, role)

	final, _, err := loop.RunConversation(runCtx, system, nil, req.Prompt)
	if err != nil {
		res.Error = err.Error()
		m.captureDiff(context.Background(), worktree, &res)
		return res
	}
	parsed, perr := ParseResult(final)
	if perr != nil {
		res.Status = "invalid"
		res.Summary = truncate(strings.TrimSpace(final), 1000)
		res.Error = "worker result was not valid JSON: " + perr.Error()
	} else {
		res.Status = parsed.Status
		res.Summary = parsed.Summary
		res.Findings = parsed.Findings
	}
	m.captureDiff(context.Background(), worktree, &res)
	return res
}

// fireWorkerHook fires one subagent lifecycle event (SubagentStart or
// SubagentStop). The event carries the task title, the role and the resolved
// model; SubagentStop also carries the worker's status.
//
// A hook failure is telemetry, never a decision: the runner already logged
// it, and the worker is not failed by it.
func (m *Manager) fireWorkerHook(ctx context.Context, name string, req tools.TaskRequest, role, model, status string) hooks.Decision {
	if m.Cfg.Hooks == nil {
		return hooks.Decision{}
	}
	extra := map[string]any{
		"title": req.Title,
		"role":  role,
	}
	if agent := strings.TrimSpace(req.Agent); agent != "" {
		extra["agent"] = agent
	}
	if status != "" {
		extra["status"] = status
	}
	cwd := m.Cfg.CWD
	if cwd == "" {
		cwd = m.Cfg.Workspace
	}
	dec, err := m.Cfg.Hooks.Fire(ctx, hooks.Event{
		Name:      name,
		SessionID: m.Cfg.SessionID,
		CWD:       cwd,
		Model:     model,
		Extra:     extra,
	})
	if err != nil && m.Cfg.Log != nil {
		m.Cfg.Log("subagent_hook_error", map[string]any{
			"event": name,
			"title": req.Title,
			"error": err.Error(),
		})
	}
	return dec
}

// modelFor reports the model a worker role resolves to, for the hook events:
// the ref the caller resolved, or the worker's own client model id.
func (m *Manager) modelFor(role string, client *provider.Client) string {
	if m.Cfg.ResolveModel != nil {
		if ref := strings.TrimSpace(m.Cfg.ResolveModel(role)); ref != "" {
			return ref
		}
	}
	if client != nil {
		return strings.TrimSpace(client.Model)
	}
	return ""
}

// hookDenyReason is the text a parent sees when a hook denied a worker.
func hookDenyReason(dec hooks.Decision) string {
	if r := strings.TrimSpace(dec.Reason); r != "" {
		return r
	}
	return "denied by hook"
}

// workerSystem composes the system prompt for one worker: the parent compiled
// stack, then the task's rendered context bundle (M8.5.5, omitted entirely when
// it is empty), then the result contract and the assignment lines.
//
// The concatenation is deliberately byte-identical to the pre-bundle prompt when
// the bundle is empty, so a manager without bundles behaves exactly as before.
func (m *Manager) workerSystem(req tools.TaskRequest, role string) string {
	system := strings.TrimSpace(m.Cfg.SystemPrompt) + "\n\n"
	if bundle := redact.Redact(RenderBundle(m.bundleFor(req, role), m.Cfg.BundleTokens)); bundle != "" {
		system += bundle + "\n\n"
	}
	system += workerContract + "\n\nSubagent assignment: " + req.Title
	if agent := strings.TrimSpace(req.Agent); agent != "" {
		system += "\nWorker role: " + agent
	}
	if role != DefaultRole {
		system += "\nModel role: " + role
	}
	return system
}

// bundleFor resolves the context bundle for one worker. BundleFor wins over
// Config.ContextBundle; the task-level handoff from the task tool is appended
// last so the most specific instruction is the final word. Every call returns a
// value copy, so one worker's bundle can never be mutated into another's.
func (m *Manager) bundleFor(req tools.TaskRequest, role string) ContextBundle {
	bundle := m.Cfg.ContextBundle
	if m.Cfg.BundleFor != nil {
		bundle = m.Cfg.BundleFor(req.Title, role)
	}
	if extra := strings.TrimSpace(req.Context); extra != "" {
		if handoff := strings.TrimSpace(bundle.Handoff); handoff != "" {
			bundle.Handoff = handoff + "\n\n" + extra
		} else {
			bundle.Handoff = extra
		}
	}
	return bundle
}

// captureDiff stages the worktree and records the worker's diff.
func (m *Manager) captureDiff(ctx context.Context, worktree string, res *Result) {
	if out, err := m.git(ctx, worktree, "add", "-A"); err == nil {
		_ = out
	}
	if out, err := m.git(ctx, worktree, "diff", "--cached", "--stat"); err == nil {
		res.DiffStat = strings.TrimSpace(string(out))
	}
	if out, err := m.git(ctx, worktree, "diff", "--cached", "--name-only"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				res.ChangedFiles = append(res.ChangedFiles, line)
			}
		}
	}
	if out, err := m.git(ctx, worktree, "diff", "--cached"); err == nil {
		res.Diff = truncate(string(out), 40000)
	}
}

// clientFor resolves the provider client for one worker role. It is the only
// place role→model routing can fail, and it never fails the task: a resolution
// error or an empty client means "use the parent client" plus a log event, so a
// misconfigured roles table degrades to pre-WP-4.1 behaviour instead of
// blocking the fan-out.
func (m *Manager) clientFor(role string) *provider.Client {
	if m.Cfg.ResolveClient == nil {
		return m.Cfg.Client
	}
	client, err := m.Cfg.ResolveClient(role)
	if err == nil && client != nil {
		return client
	}
	detail := "client is nil"
	if err != nil {
		detail = err.Error()
	}
	if m.Cfg.Log != nil {
		m.Cfg.Log("subagent_role_fallback", map[string]any{
			"role": role, "error": detail, "fallback": "parent client",
		})
	}
	return m.Cfg.Client
}

// newWorkerExecutor builds the tool executor for one worker. It mirrors
// App.AttachTools: the MCP manager is the parent's, shared not copied, so the
// MCP child processes outlive the worker and are closed exactly once by the
// owner. SkillsDir is deliberately unset: workers run in their own worktree and
// must not read the parent's skill bodies.
func newWorkerExecutor(cfg Config, worktree string, policy tools.Policy) *tools.Executor {
	ex := &tools.Executor{
		Workspace: worktree,
		Memory:    cfg.Memory,
		MCP:       cfg.MCP,
		Policy:    policy,
		Log:       cfg.Log,
		Offline:   cfg.Offline,
	}
	// A worker is a full agent: it inherits the sandbox and the declared scope,
	// resolved against its own worktree (same relative layout as the parent).
	ex.Sandbox = sandbox.FromConfig(cfg.Security.Sandbox)
	ex.Scope = scope.FromWorkspace(cfg.Security, worktree)
	return ex
}

// workerPolicy keeps read-only roles read-only at the permission layer. A
// coder cannot ask the operator, so writes inside its own worktree are allowed;
// deny rules still apply. researcher/opsec workers get no approver, so
// edit/bash fail closed.
func workerPolicy(base tools.Policy, role string) tools.Policy {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "researcher", "opsec", "reviewer":
		base.Yolo = false
	default:
		base.Yolo = true
	}
	return base
}

func (m *Manager) requireGit(ctx context.Context) error {
	out, err := m.git(ctx, m.Cfg.Workspace, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("subagents require a git workspace: %s", truncate(string(out), 300))
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("subagents require a git workspace")
	}
	return nil
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	return cmd.CombinedOutput()
}

// ParseResult accepts strict JSON, fenced JSON, or JSON embedded in prose and
// validates the result contract.
func ParseResult(s string) (Result, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Result{}, fmt.Errorf("no JSON object")
	}
	var res Result
	if err := json.Unmarshal([]byte(s[start:end+1]), &res); err != nil {
		return Result{}, err
	}
	res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	if res.Status != "done" && res.Status != "failed" {
		return Result{}, fmt.Errorf("status must be done or failed, got %q", res.Status)
	}
	for i := range res.Findings {
		switch strings.ToLower(strings.TrimSpace(res.Findings[i].Severity)) {
		case "info", "low", "medium", "high":
			res.Findings[i].Severity = strings.ToLower(strings.TrimSpace(res.Findings[i].Severity))
		default:
			res.Findings[i].Severity = "info"
		}
		res.Findings[i].Note = strings.TrimSpace(res.Findings[i].Note)
	}
	if len(res.Summary) > 4000 {
		res.Summary = res.Summary[:4000] + "…"
	}
	return res, nil
}

// Merge renders the deterministic findings section the parent sees; prose is
// never substituted for the typed result list.
func Merge(results []Result) string {
	var b strings.Builder
	b.WriteString("### Findings\n")
	count := 0
	for _, r := range results {
		for _, f := range r.Findings {
			count++
			line := f.File
			if f.Line > 0 {
				line = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(&b, "- [%s] %s — %s (%s)\n", f.Severity, line, f.Note, r.Title)
		}
	}
	if count == 0 {
		b.WriteString("- none\n")
	}
	b.WriteString("\n### Results\n")
	for _, r := range results {
		fmt.Fprintf(&b, "- %s — %s", r.Title, r.Status)
		if r.Summary != "" {
			b.WriteString(": " + truncate(oneLine(r.Summary), 240))
		}
		if len(r.ChangedFiles) > 0 {
			fmt.Fprintf(&b, " [%d file(s)]", len(r.ChangedFiles))
		}
		if r.Error != "" {
			b.WriteString(" — error: " + truncate(oneLine(r.Error), 200))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

type loggerFunc func(kind string, v any)

func (f loggerFunc) Log(kind string, v any) {
	if f != nil {
		f(kind, v)
	}
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
		if b.Len() >= 24 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "task"
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
