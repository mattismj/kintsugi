// Package commands implements the operator's custom slash commands (M9.8) and
// the context references (@file, @folder, @diff, @url) that expand inline in the
// operator's line.
//
// A custom command is a Markdown file with YAML frontmatter:
//
//	---
//	description: Review the current diff
//	argument-hint: <path> [focus]
//	---
//	Review $1 with a focus on $2.
//
//	$ARGUMENTS is everything the operator typed after the command name,
//	$1..$9 are the numbered arguments. A body may carry bang lines — a bang
//	followed by a backtick-quoted command: they run in the workspace before
//	the prompt is sent and their output is substituted back into the
//	template.
//
// Two tiers are read, most specific first:
//
//	<workspace>/.kintsugi/commands/*.md
//	<user config dir>/commands/*.md
//
// The workspace wins on a name clash: a project command shadows the global
// one of the same name. Loading is deliberately tolerant — a malformed file is
// skipped, never fatal, because one bad command file must not take a session
// down.
//
// The context references are the other half of the feature: the operator
// brings the right context in, explicitly, instead of letting the model guess.
// @file injects a file's contents (bounded by the read tool's cap), @folder
// injects a bounded listing of a directory, @diff injects the working-tree
// diff, and @url injects one URL's site-aware reduction (bounded, and fetched
// through webfetch's own guards: its offline posture, its declared scope and
// its egress permission). A path or a URL that cannot be read stays literal in
// the text: the turn never fails because of a reference.
package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kintsugi/internal/config"
	"kintsugi/internal/redact"
	"kintsugi/internal/tools"
)

// Tiers. A command file is read from the workspace tier or the user tier.
const (
	TierWorkspace = "workspace"
	TierUser      = "user"
)

// Reference kinds.
const (
	KindFile   = "file"
	KindFolder = "folder"
	KindDiff   = "diff"
	KindURL    = "url"
)

// Bounds. Every injected piece of context is capped, so a reference can never
// blow the prompt up.
const (
	// ReadCap is the read tool's cap (internal/tools.readCap): a referenced
	// file is injected up to this many bytes.
	ReadCap = 96 << 10
	// FolderCap is the largest directory listing injected.
	FolderCap = 8 << 10
	// FolderEntries is the largest number of entries in a directory listing.
	FolderEntries = 200
	// DiffCap is the largest diff injected.
	DiffCap = 64 << 10
	// CommandCap is the largest `!` command output injected.
	CommandCap = 32 << 10
	// URLCap is the largest reduction of one `@url` reference. The
	// extraction itself caps at tools.ExtractCap (24 KiB), so this bound is
	// the safety net behind it, not the usual cut.
	URLCap = 32 << 10
	// URLFetchBytes caps one `@url` download. It is the cap webfetch's fetch
	// path applies to a reference.
	URLFetchBytes = 512 << 10
	// DefaultTimeout bounds one `!` command.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout is the ceiling for one `!` command.
	MaxTimeout = 120 * time.Second
)

// Command is one discovered slash command.
type Command struct {
	Name         string
	Description  string
	ArgumentHint string
	Body         string
	Path         string // absolute path of the file it came from
	Rel          string // workspace-relative, slash-separated (when inside)
	Tier         string // TierWorkspace or TierUser
}

// frontmatter is the YAML header of a command file.
type frontmatter struct {
	Description  string `yaml:"description"`
	ArgumentHint string `yaml:"argument-hint"`
}

// WorkspaceDir is the workspace tier: <workspace>/.kintsugi/commands.
func WorkspaceDir(workspace string) string {
	return filepath.Join(workspace, ".kintsugi", "commands")
}

// GlobalDir is the user tier: the commands folder beside the harness config
// (honours XDG_CONFIG_HOME, so it is ~/.config/kintsugi/commands by
// default).
func GlobalDir() string {
	return filepath.Join(filepath.Dir(config.GlobalPath()), "commands")
}

// Dirs returns the command tiers for a workspace, most specific first.
func Dirs(workspace string) []string {
	var dirs []string
	if strings.TrimSpace(workspace) != "" {
		dirs = append(dirs, WorkspaceDir(workspace))
	}
	return append(dirs, GlobalDir())
}

