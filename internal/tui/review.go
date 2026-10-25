package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/gate"
	"kintsugi/internal/ledger"
	"kintsugi/internal/plan"
	"kintsugi/internal/provider"
	"kintsugi/internal/ui"
)

// planReview is the plan workbench's snapshot: the document, its review history
// and the cursor. It is rebuilt from disk on open and on refresh rather than
// mutated in place, so what the surface shows is what is actually stored.
type planReview struct {
	art      plan.Artifact
	rev      *plan.Review
	cursor   int    // 0-based line the selection is on
	compose  bool   // the comment composer is open
	draft    string // the comment being written
	stage    reviewStage
	busy     bool // a review round is running
	diffFrom int  // round to diff against, 0 = no diff
}

// reviewStage is the composer's state machine. It exists because the workbench
// owns the whole keyboard: a text-entry mode has to intercept before the browse
// keys, the same way handleConnectKey sits ahead of the global switch.
type reviewStage int

const (
	reviewBrowse reviewStage = iota
	reviewCompose
)

// reviewResult is what the async review command sends back. It carries the slug
// and round so a result that arrives after the operator moved on is dropped
// rather than applied to the wrong plan.
type reviewResult struct {
	slug  string
	round int
	rev   *gate.Review
	err   error
}

// planLines renders the plan as a numbered document with comments inline under
// the line they belong to. Line numbers are 1-based and match the file, so a
// finding's `file:line` and a comment's anchor refer to the same thing.
func (m *Model) planLines(width int) []string {
	p := m.wb.plan
	if p == nil {
		return nil
	}
	var out []string
	header := fmt.Sprintf("round %d · %d comments · verdict %s",
		p.roundNumber(), p.commentCount(), orDash(p.rev.Verdict))
	if p.busy {
		header = "reviewing… · " + header
	}
	out = append(out, m.theme.Paint(ui.PaperSoft, header))
	gateVerdict, findings := p.gateSummary()
	if gateVerdict != "" {
		out = append(out, m.theme.Paint(ui.PaperSoft, "gate      ")+gateVerdict)
	}
	for _, f := range findings {
		out = append(out, f)
	}
	if p.diffFrom > 0 {
		out = append(out, "")
		out = append(out, m.theme.Paint(ui.Gold, fmt.Sprintf("// diff · round %d → %d", p.diffFrom, p.roundNumber())))
		if diff, err := plan.Diff(m.app.Workspace, p.art.Name, p.diffFrom, p.roundNumber()); err == nil {
			for _, l := range diff {
				out = append(out, m.diffLine(l))
			}
		} else {
			out = append(out, m.theme.Paint(ui.Warn, "✸ "+err.Error()))
		}
		out = append(out, "")
	}
	out = append(out, "")

	lines := strings.Split(strings.TrimRight(p.art.Content, "\n"), "\n")
	byLine := p.commentsByLine()
	// The gutter holds the widest line number plus a marker column.
	gutter := 2
	for i := 10; i <= len(lines); i *= 10 {
		gutter++
	}
	for i, line := range lines {
		num := i + 1
		marker := " "
		tone := ui.PaperSoft
		if i == p.cursor {
			marker = "▸"
			tone = ui.Gold
		}
		label := fmt.Sprintf("%s%*d ", marker, gutter, num)
		// The plan marker is bookkeeping, not content. It keeps its line number
		// — a finding cites file:line, so the numbering must match the file —
		// but it recedes so the plan reads as the operator's document.
		lineTone := tone
		if strings.TrimSpace(line) == plan.Marker {
			lineTone = ui.PaperDim
		}
		body := truncate(expandTabs(line), max(width-gutter-2, 8))
		if lineTone == tone {
			out = append(out, m.theme.Paint(tone, label)+body)
		} else {
			out = append(out, m.theme.Paint(tone, label)+m.theme.Paint(lineTone, body))
		}
		for _, c := range byLine[num] {
			out = append(out, m.commentLine(c, width, gutter))
		}
	}
	if p.compose {
		out = append(out, "")
		out = append(out, m.theme.Paint(ui.Gold, fmt.Sprintf("comment on line %d:", p.cursor+1)))
		kind := "note"
		if k := draftKind(p.draft); k != "" {
			kind = k
		}
		text := strings.TrimLeft(p.draft, "!?-")
		out = append(out, m.theme.Paint(ui.PaperSoft, "  ["+kind+"] ")+text+"▌")
		out = append(out, m.theme.Paint(ui.PaperSoft, "  enter save · esc cancel · start with !  risky · ?  why · -  cut"))
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft,
		"comments are anchored to the line text, so they follow an edit above them"))
	return out
}

