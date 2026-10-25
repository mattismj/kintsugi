package taste

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintRejectsABulletWithoutADate(t *testing.T) {
	ws := isolate(t)
	writeStore(t, ws, OriginProject, `# Taste (project)

- accept bash: no date on this one
- 2026-03-04 accept bash: this one is fine
`)

	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	errs := Errors(findings)
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1:\n%s", len(errs), report(findings))
	}
	if !strings.Contains(errs[0].Text, "- accept bash") {
		t.Fatalf("the finding does not name the line: %+v", errs[0])
	}
	if errs[0].Line != 3 {
		t.Fatalf("line = %d, want 3", errs[0].Line)
	}
	if !strings.Contains(errs[0].Message, "want \"- <YYYY-MM-DD>") {
		t.Fatalf("message = %q, want the grammar", errs[0].Message)
	}
}

func TestLintRejectsABulletWithoutAKind(t *testing.T) {
	ws := isolate(t)
	writeStore(t, ws, OriginProject, `# Taste (project)

- 2026-03-04: a date and a note, but no kind
- 2026-03-04 maybe bash: an unknown kind
- 2026-03-04 reject bash: correct
`)

	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	errs := Errors(findings)
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want 2 (no kind, unknown kind):\n%s", len(errs), report(findings))
	}
	for _, f := range errs {
		if f.Line == 5 {
			t.Fatalf("the correct bullet was flagged: %+v", f)
		}
	}
}

func TestLintFlagsADanglingSupersede(t *testing.T) {
	ws := isolate(t)
	writeStore(t, ws, OriginProject, `# Taste (project)

- 2026-03-04 accept bash: fine (supersedes 2020-01-01)
`)
	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	errs := Errors(findings)
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "no bullet in this file carries that date") {
		t.Fatalf("errors = %+v, want the dangling supersede", errs)
	}

	// Naming a date the file does carry is clean.
	writeStore(t, ws, OriginProject, `# Taste (project)

- 2026-03-01 reject bash: no shell here
- 2026-03-04 accept bash: go test is fine (supersedes 2026-03-01)
`)
	findings, err = Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(Errors(findings)) != 0 {
		t.Fatalf("a valid supersede was flagged:\n%s", report(findings))
	}
}

func TestLintOnACleanStoreIsClean(t *testing.T) {
	ws := isolate(t)
	freezeDate(t, "2026-03-04")
	if _, err := Record(ws, false, KindAccept, "edit_file", "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(ws, true, KindReject, "bash", "denied"); err != nil {
		t.Fatal(err)
	}
	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("a machine-written store lints dirty:\n%s", report(findings))
	}
}

func TestLintOnAMissingStoreIsClean(t *testing.T) {
	ws := isolate(t)
	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("got %d findings for a workspace with no store", len(findings))
	}
}

func TestLintWarnsAboutALooseFileMode(t *testing.T) {
	ws := isolate(t)
	writeStore(t, ws, OriginProject, "# Taste (project)\n\n- 2026-03-04 accept bash: fine\n")
	if err := os.Chmod(ProjectPath(ws, Config{}), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(Errors(findings)) != 0 {
		t.Fatalf("a loose mode is an error, want a warning:\n%s", report(findings))
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Message, "0600") {
		t.Fatalf("findings = %+v, want the mode warning", findings)
	}
	if findings[0].Severity != SeverityWarning {
		t.Fatalf("severity = %q, want warning", findings[0].Severity)
	}
}

func TestLintWarnsAboutAFutureDate(t *testing.T) {
	ws := isolate(t)
	freezeDate(t, "2026-03-04")
	future := timeNow().AddDate(1, 0, 0).Format(DateLayout)
	writeStore(t, ws, OriginProject, "# Taste (project)\n\n- "+future+" accept bash: from the future\n")

	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(findings) != 1 || findings[0].Severity != SeverityWarning ||
		!strings.Contains(findings[0].Message, "future") {
		t.Fatalf("findings = %+v, want the future-date warning", findings)
	}
}

func TestLintReportsBothStores(t *testing.T) {
	ws := isolate(t)
	writeStore(t, ws, OriginProject, "# Taste (project)\n\n- broken project bullet\n")
	writeStore(t, ws, OriginGlobal, "# Taste (global)\n\n- broken global bullet\n")

	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want one per store:\n%s", len(findings), report(findings))
	}
	seen := map[Origin]bool{}
	for _, f := range findings {
		seen[f.Origin] = true
	}
	if !seen[OriginProject] || !seen[OriginGlobal] {
		t.Fatalf("origins = %+v, want project and global", seen)
	}
}

func TestLintIgnoresTheFileHeader(t *testing.T) {
	ws := isolate(t)
	freezeDate(t, "2026-03-04")
	if _, err := Record(ws, false, KindAccept, "bash", "fine"); err != nil {
		t.Fatal(err)
	}
	// The header itself contains the grammar in prose; it must not be read as
	// a bullet, broken or otherwise.
	body, err := os.ReadFile(ProjectPath(ws, Config{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "- <YYYY-MM-DD>") {
		t.Fatalf("the header changed shape:\n%s", body)
	}
	findings, err := Lint(ws)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("the header lints dirty:\n%s", report(findings))
	}
}

// writeStore replaces a store file with a fixed body, 0600, so a test can seed
// malformed bullets that the write path would refuse.
func writeStore(t *testing.T, ws string, origin Origin, body string) {
	t.Helper()
	var path string
	switch origin {
	case OriginGlobal:
		path = GlobalPath(Config{})
	default:
		path = ProjectPath(ws, Config{})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func report(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString(f.String())
		b.WriteString("\n")
	}
	return b.String()
}
