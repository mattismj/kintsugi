package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"kintsugi/internal/harness"
	"kintsugi/internal/plan"
)

// reviewWorkspace builds a workspace holding one plan, ready to review.
func reviewWorkspace(t *testing.T, name, body string) (string, plan.Artifact) {
	t.Helper()
	ws := t.TempDir()
	art, err := plan.Save(ws, name, body)
	if err != nil {
		t.Fatalf("plan.Save: %v", err)
	}
	return ws, art
}

// reviewModel is a model bound to a workspace with one plan, with the plan
// workbench open. It uses the real App so the ledger and the plan store are the
// ones the operator would get.
func reviewModel(t *testing.T, w, h int) (*Model, plan.Artifact) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws, art := reviewWorkspace(t, "Add health check",
		"# Add health check\n\n1. read the config\n2. patch the handler\n3. add a test\n")

	app, err := harness.New(ws)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	m := testModel(w, h)
	m.app = app
	m.openPlanWorkbench(art.Name)
	if m.wb == nil || m.wb.kind != "plan" {
		t.Fatalf("the plan workbench did not open: %+v", m.wb)
	}
	return m, art
}

func TestPlanWorkbenchRendersTheDocumentWithLineNumbers(t *testing.T) {
	m, _ := reviewModel(t, 140, 40)
	out := plain(m.View())
	for _, want := range []string{"// 04 plan", "add-health-check", "4 ", "read the config", "patch the handler", "unreviewed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan workbench missing %q:\n%s", want, out)
		}
	}
	assertFits(t, m)
}

func TestPlanWorkbenchCursorMovesAndStaysVisible(t *testing.T) {
	m, _ := reviewModel(t, 100, 30)
	start := m.wb.plan.cursor

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.wb.plan.cursor != start+1 {
		t.Fatalf("ctrl+n moved the cursor to %d, want %d", m.wb.plan.cursor, start+1)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.wb.plan.cursor != start {
		t.Fatalf("ctrl+p moved the cursor to %d, want %d", m.wb.plan.cursor, start)
	}
	// j still scrolls, so a document cursor cannot break reading.
	before := m.wb.scroll
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.wb.plan.cursor != start {
		t.Fatal("j moved the cursor instead of scrolling")
	}
	if m.wb.scroll <= before {
		t.Fatal("j did not scroll")
	}
}

func TestPlanWorkbenchCursorClampsAtTheEnds(t *testing.T) {
	m, _ := reviewModel(t, 100, 30)
	for i := 0; i < 100; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	}
	if got, want := m.wb.plan.cursor, m.wb.plan.lineCount()-1; got != want {
		t.Fatalf("cursor = %d, want the last line %d", got, want)
	}
	for i := 0; i < 100; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	}
	if m.wb.plan.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.wb.plan.cursor)
	}
}

