package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"kintsugi/internal/config"
	"kintsugi/internal/harness"
)

// ---------- helpers ----------

// suggestModel is a model bound to a workspace with a few files and a custom
// command, so completions have something to offer.
func suggestModel(t *testing.T, w, h int) *Model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	for _, f := range []string{"main.go", "README.md", "internal/tui/view.go", "internal/tui/complete.go"} {
		full := filepath.Join(ws, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(ws, ".kintsugi", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.md"),
		[]byte("---\ndescription: review a diff\nargument-hint: <path>\n---\nReview $1.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := testModel(w, h)
	m.app = &harness.App{Cfg: config.File{}, Workspace: ws}
	return m
}

// press is Update with the type assertion the key handlers need.
func press(m *Model, msgs ...tea.KeyMsg) *Model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		if mm, ok := next.(*Model); ok {
			m = mm
		}
	}
	return m
}

// tabKey is the completion key.
func tabKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyTab} }

// escKey dismisses a popup (and otherwise returns the transcript to the bottom).
func escKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEsc} }

// upKey and downKey move the popup's selection.
func upKey() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyUp} }
func downKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyDown} }

// popupValues is the popup's candidate values, for a compact assertion.
func popupValues(m *Model) []string {
	if m.suggest == nil {
		return nil
	}
	out := make([]string, 0, len(m.suggest.cands))
	for _, c := range m.suggest.cands {
		out = append(out, c.Value)
	}
	return out
}

// ---------- opening ----------

func TestPopupOpensOnAnAtSign(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@"))

	if m.suggest == nil {
		t.Fatal("typing @ did not open the popup")
	}
	if got := popupValues(m); len(got) != 4 {
		t.Fatalf("a bare @ offered %v, want the four reference kinds", got)
	}
	// The input line kept the keystroke: the popup never owns the typing.
	if m.input != "@" || m.inputCursor != 1 {
		t.Fatalf("input = %q caret = %d, want @ at 1", m.input, m.inputCursor)
	}
}

func TestPopupListsWorkspacePathsAfterTheKeyword(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file ma"))

	if m.suggest == nil {
		t.Fatalf("no popup for @file ma (input %q)", m.input)
	}
	if len(m.suggest.cands) != 1 || m.suggest.cands[0].Value != "main.go" {
		t.Fatalf("candidates = %v, want main.go", popupValues(m))
	}
	// The token span covers the whole word, so accepting replaces all of it.
	buf := []rune(m.input)
	if got := string(buf[m.suggest.start:m.suggest.end]); got != "ma" {
		t.Fatalf("token span = %q, want ma", got)
	}
}

func TestPopupDoesNotOpenForAnOrdinaryTask(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("refactor the auth layer"))
	if m.suggest != nil {
		t.Fatalf("a plain task opened the popup: %v", popupValues(m))
	}
	if m.input != "refactor the auth layer" {
		t.Fatalf("input = %q", m.input)
	}
}

// ---------- selection ----------

func TestPopupSelectionMovesAndClamps(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@"))
	if m.suggest == nil || len(m.suggest.cands) < 4 {
		t.Fatalf("popup = %+v", m.suggest)
	}
	rows := m.suggestWindow()

	m = press(m, downKey(), downKey())
	if m.suggest.sel != 2 {
		t.Fatalf("two downs put the selection at %d, want 2", m.suggest.sel)
	}
	for i := 0; i < 20; i++ {
		m = press(m, downKey())
	}
	if m.suggest.sel != len(m.suggest.cands)-1 {
		t.Fatalf("down clamped at %d, want %d", m.suggest.sel, len(m.suggest.cands)-1)
	}
	for i := 0; i < 20; i++ {
		m = press(m, upKey())
	}
	if m.suggest.sel != 0 {
		t.Fatalf("up clamped at %d, want 0", m.suggest.sel)
	}
	// The window followed the selection, and never left the list.
	if m.suggest.scroll < 0 || m.suggest.scroll > m.suggest.maxScrollFor(rows) {
		t.Fatalf("scroll = %d outside [0, %d]", m.suggest.scroll, m.suggest.maxScrollFor(rows))
	}
	// Selecting is not typing: the buffer and the caret never moved.
	if m.input != "@" || m.inputCursor != 1 {
		t.Fatalf("the arrows moved the buffer: %q @ %d", m.input, m.inputCursor)
	}
}

