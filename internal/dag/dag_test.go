package dag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kintsugi/internal/gate"
)

type fakeRunner struct {
	attempts  int
	feedbacks []string
	outputs   []string
	fail      map[int]error
}

func (f *fakeRunner) RunNode(_ context.Context, _ Node, feedback string) (string, error) {
	f.attempts++
	f.feedbacks = append(f.feedbacks, feedback)
	if err := f.fail[f.attempts]; err != nil {
		return "", err
	}
	if f.attempts <= len(f.outputs) {
		return f.outputs[f.attempts-1], nil
	}
	return "output", nil
}

func TestPlanReviewGateJudgesThePlanNotTheDiff(t *testing.T) {
	var seenByKind = map[string]string{}
	e := &Engine{
		Runner:       &fakeRunner{},
		Diff:         func(context.Context) (string, error) { return "THE DIFF", nil },
		PlanArtifact: "THE PLAN",
		GateFn: func(_ context.Context, spec gate.Spec, artifact string) gate.Result {
			seenByKind[spec.Kind] = artifact
			return gate.Result{Kind: spec.Kind, Verdict: "pass", Passed: true}
		},
	}
	if _, err := e.Run(context.Background(), Plan{ID: "p", Nodes: []Node{{
		ID: "build", Task: "build it",
		Gates: []gate.Spec{
			{Kind: "opsec-review", Blocking: true},
			{Kind: "plan-review", Blocking: true},
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if got := seenByKind["opsec-review"]; got != "THE DIFF" {
		t.Fatalf("opsec-review judged %q, want the diff", got)
	}
	if got := seenByKind["plan-review"]; got != "THE PLAN" {
		t.Fatalf("plan-review judged %q, want the plan: a plan gate asked about the proposal, not the change", got)
	}
}

func TestPlanReviewGateFallsBackToTheDiffWithNoPlan(t *testing.T) {
	var seen string
	e := &Engine{
		Runner: &fakeRunner{},
		Diff:   func(context.Context) (string, error) { return "THE DIFF", nil },
		GateFn: func(_ context.Context, spec gate.Spec, artifact string) gate.Result {
			seen = artifact
			return gate.Result{Kind: spec.Kind, Verdict: "pass", Passed: true}
		},
	}
	if _, err := e.Run(context.Background(), Plan{ID: "p", Nodes: []Node{{
		ID: "build", Task: "build it",
		Gates: []gate.Spec{{Kind: "plan-review", Blocking: true}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if seen != "THE DIFF" {
		t.Fatalf("with no plan the gate got %q; it must not silently pass", seen)
	}
}

func TestBlockingGateSeedsReviseThenDone(t *testing.T) {
	run := &fakeRunner{outputs: []string{"bad diff", "fixed diff"}}
	e := &Engine{
		Runner: run,
		GateFn: func(_ context.Context, spec gate.Spec, _ string) gate.Result {
			if run.attempts == 1 {
				return gate.Result{Kind: spec.Kind, Verdict: "revise", Passed: false,
					Findings: []gate.Finding{{Severity: "high", File: "a.go", Line: 3, Consequence: "outside scope"}}}
			}
			return gate.Result{Kind: spec.Kind, Verdict: "pass", Passed: true}
		},
		MaxRevisions: 1,
	}
	report, err := e.Run(context.Background(), Plan{ID: "p", Nodes: []Node{{
		ID: "build", Task: "build it",
		Gates: []gate.Spec{{Kind: "opsec-review", Blocking: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Done() {
		t.Fatalf("report not done: %+v", report)
	}
	if run.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", run.attempts)
	}
	if !strings.Contains(run.feedbacks[1], "outside scope") {
		t.Fatalf("revise feedback missing findings: %q", run.feedbacks[1])
	}
	if report.Nodes[0].Revisions != 1 {
		t.Fatalf("revisions = %d", report.Nodes[0].Revisions)
	}
}

func TestBlockingGatePreventsDone(t *testing.T) {
	run := &fakeRunner{}
	e := &Engine{
		Runner: run,
		GateFn: func(context.Context, gate.Spec, string) gate.Result {
			return gate.Result{Verdict: "block", Passed: false,
				Findings: []gate.Finding{{Severity: "high", File: "x", Line: 1, Consequence: "leak"}}}
		},
		MaxRevisions: 1,
	}
	report, err := e.Run(context.Background(), Plan{ID: "p", Nodes: []Node{{
		ID: "build", Gates: []gate.Spec{{Kind: "opsec-review", Blocking: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Done() {
		t.Fatal("blocked plan reported done")
	}
	if report.Nodes[0].State != "blocked" {
		t.Fatalf("state = %q, want blocked", report.Nodes[0].State)
	}
	if run.attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (initial + one revise)", run.attempts)
	}
}

func TestAdvisoryGateDoesNotBlock(t *testing.T) {
	run := &fakeRunner{}
	e := &Engine{
		Runner: run,
		GateFn: func(context.Context, gate.Spec, string) gate.Result {
			return gate.Result{Verdict: "revise", Passed: false}
		},
	}
	report, err := e.Run(context.Background(), Plan{ID: "p", Nodes: []Node{{
		ID: "build", Gates: []gate.Spec{{Kind: "opsec-review", Blocking: false}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Done() || run.attempts != 1 {
		t.Fatalf("advisory gate blocked: %+v attempts=%d", report, run.attempts)
	}
}

func TestDependenciesAndValidation(t *testing.T) {
	order := []string{}
	e := &Engine{Runner: runnerFunc(func(_ context.Context, n Node, _ string) (string, error) {
		order = append(order, n.ID)
		return "ok", nil
	})}
	report, err := e.Run(context.Background(), Plan{ID: "p", Nodes: []Node{
		{ID: "a", Task: "first"},
		{ID: "b", DependsOn: []string{"a"}, Task: "second"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Done() || strings.Join(order, ",") != "a,b" {
		t.Fatalf("order = %v, report = %+v", order, report)
	}

	if _, err := e.Run(context.Background(), Plan{}); err == nil {
		t.Fatal("empty plan accepted")
	}
	if _, err := e.Run(context.Background(), Plan{Nodes: []Node{{ID: "a", DependsOn: []string{"missing"}}}}); err == nil {
		t.Fatal("unknown dependency accepted")
	}
	if _, err := e.Run(context.Background(), Plan{Nodes: []Node{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}}}); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestRunnerFailureStopsNode(t *testing.T) {
	e := &Engine{Runner: &fakeRunner{fail: map[int]error{1: errors.New("boom")}}}
	report, err := e.Run(context.Background(), Plan{Nodes: []Node{{ID: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Nodes[0].State != "failed" || !strings.Contains(report.Nodes[0].Output, "boom") {
		t.Fatalf("state = %+v", report.Nodes[0])
	}
}

type runnerFunc func(context.Context, Node, string) (string, error)

func (f runnerFunc) RunNode(ctx context.Context, n Node, feedback string) (string, error) {
	return f(ctx, n, feedback)
}
