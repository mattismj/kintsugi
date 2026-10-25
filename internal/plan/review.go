package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Operator verdicts on a plan. The empty verdict means nobody has decided yet —
// an unreviewed plan, which is a state the surface must be able to show.
const (
	// VerdictApprove is an explicit operator decision to implement the plan.
	VerdictApprove = "approve"
	// VerdictReject is an explicit decision not to.
	VerdictReject = "reject"
)

// Comment kinds are the quick-comment verbs: the operator's one-word reason for
// touching a line. An empty kind is a plain note.
const (
	// CommentWhy asks why a step exists.
	CommentWhy = "why"
	// CommentCut marks a step to remove.
	CommentCut = "cut"
	// CommentRisky marks a step that worries the operator.
	CommentRisky = "risky"
)

// Anchor states report whether a comment still points at the line it was
// written on. A plan is edited between rounds, so a stored line number alone
// would drift onto a step nobody commented on.
const (
	// AnchorOK means the line number still carries the commented text.
	AnchorOK = "ok"
	// AnchorMoved means the text was found elsewhere in the document.
	AnchorMoved = "moved"
	// AnchorGone means the text is no longer in the document.
	AnchorGone = "gone"
)

// Review is one plan's review history. Rounds are append-only: each round is a
// snapshot of the plan plus what was said about it, so a later round can never
// rewrite what an earlier one reviewed.
type Review struct {
	Plan     string  `json:"plan"`              // slug
	Rounds   []Round `json:"rounds"`            // oldest first
	Verdict  string  `json:"verdict,omitempty"` // "" | approve | reject
	Note     string  `json:"note,omitempty"`    // why the verdict was reached
	Operator string  `json:"operator,omitempty"`
	Updated  string  `json:"updated,omitempty"` // RFC3339
}

// Round is one review pass over one version of a plan.
type Round struct {
	N        int       `json:"n"`                  // 1-based
	At       string    `json:"at"`                 // RFC3339
	Snapshot string    `json:"snapshot"`           // path of the version this round reviewed
	Comments []Comment `json:"comments,omitempty"` // operator comments
	// Findings is what the reviewer returned, kept verbatim as it was parsed so
	// a later round can show what was already said. The gate package owns the
	// meaning of these fields; this package only stores them.
	Findings    []StoredFinding `json:"findings,omitempty"`
	NotAFinding []string        `json:"not_a_finding,omitempty"`
	GateVerdict string          `json:"gate_verdict,omitempty"` // pass | revise | block
}

// StoredFinding is a gate finding as it was recorded. It mirrors the gate
// package's finding shape without importing it: a stored review must stay
// readable even if the reviewer's severity vocabulary changes.
type StoredFinding struct {
	Severity    string `json:"severity"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Consequence string `json:"consequence,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
}

// Comment is a note anchored to a line of the plan as it was in that round. The
// line text is stored with it so the comment survives an edit above it: if the
// line at Line no longer carries Text, the anchor reports moved or gone rather
// than silently pointing at the wrong step.
type Comment struct {
	Line int    `json:"line"` // 1-based line in the round's snapshot
	Text string `json:"text"` // the plan line it was attached to
	Body string `json:"body"` // the operator's note
	Kind string `json:"kind,omitempty"`
	At   string `json:"at,omitempty"`
}

// ReviewPath is where a plan's review record lives. It sits beside the plan so
// an operator can read the pair without a database, and so it is workspace
// scoped: two engagements never share a review history.
func ReviewPath(workspace, slug string) string {
	return filepath.Join(Dir(workspace), slug+".review.json")
}

// VersionsDir is where the per-round snapshots live for one plan.
func VersionsDir(workspace, slug string) string {
	return filepath.Join(Dir(workspace), "versions", slug)
}

// SnapshotPath is the file one round reviewed.
func SnapshotPath(workspace, slug string, n int) string {
	return filepath.Join(VersionsDir(workspace, slug), fmt.Sprintf("%d.md", n))
}

