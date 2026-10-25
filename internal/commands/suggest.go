package commands

// Completion suggestions for the operator's input line (W3 item 3.4).
//
// The generator lives beside the reference grammar on purpose. Suggest and
// ExpandReferences must agree about what a token *is*, or the popup would offer
// text the expander reads differently — the same argument the codebase already
// makes for ResolveMouse. So every candidate here is built from the same rules:
//
//   - the token is the whitespace-delimited word under the caret (takeArg's
//     unquoted form), so a trailing comma stays part of it: "@file main.go," is
//     the path "main.go," and nothing is offered for it;
//   - a reference only exists where scanIndex says one does — at the start or
//     after a space — and only inside a kind keyword it recognised;
//   - a path candidate is gated by exactly the checks fileReference and
//     folderReference apply (PathDenied, then DecideSpec), so the popup can
//     never offer a path ExpandReferences would refuse.
//
// What it deliberately does not do: guess. There is no candidate for an `@url`
// (a host nobody typed is an egress request nobody made), none for an `@diff`
// beyond the refs git always has, and none inside a quoted argument — that
// token's boundary is not the word boundary the popup replaces.

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/tools"
)

// Candidate is one completion suggestion.
type Candidate struct {
	// Value is the text that replaces the token under the caret. For a
	// reference argument it is the argument alone ("internal/tui/view.go"), not
	// the whole token; for a keyword or a slash command it is the whole token
	// ("@file", "/review"), because those *are* the token.
	Value string
	// Label is what the popup lists: the same text the operator is choosing
	// between, without the surrounding reference.
	Label string
	// Hint is the dim second column: a command's argument hint, or what kind of
	// thing the candidate names.
	Hint string
	// Kind is one of the reference kinds (KindFile, KindFolder, KindDiff,
	// KindURL) or KindCommand.
	Kind string
}

// KindCommand is the Candidate.Kind of a slash command.
const KindCommand = "command"

// Bounds. A popup is a peephole, not a file manager: it shows a window over the
// candidates and a walk that runs on every keystroke has to stay cheap.
const (
	// SuggestCap is the most candidates one suggestion set holds.
	SuggestCap = 40
	// suggestWalkCap bounds the entries one workspace walk visits, so a huge
	// tree costs a bounded amount of work per keystroke. A hit beyond it is
	// simply not offered — the operator can always type the path.
	suggestWalkCap = 20000
)

// suggestSkipDirs are directories never worth walking: version-control
// internals and dependency trees. Every path inside a denied directory is
// refused by the policy gate anyway; this only saves the walk.
var suggestSkipDirs = map[string]bool{
	".git": true, "node_modules": true, ".hg": true, ".svn": true,
}

