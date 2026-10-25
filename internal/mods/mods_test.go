package mods

// Tests for the constrained mod API (M12.5).
//
// The fake mod is the test binary itself, re-executed as a child process:
// it speaks the NDJSON JSON-RPC protocol on stdin/stdout, exactly like a
// real mod would. No model, no network, no trust of the test's own binary.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kintsugi/internal/hooks"
)

// TestHelperMod is the fake mod process. It is re-executed as a child by
// the tests below; the env guard keeps it out of normal test runs.
//
// MODS_HELPER_MODE selects the behaviour:
//
//	normal      answer the handshake and the calls
//	hang        never answer the handshake
//	garbage     answer with a line that is not JSON
//	missing     advertise fewer tools than the manifest declares
//	extra       advertise a tool the manifest does not declare
func TestHelperMod(t *testing.T) {
	if os.Getenv("MODS_HELPER") != "1" {
		return
	}
	mode := os.Getenv("MODS_HELPER_MODE")
	if mode == "" {
		mode = "normal"
	}
	advertised := []string{}
	for _, name := range strings.Split(os.Getenv("MODS_HELPER_TOOLS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			advertised = append(advertised, name)
		}
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
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			// A notification: nothing to answer.
			continue
		}
		reply := func(result any) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			switch mode {
			case "hang":
				time.Sleep(10 * time.Minute)
				os.Exit(0)
			case "garbage":
				_, _ = os.Stdout.WriteString("this is not json\n")
				continue
			case "missing":
				advertised = []string{"echo"}
			case "extra":
				advertised = append(advertised, "sneaky")
			}
			tools := make([]any, 0, len(advertised))
			for _, name := range advertised {
				tools = append(tools, map[string]any{
					"name":        name,
					"description": "the " + name + " tool",
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			reply(map[string]any{
				"protocolVersion": protocolVersion,
				"tools":           tools,
				"commands": []any{
					map[string]any{"name": "hello", "description": "Say hello.", "argumentHint": "[name]"},
				},
			})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text, _ := p.Arguments["text"].(string)
			if p.Name == "boom" {
				reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}})
				continue
			}
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + text}}})
		case "commands/call":
			var p struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello " + p.Arguments}}})
		default:
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "no such method"},
			})
		}
	}
	os.Exit(0)
}

// eventLog collects the manager's log events.
type eventLog struct {
	mu     sync.Mutex
	events []map[string]any
}

func (e *eventLog) log(kind string, v any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	m["kind"] = kind
	e.events = append(e.events, m)
}

func (e *eventLog) find(kind string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev["kind"] == kind {
			return ev
		}
	}
	return nil
}

// helperCommand is the argv that starts the fake mod.
func helperCommand() (string, []string) {
	return os.Args[0], []string{"-test.run=TestHelperMod", "--"}
}

// writeManifest writes a manifest into a mod directory and returns it.
func writeManifest(t *testing.T, dir, name, body string) Manifest {
	t.Helper()
	home := filepath.Join(dir, name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "mod.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mf, err := readManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

// helperManifest is a manifest for the fake mod.
func helperManifest(t *testing.T, dir string, tools ...string) Manifest {
	t.Helper()
	cmd, args := helperCommand()
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, strconv(a))
	}
	var b strings.Builder
	b.WriteString("name = \"hello\"\nversion = \"0.1.0\"\ndescription = \"the fake mod\"\n")
	b.WriteString("command = " + strconv(cmd) + "\n")
	b.WriteString("args = [" + strings.Join(quoted, ", ") + "]\n")
	b.WriteString("events = [\"PreToolUse\"]\n")
	for _, tool := range tools {
		b.WriteString("\n[[tools]]\nname = " + strconv(tool) + "\ndescription = \"the " + tool + " tool\"\n")
		b.WriteString("[tools.parameters]\ntype = \"object\"\n")
		b.WriteString("[tools.parameters.properties.text]\ntype = \"string\"\n")
	}
	b.WriteString("\n[[commands]]\nname = \"hello\"\ndescription = \"Say hello.\"\nargument_hint = \"[name]\"\n")
	return writeManifest(t, dir, "hello", b.String())
}

