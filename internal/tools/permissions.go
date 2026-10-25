// Permission matrix evaluator (DESIGN §6.5, M9.4).
//
// This file is a pure unit: no I/O, no mutable package state, no clock, no
// randomness. Evaluate is a total function of (mode, rules, tool, spec) and
// always returns the same decision for the same inputs.
//
// One table decides everything. There are no per-call-site special cases: the
// caller asks "may this tool run with this argument?" and gets back a
// Decision. The rules, in order:
//
//  1. The mode provides a baseline: read-only tools are allowed, mutating
//     tools are asked about (or denied, in plan/dont-ask).
//  2. Rules refine the baseline. Matching rules are grouped by action and
//     the classes are ranked deny > ask > allow, so a matching deny always
//     wins, even in yolo.
//  3. Inside one action class the LAST matching rule wins, so a specific rule
//     can override a broad one.
//
// Plan mode is the one floor: it is read-only and no rule lifts it.
//
// A rule matches when its Tool names the tool (case-insensitively, `*`
// wildcards allowed) and, when the rule carries a Spec, the spec matches the
// argument:
//
//   - shell-ish tools (bash): the spec is a command prefix,
//   - file-ish tools (read/write/edit/...): the spec is a path glob,
//   - every other tool: the spec cannot be checked, so a rule that carries
//     one does not apply. A spec-scoped rule must not leak onto a tool whose
//     argument it cannot describe, and an allow rule that cannot name what
//     it grants never grants anything.
package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"kintsugi/internal/config"
)

// Mode is a session permission mode.
type Mode string

const (
	// ModeDefault asks before every mutating tool call.
	ModeDefault Mode = "default"
	// ModeAcceptEdits allows file edits but still asks for shell commands.
	ModeAcceptEdits Mode = "accept-edits"
	// ModePlan is read-only: edits are refused, reads are allowed.
	ModePlan Mode = "plan"
	// ModeYolo allows everything. Explicit deny rules still win.
	ModeYolo Mode = "yolo"
	// ModeDontAsk never prompts: read-only tools run, everything else is denied.
	ModeDontAsk Mode = "dont-ask"
)

// Decision actions.
const (
	ActionAllow = "allow"
	ActionAsk   = "ask"
	ActionDeny  = "deny"
)

// Decision is the outcome of the permission matrix for one tool call.
type Decision struct {
	// Action is "allow", "ask" or "deny".
	Action string
	// Reason explains the decision in one human-readable line, naming the
	// mode or the rule that decided. The UI shows it verbatim.
	Reason string
	// Rule is the rule that decided, formatted as "tool" or "tool(spec)".
	// It is empty when the mode baseline decided.
	Rule string
}

// ParseMode resolves a mode name. It accepts the canonical names
// (default, accept-edits, plan, yolo, dont-ask), a few historical aliases
// and is case-insensitive. An empty name means "unset" and resolves to the
// default mode; a genuinely unknown name is an error so a config typo is
// reported instead of silently ignored.
func ParseMode(s string) (Mode, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	key = strings.ReplaceAll(key, "_", "-")
	switch key {
	case "":
		return ModeDefault, nil
	case "default":
		return ModeDefault, nil
	case "accept-edits":
		return ModeAcceptEdits, nil
	case "plan":
		return ModePlan, nil
	case "yolo":
		return ModeYolo, nil
	case "dont-ask":
		return ModeDontAsk, nil
	}
	switch key {
	case "auto-accept", "accept-edit", "acceptedits":
		return ModeAcceptEdits, nil
	case "bypass", "bypasspermissions":
		return ModeYolo, nil
	case "manual":
		return ModeDefault, nil
	case "dontask":
		return ModeDontAsk, nil
	}
	return "", fmt.Errorf("unknown permission mode %q (want default|accept-edits|plan|yolo|dont-ask)", s)
}

// ModeTools reports which tools a mode allows outright, split into the
// mutating ones and the read-only ones. A tool that appears in neither list
// is not allowed outright: Evaluate decides whether it asks or denies.
func ModeTools(m Mode) (mutating []string, readOnly []string) {
	readOnly = readOnlyToolNames()
	switch m {
	case ModeAcceptEdits:
		mutating = []string{"apply_patch", "edit", "write"}
	case ModeYolo:
		mutating = mutatingToolNames()
	}
	return mutating, readOnly
}

