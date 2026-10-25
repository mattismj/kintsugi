package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kintsugi/internal/lab"
)

// writeLabLock puts a lab lock in the workspace, the way `kintsugi lab up`
// would.
func writeLabLock(t *testing.T, ws, service string) {
	t.Helper()
	if err := lab.WriteLock(ws, lab.Lock{
		Project:     "kintsugi-lab",
		Runtime:     lab.RuntimeDocker,
		ComposeFile: lab.DefaultComposeFile,
		StartedAt:   time.Now().UTC(),
		Workspace:   ws,
		Services:    []string{service},
	}); err != nil {
		t.Fatal(err)
	}
}

// captureLabExec replaces the compose seam for one test and returns the argv
// and the environment it was handed.
func captureLabExec(t *testing.T, out string, code int) (argv *[]string, env *[]string) {
	t.Helper()
	var got []string
	var gotEnv []string
	old := labExec
	labExec = func(ctx context.Context, a []string, dir string, e []string) (string, int, error) {
		got = a
		gotEnv = e
		return out, code, nil
	}
	t.Cleanup(func() { labExec = old })
	return &got, &gotEnv
}

func TestRunInLabWithoutALabFails(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "run_in_lab", `{"command":"echo hi"}`)
	if !res.Failed {
		t.Fatalf("a command ran without a lab: %+v", res)
	}
	if !strings.Contains(res.Content, "no lab is up") {
		t.Fatalf("the refusal does not say what to do: %+v", res)
	}
	if !strings.Contains(res.Content, "kintsugi lab up") {
		t.Fatalf("the refusal does not name the command that starts a lab: %+v", res)
	}
}

func TestRunInLabBuildsTheComposeExecArgv(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")
	got, gotEnv := captureLabExec(t, "hello from the lab\n", 0)

	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Lab:       &lab.Config{Services: []string{"fixture"}},
	}
	res := dispatch(t, ex, "run_in_lab", `{"command":"echo hi"}`)
	if res.Failed {
		t.Fatalf("the lab command failed: %+v", res)
	}
	if !strings.Contains(res.Content, "hello from the lab") {
		t.Fatalf("the lab output is missing: %+v", res)
	}
	if !strings.Contains(res.Content, "[exit 0]") {
		t.Fatalf("the exit code is missing: %+v", res)
	}

	want := []string{"docker", "compose", "-p", "kintsugi-lab", "exec", "-T", "fixture", "sh", "-lc", "echo hi"}
	argv := *got
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}

	// The compose client finds the lab's compose file through the standard
	// compose environment variable: the argv stays the documented one.
	joined := strings.Join(*gotEnv, " ")
	if !strings.Contains(joined, "COMPOSE_FILE="+filepath.Join(ws, "lab", "docker-compose.yml")) {
		t.Fatalf("the compose file is not named: %v", *gotEnv)
	}
	if !strings.Contains(joined, "COMPOSE_PROJECT_NAME=kintsugi-lab") {
		t.Fatalf("the project is not named: %v", *gotEnv)
	}
}

func TestRunInLabNeverRunsTheCommandOnTheHost(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")
	got, _ := captureLabExec(t, "", 0)

	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Lab:       &lab.Config{Services: []string{"fixture"}},
	}
	res := dispatch(t, ex, "run_in_lab", `{"command":"curl https://evil.example"}`)
	if res.Failed {
		t.Fatalf("the lab command failed: %+v", res)
	}
	argv := *got
	// The command is the argument of the in-container shell. No host shell
	// ever sees it.
	for i, a := range argv {
		if a == "bash" {
			t.Fatalf("the argv runs a host shell: %v", argv)
		}
		if a == "sh" && (i != len(argv)-3 || argv[i+1] != "-lc") {
			t.Fatalf("the argv runs a shell outside the compose exec: %v", argv)
		}
	}
	if len(argv) < 3 || argv[len(argv)-3] != "sh" || argv[len(argv)-2] != "-lc" {
		t.Fatalf("the command is not the argument of `sh -lc`: %v", argv)
	}
	if argv[len(argv)-1] != "curl https://evil.example" {
		t.Fatalf("the command is not the last argument: %v", argv)
	}
	// And the compose client is the only host process: it is the argv[0].
	if argv[0] != "docker" {
		t.Fatalf("the host process is not the container runtime: %v", argv)
	}
}