// strconv quotes a value for TOML.
func strconv(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestLoadReadsManifests proves mod.toml and mod.json are both read and
// that the manifest's fields survive.
func TestLoadReadsManifests(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "hello")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	tomlBody := `
name = "hello"
version = "0.2.0"
description = "the fake mod"
command = "./run.sh"
args = ["--serve"]
events = ["PreToolUse", "PostToolUse"]

[env]
TOKEN = "HELPER_TOKEN"

[[tools]]
name = "echo"
description = "Echo text."
[tools.parameters]
type = "object"

[[commands]]
name = "greet"
description = "Greet someone."
argument_hint = "[name]"
`
	if err := os.WriteFile(filepath.Join(home, "mod.toml"), []byte(tomlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonHome := filepath.Join(dir, "other")
	if err := os.MkdirAll(jsonHome, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonBody := `{"name":"other","version":"1.0","description":"json mod","command":"./run.sh",
		"tools":[{"name":"ping","description":"Ping."}]}`
	if err := os.WriteFile(filepath.Join(jsonHome, "mod.json"), []byte(jsonBody), 0o644); err != nil {
		t.Fatal(err)
	}

	manifests, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("manifests = %d, want 2", len(manifests))
	}
	if manifests[0].Name != "hello" || manifests[1].Name != "other" {
		t.Fatalf("names = %q, %q", manifests[0].Name, manifests[1].Name)
	}
	m := manifests[0]
	if m.Version != "0.2.0" || m.Command != "./run.sh" || len(m.Args) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Tools) != 1 || m.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", m.Tools)
	}
	if m.Tools[0].Parameters["type"] != "object" {
		t.Fatalf("parameters = %+v", m.Tools[0].Parameters)
	}
	if len(m.Commands) != 1 || m.Commands[0].ArgumentHint != "[name]" {
		t.Fatalf("commands = %+v", m.Commands)
	}
	if len(m.Events) != 2 || m.Events[0] != "PreToolUse" {
		t.Fatalf("events = %+v", m.Events)
	}
	if m.Env["TOKEN"] != "HELPER_TOKEN" {
		t.Fatalf("env = %+v", m.Env)
	}
	if m.Dir != home {
		t.Fatalf("dir = %q, want %q", m.Dir, home)
	}
}

// TestLoadReportsBrokenManifests proves a broken manifest is reported and
// skipped, and does not hide the working ones.
func TestLoadReportsBrokenManifests(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "mod.toml"), []byte("name = \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "mod.toml"), []byte("name = \"good\"\ncommand = \"./run.sh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifests, err := Load(dir)
	if err == nil {
		t.Fatal("a broken manifest was not reported")
	}
	if len(manifests) != 1 || manifests[0].Name != "good" {
		t.Fatalf("manifests = %+v, want only the good one", manifests)
	}
}

// TestLoadMissingDirectoryIsNotAnError proves an absent directory is simply
// empty.
func TestLoadMissingDirectoryIsNotAnError(t *testing.T) {
	manifests, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(manifests) != 0 {
		t.Fatalf("Load = %v, %v", manifests, err)
	}
}

// TestModIsDiscoveredAndCalled proves the whole path: the manifest is read,
// the mod is started, the handshake is checked, the tool is exposed under
// its namespaced name and the call reaches the mod.
func TestModIsDiscoveredAndCalled(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	mgr := NewManager(5*time.Second, nil)
	defer mgr.Close()
	if err := mgr.Discover([]Manifest{mf}, true); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	defs := mgr.Definitions()
	if len(defs) != 1 {
		t.Fatalf("definitions = %+v, want one tool", defs)
	}
	if defs[0].Function.Name != "mod__hello__echo" {
		t.Fatalf("tool name = %q, want the namespaced name", defs[0].Function.Name)
	}
	if defs[0].Function.Parameters.Properties["text"].Type != "string" {
		t.Fatalf("schema not translated: %+v", defs[0].Function.Parameters)
	}

	out, isErr, err := mgr.Call(context.Background(), "mod__hello__echo", `{"text":"hi"}`)
	if err != nil || isErr || out != "echo: hi" {
		t.Fatalf("call = %q isErr=%v err=%v", out, isErr, err)
	}
	// The raw name works too.
	out, _, err = mgr.Call(context.Background(), "echo", `{"text":"there"}`)
	if err != nil || out != "echo: there" {
		t.Fatalf("raw call = %q err=%v", out, err)
	}
}

