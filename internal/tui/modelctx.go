package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/agentsctx"
	"kintsugi/internal/harness"
	"kintsugi/internal/persona"
	"kintsugi/internal/ui"
)

// modelCtxSnapshot is the model-keyed context surface: the documents in the
// workspace, the one resolved for the current model, and the tiers that actually
// reach the prompt.
type modelCtxSnapshot struct {
	docs     []agentsctx.Doc
	warns    []string
	selected agentsctx.Selected
	// resolved is what the compiled prompt carries right now, tier by tier.
	resolved []agentsctx.Selected
	cursor   int
}

// openModelCtxWorkbench opens the per-model context surface (I-80). It is the
// place an operator sees which document a model gets, and edits it without
// leaving the session — the whole point of the feature being reachable.
func (m *Model) openModelCtxWorkbench() {
	if m.app == nil {
		m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
		return
	}
	snap := m.modelCtxSnapshot()
	wb := &workbenchView{kind: "agents", agents: &snap}
	for _, w := range snap.warns {
		wb.notices = append(wb.notices, "✸ "+w)
	}
	if snap.selected.Absent {
		wb.notices = append(wb.notices, "· no document is keyed to this model · n creates one")
	}
	m.wb = wb
}

// modelCtxSnapshot reads the live state. It also asks the App for the tiers that
// made it into the prompt, so the surface can never claim a document applies
// when the compiled stack does not carry it.
func (m *Model) modelCtxSnapshot() modelCtxSnapshot {
	docs, warns, sel := m.app.ModelDocs()
	snap := modelCtxSnapshot{docs: docs, warns: warns, selected: sel}
	// The tiers the prompt actually carries, resolved the same way Compile does.
	if text, path := m.app.AgentsMD(); text != "" {
		_ = path // the labels are recomposed below from the tiers
	}
	snap.resolved = m.app.AgentsTiers()
	for i := range docs {
		if sel.Doc != nil && docs[i].Name == sel.Doc.Name {
			snap.cursor = i
		}
	}
	return snap
}

// modelCtxLines renders the surface: the tier stack that reaches the prompt on
// top, then the documents with the selected one marked, then the warnings.
func (m *Model) modelCtxLines(width int) []string {
	w := m.wb.agents
	if w == nil {
		return nil
	}
	var out []string
	out = append(out, m.theme.Paint(ui.PaperSoft, "// tiers in this prompt · most specific last"))
	if len(w.resolved) == 0 {
		out = append(out, m.theme.Paint(ui.PaperSoft, "  (none — the prompt carries no AGENTS.md)"))
	}
	for _, t := range w.resolved {
		rule := ""
		if t.Rule != "" {
			rule = m.theme.Paint(ui.PaperSoft, " · "+t.Rule)
		}
		out = append(out, "  "+m.theme.Paint(ui.Paper, t.Tier)+rule)
		out = append(out, m.theme.Paint(ui.PaperSoft, "    "+truncate(t.Path, max(width-4, 12))))
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft, "// model documents"))
	if len(w.docs) == 0 {
		out = append(out, m.theme.Paint(ui.PaperSoft, "  none · n creates one for this model"))
	}
	for i, d := range w.docs {
		mark := " "
		tone := ui.Paper
		if i == w.cursor {
			mark = "▸"
			tone = ui.Gold
		}
		// Say which models a document claims, so an operator can see at a glance
		// why one is or is not applying.
		rules := describeMatch(d.Match)
		line := fmt.Sprintf("%s %-20s ", mark, truncate(d.Name, 20))
		out = append(out, m.theme.Paint(tone, line)+m.theme.Paint(ui.PaperSoft, truncate(rules, max(width-len(line)-2, 10))))
		if d.Note != "" {
			out = append(out, m.theme.Paint(ui.PaperSoft, "    "+truncate(d.Note, max(width-6, 10))))
		}
	}
	if w.selected.Absent && len(w.docs) > 0 {
		out = append(out, "")
		out = append(out, m.theme.Paint(ui.Warn, "✸ none of these is keyed to the current model — press u to force one"))
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft,
		"a model document is layered between the global AGENTS.md and the workspace one"))
	return out
}

// describeMatch renders a document's match rules in one phrase.
func describeMatch(match persona.Match) string {
	var parts []string
	if len(match.Family) > 0 {
		parts = append(parts, "family "+strings.Join(match.Family, ","))
	}
	if len(match.IDGlob) > 0 {
		parts = append(parts, "id "+strings.Join(match.IDGlob, ","))
	}
	if len(match.Capabilities) > 0 {
		parts = append(parts, "cap "+strings.Join(match.Capabilities, ","))
	}
	if len(parts) == 0 {
		return "fallback"
	}
	return strings.Join(parts, " · ")
}