// Suggest returns the completion candidates for the token under cursor, which
// is a byte offset into line (the unit this package works in).
//
// It returns nil when there is nothing honest to offer: an ordinary task, an
// unknown @mention, a token a reference cannot own, or a prefix nothing
// matches. It never panics on an empty line, a cursor past either end, or a
// malformed token.
func Suggest(line string, cursor int, opts Options) []Candidate {
	if line == "" {
		return nil
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(line) {
		cursor = len(line)
	}
	start, _ := TokenSpan(line, cursor)
	// typed is what the operator has written of this token: the part a prefix
	// filter may look at. A caret in mid-word leaves the tail out of it, so the
	// suggestion matches what was typed rather than what was already there.
	typed := line[start:cursor]

	if typed == "" {
		// A fresh token: only the argument of a reference that is already open
		// can be completed.
		if kind, ok := referenceBefore(line, start); ok {
			return argCandidates(kind, "", opts)
		}
		return nil
	}

	switch {
	case strings.HasPrefix(typed, "/"):
		// A slash line is a command only as the first thing on the buffer:
		// the submit path trims the whole draft and looks for a leading "/",
		// so offering a command deeper in a multi-line draft would promise an
		// expansion that cannot happen.
		head := line[:start]
		if strings.ContainsAny(head, "\n\r") || !onlySpace(head) {
			return nil
		}
		return commandCandidates(typed[1:], opts)

	case strings.HasPrefix(typed, "@"):
		return atCandidates(line, start, cursor, typed, opts)
	}

	// Not a token of its own: it may still be the argument of an @kind that
	// precedes it, which is the common case ("@fi le" is not, "@file ma" is).
	if strings.HasPrefix(typed, `"`) || strings.HasPrefix(typed, "'") {
		return nil
	}
	if kind, ok := referenceBefore(line, start); ok {
		return argCandidates(kind, typed, opts)
	}
	return nil
}

// TokenSpan is the span of the whitespace-delimited word under cursor: the byte
// offsets of the text a candidate replaces. Suggest and the popup's accept path
// both use it, so the completion offered is exactly the text overwritten.
//
// It is takeArg's unquoted rule, not a second grammar: a quoted argument is a
// different token shape, and Suggest offers nothing inside one.
func TokenSpan(line string, cursor int) (start, end int) {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(line) {
		cursor = len(line)
	}
	start, end = cursor, cursor
	for start > 0 && !isSpace(line[start-1]) {
		start--
	}
	for end < len(line) && !isSpace(line[end]) {
		end++
	}
	return start, end
}

// atCandidates handles a token that begins with '@'. Three shapes exist: a
// partly typed kind keyword ("@fi"), the colon form ("@file:main"), and an
// agent mention, which is not a reference and gets nothing.
func atCandidates(line string, start, cursor int, typed string, opts Options) []Candidate {
	// The colon form keeps the argument glued to the keyword, so the
	// replacement has to carry the keyword back with it.
	for _, k := range referenceKinds {
		kw := "@" + k + ":"
		if len(typed) < len(kw) || !strings.EqualFold(typed[:len(kw)], kw) {
			continue
		}
		if cursor < start+len(kw) {
			return nil // the caret is still inside the keyword
		}
		return withKeyword(kw, argCandidates(k, typed[len(kw):], opts))
	}

	// A partly typed keyword: only the four kinds can be offered.
	rest := strings.ToLower(typed[1:])
	var out []Candidate
	for _, k := range referenceKinds {
		if !strings.HasPrefix(k, rest) {
			continue
		}
		out = append(out, Candidate{
			Value: "@" + k, Label: "@" + k, Hint: kindHint(k), Kind: k,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

// referenceBefore reports the kind of the @reference whose argument sits at
// start: the token immediately before the word, on the same line.
func referenceBefore(line string, start int) (string, bool) {
	i := start
	for i > 0 && (line[i-1] == ' ' || line[i-1] == '\t') {
		i--
	}
	if i == 0 || i == start {
		// Nothing was skipped: the word is glued to whatever came before it,
		// so it is not an argument position at all.
		return "", false
	}
	j := i
	for j > 0 && !isSpace(line[j-1]) {
		j--
	}
	prev := line[j:i]
	for _, k := range referenceKinds {
		kw := "@" + k
		if strings.EqualFold(prev, kw) || strings.EqualFold(prev, kw+":") {
			return k, true
		}
	}
	return "", false
}

// argCandidates completes one reference kind's argument.
func argCandidates(kind, prefix string, opts Options) []Candidate {
	switch kind {
	case KindFile, KindFolder:
		return pathCandidates(kind, prefix, opts)
	case KindDiff:
		return refCandidates(prefix)
	default:
		// @url: a URL is something the operator names, not something a popup
		// can propose. Offering hosts would be an egress request nobody made.
		return nil
	}
}

// refCandidates offers the refs git always has. Nothing else is honestly
// guessable: a branch name the popup invented might not exist, and an @diff
// whose argument is not refLike ("@diff fix the test") is prose, not a ref.
func refCandidates(prefix string) []Candidate {
	out := make([]Candidate, 0, 3)
	for _, ref := range []string{"HEAD", "HEAD~1", "HEAD~2"} {
		if !strings.HasPrefix(strings.ToLower(ref), strings.ToLower(prefix)) {
			continue
		}
		out = append(out, Candidate{Value: ref, Label: ref, Hint: "git ref", Kind: KindDiff})
	}
	return out
}

// commandCandidates completes a slash command name: the built-ins this package
// knows (the /help inventory) followed by the operator's own commands. A
// built-in wins a name clash, because handleSlash resolves the built-ins first.
func commandCandidates(prefix string, opts Options) []Candidate {
	seen := map[string]bool{}
	var out []Candidate
	add := func(c Command) {
		name := strings.TrimSpace(strings.TrimPrefix(c.Name, "/"))
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if seen[key] || !strings.HasPrefix(key, strings.ToLower(prefix)) {
			return
		}
		seen[key] = true
		hint := c.ArgumentHint
		if hint == "" {
			hint = oneLine(c.Description)
		}
		out = append(out, Candidate{
			Value: "/" + name, Label: "/" + name, Hint: hint, Kind: KindCommand,
		})
	}
	for _, c := range Builtins() {
		add(c)
	}
	for _, c := range opts.Commands {
		add(c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	if len(out) > SuggestCap {
		out = out[:SuggestCap]
	}
	return out
}

// pathCandidates completes an @file or @folder argument: the workspace entries
// whose relative path starts with prefix, case-insensitively, sorted by path.
//
// A directory candidate carries a trailing slash — the same form the colon
// reference accepts — which is what makes it a navigation step rather than a
// finished argument: accepting "internal/" keeps the popup open on its
// children. @folder offers directories only, because @folder on a file is
// refused ("is not a directory"); @file offers both, since a directory there is
// only ever a prefix of the path being typed.
func pathCandidates(kind, prefix string, opts Options) []Candidate {
	root := strings.TrimSpace(opts.Workspace)
	if root == "" {
		return nil
	}
	// A leading "./" is what the operator's shell habit types; it is also what
	// resolvePath normalises away, so the prefix filter works on the same form.
	for strings.HasPrefix(prefix, "./") {
		prefix = strings.TrimPrefix(prefix, "./")
	}
	if prefix == "." || prefix == "/" {
		prefix = ""
	}
	prefix = strings.TrimPrefix(prefix, "/")
	// The prefix is a path prefix, not a segment prefix: "int" matches
	// "internal/…" and "internal/tui/ed" matches "internal/tui/editor.go".
	lower := strings.ToLower(prefix)

	type entry struct {
		rel string
		dir bool
	}
	var hits []entry
	visited := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil // an unreadable entry is simply not offered
		}
		if p == root {
			return nil
		}
		visited++
		if visited > suggestWalkCap {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		isDir := d.IsDir()
		// The walk skips what no reference could ever reach: a version-control
		// or dependency directory, or one the policy denies outright.
		if isDir && !walkableDir(rel, opts) {
			return fs.SkipDir
		}
		if !strings.HasPrefix(strings.ToLower(rel), lower) {
			if isDir && !dirCouldMatch(rel, lower) {
				return fs.SkipDir
			}
			return nil
		}
		if kind == KindFolder && !isDir {
			return nil
		}
		if !walkableArg(kind, rel, isDir, opts) || !tokenSafe(rel) {
			return nil
		}
		hits = append(hits, entry{rel: rel, dir: isDir})
		return nil
	})

	sort.Slice(hits, func(i, j int) bool { return hits[i].rel < hits[j].rel })
	if len(hits) > SuggestCap {
		hits = hits[:SuggestCap]
	}
	out := make([]Candidate, 0, len(hits))
	for _, h := range hits {
		value, hint := h.rel, "file"
		kind := KindFile
		if h.dir {
			value += "/"
			hint, kind = "folder", KindFolder
		}
		out = append(out, Candidate{Value: value, Label: value, Hint: hint, Kind: kind})
	}
	return out
}

// walkableDir reports whether the walk may descend into a directory. Only the
// always-worthless trees are pruned here: the Policy interface cannot tell a
// denied directory from one that merely sits outside an allow list, and the
// allow list may well name children of it ("internal/**" is not a match for
// "internal" itself), so pruning on PathDenied would hide a whole allowed tree.
// The per-entry gate in walkableArg is what keeps refused paths out.
func walkableDir(rel string, opts Options) bool {
	_ = opts
	return !suggestSkipDirs[filepath.Base(rel)]
}

// walkableArg is the policy gate for one candidate, in the same order and with
// the same arguments fileReference and folderReference apply: PathDenied
// first, then DecideSpec with the tool the expansion would use — "read" for a
// file (and for a directory offered as an @file prefix), "list" for a
// directory. A path this refuses is one ExpandReferences would refuse, so the
// popup never offers it.
func walkableArg(kind, rel string, dir bool, opts Options) bool {
	if suggestSkipDirs[filepath.Base(rel)] {
		return false
	}
	if opts.Policy == nil {
		return true
	}
	if opts.Policy.PathDenied(rel) {
		return false
	}
	// @folder lists a directory, so its gate is "list" — the same call
	// folderReference makes. An @file candidate that happens to be a directory
	// is a prefix of the path being typed, and the expansion would read a file
	// under it, so it is gated as a read.
	tool := "read"
	if kind == KindFolder && dir {
		tool = "list"
	}
	return opts.Policy.DecideSpec(tool, rel).Action != tools.ActionDeny
}

// dirCouldMatch reports whether a directory can still contain a path with the
// given prefix. It is a prune, not a filter: a false negative would hide a
// candidate, so it only says no when the prefix and the directory share no
// beginning at all.
func dirCouldMatch(rel, lower string) bool {
	if lower == "" {
		return true
	}
	r := strings.ToLower(rel) + "/"
	return strings.HasPrefix(r, lower) || strings.HasPrefix(lower, r)
}

// tokenSafe reports whether a path survives takeArg unquoted: a path with a
// space or a quote is a token the popup cannot replace honestly, because the
// word boundary it overwrites is not the argument's boundary.
func tokenSafe(rel string) bool {
	return !strings.ContainsAny(rel, " \t\n\r\"'")
}

// onlySpace reports whether s is empty or whitespace only.
func onlySpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isSpace(s[i]) {
			return false
		}
	}
	return true
}

// withKeyword prefixes every candidate's value with a reference keyword, for
// the colon form where the argument is glued to it.
func withKeyword(kw string, list []Candidate) []Candidate {
	for i := range list {
		list[i].Value = kw + list[i].Value
	}
	return list
}

// kindHint is the one-line description of a reference kind in the popup.
func kindHint(kind string) string {
	switch kind {
	case KindFile:
		return "a file's contents"
	case KindFolder:
		return "a directory listing"
	case KindDiff:
		return "the working-tree diff"
	case KindURL:
		return "one URL, reduced"
	}
	return ""
}

// Builtins is the inventory of slash commands the TUI's handleSlash answers, in
// name order. It exists so the completion popup can offer the built-ins without
// the generator reaching into the TUI; the /help card is the same list in prose,
// and a popup test checks the two agree.
func Builtins() []Command {
	list := make([]Command, 0, len(builtinCommands))
	list = append(list, builtinCommands...)
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// builtinCommands is the built-in command inventory, kept in name order.
var builtinCommands = []Command{
	{Name: "accept", ArgumentHint: "<text>", Description: "record an acceptance in the workspace ledger"},
	{Name: "agents", Description: "show the loaded AGENTS.md, persona rule and markdown agents"},
	{Name: "clear", Description: "clear the transcript"},
	{Name: "commands", Description: "list the custom commands (.kintsugi/commands)"},
	{Name: "connect", ArgumentHint: "[provider]", Description: "provider sign-in panel"},
	{Name: "context", ArgumentHint: "edit|diff|refresh", Description: "open the layer-5 workbench"},
	{Name: "frame", Description: "inject Authorization & Domain once"},
	{Name: "guard", ArgumentHint: "[on|off]", Description: "continuity guard"},
	{Name: "help", Description: "list the commands"},
	{Name: "kb", ArgumentHint: "[query]", Description: "list knowledge bases and search them"},
	{Name: "ledger", ArgumentHint: "[kind]", Description: "recent ledger entries"},
	{Name: "memory", Description: "recent workspace memory entries"},
	{Name: "mode", ArgumentHint: "[name]", Description: "show or switch the permission mode"},
	{Name: "model", ArgumentHint: "<ref>", Description: "switch model"},
	{Name: "panel", ArgumentHint: "<pillar>", Description: "expand one pillar"},
	{Name: "persona", ArgumentHint: "edit|base|use <n>", Description: "open the layer-2 pack workbench"},
	{Name: "plan", ArgumentHint: "[name]", Description: "list saved plans, or print one"},
	{Name: "prompt", Description: "prompt-stack inspector"},
	{Name: "quit", Description: "leave the session"},
	{Name: "recall", ArgumentHint: "[query]", Description: "recall prior accepted work"},
	{Name: "remember", ArgumentHint: "<text>", Description: "append a lesson to learned.md"},
	{Name: "rewind", ArgumentHint: "[id]", Description: "list checkpoints, or restore one"},
	{Name: "role", ArgumentHint: "<name>", Description: "switch to a configured model role"},
	{Name: "skills", Description: "list workspace skills"},
	{Name: "status", Description: "session, budget and counters"},
}
