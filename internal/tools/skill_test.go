package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

func TestSkillToolListsAndLoads(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".kintsugi", "skills")
	if err := os.MkdirAll(filepath.Join(dir, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: deploy\ndescription: Deploy the lab.\n---\n\n# Deploy\n\nRun make deploy.\n"
	if err := os.WriteFile(filepath.Join(dir, "deploy", "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{Workspace: ws, SkillsDir: dir}
	found := false
	for _, d := range ex.Definitions() {
		if d.Function.Name == "skill" {
			found = true
		}
	}
	if !found {
		t.Fatal("skill tool not registered")
	}

	res := ex.Dispatch(provider.ToolCall{Function: provider.Function{Name: "skill", Arguments: `{"name":"deploy"}`}})
	if res.Failed || !strings.Contains(res.Content, "Run make deploy.") || strings.Contains(res.Content, "description:") {
		t.Fatalf("load result = %+v", res)
	}
	res = ex.Dispatch(provider.ToolCall{Function: provider.Function{Name: "skill", Arguments: `{}`}})
	if res.Failed || !strings.Contains(res.Content, "`deploy`") || strings.Contains(res.Content, "Run make deploy.") {
		t.Fatalf("list result = %+v", res)
	}
	res = ex.Dispatch(provider.ToolCall{Function: provider.Function{Name: "skill", Arguments: `{"name":"missing"}`}})
	if !res.Failed || !strings.Contains(res.Content, "available") {
		t.Fatalf("missing result = %+v", res)
	}
}
