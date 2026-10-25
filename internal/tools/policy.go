package tools

import (
	"path/filepath"
	"regexp"
	"strings"

	"kintsugi/internal/config"
)

// Policy is the per-profile declarative permission set (DESIGN §6.5).
// Path patterns are workspace-relative globs; `**/` is supported as a prefix.
// Command allow entries are prefixes; deny entries are regular expressions.
type Policy struct {
	AllowPaths    []string `toml:"allow_paths"`
	DenyPaths     []string `toml:"deny_paths"`
	AllowCommands []string `toml:"allow_commands"`
	DenyCommands  []string `toml:"deny_commands"`
	Yolo          bool     `toml:"yolo"`
	// Mode is the permission mode baseline: default | accept-edits | plan |
	// yolo | dont-ask. Rules refine it (M9.4).
	Mode string `toml:"mode"`
	// Rules are the operator's permission matrix, evaluated over the mode.
	Rules []config.PermRule `toml:"rules"`
}

// Decide resolves one tool call against the mode baseline and the rule
// matrix, deriving the spec argument from the raw tool-call arguments.
func (p Policy) Decide(tool, argsJSON string) Decision {
	return p.DecideSpec(tool, SpecArg(tool, argsJSON))
}

// DecideSpec resolves one tool call when the spec argument is already known
// (the tool implementations have the resolved command, path or URL in hand).
func (p Policy) DecideSpec(tool, spec string) Decision {
	mode := p.Mode
	if mode == "" {
		mode = string(ModeDefault)
	}
	if p.Yolo {
		mode = string(ModeYolo)
	}
	return Evaluate(mode, p.Rules, tool, spec)
}

// DefaultDenyPaths is the built-in floor: even an empty config refuses the
// files most likely to hold credentials.
var DefaultDenyPaths = []string{
	"**/.env", "**/.env.*", "**/id_rsa", "**/id_ed25519", "**/*.pem", "**/.git/**",
}

// PathDenied reports whether a workspace-relative path is blocked.
func (p Policy) PathDenied(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, pat := range p.DenyPaths {
		if globMatch(pat, rel) {
			return true
		}
	}
	if len(p.AllowPaths) == 0 {
		return false
	}
	for _, pat := range p.AllowPaths {
		if globMatch(pat, rel) {
			return false
		}
	}
	return true
}

// CommandDenied reports whether a command is refused outright, or whether it
// is not pre-approved and therefore needs operator approval.
func (p Policy) CommandDenied(cmd string) (denied, needsApproval bool) {
	for _, pat := range p.DenyCommands {
		re, err := regexp.Compile(pat)
		if err != nil {
			return true, false // a broken deny rule fails closed
		}
		if re.MatchString(cmd) {
			return true, false
		}
	}
	trimmed := strings.TrimSpace(cmd)
	for _, prefix := range p.AllowCommands {
		if strings.HasPrefix(trimmed, prefix) {
			return false, false
		}
	}
	// Not pre-approved and not denied: ask the operator.
	return false, true
}

// globMatch supports `**` segments; filepath.Match does not.
func globMatch(pattern, rel string) bool {
	re := globRegexp(pattern)
	if re.MatchString(rel) {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return re.MatchString(filepath.Base(rel))
	}
	return false
}

func globRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		// A malformed pattern matches nothing (fail closed for deny rules).
		return regexp.MustCompile(`^$a`)
	}
	return re
}

// ApprovalRequest is one ask: the tool, its human-readable summary, and —
// for the file-authoring tools — the proposed change, so the operator
// answers with the diff in front of them rather than a one-line summary.
// Changes is nil for every tool that does not author files.
type ApprovalRequest struct {
	Tool    string
	Detail  string
	Changes []FileChange
}

// Approver asks the operator about one tool use. The executor caches a yes for
// the rest of the session, so this is called at most once per tool.
type Approver interface {
	Approve(req ApprovalRequest) bool
}

type ApproverFunc func(req ApprovalRequest) bool

func (f ApproverFunc) Approve(req ApprovalRequest) bool { return f(req) }
