package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kintsugi/internal/provider"
)

// CodeReview is the output of /review: P0–P3 findings with confidence and a
// ship/no-ship verdict. It reuses the gate findings shape.
type CodeReview struct {
	Findings []CodeFinding `json:"findings"`
	Verdict  string        `json:"verdict"` // ship | no-ship
	Summary  string        `json:"summary,omitempty"`
}

// CodeFinding is one issue found by the reviewer. Severity is P0–P3.
type CodeFinding struct {
	Severity    string `json:"severity"` // P0 | P1 | P2 | P3
	File        string `json:"file"`
	Line        int    `json:"line"`
	Consequence string `json:"consequence"`
	Confidence  string `json:"confidence"` // high | medium | low
	Evidence    string `json:"evidence,omitempty"`
}

// CodeReviewArtifact is what the reviewer is asked to judge.
type CodeReviewArtifact struct {
	Kind    string // diff | branch | working-tree
	Content string
	Ref     string // branch name or commit ref
}

const codeReviewPrompt = `You are a senior code reviewer. Review the supplied diff or code changes.
Look for:
- P0: security vulnerabilities, data loss, crashes, correctness bugs
- P1: performance issues, resource leaks, race conditions
- P2: code quality, maintainability, missing error handling
- P3: style, naming, documentation
For each finding, cite file:line, state the consequence, and rate your confidence (high|medium|low).
Do not invent issues. If the code is clean, say so.
Return ONLY JSON, no prose and no code fences:
{"findings":[{"severity":"P0|P1|P2|P3","file":"path","line":0,"consequence":"...","confidence":"high|medium|low"}],
 "verdict":"ship|no-ship",
 "summary":"one-line summary"}`

// ReviewCode judges a diff, branch, or working tree.
func (r *Reviewer) ReviewCode(ctx context.Context, art CodeReviewArtifact) (*CodeReview, error) {
	content := art.Content
	if strings.TrimSpace(content) == "" {
		return &CodeReview{Verdict: "ship", Summary: "no changes to review"}, nil
	}
	if len(content) > maxReviewBytes {
		content = content[:maxReviewBytes] + "\n…[truncated]"
	}
	msgs := []provider.Message{
		{Role: "system", Content: codeReviewPrompt},
		{Role: "user", Content: content},
	}
	resp, err := r.Client.Stream(ctx, msgs, nil, provider.StreamCallbacks{})
	if err != nil {
		return nil, err
	}
	return ParseCodeReview(resp.Content)
}

// ParseCodeReview accepts strict JSON, fenced JSON, or JSON embedded in prose.
func ParseCodeReview(s string) (*CodeReview, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("review returned no JSON object")
	}
	var rev CodeReview
	if err := json.Unmarshal([]byte(s[start:end+1]), &rev); err != nil {
		return nil, fmt.Errorf("review JSON: %w", err)
	}
	rev.Verdict = strings.ToLower(strings.TrimSpace(rev.Verdict))
	switch rev.Verdict {
	case "ship", "no-ship":
	case "":
		rev.Verdict = deriveCodeVerdict(rev.Findings)
	default:
		rev.Verdict = "no-ship"
	}
	for i := range rev.Findings {
		rev.Findings[i].Severity = strings.ToUpper(strings.TrimSpace(rev.Findings[i].Severity))
		rev.Findings[i].Confidence = strings.ToLower(strings.TrimSpace(rev.Findings[i].Confidence))
	}
	return &rev, nil
}

func deriveCodeVerdict(findings []CodeFinding) string {
	for _, f := range findings {
		if f.Severity == "P0" || f.Severity == "P1" {
			return "no-ship"
		}
	}
	return "ship"
}

// FormatCodeReview renders the review as human-readable text.
func FormatCodeReview(rev *CodeReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "verdict: %s\n", rev.Verdict)
	if rev.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", rev.Summary)
	}
	if len(rev.Findings) == 0 {
		fmt.Fprintln(&b, "no findings")
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "\nfindings (%d):\n", len(rev.Findings))
	for _, f := range rev.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "  [%s/%s] %s — %s\n", f.Severity, f.Confidence, loc, f.Consequence)
		if f.Evidence != "" {
			fmt.Fprintf(&b, "    evidence: %s\n", f.Evidence)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
