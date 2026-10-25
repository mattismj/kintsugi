package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savePlan(t *testing.T, ws, name string) Artifact {
	t.Helper()
	art, err := Save(ws, name, "# "+name+"\n\n1. read the config\n2. patch the handler\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return art
}

func TestLoadReviewOnAnUnreviewedPlanIsNotAnError(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")

	rev, err := LoadReview(ws, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	if rev.Plan != art.Name {
		t.Fatalf("Plan = %q, want %q", rev.Plan, art.Name)
	}
	if len(rev.Rounds) != 0 || rev.Decided() {
		t.Fatalf("a fresh plan is not unreviewed: %+v", rev)
	}
}

func TestBeginRoundSnapshotsThePlanEachTime(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")

	rev, r1, err := BeginRound(ws, art)
	if err != nil {
		t.Fatalf("BeginRound: %v", err)
	}
	if r1.N != 1 {
		t.Fatalf("first round = %d, want 1", r1.N)
	}
	snap := SnapshotPath(ws, art.Name, 1)
	if raw, err := os.ReadFile(snap); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	} else if string(raw) != art.Content {
		t.Fatalf("snapshot does not hold the reviewed plan:\n%s", raw)
	}

	// Edit the plan, then open a second round: round 1 must still hold the
	// text it reviewed.
	edited, err := Save(ws, "Add health check", "# Add health check\n\n1. read the config\n2. patch the handler\n3. add a test\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, r2, err := BeginRound(ws, edited); err != nil || r2.N != 2 {
		t.Fatalf("second round = %+v, err %v", r2, err)
	}
	raw, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("round 1 snapshot vanished: %v", err)
	}
	if strings.Contains(string(raw), "add a test") {
		t.Fatalf("round 1 was rewritten by the second round:\n%s", raw)
	}
	if len(rev.Rounds) != 1 {
		t.Fatalf("returned review has %d rounds, want the state at that moment", len(rev.Rounds))
	}
	// The on-disk record has both.
	reloaded, err := LoadReview(ws, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	if len(reloaded.Rounds) != 2 {
		t.Fatalf("stored rounds = %d, want 2", len(reloaded.Rounds))
	}
}

func TestCommentsRoundTripAndAnchor(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")

	lines := strings.Split(art.Content, "\n")
	// Line 4 is "1. read the config" (1 marker, 2 heading, 3 blank).
	target := strings.TrimSpace(lines[3])
	rev, err := AddComment(ws, art, Comment{Line: 4, Text: target, Body: "why not the loader?", Kind: CommentWhy})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if len(rev.Current().Comments) != 1 {
		t.Fatalf("comment not stored: %+v", rev.Current())
	}

	reloaded, err := LoadReview(ws, art.Name)
	if err != nil {
		t.Fatalf("LoadReview: %v", err)
	}
	got := reloaded.Current().Comments[0]
	if got.Body != "why not the loader?" || got.Kind != CommentWhy {
		t.Fatalf("comment round-trip lost data: %+v", got)
	}

	line, state := CommentAnchor(art.Content, got)
	if state != AnchorOK || line != 4 {
		t.Fatalf("anchor = (%d,%s), want (4,ok)", line, state)
	}
}

func TestCommentFollowsAnEditAboveIt(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")
	line := strings.TrimSpace(strings.Split(art.Content, "\n")[3])
	c := Comment{Line: 4, Text: line, Body: "note"}

	// Two lines inserted above push the target down to 6.
	edited := strings.Replace(art.Content, "1. read the config", "0. read the issue\n0b. read the log\n1. read the config", 1)
	n, state := CommentAnchor(edited, c)
	if state != AnchorMoved {
		t.Fatalf("state = %q, want %q", state, AnchorMoved)
	}
	if n != 6 {
		t.Fatalf("comment landed on line %d, want 6", n)
	}
}

func TestCommentOnADeletedLineReportsGone(t *testing.T) {
	c := Comment{Line: 3, Text: "2. patch the handler", Body: "note"}
	_, state := CommentAnchor("step one\nstep two\n", c)
	if state != AnchorGone {
		t.Fatalf("state = %q, want %q", state, AnchorGone)
	}
}

func TestSetVerdictRejectsUnknownValues(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")

	if _, err := SetVerdict(ws, art.Name, "maybe", "", ""); err == nil {
		t.Fatal("an unknown verdict was accepted")
	}
	rev, err := SetVerdict(ws, art.Name, VerdictApprove, "reviewed", "operator")
	if err != nil {
		t.Fatalf("SetVerdict: %v", err)
	}
	if !rev.Decided() || rev.Verdict != VerdictApprove || rev.Note != "reviewed" {
		t.Fatalf("verdict not recorded: %+v", rev)
	}
}

func TestANewRoundReopensTheDecision(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")
	if _, err := SetVerdict(ws, art.Name, VerdictApprove, "", ""); err != nil {
		t.Fatalf("SetVerdict: %v", err)
	}
	rev, _, err := BeginRound(ws, art)
	if err != nil {
		t.Fatalf("BeginRound: %v", err)
	}
	if rev.Decided() {
		t.Fatalf("a new round kept the old approval: %+v", rev)
	}
}

func TestDiffBetweenRounds(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")
	if _, _, err := BeginRound(ws, art); err != nil {
		t.Fatalf("BeginRound: %v", err)
	}
	edited, err := Save(ws, "Add health check", "# Add health check\n\n1. read the config\n2. patch the handler\n3. add a test\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := BeginRound(ws, edited); err != nil {
		t.Fatalf("BeginRound: %v", err)
	}

	out, err := Diff(ws, art.Name, 1, 2)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "+3. add a test") {
		t.Fatalf("diff does not show the added step:\n%s", joined)
	}
	if strings.Contains(joined, "-1. read the config") {
		t.Fatalf("diff dropped a line both rounds share:\n%s", joined)
	}
}

