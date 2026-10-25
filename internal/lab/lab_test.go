package lab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kintsugi/internal/blueprints"
	"kintsugi/internal/config"
)

// fakeRuntime installs a fake container runtime on PATH and returns the path
// of the file it logs its calls in.
//
// The fake answers `compose version` and accepts every other compose call, so
// the harness's own behaviour (argv, lock, teardown) is what the tests
// check — never the runtime's.
func fakeRuntime(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"log=\"${0%/*}/calls.log\"\n" +
		"printf '%s\\n' \"$*\" >> \"$log\"\n" +
		"if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"version\" ]; then\n" +
		"  echo \"Docker Compose version v2.29.0\"\n" +
		"fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return filepath.Join(dir, "calls.log")
}

// labWorkspace is a workspace with a lab compose file.
func labWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "lab"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "services:\n  fixture:\n    image: alpine:3\n    command: [\"sleep\", \"infinity\"]\n"
	if err := os.WriteFile(filepath.Join(ws, "lab", "docker-compose.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// calls reads the fake runtime's call log.
func calls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestDetectFindsDockerWhenPresent(t *testing.T) {
	fakeRuntime(t, "docker")

	rt, err := Detect(Config{Runtime: RuntimeAuto})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if rt != RuntimeDocker {
		t.Fatalf("Detect = %q, want %q", rt, RuntimeDocker)
	}
}

func TestDetectFindsPodmanWhenDockerIsAbsent(t *testing.T) {
	fakeRuntime(t, "podman")

	rt, err := Detect(Config{Runtime: RuntimeAuto})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if rt != RuntimePodman {
		t.Fatalf("Detect = %q, want %q", rt, RuntimePodman)
	}
}

func TestDetectReportsNoneWithAReason(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	rt, err := Detect(Config{Runtime: RuntimeAuto})
	if rt != RuntimeNone {
		t.Fatalf("Detect = %q, want %q", rt, RuntimeNone)
	}
	if err == nil {
		t.Fatal("Detect returned no error: a missing runtime must be reported")
	}
	if !strings.Contains(err.Error(), "no container runtime") {
		t.Fatalf("the reason does not explain the absence: %v", err)
	}
	if !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("the reason does not say what is missing: %v", err)
	}
}

func TestDetectRejectsAnUnknownRuntime(t *testing.T) {
	rt, err := Detect(Config{Runtime: "kubernetes"})
	if rt != RuntimeNone {
		t.Fatalf("Detect = %q, want %q", rt, RuntimeNone)
	}
	if err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatalf("a typo in the runtime name must be reported: %v", err)
	}
}

