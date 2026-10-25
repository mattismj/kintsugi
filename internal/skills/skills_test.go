package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDiscoverIndexAndFind(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".kintsugi", "skills")
	write(t, filepath.Join(dir, "deploy", "SKILL.md"), `---
name: deploy
description: Build and deploy the lab stack.
---
# Deploy

Run make deploy.
`)
	write(t, filepath.Join(dir, "triage.md"), `# Triage

Check the logs first.
`)
	write(t, filepath.Join(dir, "nested", "deep", "SKILL.md"), `---
name: deep
description: Nested skill.
---
Body.
`)
	write(t, filepath.Join(dir, "nested", "notes.md"), `not a skill`)
	write(t, filepath.Join(dir, "broken", "SKILL.md"), `---
name: broken
`)
	write(t, filepath.Join(dir, "empty.md"), ``)

	list := Load(ws, dir)
	if len(list) != 3 {
		t.Fatalf("loaded %d skills: %+v", len(list), list)
	}
	idx := Index(list)
	for _, want := range []string{"`deploy`", "Build and deploy the lab stack.", "`triage`", "Check the logs first.", "`deep`", ".kintsugi/skills/deploy/SKILL.md"} {
		if !strings.Contains(idx, want) {
			t.Fatalf("index missing %q:\n%s", want, idx)
		}
	}
	if strings.Contains(idx, "Run make deploy") {
		t.Fatal("skill body leaked into the injected index")
	}
	sk, ok := Find(list, "DEPLOY")
	if !ok || !strings.Contains(sk.Body, "make deploy") {
		t.Fatalf("find = %+v ok=%v", sk, ok)
	}
	if _, ok := Find(list, "missing"); ok {
		t.Fatal("missing skill resolved")
	}
}

func TestDirAndEmptyIndex(t *testing.T) {
	ws := t.TempDir()
	if got := Dir(ws, ""); got != filepath.Join(ws, DefaultDir) {
		t.Fatalf("default dir = %q", got)
	}
	if got := Dir(ws, "/tmp/custom"); got != "/tmp/custom" {
		t.Fatalf("absolute dir = %q", got)
	}
	if Index(nil) != "" {
		t.Fatal("empty index should be empty")
	}
}