// LoadReview reads a plan's review record. A plan that has never been reviewed
// is not an error: it returns an empty record bound to that slug, which is the
// unreviewed state the surface renders.
func LoadReview(workspace, slug string) (*Review, error) {
	slug = Slug(slug)
	if slug == "" {
		return nil, fmt.Errorf("plan: %q is not a plan name", slug)
	}
	raw, err := os.ReadFile(ReviewPath(workspace, slug))
	if err != nil {
		if os.IsNotExist(err) {
			return &Review{Plan: slug}, nil
		}
		return nil, fmt.Errorf("plan: %w", err)
	}
	var r Review
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("plan: %s is not a review record: %w", ReviewPath(workspace, slug), err)
	}
	// The file name is the authority on which plan this is: a record copied to
	// the wrong slug must not silently adopt the slug in its body.
	r.Plan = slug
	r.normalize()
	return &r, nil
}

// SaveReview writes a review record atomically.
func SaveReview(workspace string, r *Review) error {
	if r == nil {
		return fmt.Errorf("plan: no review to save")
	}
	r.Plan = Slug(r.Plan)
	if r.Plan == "" {
		return fmt.Errorf("plan: review has no plan slug")
	}
	r.normalize()
	r.Updated = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if err := os.MkdirAll(Dir(workspace), 0o755); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	return writeAtomic(ReviewPath(workspace, r.Plan), append(raw, '\n'))
}

// Current is the newest round, or nil when the plan has never been reviewed.
func (r *Review) Current() *Round {
	if r == nil || len(r.Rounds) == 0 {
		return nil
	}
	return &r.Rounds[len(r.Rounds)-1]
}

// Decided reports whether an operator has approved or rejected the plan.
func (r *Review) Decided() bool {
	return r != nil && (r.Verdict == VerdictApprove || r.Verdict == VerdictReject)
}

// EnsureRound returns the current round, opening one only when this plan
// version has not been reviewed yet. A round is a review of a plan *version*,
// so re-running the reviewer on unchanged text refreshes the round it already
// has rather than superseding it — which matters because superseding would
// orphan the comments the operator just wrote against that version.
func EnsureRound(workspace string, art Artifact) (*Review, *Round, error) {
	slug := Slug(art.Name)
	if slug == "" {
		return nil, nil, fmt.Errorf("plan: %q has no usable plan name", art.Name)
	}
	rev, err := LoadReview(workspace, slug)
	if err != nil {
		return nil, nil, err
	}
	if cur := rev.Current(); cur != nil {
		// Same version: the snapshot it recorded is byte-identical to the plan.
		if raw, err := os.ReadFile(cur.Snapshot); err == nil && string(raw) == art.Content {
			return rev, cur, nil
		}
	}
	return BeginRound(workspace, art)
}

// BeginRound snapshots the plan as it stands and opens a new round on it. The
// snapshot is written before the round is recorded, so a recorded round always
// has something to show even if the plan is edited a moment later.
func BeginRound(workspace string, art Artifact) (*Review, *Round, error) {
	slug := Slug(art.Name)
	if slug == "" {
		return nil, nil, fmt.Errorf("plan: %q has no usable plan name", art.Name)
	}
	rev, err := LoadReview(workspace, slug)
	if err != nil {
		return nil, nil, err
	}
	n := len(rev.Rounds) + 1
	dir := VersionsDir(workspace, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("plan: %w", err)
	}
	path := SnapshotPath(workspace, slug, n)
	if err := writeAtomic(path, []byte(art.Content)); err != nil {
		return nil, nil, err
	}
	round := Round{
		N:        n,
		At:       time.Now().UTC().Format(time.RFC3339),
		Snapshot: path,
	}
	rev.Rounds = append(rev.Rounds, round)
	// A new round reopens the decision: the plan being reviewed is not the plan
	// that was approved.
	rev.Verdict, rev.Note, rev.Operator = "", "", ""
	if err := SaveReview(workspace, rev); err != nil {
		return nil, nil, err
	}
	return rev, rev.Current(), nil
}

