// Package scope turns the operator's declared engagement scope into
// machine-checked rules (M11.2, DESIGN §6.1).
//
// The scope is declared in the [scope] block of kintsugi.toml and/or in
// the "Scope" section of CONTEXT.md. In the prompt it is context for the
// model; here it is a rule set the tool layer checks before it runs
// anything: a path, a command or a host outside the declared scope is
// refused with "out of scope".
//
// Semantics:
//
//   - An empty rule set means no scope was declared: everything is allowed.
//     Declaring no scope is not a permission decision, it is an absence of
//     one.
//   - A declared rule set denies whatever it does not name. The categories
//     are independent: declaring commands does not narrow paths.
//   - out_of_scope entries are explicit deny lists and always win.
//
// Matching:
//
//   - paths: glob style ("src/**", "*.md"). A plain directory name covers
//     everything under it ("src" covers "src/main.go").
//   - commands: prefix at a word boundary ("go test" covers "go test ./..."),
//     or a wildcard ("git*"). A compound command is checked segment by
//     segment, so "go test && curl evil.example" does not pass a "go test"
//     rule.
//   - hosts: exact, subdomain ("example.com" covers "api.example.com"), or
//     wildcard ("*.example.com").
//
// The parser is deliberately tolerant: the source may be a hand-written
// CONTEXT.md section, so comments, blank lines, prose and unknown keys are
// ignored rather than treated as errors.
package scope

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"kintsugi/internal/config"
	"kintsugi/internal/context"
)

// Rules is one declared scope.
type Rules struct {
	// Paths are the workspace-relative paths in scope (glob style).
	Paths []string `toml:"paths"`
	// Commands are the commands in scope (prefix or wildcard).
	Commands []string `toml:"commands"`
	// Hosts are the hosts in scope (exact, subdomain suffix or wildcard).
	Hosts []string `toml:"hosts"`
	// OutOfScope is the explicit deny list. It always wins.
	OutOfScope []string `toml:"out_of_scope"`
}

// Empty reports whether no scope was declared at all.
func (r Rules) Empty() bool {
	return len(r.Paths) == 0 && len(r.Commands) == 0 &&
		len(r.Hosts) == 0 && len(r.OutOfScope) == 0
}

// AllowsPath reports whether a workspace-relative path is in scope.
//
// rel is relative to the workspace root. When a path rule set exists, a path
// outside it is denied.
func (r Rules) AllowsPath(rel string) bool {
	if r.Empty() {
		return true
	}
	cand := normalizePath(rel)
	for _, d := range r.OutOfScope {
		if matchDeny(d, cand) {
			return false
		}
	}
	if len(r.Paths) == 0 {
		return true
	}
	for _, p := range r.Paths {
		if matchPathRule(p, cand) {
			return true
		}
	}
	return false
}

// AllowsCommand reports whether a shell command is in scope.
//
// The command is split on shell operators and every segment must be in
// scope, so an allowed command cannot smuggle a denied one behind "&&",
// "||", ";" or a pipe.
func (r Rules) AllowsCommand(cmd string) bool {
	if r.Empty() {
		return true
	}
	c := strings.TrimSpace(cmd)
	for _, d := range r.OutOfScope {
		if matchDeny(d, c) {
			return false
		}
	}
	if len(r.Commands) == 0 {
		return true
	}
	segs := commandSegments(c)
	if len(segs) == 0 {
		return true
	}
	for _, seg := range segs {
		if !matchCommandRule(r.Commands, seg) {
			return false
		}
	}
	return true
}

// AllowsHost reports whether a host is in scope.
//
// host is a bare hostname (no scheme, no port). When a host rule set
// exists, a host outside it is denied.
func (r Rules) AllowsHost(host string) bool {
	if r.Empty() {
		return true
	}
	h := normalizeHost(host)
	for _, d := range r.OutOfScope {
		if matchDeny(d, h) {
			return false
		}
	}
	if len(r.Hosts) == 0 {
		return true
	}
	for _, rule := range r.Hosts {
		if matchHostRule(rule, h) {
			return true
		}
	}
	return false
}

