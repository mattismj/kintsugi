package main

// Tests for `kintsugi lab up|down|status`. The container runtime is a fake on
// PATH: the command's own behaviour (the printed lines, the lock, the
// teardown) is what is checked here, never a real container.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/lab"
)

// fakeLabRuntime installs a fake container runtime on PATH.
func fakeLabRuntime(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"log=\"${0%/*}/calls.log\"\n" +
		"printf '%s\\n' \"$*\" >> \"$log\"\n" +
		"if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"version\" ]; then\n" +
		"  echo \"Docker Compose version v2.29.0\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return filepath.Join(dir, "calls.log")
}

// labWorkspace is a workspace with a [lab] block and a compose file.
func labWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if err := os.MkdirAll(filepath.Join(ws, "lab"), 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  fixture:\n    image: alpine:3\n    command: [\"sleep\", \"infinity\"]\n"
	if err := os.WriteFile(filepath.Join(ws, "lab", "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "[lab]\n" +
		"runtime = \"docker\"\n" +
		"compose_file = \"lab/docker-compose.yml\"\n" +
		"project = \"kintsugi-lab\"\n" +
		"services = [\"fixture\"]\n"
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestLabUpPrintsTheRuntimeProjectServicesAndLock(t *testing.T) {
	fakeLabRuntime(t)
	ws := labWorkspace(t)

	out := captureStdout(t, func() { cmdLab(ws, []string{"up"}) })

	for _, want := range []string{"docker", "kintsugi-lab", "fixture", lab.LockPath(ws)} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output does not mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "lab up") {
		t.Fatalf("the output does not say what happened:\n%s", out)
	}
	if _, err := os.Stat(lab.LockPath(ws)); err != nil {
		t.Fatalf("the lock was not written: %v", err)
	}
}

func TestLabStatusReportsTheRunningLab(t *testing.T) {
	fakeLabRuntime(t)
	ws := labWorkspace(t)
	captureStdout(t, func() { cmdLab(ws, []string{"up"}) })

	out := captureStdout(t, func() { cmdLab(ws, []string{"status"}) })

	if !strings.Contains(out, "up") {
		t.Fatalf("the status does not say the lab is up:\n%s", out)
	}
	if !strings.Contains(out, "docker") {
		t.Fatalf("the status does not name the runtime:\n%s", out)
	}
	if !strings.Contains(out, "kintsugi-lab") {
		t.Fatalf("the status does not name the project:\n%s", out)
	}
	if !strings.Contains(out, "ago") {
		t.Fatalf("the status does not report the lock's age:\n%s", out)
	}
}

func TestLabStatusSaysDownWithoutALock(t *testing.T) {
	fakeLabRuntime(t)
	ws := labWorkspace(t)

	out := captureStdout(t, func() { cmdLab(ws, []string{"status"}) })

	if !strings.Contains(out, "down") {
		t.Fatalf("the status does not say the lab is down:\n%s", out)
	}
	if strings.Contains(out, "runtime  docker") {
		t.Fatalf("the status reports a runtime for a lab that is not up:\n%s", out)
	}
}

func TestLabDownReportsWhatItRemoved(t *testing.T) {
	log := fakeLabRuntime(t)
	ws := labWorkspace(t)
	captureStdout(t, func() { cmdLab(ws, []string{"up"}) })

	out := captureStdout(t, func() { cmdLab(ws, []string{"down"}) })

	if !strings.Contains(out, "removed") {
		t.Fatalf("the output does not report the teardown:\n%s", out)
	}
	if !strings.Contains(out, "kintsugi-lab") {
		t.Fatalf("the output does not name the project:\n%s", out)
	}
	if !strings.Contains(out, lab.LockPath(ws)) {
		t.Fatalf("the output does not name the lock:\n%s", out)
	}
	if _, err := os.Stat(lab.LockPath(ws)); !os.IsNotExist(err) {
		t.Fatal("the lock survived the teardown")
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "down -v") {
		t.Fatalf("the teardown did not run compose down -v:\n%s", data)
	}
}

func TestLabDownOnALabThatIsNotUp(t *testing.T) {
	fakeLabRuntime(t)
	ws := labWorkspace(t)

	out := captureStdout(t, func() { cmdLab(ws, []string{"down"}) })

	if !strings.Contains(out, "nothing was up") {
		t.Fatalf("the output does not say that nothing was up:\n%s", out)
	}
}

func TestLabUsageNamesEverySubcommand(t *testing.T) {
	u := labUsageText()
	for _, want := range []string{"kintsugi lab up", "kintsugi lab down", "kintsugi lab status", "run_in_lab"} {
		if !strings.Contains(u, want) {
			t.Errorf("the usage text does not mention %q:\n%s", want, u)
		}
	}
}
