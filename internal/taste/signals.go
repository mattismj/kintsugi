package taste

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/redact"
)

// The signals API (WP M10-B requirement 2): the exported functions the TUI and
// the plain CLI call when an operator decides something.
//
//	approval granted → accept
//	approval denied  → reject
//	operator re-edits a file the agent had just written, within the session → edit
//
// Boundary. An `accept`/`reject` bullet records the operator's decision about
// a tool call — the tool name plus the redacted call detail the operator was
// shown. That is the same material the workspace audit log already keeps
// (internal/audit), not a model's refusal prose: refusal telemetry stays in
// the session log and never becomes memory (the M8.5 invariant). Nothing here
// reads a transcript, a provider response or a session log.
//
// The note is always redacted and bounded before it reaches the file, because
// the bullets are injected back into a prompt: a bullet must not be able to
// carry a credential into the next request.

// SignalNoteBytes bounds a signal's note. It is tighter than MaxNoteBytes
// because a signal's note is machine-derived call detail, not operator prose.
const SignalNoteBytes = 120

// redactNote scrubs secret shapes and flattens the note to one line.
func redactNote(s string) string {
	return flatten(redact.Redact(strings.TrimSpace(s)))
}

// boundNote redacts, flattens and truncates a note to max bytes.
func boundNote(s string, max int) string {
	n := redactNote(s)
	if max > 0 && len(n) > max {
		n = strings.TrimRight(truncateBytes(n, max), " ")
	}
	return n
}

// ---------- approvals ----------

// ApprovalGranted records an `accept` bullet for a tool approval the operator
// granted. It is the one-line hook the TUI's resolveApproval and the plain
// CLI's approver call.
func (s *Store) ApprovalGranted(tool, detail string) (Entry, error) {
	return s.recordSignal(KindAccept, tool, detail)
}

// ApprovalDenied records a `reject` bullet for a tool approval the operator
// denied.
func (s *Store) ApprovalDenied(tool, detail string) (Entry, error) {
	return s.recordSignal(KindReject, tool, detail)
}

// Approval records either outcome. It is the shape the TUI's approver wants:
// one call at the point where the answer is known.
func (s *Store) Approval(tool, detail string, granted bool) (Entry, error) {
	if granted {
		return s.ApprovalGranted(tool, detail)
	}
	return s.ApprovalDenied(tool, detail)
}

// RecordApproval is the package-level convenience the TUI and the plain CLI
// call when they have a workspace and no Config in hand: it resolves the store
// from the config files, so `[taste] enabled = false` is honoured, and records
// one approval outcome.
func RecordApproval(ws, tool, detail string, granted bool) (Entry, error) {
	return StoreFor(ws).Approval(tool, detail, granted)
}

// recordSignal turns one approval outcome into a bullet. The subject is the
// tool name; the note is the redacted call detail the operator was shown.
func (s *Store) recordSignal(kind, tool, detail string) (Entry, error) {
	if !s.Enabled() {
		return Entry{}, nil
	}
	subject := strings.ToLower(strings.TrimSpace(tool))
	if subject == "" {
		subject = "tool"
	}
	note := boundNote(detail, SignalNoteBytes)
	if note == "" {
		note = "no call detail recorded"
	}
	return s.Record(OriginProject, kind, subject, note)
}

// ---------- the edit heuristic ----------

// Tracker is one session's edit heuristic. It is deliberately in-memory and
// session-scoped: the contract is "the operator re-edited a file the agent had
// written *within this session*", so nothing is persisted between sessions and
// a restart forgets every path it was watching.
//
// Contract (documented, per WP M10-B requirement 2):
//
//  1. HarnessWrote(path) — called after an edit/write/patch tool call
//     succeeds. It stamps the path with the moment the harness finished
//     writing it.
//  2. Observed(path, mod, size) — called when something notices the file on
//     disk again (the TUI already sweeps stamps between turns for hot reload;
//     this is the same data). It records an `edit` bullet only when the file
//     was written by the harness in this session and the observed modification
//     is strictly newer than that write.
//  3. At most one `edit` bullet per path per session: the heuristic reports
//     that the operator corrected the agent here, not how many times.
//
// The heuristic is conservative by design. It cannot see who typed into the
// file, so it reports an observation ("this file changed after the agent wrote
// it"), never an accusation. A false positive costs one bullet the operator
// can supersede; a false negative costs nothing.
//
// Wiring (reported, not done — the TUI's Model would need a field for the
// tracker): call HarnessWrote from the executor's PostToolUse path for
// edit/write/patch, and Observed from the TUI's existing stamp sweep. Until
// then no `edit` bullet is produced, and `kintsugi taste list` simply shows the
// accept/reject ones.
type Tracker struct {
	store   *Store
	origin  Origin
	enabled bool

	mu       sync.Mutex
	written  map[string]time.Time
	reported map[string]bool
}

