package tui

import (
	"fmt"
	"strings"

	"kintsugi/internal/continuity"
	"kintsugi/internal/harness"
	"kintsugi/internal/ledger"
	"kintsugi/internal/provider"
	"kintsugi/internal/ui"
)

// Continuity in the panel TUI (M8.5).
//
// Everything in this file is local: no provider call, no network, no transcript.
// A refusal is never memory — the guard only ever re-injects prior *accepted*
// work it recalled from the ledger, and a drift event is written as metadata
// (task hash, matched ids, class, outcome) that is never recallable. The cards
// below show counters, ledger ids and accepted summaries; there is deliberately
// no path here that renders a refusal as memory.

// continuityReady reports whether the app can act on continuity at all: the
// layer must be enabled and a workspace must be bound. The workspace check is
// load-bearing — the ledger path is derived from it, so an unbound app would
// resolve to a relative `.kintsugi/ledger` and write into the process CWD.
func continuityReady(app *harness.App) bool {
	return app != nil && strings.TrimSpace(app.Workspace) != "" && app.Cfg.Continuity.Enabled
}

// continuityBlocks is what the local continuity layer has to say about one task.
type continuityBlocks struct {
	// Messages are the system messages to queue for the turn, in order:
	// the preflight frame first, then the recalled prior work.
	Messages []provider.Message

	Preflight bool   // the preflight frame was injected
	Score     int    // preflight score (trigger terms + class counters)
	Classes   string // telemetry classes that scored (labels only)

	Prior     []string // ledger ids of the recalled prior accepted work
	PriorText string   // the rendered prior-work block
}

// continuityForTask runs the local preflight scorer and the prior-work recall
// for task. Both are best-effort: an error is dropped, never raised, because
// continuity must never fail a turn.
func continuityForTask(app *harness.App, res *harness.Resolved, task string) continuityBlocks {
	out := continuityBlocks{}
	if !continuityReady(app) || strings.TrimSpace(task) == "" {
		return out
	}
	if pre, risk := app.PreflightFor(res, task); strings.TrimSpace(pre) != "" {
		if block := continuity.Block(pre); block != "" {
			out.Messages = append(out.Messages, provider.Message{Role: "system", Content: block})
			out.Preflight, out.Score = true, risk.Score
			out.Classes = classLabels(risk.Classes)
		}
	}
	if prior, ids, err := app.RecallPriorWork(task); err == nil && strings.TrimSpace(prior) != "" {
		if block := continuity.Block(prior); block != "" {
			out.Messages = append(out.Messages, provider.Message{Role: "system", Content: block})
			out.Prior, out.PriorText = ids, prior
		}
	}
	return out
}

// classLabels renders the telemetry classes a preflight scored. A class is a
// label: the phrase that produced the counter was dropped at the telemetry
// boundary.
func classLabels(classes []string) string {
	if len(classes) == 0 {
		return "none"
	}
	return strings.Join(classes, ", ")
}

// installContinuityGuard wires refusal re-injection and drift recording onto the
// loop. GuardRefusal receives the task and the telemetry class only — never the
// model's output — so a refusal cannot be echoed back into a prompt. Drift is
// recorded as metadata through app.RecordDrift and announced to the transcript.
func (m *Model) installContinuityGuard() {
	if m.loop == nil || !continuityReady(m.app) {
		return
	}
	st := m.ledgerStore()
	if st == nil {
		return
	}
	guard := &continuity.Guard{
		Store:  st,
		TopK:   m.app.Cfg.Continuity.PriorWorkTopK,
		Budget: m.app.Cfg.Continuity.PriorWorkBudgetTokens,
		Log:    m.logFunc(),
	}
	m.guard = guard
	m.loop.GuardRefusal = guard.OnRefusal
	m.loop.OnDrift = func(task, class, outcome string, matched []string) {
		// A refusal with no matching accepted work is ordinary telemetry, not
		// drift; do not write metadata for it.
		if len(matched) == 0 {
			return
		}
		// Metadata only, written by the agent goroutine before the notice is
		// sent: nothing here can carry prose into the ledger.
		if err := m.app.RecordDrift(task, class, outcome, matched); err != nil && m.log != nil {
			m.log.Log("meta", map[string]string{"continuity": "drift not recorded: " + err.Error()})
		}
		if m.log != nil {
			m.log.Log("drift", map[string]string{"class": class, "outcome": outcome})
		}
		select {
		case m.ch <- driftNotice{matched: matched, class: class, outcome: outcome}:
		default: // drop under back-pressure; the ledger row is the durable record
		}
	}
}

