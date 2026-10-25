// Package hooks implements the lifecycle hook runtime (M9.1).
//
// A hook is an operator-configured command. The runner hands the event to the
// hook as one JSON object on stdin and reads a JSON decision object on stdout:
//
//	{"decision":"deny","reason":"...","additionalContext":"...","continue":true}
//
// Exit code 2 blocks the event and the hook's stderr becomes the reason. Any
// other non-zero exit is a hook failure: non-blocking, unless the rule sets
// on_failure = "block".
//
// Trust boundary: hooks may deny or constrain, they never grant. A hook that
// answers "allow" cannot widen what the permission layer allows, and a hook
// failure is never silently treated as an approval.
//
// Trust boundary of the rules themselves: the operator's (global) rules come
// from the operator's own config and always run; a workspace's project rules
// (<ws>/.kintsugi/hooks.toml) are commands the repository chose, so they
// run only in a workspace the operator marked as trusted (see trust.go).
//
// The runner never logs the payload it hands to a hook: a hook's stdin can
// carry tool arguments and file contents, so the log record carries the event
// name, the tool name, the rule and the outcome — nothing else.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"kintsugi/internal/config"
)

// Event names. The list mirrors config.Hooks.
const (
	SessionStart     = "SessionStart"
	UserPromptSubmit = "UserPromptSubmit"
	PreToolUse       = "PreToolUse"
	PostToolUse      = "PostToolUse"
	Stop             = "Stop"
	PreCompact       = "PreCompact"
	PostCompact      = "PostCompact"
	SubagentStart    = "SubagentStart"
	SubagentStop     = "SubagentStop"
	Notification     = "Notification"
	ModelSwitch      = "ModelSwitch"
)

// Events lists every event name the runtime knows.
var Events = []string{
	SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop,
	PreCompact, PostCompact, SubagentStart, SubagentStop, Notification,
	ModelSwitch,
}

// Timeout bounds (seconds).
const (
	DefaultTimeout = 600
	MaxTimeout     = 900
)

// Event is one lifecycle event handed to the hooks.
//
// ToolInput carries the raw arguments JSON for tool events and the operator's
// prompt text for prompt events. Extra adds event-specific keys (for example
// tool_use_id); they are merged into the JSON object on the hook's stdin.
type Event struct {
	Name      string
	SessionID string
	CWD       string
	ToolName  string
	ToolInput string
	Model     string
	Persona   string
	Extra     map[string]any
}

// Decision is what the hooks answered for one event.
//
// Decision is "allow", "deny", "ask" or "" (no decision). Reason explains a
// deny or an ask. AdditionalContext is text the caller appends to the
// model's context. Continue is a tri-state: nil means "not answered".
type Decision struct {
	Decision          string `json:"decision,omitempty"`
	Reason            string `json:"reason,omitempty"`
	AdditionalContext string `json:"additionalContext,omitempty"`
	SystemMessage     string `json:"systemMessage,omitempty"`
	Continue          *bool  `json:"continue,omitempty"`
}

// Runner fires the configured hooks for an event.
//
// Rules is keyed by event name (config.Hooks.For) and holds the operator's
// (global) rules. ProjectRules holds the rules a workspace's own hook file
// contributes (<ws>/.kintsugi/hooks.toml): those are repository-controlled
// commands, so they only run when Trusted is true.
//
// Log receives one record per hook run ("hook") and one record per skipped
// project attempt ("hook_trust_denied"). Dir is the working directory for
// hook commands (the workspace root); empty means the process's own
// directory.
type Runner struct {
	Rules        map[string][]config.HookRule
	ProjectRules map[string][]config.HookRule
	Log          func(kind string, v any)
	Dir          string
	// Trusted is the workspace trust flag. False means the project's own
	// hooks are skipped, and every attempt is logged as hook_trust_denied.
	Trusted bool
}

// New returns a runner for the given rule table.
func New(rules map[string][]config.HookRule, log func(string, any)) *Runner {
	return &Runner{Rules: rules, Log: log}
}