// draftKind maps the quick-comment prefix to a stored kind.
func draftKind(draft string) string {
	switch {
	case strings.HasPrefix(draft, "!"):
		return plan.CommentRisky
	case strings.HasPrefix(draft, "?"):
		return plan.CommentWhy
	case strings.HasPrefix(draft, "-"):
		return plan.CommentCut
	}
	return ""
}

// expandTabs keeps a tab-indented plan line from breaking the gutter alignment.
func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

func (m *Model) commentLine(c plan.Comment, width, gutter int) string {
	kind := c.Kind
	if kind == "" {
		kind = "note"
	}
	tone := ui.PaperDim
	switch c.Kind {
	case plan.CommentRisky:
		tone = ui.Danger
	case plan.CommentCut:
		tone = ui.Warn
	case plan.CommentWhy:
		tone = ui.Gold
	}
	indent := strings.Repeat(" ", gutter+2)
	// The anchor state is what keeps a comment honest after the plan moves:
	// "moved"/"gone" says the line it was written on is not there any more.
	line := ""
	if l, st := plan.CommentAnchor(m.wb.plan.art.Content, c); st != plan.AnchorOK {
		switch st {
		case plan.AnchorMoved:
			line = fmt.Sprintf(" · moved to %d", l)
		case plan.AnchorGone:
			line = " · line gone"
		}
	}
	head := fmt.Sprintf("└ %s%s ", kind, line)
	return m.theme.Paint(tone, indent+head) +
		truncate(c.Body, max(width-gutter-len([]rune(head))-2, 8))
}

// commentsByLine indexes the current round's comments by their anchor line in
// the document as it stands now, not as it stood when they were written.
func (p *planReview) commentsByLine() map[int][]plan.Comment {
	out := map[int][]plan.Comment{}
	cur := p.rev.Current()
	if cur == nil {
		return out
	}
	for _, c := range cur.Comments {
		line, _ := plan.CommentAnchor(p.art.Content, c)
		if line <= 0 {
			continue
		}
		out[line] = append(out[line], c)
	}
	return out
}

func (p *planReview) roundNumber() int {
	if cur := p.rev.Current(); cur != nil {
		return cur.N
	}
	return 0
}

func (p *planReview) commentCount() int {
	if cur := p.rev.Current(); cur != nil {
		return len(cur.Comments)
	}
	return 0
}

// gateSummary renders what the reviewer said, in the plan severity vocabulary
// (info|low|medium|high). Code review uses P0..P3 and must not share this.
func (p *planReview) gateSummary() (string, []string) {
	cur := p.rev.Current()
	if cur == nil || cur.GateVerdict == "" {
		return "", nil
	}
	verdict := cur.GateVerdict
	var out []string
	for _, f := range cur.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		line := fmt.Sprintf("  %-6s %s — %s", f.Severity, loc, oneLine(f.Consequence))
		out = append(out, line)
	}
	if len(cur.NotAFinding) > 0 {
		out = append(out, fmt.Sprintf("  %d checked and fine", len(cur.NotAFinding)))
	}
	return verdict, out
}

func (m *Model) diffLine(l string) string {
	if l == "" {
		return ""
	}
	switch l[0] {
	case '+':
		return m.theme.Paint(ui.OK, l)
	case '-':
		return m.theme.Paint(ui.Danger, l)
	}
	return m.theme.Paint(ui.PaperSoft, l)
}

// planSnapshot is the plan body on its own, used for the scroll mirror.
func (m *Model) openPlanWorkbench(name string) {
	if m.app == nil {
		m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
		return
	}
	art, err := plan.Load(m.app.Workspace, name)
	if err != nil {
		m.addNote("✗ open plan: " + err.Error() + " · /plan lists the saved ones")
		return
	}
	rev, err := plan.LoadReview(m.app.Workspace, art.Name)
	if err != nil {
		m.addNote("✗ load review: " + err.Error() + " · /plan reopens the plan and starts a fresh round")
		return
	}
	p := &planReview{art: art, rev: rev}
	p.cursor = p.firstBodyLine()
	m.wb = &workbenchView{kind: "plan", plan: p}
	if cur := rev.Current(); cur == nil {
		m.wb.notices = append(m.wb.notices, "· unreviewed · enter or r opens round 1")
	}
	if rev.Decided() {
		m.wb.notices = append(m.wb.notices, "· verdict "+rev.Verdict+" recorded")
	}
}

// firstBodyLine puts the cursor on the first line after the heading, which is
// where the steps start: landing on the marker or the title would make the first
// keystroke feel pointless.
func (p *planReview) firstBodyLine() int {
	lines := strings.Split(p.art.Content, "\n")
	seenHeading := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "# ") {
			seenHeading = true
			continue
		}
		if seenHeading && t != "" && t != plan.Marker {
			return i
		}
	}
	return 0
}