// Load reads the command files from every directory, in order. The first
// directory that defines a name wins, so the workspace beats the user tier.
//
// Loading never fails: a directory that cannot be read yields no commands and
// a malformed file is skipped.
func Load(dirs ...string) []Command {
	seen := map[string]bool{}
	var out []Command
	for _, dir := range dirs {
		d := strings.TrimSpace(dir)
		if d == "" {
			continue
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			continue // a missing or unreadable tier is simply empty
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				continue
			}
			full := filepath.Join(d, e.Name())
			raw, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			c, err := Parse(raw)
			if err != nil {
				continue // a broken file is skipped, never fatal
			}
			if c.Name == "" {
				c.Name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			}
			c.Name = strings.TrimSpace(c.Name)
			if c.Name == "" || c.Body == "" || seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			c.Path = full
			c.Tier = tierOf(d)
			if c.Tier == TierWorkspace {
				// The workspace tier is <workspace>/.kintsugi/commands.
				if rel, err := filepath.Rel(filepath.Dir(filepath.Dir(d)), full); err == nil {
					c.Rel = filepath.ToSlash(rel)
				}
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// tierOf names the tier a directory belongs to.
func tierOf(dir string) string {
	if sameDir(dir, GlobalDir()) {
		return TierUser
	}
	return TierWorkspace
}

func sameDir(a, b string) bool {
	return filepath.Clean(strings.TrimSpace(a)) == filepath.Clean(strings.TrimSpace(b))
}

// Parse splits one command file into its frontmatter and its prompt body. A
// file with no frontmatter is not broken: the name falls back to the file
// name and the whole file is the prompt.
func Parse(raw []byte) (Command, error) {
	text := string(raw)
	var fm frontmatter
	body := text

	if strings.HasPrefix(text, "---") {
		rest := strings.TrimPrefix(text, "---")
		rest = strings.TrimPrefix(rest, "\r")
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return Command{}, fmt.Errorf("unterminated frontmatter")
		}
		rest = rest[nl+1:]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return Command{}, fmt.Errorf("missing closing frontmatter delimiter")
		}
		if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
			return Command{}, fmt.Errorf("frontmatter: %w", err)
		}
		body = rest[end+len("\n---"):]
		body = strings.TrimPrefix(body, "\r")
		body = strings.TrimPrefix(body, "\n")
	}

	return Command{
		Description:  strings.TrimSpace(fm.Description),
		ArgumentHint: strings.TrimSpace(fm.ArgumentHint),
		Body:         strings.TrimSpace(body),
	}, nil
}

// Find resolves a command by exact name, then case-insensitively. The name may
// carry its leading slash.
func Find(list []Command, name string) (Command, bool) {
	want := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "/"))
	if want == "" {
		return Command{}, false
	}
	for _, c := range list {
		if c.Name == want {
			return c, true
		}
	}
	for _, c := range list {
		if strings.EqualFold(c.Name, want) {
			return c, true
		}
	}
	return Command{}, false
}

// Index renders the command list for /commands: one row per command, with its
// argument hint and its description.
func Index(list []Command) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range list {
		name := "/" + c.Name
		if c.ArgumentHint != "" {
			name += " " + c.ArgumentHint
		}
		desc := c.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "%-30s %s\n", name, oneLine(desc))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Policy is the permission gate a `!` line and a path reference must pass.
// tools.Policy implements it.
type Policy interface {
	// CommandDenied reports whether a shell command is refused outright, or
	// is not pre-approved and therefore needs the operator's approval.
	CommandDenied(cmd string) (denied, needsApproval bool)
	// DecideSpec resolves one tool call against the permission matrix.
	DecideSpec(tool, spec string) tools.Decision
	// PathDenied reports whether a workspace-relative path is blocked.
	PathDenied(rel string) bool
}

// Options carries everything an expansion needs beyond the template itself.
type Options struct {
	// Workspace is the root every path and every command resolves against.
	Workspace string
	// Commands is the discovered command list. A slash line that names none
	// of them is not a custom command.
	Commands []Command
	// Policy gates the `!` lines and the path references. nil means no gate.
	Policy Policy
	// Approve is asked when the policy says a `!` line needs approval. nil
	// means such a line is not run: the fail-closed default. It is also the
	// approver an `@url` fetch uses, so an egress permission the reference
	// asks for reaches the operator exactly as a tool call's would.
	Approve func(command string) bool
	// Timeout bounds one `!` command. Zero means DefaultTimeout.
	Timeout time.Duration
	// Fetcher fetches one URL for an `@url` reference. nil means the default:
	// a tools.Executor built from Workspace, Policy and Approve, which carries
	// webfetch's bounds, its offline guard, the declared scope and the egress
	// permission. The seam exists so a test can answer without a network.
	Fetcher PageFetcher
}

