// Package sandbox runs commands inside an OS-level sandbox (M11.1,
// DESIGN §16.9).
//
// The permission layer answers "may the agent ask for this?"; the sandbox
// answers "what can the process actually do?". On Linux the backend is
// bubblewrap (bwrap): the harness builds a minimal read-only view of the
// system, binds the workspace read-write (read-only in "readonly" mode),
// gives the process a private /tmp, a private /proc and — depending on the
// network posture — no network namespace at all.
//
// The backend degrades honestly. There is no fallback: on a platform without
// a backend, Detect says so and Wrap returns an error. A caller that
// promised isolation never silently runs the command unsandboxed.
//
// Only Linux is implemented (ROADMAP: "M11: Linux only (bubblewrap,
// landlock fallback)"). The landlock fallback is not written yet, so it is
// not advertised here.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"kintsugi/internal/config"
)

// Mode is the filesystem posture of the sandbox.
type Mode string

const (
	// ModeOff runs commands without OS isolation. This is the default: the
	// sandbox is opt-in, because an unavailable backend must fail loudly
	// rather than surprise an operator who expected isolation.
	ModeOff Mode = "off"
	// ModeReadonly binds the workspace read-only: the command may read the
	// workspace but not change it. The rest of the system is read-only too.
	ModeReadonly Mode = "readonly"
	// ModeWorkspace binds the workspace read-write: the command may work in
	// the workspace. Everything else on the host stays read-only.
	ModeWorkspace Mode = "workspace"
)

// Network is the network posture of the sandbox.
type Network string

const (
	// NetworkNone removes the network namespace: no egress at all.
	NetworkNone Network = "none"
	// NetworkLoopback keeps a network namespace but only the loopback
	// interface is usable (a lab listener on 127.0.0.1 still answers).
	NetworkLoopback Network = "loopback"
	// NetworkAllowlist keeps the host network namespace. bubblewrap cannot
	// filter traffic by host, so the allowlist is *not* enforced here: it
	// is enforced by the fetch layer (scope host rules + the offline
	// guard). This is the one posture the sandbox does not narrow, and it
	// is documented as such rather than pretended.
	NetworkAllowlist Network = "allowlist"
)

// Backend names.
const (
	// BackendBubblewrap is the only backend implemented so far.
	BackendBubblewrap = "bubblewrap"
	// BackendNone is reported when the sandbox is switched off.
	BackendNone = "none"
)

// defaultBinary is the bubblewrap executable name.
const defaultBinary = "bwrap"

// Config is the sandbox posture for one session.
type Config struct {
	// Mode is off | readonly | workspace.
	Mode Mode
	// Network is none | loopback | allowlist. An empty value means none.
	Network Network
	// Allowlist names the hosts reachable in allowlist mode. It is enforced
	// by the fetch layer, not by the sandbox (see NetworkAllowlist).
	Allowlist []string
	// Binary overrides the backend executable (default: bwrap).
	Binary string
	// ExtraArgs are appended to the backend argv, before the command.
	ExtraArgs []string
}

// Enabled reports whether the configuration asks for a sandbox at all.
// A mode of "off" (or an unset mode) means commands run unsandboxed.
func (c Config) Enabled() bool {
	switch Mode(strings.ToLower(strings.TrimSpace(string(c.Mode)))) {
	case ModeReadonly, ModeWorkspace:
		return true
	}
	return false
}

// Result reports whether a sandbox backend can be used here.
type Result struct {
	// Available is true when the configured posture can be enforced.
	Available bool
	// Backend names the backend: bubblewrap, or none when the sandbox is off.
	Backend string
	// Reason explains the result in one human-readable line. It is always
	// set: a refusal without a reason is not actionable.
	Reason string
}

// FromConfig converts the [sandbox] config block into a runtime Config.
// An unset network means "none" whenever the sandbox is on.
func FromConfig(c config.Sandbox) Config {
	cfg := Config{
		Mode:      Mode(strings.ToLower(strings.TrimSpace(c.Mode))),
		Network:   Network(strings.ToLower(strings.TrimSpace(c.Network))),
		Allowlist: append([]string(nil), c.Allowlist...),
		Binary:    strings.TrimSpace(c.Binary),
		ExtraArgs: append([]string(nil), c.ExtraArgs...),
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeOff
	}
	if cfg.Network == "" {
		cfg.Network = NetworkNone
	}
	return cfg
}

// Detect reports whether a sandbox backend can be used here.
//
// It checks the platform and that the backend binary exists. It never
// guesses: on a machine without bubblewrap (or without a supported
// platform) the result is unavailable, with the reason spelled out.
func Detect(cfg Config) Result {
	mode, network, err := normalize(cfg)
	if err != nil {
		return Result{Available: false, Backend: BackendBubblewrap, Reason: err.Error()}
	}
	if mode == ModeOff {
		return Result{
			Available: true,
			Backend:   BackendNone,
			Reason:    "sandbox is off: commands run without OS isolation",
		}
	}
	if runtime.GOOS != "linux" {
		return Result{
			Available: false,
			Backend:   BackendBubblewrap,
			Reason: fmt.Sprintf("the OS sandbox needs bubblewrap, which is implemented for Linux only (this host is %s)",
				runtime.GOOS),
		}
	}
	bin := binaryName(cfg)
	path, err := exec.LookPath(bin)
	if err != nil {
		return Result{
			Available: false,
			Backend:   BackendBubblewrap,
			Reason: fmt.Sprintf("%s (%s) is not installed: install it, or set sandbox.mode = %q to run without isolation",
				bin, BackendBubblewrap, string(ModeOff)),
		}
	}
	return Result{
		Available: true,
		Backend:   BackendBubblewrap,
		Reason:    fmt.Sprintf("%s is available at %s (mode %s, network %s)", bin, path, mode, network),
	}
}