// uninstallContinuityGuard turns drift re-injection off for this session.
func (m *Model) uninstallContinuityGuard() {
	m.guard = nil
	if m.loop != nil {
		m.loop.GuardRefusal = nil
		m.loop.OnDrift = nil
	}
}

// ledgerStore lazily opens (and then reuses) the workspace ledger for the
// session. A ledger that cannot be opened stays closed: continuity is
// best-effort and never blocks a turn.
func (m *Model) ledgerStore() *ledger.Store {
	if m.ledger != nil {
		return m.ledger
	}
	if m.app == nil {
		return nil
	}
	st, err := m.app.LedgerStore()
	if err != nil {
		if m.log != nil {
			m.log.Log("meta", map[string]string{"continuity": "ledger unavailable: " + err.Error()})
		}
		return nil
	}
	m.ledger = st
	return st
}

func (m *Model) logFunc() func(string, any) {
	if m.log == nil {
		return nil
	}
	return m.log.Log
}

// lastTask is the most recent operator task; /recall uses it as its default
// query so the operator does not have to retype it.
func (m *Model) lastTask() string {
	if len(m.tasks) == 0 {
		return ""
	}
	return m.tasks[len(m.tasks)-1].label
}

// handleRecall implements /recall [query]: the same recall the loop performs,
// on demand. Titles and ids only — the ledger never holds a refusal.
func (m *Model) handleRecall(rest string) {
	query := strings.TrimSpace(rest)
	if query == "" {
		query = m.lastTask()
	}
	if query == "" {
		m.addNote("usage: /recall <query> (no task to default to yet)")
		return
	}
	if m.app == nil || !m.app.Cfg.Continuity.Enabled {
		m.addNote("· continuity is disabled: set [continuity] enabled = true")
		return
	}
	block, ids, err := m.app.RecallPriorWork(query)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	if strings.TrimSpace(block) == "" {
		m.addNote(fmt.Sprintf("· no prior accepted work matches %q", truncate(oneLine(query), 60)))
		return
	}
	body := fmt.Sprintf("query %s\nids %s\n\n%s",
		truncate(oneLine(query), 60), strings.Join(ids, ", "), truncate(block, 1400))
	m.addCard("// recall · prior accepted work", body, ui.Gold)
}

// handleLedger implements /ledger [kind]: the newest ledger entries. Positive
// kinds show their summary; drift rows show matched ids, class and outcome —
// metadata, never prose.
func (m *Model) handleLedger(kind string) {
	rows, err := ledgerRows(m.app, strings.TrimSpace(kind), 8)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	if len(rows) == 0 {
		m.addNote("· no ledger entries yet · /accept records one, `kintsugi ledger add` records plans and decisions")
		return
	}
	lines := make([]string, 0, len(rows)+2)
	for _, e := range rows {
		lines = append(lines, fmt.Sprintf("%-15s %-10s %s", e.ID, e.Kind, truncate(ledgerEntryLine(e), 60)))
	}
	lines = append(lines, "", "positive kinds + drift metadata · drift is never recallable")
	m.addCard("// ledger · recent", strings.Join(lines, "\n"), ui.Gold)
}

// handleAccept implements /accept <summary>: one acceptance entry for the
// current session and task. An acceptance is positive work the operator
// explicitly accepted; nothing else can be recorded from here.
func (m *Model) handleAccept(summary string) {
	summary = strings.TrimSpace(summary)
	// TUI input often arrives quoted (`/accept "..."`); keep the words, drop
	// the surrounding quotes. Apostrophes inside the text are preserved.
	summary = strings.Trim(summary, `"'`)
	summary = strings.TrimSpace(summary)
	if summary == "" {
		m.addNote("usage: /accept <what was accepted>")
		return
	}
	sessionID := ""
	if m.log != nil {
		sessionID = m.log.ID
	}
	entry, err := recordAcceptance(m.app, summary, sessionID, m.viewModel(), m.personaName(), m.lastTask())
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	m.addCard("// continuity · acceptance recorded",
		fmt.Sprintf("%s · %s\noperator-accepted work in the workspace ledger (context, never policy)",
			entry.ID, truncate(oneLine(summary), 96)), ui.OK)
}

