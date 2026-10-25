package hooks

// Trust boundary for project hooks (M9.1).
//
// Two sources of hook rules exist and they are not equally trusted:
//
//   - Operator (global) rules live in the operator's own config
//     (~/.config/kintsugi/config.toml, [hooks]). The operator wrote them
//     for this machine; they run without a marker.
//
//   - Project rules live in the repository: <ws>/.kintsugi/hooks.toml. A
//     cloned repository ships that file, so its rules are commands the
//     repository chooses to run on the operator's machine. Running them
//     without consent would turn "clone a repo" into "execute code from that
//     repo", so they require an explicit trust marker.
//
// The marker is <ws>/.kintsugi/hooks-trusted. It is a permission, not a
// rule: `kintsugi hooks trust` writes it, and the project's hooks only run when
// it exists AND the hook file actually defines rules. A marker alone runs
// nothing; an untrusted workspace's project rules are skipped and the attempt
// is logged as hook_trust_denied.
//
// The marker is per workspace and is meant to be revoked (Untrust): trust is
// a decision about one tree, not a permanent property of the machine.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"kintsugi/internal/config"
)

const (
	// ProjectDir is the workspace directory that holds the project hook file
	// and the trust marker.
	ProjectDir = ".kintsugi"
	// ProjectHooksFile is the file name of the project's hook rules.
	ProjectHooksFile = "hooks.toml"
	// TrustedMarkerFile is the file name of the trust marker.
	TrustedMarkerFile = "hooks-trusted"
)

// ProjectHooksPath returns the workspace's project hook file.
//
// The file lives inside the repository, so everything in it is
// repository-controlled. It uses the same tables as the config's [hooks]
// section ([[PreToolUse]], [[SessionStart]], ...), either at the top level of
// the file or nested under a [hooks] table.
func ProjectHooksPath(workspace string) string {
	return filepath.Join(workspace, ProjectDir, ProjectHooksFile)
}

// TrustedMarkerPath returns the workspace's trust marker path. The marker's
// presence is the operator's explicit consent to run the project's hooks.
func TrustedMarkerPath(workspace string) string {
	return filepath.Join(workspace, ProjectDir, TrustedMarkerFile)
}

// IsTrusted reports whether the workspace carries the trust marker.
//
// Trust is explicit and per workspace: a freshly cloned repository has no
// marker, so its project hooks do not run.
func IsTrusted(workspace string) bool {
	if strings.TrimSpace(workspace) == "" {
		return false
	}
	info, err := os.Stat(TrustedMarkerPath(workspace))
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// Trust marks the workspace as trusted: the project hooks in
// <ws>/.kintsugi/hooks.toml may run.
//
// The marker records when it was written, so the operator can see how old the
// consent is. It does not, by itself, run anything: the rules still have to
// exist in the project's hook file.
func Trust(workspace string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("hooks: no workspace to trust")
	}
	path := TrustedMarkerPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("trust marker: %w", err)
	}
	body := fmt.Sprintf("kintsugi: project hooks trusted at %s\n", time.Now().UTC().Format(time.RFC3339))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return fmt.Errorf("trust marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("trust marker: %w", err)
	}
	return nil
}

// Untrust removes the workspace's trust marker, so the project's hooks stop
// running. Removing a marker that is not there is not an error.
func Untrust(workspace string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("hooks: no workspace to untrust")
	}
	if err := os.Remove(TrustedMarkerPath(workspace)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("trust marker: %w", err)
	}
	return nil
}

// TrustMarkerTime returns the time the trust marker was written, or the zero
// time when the workspace is not trusted.
func TrustMarkerTime(workspace string) time.Time {
	info, err := os.Stat(TrustedMarkerPath(workspace))
	if err != nil || info.IsDir() {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

// LoadProjectHooks reads the workspace's project hook file and reports the
// workspace trust state.
//
// The rules are returned whether or not the workspace is trusted: the caller
// decides what to do with them, and the runner refuses to run them without
// the marker. A missing hook file is not an error (there is simply nothing to
// run); a malformed one is an error, and nothing is written, moved or deleted
// while parsing it.
func LoadProjectHooks(workspace string) (map[string][]config.HookRule, bool, error) {
	trusted := IsTrusted(workspace)
	rules := map[string][]config.HookRule{}
	if strings.TrimSpace(workspace) == "" {
		return rules, trusted, nil
	}
	path := ProjectHooksPath(workspace)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return rules, trusted, nil
		}
		return rules, trusted, fmt.Errorf("project hooks %s: %w", path, err)
	}
	parsed, err := parseProjectHooks(path)
	if err != nil {
		return nil, trusted, fmt.Errorf("project hooks %s: %w", path, err)
	}
	return parsed, trusted, nil
}

// parseProjectHooks decodes the project hook file.
//
// Two shapes are accepted, and they mean the same thing:
//
//	[[PreToolUse]]
//	command = "./scripts/check.sh"
//
// and the config's own layout, with the tables under a [hooks] header. The
// top-level tables win when a file carries both.
func parseProjectHooks(path string) (map[string][]config.HookRule, error) {
	var top config.Hooks
	if _, err := toml.DecodeFile(path, &top); err != nil {
		return nil, err
	}
	var nested struct {
		Hooks *config.Hooks `toml:"hooks"`
	}
	if _, err := toml.DecodeFile(path, &nested); err != nil {
		return nil, err
	}
	rules := map[string][]config.HookRule{}
	for _, ev := range Events {
		rs := top.For(ev)
		if len(rs) == 0 && nested.Hooks != nil {
			rs = nested.Hooks.For(ev)
		}
		if len(rs) > 0 {
			rules[ev] = rs
		}
	}
	return rules, nil
}
