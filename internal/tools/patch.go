package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kintsugi/internal/provider"
)

// writeTool and applyPatchTool mirror the file-authoring tools that upstream
// harnesses converged on: a whole-file writer for new files, and a single
// envelope that can add/update/delete/move several files in one approved call.
// The envelope is the Codex apply_patch grammar.

func writeTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "write",
			Description: "Write a whole UTF-8 file in the workspace, creating parent directories. Use edit for a targeted change; use write for new files or deliberate full replacement.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"path", "content"},
				Properties: map[string]provider.ToolProp{
					"path":    {Type: "string", Description: "Workspace-relative file path."},
					"content": {Type: "string", Description: "Complete file content."},
					"append":  {Type: "boolean", Description: "Append instead of replacing (default false)."},
				},
			},
		},
	}
}

func applyPatchTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "apply_patch",
			Description: "Apply one patch envelope containing additions, updates, deletions, or moves. Update hunks use context lines (space), removals (-), and additions (+). Prefer this over many edit calls when changing several files.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"patch"},
				Properties: map[string]provider.ToolProp{
					"patch": {Type: "string", Description: "Patch text beginning with *** Begin Patch and ending with *** End Patch."},
				},
			},
		},
	}
}

func (e *Executor) write(args string) Result {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return Result{Content: `{"error":"write: missing required property 'path'"}`, Failed: true}
	}
	p, err := e.resolve(a.Path)
	if err != nil {
		return failJSON(err)
	}
	verb := "write"
	if a.Append {
		verb = "append"
	}
	// Capture the before state first so the approval carries the proposed
	// change: the operator answers y/n with the diff in front of them. A
	// read failure is not a tool failure — the diff is simply empty, same as
	// a brand-new file written whole.
	before := ""
	if beforeData, rerr := os.ReadFile(p); rerr == nil {
		before = string(beforeData)
	}
	kind := "write-new"
	if a.Append {
		kind = "append"
	} else if before != "" {
		kind = "write-replace"
	}
	after := a.Content
	if a.Append {
		after = before + a.Content
	}
	if err := e.authorize("write", a.Path, fmt.Sprintf("%s %s (%d bytes)", verb, a.Path, len(a.Content)),
		FileChange{Path: a.Path, Kind: kind, Before: before, After: after}); err != nil {
		return failJSON(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return failJSON(err)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if a.Append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(p, flags, 0o644)
	if err != nil {
		return failJSON(err)
	}
	if _, err := f.WriteString(a.Content); err != nil {
		f.Close()
		return failJSON(err)
	}
	if err := f.Close(); err != nil {
		return failJSON(err)
	}
	info, _ := os.Stat(p)
	size := int64(-1)
	if info != nil {
		size = info.Size()
	}
	return Result{
		Content: fmt.Sprintf(`{"ok":true,"path":%q,"append":%v,"bytes":%d}`, a.Path, a.Append, size),
		Diff:    []FileChange{{Path: a.Path, Kind: kind, Before: before, After: after}},
	}
}

// ---------- apply_patch envelope ----------

type patchKind int

const (
	patchAdd patchKind = iota
	patchUpdate
	patchDelete
)

type patchLine struct {
	kind byte // ' ', '-', '+'
	text string
}

type patchOp struct {
	kind   patchKind
	path   string
	moveTo string
	add    []string
	hunks  [][]patchLine
}

func parsePatch(raw string) ([]patchOp, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i]), "*** Begin Patch") {
		return nil, fmt.Errorf("patch must start with *** Begin Patch")
	}
	i++

	var ops []patchOp
	var cur *patchOp
	var hunk []patchLine
	ended := false
	flushHunk := func() {
		if cur == nil || cur.kind != patchUpdate || len(hunk) == 0 {
			hunk = nil
			return
		}
		cur.hunks = append(cur.hunks, hunk)
		hunk = nil
	}
	flushOp := func() {
		flushHunk()
		if cur != nil {
			ops = append(ops, *cur)
			cur = nil
		}
	}

	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "*** End Patch") {
			flushOp()
			ended = true
			i++
			break
		}
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			flushOp()
			cur = &patchOp{kind: patchAdd, path: strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))}
		case strings.HasPrefix(line, "*** Update File: "):
			flushOp()
			cur = &patchOp{kind: patchUpdate, path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))}
		case strings.HasPrefix(line, "*** Delete File: "):
			flushOp()
			cur = &patchOp{kind: patchDelete, path: strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))}
		case strings.HasPrefix(line, "*** Move to: "):
			if cur == nil || cur.kind != patchUpdate {
				return nil, fmt.Errorf("*** Move to requires an update section")
			}
			cur.moveTo = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: "))
		case strings.HasPrefix(trimmed, "@@"):
			flushHunk()
		case strings.HasPrefix(trimmed, "*** End of File"):
			continue
		default:
			if cur == nil {
				if strings.TrimSpace(line) == "" {
					continue
				}
				return nil, fmt.Errorf("patch content before first file section: %q", line)
			}
			if cur.kind == patchAdd {
				if !strings.HasPrefix(line, "+") {
					return nil, fmt.Errorf("add file %s: every content line must start with '+': %q", cur.path, line)
				}
				cur.add = append(cur.add, strings.TrimPrefix(line, "+"))
				continue
			}
			if cur.kind == patchDelete {
				if strings.TrimSpace(line) == "" {
					continue
				}
				return nil, fmt.Errorf("delete file %s: unexpected content", cur.path)
			}
			if line == "" {
				hunk = append(hunk, patchLine{kind: ' ', text: ""})
				continue
			}
			switch line[0] {
			case ' ', '+', '-':
				hunk = append(hunk, patchLine{kind: line[0], text: line[1:]})
			default:
				return nil, fmt.Errorf("update %s: hunk line must start with space, '+', or '-': %q", cur.path, line)
			}
		}
	}
	flushOp()
	if !ended {
		return nil, fmt.Errorf("patch is missing *** End Patch")
	}
	if hasNonEmpty(lines[i:]) {
		return nil, fmt.Errorf("unexpected content after *** End Patch")
	}
	for _, op := range ops {
		if op.path == "" {
			return nil, fmt.Errorf("patch section has an empty path")
		}
		if op.kind == patchUpdate && len(op.hunks) == 0 && op.moveTo == "" {
			return nil, fmt.Errorf("update %s: no hunks", op.path)
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("patch contains no file operations")
	}
	return ops, nil
}