// PageFetcher fetches one URL for an `@url` reference: bounded, offline-guarded
// and permission-gated. *tools.Executor implements it.
type PageFetcher interface {
	FetchPage(ctx context.Context, raw string, maxBytes int) (tools.Page, error)
}

// Expansion is the result of expanding one operator line.
type Expansion struct {
	Original string      // what the operator typed
	Prompt   string      // what is sent to the model
	Command  string      // the custom command that expanded the line, if any
	Notes    []string    // operator-visible notes (a refused `!` line, ...)
	Refs     []Reference // the context references that were injected
}

// Summary is the operator-visible note: what was injected, how big it is and
// what could not be read.
func (e Expansion) Summary() string {
	var lines []string
	if n := e.injected(); n > 0 {
		lines = append(lines, fmt.Sprintf("injected %d context reference(s)", n))
	} else if len(e.Refs) > 0 {
		lines = append(lines, "no context injected")
	}
	for _, r := range e.Refs {
		lines = append(lines, "  "+r.Summary())
	}
	lines = append(lines, e.Notes...)
	return strings.Join(lines, "\n")
}

func (e Expansion) injected() int {
	n := 0
	for _, r := range e.Refs {
		if !r.Failed {
			n++
		}
	}
	return n
}

// Expand resolves an operator line into the prompt to send: a custom slash
// command first (when the line names one), then the context references.
func Expand(line string, opts Options) Expansion {
	exp, ok := ExpandCommand(line, opts)
	if !ok {
		exp = Expansion{Original: line}
	}
	refs := ExpandReferences(exp.Prompt, opts)
	exp.Prompt = refs.Prompt
	exp.Refs = refs.Refs
	return exp
}

// ExpandCommand resolves a slash line against the discovered commands. The
// second result is false when the line names no custom command.
func ExpandCommand(line string, opts Options) (Expansion, bool) {
	name, args, ok := splitSlash(line)
	if !ok {
		return Expansion{}, false
	}
	c, found := Find(opts.Commands, name)
	if !found {
		return Expansion{}, false
	}
	prompt, notes := ExpandBody(c, args, opts)
	return Expansion{Original: line, Prompt: prompt, Command: c.Name, Notes: notes}, true
}

// splitSlash splits "/name the rest" into the command name and its arguments.
func splitSlash(line string) (name, args string, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "/") {
		return "", "", false
	}
	s = s[1:]
	if s == "" {
		return "", "", false
	}
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, "", true
	}
	return s[:i], strings.TrimSpace(s[i+1:]), true
}

// ExpandBody renders one command body into a prompt.
//
// The `!` lines run first, so the operator's arguments never reach the shell
// through the template: inside a command line an argument is substituted
// shell-quoted, in the prose it is substituted verbatim.
func ExpandBody(c Command, args string, opts Options) (string, []string) {
	body, notes := runBangLines(c.Body, args, opts)
	return substitute(body, args, identity), notes
}

// identity is the substitution used for prose: the value is inserted as typed.
func identity(s string) string { return s }

// substitute replaces $ARGUMENTS and $1..$9 in a template. quote is applied to
// every substituted value.
func substitute(template, args string, quote func(string) string) string {
	list := strings.Fields(args)
	var b strings.Builder
	for i := 0; i < len(template); {
		c := template[i]
		if c != '$' || i+1 >= len(template) {
			b.WriteByte(c)
			i++
			continue
		}
		switch {
		case template[i+1] == '$':
			b.WriteByte('$')
			i += 2
		case strings.HasPrefix(template[i+1:], "ARGUMENTS"):
			b.WriteString(quote(args))
			i += 1 + len("ARGUMENTS")
		case template[i+1] >= '1' && template[i+1] <= '9':
			n := int(template[i+1] - '0')
			if n <= len(list) {
				b.WriteString(quote(list[n-1]))
			}
			i += 2
		default:
			b.WriteByte('$')
			i++
		}
	}
	return b.String()
}

