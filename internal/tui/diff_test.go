package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/provider"
	"kintsugi/internal/tools"
)

func TestUnifiedDiffProducesRemovalsAndAdditions(t *testing.T) {
	before := []string{"package main", "", "func a() {}", "func b() {}"}
	after := []string{"package main", "", "func a() {}", "func c() {}"}
	hunks := unifiedDiff(before, after)
	if len(hunks) == 0 {
		t.Fatal("no hunks for a change that should produce one")
	}
	var flat []diffLine
	for _, h := range hunks {
		flat = append(flat, h.lines...)
	}
	// The change is b → c: exactly one removal and one addition, with the
	// surrounding context the hunk wraps it in.
	removals, additions := 0, 0
	for _, l := range flat {
		switch l.kind {
		case '-':
			removals++
			if l.text != "func b() {}" {
				t.Errorf("removal text %q, want %q", l.text, "func b() {}")
			}
		case '+':
			additions++
			if l.text != "func c() {}" {
				t.Errorf("addition text %q, want %q", l.text, "func c() {}")
			}
		}
	}
	if removals != 1 || additions != 1 {
		t.Errorf("got %d removals, %d additions; want 1, 1 (flat: %+v)", removals, additions, flat)
	}
}

func TestUnifiedDiffAddOnlyFileIsAllAdditions(t *testing.T) {
	after := []string{"package main", "", "func main() {}"}
	hunks := unifiedDiff(nil, after)
	var flat []diffLine
	for _, h := range hunks {
		flat = append(flat, h.lines...)
	}
	if len(flat) != len(after) {
		t.Fatalf("add-only diff has %d lines, want %d", len(flat), len(after))
	}
	for _, l := range flat {
		if l.kind != '+' {
			t.Errorf("add-only diff has a non-addition line %+v", l)
		}
	}
}

func TestGroupHunksSeparatesDistantChanges(t *testing.T) {
	// Two changes far apart (separated by >2 context lines) become two hunks.
	flat := []diffLine{
		{kind: '-', text: "old1"},
		{kind: '+', text: "new1"},
		{kind: ' ', text: "ctx"},
		{kind: ' ', text: "ctx"},
		{kind: ' ', text: "ctx"},
		{kind: ' ', text: "ctx"},
		{kind: '-', text: "old2"},
		{kind: '+', text: "new2"},
	}
	hunks := groupHunks(flat)
	if len(hunks) != 2 {
		t.Errorf("distant changes produced %d hunks, want 2", len(hunks))
	}
}

func TestDiffLinesRendersHeaderAndCappedDiff(t *testing.T) {
	m := testModel(80, 24)
	// A small edit: one line replaced.
	before := "package main\n\nfunc a() {}\n"
	after := "package main\n\nfunc a() {}\nfunc b() {}\n"
	fc := tools.FileChange{Path: "main.go", Kind: "edit", Before: before, After: after}
	lines := m.diffLines(fc, 80)
	joined := strings.Join(lines, "\n")
	// Header names the path, the kind label, and the line delta (3 → 4).
	for _, want := range []string{"main.go", "edit", "3", "4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff header missing %q:\n%s", want, joined)
		}
	}
	// The addition appears with a + prefix.
	if !strings.Contains(joined, "func b() {}") {
		t.Errorf("diff body missing the added line:\n%s", joined)
	}
	// The file grew, so the header tone is Ochre (additions outweigh) — in mono
	// that is plain text, but the header line is present either way.
	if len(lines) < 2 {
		t.Errorf("diff produced %d lines, want at least a header and body", len(lines))
	}
}

func TestDiffLinesCapsLargeChanges(t *testing.T) {
	m := testModel(80, 24)
	// A change whose grouped diff exceeds diffMaxLines: the cap names how many
	// were elided so the operator knows there is more in the session log.
	// 30 additions + the one context line groupHunks keeps = 31 grouped lines,
	// which exceeds the 24-line cap by 7.
	beforeLines := make([]string, 0, 5)
	afterLines := make([]string, 0, 35)
	for i := 0; i < 5; i++ {
		beforeLines = append(beforeLines, "line "+itoa(i))
		afterLines = append(afterLines, "line "+itoa(i))
	}
	for i := 0; i < 30; i++ {
		afterLines = append(afterLines, "added "+itoa(i))
	}
	fc := tools.FileChange{
		Path:   "big.go",
		Kind:   "edit",
		Before: strings.Join(beforeLines, "\n"),
		After:  strings.Join(afterLines, "\n"),
	}
	lines := m.diffLines(fc, 80)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "more line(s) in the session log") {
		t.Errorf("large diff did not report elided lines:\n%s", joined)
	}
}

func TestDiffLinesDeleteShowsHeaderOnly(t *testing.T) {
	m := testModel(80, 24)
	fc := tools.FileChange{Path: "gone.go", Kind: "patch-delete", Before: "package main\nfunc main() {}\n", After: ""}
	lines := m.diffLines(fc, 80)
	if len(lines) != 1 {
		t.Errorf("delete produced %d lines, want a header only", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "gone.go") || !strings.Contains(joined, "deleted") || !strings.Contains(joined, "removed") {
		t.Errorf("delete header missing path/label/delta:\n%s", joined)
	}
}

func TestDiffLinesTooLargeFallsBackToSummary(t *testing.T) {
	m := testModel(80, 24)
	big := make([]string, diffLineCap+5)
	for i := range big {
		big[i] = "line"
	}
	fc := tools.FileChange{Path: "huge.go", Kind: "edit", Before: strings.Join(big, "\n"), After: "changed\n"}
	lines := m.diffLines(fc, 80)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "too large to diff inline") {
		t.Errorf("oversized diff did not fall back to a summary:\n%s", joined)
	}
}