func TestUpWritesTheLockAndDownRemovesIt(t *testing.T) {
	log := fakeRuntime(t, "docker")
	ws := labWorkspace(t)

	state, err := Up(context.Background(), ws, Config{
		Runtime:     RuntimeDocker,
		ComposeFile: DefaultComposeFile,
		Project:     DefaultProject,
		Services:    []string{"fixture"},
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !state.Up || !state.Locked {
		t.Fatalf("Up reported %+v, want an up, locked lab", state)
	}
	if state.Runtime != RuntimeDocker || state.Project != DefaultProject {
		t.Fatalf("Up reported %+v, want the docker project", state)
	}

	// The lock is on disk, and it names the lab.
	raw, err := os.ReadFile(LockPath(ws))
	if err != nil {
		t.Fatalf("the lock was not written: %v", err)
	}
	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("the lock is not JSON: %v", err)
	}
	if l.Project != DefaultProject || l.Runtime != RuntimeDocker {
		t.Fatalf("the lock names the wrong lab: %+v", l)
	}
	if l.ComposeFile != DefaultComposeFile {
		t.Fatalf("the lock names the wrong compose file: %+v", l)
	}
	if !samePath(l.Workspace, ws) {
		t.Fatalf("the lock names the wrong workspace: %+v", l)
	}
	if l.StartedAt.IsZero() {
		t.Fatalf("the lock has no start time: %+v", l)
	}

	// The compose command is the documented one.
	joined := strings.Join(calls(t, log), "\n")
	if !strings.Contains(joined, "compose -p kintsugi-lab -f "+filepath.Join(ws, "lab", "docker-compose.yml")+" up -d fixture") {
		t.Fatalf("the compose up command is not the documented one:\n%s", joined)
	}

	// Down removes the lock.
	if err := Down(context.Background(), ws, Config{Runtime: RuntimeDocker}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := os.Stat(LockPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("the lock survived the teardown: %v", err)
	}
	joined = strings.Join(calls(t, log), "\n")
	if !strings.Contains(joined, "down -v") {
		t.Fatalf("the teardown did not run compose down -v:\n%s", joined)
	}
}

func TestDownIsIdempotent(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := labWorkspace(t)

	if _, err := Up(context.Background(), ws, Config{Runtime: RuntimeDocker}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := Down(context.Background(), ws, Config{Runtime: RuntimeDocker}); err != nil {
		t.Fatalf("the first Down failed: %v", err)
	}
	// The second Down has nothing to do, and says so instead of failing.
	rep, err := DownReport(context.Background(), ws, Config{Runtime: RuntimeDocker})
	if err != nil {
		t.Fatalf("the second Down failed: %v", err)
	}
	if rep.LockFound {
		t.Fatalf("the second Down found a lock: %+v", rep)
	}
	if _, err := os.Stat(LockPath(ws)); !os.IsNotExist(err) {
		t.Fatal("the second Down left a lock behind")
	}
}

func TestDownRefusesALockFromAnotherWorkspace(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := labWorkspace(t)

	other := t.TempDir()
	if err := WriteLock(ws, Lock{
		Project:     DefaultProject,
		Runtime:     RuntimeDocker,
		ComposeFile: DefaultComposeFile,
		StartedAt:   time.Now().UTC(),
		Workspace:   other,
	}); err != nil {
		t.Fatal(err)
	}

	err := Down(context.Background(), ws, Config{Runtime: RuntimeDocker})
	if err == nil {
		t.Fatal("a lock from another workspace was removed")
	}
	if !strings.Contains(err.Error(), "another workspace") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	if _, statErr := os.Stat(LockPath(ws)); statErr != nil {
		t.Fatalf("the foreign lock was removed anyway: %v", statErr)
	}
}

func TestUpWithoutAComposeFileSaysSoPlainly(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := t.TempDir()

	_, err := Up(context.Background(), ws, Config{Runtime: RuntimeDocker, ComposeFile: "lab/absent-compose.yml"})
	if err == nil {
		t.Fatal("Up started a lab without a compose file")
	}
	if !strings.Contains(err.Error(), "no compose file") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
	// Nothing shipped one for this workspace, and the harness says so instead
	// of inventing one.
	if !strings.Contains(err.Error(), "no blueprint ships one") {
		t.Fatalf("the error does not say that no blueprint ships one: %v", err)
	}
	if _, statErr := os.Stat(LockPath(ws)); !os.IsNotExist(statErr) {
		t.Fatal("a lock was written for a lab that never came up")
	}
}

// A bare workspace — one never rendered from a blueprint — must not inherit a
// blueprint lab even at the default compose path, because a lab lock refuses
// host bash. Only the render marker makes the fallback apply.
func TestUpInABareWorkspaceNeverInheritsABlueprintLab(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := t.TempDir()

	if _, rendered := blueprints.RenderedFrom(ws); rendered {
		t.Fatal("a bare workspace reports a render marker")
	}
	_, err := Up(context.Background(), ws, Config{Runtime: RuntimeDocker})
	if err == nil {
		t.Fatal("a bare workspace got a lab it never asked for")
	}
	if !strings.Contains(err.Error(), "no compose file") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
	if fileExists(ComposePath(ws, DefaultComposeFile)) {
		t.Fatal("the blueprint compose file was written into a bare workspace")
	}
	if _, statErr := os.Stat(LockPath(ws)); !os.IsNotExist(statErr) {
		t.Fatal("a lock was written for a lab that never came up")
	}
}

// A workspace rendered from a blueprint does get its compose file via the
// fallback, which is the behaviour the marker exists to allow.
func TestUpInARenderedWorkspaceUsesTheBlueprintComposeFile(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := t.TempDir()
	if _, _, err := blueprints.Render("c2", ws, "lab", true); err != nil {
		t.Fatalf("render: %v", err)
	}
	// The blueprint ships the file, so this resolves it from the workspace.
	if !fileExists(ComposePath(ws, DefaultComposeFile)) {
		t.Fatal("the rendered workspace has no compose file")
	}
	if _, err := Up(context.Background(), ws, Config{Runtime: RuntimeDocker}); err != nil {
		t.Fatalf("Up in a rendered workspace: %v", err)
	}
}

func TestDownSaysWhyItSkippedTheTeardown(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := labWorkspace(t)

	if _, err := Up(context.Background(), ws, Config{Runtime: RuntimeDocker}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// The runtime disappears between up and down.
	t.Setenv("PATH", t.TempDir())

	rep, err := DownReport(context.Background(), ws, Config{Runtime: RuntimeDocker})
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if rep.Teardown {
		t.Fatal("the teardown ran without a runtime")
	}
	if !strings.Contains(rep.SkipReason, "not on PATH") {
		t.Fatalf("the report does not say why the teardown was skipped: %+v", rep)
	}
	if !rep.LockRemoved {
		t.Fatal("the lock was not removed")
	}
}

func TestStatusReportsDownWithoutALock(t *testing.T) {
	ws := t.TempDir()

	state, err := Status(context.Background(), ws, Config{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state.Up || state.Locked {
		t.Fatalf("Status reported %+v, want a lab that is down", state)
	}
}

func TestStatusReportsTheRunningLab(t *testing.T) {
	fakeRuntime(t, "docker")
	ws := labWorkspace(t)

	if _, err := Up(context.Background(), ws, Config{
		Runtime:  RuntimeDocker,
		Services: []string{"fixture"},
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	state, err := Status(context.Background(), ws, Config{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !state.Up || !state.Locked {
		t.Fatalf("Status reported %+v, want an up, locked lab", state)
	}
	if state.Runtime != RuntimeDocker || state.Project != DefaultProject {
		t.Fatalf("Status reported %+v, want the docker project", state)
	}
	if len(state.Services) != 1 || state.Services[0] != "fixture" {
		t.Fatalf("Status reported %+v, want the fixture service", state)
	}
	if state.StartedAt.IsZero() {
		t.Fatal("Status reported no start time")
	}
}

func TestExecArgvIsTheComposeExecForm(t *testing.T) {
	argv := ExecArgv(RuntimeDocker, "kintsugi-lab", "fixture", "echo hi")
	want := []string{"docker", "compose", "-p", "kintsugi-lab", "exec", "-T", "fixture", "sh", "-lc", "echo hi"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
	// The command is the argument of the in-container shell, never of a host
	// shell.
	for _, a := range argv {
		if a == "bash" {
			t.Fatalf("the argv runs a host shell: %v", argv)
		}
	}
}

func TestExecEnvNamesTheComposeFile(t *testing.T) {
	env := ExecEnv("/ws/lab/docker-compose.yml", "kintsugi-lab")
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "COMPOSE_FILE=/ws/lab/docker-compose.yml") {
		t.Fatalf("the compose file is not named: %v", env)
	}
	if !strings.Contains(joined, "COMPOSE_PROJECT_NAME=kintsugi-lab") {
		t.Fatalf("the project is not named: %v", env)
	}
}

func TestFromConfigAppliesTheDefaults(t *testing.T) {
	cfg := FromConfig(config.Lab{})
	if cfg.Runtime != RuntimeAuto {
		t.Fatalf("runtime = %q, want %q", cfg.Runtime, RuntimeAuto)
	}
	if cfg.ComposeFile != DefaultComposeFile {
		t.Fatalf("compose file = %q, want %q", cfg.ComposeFile, DefaultComposeFile)
	}
	if cfg.Project != DefaultProject {
		t.Fatalf("project = %q, want %q", cfg.Project, DefaultProject)
	}

	cfg = FromConfig(config.Lab{
		Runtime:     "Docker",
		ComposeFile: "fixtures/lab.yml",
		Project:     "proj",
		Services:    []string{"a", " ", "b"},
		Network:     "lab-net",
	})
	if cfg.Runtime != RuntimeDocker {
		t.Fatalf("runtime = %q, want %q", cfg.Runtime, RuntimeDocker)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("services = %v, want the two named ones", cfg.Services)
	}
}

// samePath compares two paths after cleaning them.
func samePath(a, b string) bool {
	absA, err := filepath.Abs(a)
	if err != nil {
		absA = filepath.Clean(a)
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		absB = filepath.Clean(b)
	}
	return absA == absB
}