// NewTracker returns a session tracker writing to the project store. A nil or
// disabled store yields a tracker that observes and records nothing.
func NewTracker(s *Store) *Tracker {
	t := &Tracker{store: s, origin: OriginProject}
	if s != nil {
		cfg := s.cfg()
		t.enabled = cfg.On() && cfg.EditsOn()
	}
	return t
}

// Enabled reports whether the tracker will record anything.
func (t *Tracker) Enabled() bool { return t != nil && t.enabled }

// HarnessWrote stamps a path the agent just wrote successfully.
func (t *Tracker) HarnessWrote(path string) {
	if !t.Enabled() {
		return
	}
	path = cleanPath(path)
	if path == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.written == nil {
		t.written = map[string]time.Time{}
	}
	t.written[path] = timeNow()
}

// Observed reports a file's on-disk state and records an `edit` bullet when the
// heuristic fires. It returns the recorded entry and whether one was recorded.
//
// size is accepted so the caller can hand over the stamp it already has (the
// TUI's fileStamp carries modtime and size); the mtime is what decides, so a
// same-size rewrite still counts as a re-edit.
func (t *Tracker) Observed(path string, mod time.Time, size int64) (Entry, bool) {
	if !t.Enabled() {
		return Entry{}, false
	}
	path = cleanPath(path)
	if path == "" {
		return Entry{}, false
	}

	t.mu.Lock()
	wrote, watched := t.written[path]
	if !watched || t.reported[path] || !mod.After(wrote) {
		t.mu.Unlock()
		return Entry{}, false
	}
	if t.reported == nil {
		t.reported = map[string]bool{}
	}
	t.reported[path] = true
	delete(t.written, path)
	t.mu.Unlock()

	e, err := t.store.Record(t.origin, KindEdit, FileClass(path),
		boundNote("operator re-edited "+path+" after the agent wrote it", SignalNoteBytes))
	if err != nil {
		return Entry{}, false
	}
	return e, e.Kind != ""
}

// Watching is how many paths the tracker is holding. It exists so a test (and
// an operator reading a debug line) can see the heuristic is armed.
func (t *Tracker) Watching() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.written)
}

func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// FileClass reduces a path to the short class a taste bullet names: the file
// kind an operator thinks in, not the path itself. It is what makes the
// injected block general ("go", "docs") rather than a list of filenames.
func FileClass(path string) string {
	p := cleanPath(path)
	if p == "" {
		return "file"
	}
	base := filepath.Base(p)
	dir := strings.ToLower(filepath.ToSlash(filepath.Dir(p)))

	switch {
	case strings.HasSuffix(base, "_test.go"):
		return "go tests"
	case strings.HasSuffix(base, ".go"):
		return "go"
	case strings.HasSuffix(base, ".md"):
		if dir == "docs" || strings.HasPrefix(dir, "docs/") {
			return "docs"
		}
		return "markdown"
	case strings.HasSuffix(base, ".toml"), strings.HasSuffix(base, ".yaml"),
		strings.HasSuffix(base, ".yml"), strings.HasSuffix(base, ".json"):
		return "config"
	case strings.HasSuffix(base, ".sh"), strings.HasSuffix(base, ".bash"):
		return "scripts"
	case strings.HasSuffix(base, ".py"):
		return "python"
	case strings.HasSuffix(base, ".ts"), strings.HasSuffix(base, ".tsx"),
		strings.HasSuffix(base, ".js"), strings.HasSuffix(base, ".jsx"):
		return "javascript"
	}
	// No recognised kind: name the top-level directory, which is the coarsest
	// useful class ("internal", "pack", "scripts").
	if dir != "." && dir != "/" && dir != "" {
		head := strings.SplitN(strings.TrimPrefix(dir, "./"), "/", 2)[0]
		if head != "" {
			return head
		}
	}
	return "file"
}
