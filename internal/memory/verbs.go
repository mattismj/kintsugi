// Memory verbs (WP M10-C, DESIGN §16.17): first-class operations over the
// existing tier-2 store, plus the session mental-model digest.
//
// The verbs are retain (queue a fact), recall (search), reflect (synthesize
// over the bank), memory_edit (update/forget/invalidate by id), learn (promote
// a lesson to a skill) and manage_skill (create/update/delete a skill file).
// They add a write surface to a store that was read-only for the model, so the
// two rules that already govern project memory are reused rather than restated:
//
//  1. Gated classes are operator-only. `authorization` and `scope` are hard
//     gated by internal/context, and the operator's `context.gated_classes`
//     list adds to them. A verb asked to write one does not write: it files a
//     proposal in the curator's own review queue
//     (.kintsugi/proposals, internal/context.File), which `kintsugi context
//     apply` reviews exactly like a curator proposal. There is no second gate
//     and no verb-local notion of "allowed".
//
//  2. A refusal is telemetry, never memory. Every model-supplied text passes
//     the curator's own filter (internal/context.StripRefusalTurns) before it
//     can be written, and every read path (Recall, RecentLessons, Bank, the
//     digest) filters on the way out too. Text that is nothing but a harness
//     refusal marker is rejected outright, so a refused turn cannot become a
//     lesson, a skill, a proposal or a line of the mental model.
//
// Nothing here grants a permission: the verbs are prompt-visible tools, and the
// permission matrix, the sandbox and the declared scope still decide whether a
// call runs at all.

package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/ledger"
)

const (
	// DefaultRecallK is the number of hits a recall returns when the call does
	// not ask for a number. It matches the store's own search default.
	DefaultRecallK = 8
	// MaxRecallK caps one recall, so a verb cannot pull the whole bank into a
	// tool result.
	MaxRecallK = 25

	// DefaultDigestTokens is the session mental-model budget (WP M10-C: a
	// digest of at most 200 tokens).
	DefaultDigestTokens = 200

	// bytesPerToken is the rough estimate the rest of the harness uses
	// (session.ApproxTokens, prompt.Compile, continuity.RenderPriorWork): one
	// token per four bytes.
	bytesPerToken = 4

	// BankLimits bound the synthesis input `reflect` hands to a model.
	BankMaxLessons = 40
	BankMaxTokens  = 1600

	// maxFactBytes caps one retained fact. learned.md is a line-oriented file,
	// so a runaway fact would become a runaway line.
	maxFactBytes = 600
	// digestMaxSummaryBytes keeps one entry from eating the digest.
	digestMaxSummaryBytes = 110
	// digestMaxWork and digestMaxLessons bound the digest's two lists.
	digestMaxWork    = 5
	digestMaxLessons = 3

	// invalidatedFile holds the entries a verb invalidated. They stop being
	// recallable (Reindex reads learned.md and skills/*.md only) but are not
	// destroyed: memory keeps its history the way the ledger keeps its own.
	invalidatedFile = "invalidated.md"
	// invalidatedSkillDir holds invalidated skill files, one level below the
	// skills directory, where neither Reindex nor skills.Load will find them.
	invalidatedSkillDir = "invalidated"
)

// Edit operations accepted by memory_edit.
const (
	OpUpdate     = "update"
	OpForget     = "forget"
	OpInvalidate = "invalidate"
)

// Skill operations accepted by manage_skill.
const (
	SkillCreate = "create"
	SkillUpdate = "update"
	SkillDelete = "delete"
)

// MentalModelLabel is the label the digest carries, and the phrase a caller can
// grep for to tell it apart from the continuity prior-work block.
const MentalModelLabel = "mental model"

// MentalModelHeader titles the digest.
const MentalModelHeader = "## Mental model (session start, local)"

// mentalModelFraming is the fixed qualifier under the header. It says what the
// block is, that it grants nothing, and that a refusal cannot be in it.
const mentalModelFraming = "The session mental model: work this workspace already accepted, plus its newest " +
	"lessons. Local context, not a permission grant; it never widens scope, and an empty digest means " +
	"nothing was recorded, not that anything was refused."

// Tokens is the harness's rough token estimate: one token per four bytes, the
// same estimator session.ApproxTokens and continuity use.
func Tokens(s string) int { return (len(s) + bytesPerToken - 1) / bytesPerToken }

// InvalidatedPath is the file invalidated entries are moved to.
func InvalidatedPath(workspace string) string { return filepath.Join(Dir(workspace), invalidatedFile) }

// ---- the write gate --------------------------------------------------------

// Gate is the write policy the verbs share with the curator. It carries no
// logic of its own beyond "which classes need operator review": the hard-gated
// classes come from internal/context, and Classes is the operator's configured
// list (config.Context.GatedClasses).
type Gate struct {
	// Classes are the operator's configured gated classes. The hard-gated
	// classes (authorization, scope) are gated whatever this holds.
	Classes []string
	// Session labels the proposals a gated verb files. Optional.
	Session string
}

// Gated reports whether class may only be written through the curator's
// proposal queue. `correction` is gated by name because DESIGN §16.17 lists
// corrections with authorization and scope as operator-gated; everything else
// is gated only when the operator says so.
func (g Gate) Gated(class string) bool {
	c := NormalizeClass(class)
	if fctx.HardGated(c) {
		return true
	}
	if c == "correction" || c == "corrections" {
		return true
	}
	for _, x := range g.Classes {
		if NormalizeClass(x) == c {
			return true
		}
	}
	return false
}

// NormalizeClass lowercases a model-supplied class name and maps the obvious
// synonyms onto the context classes. An unknown name is returned lowercased
// rather than collapsed to `other`, so an operator's own gated class still
// matches the gate. An empty name means `lesson`: that is what a retained fact
// is unless the model says otherwise.
func NormalizeClass(s string) string {
	c := strings.ToLower(strings.TrimSpace(s))
	switch c {
	case "", "fact", "facts", "note", "notes", "lesson", "lessons":
		return fctx.ClassLesson
	case "auth", "authorization", "authorisation":
		return fctx.ClassAuthorization
	case "convention", "conventions":
		return fctx.ClassConvention
	case "state", "status":
		return fctx.ClassState
	case "skill", "skills", "procedure":
		return fctx.ClassConvention
	}
	return c
}

