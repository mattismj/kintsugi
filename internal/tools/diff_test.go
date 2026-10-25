package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEditPopulatesDiff confirms the edit tool carries the before/after so the
// transcript can render an inline diff. Before is the file's content at call
// time; After is the post-edit content; Kind names the operation.
func TestEditPopulatesDiff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "edit", `{"path":"README.md","old":"hello","new":"hi"}`)
	if res.Failed {
		t.Fatalf("edit failed: %+v", res)
	}
	if len(res.Diff) != 1 {
		t.Fatalf("edit produced %d file changes, want 1", len(res.Diff))
	}
	fc := res.Diff[0]
	if fc.Path != "README.md" {
		t.Errorf("diff path %q, want README.md", fc.Path)
	}
	if fc.Kind != "edit" {
		t.Errorf("diff kind %q, want edit", fc.Kind)
	}
	if !strings.Contains(fc.Before, "hello") {
		t.Errorf("diff Before missing the old text: %q", fc.Before)
	}
	if !strings.Contains(fc.After, "hi") {
		t.Errorf("diff After missing the new text: %q", fc.After)
	}
	if strings.Contains(fc.After, "hello") {
		t.Errorf("diff After still contains the old text: %q", fc.After)
	}
}

// TestWriteNewFilePopulatesDiff confirms a write to a brand-new file carries
// an empty Before (there was no file) and the full content as After, tagged
// write-new so the renderer can label it "new file" rather than "replaced".
func TestWriteNewFilePopulatesDiff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "write", `{"path":"new.go","content":"package main\n"}`)
	if res.Failed {
		t.Fatalf("write failed: %+v", res)
	}
	fc := res.Diff[0]
	if fc.Kind != "write-new" {
		t.Errorf("kind %q, want write-new", fc.Kind)
	}
	if fc.Before != "" {
		t.Errorf("new file Before is %q, want empty", fc.Before)
	}
	if fc.After != "package main\n" {
		t.Errorf("After %q, want the written content", fc.After)
	}
}

// TestWriteReplacePopulatesDiff confirms a write over an existing file tags the
// change write-replace and carries both the old and new content, so the
// transcript shows the full-file diff for a deliberate replacement.
func TestWriteReplacePopulatesDiff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "write", `{"path":"README.md","content":"replaced\n"}`)
	if res.Failed {
		t.Fatalf("write failed: %+v", res)
	}
	fc := res.Diff[0]
	if fc.Kind != "write-replace" {
		t.Errorf("kind %q, want write-replace", fc.Kind)
	}
	if !strings.Contains(fc.Before, "hello") {
		t.Errorf("replace Before missing old content: %q", fc.Before)
	}
	if fc.After != "replaced\n" {
		t.Errorf("After %q, want the written content", fc.After)
	}
}

// TestWriteAppendPopulatesDiff confirms an append carries the existing content
// as Before and the concatenated content as After, so the diff shows only the
// appended lines as additions against the existing body.
func TestWriteAppendPopulatesDiff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "write", `{"path":"README.md","content":"more\n","append":true}`)
	if res.Failed {
		t.Fatalf("append failed: %+v", res)
	}
	fc := res.Diff[0]
	if fc.Kind != "append" {
		t.Errorf("kind %q, want append", fc.Kind)
	}
	if !strings.Contains(fc.Before, "hello") {
		t.Errorf("append Before missing existing content: %q", fc.Before)
	}
	if !strings.Contains(fc.After, "hello") || !strings.Contains(fc.After, "more") {
		t.Errorf("append After missing concatenated content: %q", fc.After)
	}
}

