package main

// Tests for `kintsugi mods`. The command is thin: the trust marker, the
// manifest listing and the debugging call are all checked against a real
// workspace and a real fake mod process, without a model or a network.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/hooks"
)

// TestModHelperProcess is the fake mod. It is re-executed as a child by
// TestModsCallRunsATool; the env guard keeps it out of normal test runs.
//
// It speaks the mod protocol: one JSON object per line on stdin, one JSON
// object per line on stdout.
func TestModHelperProcess(t *testing.T) {
	if os.Getenv("KINTSUGI_MOD_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil || len(req.ID) == 0 {
			continue
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2026-01-01",
				"tools": []any{map[string]any{
					"name":        "echo",
					"description": "Echo text.",
					"inputSchema": map[string]any{"type": "object"},
				}},
				"commands": []any{map[string]any{
					"name":         "hello",
					"description":  "Say hello.",
					"argumentHint": "[name]",
				}},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text, _ := p.Arguments["text"].(string)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "echo: " + text}},
			}})
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "no such method"}})
		}
	}
	os.Exit(0)
}

// isolatedConfig points the global config at a temporary directory so the
// test never reads the operator's own configuration.
func isolatedConfig(t *testing.T) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "kintsugi"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeMod writes a mod manifest into the workspace's mods directory.
func writeMod(t *testing.T, ws, name, body string) {
	t.Helper()
	dir := filepath.Join(ws, ".kintsugi", "mods", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mod.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// helperModBody is a manifest for the fake mod.
func helperModBody(t *testing.T) string {
	t.Helper()
	args := []string{"-test.run=TestModHelperProcess", "--"}
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		b, _ := json.Marshal(a)
		quoted = append(quoted, string(b))
	}
	return "name = \"hello\"\n" +
		"version = \"0.1.0\"\n" +
		"description = \"the fake mod\"\n" +
		"command = " + quoteJSON(t, os.Args[0]) + "\n" +
		"args = [" + strings.Join(quoted, ", ") + "]\n" +
		"events = [\"PreToolUse\"]\n" +
		"\n[[tools]]\nname = \"echo\"\ndescription = \"Echo text.\"\n" +
		"[tools.parameters]\ntype = \"object\"\n" +
		"[tools.parameters.properties.text]\ntype = \"string\"\n" +
		"\n[[commands]]\nname = \"hello\"\ndescription = \"Say hello.\"\nargument_hint = \"[name]\"\n"
}

// quoteJSON quotes a value for TOML.
func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, _ := json.Marshal(s)
	return string(b)
}

// TestModsListShowsTheManifests proves the listing carries the mod, what it
// provides, and the trust state it would run under.
func TestModsListShowsTheManifests(t *testing.T) {
	isolatedConfig(t)
	ws := t.TempDir()
	writeMod(t, ws, "hello", helperModBody(t))

	out := captureStdout(t, func() { cmdMods(ws, []string{"list"}) })

	for _, want := range []string{
		"hello", "0.1.0", "the fake mod", "echo", "hello [name]", "PreToolUse",
		"untrusted", "listed only",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the listing does not mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, filepath.Join(ws, ".kintsugi", "mods")) {
		t.Fatalf("the listing does not name the mods directory:\n%s", out)
	}
}

// TestModsListWithoutAnyMods proves the command still answers when the
// workspace has no mods at all.
func TestModsListWithoutAnyMods(t *testing.T) {
	isolatedConfig(t)
	ws := t.TempDir()

	out := captureStdout(t, func() { cmdMods(ws, []string{"list"}) })

	if !strings.Contains(out, "(no mod manifests)") {
		t.Fatalf("the listing does not report the empty directory:\n%s", out)
	}
}

// TestModsTrustWritesTheMarker proves `kintsugi mods trust` writes the shared
// marker (the one the project hooks use).
func TestModsTrustWritesTheMarker(t *testing.T) {
	isolatedConfig(t)
	ws := t.TempDir()

	out := captureStdout(t, func() { cmdMods(ws, []string{"trust"}) })

	if !hooks.IsTrusted(ws) {
		t.Fatal("the command did not make the workspace trusted")
	}
	if !strings.Contains(out, hooks.TrustedMarkerPath(ws)) {
		t.Fatalf("the output does not name the marker:\n%s", out)
	}
	if !strings.Contains(out, "kintsugi mods list") {
		t.Fatalf("the output does not tell the operator to review the mods:\n%s", out)
	}
}

// TestModsUntrustRemovesTheMarker proves the trust decision is revocable.
func TestModsUntrustRemovesTheMarker(t *testing.T) {
	isolatedConfig(t)
	ws := t.TempDir()
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdMods(ws, []string{"untrust"}) })

	if hooks.IsTrusted(ws) {
		t.Fatal("the marker was not removed")
	}
	if !strings.Contains(out, hooks.TrustedMarkerPath(ws)) {
		t.Fatalf("the output does not name the marker:\n%s", out)
	}
}

// TestModsListAfterTrustShowsTheModRunning proves the listing follows the
// marker.
func TestModsListAfterTrustShowsTheModRunning(t *testing.T) {
	isolatedConfig(t)
	ws := t.TempDir()
	writeMod(t, ws, "hello", helperModBody(t))
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdMods(ws, []string{"list"}) })

	if !strings.Contains(out, "trusted") {
		t.Fatalf("the listing does not report the trusted state:\n%s", out)
	}
	if !strings.Contains(out, "will run") {
		t.Fatalf("the listing does not say the mod will run:\n%s", out)
	}
}

// TestModsCallRunsATool proves the debugging path: the mod is started, the
// tool is called and the answer is printed.
func TestModsCallRunsATool(t *testing.T) {
	isolatedConfig(t)
	t.Setenv("KINTSUGI_MOD_HELPER", "1")
	ws := t.TempDir()
	writeMod(t, ws, "hello", helperModBody(t))
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdMods(ws, []string{"call", "hello", "echo", `{"text":"hi"}`})
	})

	if !strings.Contains(out, "echo: hi") {
		t.Fatalf("the call did not print the answer:\n%s", out)
	}
}