// Merge combines two rule sets: the union of every list.
func Merge(a, b Rules) Rules {
	return Rules{
		Paths:      append(append([]string{}, a.Paths...), b.Paths...),
		Commands:   append(append([]string{}, a.Commands...), b.Commands...),
		Hosts:      append(append([]string{}, a.Hosts...), b.Hosts...),
		OutOfScope: append(append([]string{}, a.OutOfScope...), b.OutOfScope...),
	}
}

// FromConfig returns the scope rules declared in the [scope] config block.
func FromConfig(c config.Config) Rules {
	return Rules{
		Paths:      cleanList(c.Scope.Paths),
		Commands:   cleanList(c.Scope.Commands),
		Hosts:      cleanList(c.Scope.Hosts),
		OutOfScope: cleanList(c.Scope.OutOfScope),
	}
}

// FromContext returns the scope rules declared in a CONTEXT.md document's
// "Scope" section.
//
// The heading must classify as the scope class: a "## Scope" heading does,
// while the "## Authorization & scope" prose section does not (that one is
// the operator's frame, not a rule set). A section that holds prose rather
// than machine rules yields no rules: prose is context for the model, not a
// rule set for the tool layer.
func FromContext(doc *context.Document) Rules {
	if doc == nil {
		return Rules{}
	}
	sec, ok := doc.Find(context.ClassScope)
	if !ok {
		return Rules{}
	}
	r, err := Parse(sec.Body)
	if err != nil {
		return Rules{}
	}
	return r
}

// FromContextFile reads a CONTEXT.md and returns its scope rules.
func FromContextFile(path string) (Rules, error) {
	doc, _, err := context.LoadFile(path)
	if err != nil {
		return Rules{}, err
	}
	return FromContext(doc), nil
}

// FromWorkspace returns the effective scope for a workspace: the [scope]
// config block merged with the CONTEXT.md "Scope" section when the context
// layer is enabled and that file exists.
func FromWorkspace(c config.Config, workspace string) Rules {
	r := FromConfig(c)
	if !c.Context.Enabled || strings.TrimSpace(c.Context.Path) == "" {
		return r
	}
	p := strings.TrimSpace(c.Context.Path)
	if !filepath.IsAbs(p) {
		if strings.TrimSpace(workspace) == "" {
			return r
		}
		p = filepath.Join(workspace, p)
	}
	cr, err := FromContextFile(p)
	if err != nil {
		return r
	}
	return Merge(r, cr)
}

// Parse reads scope rules from a source.
//
// It accepts the TOML-ish shape used in kintsugi.toml:
//
//	paths = ["src/**", "docs/**"]
//	commands = ["go test"]
//	hosts = ["example.com"]
//	out_of_scope = ["**/prod/**"]
//
// the line-oriented shape:
//
//	paths: src/**
//	paths: docs/**
//
// and the list shape:
//
//	paths:
//	  - src/**
//
// A "[scope]" table header scopes the keys that follow it; keys in other
// tables are ignored. Comments, blank lines, unknown keys and prose are
// ignored. Only a malformed value (an unterminated list or quote) is an
// error.
func Parse(src string) (Rules, error) {
	vals := map[string][]string{}
	inScope := true
	current := ""
	for i, line := range logicalLines(src) {
		text := stripComment(line)
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			inScope = name == "scope"
			current = ""
			continue
		}
		if !inScope {
			continue
		}
		if trimmed == "-" || strings.HasPrefix(trimmed, "- ") {
			item := unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
			if current != "" && item != "" {
				vals[current] = append(vals[current], item)
			}
			continue
		}
		key, value, ok := splitKeyValue(trimmed)
		if !ok {
			continue
		}
		key = normalizeKey(key)
		if key == "" {
			continue
		}
		current = key
		if value == "" {
			continue
		}
		items, err := parseValue(value)
		if err != nil {
			return Rules{}, fmt.Errorf("scope: line %d: %w", i+1, err)
		}
		vals[key] = append(vals[key], items...)
	}
	return Rules{
		Paths:      vals["paths"],
		Commands:   vals["commands"],
		Hosts:      vals["hosts"],
		OutOfScope: vals["out_of_scope"],
	}, nil
}

