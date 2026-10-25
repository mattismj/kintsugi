package main

// Tests for `kintsugi hooks`. The command is thin: the trust marker, the rule
// listing and the status line are all checked against a real workspace and a
// real global config, without a model or a network.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/hooks"
)

// globalConfigWithHooks writes a global config with one operator hook and
// points XDG_CONFIG_HOME at it.
func globalConfigWithHooks(t *testing.T, body string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "kintsugi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// projectHooksFile writes a workspace project hook file.
func projectHooksFile(t *testing.T, ws, body string) {
	t.Helper()
	path := hooks.ProjectHooksPath(ws)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHooksTrustWritesTheMarker proves `kintsugi hooks trust` writes the marker
// and reports where it went.
func TestHooksTrustWritesTheMarker(t *testing.T) {
	ws := t.TempDir()

	out := captureStdout(t, func() { cmdHooks(ws, []string{"trust"}) })

	if !hooks.IsTrusted(ws) {
		t.Fatal("the command did not make the workspace trusted")
	}
	marker := hooks.TrustedMarkerPath(ws)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the marker was not written: %v", err)
	}
	if !strings.Contains(out, marker) {
		t.Fatalf("the output does not name the marker:\n%s", out)
	}
	if !strings.Contains(out, "kintsugi hooks list") {
		t.Fatalf("the output does not tell the operator to review the hooks:\n%s", out)
	}
	if !strings.Contains(out, hooks.ProjectHooksPath(ws)) {
		t.Fatalf("the output does not name the hook file it enables:\n%s", out)
	}
}

// TestHooksUntrustRemovesTheMarker proves the trust decision is revocable.
func TestHooksUntrustRemovesTheMarker(t *testing.T) {
	ws := t.TempDir()
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdHooks(ws, []string{"untrust"}) })

	if hooks.IsTrusted(ws) {
		t.Fatal("the marker was not removed")
	}
	if !strings.Contains(out, hooks.TrustedMarkerPath(ws)) {
		t.Fatalf("the output does not name the marker:\n%s", out)
	}
}

// TestHooksStatusReportsTheMarkerPath proves the status line says whether the
// workspace is trusted and where the marker lives.
func TestHooksStatusReportsTheMarkerPath(t *testing.T) {
	ws := t.TempDir()

	out := captureStdout(t, func() { cmdHooks(ws, []string{"status"}) })

	if !strings.Contains(out, hooks.TrustedMarkerPath(ws)) {
		t.Fatalf("the status does not name the marker:\n%s", out)
	}
	if !strings.Contains(out, hooks.ProjectHooksPath(ws)) {
		t.Fatalf("the status does not name the hook file:\n%s", out)
	}
	if !strings.Contains(out, "no") {
		t.Fatalf("the status does not report the untrusted state:\n%s", out)
	}

	// After trusting, the same command reports the trusted state.
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { cmdHooks(ws, []string{"status"}) })
	if !strings.Contains(out, "yes") {
		t.Fatalf("the status does not report the trusted state:\n%s", out)
	}
}

// TestHooksListShowsOperatorAndProjectHooks proves the listing carries both
// sources, the rule fields, and the trust state of the project's rules.
func TestHooksListShowsOperatorAndProjectHooks(t *testing.T) {
	globalConfigWithHooks(t, `
[[hooks.PreToolUse]]
matcher = "bash"
command = "./operator-check.sh"
timeout = 15
`)
	ws := t.TempDir()
	projectHooksFile(t, ws, `
[[PreToolUse]]
matcher = "read"
command = "./project-format.sh"
timeout = 30
async = true
`)

	out := captureStdout(t, func() { cmdHooks(ws, []string{"list"}) })

	// The operator's rule, with its fields.
	if !strings.Contains(out, "operator") {
		t.Fatalf("the listing has no operator column:\n%s", out)
	}
	if !strings.Contains(out, "./operator-check.sh") {
		t.Fatalf("the listing does not show the operator hook:\n%s", out)
	}
	if !strings.Contains(out, "matcher=bash") {
		t.Fatalf("the listing does not show the matcher:\n%s", out)
	}
	if !strings.Contains(out, "timeout=15") {
		t.Fatalf("the listing does not show the timeout:\n%s", out)
	}

	// The project's rule, with its fields and its trust state.
	if !strings.Contains(out, "project") {
		t.Fatalf("the listing has no project column:\n%s", out)
	}
	if !strings.Contains(out, "./project-format.sh") {
		t.Fatalf("the listing does not show the project hook:\n%s", out)
	}
	if !strings.Contains(out, "matcher=read") {
		t.Fatalf("the listing does not show the project matcher:\n%s", out)
	}
	if !strings.Contains(out, "timeout=30") {
		t.Fatalf("the listing does not show the project timeout:\n%s", out)
	}
	if !strings.Contains(out, "async=yes") {
		t.Fatalf("the listing does not show the async flag:\n%s", out)
	}
	if !strings.Contains(out, "untrusted") {
		t.Fatalf("the listing does not report the trust state:\n%s", out)
	}
	if !strings.Contains(out, "skipped") {
		t.Fatalf("the listing does not say the project hook is skipped:\n%s", out)
	}
	if !strings.Contains(out, hooks.ProjectHooksPath(ws)) {
		t.Fatalf("the listing does not name the project hook file:\n%s", out)
	}
}

// TestHooksListAfterTrustShowsTheProjectHookRunning proves the trust state
// in the listing follows the marker.
func TestHooksListAfterTrustShowsTheProjectHookRunning(t *testing.T) {
	globalConfigWithHooks(t, "")
	ws := t.TempDir()
	projectHooksFile(t, ws, `
[[PreToolUse]]
command = "./project-format.sh"
`)
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdHooks(ws, []string{"list"}) })

	if !strings.Contains(out, "trusted") {
		t.Fatalf("the listing does not report the trusted state:\n%s", out)
	}
	if strings.Contains(out, "skipped") {
		t.Fatalf("the listing skips a trusted project hook:\n%s", out)
	}
}

// TestHooksListWithoutAnyHookFile proves the command still answers when the
// workspace has no hook file at all.
func TestHooksListWithoutAnyHookFile(t *testing.T) {
	globalConfigWithHooks(t, "")
	ws := t.TempDir()

	out := captureStdout(t, func() { cmdHooks(ws, []string{"list"}) })

	if !strings.Contains(out, "(none)") {
		t.Fatalf("the listing does not report the empty events:\n%s", out)
	}
}