// QueueProposal files one proposal in the curator's review queue, reusing the
// curator's own directory and file format so `kintsugi context apply` reviews it
// exactly like a curator proposal. It returns the proposal file's path.
//
// A file created in the same second is appended to rather than overwritten: two
// verbs in one turn queue two proposals, and neither may lose the other.
func QueueProposal(workspace, session string, p fctx.Proposal) (string, error) {
	dir := fctx.ProposalsDir(workspace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	created := timeNow().UTC().Format("2006-01-02T15:04:05Z07:00")
	name := strings.ReplaceAll(created, ":", "") + ".json"
	f := &fctx.File{Created: created, Session: session, Proposals: []fctx.Proposal{p}}
	if old, err := fctx.LoadProposalFile(filepath.Join(dir, name)); err == nil && old != nil {
		f.Created = old.Created
		f.Session = firstNonEmpty(old.Session, session)
		f.Model = old.Model
		f.Proposals = append(old.Proposals, p)
	}
	return f.Save(workspace)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---- refusal policy (reused, not restated) ---------------------------------

// refusalFree applies the curator's refusal filter to model-supplied text. This
// is the whole refusal policy of the verbs: internal/context already decides
// what a harness refusal marker looks like, and the verbs have no second idea
// of their own.
//
// Text that is nothing but refusal telemetry is an error, not a silent drop —
// the model has to hear that a refusal is not something it can store.
func refusalFree(text string) (string, error) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return "", fmt.Errorf("memory: no text to record")
	}
	clean := strings.TrimSpace(fctx.StripRefusalTurns(raw))
	if clean == "" {
		return "", fmt.Errorf("memory: a refusal is telemetry, never memory — nothing was recorded")
	}
	return oneLine(clean), nil
}

// scrub filters a stored text on the way out. learned.md is operator-editable,
// so a refusal marker can be in the file even though no verb would have written
// it; the read path drops it rather than handing it to a model.
func scrub(text string) string {
	return strings.TrimSpace(fctx.StripRefusalTurns(text))
}

// fact prepares one retained fact: refusal-filtered, flattened to a single
// line (learned.md is line-oriented) and capped.
func fact(text string) (string, error) {
	clean, err := refusalFree(text)
	if err != nil {
		return "", err
	}
	return truncateBytes(clean, maxFactBytes), nil
}

// LessonLine renders one learned.md line, the shape AddLesson writes.
func LessonLine(date, text, models, source string) string {
	line := "- [" + date + "] " + strings.TrimSpace(text)
	if m := strings.TrimSpace(models); m != "" {
		line += " _models: [" + m + "]"
	}
	if s := strings.TrimSpace(source); s != "" {
		line += " _source: " + s
	}
	return line
}

// ---- write results ---------------------------------------------------------

// WriteResult is what a mutating verb reports. Exactly one of Written and
// Queued is set on success: the verb either changed the store, or the class was
// gated and the change went to the curator's review queue instead.
type WriteResult struct {
	// Verb is the verb that produced this result.
	Verb string
	// Class is the resolved context class of the write.
	Class string
	// Written is true when the store changed.
	Written bool
	// Queued is true when the write was routed to the proposal gate.
	Queued bool
	// ID is the stable id of the affected entry ("" when queued).
	ID string
	// Path is the file that changed, or the proposal file when queued.
	Path string
	// Op is the operation performed (memory_edit, manage_skill).
	Op string
	// Detail is one operator-readable line saying what happened.
	Detail string
}

// ---- retain ----------------------------------------------------------------

// Retain records one fact. A gated class is never written by the model: it is
// filed as a proposal in the curator's queue and reported as queued, so the
// operator reviews it with `kintsugi context apply`. Anything else becomes a dated
// lesson in learned.md, which Reindex then makes recallable.
func (s *Store) Retain(text, class, models, source string, g Gate) (WriteResult, error) {
	body, err := fact(text)
	if err != nil {
		return WriteResult{}, err
	}
	cls := NormalizeClass(class)
	res := WriteResult{Verb: "retain", Class: cls}

	if g.Gated(cls) {
		path, err := QueueProposal(s.Workspace, g.Session, fctx.Proposal{
			Section:    cls,
			Op:         "append",
			Content:    LessonLine(today(), body, models, source),
			Reason:     "retain: the model asked to record this fact, and the class is operator-gated",
			Confidence: "low",
		})
		if err != nil {
			return WriteResult{}, err
		}
		res.Queued = true
		res.Path = path
		res.Detail = fmt.Sprintf("class %q is operator-gated: queued as a proposal for review, not written to memory", cls)
		return res, nil
	}

	line, err := s.AddLesson(body, strings.TrimSpace(models), strings.TrimSpace(source))
	if err != nil {
		return WriteResult{}, err
	}
	id, err := s.lineID(LearnedPath(s.Workspace), body)
	if err != nil {
		// The lesson is written; only its id could not be resolved. Report the
		// write rather than failing it.
		res.Written = true
		res.Path = rel(s.Workspace, LearnedPath(s.Workspace))
		res.Detail = "recorded as a lesson (id unavailable: " + err.Error() + ")"
		return res, nil
	}
	res.Written = true
	res.ID = id
	res.Path = rel(s.Workspace, LearnedPath(s.Workspace))
	res.Detail = "recorded as a lesson: " + oneLine(line)
	return res, nil
}

// lineID finds the id of the lesson line just written: the last line of
// learned.md whose text matches. learned.md is append-only, so that is the new
// entry.
func (s *Store) lineID(path, text string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		e, ok := ParseLesson(lines[i])
		if ok && e.Text == text {
			return EntryID(rel(s.Workspace, path), i+1), nil
		}
	}
	return "", fmt.Errorf("memory: the new entry was not found in %s", rel(s.Workspace, path))
}

