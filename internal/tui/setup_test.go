package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/plan"
)

// ---------- the readiness ladder ----------

func TestLadderNamesTheFixForEveryGap(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Wide enough that the fix text is not the thing being truncated: the
	// narrow case is tested separately below.
	m := testModel(160, 40)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: t.TempDir()}

	out := strings.Join(m.gettingStarted(), "\n")
	for _, want := range []string{"getting started", "model", "context", "provider key"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ladder missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "kintsugi context init") {
		t.Fatalf("the context gap names no command:\n%s", out)
	}
	// Every ✗ row must carry a fix on its own row.
	for _, line := range m.gettingStarted() {
		if strings.Contains(line, "✗") && !strings.Contains(line, "→") {
			t.Fatalf("a gap row names no fix: %q", line)
		}
	}
}

// At the smallest supported terminal the ladder still fits and still says what
// to do: truncation may shorten the value, but the arrow that introduces the
// fix must survive, or the row becomes a nag.
func TestLadderFitsAt80Columns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := testModel(80, 24)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: t.TempDir()}

	for _, line := range m.gettingStarted() {
		if strings.Contains(line, "✗") && !strings.Contains(line, "→") {
			t.Fatalf("at 80 cols a gap row lost its fix: %q", line)
		}
	}
}

func TestLadderCollapsesWhenEverythingIsConfigured(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: ws}
	// A loopback provider needs no key, the model is resolved, and the context
	// is complete: there is nothing left to do.
	m.res.ProviderCfg.Offline = true
	if err := writeContext(t, ws); err != nil {
		t.Fatal(err)
	}

	lines := m.gettingStarted()
	if len(lines) != 1 {
		t.Fatalf("a fully configured workspace should collapse to one line, got %d:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "all set") {
		t.Fatalf("collapsed line = %q", lines[0])
	}
}

func TestLadderMarksASatisfiedStep(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: ws}
	if err := writeContext(t, ws); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(m.gettingStarted(), "\n")
	// The model resolves in the fixture and the context is written; the key is
	// the one that cannot be satisfied in a test.
	for _, line := range m.gettingStarted() {
		if strings.Contains(line, "context") && !strings.Contains(line, "✓") {
			t.Fatalf("a satisfied context step is not marked done: %q\n%s", line, out)
		}
	}
}

func writeContext(t *testing.T, ws string) error {
	t.Helper()
	// CONTEXT.md lives at the workspace root: that is what App.ContextPath
	// resolves, and what `kintsugi context init` writes. The two sections are
	// classified by title, so they must be named the way classify() expects:
	// "authorization" and "domain" are the two classes the readiness check reads.
	doc := "# Engagement\n\n" +
		"## Authorization\n\n- authorised against 10.0.0.0/8\n\n" +
		"## Domain context\n\n- internal lab, no third-party data\n"
	return os.WriteFile(filepath.Join(ws, "CONTEXT.md"), []byte(doc), 0o644)
}

// ---------- the derived next action ----------

func TestNextActionLeadsWithTheFirstGap(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: t.TempDir()}
	m.res.ProviderCfg.Offline = true

	// The context is the first thing missing here.
	next := m.nextAction()
	if !strings.Contains(next, "context") {
		t.Fatalf("next action = %q, want the context gap first", next)
	}

	if err := writeContext(t, m.app.Workspace); err != nil {
		t.Fatal(err)
	}
	if got := m.nextAction(); strings.Contains(got, "context") {
		t.Fatalf("next action still names a satisfied step: %q", got)
	}
}

func TestNextActionPointsAtAnUnreviewedPlan(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: ws}
	m.res.ProviderCfg.Offline = true
	if err := writeContext(t, ws); err != nil {
		t.Fatal(err)
	}
	art, err := plan.Save(ws, "Add health check", "# Add health check\n\n1. step\n")
	if err != nil {
		t.Fatal(err)
	}

	next := m.nextAction()
	if !strings.Contains(next, "review plan") || !strings.Contains(next, art.Name) {
		t.Fatalf("next action = %q, want it to point at the unreviewed plan", next)
	}

	// Once the verdict is recorded the plan stops being the next thing.
	if _, err := plan.SetVerdict(ws, art.Name, plan.VerdictApprove, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.nextAction(); strings.Contains(got, "review plan") {
		t.Fatalf("a decided plan is still suggested: %q", got)
	}
}

