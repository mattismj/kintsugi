// Package taste is the local taste store (M10-B, DESIGN §16.15).
//
// Every accept, reject or edit of a proposed change is a signal. Signals
// accumulate as dated Markdown bullets in two append-only files:
//
//	<ws>/.kintsugi/taste/taste.md            the workspace's own taste
//	~/.config/kintsugi/taste/taste.md        the operator's global taste
//
// The compiled behavior layer may append the bounded rendering of those
// bullets (Block) as context, labelled "operator taste (local)".
//
// Trust boundary. Taste is prompt text and nothing else. This package exposes
// no permission type, no rule type and no way to reach the tool policy: the
// only thing it can add to a request is Markdown, and every block it renders
// carries the Framing sentence that says so. It is local-only — there is no
// remote sync, no provider call, and no model-authored policy: the bullets are
// written from the operator's own decisions (an approval granted or denied, a
// file the operator re-edited), never from model output.
//
// A refusal is never memory (the M8.5 invariant) and nothing here changes
// that: a `reject` bullet records the operator's decision about a tool call —
// tool name plus the redacted call detail the operator was shown — and never
// quotes a model's refusal prose. Refusal telemetry stays in the session log.
//
// Storage rules. The files are 0600 inside a 0700 directory: taste is the
// operator's, not the repository's. Writes are append-only; there is no code
// path in this package that rewrites or removes a bullet. A changed mind is a
// new bullet that names the old date, `(supersedes <YYYY-MM-DD>)`.
package taste

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Origin selects which of the two stores an operation touches.
type Origin string

const (
	// OriginProject is <ws>/.kintsugi/taste/taste.md.
	OriginProject Origin = "project"
	// OriginGlobal is ~/.config/kintsugi/taste/taste.md.
	OriginGlobal Origin = "global"
	// OriginAny means "both stores", project first. It is only valid for the
	// read operations (List, Lint, Block).
	OriginAny Origin = ""
)

// DefaultDir is the workspace-relative taste directory, and FileName the
// store's name inside it.
const (
	DefaultDir = ".kintsugi/taste"
	FileName   = "taste.md"
)

// Entry is one bullet.
type Entry struct {
	// Origin is the store the bullet lives in.
	Origin Origin `json:"origin"`
	// Date is the local calendar date of the signal, YYYY-MM-DD.
	Date string `json:"date"`
	// Kind is accept | reject | edit.
	Kind string `json:"kind"`
	// Subject is the tool name ("edit_file") or the file class ("docs/**")
	// the signal is about. It never contains a colon: that separates it from
	// the note in the bullet grammar.
	Subject string `json:"subject"`
	// Note is the one-line reason. Redacted, flattened, bounded.
	Note string `json:"note"`
	// Supersedes is the date of the bullet this one replaces, when it names
	// one.
	Supersedes string `json:"supersedes,omitempty"`
	// Superseded reports that a later bullet in the same store named this
	// bullet's date. Superseded bullets stay in the file (history) and stay
	// in List, but never reach Block.
	Superseded bool `json:"superseded,omitempty"`

	// Line is the 1-based line in Path, for `kintsugi taste lint`.
	Line int    `json:"line,omitempty"`
	Path string `json:"path,omitempty"`
}

// String renders the entry as its canonical bullet line.
func (e Entry) String() string {
	line, err := Render(e)
	if err != nil {
		return "- " + e.Date + " " + e.Kind + " " + e.Subject + ": (unrenderable: " + err.Error() + ")"
	}
	return line
}

// Store is one workspace's taste store. The zero value is usable: an empty
// Workspace means "no project store" and a zero Config means enabled with the
// default budget.
//
// A Store holds no open file handle, so it needs no Close: every write opens,
// appends and syncs, which is what makes an append-only file safe from two
// processes (a TUI session and a `kintsugi taste add` in another terminal).
type Store struct {
	// Workspace is the project root the project store lives under.
	Workspace string
	// Cfg is the [taste] block. Every read of it goes through cfg(), so the
	// environment's overrides apply to writes, reads and rendering alike.
	Cfg Config

	// mu serialises appends from the TUI's agent goroutine and the update
	// loop, which can both record a signal.
	mu sync.Mutex
}