// handleModelCtxKey is the key path for the model-context surface. It reports
// false for anything it does not own, so the shared workbench keys (scroll,
// close) still apply.
func (m *Model) handleModelCtxKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	w := m.wb.agents
	if w == nil {
		return m, nil, false
	}
	switch strings.ToLower(msg.String()) {
	case "ctrl+n", "down":
		if len(w.docs) > 0 {
			w.cursor = clampInt(w.cursor+1, 0, len(w.docs)-1)
		}
		return m, nil, true
	case "ctrl+p", "up":
		if len(w.docs) > 0 {
			w.cursor = clampInt(w.cursor-1, 0, len(w.docs)-1)
		}
		return m, nil, true
	case "e":
		if len(w.docs) == 0 {
			return m, nil, true
		}
		path := w.docs[w.cursor].Path
		m.wb = nil
		return m, m.editFile("agents", path), true
	case "n":
		// Create a document keyed to *this* model — the action that makes the
		// feature usable without hand-writing YAML frontmatter.
		return m, m.createModelDoc(), true
	case "u":
		m.useSelectedModelDoc()
		return m, nil, true
	case "r":
		if m.app != nil {
			m.app.SetModelDocOverride("")
		}
		m.openModelCtxWorkbench()
		m.addNote("· back to rule matching")
		return m, nil, true
	}
	return m, nil, false
}

// createModelDoc writes a starter document keyed to the resolved model and opens
// it in the operator's editor. The starter body is a comment, not invented
// doctrine: the operator writes the instruction, the harness supplies the key.
func (m *Model) createModelDoc() tea.Cmd {
	if m.app == nil {
		return nil
	}
	res := m.app.ResolvedModel()
	if res == nil {
		m.addNote("✗ no model resolved yet · /model first")
		return nil
	}
	name := res.Model.ID
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	path := m.app.ModelDocPath(sanitizeDocName(name))

	body := m.modelDocTemplate(res)
	match := persona.Match{}
	if res.Model.Family != "" {
		match.Family = []string{res.Model.Family}
	} else {
		match.IDGlob = []string{res.Model.ID}
	}
	if err := agentsctx.SaveDoc(path, "model "+res.Model.ID, match, body); err != nil {
		m.addNote("✗ " + err.Error())
		return nil
	}
	m.wb = nil
	m.addNote("· created " + path + " · opening in $EDITOR")
	return m.editFile("agents", path)
}

// modelDocTemplate is the body a new per-model AGENTS.md opens on. It is a
// scaffold, not a blank page: the four sections are the ones that actually
// move a model's refusal posture and reliability — how the model behaves,
// what it tends to refuse and why that does not apply here, the output
// contract, and its tracked quirks. The operator fills it with what they
// know; the structure is the part worth not hand-writing.
func (m *Model) modelDocTemplate(res *harness.Resolved) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Model context: %s\n\n", res.Model.ID)
	b.WriteString("<!-- This document rides between the global AGENTS.md and the workspace\n")
	b.WriteString("     one, and applies only when this model is answering. The match block\n")
	b.WriteString("     keys it to the model; everything below is operator-authored. -->\n\n")

	b.WriteString("## Model behavior\n\n")
	b.WriteString("- How this model actually behaves: verbosity, tool-call discipline,\n")
	b.WriteString("  drift on long contexts, format breaks — what to expect before trusting\n")
	b.WriteString("  it with a task.\n\n")

	b.WriteString("## Refusal posture\n\n")
	b.WriteString("- What this model tends to refuse: phrasings, topics, terms that trip it.\n")
	b.WriteString("- Why that does not apply here: the workspace authorization (CONTEXT.md)\n")
	b.WriteString("  covers this work; name the scope when the model balks.\n")
	b.WriteString("- Known rephrasings that work with this model: a term that trips it and\n")
	b.WriteString("  the honest replacement that does not.\n\n")

	b.WriteString("## Output contract\n\n")
	b.WriteString("- How this model should shape output: code style, answer structure,\n")
	b.WriteString("  register, what a finished deliverable looks like.\n\n")

	b.WriteString("## Quirks\n\n")
	b.WriteString("- Tracked quirks and their workarounds, dated. These become the trigger\n")
	b.WriteString("  map's raw material when a quirk is a refusal pattern.\n")
	return b.String()
}

// useSelectedModelDoc forces the highlighted document for this model.
func (m *Model) useSelectedModelDoc() {
	w := m.wb.agents
	if w == nil || len(w.docs) == 0 || m.app == nil {
		return
	}
	name := w.docs[w.cursor].Name
	m.app.SetModelDocOverride(name)
	m.openModelCtxWorkbench()
	m.addNote("✓ using " + name + " for this model · r returns to rule matching")
}

// sanitizeDocName turns a model id into a file name.
func sanitizeDocName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "model"
	}
	return out
}
