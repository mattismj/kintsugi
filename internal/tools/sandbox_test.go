package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/sandbox"
	"kintsugi/internal/scope"
)

// missingBackend is a sandbox configuration whose backend cannot exist, so
// the "configured but unavailable" path is exercised without depending on
// the host's packages.
func missingBackend() sandbox.Config {
	return sandbox.Config{
		Mode:    sandbox.ModeWorkspace,
		Network: sandbox.NetworkNone,
		Binary:  "bwrap-not-installed-on-this-host",
	}
}

func TestBashRefusedWhenSandboxUnavailable(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, Sandbox: missingBackend()}

	res := dispatch(t, ex, "bash", `{"command":"touch ran.txt"}`)
	if !res.Failed {
		t.Fatalf("a command ran without the promised sandbox: %+v", res)
	}
	if !strings.Contains(res.Content, "sandbox unavailable") {
		t.Fatalf("the refusal does not name the reason: %+v", res)
	}
	if !strings.Contains(res.Content, "bwrap") {
		t.Fatalf("the refusal does not name the missing backend: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
		t.Fatal("the command ran even though the sandbox was unavailable")
	}
}

func TestBashRefusedWhenSandboxUnavailableIsNotAskedAbout(t *testing.T) {
	ws := testWorkspace(t)
	asked := 0
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{},
		Approver: ApproverFunc(func(ar ApprovalRequest) bool {
			asked++
			return true
		}),
		Sandbox: missingBackend(),
	}
	res := dispatch(t, ex, "bash", `{"command":"echo hi"}`)
	if !res.Failed || !strings.Contains(res.Content, "sandbox unavailable") {
		t.Fatalf("expected a sandbox refusal: %+v", res)
	}
	if asked != 0 {
		t.Fatalf("the operator was asked %d times for a command that cannot run", asked)
	}
}

func TestBashRunsWhenSandboxIsOff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "bash", `{"command":"echo hi"}`)
	if res.Failed || !strings.Contains(res.Content, "hi") {
		t.Fatalf("an unsandboxed command failed: %+v", res)
	}
}

func TestScopeDeniesOutOfScopePath(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Scope:     scope.Rules{Paths: []string{"src/**"}},
	}

	res := dispatch(t, ex, "read", `{"path":"README.md"}`)
	if !res.Failed || !strings.Contains(res.Content, "out of scope") {
		t.Fatalf("an out-of-scope path was not denied: %+v", res)
	}

	res = dispatch(t, ex, "read", `{"path":"src/main.go"}`)
	if res.Failed || !strings.Contains(res.Content, "package main") {
		t.Fatalf("an in-scope path was denied: %+v", res)
	}
}

func TestScopeDeniesOutOfScopeCommand(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Scope:     scope.Rules{Commands: []string{"echo*"}},
	}

	res := dispatch(t, ex, "bash", `{"command":"curl https://evil.example"}`)
	if !res.Failed || !strings.Contains(res.Content, "out of scope") {
		t.Fatalf("an out-of-scope command was not denied: %+v", res)
	}

	res = dispatch(t, ex, "bash", `{"command":"echo ok"}`)
	if res.Failed || !strings.Contains(res.Content, "ok") {
		t.Fatalf("an in-scope command was denied: %+v", res)
	}
}

func TestScopeDeniesOutOfScopeHost(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Scope:     scope.Rules{Hosts: []string{"example.com"}},
	}

	res := dispatch(t, ex, "webfetch", `{"url":"https://evil.example/secret"}`)
	if !res.Failed || !strings.Contains(res.Content, "out of scope") {
		t.Fatalf("an out-of-scope host was not denied: %+v", res)
	}
	if strings.Contains(res.Content, "status") {
		t.Fatalf("the fetch was attempted anyway: %+v", res)
	}
}

func TestScopeOutOfScopeWins(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Scope: scope.Rules{
			Paths:      []string{"**"},
			OutOfScope: []string{"**/src/**"},
		},
	}
	res := dispatch(t, ex, "read", `{"path":"src/main.go"}`)
	if !res.Failed || !strings.Contains(res.Content, "out of scope") {
		t.Fatalf("out_of_scope did not win: %+v", res)
	}
}