// Open returns a store for a workspace with a resolved configuration. It
// creates nothing: the files appear on the first recorded signal.
func Open(workspace string, cfg Config) *Store {
	return &Store{Workspace: workspace, Cfg: cfg}
}

// Default returns an enabled store with the default budget. It never reads a
// config file, so it is the store tests and callers that already hold a
// Config use; StoreFor is the one that honours `[taste]`.
func Default(workspace string) *Store { return Open(workspace, Config{}) }

// cfg is the store's configuration with the environment's overrides applied.
func (s *Store) cfg() Config {
	if s == nil {
		return Config{}
	}
	return s.Cfg.WithEnv()
}

// Enabled reports whether signals are written at all. A disabled store writes
// nothing, renders no block, and returns no error from the write paths: taste
// being off must never break a session. Reading (List, Lint) still works, so
// an operator who turns taste off can still see what was recorded before.
func (s *Store) Enabled() bool { return s != nil && s.cfg().On() }

// Config returns the store's effective configuration, environment included.
func (s *Store) Config() Config { return s.cfg() }

// Path is the store file for an origin. OriginGlobal with no resolvable home
// directory yields "", and every operation on it is a no-op.
func (s *Store) Path(origin Origin) string {
	if s == nil {
		return ""
	}
	cfg := s.cfg()
	if origin == OriginGlobal {
		return GlobalPath(cfg)
	}
	if strings.TrimSpace(s.Workspace) == "" {
		return ""
	}
	return ProjectPath(s.Workspace, cfg)
}

