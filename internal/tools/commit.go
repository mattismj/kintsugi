package tools

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CommitGroup is one atomic commit the splitter carved out of the working tree.
type CommitGroup struct {
	Label   string   `json:"label"`
	Files   []string `json:"files"`
	Message string   `json:"message"`
	Order   int      `json:"order"` // dependency order: source < tests < docs
}

// CommitPlan is the dry-run output of kintsugi commit: the groups it would make.
type CommitPlan struct {
	Groups []CommitGroup `json:"groups"`
}

// CommitOptions controls the commit splitter.
type CommitOptions struct {
	Workspace string
	Apply     bool // actually run git commit per group
	Confirm   func() bool
}

// PlanCommits reads the working tree and splits unrelated changes into atomic
// commits ordered by dependency (source before tests/docs). It returns the
// groups without touching git when opts.Apply is false.
func PlanCommits(opts CommitOptions) (CommitPlan, error) {
	ws := opts.Workspace
	if ws == "" {
		return CommitPlan{}, fmt.Errorf("commit: no workspace")
	}
	changed, err := gitChangedFiles(ws)
	if err != nil {
		return CommitPlan{}, fmt.Errorf("commit: %w", err)
	}
	if len(changed) == 0 {
		return CommitPlan{}, nil
	}
	groups := classifyFiles(changed)
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Order < groups[j].Order
	})
	for i := range groups {
		groups[i].Message = generateMessage(ws, groups[i].Files)
		groups[i].Order = i
	}
	return CommitPlan{Groups: groups}, nil
}

// ApplyCommit runs git commit per group. It requires confirmation.
func ApplyCommit(plan CommitPlan, opts CommitOptions) error {
	if !plan.HasChanges() {
		return fmt.Errorf("commit: nothing to commit")
	}
	if opts.Confirm != nil && !opts.Confirm() {
		return fmt.Errorf("commit: aborted by operator")
	}
	for _, g := range plan.Groups {
		if err := runGitCommit(opts.Workspace, g); err != nil {
			return fmt.Errorf("commit: group %q: %w", g.Label, err)
		}
	}
	return nil
}

// HasChanges reports whether the plan has any groups.
func (p CommitPlan) HasChanges() bool {
	return len(p.Groups) > 0
}

// Format renders the plan as human-readable text.
func (p CommitPlan) Format() string {
	if len(p.Groups) == 0 {
		return "no changes to commit"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "commit plan (%d groups):\n", len(p.Groups))
	for i, g := range p.Groups {
		fmt.Fprintf(&b, "\n[%d] %s\n", i+1, g.Label)
		fmt.Fprintf(&b, "    files: %s\n", strings.Join(g.Files, ", "))
		fmt.Fprintf(&b, "    message: %s\n", g.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// gitChangedFiles returns the list of changed files in the workspace.
func gitChangedFiles(ws string) ([]string, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = ws
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		file := strings.TrimSpace(line[3:])
		if strings.Contains(file, " -> ") {
			parts := strings.SplitN(file, " -> ", 2)
			if len(parts) == 2 {
				file = parts[1]
			}
		}
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// classifyFiles groups files by their role: source, test, or doc.
func classifyFiles(files []string) []CommitGroup {
	var source, test, doc []string
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "_test.go"):
			test = append(test, f)
		case strings.HasSuffix(f, ".md"), strings.HasSuffix(f, ".txt"):
			doc = append(doc, f)
		default:
			source = append(source, f)
		}
	}
	var groups []CommitGroup
	if len(source) > 0 {
		groups = append(groups, CommitGroup{
			Label: "source changes",
			Files: source,
			Order: 0,
		})
	}
	if len(test) > 0 {
		groups = append(groups, CommitGroup{
			Label: "test changes",
			Files: test,
			Order: 1,
		})
	}
	if len(doc) > 0 {
		groups = append(groups, CommitGroup{
			Label: "documentation changes",
			Files: doc,
			Order: 2,
		})
	}
	return groups
}

// generateMessage creates a commit message from the diff of the given files.
func generateMessage(ws string, files []string) string {
	if len(files) == 0 {
		return "update files"
	}
	if len(files) == 1 {
		f := files[0]
		base := filepath.Base(f)
		ext := filepath.Ext(f)
		name := strings.TrimSuffix(base, ext)
		return fmt.Sprintf("update %s", name)
	}
	dirs := make(map[string]int)
	for _, f := range files {
		dir := filepath.Dir(f)
		if dir == "." {
			dir = "root"
		}
		dirs[dir]++
	}
	if len(dirs) == 1 {
		for dir := range dirs {
			return fmt.Sprintf("update %s", dir)
		}
	}
	return fmt.Sprintf("update %d files", len(files))
}

// runGitCommit stages the files in the group and commits them.
func runGitCommit(ws string, g CommitGroup) error {
	addArgs := append([]string{"add", "--"}, g.Files...)
	cmd := exec.Command("git", addArgs...)
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	cmd = exec.Command("git", "commit", "-m", g.Message)
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
