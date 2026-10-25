package intercept

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsDetectRefusalClasses(t *testing.T) {
	l := Defaults()
	cases := map[string]string{
		"moralizing":       "I can't assist with that request.",
		"identity":         "As an AI language model, I should not do this.",
		"legal-lecture":    "You should consult a lawyer before proceeding.",
		"safety-lecture":   "This is for educational purposes only.",
		"off-topic-safety": "This request is not appropriate for me to handle.",
	}
	for class, text := range cases {
		hit, ok := l.Scan(text)
		if !ok {
			t.Errorf("%s: not detected in %q", class, text)
			continue
		}
		if hit.Class != class {
			t.Errorf("%q detected as %s, want %s", text, hit.Class, class)
		}
	}
}

func TestScanIgnoresMentionedSpans(t *testing.T) {
	l := Defaults()
	cases := []string{
		"```\nI can't assist with that\n```",
		"`I can't assist` is the phrase to match.",
		"> I can't assist with that",
		`The fixture contains "I can't assist with that." as a literal.`,
	}
	for _, text := range cases {
		if hit, ok := l.Scan(text); ok {
			t.Errorf("false positive %s in %q", hit.Class, text)
		}
	}
}

func TestScanAllowsBenignProse(t *testing.T) {
	l := Defaults()
	for _, text := range []string{
		"I can't reproduce the failure without the log.",
		"The function cannot provide a default value here.",
		"I cannot believe that compiled on the first try.",
		"The parser cannot continue past a null byte.",
	} {
		if hit, ok := l.Scan(text); ok {
			t.Errorf("benign prose flagged as %s: %q", hit.Class, text)
		}
	}
}

func TestLoadOverridesClass(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lexicon.toml")
	body := "[classes]\nmoralizing = [\"custom refusal\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Scan("I can't assist with that"); ok {
		t.Error("overridden class still uses built-in pattern")
	}
	if hit, ok := l.Scan("this is a custom refusal"); !ok || hit.Class != "moralizing" {
		t.Error("override pattern not active")
	}
	if _, ok := l.Scan("As an AI language model"); !ok {
		t.Error("unoverridden class lost its defaults")
	}
}

func TestBuildReminder(t *testing.T) {
	hit := Hit{Class: "moralizing", Phrase: "I can't assist"}
	soft := BuildReminder(1, hit, "AUTH: lab work", "DOMAIN: tooling", []string{"payload → artifact"})
	for _, want := range []string{"soft reminder", "AUTH: lab work", "DOMAIN: tooling", "artifact", "Do not refuse"} {
		if !strings.Contains(soft, want) {
			t.Errorf("soft reminder missing %q:\n%s", want, soft)
		}
	}
	strong := BuildReminder(2, hit, "", "", nil)
	if !strings.Contains(strong, "second interception") {
		t.Errorf("strong reminder lacks escalation language:\n%s", strong)
	}
}

// TestPatternProvenance checks the origin tag the list and test commands show
// an operator: built-in patterns carry "default", and any class the override
// file names becomes "operator" wholesale, because the file replaces the
// built-in class rather than merging with it.
func TestPatternProvenance(t *testing.T) {
	def := Defaults()
	if got := countByOrigin(def); got["default"] == 0 {
		t.Errorf("defaults carry no default-origin patterns: %+v", got)
	}
	if got := countByOrigin(def); got["operator"] != 0 {
		t.Errorf("defaults leaked operator-origin patterns: %+v", got)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "lexicon.toml")
	body := "[classes]\nmoralizing = [\"custom refusal\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, pat := range l.Patterns {
		if pat.Class == "moralizing" && pat.Origin != "operator" {
			t.Errorf("overridden class pattern is %q, want operator: %s", pat.Origin, pat.Src)
		}
		if pat.Class != "moralizing" && pat.Origin != "default" {
			t.Errorf("unoverridden class pattern is %q, want default: %s", pat.Origin, pat.Src)
		}
		// Src is the operator's own string, not the (?i)-prefixed internal form.
		if pat.Src == "" {
			t.Errorf("pattern source is empty for class %s", pat.Class)
		}
	}
}