// ---------- matching ----------

// matchPathRule reports whether a path rule covers a candidate path.
func matchPathRule(rule, cand string) bool {
	r := strings.TrimSpace(rule)
	if r == "" {
		return false
	}
	if pathGlob(r, cand) {
		return true
	}
	prefix := rulePrefix(r)
	if prefix != "" && (cand == prefix || strings.HasPrefix(cand, prefix+"/")) {
		return true
	}
	return false
}

// rulePrefix returns the directory prefix of a rule, when it has one:
// "src/**" and "src/*" both cover "src", "**/prod/**" covers nothing.
func rulePrefix(rule string) string {
	r := strings.TrimSpace(rule)
	r = strings.TrimSuffix(r, "/**")
	r = strings.TrimSuffix(r, "/*")
	if strings.ContainsAny(r, "*?") {
		return ""
	}
	return strings.Trim(r, "/")
}

// matchCommandRule reports whether any command rule covers a segment.
func matchCommandRule(rules []string, seg string) bool {
	s := normalizeSegment(seg)
	if s == "" {
		return true
	}
	for _, rule := range rules {
		r := strings.TrimSpace(rule)
		if r == "" {
			continue
		}
		if cmdWildcard(r, s) {
			return true
		}
		if s == r {
			return true
		}
		if strings.HasPrefix(s, r+" ") || strings.HasPrefix(s, r+"\t") {
			return true
		}
	}
	return false
}

// matchHostRule reports whether a host rule covers a host.
func matchHostRule(rule, host string) bool {
	r := normalizeHost(rule)
	if r == "" {
		return false
	}
	if strings.ContainsAny(r, "*?") {
		return cmdWildcard(r, host)
	}
	if host == r {
		return true
	}
	// A rule names a zone: "example.com" covers "api.example.com" but not
	// "notexample.com".
	return strings.HasSuffix(host, "."+r)
}

// matchDeny reports whether an out_of_scope entry covers a candidate.
//
// A glob covers it by shape; a plain word covers it as a case-insensitive
// substring, so "production" denies "deploy/production.yml" as well.
func matchDeny(entry, cand string) bool {
	e := strings.TrimSpace(entry)
	if e == "" {
		return false
	}
	if pathGlob(e, cand) {
		return true
	}
	if strings.ContainsAny(e, "*?") {
		return false
	}
	return strings.Contains(strings.ToLower(cand), strings.ToLower(e))
}

// commandSegments splits a command line into the commands it runs.
//
// Shell operators ("&&", "||", ";", "|", newline) end a segment, and the
// contents of a command substitution are segments of their own: an allowed
// command cannot hide a denied one behind "&&" or "$(...)". Operators inside
// quotes do not split.
func commandSegments(cmd string) []string {
	var segs []string
	for _, part := range splitShell(cmd) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		segs = append(segs, part)
		for _, sub := range substitutions(part) {
			if s := strings.TrimSpace(sub); s != "" {
				segs = append(segs, s)
			}
		}
	}
	return segs
}

// splitShell splits on shell operators outside quotes.
func splitShell(s string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case ';', '\n':
			segs = append(segs, cur.String())
			cur.Reset()
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				segs = append(segs, cur.String())
				cur.Reset()
				i++
				continue
			}
			cur.WriteByte(c)
		case '|':
			if i+1 < len(s) && s[i+1] == '|' {
				segs = append(segs, cur.String())
				cur.Reset()
				i++
				continue
			}
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(segs, cur.String())
}

// substitutions returns the bodies of "$(...)" and "`...`" expressions.
func substitutions(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, "$(")
		if i < 0 {
			break
		}
		rest = rest[i+2:]
		j := strings.Index(rest, ")")
		if j < 0 {
			break
		}
		out = append(out, rest[:j])
		rest = rest[j+1:]
	}
	rest = s
	for {
		i := strings.Index(rest, "`")
		if i < 0 {
			break
		}
		rest = rest[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			break
		}
		out = append(out, rest[:j])
		rest = rest[j+1:]
	}
	return out
}

