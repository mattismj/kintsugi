package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreateAppendOverwrite(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "write", `{"path":"notes/today.md","content":"first\n"}`)
	if res.Failed {
		t.Fatalf("create failed: %+v", res)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "notes/today.md")); string(body) != "first\n" {
		t.Fatalf("unexpected create body: %q", body)
	}

	res = dispatch(t, ex, "write", `{"path":"notes/today.md","content":"second\n","append":true}`)
	if res.Failed {
		t.Fatalf("append failed: %+v", res)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "notes/today.md")); string(body) != "first\nsecond\n" {
		t.Fatalf("unexpected append body: %q", body)
	}

	res = dispatch(t, ex, "write", `{"file_path":"notes/today.md","text":"replaced\n"}`)
	if res.Failed {
		t.Fatalf("alias repair failed: %+v", res)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "notes/today.md")); string(body) != "replaced\n" {
		t.Fatalf("unexpected overwrite body: %q", body)
	}
}

func TestWritePathGuardsAndApproval(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{DenyPaths: DefaultDenyPaths, Yolo: true}}

	res := dispatch(t, ex, "write", `{"path":"../escape.txt","content":"x"}`)
	if !res.Failed || !strings.Contains(res.Content, "escapes the workspace") {
		t.Fatalf("escape not blocked: %+v", res)
	}
	res = dispatch(t, ex, "write", `{"path":".env","content":"x"}`)
	if !res.Failed || !strings.Contains(res.Content, "denied by policy") {
		t.Fatalf("denied path not blocked: %+v", res)
	}

	noApproval := &Executor{Workspace: ws, Policy: Policy{Yolo: false}}
	res = dispatch(t, noApproval, "write", `{"path":"ok.txt","content":"x"}`)
	if !res.Failed || !strings.Contains(res.Content, "requires approval") {
		t.Fatalf("missing approver not enforced: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "ok.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write created a file")
	}
}

func TestApplyPatchAddUpdateDelete(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	patch := `*** Begin Patch
*** Add File: docs/new.md
+# New
+
+body
*** Update File: README.md
@@
 hello
-world
+kintsugi
*** Delete File: dup.txt
*** End Patch
`
	res := dispatch(t, ex, "apply_patch", `{"patch":`+jsonString(patch)+`}`)
	if res.Failed {
		t.Fatalf("patch failed: %+v", res)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "README.md")); string(body) != "hello\nkintsugi\n" {
		t.Fatalf("update wrong: %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "docs/new.md")); string(body) != "# New\n\nbody\n" {
		t.Fatalf("add wrong: %q", body)
	}
	if _, err := os.Stat(filepath.Join(ws, "dup.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete left file behind")
	}
}

func TestApplyPatchMultiHunkAndMove(t *testing.T) {
	ws := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "multi.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "move.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	patch := `*** Begin Patch
*** Update File: multi.txt
@@
 one
-two
+TWO
@@
 three
-four
+FOUR
*** Update File: move.txt
*** Move to: moved/move.txt
@@
-a
+b
*** End Patch
`
	res := dispatch(t, ex, "apply_patch", `{"patch":`+jsonString(patch)+`}`)
	if res.Failed {
		t.Fatalf("patch failed: %+v", res)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "multi.txt")); string(body) != "one\nTWO\nthree\nFOUR\n" {
		t.Fatalf("multi-hunk wrong: %q", body)
	}
	if _, err := os.Stat(filepath.Join(ws, "move.txt")); !os.IsNotExist(err) {
		t.Fatalf("move left old file behind")
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "moved/move.txt")); string(body) != "b\n" {
		t.Fatalf("move body wrong: %q", body)
	}
}

func TestApplyPatchIsAtomic(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	patch := `*** Begin Patch
*** Update File: README.md
@@
 hello
-world
+kintsugi
*** Update File: src/main.go
@@
 package main
-func nope
+func yes
*** End Patch
`
	res := dispatch(t, ex, "apply_patch", `{"patch":`+jsonString(patch)+`}`)
	if !res.Failed || !strings.Contains(res.Content, "did not match") {
		t.Fatalf("bad hunk not rejected: %+v", res)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "README.md")); string(body) != "hello\nworld\n" {
		t.Fatalf("partial patch mutated first file: %q", body)
	}
}

func TestApplyPatchGuards(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{DenyPaths: DefaultDenyPaths, Yolo: true}}

	res := dispatch(t, ex, "apply_patch", `{"patch":"*** Begin Patch\n*** Add File: ../escape.txt\n+x\n*** End Patch"}`)
	if !res.Failed || !strings.Contains(res.Content, "escapes the workspace") {
		t.Fatalf("escape not blocked: %+v", res)
	}
	res = dispatch(t, ex, "apply_patch", `{"patch":"*** Begin Patch\n*** Update File: .env\n@@\n-API_KEY=super-secret-value\n+API_KEY=x\n*** End Patch"}`)
	if !res.Failed || !strings.Contains(res.Content, "denied by policy") {
		t.Fatalf("denied path not blocked: %+v", res)
	}
	res = dispatch(t, ex, "apply_patch", `*** Begin Patch
*** Add File: same.txt
+x
*** Add File: same.txt
+y
*** End Patch`)
	if !res.Failed || !strings.Contains(res.Content, "two patch operations") {
		t.Fatalf("duplicate target not rejected: %+v", res)
	}
}

func TestApplyPatchRepairsFreeformEnvelope(t *testing.T) {
	rep, err := Repair("apply_patch", "*** Begin Patch\n*** Add File: a.txt\n+x\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Args, `"patch"`) || !strings.Contains(rep.Args, "Begin Patch") {
		t.Fatalf("freeform envelope not wrapped: %s", rep.Args)
	}
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