// ---- ids -------------------------------------------------------------------

// EntryID is the stable id of one memory entry: "<workspace-relative path>:<line>"
// for a lesson line, or the bare path for a whole skill file. It is the id
// Format prints as the source, so a recall result already carries it.
func EntryID(path string, line int) string {
	p := filepath.ToSlash(strings.TrimSpace(path))
	if line > 0 {
		return fmt.Sprintf("%s:%d", p, line)
	}
	return p
}

// ID returns the entry's stable id.
func (e Entry) ID() string { return EntryID(e.Path, e.Line) }

// ParseEntryID splits an id into a workspace-relative path and a line number
// (0 for a whole file).
func ParseEntryID(id string) (string, int, error) {
	raw := strings.TrimSpace(id)
	if raw == "" {
		return "", 0, fmt.Errorf("memory: no id given")
	}
	raw = filepath.ToSlash(raw)
	if i := strings.LastIndex(raw, ":"); i > 0 {
		n := 0
		if _, err := fmt.Sscanf(raw[i+1:], "%d", &n); err == nil && n > 0 && strings.TrimSpace(raw[i+1:]) != "" {
			return raw[:i], n, nil
		}
	}
	return raw, 0, nil
}

// target is one resolved id: where it lives and what it is.
type target struct {
	abs   string
	rel   string
	line  int
	skill bool
}

// resolveID maps an id onto a file the verbs are allowed to touch: learned.md
// or a file inside the skills directory. Anything else — a traversal, a path
// outside those two roots — is refused, because an id is model-supplied.
func (s *Store) resolveID(id, skillsDir string) (target, error) {
	p, line, err := ParseEntryID(id)
	if err != nil {
		return target{}, err
	}
	abs := p
	if !filepath.IsAbs(abs) {
		if s.Workspace == "" {
			return target{}, fmt.Errorf("memory: no workspace configured")
		}
		abs = filepath.Join(s.Workspace, filepath.FromSlash(p))
	}
	abs = filepath.Clean(abs)

	learned := filepath.Clean(LearnedPath(s.Workspace))
	if abs == learned {
		if line <= 0 {
			return target{}, fmt.Errorf("memory: %s is the whole lesson file; name a line (path:line)", p)
		}
		return target{abs: abs, rel: rel(s.Workspace, abs), line: line}, nil
	}
	root := strings.TrimSpace(skillsDir)
	if root == "" {
		root = SkillsDir(s.Workspace)
	}
	root = filepath.Clean(root)
	if strings.HasPrefix(abs, root+string(os.PathSeparator)) && strings.EqualFold(filepath.Ext(abs), ".md") {
		return target{abs: abs, rel: rel(s.Workspace, abs), skill: true}, nil
	}
	return target{}, fmt.Errorf("memory: id %q does not name a lesson line or a skill file", id)
}

// rel renders a path workspace-relative when it is inside the workspace.
func rel(workspace, path string) string {
	if workspace == "" {
		return filepath.ToSlash(path)
	}
	r, err := filepath.Rel(workspace, path)
	if err != nil || strings.HasPrefix(r, "..") {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}

// ---- memory_edit -----------------------------------------------------------

// EditRequest is one memory_edit call.
type EditRequest struct {
	// Op is update | forget | invalidate.
	Op string
	// ID is the entry id Recall printed ("<path>:<line>" or "<path>").
	ID string
	// Text is the replacement for update.
	Text string
	// Reason is why an entry is invalidated or a skill deleted; with it the
	// entry is kept on disk instead of destroyed.
	Reason string
	// Class overrides the entry's own class. Empty means lesson (or
	// convention for a skill file).
	Class string
	// SkillsDir overrides the skills directory. Empty means the workspace's
	// own.
	SkillsDir string
}

// Edit updates, forgets or invalidates one entry by id. As with Retain, a
// gated class is never written directly: the change is queued as a proposal for
// the operator instead, and the entry on disk is left alone.
//
// forget removes the entry; invalidate moves it out of the recallable set while
// keeping it on disk (invalidated.md, or skills/invalidated/), because memory
// keeps its history the way the ledger does.
func (s *Store) Edit(req EditRequest, g Gate) (WriteResult, error) {
	operation, err := normalizeEditOp(req.Op)
	if err != nil {
		return WriteResult{}, err
	}
	t, err := s.resolveID(req.ID, req.SkillsDir)
	if err != nil {
		return WriteResult{}, err
	}
	cls := NormalizeClass(req.Class)
	if strings.TrimSpace(req.Class) == "" {
		if t.skill {
			cls = fctx.ClassConvention
		} else {
			cls = fctx.ClassLesson
		}
	}
	res := WriteResult{Verb: "memory_edit", Class: cls, Op: operation, ID: EntryID(t.rel, t.line)}

	body := ""
	if operation != OpForget {
		if body, err = refusalFree(textOrReason(operation, req.Text, req.Reason)); err != nil {
			return WriteResult{}, err
		}
	}

	if g.Gated(cls) {
		content := body
		if operation == OpForget {
			content = "(forget) " + EntryID(t.rel, t.line)
		}
		path, err := QueueProposal(s.Workspace, g.Session, fctx.Proposal{
			Section:    cls,
			Op:         "append",
			Content:    LessonLine(today(), content, "", "memory_edit "+operation+" on "+EntryID(t.rel, t.line)),
			Reason:     "memory_edit: the model asked to " + operation + " a memory entry, and the class is operator-gated",
			Confidence: "low",
		})
		if err != nil {
			return WriteResult{}, err
		}
		res.Queued = true
		res.Path = path
		res.Detail = fmt.Sprintf("class %q is operator-gated: the %s was queued as a proposal for review; %s is unchanged", cls, operation, res.ID)
		return res, nil
	}

	switch operation {
	case OpUpdate:
		if err := s.updateTarget(t, body); err != nil {
			return WriteResult{}, err
		}
		res.Detail = "updated " + res.ID
	case OpForget:
		if err := s.removeTarget(t, ""); err != nil {
			return WriteResult{}, err
		}
		res.Detail = "forgot " + res.ID
	case OpInvalidate:
		if err := s.removeTarget(t, body); err != nil {
			return WriteResult{}, err
		}
		res.Detail = "invalidated " + res.ID + " (kept on disk, no longer recallable)"
	}
	res.Written = true
	res.Path = t.rel
	if _, err := s.Reindex(); err != nil {
		return res, err
	}
	return res, nil
}

// textOrReason picks the text an operation records: the replacement for update,
// the reason for invalidate.
func textOrReason(op, text, reason string) string {
	if t := strings.TrimSpace(text); t != "" {
		return t
	}
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	return op + " (no text given)"
}

func normalizeEditOp(op string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case OpUpdate, "replace", "rewrite", "set":
		return OpUpdate, nil
	case OpForget, "delete", "remove", "drop":
		return OpForget, nil
	case OpInvalidate, "supersede", "retire", "stale":
		return OpInvalidate, nil
	}
	return "", fmt.Errorf("memory: unknown memory_edit op %q (want update|forget|invalidate)", op)
}