// TestUntrustedWorkspaceDoesNotStartTheMod proves the trust gate: an
// untrusted mod is listed and reported, never started.
func TestUntrustedWorkspaceDoesNotStartTheMod(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	log := &eventLog{}
	mgr := NewManager(5*time.Second, log.log)
	defer mgr.Close()

	err := mgr.Discover([]Manifest{mf}, false)
	if err == nil {
		t.Fatal("an untrusted mod was accepted")
	}
	if !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("error = %v, want the trust reason", err)
	}
	if len(mgr.Definitions()) != 0 {
		t.Fatalf("definitions = %+v, want none", mgr.Definitions())
	}
	ev := log.find("mod_trust_denied")
	if ev == nil {
		t.Fatal("no mod_trust_denied event")
	}
	// The mod is still listed, with the reason.
	mods := mgr.Mods()
	if len(mods) != 1 {
		t.Fatalf("mods = %d, want the untrusted one listed", len(mods))
	}
	status, reason := mods[0].State()
	if status != StatusUntrusted {
		t.Fatalf("status = %q, want %q", status, StatusUntrusted)
	}
	if reason == nil || !strings.Contains(reason.Error(), "kintsugi mods trust") {
		t.Fatalf("reason = %v, want it to name the trust command", reason)
	}
}

// TestHangingModIsKilledWithATimeout proves a mod that never answers is
// dropped and its process is killed.
func TestHangingModIsKilledWithATimeout(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_MODE", "hang")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	mgr := NewManager(1*time.Second, nil)
	defer mgr.Close()

	err := mgr.Discover([]Manifest{mf}, true)
	if err == nil {
		t.Fatal("a hanging mod was accepted")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if len(mgr.Definitions()) != 0 {
		t.Fatalf("definitions = %+v, want none", mgr.Definitions())
	}
}

// TestMalformedAnswerIsAnError proves a mod that answers with garbage is an
// error, not a panic and not a hang.
func TestMalformedAnswerIsAnError(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_MODE", "garbage")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	mgr := NewManager(3*time.Second, nil)
	defer mgr.Close()

	err := mgr.Discover([]Manifest{mf}, true)
	if err == nil {
		t.Fatal("a malformed answer was accepted")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v, want a malformed-answer error", err)
	}
	if len(mgr.Definitions()) != 0 {
		t.Fatalf("definitions = %+v, want none", mgr.Definitions())
	}
}

// TestDeclaredToolsAreExactlyWhatIsExposed proves the harness hands the mod
// no more than the manifest declared: a tool the mod advertises but the
// manifest does not declare is never exposed.
func TestDeclaredToolsAreExactlyWhatIsExposed(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_MODE", "extra")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	log := &eventLog{}
	mgr := NewManager(5*time.Second, log.log)
	defer mgr.Close()
	if err := mgr.Discover([]Manifest{mf}, true); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	defs := mgr.Definitions()
	if len(defs) != 1 || defs[0].Function.Name != "mod__hello__echo" {
		t.Fatalf("definitions = %+v, want only the declared tool", defs)
	}
	if ev := log.find("mod_extra_tool"); ev == nil {
		t.Fatal("the undeclared tool was not reported")
	}
	// The undeclared tool cannot be called.
	if _, _, err := mgr.Call(context.Background(), "mod__hello__sneaky", `{}`); err == nil {
		t.Fatal("an undeclared tool was callable")
	}
}

// TestModDeclaringAToolItDoesNotProvideIsDropped proves the manifest is the
// contract: a declared tool the mod does not answer is an error.
func TestModDeclaringAToolItDoesNotProvideIsDropped(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_MODE", "missing")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo", "secret")

	mgr := NewManager(5*time.Second, nil)
	defer mgr.Close()

	err := mgr.Discover([]Manifest{mf}, true)
	if err == nil {
		t.Fatal("a mod that does not provide a declared tool was accepted")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v, want it to name the missing tool", err)
	}
	if len(mgr.Definitions()) != 0 {
		t.Fatalf("definitions = %+v, want none", mgr.Definitions())
	}
}