func TestPopupScrollKeysStayInsideTheWindow(t *testing.T) {
	m := suggestModel(t, 100, 30)
	// A wide workspace so the list is longer than the window.
	ws := m.app.Workspace
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(ws, "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m = press(m, keyRunes("@file "))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	rows := m.suggestWindow()
	if m.suggest.maxScrollFor(rows) == 0 {
		t.Fatalf("the test needs a scrollable list: %d candidates, %d rows", len(m.suggest.cands), rows)
	}
	for i := 0; i < 40; i++ {
		m = press(m, tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if m.suggest.scroll != m.suggest.maxScrollFor(rows) {
		t.Fatalf("PgDn clamped at %d, want %d", m.suggest.scroll, m.suggest.maxScrollFor(rows))
	}
	if m.suggest.sel >= m.suggest.scroll+rows {
		t.Fatalf("the selection %d left the window [%d, %d)", m.suggest.sel, m.suggest.scroll, m.suggest.scroll+rows)
	}
	for i := 0; i < 40; i++ {
		m = press(m, tea.KeyMsg{Type: tea.KeyPgUp})
	}
	if m.suggest.scroll != 0 {
		t.Fatalf("PgUp clamped at %d, want 0", m.suggest.scroll)
	}
}

// ---------- accepting ----------

func TestTabAcceptsTheSelectedCandidate(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file ma"))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	m = press(m, tabKey())

	if m.input != "@file main.go " {
		t.Fatalf("tab produced %q, want the completed token", m.input)
	}
	if m.inputCursor != len([]rune(m.input)) {
		t.Fatalf("caret at %d, want the end (%d)", m.inputCursor, len([]rune(m.input)))
	}
}

func TestTabCompletesAKeywordThenOpensThePathList(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@fi"))
	if m.suggest == nil || m.suggest.cands[0].Value != "@file" {
		t.Fatalf("candidates = %v", popupValues(m))
	}
	m = press(m, tabKey())

	if m.input != "@file " {
		t.Fatalf("tab produced %q, want %q", m.input, "@file ")
	}
	// Accepting a keyword opens the next step rather than ending the popup:
	// the operator asked for a file, so the files are what they want next.
	if m.suggest == nil {
		t.Fatal("accepting the keyword did not open the path list")
	}
	for _, v := range popupValues(m) {
		if !strings.Contains(v, "/") && !strings.HasSuffix(v, ".go") && !strings.HasSuffix(v, ".md") {
			t.Fatalf("unexpected candidate %q", v)
		}
	}
}

func TestAcceptingADirectoryKeepsNavigating(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file internal/"))
	if m.suggest == nil {
		t.Fatal("no popup for the directory prefix")
	}
	// The directory itself is a candidate, with the trailing slash.
	found := false
	for _, v := range popupValues(m) {
		if v == "internal/tui/" {
			found = true
		}
	}
	if !found {
		t.Fatalf("candidates = %v, want the nested directory", popupValues(m))
	}
}

func TestAcceptSpansTheWholeTokenEvenFromTheMiddle(t *testing.T) {
	m := suggestModel(t, 100, 30)
	// The caret sits after "ma" inside "main.go": accepting must replace the
	// whole word, not splice into it.
	m = press(m, keyRunes("@file main.go"))
	m.inputCursor = len([]rune("@file ma"))
	m.refreshSuggest()
	if m.suggest == nil {
		t.Fatal("no popup with the caret inside the token")
	}
	m = press(m, tabKey())
	if m.input != "@file main.go " {
		t.Fatalf("accept from mid-token produced %q", m.input)
	}
}

func TestTabIsInertWhenThereIsNothingToComplete(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("a plain task"))
	m = press(m, tabKey())
	if m.input != "a plain task" {
		t.Fatalf("tab changed a plain task to %q", m.input)
	}
	if m.suggest != nil {
		t.Fatalf("tab opened a popup for a plain task: %v", popupValues(m))
	}
}

func TestTabIndentsAMultiLineDraftWithNoCompletion(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, pasteMsg("first line\nsecond"))
	m = press(m, tabKey())
	if m.input != "first line\nsecond  " {
		t.Fatalf("tab on a multi-line draft produced %q", m.input)
	}
}

// ---------- dismissing ----------

func TestEscDismissesThePopup(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@"))
	if m.suggest == nil {
		t.Fatal("no popup to dismiss")
	}
	m = press(m, escKey())
	if m.suggest != nil {
		t.Fatal("esc did not dismiss the popup")
	}
	// esc did not touch the buffer, and esc still means "to the bottom" for the
	// transcript: the popup consumed only its own key.
	if m.input != "@" {
		t.Fatalf("esc changed the buffer to %q", m.input)
	}
	// It stays dismissed while the operator keeps typing in the same token.
	m = press(m, keyRunes("ma"))
	if m.suggest != nil {
		t.Fatalf("the dismissal did not stick: %v", popupValues(m))
	}
	// A new token is a new decision, so the popup may come back. Ending the token
	// with a space means the dismissal no longer holds — it is scoped to the
	// token it was made in — and the next "@" starts a fresh token with its own
	// candidates.
	m = press(m, keyRunes(" "))
	if m.dismissalHolds() {
		t.Fatalf("ending the token did not release the dismissal: %+v", m.suggestDismissed)
	}
	m = press(m, keyRunes("@"))
	if m.suggest == nil {
		t.Fatal("a new token did not reopen the popup")
	}
}

// ---------- refreshing ----------

func TestTypingWhileOpenRefreshesAndKeepsFocus(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file "))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	before := len(m.suggest.cands)

	m = press(m, keyRunes("ma"))
	if m.suggest == nil {
		t.Fatalf("the popup closed while typing into it (input %q)", m.input)
	}
	if got := popupValues(m); len(got) != 1 || got[0] != "main.go" {
		t.Fatalf("the popup did not refresh: %v", popupValues(m))
	}
	if len(m.suggest.cands) >= before {
		t.Fatalf("the candidate list did not narrow: %d -> %d", before, len(m.suggest.cands))
	}
	// The keystrokes went to the input line, not to the popup.
	if m.input != "@file ma" || m.inputCursor != len([]rune("@file ma")) {
		t.Fatalf("input = %q caret = %d", m.input, m.inputCursor)
	}
	// The selection stayed put rather than snapping back to the top.
	m = press(m, tabKey())
	if m.input != "@file main.go " {
		t.Fatalf("accept after a refresh produced %q", m.input)
	}
}

func TestBackspaceRefreshesThePopup(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file main.go"))
	// Fully typed: the only candidate is the token itself, so the refresh is a
	// no-op list. Backspacing reopens the field.
	m = press(m, tea.KeyMsg{Type: tea.KeyBackspace}, tea.KeyMsg{Type: tea.KeyBackspace})
	if m.suggest == nil {
		t.Fatalf("backspace did not refresh the popup (input %q)", m.input)
	}
	if got := popupValues(m); m.input != "@file main." || len(got) != 1 || got[0] != "main.go" {
		t.Fatalf("input %q candidates %v", m.input, got)
	}
}

func TestSubmittingClosesThePopup(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file ma"))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	// ctrl+u clears the draft, and the popup goes with it.
	m = press(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.suggest != nil {
		t.Fatalf("the popup survived an emptied buffer: %v", popupValues(m))
	}
}

// ---------- slash commands ----------

func TestPopupOffersSlashCommandsWithHints(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("/rev"))
	if m.suggest == nil {
		t.Fatal("no popup for /rev")
	}
	if len(m.suggest.cands) != 1 {
		t.Fatalf("candidates = %v", popupValues(m))
	}
	c := m.suggest.cands[0]
	if c.Value != "/review" || c.Hint != "<path>" {
		t.Fatalf("candidate = %+v, want /review with its argument hint", c)
	}
}

func TestEnterOnAnExactCommandSubmitsInsteadOfCompleting(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("/commands"))
	// The candidate is exactly the token, so enter must mean "send" — not
	// "complete", which would only append a space.
	m = press(m, keyEnter())
	if m.input != "" {
		t.Fatalf("enter left %q in the buffer", m.input)
	}
	if len(m.blocks) < 2 {
		t.Fatal("enter did not run /commands")
	}
	if m.suggest != nil {
		t.Fatalf("the popup survived the submit: %v", popupValues(m))
	}
}