// shellQuote wraps s in single quotes: the shell sees one literal word, so an
// argument can never break out of a command.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// bangPattern matches a bang line: a bang, a backtick, the command and the
// closing backtick.
var bangPattern = regexp.MustCompile("!`([^`\n]+)`")

// runBangLines runs every bang line in the body and substitutes its output
// back into the template.
func runBangLines(body, args string, opts Options) (string, []string) {
	var notes []string
	out := bangPattern.ReplaceAllStringFunc(body, func(tok string) string {
		m := bangPattern.FindStringSubmatch(tok)
		cmd := strings.TrimSpace(m[1])
		if cmd == "" {
			return ""
		}
		text, err := runShell(cmd, args, opts)
		if err != nil {
			notes = append(notes, fmt.Sprintf("!%s → %s", cmd, err.Error()))
			return "[!" + cmd + " failed: " + err.Error() + "]"
		}
		return text
	})
	return out, notes
}

// runShell runs one `!` line under the same policy the bash tool uses: a
// denied command is refused, a command that needs approval is only run when
// the approver says yes, and the command itself runs in the workspace root.
func runShell(cmd, args string, opts Options) (string, error) {
	if opts.Workspace == "" {
		return "", fmt.Errorf("no workspace bound")
	}
	if opts.Policy == nil {
		// No permission matrix means no gate: a command file must never run
		// a shell command unguarded.
		return "", fmt.Errorf("no permission policy bound")
	}
	denied, needsApproval := opts.Policy.CommandDenied(cmd)
	switch {
	case denied:
		return "", fmt.Errorf("denied by policy")
	case !needsApproval:
		// Pre-approved by an allow_commands prefix.
	default:
		d := opts.Policy.DecideSpec("bash", cmd)
		switch d.Action {
		case tools.ActionAllow:
		case tools.ActionDeny:
			return "", fmt.Errorf("denied by policy: %s", d.Reason)
		default:
			if opts.Approve == nil {
				return "", fmt.Errorf("needs approval: allow it with allow_commands")
			}
			if !opts.Approve(cmd) {
				return "", fmt.Errorf("not approved by the operator")
			}
		}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, "bash", "-lc", substitute(cmd, args, shellQuote))
	c.Dir = opts.Workspace
	out, err := c.CombinedOutput()
	body := redact.Redact(string(out))
	if len(body) > CommandCap {
		body = body[:CommandCap] + "\n…[truncated]"
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code := ee.ExitCode()
			return fmt.Sprintf("%s\n[exit %d]", strings.TrimRight(body, "\n"), code),
				fmt.Errorf("exit %d", code)
		}
		return body, err
	}
	return body, nil
}

// ---------- context references ----------

// Reference is one piece of context injected into a prompt.
type Reference struct {
	Kind      string // KindFile, KindFolder, KindDiff or KindURL
	Arg       string // what the operator wrote
	Path      string // the resolved path (empty for a diff or a URL)
	URL       string // the resolved URL (a URL reference only)
	Site      string // the reducer that produced a URL reference's text
	Citations int    // the link targets a URL reference preserved
	Bytes     int    // the bytes injected
	Total     int    // the size on disk, when known
	Count     int    // the entries of a folder listing
	Truncated bool
	Failed    bool // the reference could not be produced
	Literal   bool // the token stays in the text as the operator typed it
	Note      string
	Text      string // the injected text
}

// Summary is the one-line report of one reference.
func (r Reference) Summary() string {
	switch {
	case r.Failed && r.Literal:
		return fmt.Sprintf("%s %s · %s · left literal", r.Kind, r.label(), r.Note)
	case r.Failed:
		return fmt.Sprintf("%s %s · %s", r.Kind, r.label(), r.Note)
	case r.Kind == KindURL:
		bits := []string{fmt.Sprintf("%d bytes", r.Bytes)}
		if r.Site != "" && r.Site != "generic" {
			bits = append(bits, r.Site)
		}
		if r.Citations > 0 {
			bits = append(bits, fmt.Sprintf("%d citations", r.Citations))
		}
		if r.Truncated {
			bits = append(bits, "truncated")
		}
		return fmt.Sprintf("%s %s · %s", r.Kind, r.label(), strings.Join(bits, " · "))
	case r.Truncated:
		return fmt.Sprintf("%s %s · %d of %d bytes · truncated", r.Kind, r.label(), r.Bytes, r.Total)
	case r.Kind == KindFolder:
		return fmt.Sprintf("%s %s · %d entries · %d bytes", r.Kind, r.label(), r.Count, r.Bytes)
	default:
		return fmt.Sprintf("%s %s · %d bytes", r.Kind, r.label(), r.Bytes)
	}
}

