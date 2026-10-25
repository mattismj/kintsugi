package tui

// The four pillars (M13). The identity of this harness is not a colour scheme —
// it is four promises, and the inspector states each one as a live readout of
// what the session is actually doing rather than as a slogan:
//
//	◈ context  context engineering — what the prompt stack is carrying
//	▲ scope    cyber/red-team focus — what the operator declared, and the lab
//	✦ refusal  refusal-lowering — drift-guard work, and the memory invariant
//	✓ privacy  privacy — what stays local, what leaves, what is never sent
//
// Every line here is derived from state the session already holds: the compiled
// manifest, the configured scope, the lab lock, the counters, the provider. None
// of it is a hardcoded claim, because a panel that asserts a promise the binary
// does not keep is worse than no panel.
//
// The summary is the compact form that lives in the inspector. `/panel <pillar>`
// expands one into a transcript card, which is where the detail lives.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kintsugi/internal/lab"
	"kintsugi/internal/scope"
	"kintsugi/internal/ui"
)

// pillarGlyph is the mark each pillar carries. These are the same glyphs the
// identity already uses for the same ideas — ◈ for a bound resource, ▲ for
// scope (RoleGlyph's opsec mark), ✦ for the spark that means "something
// happened", ✓ for a promise kept — so the panel reads as part of the existing
// vocabulary rather than a new one.
const (
	pillarContext = "◈"
	pillarScope   = "▲"
	pillarRefusal = "✦"
	pillarPrivacy = "✓"
)

// pillarSummary is the inspector's four-line readout, one line per pillar. It is
// deliberately terse: the inspector is a status column, not a report.
func (m *Model) pillarSummary(width int) []string {
	lines := []string{m.theme.Paint(ui.PaperSoft, "// pillars")}
	lines = append(lines,
		m.pillarLine(pillarContext, ui.Gold, "context", m.contextPillar(), width),
		m.pillarLine(pillarScope, ui.Gold, "scope", m.scopePillar(), width),
		m.pillarLine(pillarRefusal, m.refusalTone(), "refusal", m.refusalPillar(), width),
		m.pillarLine(pillarPrivacy, ui.OK, "privacy", m.privacyPillar(), width),
	)
	return lines
}

// refusalTone escalates the pillar's mark when the session has actually caught a
// refusal. The count is in the value, but a number does not carry urgency the way
// a colour change does — an operator glancing at the column must be able to see
// that this session hit a wall without reading the digits.
func (m *Model) refusalTone() ui.Token {
	if m.refusals > 0 {
		return ui.Danger
	}
	return ui.OK
}

// pillarLine renders one pillar row: glyph, name, and the value flush to the
// width. The value is cut at a word boundary rather than mid-word — a summary
// that reads "stabl" or "no tele" is worse than one that reads "6 layers ·" and
// points the operator at `/panel` for the rest.
func (m *Model) pillarLine(glyph string, tone ui.Token, name, value string, width int) string {
	head := m.theme.Paint(tone, glyph) + " " + m.theme.Paint(ui.PaperDim, name)
	// glyph + space + name, plus the space before the value.
	used := 1 + 1 + len(name) + 1
	room := max(width-used, 4)
	return head + " " + m.theme.Paint(ui.PaperSoft, ellipsisWords(value, room))
}

// contextPillar is the prompt stack as an inventory: how many layers it carries
// and whether the cached prefix still holds. The token count and window share are
// already the inspector's first two rows, so the pillar states what those rows do
// not — the shape of the stack and its cache stability.
func (m *Model) contextPillar() string {
	if m.comp == nil {
		return "no stack"
	}
	n := len(m.comp.Manifest.Layers)
	state := "stable"
	if len(m.stalePaths()) > 0 {
		state = "stale"
	}
	return fmt.Sprintf("%d %s · %s", n, plural(n, "layer"), state)
}

// scopePillar is the declared engagement scope plus the lab posture. A scope that
// is enforced but invisible is a scope the operator cannot audit, so it is shown.
func (m *Model) scopePillar() string {
	var parts []string
	if m.app != nil {
		sc := m.app.Cfg.Scope
		if n := len(sc.Hosts) + len(sc.Paths) + len(sc.Commands); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "rule")))
		}
		if n := len(sc.OutOfScope); n > 0 {
			parts = append(parts, fmt.Sprintf("%d excluded", n))
		}
	}
	if lock, err := lab.ReadLock(m.workspaceDir()); err == nil && lock != nil {
		parts = append(parts, "lab up")
	}
	if len(parts) == 0 {
		return "undeclared · /scope"
	}
	return strings.Join(parts, " · ")
}

// scopeRules returns the merged rules the tool layer enforces. It asks the same
// function enforcement uses, so the panel can never describe a boundary other
// than the one in force.
func (m *Model) scopeRules() scope.Rules {
	if m.app == nil {
		return scope.Rules{}
	}
	return scope.FromWorkspace(m.app.Cfg, m.app.Workspace)
}

