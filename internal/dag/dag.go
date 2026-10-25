// Package dag is the lean sequential-first orchestration engine from
// DESIGN §5.2: nodes, dependencies, gates, and a bounded revise cycle. It runs
// one node at a time; parallelism belongs to subagent fan-out, not the plan.
package dag

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kintsugi/internal/acp"
	"kintsugi/internal/gate"
)

type Node struct {
	ID         string      `json:"id" yaml:"id"`
	DependsOn  []string    `json:"depends_on,omitempty" yaml:"depends_on"`
	Task       string      `json:"task" yaml:"task"`
	AssignedTo string      `json:"assigned_to,omitempty" yaml:"assigned_to"`
	Acceptance []string    `json:"acceptance,omitempty" yaml:"acceptance"`
	Gates      []gate.Spec `json:"gates,omitempty" yaml:"gates"`
	Budget     Budget      `json:"budget,omitempty" yaml:"budget"`
}

type Budget struct {
	MaxTurns int `json:"max_turns,omitempty" yaml:"max_turns"`
}

type Plan struct {
	ID    string `json:"id" yaml:"id"`
	Nodes []Node `json:"nodes" yaml:"nodes"`
}

// Runner executes one node attempt. Feedback is empty on the first attempt
// and carries gate findings on a revise cycle.
type Runner interface {
	RunNode(ctx context.Context, node Node, feedback string) (output string, err error)
}

type Logger interface {
	Log(kind string, v any)
}

type nopLogger struct{}

func (nopLogger) Log(string, any) {}

type State struct {
	ID        string        `json:"id"`
	State     string        `json:"state"` // done | failed | blocked
	Output    string        `json:"output,omitempty"`
	Feedback  string        `json:"feedback,omitempty"`
	Gates     []gate.Result `json:"gates,omitempty"`
	Revisions int           `json:"revisions"`
}

type Report struct {
	Plan  string  `json:"plan"`
	Nodes []State `json:"nodes"`
}

func (r Report) Done() bool {
	for _, n := range r.Nodes {
		if n.State != "done" {
			return false
		}
	}
	return len(r.Nodes) > 0
}

// Engine runs a plan. The zero value is not usable: it needs a Runner (unless
// every node is an external ACP agent, see ACPSettings).
type Engine struct {
	Runner    Runner
	Diff      func(ctx context.Context) (string, error)
	Reviewer  *gate.Reviewer
	GateFn    func(ctx context.Context, spec gate.Spec, diff string) gate.Result
	Workspace string
	Scope     string
	// PlanArtifact is the plan document a plan-review gate judges. Empty means
	// no plan was supplied, and such a gate is still handed the diff rather than
	// silently passing — but the gate should be given a plan, and the caller
	// that configured a plan-review is the one that knows where it is.
	PlanArtifact string
	MaxRevisions int
	Log          Logger
	// ACP tunes the built-in external-agent adapter. The zero value is the
	// recommended configuration: adapter on, package-default timeouts,
	// read-only observer.
	ACP ACPSettings
}

func (e *Engine) Run(ctx context.Context, plan Plan) (Report, error) {
	if err := validate(plan); err != nil {
		return Report{}, err
	}
	if e.Runner == nil && !e.acpOnly(plan) {
		return Report{}, fmt.Errorf("dag: no runner")
	}
	log := e.Log
	if log == nil {
		log = nopLogger{}
	}
	maxRev := e.MaxRevisions
	if maxRev < 0 {
		maxRev = 0
	}
	if maxRev == 0 {
		maxRev = 1
	}

	report := Report{Plan: plan.ID}
	states := map[string]State{}
	for _, n := range plan.Nodes {
		if !depsDone(n, states) {
			states[n.ID] = State{ID: n.ID, State: "blocked", Output: "dependencies did not complete"}
			report.Nodes = append(report.Nodes, State{ID: n.ID, State: "blocked", Output: "dependencies did not complete"})
			continue
		}
		st := e.runNode(ctx, n, maxRev, log)
		states[n.ID] = st
		report.Nodes = append(report.Nodes, st)
	}
	return report, nil
}

