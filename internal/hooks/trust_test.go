package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/config"
)

// writeHooksFile writes the workspace's project hook file.
func writeHooksFile(t *testing.T, ws, body string) string {
	t.Helper()
	path := ProjectHooksPath(ws)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTrustMarkerLifecycle pins the marker contract: absent means untrusted,
// Trust writes it, Untrust removes it, and revoking twice is not an error.
func TestTrustMarkerLifecycle(t *testing.T) {
	ws := t.TempDir()

	if got, want := ProjectHooksPath(ws), filepath.Join(ws, ".kintsugi", "hooks.toml"); got != want {
		t.Fatalf("project hooks path = %q, want %q", got, want)
	}
	if got, want := TrustedMarkerPath(ws), filepath.Join(ws, ".kintsugi", "hooks-trusted"); got != want {
		t.Fatalf("trust marker path = %q, want %q", got, want)
	}
	if IsTrusted(ws) {
		t.Fatal("a fresh workspace is trusted")
	}

	if err := Trust(ws); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if !IsTrusted(ws) {
		t.Fatal("the marker did not make the workspace trusted")
	}
	info, err := os.Stat(TrustedMarkerPath(ws))
	if err != nil {
		t.Fatalf("the marker is not a file: %v", err)
	}
	if info.IsDir() {
		t.Fatal("the marker is a directory")
	}
	if info.ModTime().IsZero() {
		t.Fatal("the marker carries no mtime")
	}
	if TrustMarkerTime(ws).IsZero() {
		t.Fatal("TrustMarkerTime reports no time for a trusted workspace")
	}

	if err := Untrust(ws); err != nil {
		t.Fatalf("Untrust: %v", err)
	}
	if IsTrusted(ws) {
		t.Fatal("the marker was not removed")
	}
	if err := Untrust(ws); err != nil {
		t.Fatalf("Untrust on a workspace with no marker must not fail: %v", err)
	}
}

// TestTrustIsPerWorkspace proves the marker is a decision about one tree, not
// about the machine: trusting one workspace leaves another untrusted.
func TestTrustIsPerWorkspace(t *testing.T) {
	trusted := t.TempDir()
	other := t.TempDir()
	if err := Trust(trusted); err != nil {
		t.Fatal(err)
	}
	if !IsTrusted(trusted) {
		t.Fatal("the trusted workspace is not trusted")
	}
	if IsTrusted(other) {
		t.Fatal("trust leaked into another workspace")
	}
}

// TestLoadProjectHooksReadsTheTOMLShape covers the project hook file: the
// same tables as the config's [hooks] section.
func TestLoadProjectHooksReadsTheTOMLShape(t *testing.T) {
	ws := t.TempDir()
	writeHooksFile(t, ws, `
[[PreToolUse]]
matcher = "bash"
command = "./scripts/check.sh"
timeout = 30
async = true
on_failure = "block"

[[SessionStart]]
command = "./scripts/welcome.sh"
`)

	rules, trusted, err := LoadProjectHooks(ws)
	if err != nil {
		t.Fatalf("LoadProjectHooks: %v", err)
	}
	if trusted {
		t.Fatal("the workspace is trusted without a marker")
	}
	pre := rules[PreToolUse]
	if len(pre) != 1 {
		t.Fatalf("PreToolUse rules = %+v, want one", pre)
	}
	if pre[0].Matcher != "bash" || pre[0].Command != "./scripts/check.sh" {
		t.Fatalf("rule = %+v", pre[0])
	}
	if pre[0].Timeout != 30 || !pre[0].Async || pre[0].OnFailure != "block" {
		t.Fatalf("rule = %+v", pre[0])
	}
	if len(rules[SessionStart]) != 1 || rules[SessionStart][0].Command != "./scripts/welcome.sh" {
		t.Fatalf("SessionStart rules = %+v", rules[SessionStart])
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %+v, want two events", rules)
	}
}

// TestLoadProjectHooksAcceptsTheHooksSection proves the file may also use the
// config's own layout, with the tables under a [hooks] header.
func TestLoadProjectHooksAcceptsTheHooksSection(t *testing.T) {
	ws := t.TempDir()
	writeHooksFile(t, ws, `
[hooks]
[[hooks.PreToolUse]]
matcher = "bash"
command = "./scripts/check.sh"
`)

	rules, _, err := LoadProjectHooks(ws)
	if err != nil {
		t.Fatalf("LoadProjectHooks: %v", err)
	}
	if len(rules[PreToolUse]) != 1 || rules[PreToolUse][0].Command != "./scripts/check.sh" {
		t.Fatalf("PreToolUse rules = %+v", rules[PreToolUse])
	}
}

// TestLoadProjectHooksWithoutAFile proves a missing hook file is not an
// error: there is simply nothing to run.
func TestLoadProjectHooksWithoutAFile(t *testing.T) {
	ws := t.TempDir()
	rules, trusted, err := LoadProjectHooks(ws)
	if err != nil {
		t.Fatalf("LoadProjectHooks: %v", err)
	}
	if trusted {
		t.Fatal("the workspace is trusted without a marker")
	}
	if len(rules) != 0 {
		t.Fatalf("rules = %+v, want none", rules)
	}
}

// TestMalformedProjectHooksError proves a malformed hook file fails loudly
// and changes nothing: the file is left exactly as it was.
func TestMalformedProjectHooksError(t *testing.T) {
	ws := t.TempDir()
	const body = "[[PreToolUse]\ncommand = \n"
	path := writeHooksFile(t, ws, body)

	rules, trusted, err := LoadProjectHooks(ws)
	if err == nil {
		t.Fatal("a malformed hook file was accepted")
	}
	if !strings.Contains(err.Error(), "hooks.toml") {
		t.Fatalf("error = %v, want it to name the file", err)
	}
	if rules != nil {
		t.Fatalf("rules = %+v, want none on a malformed file", rules)
	}
	if trusted {
		t.Fatal("a malformed file changed the trust state")
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the malformed file was removed: %v", rerr)
	}
	if string(raw) != body {
		t.Fatalf("the malformed file was rewritten: %q", raw)
	}
}

// TestUntrustedProjectRulesAreSkippedAndLogged is the trust boundary itself:
// a repository's hook command does not run in a workspace the operator has
// not trusted, and the attempt is visible in the log.
func TestUntrustedProjectRulesAreSkippedAndLogged(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	rec := &recorder{}

	r := NewWithProject(nil, map[string][]config.HookRule{
		PreToolUse: {{Matcher: "bash", Command: "sh", Args: []string{"-c", "touch " + ran}}},
	}, false, rec.Log)

	dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	if err != nil {
		t.Fatalf("a skipped project hook is not an error: %v", err)
	}
	if dec.Decision != "" {
		t.Fatalf("decision = %+v, want none", dec)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the project hook ran in an untrusted workspace")
	}
	if !rec.hasKind("hook_trust_denied") {
		t.Fatalf("the skipped attempt was not logged as hook_trust_denied: %+v", rec.all())
	}
	for _, rec := range rec.all() {
		if rec["event"] != PreToolUse {
			t.Fatalf("the denial record does not name the event: %+v", rec)
		}
	}
}

// TestTrustedProjectRulesRun proves the other half of the boundary: once the
// workspace is trusted, the project's hook runs and its decision counts.
func TestTrustedProjectRulesRun(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	rec := &recorder{}

	r := NewWithProject(nil, map[string][]config.HookRule{
		PreToolUse: {{
			Matcher: "bash",
			Command: "sh",
			Args:    []string{"-c", "touch " + ran + `; printf '%s' '{"decision":"deny","reason":"not here"}'`},
		}},
	}, true, rec.Log)

	dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the project hook did not run in a trusted workspace: %v", err)
	}
	if dec.Decision != "deny" || dec.Reason != "not here" {
		t.Fatalf("decision = %+v, want the project's deny", dec)
	}
	if rec.hasKind("hook_trust_denied") {
		t.Fatal("a trusted workspace logged a trust denial")
	}
}