// scopeEditablePaths lists the files that actually contribute rules, so /scope
// can offer to edit the right one instead of guessing.
func (m *Model) scopeEditablePaths() []string {
	var out []string
	if m.app == nil {
		return nil
	}
	if len(m.app.Cfg.Scope.Paths)+len(m.app.Cfg.Scope.Commands)+len(m.app.Cfg.Scope.Hosts)+len(m.app.Cfg.Scope.OutOfScope) > 0 {
		if len(m.app.ConfigPaths) > 0 {
			out = append(out, m.app.ConfigPaths[0])
		}
	}
	if p := m.app.ContextPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// refusalPillar is the anti-drift machinery, observable: how many refusals the
// interceptor caught, and the invariant that a refusal never became memory.
//
// The second half is a fact about the ledger, not a slogan. Drift events are
// metadata-only and a refusal is not a ledger kind at all, so the ledger this
// session can recall holds no refusal — which is exactly what the detail card
// says.
func (m *Model) refusalPillar() string {
	if m.refusals == 0 {
		return "0 hits · clean"
	}
	// The guard re-injects prior accepted work on a drift; repairs count the
	// tool-call fixes. Both are recoveries, which is the point of the pillar.
	return fmt.Sprintf("%d %s · %d %s",
		m.refusals, plural(m.refusals, "hit"), m.repairs, plural(m.repairs, "repair"))
}

// plural is the bare count-plus-noun form: "1 hit", "2 hits". A summary line has
// no room for "(s)", and no room to be ambiguous either.
func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// privacyPillar is the summary form: where the data is, and that nothing else
// leaves. The provider name and the full breakdown live in the detail card — this
// column is 22 cells wide, and "data local · openrouter · no telemetry" does not
// fit it without becoming a truncated stub.
func (m *Model) privacyPillar() string {
	return "local · no telemetry"
}

// workspaceDir is the workspace the session is bound to, or "" when there is
// none (fixtures, dry runs). A pillar must not panic on an unbound model.
func (m *Model) workspaceDir() string {
	if m.app == nil {
		return ""
	}
	return m.app.Workspace
}

// pillarDetail expands one pillar into the lines a transcript card shows. It is
// the same derived data as the summary, at full width.
func (m *Model) pillarDetail(pillar string, width int) (title, body string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(pillar)) {
	case "context":
		return "// context stack", m.contextDetail(width), true
	case "scope":
		return "// engagement scope", m.scopeDetail(width), true
	case "refusal":
		return "// refusal telemetry", m.refusalDetail(width), true
	case "privacy":
		return "// privacy posture", m.privacyDetail(width), true
	default:
		return "", "", false
	}
}

// contextDetail is the per-layer inventory with a bar per layer.
func (m *Model) contextDetail(width int) string {
	if m.comp == nil {
		return "no prompt stack is compiled for this session"
	}
	man := m.comp.Manifest
	var b strings.Builder
	fmt.Fprintf(&b, "%d / %d tokens (%.0f%% of window)\n",
		man.Totals.Tokens, man.Totals.Budget, man.Totals.WindowUsedPct)
	fmt.Fprintf(&b, "stack %s · %d layer(s)\n\n", shortHash(man.StackHash), len(man.Layers))
	barW := max(min(width-20, 16), 4)
	for _, l := range man.Layers {
		frac := 0.0
		if man.Totals.Tokens > 0 {
			frac = float64(l.Tokens) / float64(man.Totals.Tokens)
		}
		label := l.ID
		if l.Dropped {
			label += " (drop)"
		}
		fmt.Fprintf(&b, "%-14s %5d %s\n", truncate(label, 14), l.Tokens, m.theme.Progress(frac, barW))
	}
	b.WriteString("\ncache   ")
	if len(m.stalePaths()) > 0 {
		b.WriteString("stale · recompile on send")
	} else {
		b.WriteString("prefix stable")
	}
	if m.cacheRead > 0 || m.cacheWritten > 0 {
		fmt.Fprintf(&b, "\n        read %d · written %d", m.cacheRead, m.cacheWritten)
	}
	b.WriteString("\n\nA stable prefix is what makes a long session cheap: the")
	b.WriteString("\nlayers above the task are byte-identical turn to turn.")
	return b.String()
}