// handleGuard implements /guard [on|off]. It flips continuity.guard_on_refusal
// for this process only — the config file is untouched — which is what makes the
// re-injection path testable from inside a live session.
func (m *Model) handleGuard(rest string) {
	switch arg := strings.ToLower(strings.TrimSpace(rest)); arg {
	case "", "status":
		state := "off"
		if m.guard != nil {
			state = "on"
		}
		m.addNote(fmt.Sprintf("· continuity guard %s · continuity %s · preflight %s",
			state, onOff(m.app.Cfg.Continuity.Enabled), onOff(m.app.Cfg.Continuity.Preflight)))
		return
	case "on", "off":
	default:
		m.addNote("usage: /guard [on|off]")
		return
	}
	on := strings.ToLower(strings.TrimSpace(rest)) == "on"

	if !on {
		m.app.Cfg.Continuity.GuardOnRefusal = false
		m.uninstallContinuityGuard()
		m.addNote("· continuity guard off: a drift is no longer re-injected (drift is still counted in the session log)")
		return
	}
	if !m.app.Cfg.Continuity.Enabled {
		m.addNote("✗ continuity is disabled · set [continuity] enabled = true in kintsugi.toml, then /guard on")
		return
	}
	m.app.Cfg.Continuity.GuardOnRefusal = true
	if m.loop == nil {
		m.addNote("✗ no agent loop bound to this transcript · start a turn first, then /guard on")
		return
	}
	m.installContinuityGuard()
	if m.guard == nil {
		m.addNote("✗ workspace ledger unavailable · the guard stays off · check .kintsugi/ is writable, then /guard on")
		return
	}
	m.addNote("✓ continuity guard on · prior accepted work is re-injected on drift")
}

// onOff renders a boolean the way the transcript notes expect.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ledgerRows opens the workspace ledger and returns its entries newest first,
// optionally filtered by kind and capped at limit (0 = every entry).
func ledgerRows(app *harness.App, kind string, limit int) ([]ledger.Entry, error) {
	if app == nil {
		return nil, fmt.Errorf("no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
	}
	st, err := app.LedgerStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()

	all, err := st.List(kind)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Entry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, all[i])
	}
	return out, nil
}

// ledgerEntryLine is the one-line view of an entry. A drift row carries metadata
// only — matched ids, class, outcome — and is rendered as such.
func ledgerEntryLine(e ledger.Entry) string {
	if e.Kind == ledger.KindDrift {
		matched := "none"
		if len(e.MatchedIDs) > 0 {
			matched = strings.Join(e.MatchedIDs, ", ")
		}
		return fmt.Sprintf("drift · matched %s · class %s · outcome %s",
			matched, orDash(e.Class), orDash(e.Outcome))
	}
	s := strings.TrimSpace(e.Summary)
	if s == "" {
		s = strings.TrimSpace(e.Task)
	}
	if s == "" {
		s = "(no summary)"
	}
	s = oneLine(s)
	if e.Status == ledger.StatusSuperseded {
		s += "  [superseded]"
	}
	return s
}

// plainRecall renders `/recall` output for the no-TTY stream.
func plainRecall(app *harness.App, query string) []string {
	if strings.TrimSpace(query) == "" {
		return []string{"usage: /recall <query> (no task to default to yet)"}
	}
	if app == nil || !app.Cfg.Continuity.Enabled {
		return []string{"continuity is disabled: set [continuity] enabled = true"}
	}
	block, ids, err := app.RecallPriorWork(query)
	if err != nil {
		return []string{"recall: " + err.Error()}
	}
	if strings.TrimSpace(block) == "" {
		return []string{fmt.Sprintf("no prior accepted work matches %q", oneLine(query))}
	}
	return []string{block, "", fmt.Sprintf("· %d item(s) · ids %s", len(ids), strings.Join(ids, ", "))}
}

// plainLedger renders `/ledger` output for the no-TTY stream.
func plainLedger(app *harness.App, kind string) []string {
	rows, err := ledgerRows(app, strings.TrimSpace(kind), 8)
	if err != nil {
		return []string{"ledger: " + err.Error()}
	}
	if len(rows) == 0 {
		return []string{"no ledger entries yet · /accept records one, `kintsugi ledger add` records plans and decisions"}
	}
	out := make([]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, fmt.Sprintf("%-15s %-10s %s", e.ID, e.Kind, truncate(ledgerEntryLine(e), 60)))
	}
	return append(out, "· positive kinds + drift metadata · drift is never recallable")
}

// recordAcceptance appends one acceptance entry for the current session.
func recordAcceptance(app *harness.App, summary, sessionID, model, persona, task string) (ledger.Entry, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ledger.Entry{}, fmt.Errorf("an acceptance needs a summary")
	}
	if app == nil {
		return ledger.Entry{}, fmt.Errorf("no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
	}
	st, err := app.LedgerStore()
	if err != nil {
		return ledger.Entry{}, err
	}
	defer st.Close()
	return st.Append(ledger.Entry{
		Kind:     ledger.KindAcceptance,
		Summary:  summary,
		Task:     task,
		Session:  sessionID,
		Model:    model,
		Persona:  persona,
		Operator: ledger.OperatorImplicit,
	})
}