// acpOnly reports whether every node in the plan is an external ACP agent, in
// which case the Engine needs no Runner of its own.
func (e *Engine) acpOnly(plan Plan) bool {
	if e.ACP.Off || len(plan.Nodes) == 0 {
		return false
	}
	for _, n := range plan.Nodes {
		if _, ok := ACPCommand(n); !ok {
			return false
		}
	}
	return true
}

// runnerFor picks who executes a node. A node assigned to an external agent
// (assigned_to: "acp:<command> [args...]") goes through the built-in ACP
// adapter; every other node goes to the Engine's Runner untouched.
func (e *Engine) runnerFor(n Node, log Logger) Runner {
	if !e.ACP.Off {
		if _, ok := ACPCommand(n); ok {
			settings := e.ACP
			if settings.Workspace == "" {
				settings.Workspace = e.Workspace
			}
			return &ACPRunner{Inner: e.Runner, Settings: settings, Log: log}
		}
	}
	return e.Runner
}

func (e *Engine) runNode(ctx context.Context, node Node, maxRev int, log Logger) State {
	st := State{ID: node.ID, State: "running"}
	runner := e.runnerFor(node, log)
	if runner == nil {
		st.State, st.Output = "failed", "dag: no runner"
		log.Log("dag_node", map[string]any{"node": node.ID, "state": st.State, "error": st.Output})
		return st
	}
	feedback := ""
	for attempt := 0; ; attempt++ {
		output, err := runner.RunNode(ctx, node, feedback)
		if err != nil {
			st.State, st.Output = "failed", err.Error()
			log.Log("dag_node", map[string]any{"node": node.ID, "state": st.State, "error": err.Error()})
			return st
		}
		st.Output = output

		diff, derr := e.diff(ctx)
		if derr != nil {
			log.Log("dag_diff_error", map[string]any{"node": node.ID, "error": derr.Error()})
		}
		var results []gate.Result
		for _, spec := range node.Gates {
			// The artifact a gate judges depends on what the gate is for: an
			// opsec-review judges the diff, a plan-review judges the plan. Handing
			// the diff to a plan-review asks the reviewer whether the *change*
			// stays in scope when the question is whether the *proposal* does.
			artifact := diff
			if spec.Kind == "plan-review" && e.PlanArtifact != "" {
				artifact = e.PlanArtifact
			}
			var res gate.Result
			if e.GateFn != nil {
				res = e.GateFn(ctx, spec, artifact)
			} else {
				res = gate.Run(ctx, spec, e.Reviewer, e.Workspace, e.Scope, artifact)
			}
			res.Blocking = spec.Blocking
			results = append(results, res)
			log.Log("dag_gate", map[string]any{"node": node.ID, "result": res})
		}
		st.Gates = append(st.Gates, results...)

		blockingFailures := false
		for _, res := range results {
			if !res.Passed && res.Blocking {
				blockingFailures = true
			}
		}
		if !blockingFailures {
			st.State = "done"
			log.Log("dag_node", map[string]any{"node": node.ID, "state": st.State, "revisions": st.Revisions})
			return st
		}

		if st.Revisions >= maxRev {
			st.State = "blocked"
			st.Feedback = gate.Feedback(results)
			log.Log("dag_node", map[string]any{"node": node.ID, "state": st.State, "revisions": st.Revisions})
			return st
		}
		st.Revisions++
		feedback = gate.Feedback(results)
		st.Feedback = feedback
		log.Log("dag_revise", map[string]any{"node": node.ID, "revision": st.Revisions, "feedback": feedback})
	}
}

func (e *Engine) diff(ctx context.Context) (string, error) {
	if e.Diff == nil {
		return "", nil
	}
	return e.Diff(ctx)
}

func depsDone(n Node, states map[string]State) bool {
	for _, dep := range n.DependsOn {
		if states[dep].State != "done" {
			return false
		}
	}
	return true
}