// TestApplySecurityUsesTheOperatorConfig walks the whole path: the
// [sandbox] and [scope] blocks of the workspace's kintsugi.toml become the
// executor's posture, and the posture is enforced.
func TestApplySecurityUsesTheOperatorConfig(t *testing.T) {
	ws := testWorkspace(t)
	conf := "[sandbox]\n" +
		"mode = \"workspace\"\n" +
		"network = \"none\"\n" +
		"binary = \"bwrap-not-installed-on-this-host\"\n" +
		"\n[scope]\n" +
		"paths = [\"src/**\"]\n" +
		"commands = [\"echo*\"]\n" +
		"out_of_scope = [\"**/secrets/**\"]\n"
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(ws)
	if err != nil {
		t.Fatalf("config load failed: %v", err)
	}
	if cfg.Sandbox.Mode != "workspace" || cfg.Sandbox.Network != "none" {
		t.Fatalf("the [sandbox] block was not decoded: %+v", cfg.Sandbox)
	}
	if len(cfg.Scope.Paths) != 1 || cfg.Scope.Paths[0] != "src/**" {
		t.Fatalf("the [scope] block was not decoded: %+v", cfg.Scope)
	}

	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	ex.ApplySecurity(cfg)
	if !ex.Sandbox.Enabled() {
		t.Fatalf("the sandbox posture was not applied: %+v", ex.Sandbox)
	}
	if ex.Scope.Empty() {
		t.Fatal("the scope rules were not applied")
	}
	res := dispatch(t, ex, "read", `{"path":"README.md"}`)
	if !res.Failed || !strings.Contains(res.Content, "out of scope") {
		t.Fatalf("the applied scope is not enforced: %+v", res)
	}
	res = dispatch(t, ex, "bash", `{"command":"echo hi"}`)
	if !res.Failed || !strings.Contains(res.Content, "sandbox unavailable") {
		t.Fatalf("the applied sandbox posture is not enforced: %+v", res)
	}
}

// TestScopeFiltersPathLists checks the tools that return a list of paths:
// the scope filters the results, so an out_of_scope file never shows up in
// a glob or a grep hit.
func TestScopeFiltersPathLists(t *testing.T) {
	ws := testWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws, "src", "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "secrets", "key.txt"), []byte("topsecret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Scope: scope.Rules{
			Paths:      []string{"src/**"},
			OutOfScope: []string{"**/secrets/**"},
		},
	}

	res := dispatch(t, ex, "glob", `{"pattern":"**/*.txt","root":"src"}`)
	if res.Failed {
		t.Fatalf("glob failed: %+v", res)
	}
	if strings.Contains(res.Content, "secrets") {
		t.Fatalf("an out-of-scope file showed up in a glob: %+v", res)
	}

	res = dispatch(t, ex, "grep", `{"pattern":"topsecret","path":"src"}`)
	if res.Failed {
		t.Fatalf("grep failed: %+v", res)
	}
	if strings.Contains(res.Content, "topsecret") {
		t.Fatalf("an out-of-scope file showed up in a grep hit: %+v", res)
	}
}

// TestSandboxAllowlistGuardsTheFetchLayer checks the one posture bubblewrap
// cannot enforce: with network = allowlist the host list is checked by the
// fetch layer, so a host outside it never gets a request.
func TestSandboxAllowlistGuardsTheFetchLayer(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Sandbox: sandbox.Config{
			Mode:      sandbox.ModeWorkspace,
			Network:   sandbox.NetworkAllowlist,
			Allowlist: []string{"example.com"},
		},
	}
	res := dispatch(t, ex, "webfetch", `{"url":"https://evil.example/secret"}`)
	if !res.Failed || !strings.Contains(res.Content, "allowlist") {
		t.Fatalf("a host outside the sandbox allowlist was not denied: %+v", res)
	}
}

func TestEmptyScopeChangesNothing(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "read", `{"path":"README.md"}`)
	if res.Failed || !strings.Contains(res.Content, "hello") {
		t.Fatalf("an empty scope must not narrow anything: %+v", res)
	}
}

// TestBashRunsInsideRealSandbox is the end-to-end proof: with a real
// bubblewrap the command runs inside it, and it cannot write outside the
// workspace.
func TestBashRunsInsideRealSandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the backend is Linux-only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed")
	}
	ws := testWorkspace(t)
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Sandbox:   sandbox.Config{Mode: sandbox.ModeWorkspace, Network: sandbox.NetworkNone},
	}
	if err := sandbox.Probe(ex.Sandbox); err != nil {
		t.Skipf("bubblewrap cannot isolate on this host: %v", err)
	}

	res := dispatch(t, ex, "bash", `{"command":"echo inside; touch `+outside+` || true"}`)
	if res.Failed {
		t.Fatalf("the sandboxed command failed: %+v", res)
	}
	if !strings.Contains(res.Content, "inside") {
		t.Fatalf("the sandboxed command did not run: %+v", res)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("the sandboxed command wrote outside the workspace")
	}
}