// Wrap builds the argv that runs cmdline inside the sandbox.
//
// The result is the backend executable, the backend flags and the original
// command. Wrap returns an error — never a bare passthrough — when the
// backend is missing or the configuration is invalid: the caller must never
// run a command unsandboxed after it promised isolation.
func Wrap(cmdline string, cfg Config, workspace string) ([]string, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("sandbox mode is %q: there is nothing to wrap", string(cfg.Mode))
	}
	mode, network, err := normalize(cfg)
	if err != nil {
		return nil, err
	}
	res := Detect(cfg)
	if !res.Available {
		return nil, fmt.Errorf("sandbox unavailable: %s", res.Reason)
	}
	binPath, err := exec.LookPath(binaryName(cfg))
	if err != nil {
		return nil, fmt.Errorf("sandbox unavailable: %s", res.Reason)
	}
	ws, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}

	args := []string{binPath}
	// The command must not outlive the harness, and it must not see the
	// host's processes, IPC or hostname.
	args = append(args, "--die-with-parent", "--unshare-pid", "--unshare-ipc", "--unshare-uts")
	// A minimal read-only view of the system: a shell, a standard library
	// and the TLS material the loopback/allowlist postures need. Nothing
	// else of the host is visible.
	for _, p := range systemPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		args = append(args, "--ro-bind", p, p)
	}
	args = append(args, "--tmpfs", "/tmp", "--dev", "/dev", "--proc", "/proc")
	if mode == ModeReadonly {
		args = append(args, "--ro-bind", ws, ws)
	} else {
		args = append(args, "--bind", ws, ws)
	}
	args = append(args, "--chdir", ws, "--setenv", "HOME", "/tmp", "--setenv", "TMPDIR", "/tmp")
	switch network {
	case NetworkNone, NetworkLoopback:
		// A fresh network namespace has no route out and no interface up.
		args = append(args, "--unshare-net")
	case NetworkAllowlist:
		// bubblewrap cannot filter by host: the host network namespace is
		// kept and the allowlist is enforced by the fetch layer.
	}
	for _, extra := range cfg.ExtraArgs {
		if e := strings.TrimSpace(extra); e != "" {
			args = append(args, e)
		}
	}
	args = append(args, "--")
	if network == NetworkLoopback {
		// The loopback interface of a fresh namespace is down; bring it up
		// before the command runs. `ip` is read-only inside the sandbox, and
		// a missing `ip` only means loopback stays down.
		args = append(args, "sh", "-c", `ip link set lo up 2>/dev/null || true; exec "$@"`, "sh", "bash", "-lc", cmdline)
	} else {
		args = append(args, "bash", "-lc", cmdline)
	}
	return args, nil
}

// Probe runs a trivial command inside the sandbox to check that the backend
// really isolates (user namespaces available, mounts permitted). It is what
// `kintsugi doctor` wants to know; the tool layer does not run it per command.
func Probe(cfg Config) error {
	if !cfg.Enabled() {
		return nil
	}
	res := Detect(cfg)
	if !res.Available {
		return errors.New(res.Reason)
	}
	ws, err := os.Getwd()
	if err != nil {
		ws = os.TempDir()
	}
	argv, err := Wrap("true", cfg, ws)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("sandbox probe failed: %s", msg)
	}
	return nil
}

// systemPaths are the host paths the sandbox exposes read-only. The list is
// deliberately small: the command needs a shell, a standard library and the
// TLS/DNS material for the loopback and allowlist postures. The home
// directory, /run, /var and /opt stay invisible.
var systemPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/etc/alternatives", "/etc/ssl", "/etc/ca-certificates", "/etc/passwd",
	"/etc/group", "/etc/nsswitch.conf", "/etc/resolv.conf", "/etc/hosts",
	"/etc/localtime", "/etc/hostname",
}

// normalize resolves the mode and network names, defaulting and rejecting
// unknown values (a typo must not silently weaken the posture).
func normalize(cfg Config) (Mode, Network, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(string(cfg.Mode))))
	switch mode {
	case ModeOff, ModeReadonly, ModeWorkspace:
	default:
		return "", "", fmt.Errorf("unknown sandbox mode %q (want off|readonly|workspace)", string(cfg.Mode))
	}
	network := Network(strings.ToLower(strings.TrimSpace(string(cfg.Network))))
	switch network {
	case "":
		network = NetworkNone
	case NetworkNone, NetworkLoopback, NetworkAllowlist:
	default:
		return "", "", fmt.Errorf("unknown sandbox network %q (want none|loopback|allowlist)", string(cfg.Network))
	}
	return mode, network, nil
}

// binaryName is the backend executable to use.
func binaryName(cfg Config) string {
	if b := strings.TrimSpace(cfg.Binary); b != "" {
		return b
	}
	return defaultBinary
}

// absWorkspace resolves the workspace directory that gets bound.
func absWorkspace(workspace string) (string, error) {
	w := strings.TrimSpace(workspace)
	if w == "" {
		return "", fmt.Errorf("sandbox: no workspace to bind")
	}
	abs, err := filepath.Abs(w)
	if err != nil {
		return "", fmt.Errorf("sandbox: workspace %q: %w", workspace, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("sandbox: workspace %q: %w", workspace, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox: workspace %q is not a directory", workspace)
	}
	return abs, nil
}