// Evaluate decides whether a tool call may run.
//
// mode is the session permission mode; an unknown or empty name falls back to
// the default mode. rules are the configured permission rules. tool is the
// tool name ("read", "edit", "bash", "mcp__server__tool", ...). spec is the
// argument the rule specs are matched against: the command line for
// shell-ish tools, the workspace-relative path for file-ish tools, and ""
// for every other tool.
func Evaluate(mode string, rules []config.PermRule, tool, spec string) Decision {
	m, err := ParseMode(mode)
	if err != nil {
		m = ModeDefault
	}
	name := strings.ToLower(strings.TrimSpace(tool))

	var deny, ask, allow *matchedRule
	for i := range rules {
		r := rules[i]
		if !ruleMatches(r, name, spec) {
			continue
		}
		action := strings.ToLower(strings.TrimSpace(r.Action))
		switch action {
		case ActionAllow:
			// An allow rule must name what it grants. A wildcard rule never
			// grants capability, so it is dropped here.
			if !namesATool(r.Tool) {
				continue
			}
			allow = &matchedRule{rule: r, action: action}
		case ActionAsk:
			ask = &matchedRule{rule: r, action: action}
		case ActionDeny:
			deny = &matchedRule{rule: r, action: action}
		default:
			// An unknown action fails closed: it denies.
			deny = &matchedRule{rule: r, action: action}
		}
	}

	switch {
	case deny != nil:
		return Decision{
			Action: ActionDeny,
			Reason: denyReason(deny),
			Rule:   ruleText(deny.rule),
		}
	case ask != nil:
		if m == ModeDontAsk {
			// dont-ask never prompts: a rule that asks is refused.
			return Decision{
				Action: ActionDeny,
				Reason: "dont-ask never prompts: rule " + ruleText(ask.rule) + " asks, so the action is denied",
				Rule:   ruleText(ask.rule),
			}
		}
		return Decision{
			Action: ActionAsk,
			Reason: "rule " + ruleText(ask.rule) + " asks for approval",
			Rule:   ruleText(ask.rule),
		}
	case allow != nil:
		if m == ModePlan && isEditTool(name) {
			// Plan mode is read-only: no rule lifts that floor.
			return Decision{Action: ActionDeny, Reason: "plan mode is read-only"}
		}
		return Decision{
			Action: ActionAllow,
			Reason: "allowed by rule " + ruleText(allow.rule),
			Rule:   ruleText(allow.rule),
		}
	}
	return modeDecision(m, name)
}

// matchedRule is one rule that matched, with its normalized action.
type matchedRule struct {
	rule   config.PermRule
	action string
}

// modeDecision is the baseline of a mode: what happens when no rule matches.
func modeDecision(m Mode, tool string) Decision {
	switch m {
	case ModeAcceptEdits:
		switch {
		case isEditTool(tool):
			return Decision{Action: ActionAllow, Reason: "accept-edits mode allows edits"}
		case tool == "bash":
			return Decision{Action: ActionAsk, Reason: "accept-edits mode still asks before running a command"}
		case isReadOnlyTool(tool):
			return Decision{Action: ActionAllow, Reason: "accept-edits mode allows read-only tools"}
		case isMutatingTool(tool):
			return Decision{Action: ActionAsk, Reason: "accept-edits mode asks before running a mutating tool"}
		default:
			return Decision{Action: ActionAsk, Reason: "accept-edits mode asks before running an unknown tool"}
		}
	case ModePlan:
		switch {
		case isEditTool(tool):
			return Decision{Action: ActionDeny, Reason: "plan mode is read-only"}
		case isReadOnlyTool(tool):
			return Decision{Action: ActionAllow, Reason: "plan mode allows read-only tools"}
		case tool == "bash":
			return Decision{Action: ActionAsk, Reason: "plan mode asks before running a command"}
		case isMutatingTool(tool):
			return Decision{Action: ActionAsk, Reason: "plan mode asks before running a mutating tool"}
		default:
			return Decision{Action: ActionAsk, Reason: "plan mode asks before running an unknown tool"}
		}
	case ModeYolo:
		return Decision{Action: ActionAllow, Reason: "yolo mode allows every tool"}
	case ModeDontAsk:
		if isReadOnlyTool(tool) {
			return Decision{Action: ActionAllow, Reason: "dont-ask mode allows read-only tools"}
		}
		return Decision{Action: ActionDeny, Reason: "dont-ask never prompts; unlisted actions are denied"}
	default: // ModeDefault
		if isReadOnlyTool(tool) {
			return Decision{Action: ActionAllow, Reason: "default mode allows read-only tools"}
		}
		if isMutatingTool(tool) {
			return Decision{Action: ActionAsk, Reason: "default mode asks before running a mutating tool"}
		}
		return Decision{Action: ActionAsk, Reason: "default mode asks before running an unknown tool"}
	}
}

// denyReason explains a denial caused by a rule.
func denyReason(m *matchedRule) string {
	if m.action != ActionDeny {
		return "denied by rule " + ruleText(m.rule) + " (unknown action " + m.action + ", failing closed)"
	}
	return "denied by rule " + ruleText(m.rule)
}

// ruleMatches reports whether a rule applies to the (tool, spec) pair.
func ruleMatches(r config.PermRule, tool, spec string) bool {
	if !matchToolName(r.Tool, tool) {
		return false
	}
	ruleSpec := strings.TrimSpace(r.Spec)
	if ruleSpec == "" {
		return true
	}
	switch specKindOf(tool) {
	case specCommand:
		return matchCommand(ruleSpec, spec)
	case specPath:
		return matchPath(ruleSpec, spec)
	default:
		// The tool has no command or path argument, so the spec cannot be
		// checked. A rule that cannot be verified does not apply: it must
		// never grant capability and it must not leak into other tools.
		return false
	}
}

