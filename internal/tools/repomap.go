package tools

import (
	"path/filepath"
	"strings"

	"kintsugi/internal/provider"
	"kintsugi/internal/repomap"
)

// repoMapTool exposes the repository map (M10.1): files and key symbols,
// ranked by how often they are referenced and capped to a token budget. For a
// weak open-weights model this is the cheapest way to learn a codebase's shape
// before editing it.
func repoMapTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "repo_map",
			Description: "Map this repository: files and their key symbols (functions, types, methods), " +
				"ranked by how often they are referenced and capped to a token budget. " +
				"Read it before editing so you use the existing abstractions instead of inventing new ones.",
			Parameters: provider.ToolParameters{
				Type: "object",
				Properties: map[string]provider.ToolProp{
					"max_tokens": {
						Type:        "integer",
						Description: "Token budget for the map (default 1000, max 4000).",
					},
					"path": {
						Type:        "string",
						Description: "Optional file or directory to prioritize in the ranking.",
					},
				},
			},
		},
	}
}

// repoMap builds and renders the repository map. It is read-only: it walks
// the tree and parses sources, and never writes anything.
func (e *Executor) repoMap(args string) Result {
	var a struct {
		MaxTokens int    `json:"max_tokens"`
		Path      string `json:"path"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	budget := a.MaxTokens
	if budget <= 0 {
		budget = repomap.DefaultMaxTokens
	}
	if budget > 4000 {
		budget = 4000
	}
	opts := repomap.Options{Root: e.Workspace, MaxTokens: budget}
	if p := strings.TrimSpace(a.Path); p != "" {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(e.Workspace, p)
		}
		opts.Include = []string{abs}
	}
	m, err := repomap.Build(opts)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: m.Render()}
}