func validate(plan Plan) error {
	if len(plan.Nodes) == 0 {
		return fmt.Errorf("dag: plan has no nodes")
	}
	seen := map[string]bool{}
	for _, n := range plan.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			return fmt.Errorf("dag: node with empty id")
		}
		if seen[n.ID] {
			return fmt.Errorf("dag: duplicate node %q", n.ID)
		}
		seen[n.ID] = true
	}
	for _, n := range plan.Nodes {
		for _, dep := range n.DependsOn {
			if !seen[dep] {
				return fmt.Errorf("dag: node %q depends on unknown node %q", n.ID, dep)
			}
		}
	}
	// Cycle check: repeatedly remove nodes whose deps are resolved.
	resolved := map[string]bool{}
	for progress := true; progress; {
		progress = false
		for _, n := range plan.Nodes {
			if resolved[n.ID] {
				continue
			}
			ok := true
			for _, dep := range n.DependsOn {
				if !resolved[dep] {
					ok = false
					break
				}
			}
			if ok {
				resolved[n.ID] = true
				progress = true
			}
		}
	}
	if len(resolved) != len(plan.Nodes) {
		return fmt.Errorf("dag: dependency cycle")
	}
	return nil
}

// ---------- external ACP agents as nodes (M12-A) ----------

// ACPPrefix marks a DAG node that is executed by an external ACP agent instead
// of the harness's own agent loop.
//
//	node:
//	  id: research
//	  task: summarize what ACP promises and where it breaks
//	  assigned_to: "acp:claude-code --print --model sonnet"
//
// Everything after the prefix is the command line that spawns the ACP server
// (quoted shell-ish, resolved against PATH — never a shell). The node's task
// text, its acceptance criteria and any gate feedback become the prompt; the
// agent's final message becomes the node's output. Non-acp nodes are untouched
// and keep going to Engine.Runner.
//
// An external agent is a worker, not a deputy: see package acp for the
// read-only observer contract. The harness executes nothing on the agent's
// behalf, so a node assigned to an external agent cannot be used to widen the
// harness's own tool policy.
const ACPPrefix = "acp:"

// ACPSettings configures the built-in external-agent adapter. The zero value is
// the recommended one: adapter on, package-default timeouts, read-only
// observer.
type ACPSettings struct {
	// Off disables the adapter entirely: an `acp:` node then goes to the
	// Engine's Runner like any other, which normally means it fails as an
	// unknown role rather than spawning anything.
	Off bool
	// Timeout bounds the handshake and each control request. Default
	// acp.DefaultTimeout.
	Timeout time.Duration
	// PromptTimeout bounds one prompt turn (one node attempt). Default
	// acp.DefaultPromptTimeout. A cancelled DAG run interrupts it earlier.
	PromptTimeout time.Duration
	// CancelGrace is how long a cancelled node waits after session/cancel
	// before the agent process is killed. Default acp.CancelTimeout.
	CancelGrace time.Duration
	// Workspace is the agent's working directory and the session cwd. Empty
	// falls back to Engine.Workspace.
	Workspace string
	// Env adds environment variables for the agent process (names and values
	// the operator already put in the plan or config — nothing is read from
	// the harness's own credential store).
	Env map[string]string
	// Observer records and refuses the agent's tool calls. Nil means the
	// read-only observer; there is no setting that makes the harness execute
	// on the agent's behalf.
	Observer acp.Observer
	// Log receives per-node diagnostics. Nil falls back to the Engine's Log.
	Log Logger
}

func (s ACPSettings) logger(fallback Logger) Logger {
	if s.Log != nil {
		return s.Log
	}
	if fallback != nil {
		return fallback
	}
	return nopLogger{}
}

// ACPCommand extracts the agent command line from a node. The prefix is read
// from assigned_to, which is where a node says who runs it; a leading `acp:` on
// the task text is accepted too, in which case the first line names the agent
// and the rest stays the prompt.
func ACPCommand(n Node) (string, bool) {
	if cmd, ok := stripACPPrefix(n.AssignedTo); ok {
		return cmd, true
	}
	return stripACPPrefix(n.Task)
}

func stripACPPrefix(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(trimmed), ACPPrefix) {
		return "", false
	}
	cmd := strings.TrimSpace(trimmed[len(ACPPrefix):])
	if cmd == "" {
		return "", false
	}
	return cmd, true
}