// NewWithProject returns a runner for the operator's rules plus a workspace's
// project rules and the workspace trust state. Untrusted project rules are
// loaded but never run.
func NewWithProject(rules, project map[string][]config.HookRule, trusted bool, log func(string, any)) *Runner {
	r := New(rules, log)
	return r.WithProject(project, trusted)
}

// WithProject attaches a workspace's project rules and its trust state to the
// runner. It is the seam the coordinator uses to teach the runner about the
// repository's own hook file.
func (r *Runner) WithProject(project map[string][]config.HookRule, trusted bool) *Runner {
	if r == nil {
		return nil
	}
	r.ProjectRules = project
	r.Trusted = trusted
	return r
}

// FromConfig maps a configured hook set onto the runner's rule table.
func FromConfig(h config.Hooks) map[string][]config.HookRule {
	rules := map[string][]config.HookRule{}
	for _, ev := range Events {
		if rs := h.For(ev); len(rs) > 0 {
			rules[ev] = rs
		}
	}
	return rules
}

// Fire runs every rule registered for the event and merges their decisions.
//
// A nil runner, an unknown event or a runner without rules is a no-op. A
// hook failure is reported as an error but never blocks by itself: the
// decision decides, the error is telemetry.
//
// The operator's rules always run. The workspace's project rules run only
// when the workspace is trusted: without the marker they are skipped, and
// the attempt is logged as hook_trust_denied.
func (r *Runner) Fire(ctx context.Context, ev Event) (Decision, error) {
	if r == nil || ctx == nil {
		return Decision{}, nil
	}
	rules := r.Rules[ev.Name]
	project := r.ProjectRules[ev.Name]
	if len(rules) == 0 && len(project) == 0 {
		return Decision{}, nil
	}
	payload, err := marshalEvent(ev)
	if err != nil {
		return Decision{}, err
	}

	var (
		merged   merge
		firstErr error
	)
	for _, rule := range rules {
		r.apply(ctx, rule, ev, payload, &merged, &firstErr)
	}
	if len(project) > 0 {
		if !r.Trusted {
			// The project's commands are repository-controlled: without the
			// operator's explicit trust they never run.
			r.logTrustDenied(ev, len(project))
		} else {
			for _, rule := range project {
				r.apply(ctx, rule, ev, payload, &merged, &firstErr)
			}
		}
	}
	return merged.result(), firstErr
}

// apply runs one rule (if it matches the event) and folds its decision into
// the merged result.
func (r *Runner) apply(ctx context.Context, rule config.HookRule, ev Event, payload []byte, merged *merge, firstErr *error) {
	if !matches(rule, ev) {
		return
	}
	if rule.Async {
		// Detached: the caller gets no decision for this rule and the
		// hook keeps running on its own.
		r.startAsync(rule, ev, payload)
		return
	}
	dec, err := r.run(ctx, rule, ev, payload)
	if err != nil && *firstErr == nil {
		*firstErr = err
	}
	merged.add(dec)
}

// logTrustDenied records a skipped project hook attempt. The record names the
// event and the rule count — never the command and never the payload.
func (r *Runner) logTrustDenied(ev Event, rules int) {
	if r.Log == nil {
		return
	}
	r.Log("hook_trust_denied", map[string]any{
		"event":  ev.Name,
		"tool":   ev.ToolName,
		"rules":  rules,
		"reason": "workspace is not trusted: project hooks are skipped (run `kintsugi hooks trust`)",
	})
}

// ModelSwitchEvent describes a model change for the ModelSwitch event.
//
// From is the model the session left, To the one it moved to; the new model
// is also reported as the event's model. SessionID, CWD and Persona are the
// usual session metadata.
type ModelSwitchEvent struct {
	SessionID string
	CWD       string
	From      string
	To        string
	Persona   string
}