func TestRunInLabReportsTheExitCode(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")
	captureLabExec(t, "boom\n", 3)

	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Lab:       &lab.Config{Services: []string{"fixture"}},
	}
	res := dispatch(t, ex, "run_in_lab", `{"command":"false"}`)
	if !res.Failed {
		t.Fatalf("a failing lab command was reported as a success: %+v", res)
	}
	if !strings.Contains(res.Content, "[exit 3]") {
		t.Fatalf("the exit code is missing: %+v", res)
	}
}

func TestRunInLabRefusesAnUnconfiguredService(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")
	captureLabExec(t, "", 0)

	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{Yolo: true},
		Lab:       &lab.Config{Services: []string{"fixture"}},
	}
	res := dispatch(t, ex, "run_in_lab", `{"command":"echo hi","service":"other"}`)
	if !res.Failed {
		t.Fatalf("an unconfigured service was used: %+v", res)
	}
	if !strings.Contains(res.Content, "not one of the configured services") {
		t.Fatalf("the refusal does not say why: %+v", res)
	}
}

func TestRunInLabRefusesALockFromAnotherWorkspace(t *testing.T) {
	ws := testWorkspace(t)
	other := t.TempDir()
	if err := lab.WriteLock(ws, lab.Lock{
		Project:   "kintsugi-lab",
		Runtime:   lab.RuntimeDocker,
		StartedAt: time.Now().UTC(),
		Workspace: other,
	}); err != nil {
		t.Fatal(err)
	}
	captureLabExec(t, "", 0)

	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "run_in_lab", `{"command":"echo hi"}`)
	if !res.Failed {
		t.Fatalf("a foreign lab lock was used: %+v", res)
	}
	if !strings.Contains(res.Content, "another workspace") {
		t.Fatalf("the refusal does not say why: %+v", res)
	}
}

func TestRunInLabNeedsACommand(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")
	captureLabExec(t, "", 0)

	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "run_in_lab", `{"service":"fixture"}`)
	if !res.Failed || !strings.Contains(res.Content, "command") {
		t.Fatalf("an empty command was not refused: %+v", res)
	}
}

// TestBashIsRefusedWhileTheLabIsUp is the lab.lock guarantee: while the lab
// is up, a command may only run inside it.
func TestBashIsRefusedWhileTheLabIsUp(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")

	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "bash", `{"command":"touch pwned.txt"}`)
	if !res.Failed {
		t.Fatalf("a host command ran while the lab was up: %+v", res)
	}
	if !strings.Contains(res.Content, "a lab is up") {
		t.Fatalf("the refusal does not say why: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "pwned.txt")); err == nil {
		t.Fatal("the host command ran while the lab was up")
	}
}

// TestBashRunsAgainAfterTheLabIsDown checks the other half: the guard lifts
// with the lock.
func TestBashRunsAgainAfterTheLabIsDown(t *testing.T) {
	ws := testWorkspace(t)
	writeLabLock(t, ws, "fixture")
	if err := lab.RemoveLock(ws); err != nil {
		t.Fatal(err)
	}

	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "bash", `{"command":"echo hi"}`)
	if res.Failed || !strings.Contains(res.Content, "hi") {
		t.Fatalf("the host command did not run after the lab went down: %+v", res)
	}
}

func TestRunInLabToolIsRegistered(t *testing.T) {
	ex := &Executor{Workspace: t.TempDir()}
	found := false
	for _, d := range ex.Definitions() {
		if d.Function.Name == "run_in_lab" {
			found = true
			if len(d.Function.Parameters.Required) != 1 || d.Function.Parameters.Required[0] != "command" {
				t.Fatalf("the schema does not require the command: %+v", d.Function.Parameters)
			}
			for _, key := range []string{"service", "timeout_s"} {
				if _, ok := d.Function.Parameters.Properties[key]; !ok {
					t.Fatalf("the schema has no %q property: %+v", key, d.Function.Parameters)
				}
			}
		}
	}
	if !found {
		t.Fatal("run_in_lab is not in the tool definitions")
	}
}
