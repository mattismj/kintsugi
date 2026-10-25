// Package agents loads the Markdown-defined agents (M9.3). An agent is a
// Markdown file with YAML frontmatter: the frontmatter says who the agent is
// and what it may do, the body is its prompt.
//
// Two tiers are read, most specific first:
//
//	<workspace>/.kintsugi/agents/*.md
//	<user config dir>/agents/*.md
//
// The workspace wins on a name clash: a project agent shadows the global one
// of the same name. Loading is deliberately tolerant — a file with broken
// frontmatter is skipped, never fatal, because one bad agent file must not
// take a session down.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"kintsugi/internal/config"
)

// Agent is one Markdown-defined agent.
type Agent struct {
	Name           string   // the name @mention and Select use
	Description    string   // one line: what this agent is for
	Model          string   // a model ref (provider/model) or a role name
	PermissionMode string   // default | plan | yolo | ...
	Tools          []string // the tool allowlist
	MaxTurns       int      // the agent's own turn cap; 0 means the session's
	Background     bool     // the agent may run detached
	Path           string   // the file it came from
	Body           string   // the prompt
}

// DefaultTools is the tool set an agent gets when it declares none: the
// read-only floor. An agent that may write says so.
var DefaultTools = []string{"read", "list", "glob", "grep"}

// ReadOnlyTools is the floor a plan-mode agent is held to, whatever its
// allowlist says.
var ReadOnlyTools = []string{"read", "list", "glob", "grep"}

// WorkspaceDir is the workspace tier: <workspace>/.kintsugi/agents.
func WorkspaceDir(workspace string) string {
	return filepath.Join(workspace, ".kintsugi", "agents")
}

// GlobalDir is the user tier: the agents folder beside the harness config
// (honours XDG_CONFIG_HOME, so it is ~/.config/kintsugi/agents by default).
func GlobalDir() string {
	return filepath.Join(filepath.Dir(config.GlobalPath()), "agents")
}

// Dirs returns the agent tiers for a workspace, most specific first.
func Dirs(workspace string) []string {
	var dirs []string
	if strings.TrimSpace(workspace) != "" {
		dirs = append(dirs, WorkspaceDir(workspace))
	}
	return append(dirs, GlobalDir())
}

// Load reads the agent files from every directory, in order. The first
// directory that defines a name wins, so the workspace beats the user tier.
//
// Loading never fails: a directory that cannot be read yields no agents and a
// malformed file is skipped. The error return is part of the API so a future
// strict mode can report instead of skipping.
func Load(dirs ...string) ([]Agent, error) {
	seen := map[string]bool{}
	var out []Agent
	for _, dir := range dirs {
		d := strings.TrimSpace(dir)
		if d == "" {
			continue
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			continue // a missing or unreadable tier is simply empty
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			full := filepath.Join(d, e.Name())
			raw, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			a, err := Parse(raw)
			if err != nil {
				continue // a broken file is skipped, never fatal
			}
			a.Path = full
			if strings.TrimSpace(a.Name) == "" {
				a.Name = strings.TrimSuffix(e.Name(), ".md")
			}
			a.Name = strings.TrimSpace(a.Name)
			if a.Name == "" || seen[a.Name] {
				continue
			}
			seen[a.Name] = true
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Select finds an agent by its exact name.
func Select(list []Agent, name string) (Agent, bool) {
	want := strings.TrimSpace(name)
	if want == "" {
		return Agent{}, false
	}
	for _, a := range list {
		if a.Name == want {
			return a, true
		}
	}
	return Agent{}, false
}

// Parse splits one agent file into its frontmatter and its prompt body. A
// file with no frontmatter is not broken: the name falls back to the file
// name and the whole file is the prompt.
func Parse(raw []byte) (Agent, error) {
	text := string(raw)
	var fm frontmatter
	body := text

	if strings.HasPrefix(text, "---") {
		rest := strings.TrimPrefix(text, "---")
		rest = strings.TrimPrefix(rest, "\r")
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return Agent{}, fmt.Errorf("unterminated frontmatter")
		}
		rest = rest[nl+1:]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return Agent{}, fmt.Errorf("missing closing frontmatter delimiter")
		}
		if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
			return Agent{}, fmt.Errorf("frontmatter: %w", err)
		}
		body = rest[end+len("\n---"):]
		body = strings.TrimPrefix(body, "\r")
		body = strings.TrimPrefix(body, "\n")
	}

	a := Agent{
		Name:           strings.TrimSpace(fm.Name),
		Description:    strings.TrimSpace(fm.Description),
		Model:          strings.TrimSpace(fm.Model),
		PermissionMode: strings.TrimSpace(fm.PermissionMode),
		Tools:          fm.Tools.list(),
		MaxTurns:       fm.MaxTurns,
		Background:     fm.Background,
		Body:           body,
	}
	// The camelCase spellings are accepted as aliases: an operator writing
	// either style gets the same agent.
	if a.PermissionMode == "" {
		a.PermissionMode = strings.TrimSpace(fm.PermissionModeAlt)
	}
	if a.MaxTurns == 0 {
		a.MaxTurns = fm.MaxTurnsAlt
	}
	return a, nil
}

// ToolList is the agent's effective allowlist: what it declared, or the
// read-only default when it declared nothing.
func (a Agent) ToolList() []string {
	if len(a.Tools) == 0 {
		return append([]string(nil), DefaultTools...)
	}
	return append([]string(nil), a.Tools...)
}

// Allows reports whether the agent may use the named tool.
func (a Agent) Allows(tool string) bool {
	for _, t := range a.ToolList() {
		if t == tool {
			return true
		}
	}
	return false
}

// ReadOnly reports whether the agent is held to the read-only floor, whatever
// its allowlist says.
func (a Agent) ReadOnly() bool {
	switch strings.ToLower(strings.TrimSpace(a.PermissionMode)) {
	case "plan", "readonly", "read-only":
		return true
	}
	return false
}

// frontmatter is the YAML header of an agent file.
type frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Model       string   `yaml:"model"`
	Tools       toolList `yaml:"tools"`

	PermissionMode    string `yaml:"permission_mode"`
	PermissionModeAlt string `yaml:"permissionMode"`
	MaxTurns          int    `yaml:"max_turns"`
	MaxTurnsAlt       int    `yaml:"maxTurns"`
	Background        bool   `yaml:"background"`
}

// toolList accepts both spellings of a tool list: a comma-separated string
// and a YAML sequence.
type toolList []string

func (t *toolList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		*t = splitTools(s)
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*t = splitTools(strings.Join(list, ","))
		return nil
	}
	return nil
}

func (t toolList) list() []string {
	if len(t) == 0 {
		return nil
	}
	return append([]string(nil), t...)
}

// splitTools splits a comma-separated tool list, dropping blanks and
// duplicates while keeping the order.
func splitTools(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