func TestReviewRecordRejectsAMalformedFile(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")
	path := ReviewPath(ws, art.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReview(ws, art.Name); err == nil {
		t.Fatal("a malformed review record was accepted")
	}
}

func TestSaveReviewIsAtomicAndRebindsToTheFileName(t *testing.T) {
	ws := t.TempDir()
	if err := SaveReview(ws, &Review{Plan: "Some Plan", Verdict: VerdictApprove}); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}
	raw, err := os.ReadFile(ReviewPath(ws, "some-plan"))
	if err != nil {
		t.Fatalf("record not written under its slug: %v", err)
	}
	var got Review
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if got.Plan != "some-plan" {
		t.Fatalf("stored slug = %q, want some-plan", got.Plan)
	}
	if got.Updated == "" {
		t.Fatal("record has no timestamp")
	}
}

func TestEnsureRoundRefreshesTheSameVersionInsteadOfSupersedingIt(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")
	if _, err := AddComment(ws, art, Comment{Line: 4, Text: "1. read the config", Body: "why?"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	rev, round, err := EnsureRound(ws, art)
	if err != nil {
		t.Fatalf("EnsureRound: %v", err)
	}
	if round.N != 1 {
		t.Fatalf("round = %d, want 1: reviewing unchanged text must not open a new round", round.N)
	}
	if len(rev.Current().Comments) != 1 {
		t.Fatalf("the comment was orphaned by a review re-run: %+v", rev.Current())
	}
	if len(rev.Rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(rev.Rounds))
	}

	// A changed plan is a new version, so it does get a new round.
	edited, err := Save(ws, "Add health check", "# Add health check\n\n1. read the config\n9. new step\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	rev, round, err = EnsureRound(ws, edited)
	if err != nil {
		t.Fatalf("EnsureRound: %v", err)
	}
	if round.N != 2 {
		t.Fatalf("round = %d, want 2 after the plan changed", round.N)
	}
	if len(rev.Rounds) != 2 {
		t.Fatalf("rounds = %d, want 2", len(rev.Rounds))
	}
}

func TestAddCommentRequiresABody(t *testing.T) {
	ws := t.TempDir()
	art := savePlan(t, ws, "Add health check")
	if _, err := AddComment(ws, art, Comment{Line: 1, Body: "   "}); err == nil {
		t.Fatal("an empty comment was accepted")
	}
}