// AddComment appends a comment to the current round. A plan with no round gets
// one first, so a comment can never be orphaned.
func AddComment(workspace string, art Artifact, c Comment) (*Review, error) {
	rev, err := LoadReview(workspace, art.Name)
	if err != nil {
		return nil, err
	}
	if rev.Current() == nil {
		rev, _, err = EnsureRound(workspace, art)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(c.Body) == "" {
		return nil, fmt.Errorf("plan: a comment needs a body")
	}
	c.Body = strings.TrimSpace(c.Body)
	c.Line = max(c.Line, 0)
	c.Text = strings.TrimSpace(c.Text)
	if c.At == "" {
		c.At = time.Now().UTC().Format(time.RFC3339)
	}
	cur := rev.Current()
	cur.Comments = append(cur.Comments, c)
	sort.SliceStable(cur.Comments, func(i, j int) bool { return cur.Comments[i].Line < cur.Comments[j].Line })
	if err := SaveReview(workspace, rev); err != nil {
		return nil, err
	}
	return rev, nil
}

// SetVerdict records the operator's decision. Approving or rejecting without a
// review round is allowed — a plan handed in by someone else may already have
// been read — but the record says so.
func SetVerdict(workspace, slug, verdict, note, operator string) (*Review, error) {
	verdict = strings.ToLower(strings.TrimSpace(verdict))
	if verdict != VerdictApprove && verdict != VerdictReject {
		return nil, fmt.Errorf("plan: %q is not a verdict (want %s|%s)", verdict, VerdictApprove, VerdictReject)
	}
	rev, err := LoadReview(workspace, slug)
	if err != nil {
		return nil, err
	}
	rev.Verdict = verdict
	rev.Note = strings.TrimSpace(note)
	rev.Operator = strings.TrimSpace(operator)
	if err := SaveReview(workspace, rev); err != nil {
		return nil, err
	}
	return rev, nil
}

// CommentAnchor reports where a comment stands against a document. The line
// number is reused when its text still matches; otherwise the document is
// searched for the same text, which is how a comment follows an edit above it.
func CommentAnchor(content string, c Comment) (line int, state string) {
	lines := strings.Split(content, "\n")
	want := strings.TrimSpace(c.Text)
	if want == "" {
		// A comment on a blank line has nothing to match on; the number is the
		// only anchor it will ever have.
		if c.Line >= 1 && c.Line <= len(lines) {
			return c.Line, AnchorOK
		}
		return 0, AnchorGone
	}
	if c.Line >= 1 && c.Line <= len(lines) && strings.TrimSpace(lines[c.Line-1]) == want {
		return c.Line, AnchorOK
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == want {
			return i + 1, AnchorMoved
		}
	}
	return 0, AnchorGone
}

// Diff compares two rounds line by line and returns a unified-ish listing with
// " " for context, "-" for a line only in `from` and "+" for a line only in
// `to`. It is a longest-common-subsequence diff, not a word diff: a plan round
// changes whole steps far more often than words.
func Diff(workspace, slug string, from, to int) ([]string, error) {
	slug = Slug(slug)
	a, err := os.ReadFile(SnapshotPath(workspace, slug, from))
	if err != nil {
		return nil, fmt.Errorf("plan: round %d: %w", from, err)
	}
	b, err := os.ReadFile(SnapshotPath(workspace, slug, to))
	if err != nil {
		return nil, fmt.Errorf("plan: round %d: %w", to, err)
	}
	return diffLines(strings.Split(string(a), "\n"), strings.Split(string(b), "\n")), nil
}

// diffLines is the line diff behind Diff. It keeps the common prefix and suffix
// out of the table so the quadratic part only runs over what actually changed —
// which is what makes a 200-line plan diff cheap.
func diffLines(a, b []string) []string {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []string
	for _, l := range a[:pre] {
		out = append(out, " "+l)
	}
	mid := lcsDiff(a[pre:len(a)-suf], b[pre:len(b)-suf])
	out = append(out, mid...)
	for _, l := range a[len(a)-suf:] {
		out = append(out, " "+l)
	}
	return out
}

// lcsDiff is the quadratic core: the table is bounded by the caller's trimming,
// and a plan body is small enough that anything more elaborate is wasted.
func lcsDiff(a, b []string) []string {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		out := make([]string, 0, m)
		for _, l := range b {
			out = append(out, "+"+l)
		}
		return out
	}
	if m == 0 {
		out := make([]string, 0, n)
		for _, l := range a {
			out = append(out, "-"+l)
		}
		return out
	}
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	out := make([]string, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, " "+a[i])
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			out = append(out, "-"+a[i])
			i++
		default:
			out = append(out, "+"+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "-"+a[i])
	}
	for ; j < m; j++ {
		out = append(out, "+"+b[j])
	}
	return out
}

// normalize drops rounds that carry no state and keeps the numbering dense, so
// a hand-edited record cannot produce a round list the surface cannot count.
func (r *Review) normalize() {
	if len(r.Rounds) == 0 {
		r.Rounds = nil
		return
	}
	for i := range r.Rounds {
		r.Rounds[i].N = i + 1
	}
}