// label is what the operator asked for, not the resolved path.
func (r Reference) label() string {
	if r.Arg != "" {
		return r.Arg
	}
	switch r.Kind {
	case KindDiff:
		return "working tree"
	case KindURL:
		return r.URL
	}
	return r.Path
}

// Render is the text injected in place of the reference token.
func (r Reference) Render() string {
	if r.Failed {
		return fmt.Sprintf("--- %s: %s (unavailable) ---\n%s", r.Kind, r.label(), r.Text)
	}
	switch r.Kind {
	case KindFolder:
		head := fmt.Sprintf("--- folder: %s (%d entries, %d bytes) ---", r.Path, r.Count, r.Bytes)
		return head + "\n" + r.Text
	case KindDiff:
		head := fmt.Sprintf("--- diff: %s (%d bytes) ---", r.label(), r.Bytes)
		return head + "\n" + r.Text
	case KindURL:
		head := fmt.Sprintf("--- url: %s (%d bytes)", r.URL, r.Bytes)
		if r.Site != "" && r.Site != "generic" {
			head += fmt.Sprintf(", %s", r.Site)
		}
		if r.Citations > 0 {
			head += fmt.Sprintf(", %d citations", r.Citations)
		}
		return head + ") ---\n" + r.Text
	default:
		head := fmt.Sprintf("--- file: %s (%d bytes) ---", r.Path, r.Bytes)
		return head + "\n" + r.Text
	}
}

// referenceKinds are the reference tokens, longest first so "@folder" is never
// read as "@file".
var referenceKinds = []string{KindFolder, KindFile, KindDiff, KindURL}

// IsReference reports whether a line starts with a context reference token
// that can be used: the keyword plus an argument.
func IsReference(line string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "@") {
		return false
	}
	kind, arg, _, ok := scanReference(s, 0)
	if !ok {
		return false
	}
	return kind == KindDiff || arg != ""
}

// IsReferenceToken reports whether a line starts with a context reference
// keyword, whether or not the reference turns out to be usable. It is what
// routes a line away from the agent-mention path: a reference keyword is
// never an agent, so "@file" with no path explains itself instead of being
// reported as an unknown agent.
func IsReferenceToken(line string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "@") {
		return false
	}
	for _, k := range referenceKinds {
		if !strings.HasPrefix(strings.ToLower(s[1:]), k) {
			continue
		}
		after := 1 + len(k)
		if after < len(s) && !isSpace(s[after]) && s[after] != ':' {
			continue
		}
		return true
	}
	return false
}

// ExpandReferences expands every context reference in a line. A reference that
// cannot be produced stays literal in the text: the turn never fails because
// of a path the operator mistyped.
func ExpandReferences(line string, opts Options) Expansion {
	exp := Expansion{Original: line, Prompt: line}
	var b strings.Builder
	i := 0
	for i < len(line) {
		idx := scanIndex(line, i)
		if idx < 0 {
			b.WriteString(line[i:])
			break
		}
		kind, arg, end, ok := scanReference(line, idx)
		if !ok {
			// Not a reference after all: the text stays as it was typed.
			b.WriteString(line[i : idx+1])
			i = idx + 1
			continue
		}
		b.WriteString(line[i:idx])
		ref := buildReference(kind, arg, opts)
		if ref.Failed && ref.Literal {
			// Unreadable: the token stays exactly as the operator typed it.
			b.WriteString(line[idx:end])
		} else {
			b.WriteString(ref.Render())
		}
		exp.Refs = append(exp.Refs, ref)
		i = end
	}
	exp.Prompt = b.String()
	return exp
}

