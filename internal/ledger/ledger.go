// Package ledger is the Context Ledger (M8.5.1): the append-only, redacted,
// human-readable record of positive, operator-accepted work in a workspace.
//
// Trust boundary (IDEAS I-94, DESIGN §4): the ledger is context, never policy.
// It records what was accepted — plans, artifacts, decisions, acceptances —
// together with the evidence that was accepted, so a fresh session can resume
// prior work instead of re-litigating it.
//
// The load-bearing invariant is that a refusal is never memory. There is no
// refusal kind, no refusal text field, and no code path that writes refusal
// prose to ledger.md or to the derived index. The only trace an interception
// may leave is a non-recallable `drift` metadata event (task hash, matched
// ids, class, outcome). Search never returns drift entries.
//
// Storage: `.kintsugi/ledger/ledger.md` is the source of truth (one section
// per entry, see renderEntry). `.kintsugi/ledger/index.sqlite` is derived
// from it and rebuildable at any time with Rebuild.
package ledger

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"kintsugi/internal/redact"
)

// Recallable kinds: positive, operator-accepted work only.
const (
	KindPlan       = "plan"
	KindArtifact   = "artifact"
	KindDecision   = "decision"
	KindAcceptance = "acceptance"
)

// KindDrift is a metadata-only continuity event (M8.5.3). It is recorded so
// drift can be counted, and it is never recallable: Search does not return it,
// the index does not contain it, and it carries no prose.
const KindDrift = "drift"

// Refused kinds. These constants exist so callers and tests can prove that
// Append says no; nothing in this package ever writes them.
const (
	// KindRefusal is not, and never becomes, a ledger kind. A refusal is
	// telemetry (internal/refusal, session events), never memory.
	KindRefusal = "refusal"
	// KindAuthorization and KindScope are operator-only context classes
	// (internal/context). The harness proposes; it never writes authorization
	// or scope, so they can never enter the ledger.
	KindAuthorization = "authorization"
	KindScope         = "scope"
)

// Operator attestation levels for an entry.
const (
	OperatorAttested = "attested" // operator explicitly accepted
	OperatorImplicit = "implicit" // accepted by continued operator use
	OperatorAuto     = "auto"     // curator proposal auto-applied (M8.5.6)
)

// Entry status.
const (
	StatusActive     = "active"
	StatusSuperseded = "superseded"
)

// Recall/search defaults. The roadmap caps prior-work injection at top-5 and
// ~600 tokens (M8.5.3), so those are the defaults here.
const (
	DefaultTopK      = 5
	DefaultMaxTokens = 600
)

// Evidence is one accepted file, with the content hash it had when accepted.
type Evidence struct {
	Path string `json:"path"`
	Hash string `json:"hash,omitempty"`
}