// itoa is a local int→string to keep the test free of strconv imports it does
// not otherwise need.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestApprovalCardRendersTheDiff pins the informed approval on the TUI side:
// the pending approval card carries the proposed change, so [y]/[n] is
// answered with the diff in front of the operator.
func TestApprovalCardRendersTheDiff(t *testing.T) {
	m := testModel(80, 24)
	m.approval = &approvalReq{
		tool:    "edit",
		detail:  "README.md  hello → hi",
		changes: []tools.FileChange{{Path: "README.md", Kind: "edit", Before: "hello\nworld\n", After: "hi\nworld\n"}},
		reply:   make(chan bool, 1),
	}
	out := plain(m.View())
	for _, want := range []string{"// approval · pending", "[y] approve", "[n] deny", "README.md", "edit"} {
		if !strings.Contains(out, want) {
			t.Errorf("the approval card missing %q:\n%s", want, out)
		}
	}
	// The diff body: the removed and added lines are visible in the card.
	if !strings.Contains(out, "- hello") || !strings.Contains(out, "+ hi") {
		t.Errorf("the approval card does not show the change:\n%s", out)
	}
	assertFits(t, m)
}

// TestChangesViewAccumulatesAndSupersedes pins the /changes contract: every
// file-authoring tool result folds into the change set, the latest state per
// path wins, and the view renders the aggregate and the diffs.
func TestChangesViewAccumulatesAndSupersedes(t *testing.T) {
	m := testModel(100, 34)

	// A first edit to one file.
	m.foldChanges([]tools.FileChange{{Path: "a.go", Kind: "edit", Before: "x\n", After: "y\n"}})
	// A second edit to the same file: supersede, touch count grows.
	m.foldChanges([]tools.FileChange{{Path: "a.go", Kind: "edit", Before: "y\n", After: "z\n"}})
	// A different file.
	m.foldChanges([]tools.FileChange{{Path: "b.go", Kind: "write-new", Before: "", After: "package b\n"}})

	if len(m.changeSet) != 2 {
		t.Fatalf("change set holds %d paths, want 2", len(m.changeSet))
	}
	if got := m.changeSet["a.go"].change.After; got != "z\n" {
		t.Fatalf("a.go latest = %q, want z\\n (superseded)", got)
	}
	if m.changeSet["a.go"].touches != 2 {
		t.Fatalf("a.go touches = %d, want 2", m.changeSet["a.go"].touches)
	}

	m.openChanges()
	out := plain(m.View())
	// The latest state per path: a.go shows y → z (the superseded first edit
	// is history, in the transcript — the view answers "what is the current
	// proposed state").
	for _, want := range []string{"// changes · this session", "2 file(s)", "a.go", "b.go",
		"- y", "+ z", "touched 2 times"} {
		if !strings.Contains(out, want) {
			t.Errorf("changes view missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- x") {
		t.Errorf("the superseded edit leaked into the view:\n%s", out)
	}
	assertFits(t, m)
}

// TestChangesViewEmptyStateNamesTheNextAction: an empty session shows what
// fills the view, not a blank panel.
func TestChangesViewEmptyStateNamesTheNextAction(t *testing.T) {
	m := testModel(80, 24)
	m.openChanges()
	out := plain(m.View())
	if !strings.Contains(out, "nothing changed yet") {
		t.Errorf("the empty state is wrong:\n%s", out)
	}
}

// TestChangesViewClosesWithEsc: the modal owns the keyboard and gives it back.
func TestChangesViewClosesWithEsc(t *testing.T) {
	m := testModel(80, 24)
	m.foldChanges([]tools.FileChange{{Path: "a.go", Kind: "edit", Before: "x\n", After: "y\n"}})
	m.openChanges()
	pressKeys(m, keyEsc())
	if m.changes != nil {
		t.Fatal("esc did not close the changes view")
	}
	// The change set survives the view closing: the facts are session state.
	if len(m.changeSet) != 1 {
		t.Fatal("closing the view dropped the change set")
	}
}

// TestToolNoticeFoldsIntoChanges: the live path — a tool notice arriving
// through Update lands in the change set.
func TestToolNoticeFoldsIntoChanges(t *testing.T) {
	m := testModel(100, 30)
	m.ch = make(chan tea.Msg, 1)
	_, _ = m.Update(toolNotice{
		call: provider.ToolCall{Type: "function", Function: provider.Function{Name: "edit", Arguments: `{"path":"x.go"}`}},
		res: tools.Result{
			Content: `{"ok":true}`,
			Diff:    []tools.FileChange{{Path: "x.go", Kind: "edit", Before: "a\n", After: "b\n"}},
		},
	})
	if len(m.changeSet) != 1 {
		t.Fatalf("the tool notice did not fold into the change set: %d", len(m.changeSet))
	}
}