// ruleText formats a rule for the operator: "tool" or "tool(spec)".
func ruleText(r config.PermRule) string {
	tool := strings.TrimSpace(r.Tool)
	if tool == "" {
		tool = "*"
	}
	spec := strings.TrimSpace(r.Spec)
	if spec == "" {
		return tool
	}
	return tool + "(" + spec + ")"
}

// namesATool reports whether a rule's tool field names a specific tool or a
// family of tools. A bare wildcard ("*", "**", empty) names no tool at all:
// such a rule may deny or ask, but it never grants capability.
func namesATool(pattern string) bool {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return false
	}
	return strings.Trim(p, "*") != ""
}

// matchToolName reports whether a rule's tool field names the tool. The
// comparison is case-insensitive and supports `*` wildcards.
func matchToolName(pattern, tool string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	t := strings.ToLower(strings.TrimSpace(tool))
	if p == "" {
		return true // an unnamed tool matches every tool
	}
	return wildcardMatch(p, t)
}

// wildcardMatch matches s against a pattern with `*` wildcards.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	rest := s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		if mid == "" {
			continue
		}
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// matchCommand matches a command prefix: "git status" matches
// "git status --short" but not "git statusx".
func matchCommand(spec, cmd string) bool {
	s := strings.TrimSpace(spec)
	c := strings.TrimSpace(cmd)
	if s == "" {
		return true
	}
	if strings.HasSuffix(s, "*") {
		return strings.HasPrefix(c, strings.TrimSuffix(s, "*"))
	}
	if c == s {
		return true
	}
	if !strings.HasPrefix(c, s) {
		return false
	}
	rest := c[len(s):]
	return rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n'
}

// matchPath matches a workspace-relative path against a path glob.
func matchPath(spec, path string) bool {
	s := strings.TrimSpace(spec)
	p := filepath.ToSlash(strings.TrimSpace(path))
	if s == "" {
		return true
	}
	if p == "" {
		return false
	}
	return globMatch(s, p)
}

// specKind is the kind of argument a tool takes, which decides how a rule
// spec is checked.
type specKind int

const (
	// specNone: the tool has no checkable argument (todo, task, webfetch, MCP).
	specNone specKind = iota
	// specCommand: the argument is a command line.
	specCommand
	// specPath: the argument is a workspace-relative path.
	specPath
)

// specKindOf classifies a tool by the argument its rules are matched against.
func specKindOf(tool string) specKind {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "bash":
		return specCommand
	case "read", "write", "edit", "list", "apply_patch", "multi_edit", "glob", "grep":
		return specPath
	}
	return specNone
}

// SpecArg returns the argument a rule spec is matched against for one tool
// call: the command line for shell-ish tools, the path (or the pattern, for
// glob) for file-ish tools, and "" for every other tool. argsJSON is the raw
// tool call arguments; a missing or malformed argument yields "".
//
// The caller passes this value as the spec argument of Evaluate, so the
// matching table lives in one place instead of at every call site.
func SpecArg(tool, argsJSON string) string {
	kind := specKindOf(tool)
	if kind == specNone {
		return ""
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	key := "path"
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "bash":
		key = "command"
	case "glob":
		key = "pattern"
	}
	raw, ok := args[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// isReadOnlyTool reports whether a tool only observes the workspace.
func isReadOnlyTool(tool string) bool {
	return containsTool(readOnlyToolNames(), tool)
}

// isMutatingTool reports whether a tool is one of the known state-changing
// tools. A tool the matrix does not know (an MCP tool, for instance) is
// treated as mutating by the modes, but it has no name in the list.
func isMutatingTool(tool string) bool {
	return containsTool(mutatingToolNames(), tool)
}

// isEditTool reports whether a tool changes files in the workspace.
func isEditTool(tool string) bool {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "edit", "write", "apply_patch":
		return true
	}
	return false
}

// readOnlyToolNames lists the tools that only observe the workspace or the
// session. `todo` is in this group on purpose: the plan list is session state,
// not a change to the operator's files, and prompting for it on every turn
// would be noise.
func readOnlyToolNames() []string {
	return []string{"glob", "grep", "kb_search", "list", "memory_search", "read", "skill", "todo"}
}

// mutatingToolNames lists the known tools that change state or reach outside
// the workspace.
func mutatingToolNames() []string {
	return []string{"apply_patch", "bash", "edit", "task", "webfetch", "write"}
}

// containsTool reports whether the (normalized) tool is in the list.
func containsTool(list []string, tool string) bool {
	name := strings.ToLower(strings.TrimSpace(tool))
	for _, t := range list {
		if t == name {
			return true
		}
	}
	return false
}