// Entry is one ledger section. Empty fields are omitted from the file.
type Entry struct {
	ID         string     `json:"id"`
	TS         string     `json:"ts"`
	Kind       string     `json:"kind"`
	Session    string     `json:"session,omitempty"`
	Model      string     `json:"model,omitempty"`
	Persona    string     `json:"persona,omitempty"`
	Task       string     `json:"task,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	Evidence   []Evidence `json:"evidence,omitempty"`
	Operator   string     `json:"operator,omitempty"` // attested | implicit | auto
	Status     string     `json:"status,omitempty"`   // active | superseded
	Supersedes string     `json:"supersedes,omitempty"`

	// drift metadata only — never free text.
	TaskHash   string   `json:"task_hash,omitempty"`
	MatchedIDs []string `json:"matched_ids,omitempty"`
	Class      string   `json:"class,omitempty"`
	Outcome    string   `json:"outcome,omitempty"`
}

// Store is one workspace's ledger. It owns the Markdown source of truth, the
// derived FTS5 index, and the parsed in-memory view used for List/Supersede.
type Store struct {
	// Workspace is the absolute project root.
	Workspace string

	db      *sql.DB
	mu      sync.Mutex
	entries []Entry // file order (chronological)
}

// timeNow is a seam for tests.
var timeNow = time.Now

// Dir is <workspace>/.kintsugi/ledger.
func Dir(workspace string) string { return filepath.Join(workspace, ".kintsugi", "ledger") }

// MarkdownPath is the append-only source of truth.
func MarkdownPath(workspace string) string { return filepath.Join(Dir(workspace), "ledger.md") }

// IndexPath is the derived SQLite/FTS5 index.
func IndexPath(workspace string) string { return filepath.Join(Dir(workspace), "index.sqlite") }

// header is the fixed preamble of ledger.md. It is regenerated on every full
// rewrite, so parsing only needs to skip it.
const header = `# Ledger

Accepted, operator-visible work for this workspace (M8.5.1). Append-only:
entries are never deleted, only superseded. This file holds accepted plans,
artifacts, decisions, and acceptances plus non-recallable drift metadata; it
never holds refusals. The ledger is context, never policy.
`

// entryHeadingRe matches a section heading. It is deliberately strict so that
// prose inside a summary can never be mistaken for the start of an entry.
var entryHeadingRe = regexp.MustCompile(`^## (L-[0-9]{8}-[0-9]{3,}) · (plan|artifact|decision|acceptance|drift)$`)

var taskHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Open opens (creating if needed) the ledger for a workspace. The derived index
// is synchronised with ledger.md on open, so an externally edited or missing
// index self-heals.
func Open(workspace string) (*Store, error) {
	if err := os.MkdirAll(Dir(workspace), 0o755); err != nil {
		return nil, err
	}
	db, err := openIndex(IndexPath(workspace))
	if err != nil {
		return nil, err
	}
	s := &Store{Workspace: workspace, db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := s.Rebuild(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the index. The Markdown source of truth is already durable.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Append normalises, redacts, validates, assigns ID/TS, appends one section to
// ledger.md and folds the entry into the index. Validation happens before any
// write, so a rejected kind leaves nothing on disk.
func (s *Store) Append(e Entry) (Entry, error) {
	clean, err := normalize(e)
	if err != nil {
		return Entry{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := timeNow().UTC()
	clean.TS = now.Format(time.RFC3339)
	clean.ID = s.nextID(now)
	clean.Status = StatusActive

	block, err := renderEntry(clean)
	if err != nil {
		return Entry{}, err
	}
	if err := s.appendBlock(block); err != nil {
		return Entry{}, err
	}
	s.entries = append(s.entries, clean)

	if indexable(clean) {
		if err := s.insertEntry(clean); err != nil {
			// The file is the source of truth and already holds the entry;
			// Rebuild repairs the index. Report the failure anyway.
			return clean, fmt.Errorf("ledger: entry %s written but not indexed: %w", clean.ID, err)
		}
	}
	return clean, nil
}

// List returns entries in file order (chronological). An empty kind returns
// every entry, superseded and drift included: List is the administrative view,
// Search is the recall view.
func (s *Store) List(kind string) ([]Entry, error) {
	want := strings.ToLower(strings.TrimSpace(kind))
	if want != "" && !knownKind(want) {
		return nil, fmt.Errorf("ledger: unknown kind %q (want plan|artifact|decision|acceptance|drift)", kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if want != "" && e.Kind != want {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Supersede marks id as superseded by byID. Both entries must exist; history is
// kept (the old section stays in the file with status: superseded) and the file
// is rewritten atomically. The index is rebuilt so the old entry stops being
// recallable and the new one becomes recallable.
func (s *Store) Supersede(id, byID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == byID {
		return fmt.Errorf("ledger: an entry cannot supersede itself")
	}
	oldIdx, ok := s.find(id)
	if !ok {
		return fmt.Errorf("ledger: no entry %q", id)
	}
	newIdx, ok := s.find(byID)
	if !ok {
		return fmt.Errorf("ledger: no entry %q", byID)
	}
	if s.entries[oldIdx].Status == StatusSuperseded {
		return fmt.Errorf("ledger: %s is already superseded", id)
	}

	s.entries[oldIdx].Status = StatusSuperseded
	if s.entries[newIdx].Supersedes == "" {
		s.entries[newIdx].Supersedes = id
	}

	if err := s.writeAll(); err != nil {
		return err
	}
	_, err := s.reindexAll()
	return err
}

// Rebuild re-reads ledger.md and rebuilds the derived index from scratch. It
// returns the number of recallable, active entries indexed.
func (s *Store) Rebuild() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := load(s.Workspace)
	if err != nil {
		return 0, err
	}
	s.entries = entries
	return s.reindexAll()
}

// ExportJSONL writes one JSON object per entry, in file order (chronological),
// including superseded and drift entries: it is an export, not a recall.
func (s *Store) ExportJSONL(w io.Writer) error {
	s.mu.Lock()
	entries := make([]Entry, len(s.entries))
	copy(entries, s.entries)
	s.mu.Unlock()

	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// HashTask returns the sha256 hex used for drift task hashes. Hashing, not
// quoting, is how a task that ran into drift is recorded: the text itself is
// never stored.
func HashTask(task string) string {
	sum := sha256.Sum256([]byte(task))
	return hex.EncodeToString(sum[:])
}

// HashBytes is the content hash recorded in Evidence.Hash.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- validation ------------------------------------------------------------

// normalize validates and redacts an entry before it can touch disk.
func normalize(e Entry) (Entry, error) {
	out := e
	out.Kind = strings.ToLower(strings.TrimSpace(e.Kind))

	switch out.Kind {
	case KindRefusal:
		return Entry{}, fmt.Errorf("ledger: %q is not a ledger kind: a refusal is never memory", out.Kind)
	case KindAuthorization, KindScope:
		return Entry{}, fmt.Errorf("ledger: %q is operator-only context, not ledger material", out.Kind)
	}
	if !knownKind(out.Kind) {
		return Entry{}, fmt.Errorf("ledger: unknown kind %q (want plan|artifact|decision|acceptance|drift)", e.Kind)
	}

	out.Task = redact.Redact(strings.TrimSpace(e.Task))
	out.Summary = redact.Redact(strings.TrimSpace(e.Summary))

	out.Session = strings.TrimSpace(e.Session)
	out.Model = strings.TrimSpace(e.Model)
	out.Persona = strings.TrimSpace(e.Persona)
	out.Supersedes = strings.TrimSpace(e.Supersedes)

	// Operator status defaults to implicit; anything else must be known.
	out.Operator = strings.ToLower(strings.TrimSpace(e.Operator))
	if out.Operator == "" {
		out.Operator = OperatorImplicit
	}
	switch out.Operator {
	case OperatorAttested, OperatorImplicit, OperatorAuto:
	default:
		return Entry{}, fmt.Errorf("ledger: unknown operator status %q (want attested|implicit|auto)", e.Operator)
	}

	// Append always writes an active entry; supersession is a separate,
	// history-preserving operation.
	if st := strings.TrimSpace(e.Status); st != "" && st != StatusActive {
		return Entry{}, fmt.Errorf("ledger: Append writes active entries only (got status %q); use Supersede", e.Status)
	}

	// Evidence paths are redacted too: a path can carry a token.
	out.Evidence = nil
	for _, ev := range e.Evidence {
		p := redact.Redact(strings.TrimSpace(ev.Path))
		if p == "" {
			continue
		}
		out.Evidence = append(out.Evidence, Evidence{Path: p, Hash: strings.TrimSpace(ev.Hash)})
	}

	if out.Kind == KindDrift {
		// A drift event is metadata only. Refusal prose must never reach the
		// file, so carrying text here is an error rather than a silent drop.
		if out.Task != "" {
			return Entry{}, fmt.Errorf("ledger: drift entries carry metadata only: task text is not allowed")
		}
		if out.Summary != "" {
			return Entry{}, fmt.Errorf("ledger: drift entries carry metadata only: summary text is not allowed")
		}
		out.TaskHash = strings.TrimSpace(e.TaskHash)
		if out.TaskHash != "" && !taskHashRe.MatchString(out.TaskHash) {
			return Entry{}, fmt.Errorf("ledger: drift task_hash must be a sha256 hex digest")
		}
		out.MatchedIDs = nil
		for _, id := range e.MatchedIDs {
			if id = strings.TrimSpace(id); id != "" {
				out.MatchedIDs = append(out.MatchedIDs, id)
			}
		}
		out.Class = strings.TrimSpace(e.Class)
		out.Outcome = strings.TrimSpace(e.Outcome)
		if out.TaskHash == "" && len(out.MatchedIDs) == 0 {
			return Entry{}, fmt.Errorf("ledger: drift entries need a task_hash or matched ids")
		}
		return out, nil
	}

	// Recallable kinds: drift-only metadata fields would blur the line between
	// a metadata event and memory, so they are rejected rather than dropped.
	if out.TaskHash != "" || len(e.MatchedIDs) > 0 || strings.TrimSpace(e.Class) != "" || strings.TrimSpace(e.Outcome) != "" {
		return Entry{}, fmt.Errorf("ledger: task_hash/matched_ids/class/outcome are drift-only fields")
	}

	if out.Task == "" && out.Summary == "" {
		return Entry{}, fmt.Errorf("ledger: an entry needs a task or a summary")
	}
	if err := checkProse("task", out.Task); err != nil {
		return Entry{}, err
	}
	if err := checkProse("summary", out.Summary); err != nil {
		return Entry{}, err
	}
	return out, nil
}

// checkProse rejects text that would break the file grammar: it must not open a
// code fence and it must not look like an entry heading.
func checkProse(field, v string) error {
	for _, line := range strings.Split(v, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			return fmt.Errorf("ledger: %s must not contain a code fence", field)
		}
		if entryHeadingRe.MatchString(line) {
			return fmt.Errorf("ledger: %s must not contain a ledger entry heading", field)
		}
	}
	return nil
}

func knownKind(k string) bool {
	switch k {
	case KindPlan, KindArtifact, KindDecision, KindAcceptance, KindDrift:
		return true
	}
	return false
}

// recallable is the gate for Search: positive kinds, active only.
func recallable(e Entry) bool {
	if e.Status != StatusActive {
		return false
	}
	switch e.Kind {
	case KindPlan, KindArtifact, KindDecision, KindAcceptance:
		return true
	}
	return false
}

// indexable is the gate for the derived index: only recallable entries exist
// there, so drift and superseded rows cannot leak into any future query.
func indexable(e Entry) bool { return recallable(e) }

// ---- file format -----------------------------------------------------------

// front is the YAML metadata block of one section. Field order is the file
// format, so it must not be reshuffled casually.
type front struct {
	ID         string     `yaml:"id"`
	TS         string     `yaml:"ts"`
	Kind       string     `yaml:"kind"`
	Session    string     `yaml:"session,omitempty"`
	Model      string     `yaml:"model,omitempty"`
	Persona    string     `yaml:"persona,omitempty"`
	Operator   string     `yaml:"operator,omitempty"`
	Status     string     `yaml:"status,omitempty"`
	Supersedes string     `yaml:"supersedes,omitempty"`
	Task       string     `yaml:"task,omitempty"`
	Evidence   []evidence `yaml:"evidence,omitempty"`
	TaskHash   string     `yaml:"task_hash,omitempty"`
	MatchedIDs []string   `yaml:"matched_ids,omitempty"`
	Class      string     `yaml:"class,omitempty"`
	Outcome    string     `yaml:"outcome,omitempty"`
}

type evidence struct {
	Path string `yaml:"path"`
	Hash string `yaml:"hash,omitempty"`
}

func toFront(e Entry) front {
	f := front{
		ID:         e.ID,
		TS:         e.TS,
		Kind:       e.Kind,
		Session:    e.Session,
		Model:      e.Model,
		Persona:    e.Persona,
		Operator:   e.Operator,
		Status:     e.Status,
		Supersedes: e.Supersedes,
		Task:       e.Task,
		TaskHash:   e.TaskHash,
		MatchedIDs: e.MatchedIDs,
		Class:      e.Class,
		Outcome:    e.Outcome,
	}
	for _, ev := range e.Evidence {
		f.Evidence = append(f.Evidence, evidence{Path: ev.Path, Hash: ev.Hash})
	}
	return f
}

func (f front) entry() Entry {
	e := Entry{
		ID:         f.ID,
		TS:         f.TS,
		Kind:       f.Kind,
		Session:    f.Session,
		Model:      f.Model,
		Persona:    f.Persona,
		Operator:   f.Operator,
		Status:     f.Status,
		Supersedes: f.Supersedes,
		Task:       f.Task,
		TaskHash:   f.TaskHash,
		MatchedIDs: f.MatchedIDs,
		Class:      f.Class,
		Outcome:    f.Outcome,
	}
	for _, ev := range f.Evidence {
		e.Evidence = append(e.Evidence, Evidence{Path: ev.Path, Hash: ev.Hash})
	}
	return e
}

// renderEntry emits one section:
//
//	## L-20261021-001 · decision
//
//	```yaml
//	id: L-20261021-001
//	...
//	```
//
//	<summary paragraph>
func renderEntry(e Entry) (string, error) {
	yb, err := yaml.Marshal(toFront(e))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s · %s\n\n", e.ID, e.Kind)
	b.WriteString("```yaml\n")
	b.Write(yb)
	b.WriteString("```\n")
	if e.Summary != "" {
		b.WriteString("\n")
		b.WriteString(e.Summary)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// renderFile is the canonical whole-file rendering. Parse(renderFile(entries))
// must be the identity, which is what makes rewrites safe.
func renderFile(entries []Entry) (string, error) {
	var b strings.Builder
	b.WriteString(header)
	for _, e := range entries {
		block, err := renderEntry(e)
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		b.WriteString(block)
	}
	return b.String(), nil
}

// Parse reads ledger.md. File order is preserved (chronological).
func Parse(data []byte) ([]Entry, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var entries []Entry

	for i := 0; i < len(lines); {
		m := entryHeadingRe.FindStringSubmatch(lines[i])
		if m == nil {
			i++ // header prose / blank line between sections
			continue
		}
		headID, headKind := m[1], m[2]
		i++

		var yamlLines, bodyLines []string
		inFence, closed := false, false
		for i < len(lines) && entryHeadingRe.FindStringSubmatch(lines[i]) == nil {
			l := lines[i]
			switch {
			case !closed && !inFence:
				switch t := strings.TrimSpace(l); {
				case t == "":
				case strings.HasPrefix(t, "```yaml"):
					inFence = true
				default:
					return nil, fmt.Errorf("ledger: entry %s: expected a yaml fence, got %q", headID, l)
				}
			case !closed && inFence:
				if strings.HasPrefix(strings.TrimSpace(l), "```") {
					inFence, closed = false, true
				} else {
					yamlLines = append(yamlLines, l)
				}
			default:
				bodyLines = append(bodyLines, l)
			}
			i++
		}
		if !closed {
			return nil, fmt.Errorf("ledger: entry %s: unterminated yaml fence", headID)
		}

		var f front
		if err := yaml.Unmarshal([]byte(strings.Join(yamlLines, "\n")), &f); err != nil {
			return nil, fmt.Errorf("ledger: entry %s: %w", headID, err)
		}
		e := f.entry()
		if e.ID != headID {
			return nil, fmt.Errorf("ledger: entry %s: metadata says id %q", headID, e.ID)
		}
		if e.Kind != headKind {
			return nil, fmt.Errorf("ledger: entry %s: heading says %q, metadata says %q", headID, headKind, e.Kind)
		}
		e.Summary = strings.TrimSpace(strings.Join(bodyLines, "\n"))
		entries = append(entries, e)
	}
	return entries, nil
}

// load parses the workspace ledger; a missing file is an empty ledger.
func load(workspace string) ([]Entry, error) {
	b, err := os.ReadFile(MarkdownPath(workspace))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// appendBlock appends one rendered section, writing the header first if the
// file does not exist yet.
func (s *Store) appendBlock(block string) error {
	p := MarkdownPath(s.Workspace)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(p); err != nil || st.Size() == 0 {
		if err := writeFileAtomic(p, header); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, "\n"+block); err != nil {
		return err
	}
	return f.Sync()
}

// writeAll rewrites ledger.md atomically from the in-memory entries.
func (s *Store) writeAll() error {
	body, err := renderFile(s.entries)
	if err != nil {
		return err
	}
	return writeFileAtomic(MarkdownPath(s.Workspace), body)
}

func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nextID assigns L-YYYYMMDD-NNN, a per-day counter derived from existing ids
// (so the sequence survives a restart without extra state).
func (s *Store) nextID(now time.Time) string {
	prefix := "L-" + now.Format("20060102") + "-"
	max := 0
	for _, e := range s.entries {
		if !strings.HasPrefix(e.ID, prefix) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(e.ID[len(prefix):], "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s%03d", prefix, max+1)
}

// find returns the index of an entry in file order.
func (s *Store) find(id string) (int, bool) {
	for i := range s.entries {
		if s.entries[i].ID == id {
			return i, true
		}
	}
	return 0, false
}