// normalizeSegment drops leading environment assignments ("FOO=bar cmd").
func normalizeSegment(s string) string {
	s = strings.TrimSpace(s)
	for {
		tok, rest, ok := strings.Cut(s, " ")
		if !ok || !isEnvAssign(tok) {
			return strings.TrimSpace(s)
		}
		s = strings.TrimSpace(rest)
	}
}

// isEnvAssign reports whether a token looks like "NAME=value".
func isEnvAssign(tok string) bool {
	i := strings.Index(tok, "=")
	if i <= 0 {
		return false
	}
	name := tok[:i]
	for j := 0; j < len(name); j++ {
		c := name[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && j > 0:
		default:
			return false
		}
	}
	return true
}

// normalizePath normalizes a workspace-relative path for matching.
func normalizePath(p string) string {
	s := filepath.ToSlash(strings.TrimSpace(p))
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "."
	}
	return s
}

// normalizeHost normalizes a host for matching.
func normalizeHost(h string) string {
	s := strings.ToLower(strings.TrimSpace(h))
	return strings.Trim(s, "[]")
}

// cleanList trims and drops empty entries.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// pathGlob matches a path against a glob ("*" does not cross "/", "**" does).
// A pattern without a "/" also matches the base name.
func pathGlob(pattern, s string) bool {
	re := globRegexp(pattern)
	if re.MatchString(s) {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return re.MatchString(baseName(s))
	}
	return false
}

// globRegexp compiles a path glob into a regular expression.
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
		// A malformed pattern matches nothing: a deny rule that cannot be
		// compiled must not grant anything.
		return regexp.MustCompile(`^$a`)
	}
	return re
}

// cmdWildcard matches a string against a wildcard pattern where "*" matches
// anything (including spaces) and "?" matches one character.
func cmdWildcard(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// baseName is the last path segment.
func baseName(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ---------- parsing helpers ----------

// logicalLines joins lines that continue an unterminated list or quote, so
// a multi-line TOML array is one logical line.
func logicalLines(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	var out []string
	var buf strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if buf.Len() > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(line)
		if openDepth(buf.String()) == 0 {
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// openDepth counts unclosed brackets and quotes in a line. Lines that do not
// look like key/value lines are never joined: prose with an apostrophe must
// not swallow the rest of the file.
func openDepth(s string) int {
	if !strings.ContainsAny(s, "=:") {
		return 0
	}
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		}
	}
	if quote != 0 {
		depth++
	}
	return depth
}

// stripComment removes a trailing comment. A "#" or "//" only starts a
// comment at the start of a line or after whitespace, so a rule that
// contains "https://" survives.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '#':
			if i == 0 || isSpace(s[i-1]) {
				return s[:i]
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '/' && (i == 0 || isSpace(s[i-1])) {
				return s[:i]
			}
		}
	}
	return s
}

// isSpace reports whether c is ASCII whitespace.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// splitKeyValue splits "key = value" or "key: value" at the first
// separator outside quotes.
func splitKeyValue(s string) (key, value string, ok bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '=', ':':
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

// parseValue turns a value into the list of rules it holds.
func parseValue(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if strings.HasPrefix(v, "[") {
		if !strings.HasSuffix(v, "]") {
			return nil, fmt.Errorf("unterminated list %q", v)
		}
		inner := strings.TrimSpace(v[1 : len(v)-1])
		if inner == "" {
			return nil, nil
		}
		parts := splitList(inner)
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := unquote(strings.TrimSpace(p)); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	if s := unquote(v); s != "" {
		return []string{s}, nil
	}
	return nil, nil
}

// splitList splits a list body on commas outside quotes.
func splitList(s string) []string {
	var parts []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, cur.String())
}

// unquote removes one layer of matching quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return strings.TrimSpace(s)
}

// normalizeKey maps a key to its canonical name. Unknown keys keep their
// name and are ignored when the rules are built.
func normalizeKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.ReplaceAll(k, "-", "_")
	return k
}