// FireModelSwitch fires the ModelSwitch event: the session moved to another
// model.
//
// The coordinator calls this from the model switch path (a --model override, a
// role switch, a fallback). The event carries the session, the workspace, the
// persona and both models; a hook may deny the switch or add context.
func (r *Runner) FireModelSwitch(ctx context.Context, ms ModelSwitchEvent) (Decision, error) {
	extra := map[string]any{}
	if ms.From != "" {
		extra["from_model"] = ms.From
	}
	if ms.To != "" {
		extra["to_model"] = ms.To
	}
	return r.Fire(ctx, Event{
		Name:      ModelSwitch,
		SessionID: ms.SessionID,
		CWD:       ms.CWD,
		Model:     ms.To,
		Persona:   ms.Persona,
		Extra:     extra,
	})
}

// NotificationEvent describes an operator-facing message for the
// Notification event.
//
// Kind classifies the message ("approval", "refusal", "info", ...), Message
// is the text the operator sees. Neither is a permission: a notification hook
// observes and may add context.
type NotificationEvent struct {
	SessionID string
	CWD       string
	Kind      string
	Message   string
	Persona   string
}

// FireNotification fires the Notification event: something needs the
// operator's attention.
//
// The coordinator calls this from the approval path and the other places that
// raise a notification. The event carries the session, the workspace, the
// persona, the kind and the message.
func (r *Runner) FireNotification(ctx context.Context, n NotificationEvent) (Decision, error) {
	extra := map[string]any{}
	if n.Kind != "" {
		extra["kind"] = n.Kind
	}
	if n.Message != "" {
		extra["message"] = n.Message
	}
	return r.Fire(ctx, Event{
		Name:      Notification,
		SessionID: n.SessionID,
		CWD:       n.CWD,
		Persona:   n.Persona,
		Extra:     extra,
	})
}

// matches reports whether a rule applies to the event.
//
// An empty matcher or "*" matches every event. Otherwise the matcher is a
// tool name pattern (case-insensitive, `*` wildcards) and only narrows tool
// events: a non-tool event ignores the matcher.
func matches(rule config.HookRule, ev Event) bool {
	m := strings.TrimSpace(rule.Matcher)
	if m == "" || m == "*" {
		return true
	}
	if !isToolEvent(ev.Name) {
		return true
	}
	return matchPattern(m, ev.ToolName)
}

// isToolEvent reports whether the event's matcher narrows a tool name.
func isToolEvent(name string) bool {
	return name == PreToolUse || name == PostToolUse
}

// matchPattern matches a name against a case-insensitive `*` wildcard
// pattern. Unlike path.Match, `*` also spans separators.
func matchPattern(pattern, name string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	name = strings.ToLower(strings.TrimSpace(name))
	if pattern == "" || pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	var b strings.Builder
	b.WriteString("^")
	for i, part := range strings.Split(pattern, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// run executes one rule and returns its decision.
func (r *Runner) run(ctx context.Context, rule config.HookRule, ev Event, payload []byte) (Decision, error) {
	res := r.exec(ctx, rule, payload)
	dec := decisionFor(res, rule)
	r.logRun(ev, rule, res, dec)
	return dec, res.err
}

// startAsync detaches a rule: the hook runs on its own context and its
// outcome is logged when it finishes. The caller gets no decision.
func (r *Runner) startAsync(rule config.HookRule, ev Event, payload []byte) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeoutOf(rule))
		defer cancel()
		res := r.exec(ctx, rule, payload)
		r.logRun(ev, rule, res, decisionFor(res, rule))
	}()
}

// decisionFor is the decision a run produced. A failed hook answers nothing,
// unless the rule turns the failure into a block.
func decisionFor(res runResult, rule config.HookRule) Decision {
	if res.err == nil {
		return res.decision
	}
	if !blocksOnFailure(rule) {
		return Decision{}
	}
	reason := strings.TrimSpace(res.stderr)
	if reason == "" {
		reason = res.err.Error()
	}
	return Decision{Decision: "deny", Reason: reason}
}

// runResult is the outcome of one hook process.
type runResult struct {
	decision Decision
	exit     int
	stderr   string
	err      error
}