func (p *planReview) lineCount() int {
	return len(strings.Split(p.art.Content, "\n"))
}

// moveCursor moves the selection and keeps the scroll window following it, so
// selecting a line near the edge is always visible.
func (m *Model) reviewMoveCursor(delta int) {
	p := m.wb.plan
	if p == nil {
		return
	}
	p.cursor = clampInt(p.cursor+delta, 0, p.lineCount()-1)
	// The rendered document is offset by the header block, so the cursor's
	// screen row is not its line number; bring it into view rather than guess.
	m.reviewRevealCursor()
}

// reviewRevealCursor scrolls the workbench so the selected line is visible. It
// counts the header rows the same helper renders, so the two cannot drift.
func (m *Model) reviewRevealCursor() {
	p := m.wb.plan
	if p == nil {
		return
	}
	height := max(m.bodyHeight()-3, 1)
	row := m.reviewCursorRow()
	if row < m.wb.scroll {
		m.wb.scroll = row
	}
	if row >= m.wb.scroll+height {
		m.wb.scroll = row - height + 1
	}
}

// reviewCursorRow is the rendered row of the selected line: the header block,
// the blank separator, then one row per document line, plus any comment rows
// above it.
func (m *Model) reviewCursorRow() int {
	p := m.wb.plan
	if p == nil {
		return 0
	}
	header := 1 // the round summary
	if v, f := p.gateSummary(); v != "" {
		header += 1 + len(f)
	}
	if p.diffFrom > 0 {
		header += 2 // the ""
		if d, err := plan.Diff(m.app.Workspace, p.art.Name, p.diffFrom, p.roundNumber()); err == nil {
			header += len(d) + 1
		}
	}
	header += 2 // "" + ""

	lines := strings.Split(strings.TrimRight(p.art.Content, "\n"), "\n")
	byLine := p.commentsByLine()
	row := header
	for i := 0; i < p.cursor && i < len(lines); i++ {
		row++
		row += len(byLine[i+1])
	}
	return row
}

// ---------- review actions ----------

// runPlanReview starts a round and asks the reviewer. The command is async: a
// review is a model call, and blocking Update on it would freeze the screen.
func (m *Model) runPlanReview() tea.Cmd {
	p := m.wb.plan
	if p == nil || p.busy {
		return nil
	}
	if m.app == nil {
		m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
		return nil
	}
	reviewer, err := m.app.GateReviewer()
	if err != nil {
		m.addNote("✗ reviewer: " + err.Error() + " · bind a model with /model")
		return nil
	}
	// A round reviews a plan version, so pressing r again on unchanged text
	// refreshes the round it already has instead of orphaning the comments
	// written against it.
	rev, round, err := plan.EnsureRound(m.app.Workspace, p.art)
	if err != nil {
		m.addNote("✗ start round: " + err.Error() + " · /plan reopens the plan, then r retries")
		return nil
	}
	p.rev = rev
	p.busy = true
	slug := p.art.Name
	n := round.N
	art := p.art
	scope := m.app.ScopeText()
	return func() tea.Msg {
		res, err := reviewer.ReviewArtifact(m.ctx, gate.Artifact{
			Kind: gate.ArtifactPlan, Content: art.Content, Path: art.Path,
		}, scope)
		return reviewResult{slug: slug, round: n, rev: res, err: err}
	}
}

// handleReviewResult stores a finished round. A result for a plan the operator
// has since closed is dropped: applying it would write to the wrong surface.
func (m *Model) handleReviewResult(msg reviewResult) (tea.Model, tea.Cmd) {
	p := m.planReview()
	if p == nil || p.art.Name != msg.slug {
		return m, nil
	}
	p.busy = false
	if msg.err != nil {
		m.addNote("✗ review: " + msg.err.Error() + " · the round is recorded without findings")
		return m, nil
	}
	cur := p.rev.Current()
	if cur == nil || cur.N != msg.round {
		return m, nil
	}
	cur.GateVerdict = msg.rev.Verdict
	cur.NotAFinding = msg.rev.NotAFinding
	for _, f := range msg.rev.Findings {
		cur.Findings = append(cur.Findings, plan.StoredFinding{
			Severity: f.Severity, File: f.File, Line: f.Line,
			Consequence: f.Consequence, Evidence: f.Evidence,
		})
	}
	if err := plan.SaveReview(m.app.Workspace, p.rev); err != nil {
		m.addNote("✗ save review: " + err.Error() + " · check .kintsugi/plans is writable, then r retries")
		return m, nil
	}
	tone := ui.OK
	if msg.rev.Verdict != "pass" {
		tone = ui.Warn
	}
	var body []string
	body = append(body, fmt.Sprintf("verdict  %s", msg.rev.Verdict))
	for _, f := range msg.rev.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		body = append(body, fmt.Sprintf("  [%s] %s — %s", f.Severity, loc, oneLine(f.Consequence)))
	}
	if len(msg.rev.NotAFinding) > 0 {
		body = append(body, fmt.Sprintf("  %d checked and fine", len(msg.rev.NotAFinding)))
	}
	if len(msg.rev.Findings) == 0 {
		body = append(body, "  no findings")
	}
	m.addCard(fmt.Sprintf("// plan review · round %d", msg.round), strings.Join(body, "\n"), tone)
	return m, nil
}

