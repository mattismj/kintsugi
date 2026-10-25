package scope

import (
	"fmt"
	"strings"
)

// PromptBlock renders the declared scope as the prompt block the model reads
// (M11.2, DESIGN §7.4.2).
//
// The point of the block is that the boundary is no longer invisible: the tool
// layer already refuses an out-of-scope call before the operator is asked, so
// the model must be able to reason about the same rule set instead of
// discovering it through refusals it cannot explain. The wording therefore
// states the boundary the way the tool layer applies it, not the way a
// configuration file happens to spell it:
//
//   - the three in-scope categories and their matching semantics,
//   - the explicit deny list, which always wins,
//   - the consequence of stepping outside: the call is refused before the
//     operator is asked.
//
// The block also carries the rules verbatim in the grammar Parse reads. That is
// not decoration: it is what lets the prompt be checked against the matcher
// (see the agreement test) instead of trusted, and it gives the model the exact
// strings it must stay inside.
//
// Output is a pure function of the rules: declared order, no timestamps, no
// counters. Two renders of the same rules are byte-identical, which is what
// keeps the prompt cache prefix stable (DESIGN §3.6). An empty rule set renders
// nothing at all: a workspace that declares no scope must not gain a prompt
// layer claiming one.
func (r Rules) PromptBlock() string {
	if r.Empty() {
		return ""
	}
	paths := cleanList(r.Paths)
	commands := cleanList(r.Commands)
	hosts := cleanList(r.Hosts)
	deny := cleanList(r.OutOfScope)
	if len(paths)+len(commands)+len(hosts)+len(deny) == 0 {
		// Only blank entries: nothing was really declared.
		return ""
	}

	var b strings.Builder
	b.WriteString("## Declared scope\n\n")
	b.WriteString("This workspace declares an engagement scope, and the harness enforces it in the tool layer: ")
	b.WriteString(`a path, command or host outside these rules is refused as "out of scope" before the operator is asked, so the call never runs. `)
	b.WriteString("Out-of-scope entries always win over every in-scope rule below.\n\n")

	fmt.Fprintf(&b, "- Paths in scope%s: %s\n",
		" (workspace-relative globs; a named directory covers everything under it)",
		orOutside(paths))

	fmt.Fprintf(&b, "- Commands in scope%s: %s\n",
		" (a prefix at a word boundary or a wildcard; a compound command is split on &&, ||, ; and |, and every segment must be in scope)",
		orOutside(commands))

	fmt.Fprintf(&b, "- Hosts in scope%s: %s\n",
		" (an exact host, any subdomain of a named zone, or a wildcard)",
		orOutside(hosts))

	if len(deny) > 0 {
		fmt.Fprintf(&b, "- Out of scope%s: %s\n",
			" (explicit deny list; always wins, whether or not a rule above allows the call — an entry is a glob, or a plain word that denies any path, command or host containing it)",
			ruleList(deny))
	} else {
		b.WriteString("- Out of scope: no deny entries declared\n")
	}

	b.WriteString("\nThe same rules verbatim, in the form the tool layer applies:\n\n")
	b.WriteString("```toml\n")
	if len(paths) > 0 {
		fmt.Fprintf(&b, "paths = %s\n", tomlList(paths))
	}
	if len(commands) > 0 {
		fmt.Fprintf(&b, "commands = %s\n", tomlList(commands))
	}
	if len(hosts) > 0 {
		fmt.Fprintf(&b, "hosts = %s\n", tomlList(hosts))
	}
	if len(deny) > 0 {
		fmt.Fprintf(&b, "out_of_scope = %s\n", tomlList(deny))
	}
	b.WriteString("```\n")

	return b.String()
}

// orOutside renders one in-scope category. An undeclared category narrows
// nothing — the tool layer's Allows* methods allow whatever it does not name —
// so the prompt has to say so rather than render an empty list that reads like
// a prohibition.
func orOutside(items []string) string {
	if len(items) == 0 {
		return "not narrowed — every such value is allowed except the out-of-scope entries"
	}
	return ruleList(items)
}

// ruleList renders rules in declared order as comma-separated code spans.
func ruleList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, codeSpan(it))
	}
	return strings.Join(parts, ", ")
}

// codeSpan wraps a rule in a code span, unless the rule contains a backtick,
// which would break the span and garble the surrounding line.
func codeSpan(s string) string {
	if strings.ContainsRune(s, '`') {
		return s
	}
	return "`" + s + "`"
}

// tomlList renders rules as a TOML string array, so Parse can read the block
// back into the same rules.
func tomlList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, `"`+tomlEscape(it)+`"`)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tomlEscape escapes the two characters that can end a TOML basic string.
func tomlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
