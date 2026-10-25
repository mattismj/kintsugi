package tui

import (
	"fmt"
	"strings"

	"kintsugi/internal/credentials"
	"kintsugi/internal/plan"
	"kintsugi/internal/ui"
)

// readyStep is one item on the getting-started ladder: what it is, whether it
// is satisfied, the value to show, and the exact command that fixes it when it
// is not. The command is not decoration — a checklist that says a thing is
// missing without saying how to fix it is a nag, not help.
type readyStep struct {
	label string
	done  bool
	value string
	fix   string
}

// readiness is the workspace's actual setup state, derived from live data so the
// ladder, the status card and the "next" line can never disagree about what is
// missing.
type readiness struct {
	model      string
	hasModel   bool
	context    string
	hasContext bool
	key        string // "env" | "stored" | "missing"
	provider   string
}

// Ready reports the setup state. It is cheap enough to call per render — one
// context read, one credential store read, no subprocess.
func (m *Model) Ready() readiness {
	r := readiness{}
	if m.res != nil {
		r.provider = m.res.ProviderName
		if id := m.res.Model.ID; id != "" {
			r.model = m.res.ProviderName + "/" + id
			r.hasModel = true
		}
	}
	secs := m.app.ContextSections("authorization", "domain")
	switch {
	case strings.TrimSpace(secs["authorization"]) != "" && strings.TrimSpace(secs["domain"]) != "":
		r.hasContext = true
		r.context = "CONTEXT.md ✓ (authorization + domain)"
	case strings.TrimSpace(secs["authorization"]) != "":
		r.hasContext = false
		r.context = "CONTEXT.md · domain missing"
	default:
		r.hasContext = false
		r.context = "CONTEXT.md missing authorization"
	}
	// The key check asks the same question the resolver does: the provider's
	// environment variable, then the credential store. A loopback provider needs
	// no key at all, so it is satisfied by construction.
	if strings.TrimSpace(r.provider) == "" {
		r.key = "missing"
	} else {
		env := ""
		if m.res != nil {
			env = m.res.ProviderCfg.APIKeyEnv
		}
		r.key = credentials.Status(r.provider, env)
		if r.key == "missing" && m.res != nil && m.res.ProviderCfg.Offline {
			r.key = "not needed"
		}
	}
	return r
}

// steps is the ladder in fix order: a model to think with, a scope to think
// inside, and a way to reach it.
func (r readiness) steps() []readyStep {
	return []readyStep{
		{label: "model", done: r.hasModel, value: orDash(r.model), fix: "/model <provider/model>"},
		{label: "context", done: r.hasContext, value: r.context, fix: "kintsugi context init"},
		{label: "provider key", done: r.key == "env" || r.key == "stored" || r.key == "not needed",
			value: r.keyValue(), fix: "kintsugi connect " + orDash(r.provider)},
	}
}

func (r readiness) keyValue() string {
	switch r.key {
	case "env":
		return "set in the environment"
	case "stored":
		return "stored · 0600"
	case "not needed":
		return "not needed (loopback)"
	}
	return "missing"
}

// allDone reports whether there is nothing left to set up.
func (r readiness) allDone() bool {
	for _, s := range r.steps() {
		if !s.done {
			return false
		}
	}
	return true
}