func TestStatusLeadsWithTheNextAction(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := testModel(120, 34)
	m.app = &harness.App{Cfg: config.Defaults(), Workspace: t.TempDir()}
	m.res.ProviderCfg.Offline = true

	m.handleSlash("/status")
	card := m.blocks[len(m.blocks)-1].text
	first := strings.SplitN(card, "\n", 2)[0]
	if !strings.Contains(first, "→ next:") {
		t.Fatalf("the status card does not lead with the next action: %q\n%s", first, card)
	}
	// The stats are still there, below.
	if !strings.Contains(card, "session ") || !strings.Contains(card, "tokens") {
		t.Fatalf("the status card lost its detail:\n%s", card)
	}
}

// ---------- grouped help ----------

func TestHelpMenuIsShortAndNamesTheGroups(t *testing.T) {
	m := testModel(120, 34)
	m.handleSlash("/help")
	card := m.blocks[len(m.blocks)-1].text
	for _, g := range []string{"session", "context", "models", "permissions", "review"} {
		if !strings.Contains(card, g) {
			t.Fatalf("the help menu is missing the %q group:\n%s", g, card)
		}
	}
	// A menu, not the manual: it must not enumerate the commands themselves.
	if strings.Contains(card, "/rewind") || strings.Contains(card, "/ledger") {
		t.Fatalf("the menu is dumping commands instead of grouping them:\n%s", card)
	}
	if n := len(strings.Split(card, "\n")); n > 12 {
		t.Fatalf("the help menu is %d lines; it is meant to be a short index:\n%s", n, card)
	}
}

func TestHelpGroupBody(t *testing.T) {
	m := testModel(120, 34)
	for _, g := range []string{"session", "context", "models", "permissions", "review"} {
		m.handleSlash("/help " + g)
		card := m.blocks[len(m.blocks)-1].text
		if !strings.Contains(card, "/") {
			t.Fatalf("/help %s listed no commands:\n%s", g, card)
		}
	}
	// A copied command works too.
	m.handleSlash("/help /context")
	if card := m.blocks[len(m.blocks)-1].text; !strings.Contains(card, "/context") {
		t.Fatalf("/help /context did not resolve:\n%s", card)
	}
}

func TestHelpUnknownGroupNamesTheValidOnes(t *testing.T) {
	m := testModel(120, 34)
	m.handleSlash("/help nope")
	card := m.blocks[len(m.blocks)-1].text
	if !strings.Contains(card, "no group") {
		t.Fatalf("an unknown group is not reported:\n%s", card)
	}
	for _, g := range []string{"session", "context", "models", "permissions", "review"} {
		if !strings.Contains(card, g) {
			t.Fatalf("the error does not name %q:\n%s", g, card)
		}
	}
}

// helpSwitchCases derives the built-in command names out of the handleSlash
// switch. It is the same discipline the docs-completeness test uses: derive the
// list from the code rather than restating it, so a new command cannot ship
// undocumented.
func helpSwitchCases(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("update.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (m *Model) handleSlash(")
	if start < 0 {
		t.Fatal("handleSlash not found")
	}
	// Walk to the matching closing brace of the function.
	depth := 0
	end := start
	for i := start; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end > start {
			break
		}
	}
	if end <= start {
		t.Fatal("could not delimit handleSlash")
	}
	re := regexp.MustCompile(`case "/([a-z]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body[start:end], -1) {
		out = append(out, "/"+m[1])
	}
	sort.Strings(out)
	return out
}

func TestEveryBuiltinCommandIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, name := range helpCommandNames() {
		documented[name] = true
	}
	for _, cmd := range helpSwitchCases(t) {
		// /q is the alias of /quit and is documented by its long form.
		if cmd == "/q" {
			continue
		}
		if !documented[cmd] {
			t.Errorf("%s is a built-in command but appears in no help group", cmd)
		}
	}
}

func TestNoHelpGroupListsACommandThatDoesNotExist(t *testing.T) {
	real := map[string]bool{}
	for _, cmd := range helpSwitchCases(t) {
		real[cmd] = true
	}
	for _, name := range helpCommandNames() {
		if !real[name] {
			t.Errorf("help documents %q, which handleSlash does not implement", name)
		}
	}
}
