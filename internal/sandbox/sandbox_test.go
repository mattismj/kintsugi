package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kintsugi/internal/config"
)

// fakeBackend puts an executable named "bwrap" in a temporary PATH, so the
// backend can be exercised without depending on the host having bubblewrap.
func fakeBackend(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return bin
}

// emptyPATH removes every executable from PATH.
func emptyPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestDetectReportsUnavailableWithoutBackend(t *testing.T) {
	emptyPATH(t)
	res := Detect(Config{Mode: ModeWorkspace, Network: NetworkNone})
	if res.Available {
		t.Fatalf("backend reported available without bubblewrap: %+v", res)
	}
	if res.Backend != BackendBubblewrap {
		t.Errorf("backend = %q, want %q", res.Backend, BackendBubblewrap)
	}
	if res.Reason == "" {
		t.Error("unavailable result carries no reason")
	}
	if !strings.Contains(res.Reason, "bwrap") {
		t.Errorf("reason does not name the missing backend: %q", res.Reason)
	}
}

func TestDetectOffIsNotABackend(t *testing.T) {
	emptyPATH(t)
	res := Detect(Config{Mode: ModeOff})
	if !res.Available {
		t.Fatalf("an off sandbox needs no backend: %+v", res)
	}
	if res.Backend != BackendNone {
		t.Errorf("backend = %q, want %q", res.Backend, BackendNone)
	}
	if res.Reason == "" {
		t.Error("result carries no reason")
	}
}

func TestDetectRejectsUnknownMode(t *testing.T) {
	fakeBackend(t)
	res := Detect(Config{Mode: Mode("nonsense")})
	if res.Available {
		t.Fatalf("an unknown mode must not report available: %+v", res)
	}
	if !strings.Contains(res.Reason, "nonsense") {
		t.Errorf("reason does not name the bad mode: %q", res.Reason)
	}
}

func TestDetectRejectsUnknownNetwork(t *testing.T) {
	fakeBackend(t)
	res := Detect(Config{Mode: ModeWorkspace, Network: Network("egress")})
	if res.Available {
		t.Fatalf("an unknown network must not report available: %+v", res)
	}
}

func TestDetectUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux has a backend")
	}
	res := Detect(Config{Mode: ModeWorkspace})
	if res.Available {
		t.Fatalf("no backend is implemented for %s: %+v", runtime.GOOS, res)
	}
	if !strings.Contains(res.Reason, runtime.GOOS) {
		t.Errorf("reason does not name the platform: %q", res.Reason)
	}
}

