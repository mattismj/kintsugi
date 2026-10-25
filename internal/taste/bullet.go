package taste

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The bullet grammar. One bullet is one Markdown list line, and one line only:
//
//   - <date> <kind> <subject>: <note>
//   - <date> <kind> <subject>: <note> (supersedes <date>)
//
// where
//
//	date     YYYY-MM-DD (the local calendar day of the signal)
//	kind     accept | reject | edit            (case-insensitive on read)
//	subject  one token, no colon: a tool name ("edit_file", "bash") or a file
//	         class ("go", "docs/**", "internal/taste/")
//	note     one flattened line, redacted, at most MaxNoteBytes
//
// Nothing else in a taste file is a bullet: headings, blank lines and prose
// are skipped by the parser and reported by the linter only when they start
// with a list marker and still do not follow the grammar.
const (
	// KindAccept is a granted approval or an accepted change.
	KindAccept = "accept"
	// KindReject is a denied approval.
	KindReject = "reject"
	// KindEdit is an operator re-edit of a file the agent had just written.
	KindEdit = "edit"
)

const (
	// DateLayout is the bullet date format.
	DateLayout = "2006-01-02"
	// MaxNoteBytes bounds one bullet's note before rendering.
	MaxNoteBytes = 160
	// MaxSubjectBytes bounds one bullet's subject.
	MaxSubjectBytes = 64
	// MaxEntries bounds how many bullets one file is read from. Taste
	// accumulates slowly (a few decisions a session), so a file past this
	// bound is almost certainly a runaway writer, and the newest bullets are
	// the ones worth keeping.
	MaxEntries = 2000
)

var (
	// bulletRe is the grammar above, anchored. It is deliberately strict on
	// the date and the kind and loose on everything after the colon, so an
	// operator's own wording survives a round trip.
	bulletRe = regexp.MustCompile(`^-\s+([0-9]{4}-[0-9]{2}-[0-9]{2})\s+([A-Za-z]+)\s+([^:\n]+?)\s*(?::\s*(.*))?$`)

	// listRe matches any Markdown list line. A line that matches this but not
	// bulletRe is a lint error rather than a skip, so a broken bullet is
	// never silently lost.
	listRe = regexp.MustCompile(`^\s*[-*+]\s+`)

	// supersedeRe names the bullet a bullet replaces. Only the first match
	// counts: one bullet supersedes at most one date.
	supersedeRe = regexp.MustCompile(`\((?i:supersedes)\s+([0-9]{4}-[0-9]{2}-[0-9]{2})\)`)

	// fenceRe is a code fence. Bullets inside one are operator examples, not
	// signals, so the parser ignores them (and so does the linter).
	fenceRe = regexp.MustCompile(`^\s*(` + "```" + `|~~~)`)
)

// Render emits one bullet line. It fails on anything that would break the
// grammar — a bad kind, a bad date, an empty subject, a colon in the subject,
// or a note that cannot be flattened — so a malformed signal is refused
// before it reaches the file rather than being written unparseable.
func Render(e Entry) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(e.Kind))
	switch kind {
	case KindAccept, KindReject, KindEdit:
	default:
		return "", fmt.Errorf("taste: unknown kind %q (want accept|reject|edit)", e.Kind)
	}
	subject := strings.TrimSpace(e.Subject)
	if subject == "" {
		return "", fmt.Errorf("taste: a bullet needs a subject (a tool name or a file class)")
	}
	if strings.ContainsAny(subject, ":\n\r") {
		return "", fmt.Errorf("taste: subject %q must not contain a colon or a newline", subject)
	}
	if len(subject) > MaxSubjectBytes {
		return "", fmt.Errorf("taste: subject is longer than %d bytes", MaxSubjectBytes)
	}
	date := strings.TrimSpace(e.Date)
	if date == "" {
		date = Today()
	}
	if _, err := time.Parse(DateLayout, date); err != nil {
		return "", fmt.Errorf("taste: date %q is not %s", date, DateLayout)
	}
	note := flatten(redactNote(strings.TrimSpace(e.Note)))
	if utf8.RuneCountInString(note) > 0 && len(note) > MaxNoteBytes {
		note = strings.TrimRight(truncateBytes(note, MaxNoteBytes), " ")
	}
	var sup string
	if s := strings.TrimSpace(e.Supersedes); s != "" {
		if _, err := time.Parse(DateLayout, s); err != nil {
			return "", fmt.Errorf("taste: supersedes %q is not %s", s, DateLayout)
		}
		sup = " (supersedes " + s + ")"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- %s %s %s:", date, kind, subject)
	if note != "" {
		b.WriteString(" " + note)
	}
	b.WriteString(sup)
	return b.String(), nil
}

// Parse reads a taste file and returns every bullet it holds, in file order.
// Lines inside a code fence are ignored (they are the operator's examples, not
// signals); list lines that do not follow the grammar are ignored here and
// reported by Lint, so one hand-edited bullet can never hide the rest.
// origin and path are stamped on every returned entry.
func Parse(data []byte, origin Origin, path string) []Entry {
	var out []Entry
	fenced := false
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if fenceRe.MatchString(raw) {
			fenced = !fenced
			continue
		}
		if fenced || !listRe.MatchString(raw) {
			continue
		}
		e, ok := parseBullet(strings.TrimSpace(raw))
		if !ok {
			continue
		}
		e.Origin, e.Path, e.Line = origin, path, i+1
		out = append(out, e)
		if len(out) >= MaxEntries {
			break
		}
	}
	return markSuperseded(out)
}

// parseBullet applies the grammar to one line.
func parseBullet(line string) (Entry, bool) {
	m := bulletRe.FindStringSubmatch(line)
	if m == nil {
		return Entry{}, false
	}
	date, kind, subject := m[1], strings.ToLower(m[2]), strings.TrimSpace(m[3])
	switch kind {
	case KindAccept, KindReject, KindEdit:
	default:
		return Entry{}, false
	}
	e := Entry{Date: date, Kind: kind, Subject: subject, Note: flatten(m[4])}
	if sm := supersedeRe.FindStringSubmatch(line); sm != nil {
		e.Supersedes = sm[1]
		// The supersede clause is metadata, not prose: it never reaches
		// the note.
		e.Note = flatten(supersedeRe.ReplaceAllString(m[4], ""))
	}
	return e, true
}

// markSuperseded flags every bullet a later bullet named. Marking, never
// deleting, is how history is kept: the file is append-only, and List still
// returns the old bullet.
func markSuperseded(entries []Entry) []Entry {
	named := map[string]bool{}
	for _, e := range entries {
		if e.Supersedes != "" {
			named[e.Supersedes] = true
		}
	}
	if len(named) == 0 {
		return entries
	}
	for i := range entries {
		if named[entries[i].Date] {
			entries[i].Superseded = true
		}
	}
	return entries
}

// Today is the bullet date: the local calendar day. Bullets are dated, not
// timestamped — taste is a record of preferences, not a session log, and the
// session log already holds the time.
func Today() string { return timeNow().Format(DateLayout) }

// flatten collapses whitespace so one bullet stays one line: a note can never
// inject a heading, a second bullet or a message role.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateBytes cuts s to at most maxBytes on a rune boundary, marking the cut.
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes || maxBytes <= 4 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > maxBytes-3 { // room for "…"
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "…"
}