func (m *Model) planReview() *planReview {
	if m.wb == nil || m.wb.kind != "plan" {
		return nil
	}
	return m.wb.plan
}

// saveDraftComment stores the composer's text against the selected line.
func (m *Model) saveDraftComment() {
	p := m.planReview()
	if p == nil {
		return
	}
	body := strings.TrimSpace(strings.TrimLeft(p.draft, "!?-"))
	if body == "" {
		m.addNote("· empty comment discarded")
		p.compose, p.draft = false, ""
		return
	}
	lines := strings.Split(p.art.Content, "\n")
	text := ""
	if p.cursor >= 0 && p.cursor < len(lines) {
		text = strings.TrimSpace(lines[p.cursor])
	}
	rev, err := plan.AddComment(m.app.Workspace, p.art, plan.Comment{
		Line: p.cursor + 1,
		Text: text,
		Body: body,
		Kind: draftKind(p.draft),
	})
	if err != nil {
		m.addNote("✗ add comment: " + err.Error() + " · the draft is still open · enter retries, esc cancels")
		return
	}
	p.rev = rev
	p.compose, p.draft = false, ""
	m.addNote(fmt.Sprintf("· comment saved on line %d", p.cursor+1))
}

// setPlanVerdict records the operator's decision and, on approval, queues the
// plan for the next turn so "approve" means execute rather than a note saying
// to retype the task.
func (m *Model) setPlanVerdict(verdict string) tea.Cmd {
	p := m.planReview()
	if p == nil {
		return nil
	}
	rev, err := plan.SetVerdict(m.app.Workspace, p.art.Name, verdict, "", ledger.OperatorAttested)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return nil
	}
	p.rev = rev
	if err := m.recordPlanDecision(rev, p.art, verdict); err != nil {
		m.addNote("✗ ledger: " + err.Error())
	}
	if verdict != plan.VerdictApprove {
		m.addNote("× plan rejected · the workspace ledger records the decision")
		return nil
	}
	// The execute seam: the same system message the architect injects before
	// the edit turn, queued for the next task.
	m.hist = append(m.hist, approvedPlanMessage(p.art.Content, p.art.Path))
	m.wb = nil
	m.addNote("✓ plan approved · it rides into the next turn as the approved plan")
	return nil
}

// approvedPlanMessage mirrors kintsugi's architect message so a plan approved in
// the TUI reaches the model in the same shape as one approved by the arch phase.
func approvedPlanMessage(planText, path string) provider.Message {
	var b strings.Builder
	b.WriteString("## Approved plan (from the architect)\n")
	b.WriteString(strings.TrimSpace(planText))
	if path != "" {
		b.WriteString("\n\nPlan file: ")
		b.WriteString(path)
	}
	return provider.Message{Role: "system", Content: b.String()}
}

// recordPlanDecision writes the operator's call to the workspace ledger. An
// approval is attested — the operator clicked it — while a rejection is a
// decision, which is the recallable kind for "we chose not to".
func (m *Model) recordPlanDecision(rev *plan.Review, art plan.Artifact, verdict string) error {
	if m.app == nil {
		return fmt.Errorf("no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
	}
	st, err := m.app.LedgerStore()
	if err != nil {
		return err
	}
	defer st.Close()
	kind := ledger.KindAcceptance
	summary := "approved plan: " + plan.Title(art.Content)
	if verdict != plan.VerdictApprove {
		kind = ledger.KindDecision
		summary = "rejected plan: " + plan.Title(art.Content)
	}
	_, err = st.Append(ledger.Entry{
		Kind:     kind,
		Summary:  summary,
		Task:     art.Path,
		Session:  m.sessionID(),
		Model:    m.viewModel(),
		Persona:  m.personaName(),
		Operator: ledger.OperatorAttested,
	})
	return err
}