// exec runs the hook command and interprets its output.
func (r *Runner) exec(ctx context.Context, rule config.HookRule, payload []byte) runResult {
	if strings.TrimSpace(rule.Command) == "" {
		return runResult{exit: -1, err: &HookError{Command: rule.Command, Kind: "config"}}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeoutOf(rule))
	defer cancel()

	var cmd *exec.Cmd
	if len(rule.Args) > 0 {
		// Explicit argv: the command runs directly, without a shell.
		cmd = exec.CommandContext(runCtx, rule.Command, rule.Args...)
	} else {
		// No argv: the command is a shell script, so pipes work.
		cmd = exec.CommandContext(runCtx, "sh", "-c", rule.Command)
	}
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}
	cmd.Stdin = bytes.NewReader(payload)
	// A hook that leaves a child holding the pipe open must not wedge Wait.
	cmd.WaitDelay = time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := runResult{stderr: stderr.String()}
	switch {
	case err == nil:
		res.exit = 0
		dec, perr := parseDecision(stdout.Bytes())
		if perr != nil {
			res.err = &HookError{Command: rule.Command, Kind: "output", Err: perr}
			return res
		}
		res.decision = dec
		return res
	default:
		// A deadline (or a cancelled parent context) is not an exit code: the
		// hook was killed before it could answer.
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res.err = &HookError{Command: rule.Command, Kind: "timeout"}
			return res
		}
		if errors.Is(runCtx.Err(), context.Canceled) {
			res.err = &HookError{Command: rule.Command, Kind: "canceled"}
			return res
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitCode()
			res.exit = code
			if code == 2 {
				// Exit code 2 blocks the event.
				res.decision = Decision{Decision: "deny", Reason: strings.TrimSpace(res.stderr)}
				return res
			}
			res.err = &HookError{Command: rule.Command, Exit: code, Kind: "exit"}
			return res
		}
		res.err = &HookError{Command: rule.Command, Kind: "spawn", Err: err}
		return res
	}
}

// logRun records one hook run. The record never carries the payload or the
// hook's output: only the event, the tool, the rule and the outcome.
func (r *Runner) logRun(ev Event, rule config.HookRule, res runResult, dec Decision) {
	if r.Log == nil {
		return
	}
	m := map[string]any{
		"event":    ev.Name,
		"tool":     ev.ToolName,
		"matcher":  rule.Matcher,
		"command":  rule.Command,
		"exit":     res.exit,
		"decision": dec.Decision,
		"async":    rule.Async,
	}
	if res.err != nil {
		m["error"] = res.err.Error()
	}
	r.Log("hook", m)
}

// HookError describes a hook that failed to produce a decision. Its message
// never carries the hook's output, so it is safe to log.
type HookError struct {
	Command string
	Exit    int
	Kind    string // config | exit | timeout | spawn | output
	Err     error
}

func (e *HookError) Error() string {
	switch e.Kind {
	case "timeout":
		return fmt.Sprintf("hook %q timed out", e.Command)
	case "canceled":
		return fmt.Sprintf("hook %q was cancelled", e.Command)
	case "exit":
		return fmt.Sprintf("hook %q exited %d", e.Command, e.Exit)
	case "output":
		return fmt.Sprintf("hook %q printed an invalid decision", e.Command)
	case "config":
		return fmt.Sprintf("hook rule for %q has no command", e.Command)
	default:
		return fmt.Sprintf("hook %q could not be started", e.Command)
	}
}

func (e *HookError) Unwrap() error { return e.Err }

// timeoutOf resolves the rule's timeout in seconds.
func timeoutOf(rule config.HookRule) time.Duration {
	return time.Duration(EffectiveTimeout(rule)) * time.Second
}

// EffectiveTimeout resolves a rule's timeout in seconds: the rule's own
// value, the default when it sets none, and the cap when it asks for more.
// The CLI prints the same number the runtime enforces.
func EffectiveTimeout(rule config.HookRule) int {
	t := rule.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	if t > MaxTimeout {
		t = MaxTimeout
	}
	return t
}