// updateTarget rewrites one entry in place.
func (s *Store) updateTarget(t target, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("memory: update needs text")
	}
	if t.skill {
		return s.updateSkill(t, text)
	}
	return s.rewriteLessonLine(t, func(e Entry) string {
		return LessonLine(e.Date, text, e.Models, e.Source)
	})
}

// removeTarget deletes an entry, or moves it to the invalidated set when a
// reason is given.
func (s *Store) removeTarget(t target, reason string) error {
	invalidated := strings.TrimSpace(reason) != ""
	if t.skill {
		return s.removeSkill(t, reason, invalidated)
	}
	var moved string
	err := s.rewriteLessonLine(t, func(e Entry) string {
		moved = LessonLine(e.Date, e.Text, e.Models, e.Source)
		return ""
	})
	if err != nil {
		return err
	}
	if invalidated && moved != "" {
		return s.archiveInvalidated(moved, reason)
	}
	return nil
}

// rewriteLessonLine applies fn to one learned.md line: a non-empty return
// replaces the line, an empty one removes it. The file is rewritten atomically.
func (s *Store) rewriteLessonLine(t target, fn func(Entry) string) error {
	b, err := os.ReadFile(t.abs)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	if t.line < 1 || t.line > len(lines) {
		return fmt.Errorf("memory: %s has no line %d (re-run recall; the file changed)", t.rel, t.line)
	}
	e, ok := ParseLesson(lines[t.line-1])
	if !ok {
		return fmt.Errorf("memory: %s:%d is not a lesson line any more", t.rel, t.line)
	}
	replacement := fn(e)
	if strings.TrimSpace(replacement) == "" {
		lines = append(lines[:t.line-1], lines[t.line:]...)
	} else {
		lines[t.line-1] = replacement
	}
	return writeAtomic(t.abs, strings.Join(lines, "\n"))
}

// archiveInvalidated appends one invalidated lesson to invalidated.md, with the
// date and the reason, so the entry keeps its provenance.
func (s *Store) archiveInvalidated(line, reason string) error {
	p := InvalidatedPath(s.Workspace)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	entry := line + " _invalidated: " + today() + " " + oneLine(reason)
	body := ""
	if b, err := os.ReadFile(p); err == nil {
		body = strings.TrimRight(string(b), "\n")
	}
	if body == "" {
		body = "# Invalidated memory\n\nEntries a verb invalidated. Kept for history; never indexed, never recalled.\n"
	}
	return writeAtomic(p, body+"\n\n"+entry+"\n")
}

// ---- learn (promote a lesson to a skill) -----------------------------------

// PromoteRequest is one `learn` call.
type PromoteRequest struct {
	// Lesson is the lesson to promote: an entry id from Recall, or the lesson
	// text itself.
	Lesson string
	// Name is the skill name; the file is <SkillsDir>/<name>.md.
	Name string
	// Description is the one-line index entry. Empty means the lesson's first
	// line.
	Description string
	// Class overrides the class. Empty means convention: a skill is a
	// procedure.
	Class string
	// SkillsDir overrides the skills directory.
	SkillsDir string
}

// Promote turns a lesson into a skill: the lesson's text (or a literal text the
// model supplies) becomes the body of `<skillsDir>/<name>.md`, which Reindex
// then makes recallable as a `skill` entry. Promoting by id leaves the original
// lesson in place — learned.md is append-only — and the skill records where it
// came from.
//
// A gated class is queued as a proposal instead of written, like every other
// verb.
func (s *Store) Promote(req PromoteRequest, g Gate) (WriteResult, error) {
	skillName, err := NormalizeSkillName(req.Name)
	if err != nil {
		return WriteResult{}, err
	}
	cls := NormalizeClass(req.Class)
	if strings.TrimSpace(req.Class) == "" {
		cls = fctx.ClassConvention
	}
	res := WriteResult{Verb: "learn", Class: cls, Op: "promote"}

	body, sourceID, err := s.lessonBody(req.Lesson)
	if err != nil {
		return WriteResult{}, err
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = truncateBytes(body, 160)
	}

	if g.Gated(cls) {
		path, err := QueueProposal(s.Workspace, g.Session, fctx.Proposal{
			Section:    cls,
			Op:         "append",
			Content:    LessonLine(today(), "skill "+skillName+": "+body, "", sourceID),
			Reason:     "learn: the model asked to promote a lesson to the skill " + skillName + ", and the class is operator-gated",
			Confidence: "low",
		})
		if err != nil {
			return WriteResult{}, err
		}
		res.Queued = true
		res.Path = path
		res.Detail = fmt.Sprintf("class %q is operator-gated: the promotion of %q was queued as a proposal for review; no skill file was written", cls, skillName)
		return res, nil
	}

	dir := s.skillRoot(req.SkillsDir)
	path := filepath.Join(dir, skillName+".md")
	created := false
	if _, err := os.Stat(path); err != nil {
		created = true
	}
	content := SkillFile(skillName, description, body)
	if !created {
		// An existing skill grows: the promoted lesson becomes a dated bullet
		// in its body rather than replacing a procedure the operator keeps.
		old, err := os.ReadFile(path)
		if err != nil {
			return WriteResult{}, err
		}
		content = strings.TrimRight(string(old), "\n") + "\n\n- [" + today() + "] " + body
		if sourceID != "" {
			content += " (promoted from " + sourceID + ")"
		}
		content += "\n"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return WriteResult{}, err
	}
	if err := writeAtomic(path, content); err != nil {
		return WriteResult{}, err
	}
	if _, err := s.Reindex(); err != nil {
		return res, err
	}
	res.Written = true
	res.Path = rel(s.Workspace, path)
	res.ID = EntryID(res.Path, 0)
	if created {
		res.Detail = "promoted to the new skill " + skillName + " (" + res.Path + ")"
	} else {
		res.Detail = "appended to the existing skill " + skillName + " (" + res.Path + ")"
	}
	return res, nil
}