// ---------- rendering ----------

func TestPopupRendersInsideTheTranscriptPanelAndNeverBelowTheFooter(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {140, 40}} {
		m := suggestModel(t, size[0], size[1])
		m = press(m, keyRunes("@file "))
		if m.suggest == nil {
			t.Fatalf("%dx%d: no popup", size[0], size[1])
		}
		assertFits(t, m)

		out := m.View()
		lines := strings.Split(out, "\n")
		// The footer's last row is the input/hint row (the session stamp was
		// folded away, M13); the popup must be above it, not spilling past the
		// frame where fitScreen would silently drop it.
		if strings.Contains(lines[len(lines)-1], "accept") {
			t.Fatalf("%dx%d: the popup legend is on the last row, below the footer:\n%s", size[0], size[1], lines[len(lines)-1])
		}
		if !strings.Contains(out, "accept") {
			t.Fatalf("%dx%d: the popup's key legend is missing:\n%s", size[0], size[1], out)
		}
		if !strings.Contains(out, "internal/") {
			t.Fatalf("%dx%d: the popup's candidates are missing:\n%s", size[0], size[1], out)
		}
		// The popup rows live inside the transcript panel: every row that holds
		// the counter also carries the panel's frame.
		for _, line := range lines {
			if strings.Contains(line, "accept") && !strings.Contains(line, "│") {
				t.Fatalf("%dx%d: the popup row escaped the panel frame: %q", size[0], size[1], line)
			}
		}
	}
}

