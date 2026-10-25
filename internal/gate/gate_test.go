package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

func reviewer(t *testing.T, content string) *Reviewer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req["messages"])
		if strings.Contains(string(raw), "curl http://198.51.100.7") && !strings.Contains(string(raw), "Declared scope") {
			t.Errorf("scope missing from review request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	c, err := provider.New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "gate-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return &Reviewer{Client: c}
}

func TestParseReview(t *testing.T) {
	rev, err := ParseReview("```json\n{\"findings\":[{\"severity\":\"high\",\"file\":\"x.go\",\"line\":9,\"consequence\":\"hardcoded C2 IP\"}],\"not_a_finding\":[\"TLS pinning checked\"],\"verdict\":\"block\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Verdict != "block" || len(rev.Findings) != 1 || rev.Findings[0].Line != 9 {
		t.Fatalf("parsed = %+v", rev)
	}
	if len(rev.NotAFinding) != 1 {
		t.Fatalf("not_a_finding lost: %+v", rev)
	}

	rev, err = ParseReview(`{"findings":[{"severity":"medium","file":"a","consequence":"noisy log"}]}`)
	if err != nil || rev.Verdict != "revise" {
		t.Fatalf("derived verdict = %+v, %v", rev, err)
	}
	if _, err := ParseReview("looks fine to me"); err == nil {
		t.Fatal("prose accepted")
	}
}

func TestEmptyDiffPassesWithoutCall(t *testing.T) {
	// No server; a nil client would panic if called.
	rev, err := (&Reviewer{}).Review(context.Background(), "  ", "lab only")
	if err != nil || rev.Verdict != "pass" {
		t.Fatalf("empty diff = %+v, %v", rev, err)
	}
}

// planReviewer answers with a fixed plan review and records what the reviewer
// was asked to judge.
func planReviewer(t *testing.T, content string, seen *string) *Reviewer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req["messages"])
		if seen != nil {
			*seen = string(raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	c, err := provider.New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "gate-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return &Reviewer{Client: c}
}

// TestPlanScopeReview proves a plan document is reviewed against the declared
// scope: the prompt asks the scope questions, and the verdict keeps the
// finding shape (severity, file:line evidence, not_a_finding list).
func TestPlanScopeReview(t *testing.T) {
	var seen string
	rev := planReviewer(t, `{"findings":[{"severity":"high","file":"plan.md","line":12,"consequence":"plan provisions a public callback host outside the declared scope"}],"not_a_finding":["plan stays inside the lab network"],"verdict":"block"}`, &seen)

	out, err := rev.ReviewPlan(context.Background(), "# Plan\n\n1. build a beacon\n2. call home to 203.0.113.9\n", "lab only: no public infrastructure")
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "block" || len(out.Findings) != 1 {
		t.Fatalf("plan review = %+v", out)
	}
	f := out.Findings[0]
	if f.Severity != "high" || f.File != "plan.md" || f.Line != 12 {
		t.Fatalf("finding lost its shape: %+v", f)
	}
	if len(out.NotAFinding) != 1 || out.NotAFinding[0] != "plan stays inside the lab network" {
		t.Fatalf("not_a_finding = %v", out.NotAFinding)
	}
	for _, want := range []string{
		"does this plan stay inside the declared scope?",
		"does it propose anything the authorization does not cover?",
		"Declared scope:",
		"lab only: no public infrastructure",
		"Plan:",
		"call home to 203.0.113.9",
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("plan review request missing %q:\n%s", want, seen)
		}
	}
}

// TestReviewArtifactKinds proves the artifact kind selects the review: a diff
// and a plan get different prompts, an unknown kind is an error, and an empty
// plan passes without calling the model.
func TestReviewArtifactKinds(t *testing.T) {
	var seen string
	rev := planReviewer(t, `{"findings":[],"not_a_finding":["nothing to flag"],"verdict":"pass"}`, &seen)

	if _, err := rev.ReviewArtifact(context.Background(), Artifact{Kind: "diff", Content: "diff --git a/x b/x"}, "scope"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "Diff:") || strings.Contains(seen, "Plan:") {
		t.Fatalf("diff artifact used the wrong prompt:\n%s", seen)
	}
	if _, err := rev.ReviewArtifact(context.Background(), Artifact{Kind: "plan", Content: "step one"}, "scope"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "Plan:") {
		t.Fatalf("plan artifact used the wrong prompt:\n%s", seen)
	}
	if _, err := rev.ReviewArtifact(context.Background(), Artifact{Kind: "screenshot", Content: "x"}, "scope"); err == nil {
		t.Fatal("unknown artifact kind accepted")
	}

	empty, err := rev.ReviewPlan(context.Background(), "   ", "scope")
	if err != nil || empty.Verdict != "pass" || len(empty.NotAFinding) != 1 {
		t.Fatalf("empty plan = %+v, %v", empty, err)
	}
}