// Dir is the directory holding the store file for an origin.
func (s *Store) Dir(origin Origin) string {
	p := s.Path(origin)
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// ---------- the API named in WP M10-B ----------
//
// These four are the package-level entry points the handoff names. They resolve
// the store through StoreFor, so they honour `[taste] enabled = false` from any
// of kintsugi's config files and the KINTSUGI_TASTE_* overrides: a disabled install
// writes nothing and injects nothing through these paths.

// Record appends one bullet to the project store (global=false) or to the
// operator's global store (global=true).
//
// A disabled store writes nothing and returns a zero Entry with no error. An
// exact duplicate of a bullet already recorded the same day is not written
// again: taste accumulates decisions, not keystrokes.
func Record(ws string, global bool, kind, subject, note string) (Entry, error) {
	return StoreFor(ws).Record(originOf(global), kind, subject, note)
}

// List returns the bullets in file order (chronological), for one origin or for
// both (OriginAny: project first, then global). A missing store is an empty
// list, not an error. Bullets that do not parse are skipped here and reported
// by Lint.
//
// List reads even when taste is disabled: an operator who turns learning off
// can still see what was recorded before.
func List(ws string, origin Origin) ([]Entry, error) { return StoreFor(ws).List(origin) }

// Lint checks both stores and returns every bullet that does not follow the
// grammar, plus the warnings (a dangling supersede, a file that is not 0600).
func Lint(ws string) ([]Finding, error) { return StoreFor(ws).Lint() }

// Block renders the bounded "operator taste (local)" block for a workspace
// (project bullets first, then the operator's global ones) and reports whether
// there is anything to inject. It returns ("", false) when taste is disabled,
// when no store exists, or when every bullet is superseded or unparseable.
func Block(ws string) (string, bool) { return StoreFor(ws).Block() }

// ---------- store methods ----------

// Record appends one bullet. See the package-level Record.
func (s *Store) Record(origin Origin, kind, subject, note string) (Entry, error) {
	return s.RecordEntry(origin, Entry{Kind: kind, Subject: subject, Note: note})
}

// RecordEntry appends a bullet the caller shaped: an explicit Date (default
// today) and an explicit Supersedes date. It is what `kintsugi taste add
// --supersedes` uses.
func (s *Store) RecordEntry(origin Origin, e Entry) (Entry, error) {
	if !s.Enabled() {
		return Entry{}, nil
	}
	origin = normalizeOrigin(origin)
	clean, err := normalize(e, origin)
	if err != nil {
		return Entry{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.Path(origin)
	if path == "" {
		return Entry{}, fmt.Errorf("taste: no %s store path (is $HOME set?)", origin)
	}
	// An identical signal recorded twice on the same day is one bullet.
	existing, err := readOrigin(s, origin)
	if err != nil {
		return Entry{}, err
	}
	if dup := sameDayDuplicate(existing, clean); dup != nil {
		return *dup, nil
	}
	if err := appendBullet(path, origin, clean); err != nil {
		return Entry{}, err
	}
	clean.Path = path
	return clean, nil
}

// List returns the bullets in file order (chronological).
func (s *Store) List(origin Origin) ([]Entry, error) {
	if s == nil {
		return nil, nil
	}
	if origin != OriginAny {
		return readOrigin(s, normalizeOrigin(origin))
	}
	out, err := readOrigin(s, OriginProject)
	if err != nil {
		return nil, err
	}
	global, err := readOrigin(s, OriginGlobal)
	if err != nil {
		return nil, err
	}
	return append(out, global...), nil
}

// Lint reports every grammar violation and warning in both stores.
func (s *Store) Lint() ([]Finding, error) {
	if s == nil {
		return nil, nil
	}
	var out []Finding
	for _, origin := range []Origin{OriginProject, OriginGlobal} {
		findings, err := lintOrigin(s.Path(origin), origin)
		if err != nil {
			return nil, err
		}
		out = append(out, findings...)
	}
	return out, nil
}

// Block renders the injected block for both stores and reports whether there
// is anything to inject. A disabled store injects nothing: this is the single
// place the config gate is applied to the prompt path, so an operator who
// turns taste off gets their bullets off the wire immediately, without
// deleting the file.
func (s *Store) Block() (string, bool) {
	return s.BlockOrigin(OriginAny)
}

// BlockOrigin renders the block for one origin (or both).
func (s *Store) BlockOrigin(origin Origin) (string, bool) {
	if !s.Enabled() {
		return "", false
	}
	entries, err := s.List(origin)
	if err != nil || len(entries) == 0 {
		return "", false
	}
	block := RenderBlock(entries, s.cfg())
	if strings.TrimSpace(block) == "" {
		return "", false
	}
	return block, true
}

// Supersede appends a bullet that replaces the one recorded on oldDate: the
// append-only way to change one's mind. The old bullet stays in the file and
// in List, flagged Superseded, so the history survives; it simply stops
// reaching Block.
//
// oldDate must name a bullet that exists in that store — superseding a date
// the file does not carry would be a bullet pointing at nothing, which Lint
// reports as an error.
func (s *Store) Supersede(origin Origin, oldDate string, kind, subject, note string) (Entry, error) {
	if !s.Enabled() {
		return Entry{}, nil
	}
	origin = normalizeOrigin(origin)
	oldDate = strings.TrimSpace(oldDate)
	if _, err := time.Parse(DateLayout, oldDate); err != nil {
		return Entry{}, fmt.Errorf("taste: the superseded date %q is not %s", oldDate, DateLayout)
	}
	existing, err := readOrigin(s, origin)
	if err != nil {
		return Entry{}, err
	}
	if !anyDate(existing, oldDate) {
		return Entry{}, fmt.Errorf("taste: no bullet in the %s store is dated %s, so there is nothing to supersede", origin, oldDate)
	}
	return s.RecordEntry(origin, Entry{
		Kind: kind, Subject: subject, Note: note, Supersedes: oldDate,
	})
}

func anyDate(entries []Entry, date string) bool {
	for _, e := range entries {
		if e.Date == date {
			return true
		}
	}
	return false
}

// normalize validates and cleans an entry before it can touch disk. It is the
// single place the grammar is enforced on the write path, so a malformed
// signal is refused rather than written unparseable.
func normalize(e Entry, origin Origin) (Entry, error) {
	out := e
	out.Origin = origin
	out.Kind = strings.ToLower(strings.TrimSpace(e.Kind))
	switch out.Kind {
	case KindAccept, KindReject, KindEdit:
	default:
		return Entry{}, fmt.Errorf("taste: unknown kind %q (want accept|reject|edit)", e.Kind)
	}
	out.Subject = strings.TrimSpace(e.Subject)
	out.Note = redactNote(e.Note)
	out.Date = strings.TrimSpace(e.Date)
	if out.Date == "" {
		out.Date = Today()
	}
	out.Supersedes = strings.TrimSpace(e.Supersedes)
	// Path and Line are read-side provenance; a caller cannot set them.
	out.Path, out.Line = "", 0
	out.Superseded = false

	// Render is the grammar check. If it fails, nothing is written.
	if _, err := Render(out); err != nil {
		return Entry{}, err
	}
	return out, nil
}

// ---------- paths ----------

// ProjectPath is the workspace store's path: <ws>/.kintsugi/taste/taste.md,
// or cfg.Dir when the operator moved it (workspace-relative, absolute or
// "~/…").
func ProjectPath(ws string, cfg Config) string {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return ""
	}
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = DefaultDir
	}
	dir = expandHome(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(ws, filepath.FromSlash(dir))
	}
	return filepath.Join(dir, FileName)
}

// GlobalPath is the operator-global store's path:
// ~/.config/kintsugi/taste/taste.md, honouring XDG_CONFIG_HOME exactly as
// the config package does. It returns "" when no home directory can be
// resolved, and every caller treats that as "no global store".
func GlobalPath(cfg Config) string {
	dir := strings.TrimSpace(cfg.GlobalDir)
	if dir == "" {
		home := configHome()
		if home == "" {
			return ""
		}
		return filepath.Join(home, "taste", FileName)
	}
	dir = expandHome(dir)
	if !filepath.IsAbs(dir) {
		if home := configHome(); home != "" {
			dir = filepath.Join(home, dir)
		}
	}
	return filepath.Join(dir, FileName)
}

// configHome is the operator's kintsugi config directory. It mirrors
// internal/config.GlobalPath's rule; it is duplicated here on purpose so that
// internal/config can depend on this package's Config type without an import
// cycle.
func configHome() string {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		return filepath.Join(x, "kintsugi")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "kintsugi")
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// ---------- internals ----------

// timeNow is the test seam for the bullet date.
var timeNow = time.Now

func originOf(global bool) Origin {
	if global {
		return OriginGlobal
	}
	return OriginProject
}

func normalizeOrigin(origin Origin) Origin {
	if origin == OriginGlobal {
		return OriginGlobal
	}
	return OriginProject
}

// readOrigin parses one store. A missing file is an empty store.
func readOrigin(s *Store, origin Origin) ([]Entry, error) {
	path := s.Path(origin)
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("taste: %w", err)
	}
	return Parse(b, origin, path), nil
}