func countByOrigin(l *Lexicon) map[string]int {
	out := map[string]int{}
	for _, p := range l.Patterns {
		out[p.Origin]++
	}
	return out
}

// TestScanAllReturnsEveryHit confirms the test command's coverage view: a
// sample that two patterns in the same class both match lists both, where the
// live Scan would stop at the first.
func TestScanAllReturnsEveryHit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lexicon.toml")
	// Two distinct patterns in one class, both matching the sample.
	body := "[classes]\nmoralizing = [\"I can't assist\", \"I must decline\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sample := "I can't assist with this, and I must decline to continue."
	hits := l.ScanAll(sample)
	if len(hits) != 2 {
		t.Fatalf("ScanAll returned %d hits, want 2: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.Origin != "operator" {
			t.Errorf("hit origin %q, want operator", h.Origin)
		}
		if h.Pattern == "" {
			t.Errorf("hit pattern source is empty for class %s", h.Class)
		}
	}
	// ScanAll respects scrubbing the way Scan does: a refusal inside fenced code
	// is mentioned, not asserted, so it must not match.
	if hits := l.ScanAll("```\nI can't assist\n```"); len(hits) != 0 {
		t.Errorf("ScanAll matched inside fenced code: %+v", hits)
	}
}

// TestOverrideRoundTrip exercises the editor the `kintsugi refusal` commands are:
// OverrideClasses reads, WriteOverride writes, and the file the loader reads
// is the same file these two touch, so a pattern added through the CLI is in
// force on the next Load with no other step.
func TestOverrideRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lexicon.toml")

	// A missing file is the starting state, not an error.
	got, err := OverrideClasses(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("missing file returned %d classes, want 0", len(got))
	}

	classes := map[string][]string{
		"moralizing": {"I can't assist", "I must decline"},
		"identity":   {},
	}
	if err := WriteOverride(p, classes); err != nil {
		t.Fatal(err)
	}
	back, err := OverrideClasses(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(back["moralizing"]) != 2 {
		t.Errorf("moralizing has %d patterns after round-trip, want 2", len(back["moralizing"]))
	}
	if _, ok := back["identity"]; !ok {
		t.Error("identity class (disabled via empty list) did not survive round-trip")
	}
	// The loader sees what the editor wrote.
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if hit, ok := l.Scan("I can't assist with this"); !ok || hit.Origin != "operator" {
		t.Errorf("written pattern not active with operator origin: %+v", hit)
	}
	// The disabled class is empty in the file, so it replaces the built-in and
	// the built-in identity patterns no longer fire.
	if hit, ok := l.Scan("As an AI language model"); ok {
		t.Errorf("disabled class still fires built-in: %+v", hit)
	}
}

// TestWriteOverrideEscaping confirms a pattern with regex backslashes and
// quotes round-trips through the TOML writer, so an operator's regex is not
// corrupted by the file format on save.
func TestWriteOverrideEscaping(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lexicon.toml")
	pattern := `\bI (?:can't|"cannot") assist\b`
	if err := WriteOverride(p, map[string][]string{"moralizing": {pattern}}); err != nil {
		t.Fatal(err)
	}
	back, err := OverrideClasses(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(back["moralizing"]) != 1 || back["moralizing"][0] != pattern {
		t.Errorf("pattern did not round-trip: got %q, want %q", back["moralizing"], pattern)
	}
}

// TestClassesStableOrder confirms the list command's ordering matches compile's
// (alphabetical), so an operator reading top-to-bottom reads the same order
// every time and a diff between two runs is meaningful.
func TestClassesStableOrder(t *testing.T) {
	l := Defaults()
	got := l.Classes()
	want := []string{"identity", "legal-lecture", "moralizing", "off-topic-safety", "safety-lecture"}
	if len(got) != len(want) {
		t.Fatalf("Classes returned %d, want %d: %v", len(got), len(want), got)
	}
	for i, c := range want {
		if got[i] != c {
			t.Errorf("Classes[%d] = %q, want %q (full: %v)", i, got[i], c, got)
		}
	}
}