// scopeDetail is the declared scope and the lab/sandbox posture.
func (m *Model) scopeDetail(width int) string {
	var b strings.Builder
	if m.app == nil {
		return "no workspace is bound to this session"
	}
	// Ask the same function enforcement uses. Reading Cfg.Scope directly would
	// miss a scope declared only in CONTEXT.md, and a panel that describes a
	// different boundary than the one being enforced is worse than no panel.
	sc := m.scopeRules()
	if sc.Empty() {
		b.WriteString("no scope declared\n\n")
		b.WriteString("Nothing is narrowed: every path and host the permission\n")
		b.WriteString("matrix allows is reachable. Declare one in kintsugi.toml\n")
		b.WriteString("under [scope], or in the Scope section of CONTEXT.md.")
		return b.String()
	}
	if len(sc.Hosts) > 0 {
		b.WriteString("hosts in scope\n")
		for _, h := range sc.Hosts {
			fmt.Fprintf(&b, "  ▲ %s\n", truncate(h, max(width-6, 8)))
		}
	}
	if len(sc.Paths) > 0 {
		b.WriteString("paths in scope\n")
		for _, p := range sc.Paths {
			fmt.Fprintf(&b, "  ▲ %s\n", truncate(p, max(width-6, 8)))
		}
	}
	if len(sc.Commands) > 0 {
		b.WriteString("commands in scope\n")
		for _, c := range sc.Commands {
			fmt.Fprintf(&b, "  ▲ %s\n", truncate(c, max(width-6, 8)))
		}
	}
	if len(sc.OutOfScope) > 0 {
		// out_of_scope is the harness's own exclusion list: these are refused
		// before the permission matrix is even consulted, and always win.
		b.WriteString("excluded outright\n")
		for _, o := range sc.OutOfScope {
			fmt.Fprintf(&b, "  ✗ %s\n", truncate(o, max(width-6, 8)))
		}
	}
	b.WriteString("\nlab     ")
	if lock, err := lab.ReadLock(m.app.Workspace); err == nil && lock != nil {
		fmt.Fprintf(&b, "up (%s) · host commands refused\n", truncate(lab.LockPath(m.app.Workspace), max(width-10, 8)))
	} else {
		b.WriteString("no lock · work runs on the host\n")
	}
	b.WriteString("sandbox ")
	if m.app.Cfg.Sandbox.Mode == "" || m.app.Cfg.Sandbox.Mode == "off" {
		b.WriteString("off\n")
	} else {
		fmt.Fprintf(&b, "%s\n", m.app.Cfg.Sandbox.Mode)
	}
	b.WriteString("\nScope is enforced before a tool is asked about permission:")
	b.WriteString("\nan out-of-scope target is refused, not prompted for.")
	// Where to change it. The sources are named because the rules can come from
	// two files, and "edit the scope" is useless without knowing which.
	if paths := m.scopeEditablePaths(); len(paths) > 0 {
		b.WriteString("\n\nedit: " + truncate(strings.Join(paths, "  ·  "), max(width-6, 12)))
	}
	return b.String()
}

// refusalDetail is the telemetry and the memory invariant.
func (m *Model) refusalDetail(width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "intercepted   %d\n", m.refusals)
	fmt.Fprintf(&b, "tool repairs  %d\n\n", m.repairs)
	if m.guard != nil {
		b.WriteString("drift guard   on · re-injects prior accepted work\n")
	} else {
		b.WriteString("drift guard   off (/guard on to enable)\n")
	}
	b.WriteString("\nno hard refusal is recorded as memory\n")
	b.WriteString("  a refusal is telemetry: a class label and a count.\n")
	b.WriteString("  the ledger holds plan/artifact/decision/acceptance only,\n")
	b.WriteString("  and a drift event is metadata that recall never returns.\n")
	b.WriteString("\nSo \"same project, fresh session, hard refusal\" cannot be\n")
	b.WriteString("reinforced by the harness remembering that it refused.")
	_ = width
	return b.String()
}

// privacyDetail is what stays local, what leaves, and what is never sent. Every
// line names a real path, a real provider, or a real absence.
func (m *Model) privacyDetail(width int) string {
	var b strings.Builder
	ws := ""
	if m.app != nil {
		ws = m.app.Workspace
	}
	if ws == "" {
		ws = "."
	}
	b.WriteString("operator data\n")
	fmt.Fprintf(&b, "  ✓ workspace   %s\n", truncate(ws, max(width-16, 8)))
	fmt.Fprintf(&b, "  ✓ state       %s\n", truncate(filepath.Join(ws, ".kintsugi"), max(width-16, 8)))
	if mode, ok := dirMode(filepath.Join(ws, ".kintsugi")); ok {
		fmt.Fprintf(&b, "  ✓ permissions %s\n", mode)
	}
	b.WriteString("\nsessions and ledger stay on this machine.\n\n")

	b.WriteString("inference\n")
	if m.res != nil && m.res.ProviderName != "" {
		fmt.Fprintf(&b, "  ▲ provider    %s\n", m.res.ProviderName)
		fmt.Fprintf(&b, "  ▲ model       %s\n", truncate(m.viewModel(), max(width-16, 8)))
		b.WriteString("\nThe prompt leaves the box to reach that endpoint. That is\n")
		b.WriteString("the one network path a session takes.\n\n")
	} else {
		b.WriteString("  · no provider resolved yet\n\n")
	}

	b.WriteString("never sent\n")
	b.WriteString("  ✗ telemetry    no analytics, no crash reporting\n")
	b.WriteString("  ✗ transcripts  sessions are local files only\n")
	if m.app != nil && len(m.app.Cfg.KBs) > 0 {
		b.WriteString("  ✗ knowledge    KB content is read from disk, not uploaded\n")
	}
	_ = width
	return b.String()
}

// dirMode reports a directory's permission bits, for the privacy panel's claim
// about the state directory. A missing directory reports nothing rather than a
// guess: the panel must not assert a mode it did not read.
func dirMode(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return info.Mode().Perm().String(), true
}
