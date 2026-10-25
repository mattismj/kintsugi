package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"kintsugi/internal/agent"
	"kintsugi/internal/config"
	"kintsugi/internal/continuity"
	"kintsugi/internal/harness"
	"kintsugi/internal/ledger"
	"kintsugi/internal/memory"
	"kintsugi/internal/ui"
)

// continuityModel is a panel model bound to a throwaway workspace with the
// continuity layer enabled and a loop attached. The cache dir is redirected so
// the preflight scorer never reads the operator's real session telemetry.
func continuityModel(t *testing.T) *Model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	m := testModel(120, 44)
	m.app = &harness.App{
		Cfg: config.File{Continuity: config.Continuity{
			Enabled:               true,
			PriorWorkTopK:         5,
			PriorWorkBudgetTokens: 600,
			Preflight:             true,
			GuardOnRefusal:        true,
		}},
		Workspace: t.TempDir(),
	}
	m.loop = &agent.Loop{}
	return m
}

// seedAcceptedWork writes one accepted plan into the workspace ledger.
func seedAcceptedWork(t *testing.T, m *Model, summary string) string {
	t.Helper()
	st, err := m.app.LedgerStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e, err := st.Append(ledger.Entry{Kind: ledger.KindPlan, Summary: summary, Evidence: []ledger.Evidence{{Path: "docs/corpus.md"}}})
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func TestContinuityCardsRender(t *testing.T) {
	m := testModel(120, 44)
	m.addCard("// preflight · frame injected", "score 4 · class moralizing\noperator frame queued as a system message for this turn", ui.Warn)
	m.addCard("// continuity · prior accepted work", "1 item(s) · ids L-20261021-001\nqueued as a system message for this turn", ui.Gold)
	m.addCard("// continuity · drift metadata", "matched L-20261021-001 · class moralizing · outcome resumed", ui.Warn)
	m.addCard("// ledger · recent", "L-20261021-001 plan  Fuzz the parser against the lab corpus", ui.Gold)
	m.addCard("// recall · prior accepted work", "query fuzz\nids L-20261021-001", ui.Gold)

	out := m.View()
	for _, want := range []string{
		"// preflight · frame injected",
		"// continuity · prior accepted work",
		"// continuity · drift metadata",
		"// ledger · recent",
		"// recall · prior accepted work",
		"L-20261021-001",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestAcceptThenLedgerThenRecall(t *testing.T) {
	m := continuityModel(t)
	m.tasks = append(m.tasks, taskItem{label: "fuzz the parser against the lab corpus", state: "running"})

	m.handleSlash("/accept fuzz the parser against the lab corpus")
	last := m.blocks[len(m.blocks)-1]
	// Card titles carry the identity glyph in front of the eyebrow (§7.4.3).
	if last.kind != blockCard || !strings.HasSuffix(last.title, "// continuity · acceptance recorded") {
		t.Fatalf("/accept card = %+v", last)
	}
	if !strings.HasPrefix(last.title, ui.Indicator(ui.MascotWorking)) {
		t.Fatalf("/accept card title must carry the working glyph: %q", last.title)
	}

	rows, err := ledgerRows(m.app, ledger.KindAcceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Summary, "lab corpus") {
		t.Fatalf("acceptance entries = %+v", rows)
	}

	m.handleSlash("/ledger")
	last = m.blocks[len(m.blocks)-1]
	if !strings.HasSuffix(last.title, "// ledger · recent") || !strings.Contains(last.text, "lab corpus") {
		t.Fatalf("/ledger card = %+v", last)
	}

	// /recall defaults to the last task, which is what was just accepted.
	m.handleSlash("/recall")
	last = m.blocks[len(m.blocks)-1]
	if !strings.HasSuffix(last.title, "// recall · prior accepted work") || !strings.Contains(last.text, rows[0].ID) {
		t.Fatalf("/recall card = %+v", last)
	}
	if strings.Count(last.text, "\n- L-") != 1 || strings.Contains(last.text, "drift") {
		t.Fatalf("recall card should cite accepted work only:\n%s", last.text)
	}

	m.handleSlash("/recall nothing-matches-this-query")
	last = m.blocks[len(m.blocks)-1]
	if last.kind != blockNote || !strings.Contains(last.text, "no prior accepted work") {
		t.Fatalf("empty recall = %+v", last)
	}
}

func TestAcceptRejectsEmptySummary(t *testing.T) {
	m := continuityModel(t)
	m.handleSlash("/accept")
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockNote || !strings.Contains(last.text, "usage: /accept") {
		t.Fatalf("/accept with no summary = %+v", last)
	}
}

func TestGuardToggle(t *testing.T) {
	m := continuityModel(t)
	m.ch = make(chan tea.Msg, 1)

	m.handleSlash("/guard on")
	if m.guard == nil || m.loop.GuardRefusal == nil || m.loop.OnDrift == nil {
		t.Fatalf("guard not installed: %+v", m.guard)
	}
	if m.ledger == nil {
		t.Fatal("ledger not opened for the session")
	}

	m.handleSlash("/guard off")
	if m.guard != nil || m.loop.GuardRefusal != nil || m.loop.OnDrift != nil {
		t.Fatal("guard still installed after /guard off")
	}
	if m.app.Cfg.Continuity.GuardOnRefusal {
		t.Fatal("/guard off did not clear guard_on_refusal for the session")
	}

	m.handleSlash("/guard")
	last := m.blocks[len(m.blocks)-1]
	if !strings.Contains(last.text, "continuity guard off") {
		t.Fatalf("/guard status = %+v", last)
	}
}

func TestContinuityInjectionUsesAcceptedWorkOnly(t *testing.T) {
	m := continuityModel(t)
	id := seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")

	// Drift metadata must never be recalled: it is not memory.
	if err := m.app.RecordDrift("fuzz the parser against the lab corpus", "moralizing", "handoff", []string{id}); err != nil {
		t.Fatal(err)
	}

	inj := continuityForTask(m.app, m.res, "fuzz the parser against the lab corpus")
	if len(inj.Prior) != 1 || inj.Prior[0] != id {
		t.Fatalf("recalled ids = %v, want [%s]", inj.Prior, id)
	}
	if len(inj.Messages) == 0 {
		t.Fatal("no system messages queued for the turn")
	}
	body := inj.Messages[len(inj.Messages)-1].Content
	if inj.Messages[len(inj.Messages)-1].Role != "system" {
		t.Fatalf("prior work was not queued as a system message: %+v", inj.Messages)
	}
	if !strings.Contains(body, continuity.BlockHeader) || !strings.Contains(body, continuity.PriorWorkHeader) {
		t.Fatalf("prior-work block not shaped as a continuity message:\n%s", body)
	}
	if !strings.Contains(body, "lab corpus") {
		t.Fatalf("recalled summary missing:\n%s", body)
	}
	// One accepted entry, and no drift row: a drift event is metadata, and the
	// block's only mention of "refusal" is the fixed framing sentence that says
	// the block is not evidence of one.
	if strings.Count(body, "\n- L-") != 1 || strings.Contains(body, "drift") {
		t.Fatalf("frame cited something other than the accepted entry:\n%s", body)
	}
	if strings.Count(body, "refusal") != strings.Count(continuity.PriorWorkFraming, "refusal") {
		t.Fatalf("frame carries text about a refusal beyond the fixed framing:\n%s", body)
	}
}

func TestSubmitInjectionAddsCardsAndSystemMessages(t *testing.T) {
	m := continuityModel(t)
	id := seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")

	m.injectContinuity("fuzz the parser against the lab corpus")

	if len(m.hist) != 1 || m.hist[0].Role != "system" {
		t.Fatalf("hist after injection = %+v", m.hist)
	}
	cards := 0
	var titles []string
	for _, b := range m.blocks {
		if b.kind == blockCard {
			cards++
			titles = append(titles, b.title)
		}
	}
	if cards == 0 || !strings.Contains(strings.Join(titles, "\n"), "// continuity · prior accepted work") {
		t.Fatalf("injection cards = %v", titles)
	}
	last := m.blocks[len(m.blocks)-1]
	if !strings.Contains(last.text, id) {
		t.Fatalf("prior-work card does not name the ledger id: %+v", last)
	}
}

func TestInjectionDisabledWhenContinuityOff(t *testing.T) {
	m := continuityModel(t)
	seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")
	m.app.Cfg.Continuity.Enabled = false

	m.injectContinuity("fuzz the parser against the lab corpus")
	if len(m.hist) != 0 {
		t.Fatalf("disabled continuity still injected: %+v", m.hist)
	}
}

// ---------- mental model (M10-C) ----------

// A fresh session opens with the orientation digest: accepted work plus the
// newest lessons, queued as a system message and named in the transcript.
func TestMentalModelInjectsOnAFreshSession(t *testing.T) {
	m := continuityModel(t)
	m.startedFresh = true
	seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")

	m.injectMentalModel()

	if len(m.hist) != 1 || m.hist[0].Role != "system" {
		t.Fatalf("hist after digest injection = %+v", m.hist)
	}
	body := m.hist[0].Content
	if !strings.Contains(body, memory.MentalModelHeader) {
		t.Fatalf("the digest is not labelled:\n%s", body)
	}
	if !strings.Contains(body, "lab corpus") {
		t.Fatalf("accepted work missing from the digest:\n%s", body)
	}
	if got := memory.Tokens(body); got > 200 {
		t.Fatalf("digest is %d tokens, want <= 200", got)
	}
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockCard || !strings.Contains(last.title, memory.MentalModelLabel) {
		t.Fatalf("the digest was not surfaced in the transcript: %+v", last)
	}
}

// It is orientation, not per-turn context: one injection per session, so the
// cached prefix stays byte-identical turn to turn.
func TestMentalModelInjectsOnlyOnce(t *testing.T) {
	m := continuityModel(t)
	m.startedFresh = true
	seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")

	m.injectMentalModel()
	m.injectMentalModel()
	m.injectMentalModel()

	if n := strings.Count(strings.Join(m.histContent(), "\n"), memory.MentalModelHeader); n != 1 {
		t.Fatalf("the digest was injected %d times, want 1", n)
	}
}

// A resumed session already carries its own history; re-telling it what it just
// said would duplicate context the operator has seen.
func TestMentalModelSkipsAResumedSession(t *testing.T) {
	m := continuityModel(t)
	m.startedFresh = false
	seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")

	m.injectMentalModel()

	if len(m.hist) != 0 {
		t.Fatalf("a resumed session got the digest: %+v", m.hist)
	}
}

// Drift metadata is not memory. The digest reads positive kinds only, so a drift
// event cannot reach it — the same invariant the prior-work block holds.
func TestMentalModelExcludesDrift(t *testing.T) {
	m := continuityModel(t)
	m.startedFresh = true
	id := seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")
	if err := m.app.RecordDrift("fuzz the parser against the lab corpus", "moralizing", "handoff", []string{id}); err != nil {
		t.Fatal(err)
	}

	m.injectMentalModel()

	if len(m.hist) != 1 {
		t.Fatalf("hist = %+v, want the digest only", m.hist)
	}
	body := m.hist[0].Content
	if strings.Contains(body, "drift") {
		t.Fatalf("the digest cited a drift event:\n%s", body)
	}
	// The drift row's own metadata must not leak in either: the digest names
	// accepted work by its ledger id, and a drift event is metadata-only.
	for _, banned := range []string{"moralizing", "handoff"} {
		if strings.Contains(body, banned) {
			t.Fatalf("the digest carried drift metadata %q:\n%s", banned, body)
		}
	}
	if strings.Count(body, "\n- L-") != 1 {
		t.Fatalf("the digest cited more than the one accepted entry:\n%s", body)
	}
}

// Nothing accepted yet means nothing to orient on: no empty block, no card.
func TestMentalModelInjectsNothingWithoutAcceptedWork(t *testing.T) {
	m := continuityModel(t)
	m.startedFresh = true

	m.injectMentalModel()

	if len(m.hist) != 0 {
		t.Fatalf("an empty ledger still injected a digest: %+v", m.hist)
	}
	for _, b := range m.blocks {
		if b.kind == blockCard && strings.Contains(b.title, memory.MentalModelLabel) {
			t.Fatalf("an empty digest surfaced a card: %+v", b)
		}
	}
}

func TestMentalModelDisabledWhenContinuityOff(t *testing.T) {
	m := continuityModel(t)
	m.startedFresh = true
	seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")
	m.app.Cfg.Continuity.Enabled = false

	m.injectMentalModel()

	if len(m.hist) != 0 {
		t.Fatalf("disabled continuity still injected the digest: %+v", m.hist)
	}
}

// histContent is the role/content of every queued message, for counting.
func (m *Model) histContent() []string {
	out := make([]string, 0, len(m.hist))
	for _, msg := range m.hist {
		out = append(out, msg.Role+": "+msg.Content)
	}
	return out
}

func TestDriftNoticeRendersMetadataCard(t *testing.T) {
	m := continuityModel(t)
	m.ch = make(chan tea.Msg, 1)

	_, cmd := m.Update(driftNotice{matched: []string{"L-20261021-001", "L-20261021-002"}, class: "moralizing", outcome: "resumed"})
	if cmd == nil {
		t.Fatal("drift notice did not re-arm the channel listener")
	}
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockCard || !strings.HasSuffix(last.title, "// continuity · drift metadata") {
		t.Fatalf("drift card = %+v", last)
	}
	if !strings.HasPrefix(last.title, ui.Indicator(ui.MascotError)) {
		t.Fatalf("drift card title must carry the error glyph: %q", last.title)
	}
	for _, want := range []string{"L-20261021-001", "L-20261021-002", "moralizing", "resumed"} {
		if !strings.Contains(last.text, want) {
			t.Fatalf("drift card missing %q: %s", want, last.text)
		}
	}
	if !strings.Contains(m.View(), "// continuity · drift metadata") {
		t.Fatal("drift card not rendered")
	}
	assertFits(t, m)
}

func TestDriftNoticeWithoutMatches(t *testing.T) {
	m := continuityModel(t)
	m.ch = make(chan tea.Msg, 1)
	m.Update(driftNotice{class: "identity", outcome: "handoff"})
	last := m.blocks[len(m.blocks)-1]
	if !strings.Contains(last.text, "none") {
		t.Fatalf("no-match drift card = %+v", last)
	}
}

// The reference is grouped now, so a command lives under its group rather than
// in one flat list. The contract that matters is unchanged — every continuity
// verb must be documented and reachable — so the check expands the group that
// owns them.
func TestHelpListsContinuityCommands(t *testing.T) {
	m := testModel(100, 30)
	m.handleSlash("/help review")
	help := m.blocks[len(m.blocks)-1].text
	for _, want := range []string{"/recall", "/ledger", "/accept"} {
		if !strings.Contains(help, want) {
			t.Fatalf("/help review missing %q:\n%s", want, help)
		}
	}
	m.handleSlash("/help permissions")
	if guard := m.blocks[len(m.blocks)-1].text; !strings.Contains(guard, "/guard") {
		t.Fatalf("/help permissions missing /guard:\n%s", guard)
	}
}

func TestPlainContinuityOutput(t *testing.T) {
	m := continuityModel(t)
	id := seedAcceptedWork(t, m, "Fuzz the parser against the lab corpus")

	recall := strings.Join(plainRecall(m.app, "fuzz the parser corpus"), "\n")
	if !strings.Contains(recall, id) || !strings.Contains(recall, "lab corpus") {
		t.Fatalf("plain /recall = %s", recall)
	}
	rows := strings.Join(plainLedger(m.app, ""), "\n")
	if !strings.Contains(rows, id) || !strings.Contains(rows, "never recallable") {
		t.Fatalf("plain /ledger = %s", rows)
	}
	if got := strings.Join(plainRecall(m.app, "nothing-matches-this-query"), "\n"); !strings.Contains(got, "no prior accepted work") {
		t.Fatalf("plain /recall empty = %s", got)
	}
}