// scanIndex finds the next reference token at or after from. A token must
// start the line or follow a space, so an address like "a@file.com" is not a
// reference.
func scanIndex(line string, from int) int {
	for i := from; i < len(line); i++ {
		if line[i] != '@' {
			continue
		}
		if i > 0 && !isSpace(line[i-1]) {
			continue
		}
		for _, k := range referenceKinds {
			if !strings.HasPrefix(strings.ToLower(line[i+1:]), k) {
				continue
			}
			after := i + 1 + len(k)
			if after < len(line) && !isSpace(line[after]) && line[after] != ':' {
				continue
			}
			return i
		}
	}
	return -1
}

// scanReference parses the reference token at idx: its kind and its argument.
// The end offset is the first character after the token.
func scanReference(line string, idx int) (kind, arg string, end int, ok bool) {
	if idx < 0 || idx >= len(line) || line[idx] != '@' {
		return "", "", idx, false
	}
	for _, k := range referenceKinds {
		if !strings.HasPrefix(strings.ToLower(line[idx+1:]), k) {
			continue
		}
		kwEnd := idx + 1 + len(k)
		explicit := false
		at := kwEnd
		if at < len(line) && line[at] == ':' {
			explicit = true
			at++
		}
		raw, argEnd := takeArg(line, at)
		end = kwEnd
		switch {
		case explicit:
			// The colon form always takes the argument.
			end = argEnd
		case k == KindDiff:
			// A diff may name a ref, but only one that cannot be prose:
			// "@diff fix the test" is a task, "@diff HEAD~1" is a ref.
			if refLike(raw) {
				end = argEnd
			}
		case raw != "":
			end = argEnd
		}
		if k != KindDiff && raw == "" {
			// A file or folder without a path is unusable: the token stays
			// literal and the note says why.
			return k, raw, kwEnd, true
		}
		return k, raw, end, true
	}
	return "", "", idx, false
}

// takeArg reads one argument: a quoted string, or the run of non-space
// characters that follows the whitespace.
func takeArg(line string, at int) (arg string, end int) {
	for at < len(line) && isSpace(line[at]) {
		at++
	}
	if at >= len(line) {
		return "", at
	}
	if q := line[at]; q == '"' || q == '\'' {
		if close := strings.IndexByte(line[at+1:], q); close >= 0 {
			return line[at+1 : at+1+close], at + close + 2
		}
		return line[at+1:], len(line)
	}
	end = at
	for end < len(line) && !isSpace(line[end]) {
		end++
	}
	return line[at:end], end
}

// refLike reports whether a token looks like a git ref rather than the next
// word of a sentence: HEAD, HEAD~1, origin/main or a commit sha. A bare word
// like "fix" is prose.
func refLike(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '-' || r == '_' || r == '~' || r == '^' || r == '@':
		default:
			return false
		}
	}
	if strings.ContainsAny(s, "/~^") || strings.HasPrefix(s, "HEAD") {
		return true
	}
	return isHex(s) && len(s) >= 7
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// buildReference produces one reference.
func buildReference(kind, arg string, opts Options) Reference {
	switch kind {
	case KindFolder:
		return folderReference(arg, opts)
	case KindDiff:
		return diffReference(arg, opts)
	case KindURL:
		return urlReference(arg, opts)
	default:
		return fileReference(arg, opts)
	}
}

// resolvePath resolves a reference path against the workspace, with the same
// rules the read tool uses: a relative path is workspace-rooted, and a path
// that leaves the workspace is refused.
func resolvePath(p, workspace string) (abs, rel string, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "", fmt.Errorf("no path given")
	}
	if workspace == "" {
		return "", "", fmt.Errorf("no workspace bound")
	}
	abs = p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspace, abs)
	}
	abs = filepath.Clean(abs)
	ws := filepath.Clean(workspace)
	if abs != ws && !strings.HasPrefix(abs, ws+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path %q escapes the workspace", p)
	}
	rel, _ = filepath.Rel(ws, abs)
	return abs, filepath.ToSlash(rel), nil
}