func hasNonEmpty(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return true
		}
	}
	return false
}

func (e *Executor) applyPatch(args string) Result {
	var a struct {
		Patch string `json:"patch"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return failJSON(err)
	}

	type change struct {
		path    string
		remove  bool
		content string
		// before/after carry the whole-file contents for diff rendering, and
		// kind names the operation so the transcript card can label it without
		// re-deriving it from the strings. before is "" for an add; after is ""
		// for a delete.
		before string
		after  string
		kind   string
	}
	var changes []change
	var names []string
	seen := map[string]bool{}
	adds, dels := 0, 0
	for _, op := range ops {
		p, err := e.resolve(op.path)
		if err != nil {
			return failJSON(err)
		}
		if seen[p] {
			return Result{Content: fmt.Sprintf(`{"error":"two patch operations target %s"}`, op.path), Failed: true}
		}
		seen[p] = true
		names = append(names, op.path)
		switch op.kind {
		case patchAdd:
			if _, err := os.Stat(p); err == nil {
				return Result{Content: fmt.Sprintf(`{"error":"add file %s: file already exists"}`, op.path), Failed: true}
			}
			body := strings.Join(op.add, "\n")
			if body != "" {
				body += "\n"
			}
			changes = append(changes, change{path: p, content: body, after: body, kind: "patch-add"})
			adds += len(op.add)
		case patchDelete:
			data, err := os.ReadFile(p)
			if err != nil {
				return failJSON(err)
			}
			changes = append(changes, change{path: p, remove: true, before: string(data), kind: "patch-delete"})
			dels++
		case patchUpdate:
			data, err := os.ReadFile(p)
			if err != nil {
				return failJSON(err)
			}
			updated, err := applyHunks(string(data), op.hunks)
			if err != nil {
				return Result{Content: fmt.Sprintf(`{"error":"update %s: %s"}`, op.path, err.Error()), Failed: true}
			}
			target := p
			if op.moveTo != "" {
				target, err = e.resolve(op.moveTo)
				if err != nil {
					return failJSON(err)
				}
				if _, err := os.Stat(target); err == nil {
					return Result{Content: fmt.Sprintf(`{"error":"move target %s already exists"}`, op.moveTo), Failed: true}
				}
				if seen[target] {
					return Result{Content: fmt.Sprintf(`{"error":"two patch operations target %s"}`, op.moveTo), Failed: true}
				}
				seen[target] = true
				// A move is a delete of the source plus a write of the target
				// with the updated content; both halves are diffed.
				changes = append(changes, change{path: p, remove: true, before: string(data), kind: "patch-move"})
			}
			kind := "patch-update"
			if op.moveTo != "" {
				kind = "patch-move"
			}
			changes = append(changes, change{path: target, content: updated, before: "", after: updated, kind: kind})
			if op.moveTo == "" {
				// The before for a plain update is the file's current content;
				// for a move's target half there is no before (the file is new).
				changes[len(changes)-1].before = string(data)
			}
			for _, h := range op.hunks {
				for _, l := range h {
					switch l.kind {
					case '+':
						adds++
					case '-':
						dels++
					}
				}
			}
		}
	}

	detail := fmt.Sprintf("%d file(s): %s (+%d/-%d)", len(changes), strings.Join(names, ", "), adds, dels)
	// The whole envelope's proposed changes ride the approval: multi-file
	// patches are approved with every diff in front of the operator.
	proposed := make([]FileChange, 0, len(changes))
	for _, c := range changes {
		fc := FileChange{Path: c.path, Kind: c.kind, Before: c.before, After: c.after}
		if c.remove {
			fc.After = ""
		}
		proposed = append(proposed, fc)
	}
	if err := e.authorize("apply_patch", detail, detail, proposed...); err != nil {
		return failJSON(err)
	}
	for _, c := range changes {
		if c.remove {
			if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
				return failJSON(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
			return failJSON(err)
		}
		if err := os.WriteFile(c.path, []byte(c.content), 0o644); err != nil {
			return failJSON(err)
		}
	}
	fileChanges := make([]FileChange, 0, len(changes))
	for _, c := range changes {
		fc := FileChange{Path: c.path, Kind: c.kind, Before: c.before, After: c.after}
		if c.remove {
			fc.After = ""
		}
		fileChanges = append(fileChanges, fc)
	}
	return Result{
		Content: fmt.Sprintf(`{"ok":true,"files":%d,"added":%d,"deleted":%d}`, len(changes), adds, dels),
		Diff:    fileChanges,
	}
}

func applyHunks(content string, hunks [][]patchLine) (string, error) {
	crlf := strings.Contains(content, "\r\n")
	text := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for hi, h := range hunks {
		var oldLines, newLines []string
		for _, l := range h {
			switch l.kind {
			case ' ':
				oldLines = append(oldLines, l.text)
				newLines = append(newLines, l.text)
			case '-':
				oldLines = append(oldLines, l.text)
			case '+':
				newLines = append(newLines, l.text)
			}
		}
		if len(oldLines) == 0 {
			return "", fmt.Errorf("hunk %d has no context or removal lines; cannot locate insertion", hi+1)
		}
		idx, count := findLines(lines, oldLines)
		if count == 0 {
			return "", fmt.Errorf("hunk %d did not match (searched %d lines)", hi+1, len(oldLines))
		}
		if count > 1 {
			return "", fmt.Errorf("hunk %d matches %d locations; add more context", hi+1, count)
		}
		next := make([]string, 0, len(lines)-len(oldLines)+len(newLines))
		next = append(next, lines[:idx]...)
		next = append(next, newLines...)
		next = append(next, lines[idx+len(oldLines):]...)
		lines = next
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out, nil
}

func findLines(haystack, needle []string) (int, int) {
	if len(needle) == 0 {
		return -1, 0
	}
	first, count := -1, 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			if first < 0 {
				first = i
			}
			count++
		}
	}
	return first, count
}