// TestCommandsAreExposed proves the slash commands the mod provides are
// listed.
func TestCommandsAreExposed(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	mgr := NewManager(5*time.Second, nil)
	defer mgr.Close()
	if err := mgr.Discover([]Manifest{mf}, true); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cmds := mgr.Commands()
	if len(cmds) != 1 || cmds[0].Command.Name != "hello" || cmds[0].Mod != "hello" {
		t.Fatalf("commands = %+v", cmds)
	}
	mod := mgr.Mods()[0]
	out, err := mod.CallCommand(context.Background(), "hello", "world")
	if err != nil || out != "hello world" {
		t.Fatalf("command = %q err=%v", out, err)
	}
}

// TestModEnvComesFromTheEnvironment proves the manifest names environment
// variables and never carries their values.
func TestModEnvComesFromTheEnvironment(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	t.Setenv("HELPER_TOKEN", "token-value")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")
	mf.Env = map[string]string{"TOKEN": "HELPER_TOKEN"}

	env := modEnv(mf)
	found := false
	for _, kv := range env {
		if kv == "TOKEN=token-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the declared variable is not in the mod's environment: %v", env)
	}
}

// TestTrustedUsesTheHooksMarker proves one marker governs both the project
// hooks and the mods.
func TestTrustedUsesTheHooksMarker(t *testing.T) {
	ws := t.TempDir()
	if Trusted(ws) {
		t.Fatal("a fresh workspace is trusted")
	}
	if err := hooks.Trust(ws); err != nil {
		t.Fatal(err)
	}
	if !Trusted(ws) {
		t.Fatal("the marker did not make the workspace trusted")
	}
	if !hooks.IsTrusted(ws) {
		t.Fatal("the mods trust marker is not the hooks marker")
	}
}

// TestPackageLevelDiscoverUsesTheProcessRegistry proves the package-level
// shorthand reaches the process-wide registry.
func TestPackageLevelDiscoverUsesTheProcessRegistry(t *testing.T) {
	mf := Manifest{Name: "offline", Command: "/nonexistent/mod"}
	if err := Discover([]Manifest{mf}, false); err == nil {
		t.Fatal("an untrusted mod was accepted")
	}
	if len(Active().Mods()) == 0 {
		t.Fatal("the untrusted mod was not listed")
	}
}

// TestHasRoutesOnlyRunningMods proves the registry routes a tool only while
// its mod runs.
func TestHasRoutesOnlyRunningMods(t *testing.T) {
	t.Setenv("MODS_HELPER", "1")
	t.Setenv("MODS_HELPER_TOOLS", "echo")
	dir := t.TempDir()
	mf := helperManifest(t, dir, "echo")

	mgr := NewManager(5*time.Second, nil)
	defer mgr.Close()
	if err := mgr.Discover([]Manifest{mf}, true); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !mgr.Has("mod__hello__echo") {
		t.Fatal("the running mod's tool is not routed")
	}
	if mgr.Has("mod__hello__nope") {
		t.Fatal("an undeclared tool is routed")
	}

	untrusted := NewManager(5*time.Second, nil)
	defer untrusted.Close()
	if err := untrusted.Discover([]Manifest{mf}, false); err == nil {
		t.Fatal("an untrusted mod was accepted")
	}
	if untrusted.Has("mod__hello__echo") {
		t.Fatal("an untrusted mod routes a tool")
	}
}

// TestToolNameIsNamespaced pins the exposed name.
func TestToolNameIsNamespaced(t *testing.T) {
	if got := ToolName("hello", "echo"); got != "mod__hello__echo" {
		t.Fatalf("ToolName = %q", got)
	}
	if got := ToolName("My Mod!", "do it"); got != "mod__My_Mod___do_it" {
		t.Fatalf("ToolName = %q", got)
	}
}

// TestDirResolvesTheConfiguredDirectory proves the default and the
// workspace-relative path.
func TestDirResolvesTheConfiguredDirectory(t *testing.T) {
	ws := "/tmp/ws"
	if got := Dir(ws, ""); got != filepath.Join(ws, DefaultDir) {
		t.Fatalf("Dir = %q", got)
	}
	if got := Dir(ws, "/opt/mods"); got != "/opt/mods" {
		t.Fatalf("Dir = %q", got)
	}
}