// lessonIDRe is the shape of a lesson entry id: a .md path plus a line number.
// Anything else the model hands `learn` is literal lesson text.
var lessonIDRe = regexp.MustCompile(`(?i)^([^\s]+\.md):([1-9][0-9]*)$`)

// lessonBody resolves the lesson to promote: an entry id is looked up, anything
// else is treated as literal text the model supplied. Either way the text is
// refusal-filtered.
func (s *Store) lessonBody(lesson string) (body, sourceID string, err error) {
	raw := strings.TrimSpace(lesson)
	if raw == "" {
		return "", "", fmt.Errorf("memory: learn needs a lesson (an id from recall, or the text itself)")
	}
	if m := lessonIDRe.FindStringSubmatch(filepath.ToSlash(raw)); m != nil {
		id, line := m[1], atoi(m[2])
		p := id
		if !filepath.IsAbs(p) {
			p = filepath.Join(s.Workspace, filepath.FromSlash(id))
		}
		if b, rerr := os.ReadFile(p); rerr == nil {
			lines := strings.Split(string(b), "\n")
			if line >= 1 && line <= len(lines) {
				if e, ok := ParseLesson(lines[line-1]); ok {
					clean := scrub(e.Text)
					if clean == "" {
						return "", "", fmt.Errorf("memory: a refusal is telemetry, never memory — nothing was promoted")
					}
					return truncateBytes(clean, maxFactBytes), EntryID(id, line), nil
				}
			}
		}
	}
	clean, err := refusalFree(raw)
	if err != nil {
		return "", "", err
	}
	return truncateBytes(clean, maxFactBytes), "", nil
}

