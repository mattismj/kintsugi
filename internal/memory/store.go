// Package memory is tier-2 recall (DESIGN §4.5): lessons and skills live in
// Markdown, a derived SQLite FTS5 index makes them retrievable, and nothing
// here is injected unless the model asks for it.
package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB        *sql.DB
	Workspace string
}

type Entry struct {
	Text   string
	Models string
	Source string
	Date   string
	Kind   string
	Path   string
	Line   int
	Rank   float64
}

func Dir(workspace string) string { return filepath.Join(workspace, ".kintsugi") }
func LearnedPath(w string) string { return filepath.Join(Dir(w), "learned.md") }
func SkillsDir(w string) string   { return filepath.Join(Dir(w), "skills") }
func IndexPath(w string) string   { return filepath.Join(Dir(w), "index.sqlite") }

func Open(workspace string) (*Store, error) {
	if err := os.MkdirAll(Dir(workspace), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", IndexPath(workspace))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Workspace: workspace}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	_, err := s.DB.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memory USING fts5(
		text, models, source, date, kind, path, line,
		tokenize = 'porter unicode61'
	)`)
	return err
}

// Reindex rebuilds the derived index from Markdown sources (idempotent).
func (s *Store) Reindex() (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM memory`); err != nil {
		return 0, err
	}

	n := 0
	insert := func(e Entry) error {
		_, err := tx.Exec(`INSERT INTO memory (text, models, source, date, kind, path, line) VALUES (?,?,?,?,?,?,?)`,
			e.Text, e.Models, e.Source, e.Date, e.Kind, e.Path, fmt.Sprint(e.Line))
		if err == nil {
			n++
		}
		return err
	}

	if b, err := os.ReadFile(LearnedPath(s.Workspace)); err == nil {
		rel, _ := filepath.Rel(s.Workspace, LearnedPath(s.Workspace))
		for i, line := range strings.Split(string(b), "\n") {
			e, ok := ParseLesson(line)
			if !ok {
				continue
			}
			e.Kind = "lesson"
			e.Path = rel
			e.Line = i + 1
			if err := insert(e); err != nil {
				return n, err
			}
		}
	}

	skills, _ := filepath.Glob(filepath.Join(SkillsDir(s.Workspace), "*.md"))
	sort.Strings(skills)
	for _, p := range skills {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(s.Workspace, p)
		if err := insert(Entry{
			Text: strings.TrimSpace(string(b)),
			Kind: "skill", Path: rel, Line: 0,
		}); err != nil {
			return n, err
		}
	}

	return n, tx.Commit()
}

var lessonRe = regexp.MustCompile(`^-\s*\[(\d{4}-\d{2}-\d{2})\]\s*(.+)$`)
var modelsRe = regexp.MustCompile(`(?i)_models:\s*\[([^\]]*)\]`)
var sourceRe = regexp.MustCompile(`(?i)_source:\s*(.+)$`)

// ParseLesson parses one learned.md line. Format:
//   - [2026-10-08] Lesson text. _models: [deepseek, qwen] _source: session 7f3a, confidence=medium
func ParseLesson(line string) (Entry, bool) {
	m := lessonRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return Entry{}, false
	}
	e := Entry{Date: m[1]}
	text := strings.TrimSpace(m[2])

	if mm := modelsRe.FindStringSubmatch(text); mm != nil {
		e.Models = strings.TrimSpace(mm[1])
		text = strings.TrimSpace(modelsRe.ReplaceAllString(text, ""))
	}
	if sm := sourceRe.FindStringSubmatch(text); sm != nil {
		e.Source = strings.TrimSpace(sm[1])
		text = strings.TrimSpace(sourceRe.ReplaceAllString(text, ""))
	}
	e.Text = strings.TrimSpace(text)
	return e, e.Text != ""
}

// AddLesson appends a dated lesson to learned.md and reindexes.
func (s *Store) AddLesson(text, models, source string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty lesson")
	}
	line := "- [" + today() + "] " + strings.TrimSpace(text)
	if models != "" {
		line += " _models: [" + models + "]"
	}
	if source != "" {
		line += " _source: " + source
	}
	p := LearnedPath(s.Workspace)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() > 0 {
		line = "\n" + line
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return "", err
	}
	if _, err := s.Reindex(); err != nil {
		return "", err
	}
	return line, nil
}

func today() string {
	return timeNow().Format("2006-01-02")
}

// Search runs an FTS5 query; user input is sanitized into quoted terms joined
// with AND so stray quotes/operators cannot break the query.
func (s *Store) Search(query string, k int) ([]Entry, error) {
	terms := sanitizeTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 8
	}
	match := make([]string, len(terms))
	for i, t := range terms {
		match[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	rows, err := s.DB.Query(`
		SELECT text, models, source, date, kind, path, CAST(line AS INTEGER), bm25(memory)
		FROM memory WHERE memory MATCH ? ORDER BY bm25(memory) LIMIT ?`,
		strings.Join(match, " AND "), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Text, &e.Models, &e.Source, &e.Date, &e.Kind, &e.Path, &e.Line, &e.Rank); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func sanitizeTerms(q string) []string {
	fields := strings.Fields(strings.ToLower(q))
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, "\"'`.,;:!?()[]{}*^~-")
		if len(f) >= 2 && !stopword(f) {
			out = append(out, f)
		}
	}
	return out
}

func stopword(w string) bool {
	switch w {
	case "the", "and", "for", "with", "that", "this", "from", "does", "did", "was", "were", "are", "how", "why", "what":
		return true
	}
	return false
}

// Format renders search results for a tool result or CLI output.
func Format(entries []Entry) string {
	if len(entries) == 0 {
		return "no memory entries matched"
	}
	var b strings.Builder
	for _, e := range entries {
		loc := e.Path
		if e.Line > 0 {
			loc = fmt.Sprintf("%s:%d", e.Path, e.Line)
		}
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
		if loc != "" {
			fmt.Fprintf(&b, "  source: %s %s\n", loc, e.Source)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