func TestComposerSavesACommentOnTheSelectedLine(t *testing.T) {
	m, art := reviewModel(t, 120, 34)
	m.wb.plan.cursor = 3 // "1. read the config"

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if !m.wb.plan.compose {
		t.Fatal("c did not open the composer")
	}
	for _, r := range "why the loader?" {
		if r == ' ' {
			m.Update(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(keyEnter())

	if m.wb.plan.compose {
		t.Fatal("the composer stayed open after saving")
	}
	rev, err := plan.LoadReview(m.app.Workspace, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	cur := rev.Current()
	if cur == nil || len(cur.Comments) != 1 {
		t.Fatalf("comment not stored: %+v", cur)
	}
	if got := cur.Comments[0].Body; got != "why the loader?" {
		t.Fatalf("body = %q", got)
	}
	if cur.Comments[0].Line != 4 {
		t.Fatalf("line = %d, want 4", cur.Comments[0].Line)
	}
	if got := cur.Comments[0].Text; got != "1. read the config" {
		t.Fatalf("anchor text = %q", got)
	}
	out := plain(m.View())
	if !strings.Contains(out, "why the loader?") {
		t.Fatalf("the comment is not rendered inline:\n%s", out)
	}
	assertFits(t, m)
}

func TestComposerQuickVerbsSetTheKind(t *testing.T) {
	m, art := reviewModel(t, 120, 34)
	m.wb.plan.cursor = 4

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	for _, r := range "!touches prod" {
		if r == ' ' {
			m.Update(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(keyEnter())

	rev, err := plan.LoadReview(m.app.Workspace, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	c := rev.Current().Comments[0]
	if c.Kind != plan.CommentRisky {
		t.Fatalf("kind = %q, want %q", c.Kind, plan.CommentRisky)
	}
	if c.Body != "touches prod" {
		t.Fatalf("the verb prefix leaked into the body: %q", c.Body)
	}
}

func TestComposerEscDiscards(t *testing.T) {
	m, art := reviewModel(t, 120, 34)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nope")})
	m.Update(keyEsc())

	if m.wb.plan.compose || m.wb.plan.draft != "" {
		t.Fatal("esc left the composer open")
	}
	rev, err := plan.LoadReview(m.app.Workspace, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	if rev.Current() != nil && len(rev.Current().Comments) != 0 {
		t.Fatal("a discarded comment was stored")
	}
}

func TestComposerKeysNeverReachTheInputLine(t *testing.T) {
	m, _ := reviewModel(t, 120, 34)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	for _, r := range "abc" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.input != "" {
		t.Fatalf("composer typing leaked into the transcript input: %q", m.input)
	}
	if m.wb.plan.draft != "abc" {
		t.Fatalf("draft = %q", m.wb.plan.draft)
	}
}

func TestApproveRecordsAnAttestedLedgerEntryAndQueuesThePlan(t *testing.T) {
	m, art := reviewModel(t, 120, 34)

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = mm.(*Model)

	if m.wb != nil {
		t.Fatal("approving left the workbench open")
	}
	// The execute seam: the plan is queued as a system message for the next turn.
	var found bool
	for _, msg := range m.hist {
		if msg.Role == "system" && strings.Contains(msg.Content, "Approved plan") &&
			strings.Contains(msg.Content, art.Path) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the approved plan was not queued for the next turn: %+v", m.hist)
	}
	rev, err := plan.LoadReview(m.app.Workspace, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	if rev.Verdict != plan.VerdictApprove {
		t.Fatalf("verdict = %q", rev.Verdict)
	}
	if rev.Operator != "attested" {
		t.Fatalf("operator = %q, want attested: an explicit click is not implicit consent", rev.Operator)
	}
}

func TestRejectRecordsADecisionAndQueuesNothing(t *testing.T) {
	m, art := reviewModel(t, 120, 34)
	before := len(m.hist)

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = mm.(*Model)

	if len(m.hist) != before {
		t.Fatal("rejecting queued a plan for execution")
	}
	rev, err := plan.LoadReview(m.app.Workspace, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	if rev.Verdict != plan.VerdictReject {
		t.Fatalf("verdict = %q", rev.Verdict)
	}
}

func TestDiffTogglesBetweenRounds(t *testing.T) {
	m, art := reviewModel(t, 120, 40)
	if _, _, err := plan.BeginRound(m.app.Workspace, art); err != nil {
		t.Fatalf("BeginRound: %v", err)
	}
	edited, err := plan.Save(m.app.Workspace, art.Name,
		"# Add health check\n\n1. read the config\n2. patch the handler\n3. add a test\n4. update the docs\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := plan.BeginRound(m.app.Workspace, edited); err != nil {
		t.Fatalf("BeginRound: %v", err)
	}
	m.openPlanWorkbench(art.Name)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	out := plain(m.View())
	if !strings.Contains(out, "// diff") {
		t.Fatalf("d did not show the diff:\n%s", out)
	}
	if !strings.Contains(out, "update the docs") {
		t.Fatalf("the diff does not show the added step:\n%s", out)
	}
	assertFits(t, m)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if m.wb.plan.diffFrom != 0 {
		t.Fatal("d did not toggle the diff off")
	}
}

func TestPlanWorkbenchBodyMatchesTheScrollMirror(t *testing.T) {
	m, _ := reviewModel(t, 120, 34)
	width := m.w - 2
	// workbenchLines is what the wheel clamps against; workbenchBody is what
	// renders. They build the same content, and a kind added to only one of them
	// would silently desync the scroll. This is the guard for that.
	lines := m.workbenchLines(width)
	body := strings.Split(m.workbenchBody(width, len(lines)), "\n")
	for i := range lines {
		if i >= len(body) {
			break
		}
		if body[i] != lines[i] {
			t.Fatalf("row %d differs between workbenchLines and workbenchBody:\n%q\n%q", i, lines[i], body[i])
		}
	}
}

func TestPlanWorkbenchAtSmallSizes(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {84, 26}, {100, 30}, {120, 34}} {
		m, _ := reviewModel(t, size[0], size[1])
		assertFits(t, m)
		// The body must not be empty at any of these: the surface exists to be
		// read at the smallest supported terminal.
		if out := plain(m.View()); !strings.Contains(out, "// 04 plan") {
			t.Fatalf("%dx%d: the plan surface did not render", size[0], size[1])
		}
	}
}

func TestReviewRoundIsRecordedEvenWhenTheReviewerFails(t *testing.T) {
	m, art := reviewModel(t, 120, 34)
	m.wb.plan.busy = true

	mm, _ := m.Update(reviewResult{slug: art.Name, round: 0, err: os.ErrDeadlineExceeded})
	m = mm.(*Model)

	if m.wb.plan.busy {
		t.Fatal("a failed review left the surface busy")
	}
	out := transcriptText(m)
	if !strings.Contains(out, "review") {
		t.Fatalf("the failure was not reported:\n%s", out)
	}
}

func TestReviewResultForAnotherPlanIsDropped(t *testing.T) {
	m, _ := reviewModel(t, 120, 34)
	m.wb.plan.busy = true

	mm, _ := m.Update(reviewResult{slug: "some-other-plan", round: 1})
	m = mm.(*Model)

	if !m.wb.plan.busy {
		t.Fatal("a result for a different plan cleared this one's busy state")
	}
}

func TestPlanListingStillReachable(t *testing.T) {
	m, art := reviewModel(t, 120, 34)
	m.wb = nil
	m.handleSlash("/plan list")
	if out := transcriptText(m); !strings.Contains(out, art.Name) {
		t.Fatalf("/plan list did not list the plan:\n%s", out)
	}
}

func TestUnknownPlanNameReportsAnError(t *testing.T) {
	m, _ := reviewModel(t, 120, 34)
	m.wb = nil
	m.handleSlash("/plan nope")
	if m.wb != nil {
		t.Fatal("a missing plan opened a workbench")
	}
	if out := transcriptText(m); !strings.Contains(out, "✗") {
		t.Fatalf("the failure was not reported:\n%s", out)
	}
}

// planFixtureDir is where the review record must land: beside the plan, never
// in a shared user-level directory.
func TestReviewRecordLivesBesideThePlan(t *testing.T) {
	ws, art := reviewWorkspace(t, "Add health check", "# Add health check\n\n1. step\n")
	if _, err := plan.AddComment(ws, art, plan.Comment{Line: 3, Text: "1. step", Body: "hi"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	path := plan.ReviewPath(ws, art.Name)
	if filepath.Dir(path) != plan.Dir(ws) {
		t.Fatalf("review record at %s, want it under %s", path, plan.Dir(ws))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("review record missing: %v", err)
	}
}