func atoi(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func (s *Store) skillRoot(skillsDir string) string {
	if d := strings.TrimSpace(skillsDir); d != "" {
		return d
	}
	return SkillsDir(s.Workspace)
}

// SkillFile renders one skill file: frontmatter the skill loader reads, then the
// body.
func SkillFile(name, description, body string) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(strings.TrimSpace(name))
	b.WriteString("\ndescription: ")
	b.WriteString(oneLine(truncateBytes(strings.TrimSpace(description), 180)))
	b.WriteString("\n---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return b.String()
}

// ---- manage_skill ----------------------------------------------------------

// SkillRequest is one `manage_skill` call.
type SkillRequest struct {
	// Op is create | update | delete.
	Op string
	// Name is the skill name; the file is <SkillsDir>/<name>.md. It is never
	// a path.
	Name string
	// Description is the one-line index entry.
	Description string
	// Body is the Markdown procedure (create and update).
	Body string
	// Reason is why the skill is deleted. With it the file is kept aside
	// instead of destroyed.
	Reason string
	// SkillsDir overrides the skills directory.
	SkillsDir string
}

// ManageSkill creates, updates or deletes one skill file under the skills
// directory. The name is validated and can never name a path: a skill lives in
// the directory the workspace already uses, and nowhere else.
//
// Like every other write verb, a gated class is queued as a proposal instead of
// written. A skill is a convention, so that only bites when the operator gated
// the class — which is the point of reusing one gate instead of inventing a
// second one.
func (s *Store) ManageSkill(req SkillRequest, g Gate) (WriteResult, error) {
	operation, err := normalizeSkillOp(req.Op)
	if err != nil {
		return WriteResult{}, err
	}
	skillName, err := NormalizeSkillName(req.Name)
	if err != nil {
		return WriteResult{}, err
	}
	dir := s.skillRoot(req.SkillsDir)
	path := filepath.Join(dir, skillName+".md")
	res := WriteResult{
		Verb: "manage_skill", Class: fctx.ClassConvention, Op: operation,
		Path: rel(s.Workspace, path),
	}

	if g.Gated(res.Class) {
		p, err := QueueProposal(s.Workspace, g.Session, fctx.Proposal{
			Section:    res.Class,
			Op:         "append",
			Content:    LessonLine(today(), "skill "+operation+" "+skillName, "", oneLine(req.Reason)),
			Reason:     "manage_skill: the model asked to " + operation + " a skill, and the class is operator-gated",
			Confidence: "low",
		})
		if err != nil {
			return WriteResult{}, err
		}
		res.Queued = true
		res.Path = p
		res.Detail = fmt.Sprintf("class %q is operator-gated: the skill %s was queued as a proposal for review; %s was not written",
			res.Class, operation, skillName)
		return res, nil
	}

	switch operation {
	case SkillCreate:
		if _, err := os.Stat(path); err == nil {
			return WriteResult{}, fmt.Errorf("memory: skill %q already exists at %s — use op=update", skillName, res.Path)
		}
		clean, err := refusalFree(req.Body)
		if err != nil {
			return WriteResult{}, err
		}
		description := strings.TrimSpace(req.Description)
		if description == "" {
			description = firstBodyLine(clean)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return WriteResult{}, err
		}
		if err := writeAtomic(path, SkillFile(skillName, description, clean)); err != nil {
			return WriteResult{}, err
		}
		res.Detail = "created the skill " + skillName + " (" + res.Path + ")"

	case SkillUpdate:
		old, err := os.ReadFile(path)
		if err != nil {
			return WriteResult{}, fmt.Errorf("memory: no skill %q at %s — use op=create", skillName, res.Path)
		}
		clean, err := refusalFree(req.Body)
		if err != nil {
			return WriteResult{}, err
		}
		description := strings.TrimSpace(req.Description)
		if description == "" {
			description = existingDescription(string(old))
		}
		if description == "" {
			description = firstBodyLine(clean)
		}
		if err := writeAtomic(path, SkillFile(skillName, description, clean)); err != nil {
			return WriteResult{}, err
		}
		res.Detail = "updated the skill " + skillName + " (" + res.Path + ")"

	case SkillDelete:
		if _, err := os.Stat(path); err != nil {
			return WriteResult{}, fmt.Errorf("memory: no skill %q at %s", skillName, res.Path)
		}
		// A reasoned delete keeps the file: memory keeps its history the way
		// the ledger does, and a skill the operator wrote is not lost to one
		// model call.
		if strings.TrimSpace(req.Reason) != "" {
			aside, err := s.MoveSkillAside(dir, skillName, req.Reason)
			if err != nil {
				return WriteResult{}, err
			}
			res.Path = aside
			res.Detail = "deleted the skill " + skillName + " (kept aside at " + aside + ", no longer indexed)"
		} else {
			if err := os.Remove(path); err != nil {
				return WriteResult{}, err
			}
			res.Detail = "deleted the skill " + skillName + " (" + res.Path + ")"
		}
	}

	res.Written = true
	res.ID = EntryID(res.Path, 0)
	if _, err := s.Reindex(); err != nil {
		return res, err
	}
	return res, nil
}

// MoveSkillAside moves one skill file out of the skills directory into
// `<dir>/invalidated/`, where neither Reindex nor the skill loader looks, and
// returns the workspace-relative path it was moved to.
func (s *Store) MoveSkillAside(dir, stem, reason string) (string, error) {
	stem, err := NormalizeSkillName(stem)
	if err != nil {
		return "", err
	}
	root := s.skillRoot(dir)
	src := filepath.Join(root, stem+".md")
	b, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("memory: no skill %q at %s", stem, rel(s.Workspace, src))
	}
	aside := filepath.Join(root, invalidatedSkillDir)
	if err := os.MkdirAll(aside, 0o755); err != nil {
		return "", err
	}
	note := "\n\n<!-- invalidated " + today() + ": " + oneLine(reason) + " -->\n"
	dst := filepath.Join(aside, stem+".md")
	if err := writeAtomic(dst, string(b)+note); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", err
	}
	return rel(s.Workspace, dst), nil
}

func normalizeSkillOp(op string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case SkillCreate, "new", "add", "write":
		return SkillCreate, nil
	case SkillUpdate, "edit", "replace", "set":
		return SkillUpdate, nil
	case SkillDelete, "remove", "forget", "drop":
		return SkillDelete, nil
	}
	return "", fmt.Errorf("memory: unknown manage_skill op %q (want create|update|delete)", op)
}

var skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NormalizeSkillName turns a model-supplied skill name into a safe file stem.
// A name is never a path: separators and traversal are refused, not repaired.
func NormalizeSkillName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".md")
	n = strings.ReplaceAll(strings.ReplaceAll(n, " ", "-"), "_", "-")
	if n == "" {
		return "", fmt.Errorf("memory: a skill needs a name")
	}
	if strings.ContainsAny(n, `/\`) || strings.Contains(n, "..") {
		return "", fmt.Errorf("memory: skill name %q must not be a path", name)
	}
	if !skillNameRe.MatchString(n) {
		return "", fmt.Errorf("memory: skill name %q is not usable (want letters, digits, '.', '_', '-' up to 64 characters)", name)
	}
	return n, nil
}

// updateSkill rewrites a skill body, keeping the frontmatter the file already
// has when the replacement is not a whole file.
func (s *Store) updateSkill(t target, text string) error {
	old, err := os.ReadFile(t.abs)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(text)
	if strings.HasPrefix(body, "---") {
		return writeAtomic(t.abs, body+"\n")
	}
	name, desc := splitSkill(string(old))
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(t.abs), ".md")
	}
	if desc == "" {
		desc = firstBodyLine(body)
	}
	return writeAtomic(t.abs, SkillFile(name, desc, body))
}

// removeSkill deletes a skill file, or moves it under <skills>/invalidated/
// when a reason is given. Neither location is indexed any more.
func (s *Store) removeSkill(t target, reason string, invalidated bool) error {
	if !invalidated {
		return os.Remove(t.abs)
	}
	_, err := s.MoveSkillAside(filepath.Dir(t.abs), strings.TrimSuffix(filepath.Base(t.abs), ".md"), reason)
	return err
}

// splitSkill reads the name and description out of a skill file's frontmatter.
func splitSkill(raw string) (name, description string) {
	if !strings.HasPrefix(raw, "---") {
		return "", ""
	}
	rest := raw[3:]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return "", ""
	}
	rest = rest[nl+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			name = strings.TrimSpace(val)
		case "description":
			description = strings.TrimSpace(val)
		}
	}
	return name, description
}

// existingDescription is splitSkill's description, for an update that did not
// supply one.
func existingDescription(raw string) string {
	_, desc := splitSkill(raw)
	return desc
}

func firstBodyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>*- "))
		if l != "" {
			return oneLine(l)
		}
	}
	return ""
}

// ---- recall ----------------------------------------------------------------

// Recall searches the store and returns the hits a model may see: bounded, and
// refusal-filtered on the way out. It is Search with the verb's two rules
// applied, so a hand-edited learned.md cannot hand a refusal marker to a model.
func (s *Store) Recall(query string, k int) ([]Entry, error) {
	if k <= 0 {
		k = DefaultRecallK
	}
	if k > MaxRecallK {
		k = MaxRecallK
	}
	entries, err := s.Search(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		e.Text = scrub(e.Text)
		if e.Text == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// FormatRecall renders recall hits for a tool result: the store's own Format,
// plus the stable id each entry needs so memory_edit and learn can name it.
// The id is derived from the same path and line Format already prints as the
// source, so it is provenance the model can act on rather than a new column.
func FormatRecall(entries []Entry) string {
	if len(entries) == 0 {
		return "no memory entries matched"
	}
	var b strings.Builder
	for _, e := range entries {
		var bits []string
		if e.Date != "" {
			bits = append(bits, e.Date)
		}
		if e.Models != "" {
			bits = append(bits, "models="+e.Models)
		}
		meta := ""
		if len(bits) > 0 {
			meta = " (" + strings.Join(bits, ", ") + ")"
		}
		fmt.Fprintf(&b, "- [%s]%s %s\n", e.Kind, meta, e.Text)
		if id := e.ID(); id != "" {
			fmt.Fprintf(&b, "  id: %s", id)
			if s := strings.TrimSpace(e.Source); s != "" {
				fmt.Fprintf(&b, "  source: %s", s)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Reflector is the seam `reflect` uses to ask a model over the bank: one plain
// completion with no tools, so a synthesis can never call a tool or change the
// workspace. tools.NewReflector builds one from the curator's client.
type Reflector func(ctx context.Context, system, user string) (string, error)

// RecentLessons reads learned.md directly — no index, no database handle — and
// returns the newest n lessons, refusal-filtered, newest first. Reading the
// Markdown rather than the store keeps the digest independent of the derived
// index and of any lock another holder has on it.
func RecentLessons(workspace string, n int) []Entry {
	if n <= 0 {
		n = digestMaxLessons
	}
	b, err := os.ReadFile(LearnedPath(workspace))
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	out := make([]Entry, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		e, ok := ParseLesson(lines[i])
		if !ok {
			continue
		}
		e.Text = scrub(e.Text)
		if e.Text == "" {
			continue
		}
		e.Kind = "lesson"
		e.Path = rel(workspace, LearnedPath(workspace))
		e.Line = i + 1
		out = append(out, e)
	}
	return out
}

// Bank renders a bounded view of the whole store for `reflect`: the newest
// lessons and the skills beside them. It is the synthesis input, so it is
// capped by tokens and refusal-filtered like every other read path.
func (s *Store) Bank(maxLessons, maxTokens int) string {
	if maxLessons <= 0 {
		maxLessons = BankMaxLessons
	}
	if maxTokens <= 0 {
		maxTokens = BankMaxTokens
	}
	lessons := RecentLessons(s.Workspace, maxLessons)
	skills := s.skillSummaries(maxLessons)

	var b strings.Builder
	b.WriteString("# Memory bank (local)\n")
	fmt.Fprintf(&b, "\n%d lesson(s), %d skill(s).\n", len(lessons), len(skills))
	if len(lessons) > 0 {
		b.WriteString("\n## Lessons (newest first)\n\n")
		for _, e := range lessons {
			fmt.Fprintf(&b, "- %s %s [%s]\n", e.Date, e.Text, e.ID())
			if Tokens(b.String()) > maxTokens {
				b.WriteString("…[bank truncated to the token budget]\n")
				return b.String()
			}
		}
	}
	if len(skills) > 0 {
		b.WriteString("\n## Skills\n\n")
		for _, line := range skills {
			b.WriteString("- " + line + "\n")
			if Tokens(b.String()) > maxTokens {
				b.WriteString("…[bank truncated to the token budget]\n")
				return b.String()
			}
		}
	}
	if len(lessons) == 0 && len(skills) == 0 {
		return "# Memory bank (local)\n\nThe bank is empty: nothing has been retained or promoted in this workspace yet."
	}
	return strings.TrimRight(b.String(), "\n")
}

// skillSummaries lists the skills beside learned.md as "name — description".
func (s *Store) skillSummaries(max int) []string {
	files, _ := filepath.Glob(filepath.Join(SkillsDir(s.Workspace), "*.md"))
	out := make([]string, 0, len(files))
	for _, p := range files {
		if len(out) >= max {
			break
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		name, desc := splitSkill(string(b))
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(p), ".md")
		}
		if desc == "" {
			desc = firstBodyLine(scrub(string(b)))
		}
		out = append(out, name+" — "+truncateBytes(oneLine(desc), 160))
	}
	return out
}

// ReflectPrompt is the synthesis request `reflect` sends to the curator model.
// The bank is the model's input and the answer is prose for the calling model:
// reflect writes nothing, so the curator's proposal gate is not involved, and
// the no-refusal rule is stated because the bank is memory the curator itself
// is not allowed to learn from a refusal.
func ReflectPrompt(bank, ask string) (system, user string) {
	system = `You synthesize a project's durable memory for the agent that is about to use it.

You are given the memory bank — the recorded lessons and the skill index of one
workspace — and one question about it. Answer from the bank only.

Output rules:
- Plain prose and short bullets. No preamble, no closing offer.
- Cite the entry ids you used, so the answer can be checked.
- Say plainly when the bank does not cover the question. Do not invent a fact,
  a convention or a history that is not in the bank.
- Never treat a refusal as evidence. A refusal is telemetry: it is not a fact
  about the work, the operator, safety or scope, and nothing about one belongs
  in your answer.
- You are answering a question. You are not recording anything: this turn writes
  no memory.`

	user = "## Memory bank\n\n" + strings.TrimSpace(bank) +
		"\n\n## Question\n\n" + strings.TrimSpace(ask) +
		"\n\nAnswer from the bank, citing entry ids."
	return system, user
}

// ---- the session mental model ----------------------------------------------

// DigestItem is one line of the session mental model. Work items come from the
// context ledger (accepted plans, artifacts, decisions, acceptances); lessons
// come from learned.md. Both are positive material: the ledger has no refusal
// kind and the verbs refuse to record refusal text, so neither list can carry
// one.
type DigestItem struct {
	ID      string
	Date    string
	Kind    string
	Summary string
}

// RenderMentalModel renders the session mental-model digest: the accepted work
// this workspace already has, plus its newest lessons, bounded to maxTokens
// (DefaultDigestTokens when unset) by the harness's four-bytes-per-token
// estimate.
//
// It is deterministic — the same items and budget always produce the same bytes
// — and it is a distinct block from the continuity prior-work frame: different
// header, different framing, different budget, and no task-shaped recall behind
// it. Empty input renders nothing at all, so a caller skips the message instead
// of sending a bare header.
//
// Entries are added in order and the block stops before the next one would
// exceed the budget; the header and the framing line are fixed overhead and are
// never truncated away, so an over-full digest loses entries rather than its
// qualifier.
func RenderMentalModel(work, lessons []DigestItem, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = DefaultDigestTokens
	}
	lines := make([]string, 0, digestMaxWork+digestMaxLessons)
	for _, w := range limit(work, digestMaxWork) {
		if line, ok := digestLine(w, ""); ok {
			lines = append(lines, line)
		}
	}
	for _, l := range limit(lessons, digestMaxLessons) {
		if line, ok := digestLine(l, "lesson"); ok {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(MentalModelHeader)
	b.WriteString("\n\n")
	b.WriteString(mentalModelFraming)
	for _, line := range lines {
		if Tokens(b.String()+"\n"+line) > maxTokens {
			break
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

// digestLine renders one entry as a single flattened Markdown bullet, so no
// field can inject a heading, a list or a message role into the digest.
func digestLine(d DigestItem, forceKind string) (string, bool) {
	summary := oneLine(scrub(d.Summary))
	if summary == "" {
		return "", false
	}
	kind := oneLine(d.Kind)
	if forceKind != "" {
		kind = forceKind
	}
	if kind == "" {
		kind = "work"
	}
	date := oneLine(d.Date)
	if len(date) >= len("2006-01-02") {
		date = date[:len("2006-01-02")]
	}
	if date == "" {
		date = "undated"
	}

	var b strings.Builder
	if id := oneLine(d.ID); id != "" {
		fmt.Fprintf(&b, "- %s · %s · %s: %s", id, date, kind, truncateBytes(summary, digestMaxSummaryBytes))
		return b.String(), true
	}
	fmt.Fprintf(&b, "- %s · %s: %s", kind, date, truncateBytes(summary, digestMaxSummaryBytes))
	return b.String(), true
}

func limit(items []DigestItem, n int) []DigestItem {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// DigestFromEntries renders the session mental model straight from the
// workspace ledger: the accepted work it holds, plus the newest lessons in
// learned.md. It is the digest WP M10-C asks the first turn of a fresh session
// to carry, and it is deliberately not the continuity prior-work block — no
// task-shaped recall runs behind it, the budget is 200 tokens instead of 600,
// and the header and framing say "mental model".
//
// The refusal invariant holds by construction: the ledger has no refusal kind
// and its drift rows are metadata only, so everything that is not a recallable
// active entry (drift, superseded) is dropped before rendering, and the lessons
// pass the same refusal filter every other read path uses. There is no
// parameter here that could carry a refusal into the digest.
//
// entries come from ledger.Store.List(""), the administrative view, because
// List is the only ledger read that needs no query; the filter below is the same
// one the ledger's own Search applies.
func DigestFromEntries(entries []ledger.Entry, lessons []DigestItem, maxTokens int) string {
	work := make([]DigestItem, 0, len(entries))
	for _, e := range entries {
		if !digestRecallable(e) {
			continue
		}
		summary := strings.TrimSpace(e.Summary)
		if summary == "" {
			summary = strings.TrimSpace(e.Task)
		}
		if summary == "" {
			continue
		}
		date := strings.TrimSpace(e.TS)
		if len(date) >= len("2006-01-02") {
			date = date[:len("2006-01-02")]
		}
		work = append(work, DigestItem{ID: e.ID, Date: date, Kind: e.Kind, Summary: summary})
	}
	// Newest first: the ledger is chronological, and a digest says where the
	// work stands, not how it began.
	reverse(work)
	return RenderMentalModel(work, lessons, maxTokens)
}

// digestRecallable is the ledger's own recall gate, restated for the digest:
// positive kinds only, active only. Drift and superseded entries are never
// digest material.
func digestRecallable(e ledger.Entry) bool {
	if e.Status != ledger.StatusActive {
		return false
	}
	switch e.Kind {
	case ledger.KindPlan, ledger.KindArtifact, ledger.KindDecision, ledger.KindAcceptance:
		return true
	}
	return false
}

// LessonDigest reads the newest lessons in learned.md as digest items, newest
// first, refusal-filtered. It needs no store handle, so the digest can be
// rendered without opening the derived index.
func LessonDigest(workspace string, n int) []DigestItem {
	entries := RecentLessons(workspace, n)
	out := make([]DigestItem, 0, len(entries))
	for _, e := range entries {
		out = append(out, DigestItem{ID: e.ID(), Date: e.Date, Kind: "lesson", Summary: e.Text})
	}
	return out
}

func reverse(items []DigestItem) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}

// ---- shared helpers --------------------------------------------------------

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateBytes cuts s to at most maxBytes on a rune boundary, marking the cut.
func truncateBytes(s string, maxBytes int) string {
	if maxBytes <= 4 || len(s) <= maxBytes {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > maxBytes-3 { // room for "…"
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

// writeAtomic writes through a temporary file in the same directory, so a
// failed verb never leaves a half-written learned.md or skill behind.
func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