// ACPTaskText is the prompt sent to an external agent for a node: the task
// (without a leading `acp:` line), its acceptance criteria and the gate
// feedback from the previous attempt. The shape matches what the harness's own
// runner builds, so a node behaves the same whichever agent runs it.
func ACPTaskText(n Node, feedback string) string {
	task := strings.TrimSpace(n.Task)
	if _, ok := stripACPPrefix(task); ok {
		if i := strings.IndexByte(task, '\n'); i >= 0 {
			task = strings.TrimSpace(task[i+1:])
		} else {
			task = ""
		}
	}
	var b strings.Builder
	b.WriteString(task)
	if len(n.Acceptance) > 0 {
		b.WriteString("\n\nAcceptance criteria:\n- " + strings.Join(n.Acceptance, "\n- "))
	}
	if feedback != "" {
		b.WriteString("\n\nGate feedback from the previous attempt (revise, then finish):\n" + feedback)
	}
	// The agent cannot call back into the harness — every tool call it sends is
	// refused — so say so once instead of letting it discover that by trial and
	// error mid-turn.
	b.WriteString("\n\nEnvironment: you are a read-only worker inside AgentForge. " +
		"This harness executes nothing on your behalf: tool calls you send are recorded and refused. " +
		"Do the work yourself and report the result in your final message.")
	return strings.TrimSpace(b.String())
}

// ACPRunner is the Runner adapter for external ACP agents. Nodes carrying the
// `acp:` prefix go through internal/acp; every other node is delegated to
// Inner. Inner may be nil for a plan made entirely of external agents.
//
// Cancellation propagates: when ctx is done the adapter sends session/cancel,
// waits CancelGrace for the agent to stop on its own, then kills the process. A
// cancelled node returns an error wrapping acp.ErrCancelled, so the Engine marks
// it failed rather than done.
type ACPRunner struct {
	Inner    Runner
	Settings ACPSettings
	Log      Logger
}

func (r *ACPRunner) RunNode(ctx context.Context, node Node, feedback string) (string, error) {
	cmdline, ok := ACPCommand(node)
	if !ok {
		if r.Inner == nil {
			return "", fmt.Errorf("dag: no runner for node %q", node.ID)
		}
		return r.Inner.RunNode(ctx, node, feedback)
	}

	prompt := ACPTaskText(node, feedback)
	observer := r.Settings.Observer
	if observer == nil {
		observer = &acp.ReadOnlyObserver{}
	}
	log := r.Settings.logger(r.Log)
	opts := acp.RunOptions{
		Options: acp.Options{
			Timeout:       r.Settings.Timeout,
			PromptTimeout: r.Settings.PromptTimeout,
			Observer:      observer,
			Log:           logAdapter{log},
			Name:          fmt.Sprintf("acp:%s (node %s)", cmdline, node.ID),
		},
		Dir:         r.Settings.Workspace,
		Env:         r.Settings.Env,
		CancelGrace: r.Settings.CancelGrace,
	}

	log.Log("dag_acp_start", map[string]any{"node": node.ID, "command": cmdline, "prompt_chars": len(prompt)})
	res, err := acp.Run(ctx, cmdline, prompt, opts)
	calls := observedCalls(observer)
	log.Log("dag_acp_done", map[string]any{
		"node": node.ID, "command": cmdline, "error": errString(err),
		"stop_reason": res.StopReason, "output_chars": len(res.Text),
		"tool_calls_observed": len(calls), "updates": len(res.Updates),
	})
	for _, c := range calls {
		log.Log("dag_acp_tool_refused", map[string]any{
			"node": node.ID, "kind": c.Kind, "tool_call": c.Summary(), "reason": c.Reason,
		})
	}
	if err != nil {
		if len(calls) > 0 {
			return "", fmt.Errorf("%w (the agent asked for %d tool call(s), all refused read-only)", err, len(calls))
		}
		return "", err
	}
	if strings.TrimSpace(res.Text) == "" {
		return "", fmt.Errorf("dag: external agent %q returned no output for node %q", cmdline, node.ID)
	}
	return res.Text, nil
}

// observedCalls pulls the recorded tool calls out of an observer, whatever its
// concrete type.
func observedCalls(o acp.Observer) []acp.ToolCall {
	if lister, ok := o.(interface{ Calls() []acp.ToolCall }); ok {
		return lister.Calls()
	}
	return nil
}

// logAdapter forwards internal/acp diagnostics to the DAG Logger.
type logAdapter struct{ l Logger }

func (a logAdapter) Log(kind string, v any) {
	if a.l != nil {
		a.l.Log(kind, v)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
