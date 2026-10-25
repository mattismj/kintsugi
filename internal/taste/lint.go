package taste

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Lint is `kintsugi taste lint`: the grammar check an operator runs before
// trusting the injected block. It never rewrites the file — taste is
// append-only, so a broken bullet is reported and the operator decides.
//
// Two severities:
//
//	error    the line starts a list item but is not a bullet: no date, no
//	         kind, an unknown kind, a subject with a colon, or a supersede
//	         clause naming a date no bullet in the file carries.
//	warning  the store is readable but the hygiene is off: the file mode is
//	         not 0600, a bullet is dated in the future, or a bullet's date is
//	         not a real calendar day.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is one lint result. Line is 1-based; Origin is the store it came
// from.
type Finding struct {
	Severity Severity `json:"severity"`
	Origin   Origin   `json:"origin"`
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Message  string   `json:"message"`
	// Text is the offending line, trimmed. It is the operator's own file, so
	// quoting it back is not a leak.
	Text string `json:"text,omitempty"`
}

// String is the one-line CLI rendering of a finding:
//
//	error  <path>:<line>: <message>
//	  | <the offending line>
func (f Finding) String() string {
	loc := f.Path
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
	line := fmt.Sprintf("%-7s %s: %s", f.Severity, loc, f.Message)
	if f.Text != "" {
		line += "\n        | " + f.Text
	}
	return line
}

// Errors returns only the error-severity findings. `kintsugi taste lint` exits
// non-zero when this is non-empty.
func Errors(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}

// lintOrigin checks one store. A missing file lints clean: an empty store has
// nothing to get wrong.
func lintOrigin(path string, origin Origin) ([]Finding, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("taste: %w", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("taste: %w", err)
	}

	var findings []Finding
	if mode := info.Mode().Perm(); mode != 0o600 {
		findings = append(findings, Finding{
			Severity: SeverityWarning, Origin: origin, Path: path,
			Message: fmt.Sprintf("file mode is %04o, want 0600 (taste is the operator's, not the repository's)", mode),
		})
	}

	entries := Parse(b, origin, path)
	dates := map[string]bool{}
	for _, e := range entries {
		if e.Date != "" {
			dates[e.Date] = true
		}
	}

	fenced := false
	for i, raw := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(raw)
		if fenceRe.MatchString(raw) {
			fenced = !fenced
			continue
		}
		if fenced || !listRe.MatchString(raw) {
			continue
		}
		n := i + 1
		if e, ok := parseBullet(trimmed); ok {
			findings = append(findings, lintEntry(e, origin, path, n, trimmed, dates)...)
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityError, Origin: origin, Path: path, Line: n,
			Message: "not a taste bullet: want \"- <YYYY-MM-DD> <accept|reject|edit> <subject>: <note>\"",
			Text:    trimmed,
		})
	}
	return findings, nil
}

// lintEntry checks one parsed bullet: the date must be a real day and not in
// the future, and a supersede clause must name a date the file actually
// carries (otherwise the operator's changed mind points at nothing).
func lintEntry(e Entry, origin Origin, path string, n int, text string, dates map[string]bool) []Finding {
	var out []Finding
	loc := Finding{Origin: origin, Path: path, Line: n, Text: text}

	day, err := time.Parse(DateLayout, e.Date)
	if err != nil {
		loc.Severity, loc.Message = SeverityError, "the date is not a real calendar day ("+DateLayout+")"
		return append(out, loc)
	}
	if day.After(timeNow().AddDate(0, 0, 1)) {
		w := loc
		w.Severity, w.Message = SeverityWarning, "the bullet is dated in the future"
		out = append(out, w)
	}
	if e.Supersedes != "" && !dates[e.Supersedes] {
		w := loc
		w.Severity = SeverityError
		w.Message = fmt.Sprintf("supersedes %s, but no bullet in this file carries that date", e.Supersedes)
		out = append(out, w)
	}
	if e.Subject == "" {
		loc.Severity, loc.Message = SeverityError, "the bullet has no subject (a tool name or a file class)"
		out = append(out, loc)
	}
	return out
}