// TestApplyPatchPopulatesDiff confirms the patch envelope carries one
// FileChange per file touched, with the right kind for an add and an update.
func TestApplyPatchPopulatesDiff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	patch := "*** Begin Patch\n" +
		"*** Add File: added.txt\n" +
		"+line one\n" +
		"*** Update File: README.md\n" +
		"@@\n" +
		" hello\n" +
		"-world\n" +
		"+universe\n" +
		"*** End Patch\n"
	res := dispatch(t, ex, "apply_patch", `{"patch":`+`"`+strings.ReplaceAll(strings.ReplaceAll(patch, "\n", `\n`), `"`, `\"`)+`"`+`}`)
	if res.Failed {
		t.Fatalf("apply_patch failed: %+v", res)
	}
	if len(res.Diff) != 2 {
		t.Fatalf("apply_patch produced %d file changes, want 2 (add + update)", len(res.Diff))
	}
	// Order is the patch's declaration order: add first, then update.
	add := res.Diff[0]
	if !strings.HasSuffix(add.Path, "added.txt") || add.Kind != "patch-add" {
		t.Errorf("add change = %+v, want path ending in added.txt kind=patch-add", add)
	}
	if add.Before != "" || !strings.Contains(add.After, "line one") {
		t.Errorf("add before/after wrong: Before=%q After=%q", add.Before, add.After)
	}
	upd := res.Diff[1]
	if !strings.HasSuffix(upd.Path, "README.md") || upd.Kind != "patch-update" {
		t.Errorf("update change = %+v, want path ending in README.md kind=patch-update", upd)
	}
	if !strings.Contains(upd.Before, "world") {
		t.Errorf("update Before missing old text: %q", upd.Before)
	}
	if !strings.Contains(upd.After, "universe") {
		t.Errorf("update After missing new text: %q", upd.After)
	}
	if strings.Contains(upd.After, "world") {
		t.Errorf("update After still contains removed text: %q", upd.After)
	}
	// The file on disk reflects the patch, independent of the diff.
	body, _ := os.ReadFile(filepath.Join(ws, "README.md"))
	if !strings.Contains(string(body), "universe") {
		t.Errorf("disk file missing applied change: %q", body)
	}
}

// TestApplyPatchDeletePopulatesDiff confirms a delete carries the file's
// content as Before and an empty After, tagged patch-delete, so the renderer
// can show "deleted · N lines removed" without a wall of removal lines.
func TestApplyPatchDeletePopulatesDiff(t *testing.T) {
	ws := testWorkspace(t)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	patch := "*** Begin Patch\n*** Delete File: README.md\n*** End Patch\n"
	res := dispatch(t, ex, "apply_patch", `{"patch":`+`"`+strings.ReplaceAll(patch, "\n", `\n`)+`"`+`}`)
	if res.Failed {
		t.Fatalf("delete failed: %+v", res)
	}
	if len(res.Diff) != 1 {
		t.Fatalf("delete produced %d file changes, want 1", len(res.Diff))
	}
	fc := res.Diff[0]
	if fc.Kind != "patch-delete" {
		t.Errorf("kind %q, want patch-delete", fc.Kind)
	}
	if !strings.Contains(fc.Before, "hello") {
		t.Errorf("delete Before missing file content: %q", fc.Before)
	}
	if fc.After != "" {
		t.Errorf("delete After %q, want empty", fc.After)
	}
}

// TestApprovalCarriesTheProposedChange pins the informed-approval contract:
// the approver sees the diff before it is written, so y/n is answered with
// the change in front of the operator. The write only happens after the yes.
func TestApprovalCarriesTheProposedChange(t *testing.T) {
	ws := testWorkspace(t)
	var seen ApprovalRequest
	ex := &Executor{
		Workspace: ws,
		Policy:    Policy{}, // default mode: an edit is asked about
		Approver: ApproverFunc(func(ar ApprovalRequest) bool {
			seen = ar
			return false // deny: nothing may be written
		}),
	}
	res := dispatch(t, ex, "edit", `{"path":"README.md","old":"hello","new":"hi"}`)
	if !res.Failed {
		t.Fatal("a denied edit reported success")
	}
	if seen.Tool != "edit" {
		t.Fatalf("the request tool = %q, want edit", seen.Tool)
	}
	if len(seen.Changes) != 1 {
		t.Fatalf("the request carried %d changes, want 1", len(seen.Changes))
	}
	fc := seen.Changes[0]
	if fc.Kind != "edit" || !strings.Contains(fc.Before, "hello") || !strings.Contains(fc.After, "hi") {
		t.Fatalf("the request's change is wrong: %+v", fc)
	}
	// Denial means denial: the file is untouched.
	body, _ := os.ReadFile(filepath.Join(ws, "README.md"))
	if string(body) != "hello\nworld\n" {
		t.Fatalf("a denied edit mutated the file: %q", body)
	}
}