// fileReference injects a file's contents, bounded by the read tool's cap.
func fileReference(arg string, opts Options) Reference {
	ref := Reference{Kind: KindFile, Arg: strings.TrimSpace(arg)}
	path, rel, err := resolvePath(ref.Arg, opts.Workspace)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	if opts.Policy != nil {
		if d := opts.Policy.DecideSpec("read", rel); d.Action == tools.ActionDeny {
			ref.Failed, ref.Literal = true, true
			ref.Note = "denied by policy: " + d.Reason
			return ref
		}
		if opts.Policy.PathDenied(rel) {
			ref.Failed, ref.Literal = true, true
			ref.Note = "path denied by policy"
			return ref
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	if info.IsDir() {
		ref.Failed, ref.Literal, ref.Note = true, true, "is a directory"
		return ref
	}
	data, err := os.ReadFile(path)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	ref.Path = rel
	ref.Total = len(data)
	text := string(data)
	if len(data) > ReadCap {
		text = fmt.Sprintf("%s\n…[truncated at %d bytes of %d]", data[:ReadCap], ReadCap, len(data))
		ref.Truncated = true
	}
	ref.Bytes = len(text)
	ref.Text = text
	return ref
}

// folderReference injects a bounded listing of a directory: name, kind and
// size. The contents are never injected.
func folderReference(arg string, opts Options) Reference {
	ref := Reference{Kind: KindFolder, Arg: strings.TrimSpace(arg)}
	path, rel, err := resolvePath(ref.Arg, opts.Workspace)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	if opts.Policy != nil {
		if d := opts.Policy.DecideSpec("list", rel); d.Action == tools.ActionDeny {
			ref.Failed, ref.Literal = true, true
			ref.Note = "denied by policy: " + d.Reason
			return ref
		}
		if opts.Policy.PathDenied(rel) {
			ref.Failed, ref.Literal = true, true
			ref.Note = "path denied by policy"
			return ref
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	if !info.IsDir() {
		ref.Failed, ref.Literal, ref.Note = true, true, "is not a directory"
		return ref
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	ref.Path = rel
	var b strings.Builder
	for i, e := range ents {
		if i >= FolderEntries {
			ref.Truncated = true
			b.WriteString(fmt.Sprintf("… %d more entries\n", len(ents)-i))
			break
		}
		kind, size := "file", "-"
		switch {
		case e.IsDir():
			kind = "dir"
		case e.Type()&os.ModeSymlink != 0:
			kind = "link"
		default:
			if info, err := e.Info(); err == nil {
				size = strconv.FormatInt(info.Size(), 10)
			}
		}
		fmt.Fprintf(&b, "%-40s %-5s %10s\n", clip(e.Name(), 40), kind, size)
		ref.Count++
	}
	text := b.String()
	if len(text) > FolderCap {
		text = text[:FolderCap] + "…[truncated]\n"
		ref.Truncated = true
	}
	ref.Bytes = len(text)
	ref.Text = text
	return ref
}

// diffReference injects the working-tree diff. It degrades plainly: a missing
// git or a directory that is not a repository says so, never fails.
func diffReference(arg string, opts Options) Reference {
	ref := Reference{Kind: KindDiff, Arg: strings.TrimSpace(arg)}
	if strings.HasPrefix(ref.Arg, "-") {
		// A ref that looks like an option must not steer git.
		ref.Failed, ref.Note = true, "a diff ref may not start with '-'"
		ref.Text = ref.Note
		return ref
	}
	if opts.Workspace == "" {
		ref.Failed, ref.Note = true, "no workspace bound"
		ref.Text = ref.Note
		return ref
	}
	if _, err := exec.LookPath("git"); err != nil {
		ref.Failed, ref.Note = true, "git is not available on this machine"
		ref.Text = ref.Note
		return ref
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"diff"}
	if ref.Arg != "" {
		args = append(args, ref.Arg)
	}
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = opts.Workspace
	out, err := c.CombinedOutput()
	text := string(out)
	if err != nil {
		low := strings.ToLower(text)
		switch {
		case strings.Contains(low, "not a git repository"):
			ref.Failed, ref.Note = true, "not a git repository"
		default:
			ref.Failed, ref.Note = true, "git diff failed: "+clipLine(text, 160)
		}
		ref.Text = ref.Note
		return ref
	}
	if len(text) > DiffCap {
		text = text[:DiffCap] + fmt.Sprintf("\n…[truncated at %d bytes]", DiffCap)
		ref.Truncated = true
	}
	if strings.TrimSpace(text) == "" {
		text = "no changes in the working tree\n"
	}
	ref.Bytes = len(text)
	ref.Text = text
	return ref
}

// urlReference injects one URL's reduction. It goes through the same bounded
// fetch, offline guard, scope check and egress permission as webfetch, because
// it is webfetch's fetch that answers: an operator reference cannot reach a
// host the model could not reach, and cannot reach one without the operator's
// answer.
//
// A URL that cannot be read — refused, out of scope, unreachable, empty —
// stays literal, exactly like an unreadable path. The turn never fails over a
// reference.
func urlReference(arg string, opts Options) Reference {
	ref := Reference{Kind: KindURL, Arg: strings.TrimSpace(arg)}
	target, err := normalizeURLArg(ref.Arg)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, err.Error()
		return ref
	}
	ref.URL = target
	fetcher := opts.Fetcher
	if fetcher == nil {
		fetcher = defaultPageFetcher(opts)
	}
	if fetcher == nil {
		ref.Failed, ref.Literal, ref.Note = true, true, "no fetcher bound"
		return ref
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	page, err := fetcher.FetchPage(ctx, target, URLFetchBytes)
	if err != nil {
		ref.Failed, ref.Literal, ref.Note = true, true, clipLine(err.Error(), 160)
		return ref
	}
	text := strings.TrimSpace(page.Markdown)
	if text == "" {
		ref.Failed, ref.Literal, ref.Note = true, true, "the page reduced to nothing readable"
		return ref
	}
	if len(text) > URLCap {
		text = fmt.Sprintf("%s\n…[truncated at %d bytes]", text[:URLCap], URLCap)
		page.Truncated = true
	}

	ref.URL = page.URL
	ref.Site = page.Site
	ref.Citations = len(page.Citations)
	ref.Truncated = page.Truncated
	ref.Total = page.Bytes
	ref.Bytes = len(text)
	ref.Text = text
	return ref
}

// normalizeURLArg turns what the operator typed into the URL to fetch. The
// token may be angle-bracketed — `@url <https://…>` — and may carry the
// punctuation of the sentence it sits in. The scheme must be named: a
// reference never guesses a host.
func normalizeURLArg(arg string) (string, error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return "", fmt.Errorf("no URL given")
	}
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	s = trimURLPunctuation(s)
	if s == "" {
		return "", fmt.Errorf("no URL given")
	}
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		return "", fmt.Errorf("%q is not an http(s) URL", s)
	}
	return s, nil
}

// trimURLPunctuation drops the sentence punctuation that trails a bare URL. A
// closing bracket is only dropped when it has no opener to match.
func trimURLPunctuation(s string) string {
	for s != "" {
		last := s[len(s)-1]
		switch last {
		case '.', ',', ';', ':', '!', '?', '"', '\'':
			s = s[:len(s)-1]
			continue
		case '>', ']':
			if strings.Count(s, "[") >= strings.Count(s, "]") && last == ']' {
				return s
			}
			s = s[:len(s)-1]
			continue
		case ')':
			if strings.Count(s, "(") >= strings.Count(s, ")") {
				return s
			}
			s = s[:len(s)-1]
			continue
		}
		return s
	}
	return s
}

// defaultPageFetcher builds the fetcher an `@url` reference uses when the
// caller named none: the session's permission matrix, the operator's approver
// and the workspace's configuration — which carries the offline posture, the
// declared scope and the sandbox host allowlist.
//
// A policy this package cannot recognise fails closed: the fetcher then has no
// rule that allows egress and no approver, so the reference stays literal
// rather than reaching the network unguarded.
func defaultPageFetcher(opts Options) PageFetcher {
	policy, ok := opts.Policy.(tools.Policy)
	if !ok && opts.Policy != nil {
		return nil
	}
	var approve tools.Approver
	if opts.Approve != nil {
		ask := opts.Approve
		approve = tools.ApproverFunc(func(req tools.ApprovalRequest) bool { return ask(req.Detail) })
	}
	return tools.URLFetcher(opts.Workspace, policy, approve)
}

// ---------- helpers ----------

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// clipLine is the first line of s, clipped: git's usage text is long and the
// operator only needs the reason.
func clipLine(s string, n int) string {
	line := s
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "no output"
	}
	return clip(line, n)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