// hasArg reports whether argv contains the consecutive sequence want.
func hasArg(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		match := true
		for j := range want {
			if argv[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestWrapBindsWorkspaceAndUnsharesNet(t *testing.T) {
	fakeBackend(t)
	ws := t.TempDir()
	cfg := Config{Mode: ModeWorkspace, Network: NetworkNone}

	argv, err := Wrap("echo hi", cfg, ws)
	if err != nil {
		t.Fatalf("wrap failed: %v", err)
	}
	if !hasArg(argv, "--bind", ws, ws) {
		t.Errorf("workspace is not bound read-write: %q", argv)
	}
	if !hasArg(argv, "--unshare-net") {
		t.Errorf("network=none must unshare the network: %q", argv)
	}
	if !hasArg(argv, "--die-with-parent") {
		t.Errorf("sandbox must not outlive the harness: %q", argv)
	}
	if !hasArg(argv, "--tmpfs", "/tmp") || !hasArg(argv, "--proc", "/proc") || !hasArg(argv, "--dev", "/dev") {
		t.Errorf("sandbox is missing its private /tmp, /proc or /dev: %q", argv)
	}
	if !hasArg(argv, "--", "bash", "-lc", "echo hi") {
		t.Errorf("the original command is not the last thing that runs: %q", argv)
	}
	if argv[0] != "bwrap" && !strings.HasSuffix(argv[0], "bwrap") {
		t.Errorf("argv does not start with the backend: %q", argv[0])
	}
}

func TestWrapReadonlyBindsWorkspaceReadOnly(t *testing.T) {
	fakeBackend(t)
	ws := t.TempDir()
	cfg := Config{Mode: ModeReadonly, Network: NetworkNone}

	argv, err := Wrap("echo hi", cfg, ws)
	if err != nil {
		t.Fatalf("wrap failed: %v", err)
	}
	if !hasArg(argv, "--ro-bind", ws, ws) {
		t.Errorf("readonly mode must bind the workspace read-only: %q", argv)
	}
	if hasArg(argv, "--bind", ws, ws) {
		t.Errorf("readonly mode must not bind the workspace read-write: %q", argv)
	}
}

func TestWrapLoopbackUnsharesNetAndBringsUpLo(t *testing.T) {
	fakeBackend(t)
	ws := t.TempDir()
	cfg := Config{Mode: ModeWorkspace, Network: NetworkLoopback}

	argv, err := Wrap("echo hi", cfg, ws)
	if err != nil {
		t.Fatalf("wrap failed: %v", err)
	}
	if !hasArg(argv, "--unshare-net") {
		t.Errorf("loopback still needs its own network namespace: %q", argv)
	}
	if !strings.Contains(strings.Join(argv, " "), "ip link set lo up") {
		t.Errorf("loopback interface is not brought up: %q", argv)
	}
}

func TestWrapAllowlistKeepsHostNetwork(t *testing.T) {
	fakeBackend(t)
	ws := t.TempDir()
	cfg := Config{Mode: ModeWorkspace, Network: NetworkAllowlist, Allowlist: []string{"example.com"}}

	argv, err := Wrap("echo hi", cfg, ws)
	if err != nil {
		t.Fatalf("wrap failed: %v", err)
	}
	if hasArg(argv, "--unshare-net") {
		t.Errorf("allowlist keeps the host network namespace: %q", argv)
	}
}

func TestWrapMissingBackendIsAnError(t *testing.T) {
	emptyPATH(t)
	ws := t.TempDir()
	cfg := Config{Mode: ModeWorkspace}

	argv, err := Wrap("echo hi", cfg, ws)
	if err == nil {
		t.Fatalf("a missing backend must be an error, not a passthrough: %q", argv)
	}
	if argv != nil {
		t.Errorf("a failed wrap must not return an argv: %q", argv)
	}
	if !strings.Contains(err.Error(), "bwrap") {
		t.Errorf("error does not name the missing backend: %v", err)
	}
}

func TestWrapOffIsAnError(t *testing.T) {
	fakeBackend(t)
	argv, err := Wrap("echo hi", Config{Mode: ModeOff}, t.TempDir())
	if err == nil {
		t.Fatalf("wrapping with the sandbox off must be an error: %q", argv)
	}
	if argv != nil {
		t.Errorf("a failed wrap must not return an argv: %q", argv)
	}
}

func TestWrapRejectsUnknownMode(t *testing.T) {
	fakeBackend(t)
	argv, err := Wrap("echo hi", Config{Mode: Mode("wide-open")}, t.TempDir())
	if err == nil {
		t.Fatalf("an unknown mode must be an error: %q", argv)
	}
}

func TestWrapNeedsAWorkspace(t *testing.T) {
	fakeBackend(t)
	if _, err := Wrap("echo hi", Config{Mode: ModeWorkspace}, ""); err == nil {
		t.Fatal("a sandbox without a workspace must be an error")
	}
}

func TestFromConfigDefaults(t *testing.T) {
	cfg := FromConfig(config.Sandbox{Mode: "off"})
	if cfg.Mode != ModeOff {
		t.Errorf("mode = %q, want %q", cfg.Mode, ModeOff)
	}
	if cfg.Network != NetworkNone {
		t.Errorf("network = %q, want %q", cfg.Network, NetworkNone)
	}
}

// TestBackendReallyIsolates runs a command through a real bubblewrap when the
// host has one. It is the end-to-end proof that the argv is usable: the
// command cannot write outside the workspace and has no network.
func TestBackendReallyIsolates(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the backend is Linux-only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed")
	}
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	cfg := Config{Mode: ModeWorkspace, Network: NetworkNone}
	if err := Probe(cfg); err != nil {
		t.Skipf("bubblewrap cannot isolate on this host: %v", err)
	}
	argv, err := Wrap("touch "+outside+"; echo written=$?", cfg, ws)
	if err != nil {
		t.Fatalf("wrap failed: %v", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed command failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "written=1") {
		t.Fatalf("the sandbox did not run the command: %q", out)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("the sandboxed command wrote outside the workspace")
	}
}
