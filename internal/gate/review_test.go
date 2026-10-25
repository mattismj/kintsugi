package gate

import (
	"strings"
	"testing"
)

func TestParseCodeReview(t *testing.T) {
	input := `{
		"findings": [
			{"severity": "P0", "file": "main.go", "line": 42, "consequence": "nil pointer dereference", "confidence": "high"},
			{"severity": "P2", "file": "util.go", "line": 10, "consequence": "missing error check", "confidence": "medium"}
		],
		"verdict": "no-ship",
		"summary": "critical bug found"
	}`
	rev, err := ParseCodeReview(input)
	if err != nil {
		t.Fatalf("ParseCodeReview error: %v", err)
	}
	if rev.Verdict != "no-ship" {
		t.Errorf("expected verdict 'no-ship', got %q", rev.Verdict)
	}
	if len(rev.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(rev.Findings))
	}
	if rev.Findings[0].Severity != "P0" {
		t.Errorf("expected P0, got %q", rev.Findings[0].Severity)
	}
	if rev.Findings[0].Confidence != "high" {
		t.Errorf("expected high confidence, got %q", rev.Findings[0].Confidence)
	}
	if rev.Summary != "critical bug found" {
		t.Errorf("expected summary, got %q", rev.Summary)
	}
}

func TestParseCodeReviewFenced(t *testing.T) {
	input := "```json\n{\"findings\":[],\"verdict\":\"ship\"}\n```"
	rev, err := ParseCodeReview(input)
	if err != nil {
		t.Fatalf("ParseCodeReview error: %v", err)
	}
	if rev.Verdict != "ship" {
		t.Errorf("expected 'ship', got %q", rev.Verdict)
	}
}

func TestDeriveCodeVerdict(t *testing.T) {
	cases := []struct {
		findings []CodeFinding
		expect   string
	}{
		{nil, "ship"},
		{[]CodeFinding{{Severity: "P2"}}, "ship"},
		{[]CodeFinding{{Severity: "P3"}}, "ship"},
		{[]CodeFinding{{Severity: "P1"}}, "no-ship"},
		{[]CodeFinding{{Severity: "P0"}}, "no-ship"},
		{[]CodeFinding{{Severity: "P2"}, {Severity: "P1"}}, "no-ship"},
	}
	for _, c := range cases {
		got := deriveCodeVerdict(c.findings)
		if got != c.expect {
			t.Errorf("deriveCodeVerdict(%v) = %q, want %q", c.findings, got, c.expect)
		}
	}
}

func TestFormatCodeReview(t *testing.T) {
	rev := &CodeReview{
		Verdict: "no-ship",
		Summary: "issues found",
		Findings: []CodeFinding{
			{Severity: "P0", File: "main.go", Line: 10, Consequence: "crash", Confidence: "high"},
		},
	}
	out := FormatCodeReview(rev)
	if !strings.Contains(out, "no-ship") {
		t.Errorf("expected 'no-ship' in output, got %q", out)
	}
	if !strings.Contains(out, "P0") {
		t.Errorf("expected 'P0' in output, got %q", out)
	}
	if !strings.Contains(out, "main.go:10") {
		t.Errorf("expected 'main.go:10' in output, got %q", out)
	}
}

func TestFormatCodeReviewEmpty(t *testing.T) {
	rev := &CodeReview{Verdict: "ship", Summary: "clean"}
	out := FormatCodeReview(rev)
	if !strings.Contains(out, "no findings") {
		t.Errorf("expected 'no findings' in output, got %q", out)
	}
}