// gettingStarted is the first-run card's body. When everything is configured it
// says so in one line and stops: a checklist with nothing to do is clutter. When
// something is missing it shows the ladder and puts the fix on the same row as
// the gap — a separate row per fix made the card taller than the viewport and
// pushed the nameplate off the screen, and reading "✗ context" without the
// command beside it is a nag rather than help.
func (m *Model) gettingStarted() []string {
	r := m.Ready()
	if r.allDone() {
		return []string{
			m.theme.Paint(ui.OK, "✓ ") + m.theme.Paint(ui.Paper, "all set") +
				m.theme.Paint(ui.PaperSoft, " · type a task, or /help for the command groups"),
		}
	}
	out := []string{m.theme.Paint(ui.PaperSoft, "// getting started")}
	budget := m.quickstartWidth()
	for i, s := range r.steps() {
		glyph, tone := "✗", ui.Warn
		if s.done {
			glyph, tone = "✓", ui.OK
		}
		head := fmt.Sprintf("%s %d  %-12s ", glyph, i+1, s.label)
		value := s.value
		tail := ""
		if !s.done && s.fix != "" {
			// The fix is reserved before the value is clipped. Truncating the
			// whole row would eat the arrow and the command first at 80 columns,
			// which turns the one useful part of the row into an ellipsis.
			tail = " → " + s.fix
		}
		// The fix is reserved before the value is clipped. Truncating the whole row
		// would eat the arrow and the command first at 80 columns, which turns the
		// one useful part of the row into an ellipsis. When even a minimal value
		// would not fit beside the fix, the value is dropped instead: on a narrow
		// terminal "✗ context → kintsugi context init" is what the operator needs, and
		// the human-readable reason is the part that can go.
		room := budget - len(head) - len(tail) - 1 // -1 for truncate's ellipsis
		if room < 8 {
			room = 0
		}
		shown := ""
		if room > 0 {
			shown = truncate(value, room)
		}
		out = append(out, m.theme.Paint(tone, head[:2])+
			m.theme.Paint(ui.PaperSoft, head[2:])+
			m.theme.Paint(ui.Paper, shown)+
			m.theme.Paint(ui.Gold, tail))
	}
	return out
}

// quickstartWidth is the column the ladder is drawn in. The ladder renders
// inside the startup card, which is itself inside the transcript panel, so the
// budget has to pay for both frames: transcript padding plus the card's two
// border cells. Budgeting only for the panel let the last character of a fix
// fall off at 80 columns.
func (m *Model) quickstartWidth() int { return max(m.transcriptWidth()-4, 20) }

// nextAction is the single derived line that answers "what now?". It is the
// difference between a tool that reports state and one that guides: every branch
// names the command that moves the session forward.
func (m *Model) nextAction() string {
	r := m.Ready()
	for _, s := range r.steps() {
		if !s.done && s.fix != "" {
			return s.fix
		}
	}
	// Setup is complete: the next thing worth doing is the work itself.
	if m.app != nil {
		if plans, err := plan.List(m.app.Workspace); err == nil {
			for _, p := range plans {
				rev, err := plan.LoadReview(m.app.Workspace, p.Name)
				if err != nil || rev.Decided() {
					continue
				}
				if rev.Current() == nil {
					return fmt.Sprintf(`review plan %q (/plan)`, p.Name)
				}
				return fmt.Sprintf(`finish reviewing plan %q (/plan)`, p.Name)
			}
		}
	}
	return "type a task"
}

// statusText is the /status card. The first line is always the derived next
// action, so the card leads with guidance rather than a stat dump.
func (m *Model) statusText() string {
	lines := []string{m.theme.Paint(ui.Gold, "→ next: ") + m.nextAction(), ""}
	lines = append(lines, strings.Split(m.statusDetail(), "\n")...)
	// Which AGENTS.md tiers this model is actually getting (I-80). It belongs in
	// the status card because it is a fact about the prompt, and an operator who
	// cannot see it cannot tell why a model behaves the way it does.
	if m.app != nil {
		if tiers := m.app.AgentsTiers(); len(tiers) > 0 {
			var names []string
			for _, t := range tiers {
				label := t.Tier
				if t.Rule != "" {
					label += "(" + t.Rule + ")"
				}
				names = append(names, label)
			}
			lines = append(lines, "", m.theme.Paint(ui.PaperSoft, "agents.md  ")+strings.Join(names, " → "))
		}
	}
	return strings.Join(lines, "\n")
}

// statusDetail is the stat block under the next-action line. It lives in
// update.go beside the rest of the status machinery.