// TestOperatorRulesRunWithoutTrust proves the operator's own hooks never need
// a marker: only the project's rules are gated.
func TestOperatorRulesRunWithoutTrust(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	rec := &recorder{}

	r := NewWithProject(map[string][]config.HookRule{
		PreToolUse: {{Matcher: "bash", Command: "sh", Args: []string{"-c", "touch " + ran}}},
	}, map[string][]config.HookRule{
		PreToolUse: {{Matcher: "bash", Command: "sh", Args: []string{"-c", "touch " + ran + ".project"}}},
	}, false, rec.Log)

	if _, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatal("the operator's hook did not run")
	}
	if _, err := os.Stat(ran + ".project"); err == nil {
		t.Fatal("the project's hook ran without trust")
	}
	if !rec.hasKind("hook_trust_denied") {
		t.Fatal("the skipped project attempt was not logged")
	}
}

// TestTrustDenialIsLoggedOncePerEvent proves the denial record names the
// event and the rule count, and carries no payload.
func TestTrustDenialIsLoggedOncePerEvent(t *testing.T) {
	rec := &recorder{}
	r := NewWithProject(nil, map[string][]config.HookRule{
		PreToolUse: {
			{Command: "sh", Args: []string{"-c", "true"}},
			{Command: "sh", Args: []string{"-c", "true"}},
		},
	}, false, rec.Log)

	_, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	if err != nil {
		t.Fatal(err)
	}
	records := rec.all()
	if len(records) != 1 {
		t.Fatalf("log records = %d, want one denial", len(records))
	}
	if records[0]["rules"] != 2 {
		t.Fatalf("the denial does not count the skipped rules: %+v", records[0])
	}
	if _, ok := records[0]["command"]; ok {
		t.Fatalf("the denial record carries a command: %+v", records[0])
	}
}

// TestTrustMarkerAloneRunsNothing proves the marker is a permission and not a
// rule: trusting a workspace with no hook file runs nothing.
func TestTrustMarkerAloneRunsNothing(t *testing.T) {
	ws := t.TempDir()
	if err := Trust(ws); err != nil {
		t.Fatal(err)
	}
	rules, trusted, err := LoadProjectHooks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted {
		t.Fatal("the workspace is not trusted")
	}
	if len(rules) != 0 {
		t.Fatalf("rules = %+v, want none", rules)
	}

	rec := &recorder{}
	r := NewWithProject(nil, rules, trusted, rec.Log)
	dec, err := r.Fire(context.Background(), Event{Name: PreToolUse, ToolName: "bash"})
	if err != nil || dec.Decision != "" {
		t.Fatalf("a trusted workspace with no rules fired something: %+v / %v", dec, err)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("log records = %+v, want none", rec.all())
	}
}
