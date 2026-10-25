package tools

import (
	"strings"
	"testing"
)

func TestClassifyFiles(t *testing.T) {
	files := []string{
		"internal/tools/astgrep.go",
		"internal/tools/astgrep_test.go",
		"README.md",
		"internal/gate/review.go",
		"docs/guide.txt",
		"cmd/kintsugi/main.go",
	}
	groups := classifyFiles(files)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	if groups[0].Label != "source changes" {
		t.Errorf("expected first group to be source, got %q", groups[0].Label)
	}
	if groups[0].Order != 0 {
		t.Errorf("expected source order 0, got %d", groups[0].Order)
	}
	if len(groups[0].Files) != 3 {
		t.Errorf("expected 3 source files, got %d", len(groups[0].Files))
	}
	if groups[1].Label != "test changes" {
		t.Errorf("expected second group to be test, got %q", groups[1].Label)
	}
	if groups[1].Order != 1 {
		t.Errorf("expected test order 1, got %d", groups[1].Order)
	}
	if len(groups[1].Files) != 1 {
		t.Errorf("expected 1 test file, got %d", len(groups[1].Files))
	}
	if groups[2].Label != "documentation changes" {
		t.Errorf("expected third group to be doc, got %q", groups[2].Label)
	}
	if groups[2].Order != 2 {
		t.Errorf("expected doc order 2, got %d", groups[2].Order)
	}
	if len(groups[2].Files) != 2 {
		t.Errorf("expected 2 doc files, got %d", len(groups[2].Files))
	}
}

func TestGenerateMessage(t *testing.T) {
	cases := []struct {
		files  []string
		expect string
	}{
		{[]string{"foo.go"}, "update foo"},
		{[]string{"internal/tools/foo.go"}, "update foo"},
		{[]string{"internal/tools/foo.go", "internal/tools/bar.go"}, "update internal/tools"},
		{[]string{"a.go", "b.go"}, "update root"},
		{[]string{"internal/a.go", "cmd/b.go"}, "update 2 files"},
	}
	for _, c := range cases {
		msg := generateMessage("/ws", c.files)
		if msg != c.expect {
			t.Errorf("generateMessage(%v) = %q, want %q", c.files, msg, c.expect)
		}
	}
}

func TestCommitPlanFormat(t *testing.T) {
	plan := CommitPlan{
		Groups: []CommitGroup{
			{Label: "source", Files: []string{"a.go"}, Message: "update a", Order: 0},
		},
	}
	out := plan.Format()
	if out == "" {
		t.Error("expected non-empty format output")
	}
	if !strings.Contains(out, "source") {
		t.Errorf("expected 'source' in output, got %q", out)
	}
}

func TestCommitPlanEmpty(t *testing.T) {
	plan := CommitPlan{}
	if plan.HasChanges() {
		t.Error("expected HasChanges to be false for empty plan")
	}
	out := plan.Format()
	if out != "no changes to commit" {
		t.Errorf("expected 'no changes to commit', got %q", out)
	}
}