func TestPopupAtANarrowTerminalStaysInsideItsFrame(t *testing.T) {
	m := suggestModel(t, 40, 20)
	m = press(m, keyRunes("@file internal/"))
	assertFits(t, m)
	out := m.View()
	// The transcript panel's title is a single space since the M13 slim-down,
	// so the sanity check is the popup itself being on screen.
	if !strings.Contains(out, "accept") {
		t.Fatalf("the popup is not on screen:\n%s", out)
	}
	// Every row is exactly the terminal width: a popup row one cell over would
	// soft-wrap and push the frame out of the measured height.
	for i, line := range strings.Split(out, "\n") {
		if w := len([]rune(line)); w > m.w {
			t.Fatalf("row %d is %d cells, terminal is %d: %q", i, w, m.w, line)
		}
	}
}

func TestPopupRowsLeaveATranscriptRowVisible(t *testing.T) {
	m := suggestModel(t, 100, 24)
	m = press(m, keyRunes("@file "))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	rows := m.suggestRows()
	if rows < 2 {
		t.Fatalf("suggestRows = %d", rows)
	}
	if rows > m.transcriptHeight()-1 {
		t.Fatalf("the popup takes %d of %d transcript rows: no transcript left", rows, m.transcriptHeight())
	}
	// The popup never renders more rows than it declared, so the splice always
	// fits the panel interior.
	if got := len(m.suggestLines(m.w - 2)); got > rows {
		t.Fatalf("suggestLines drew %d rows, declared %d", got, rows)
	}
}

func TestNoPopupRowsWithoutCandidates(t *testing.T) {
	m := suggestModel(t, 100, 24)
	if got := m.suggestLines(m.w - 2); len(got) != 0 {
		t.Fatalf("an empty popup drew %v", got)
	}
	// Nor when the transcript panel is too small to hold one.
	tiny := suggestModel(t, 100, 9)
	tiny = press(tiny, keyRunes("@file "))
	if tiny.suggest != nil && tiny.suggestRows() > tiny.transcriptHeight()-1 {
		t.Fatalf("the popup declared more rows than the panel has")
	}
	assertFits(t, tiny)
}

// ---------- the key contract ----------

func TestPopupConsumesOnlyItsOwnKeys(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file "))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	// ctrl+a is not the popup's: it reaches the input line's caret.
	m = press(m, tea.KeyMsg{Type: tea.KeyCtrlA})
	if m.inputCursor != 0 {
		t.Fatalf("ctrl+a did not reach the input line: caret %d", m.inputCursor)
	}
	// A printable key types.
	m = press(m, keyRunes("m"))
	if m.input != "m@file " {
		t.Fatalf("a printable key did not reach the buffer: %q", m.input)
	}
	// left/right move the caret rather than the selection. The "m" was typed at
	// the caret, which was at offset 0, so it sits at offset 1; right moves to 2.
	m = press(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.inputCursor != 2 {
		t.Fatalf("right did not move the caret: %d", m.inputCursor)
	}
}

func TestTypingAtTwiceKeepsBothDismissalAndReopenHonest(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@"))
	m = press(m, escKey())
	m = press(m, tabKey()) // tab is the deliberate way back in
	if m.suggest == nil {
		t.Fatal("tab did not reopen a dismissed popup")
	}
}

func TestPopupIsNotOpenWhileAModalOwnsTheKeyboard(t *testing.T) {
	m := suggestModel(t, 100, 30)
	m = press(m, keyRunes("@file "))
	if m.suggest == nil {
		t.Fatal("no popup")
	}
	// An approval request outranks the popup: the popup guard sits after the
	// modal guards.
	m.approval = &approvalReq{tool: "bash", detail: "rm -rf build", reply: make(chan bool, 1)}
	m.refreshSuggest()
	if m.suggest != nil {
		t.Fatalf("the popup stayed open behind a modal: %v", popupValues(m))
	}
	assertFits(t, m)
}

// teaMsg pins the bubbletea import: a bracketed paste arrives as one tea.KeyMsg,
// which is why the popup's refresh hook is in the message switch rather than in
// a per-rune handler.
var _ = tea.KeyMsg{}