// appendBullet appends one rendered bullet, writing the file header first when
// the store is new or empty. The file is 0600 and the directory 0700.
func appendBullet(path string, origin Origin, e Entry) error {
	line, err := Render(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("taste: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("taste: %w", err)
	}
	defer f.Close()
	// An existing file keeps its mode otherwise, so the mode is set
	// explicitly: the taste store is the operator's, not the workspace's.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("taste: %w", err)
	}
	if st, serr := f.Stat(); serr == nil && st.Size() == 0 {
		if _, werr := f.WriteString(header(origin)); werr != nil {
			return fmt.Errorf("taste: %w", werr)
		}
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("taste: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("taste: %w", err)
	}
	return nil
}

// sameDayDuplicate returns the bullet an identical signal would repeat.
func sameDayDuplicate(entries []Entry, e Entry) *Entry {
	for i := len(entries) - 1; i >= 0; i-- {
		c := entries[i]
		if c.Date == e.Date && c.Kind == e.Kind && c.Subject == e.Subject &&
			c.Note == e.Note && c.Supersedes == e.Supersedes {
			return &c
		}
	}
	return nil
}

// header is the fixed preamble of a taste file. No line of it starts with a
// list marker, so the parser and the linter never mistake prose for a bullet.
func header(origin Origin) string {
	if origin == OriginGlobal {
		return `# Taste (global)

Local operator taste for this operator, in every workspace (M10-B). One dated
bullet per signal, written as "- <YYYY-MM-DD> <accept|reject|edit> <subject>:
<note>", optionally ending in "(supersedes <YYYY-MM-DD>)" to name the bullet it
replaces. Append-only: a bullet is never edited or deleted, and a changed mind
is a new bullet. Taste is context, never policy: it grants no permission,
widens no scope, and never leaves this machine.
`
	}
	return `# Taste (project)

Local operator taste for this workspace (M10-B). One dated bullet per signal,
written as "- <YYYY-MM-DD> <accept|reject|edit> <subject>: <note>", optionally
ending in "(supersedes <YYYY-MM-DD>)" to name the bullet it replaces.
Append-only: a bullet is never edited or deleted, and a changed mind is a new
bullet. Taste is context, never policy: it grants no permission, widens no
origin, and never leaves this machine.
`
}