// blocksOnFailure reports whether a failing hook blocks the event.
func blocksOnFailure(rule config.HookRule) bool {
	return strings.EqualFold(strings.TrimSpace(rule.OnFailure), "block")
}

// marshalEvent renders the event as the JSON object the hook reads on stdin.
func marshalEvent(ev Event) ([]byte, error) {
	m := map[string]any{"event": ev.Name}
	if ev.SessionID != "" {
		m["session_id"] = ev.SessionID
	}
	if ev.CWD != "" {
		m["cwd"] = ev.CWD
	}
	if ev.ToolName != "" {
		m["tool_name"] = ev.ToolName
	}
	if ev.ToolInput != "" {
		m["tool_input"] = toolInputValue(ev.ToolInput)
	}
	if ev.Model != "" {
		m["model"] = ev.Model
	}
	if ev.Persona != "" {
		m["persona"] = ev.Persona
	}
	for k, v := range ev.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// toolInputValue keeps a JSON payload a JSON value and anything else a string.
func toolInputValue(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	return s
}

// parseDecision reads the hook's stdout. Empty stdout means "no decision".
func parseDecision(out []byte) (Decision, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return Decision{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return Decision{}, fmt.Errorf("invalid JSON decision: %w", err)
	}
	get := func(names ...string) any {
		for k, v := range raw {
			nk := normalizeKey(k)
			for _, n := range names {
				if nk == n {
					return v
				}
			}
		}
		return nil
	}
	d := Decision{}
	if s, ok := get("decision").(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "allow", "deny", "ask":
			d.Decision = strings.ToLower(strings.TrimSpace(s))
		}
	}
	if s, ok := get("reason").(string); ok {
		d.Reason = s
	}
	if s, ok := get("additionalcontext").(string); ok {
		d.AdditionalContext = s
	}
	if s, ok := get("systemmessage").(string); ok {
		d.SystemMessage = s
	}
	if b, ok := get("continue").(bool); ok {
		v := b
		d.Continue = &v
	}
	return d, nil
}

// normalizeKey folds a JSON key so additionalContext, additional_context and
// additional-context all match.
func normalizeKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.ReplaceAll(k, "_", "")
	k = strings.ReplaceAll(k, "-", "")
	return k
}

// merge accumulates the decisions of several hooks for one event.
//
// deny beats ask beats allow. The first deny reason is kept. The additional
// context of every hook is concatenated. A false "continue" wins over a true
// one: a hook that says stop, stops.
type merge struct {
	decision      string
	reason        string
	additional    []string
	systemMessage string
	continueVal   *bool
}

func (m *merge) add(d Decision) {
	switch strings.ToLower(strings.TrimSpace(d.Decision)) {
	case "deny":
		if m.decision != "deny" {
			m.decision = "deny"
			m.reason = d.Reason
		}
	case "ask":
		if m.decision == "" || m.decision == "allow" {
			m.decision = "ask"
			m.reason = d.Reason
		} else if m.reason == "" {
			m.reason = d.Reason
		}
	case "allow":
		if m.decision == "" {
			m.decision = "allow"
			m.reason = d.Reason
		}
	}
	if c := strings.TrimSpace(d.AdditionalContext); c != "" {
		m.additional = append(m.additional, c)
	}
	if m.systemMessage == "" {
		m.systemMessage = d.SystemMessage
	}
	if d.Continue != nil {
		switch {
		case m.continueVal == nil:
			v := *d.Continue
			m.continueVal = &v
		case *d.Continue == false:
			v := false
			m.continueVal = &v
		}
	}
}

func (m *merge) result() Decision {
	d := Decision{
		Decision:      m.decision,
		Reason:        m.reason,
		SystemMessage: m.systemMessage,
		Continue:      m.continueVal,
	}
	d.AdditionalContext = strings.Join(m.additional, "\n")
	return d
}