// TestPlanReviewGate proves the plan-review gate kind runs the plan through
// the reviewer and reports the verdict, findings and not_a_finding list.
func TestPlanReviewGate(t *testing.T) {
	rev := planReviewer(t, `{"findings":[{"severity":"medium","file":"plan.md","line":3,"consequence":"plan reaches for a host the authorization does not cover"}],"not_a_finding":["plan writes no logs"],"verdict":"revise"}`, nil)

	res := Run(context.Background(), Spec{Kind: "plan-review", Blocking: true}, rev, t.TempDir(),
		"lab only: no public infrastructure", "# Plan\n\n1. ship it\n")
	if res.Passed || res.Verdict != "revise" || len(res.Findings) != 1 {
		t.Fatalf("plan gate result = %+v", res)
	}
	if len(res.NotAFinding) != 1 || res.NotAFinding[0] != "plan writes no logs" {
		t.Fatalf("not_a_finding = %v", res.NotAFinding)
	}
	if !res.Blocking {
		t.Fatal("blocking flag lost")
	}
	fb := Feedback([]Result{res})
	if !strings.Contains(fb, "plan.md:3") {
		t.Fatalf("feedback = %q", fb)
	}
	none := Run(context.Background(), Spec{Kind: "plan-review"}, nil, t.TempDir(), "", "plan")
	if none.Passed || none.Verdict != "failed" {
		t.Fatalf("plan gate without reviewer = %+v", none)
	}
}

// TestPlanArtifactReadsFile proves the plan artifact carries its provenance.
func TestPlanArtifactReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte("# Plan\n\nstep one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	art, err := PlanArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if art.Kind != ArtifactPlan || art.Path != path || !strings.Contains(art.Content, "step one") {
		t.Fatalf("artifact = %+v", art)
	}
	if _, err := PlanArtifact(filepath.Join(t.TempDir(), "missing.md")); err == nil {
		t.Fatal("missing plan file accepted")
	}
}

func TestOpsecGate(t *testing.T) {
	rev := reviewer(t, `{"findings":[{"severity":"high","file":"agent.py","line":12,"consequence":"hardcoded public callback"}],"not_a_finding":["user-agent generic"],"verdict":"block"}`)
	res := Run(context.Background(), Spec{Kind: "opsec-review"}, rev, t.TempDir(), "lab only: no public infrastructure", "diff --git a/agent.py\n+curl http://198.51.100.7")
	if res.Passed || res.Verdict != "block" || len(res.Findings) != 1 {
		t.Fatalf("opsec result = %+v", res)
	}
	fb := Feedback([]Result{res})
	if !strings.Contains(fb, "agent.py:12") || !strings.Contains(fb, "hardcoded public callback") {
		t.Fatalf("feedback = %q", fb)
	}
}

func TestTestGate(t *testing.T) {
	ws := t.TempDir()
	ok := Run(context.Background(), Spec{Kind: "test-gate", Command: "true"}, nil, ws, "", "")
	if !ok.Passed || ok.Verdict != "pass" {
		t.Fatalf("true gate = %+v", ok)
	}
	bad := Run(context.Background(), Spec{Kind: "test-gate", Command: "echo boom; exit 3", Blocking: true}, nil, ws, "", "")
	if bad.Passed || bad.Verdict != "failed" || !strings.Contains(bad.Output, "boom") || !bad.Blocking {
		t.Fatalf("failing gate = %+v", bad)
	}
	none := Run(context.Background(), Spec{Kind: "test-gate"}, nil, ws, "", "")
	if none.Passed || none.Verdict != "failed" {
		t.Fatalf("empty command gate = %+v", none)
	}
}
