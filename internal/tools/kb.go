// The kb_search tool: on-demand retrieval over the workspace's knowledge
// bases (DESIGN §3.1 layer 3).
//
// The KB layer owns the format, the derived BM25 index and the license
// policy; this file is the tool face of it. The tool is read-only: it
// searches the bases the executor was configured with and writes nothing
// except the derived index, which is disposable by doctrine.
//
// # How the executor learns which bases exist
//
// The tools package never guesses a layout. The executor is handed a list of
// KBSource values — the base id the model sees and the directory that holds
// it — and the tool searches exactly those. The "kb" argument selects among
// the configured bases; it can never name a path, so a traversal attempt
// ("kb": "../../etc") matches nothing and returns an empty result.
//
// # Tolerance
//
// A base that cannot be loaded, indexed or searched is a warning, not a
// failure: the tool still answers with whatever the other bases found. A
// workspace with no knowledge base at all gets an explanation, not an error —
// the turn must not fail over content.

package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/kb"
	"kintsugi/internal/provider"
)

// KBSource is one knowledge base the workspace has open: the base id the
// model sees and the directory that holds it. The executor is handed these
// instead of guessing a workspace layout, so this package stays free of any
// assumption about where bases live.
type KBSource struct {
	// ID is the base id (the "id" in kb.yaml). Empty means "use the id the
	// base declares for itself".
	ID string
	// Dir is the directory holding the base: kb.yaml plus chunks/. Absolute
	// paths are used as-is; a relative path is resolved against the
	// workspace.
	Dir string
}

const (
	// kbDefaultTopK is the number of hits kb_search returns when neither the
	// call nor the executor asks for a different number.
	kbDefaultTopK = 8
	// kbMaxTopK caps the hits a single call may return.
	kbMaxTopK = 25
	// kbExcerptMax is the length (in runes) of the excerpt carried per hit.
	kbExcerptMax = 240
)

// kbSearchTool is the schema for the on-demand knowledge-base tool.
func kbSearchTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "kb_search",
			Description: "Search the workspace's knowledge bases (for example the MITRE ATT&CK subset) and return ranked chunks with their source and license. Use it when a knowledge base was announced in the prompt but its content was not injected.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"query"},
				Properties: map[string]provider.ToolProp{
					"query": {Type: "string", Description: "Keywords describing what to look up, e.g. \"powershell encoded command\"."},
					"kb":    {Type: "string", Description: "Optional knowledge base id to restrict the search to (default: every configured base)."},
					"top_k": {Type: "integer", Description: "Maximum hits to return (default 8, max 25)."},
				},
			},
		},
	}
}

// kbHit is one ranked chunk in a kb_search result. Provenance is not
// optional: source and license travel with every hit.
type kbHit struct {
	KB      string  `json:"kb"`
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Excerpt string  `json:"excerpt"`
	Source  string  `json:"source"`
	License string  `json:"license"`
}

// kbSearchOutput is the JSON body a kb_search call returns.
type kbSearchOutput struct {
	Query string  `json:"query"`
	Hits  []kbHit `json:"hits"`
	// Bases lists the bases that were actually searched.
	Bases []string `json:"bases,omitempty"`
	// Available lists the configured base ids, used when the model asks for
	// a base that does not exist.
	Available []string `json:"available,omitempty"`
	// Note explains an empty result to the model in words.
	Note string `json:"note,omitempty"`
	// Warnings carries per-base problems (a base that could not be loaded,
	// indexed or searched).
	Warnings []string `json:"warnings,omitempty"`
}

// kbSearch runs one search over the configured knowledge bases.
func (e *Executor) kbSearch(args string) Result {
	var a struct {
		Query string `json:"query"`
		KB    string `json:"kb"`
		TopK  int    `json:"top_k"`
	}
	if err := decode(args, &a); err != nil {
		return Result{Content: `{"error":"kb_search: arguments must be a JSON object"}`, Failed: true}
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return Result{Content: `{"error":"kb_search: missing required property 'query'"}`, Failed: true}
	}

	out := kbSearchOutput{Query: query, Hits: []kbHit{}}

	sources := e.kbConfigured()
	if len(sources) == 0 {
		out.Note = "no knowledge base is configured for this workspace; there is nothing to search"
		return kbSearchResult(out)
	}

	// The "kb" argument selects among the configured bases. It never names a
	// path, so a traversal attempt simply matches nothing.
	want := strings.TrimSpace(a.KB)
	selected := sources
	if want != "" {
		selected = nil
		for _, s := range sources {
			if strings.EqualFold(strings.TrimSpace(s.ID), want) {
				selected = append(selected, s)
			}
		}
		if len(selected) == 0 {
			// A source may leave the id blank and let the base declare it;
			// resolve those before giving up.
			for _, s := range sources {
				if strings.TrimSpace(s.ID) != "" {
					continue
				}
				k, err := kb.Load(e.kbDir(s.Dir))
				if err != nil {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(k.ID), want) {
					selected = append(selected, s)
					break
				}
			}
		}
		if len(selected) == 0 {
			out.Note = fmt.Sprintf("no knowledge base named %q is configured for this workspace", want)
			out.Available = kbAvailableIDs(sources)
			return kbSearchResult(out)
		}
	}

	topK := a.TopK
	if topK <= 0 {
		topK = e.KBTopK
	}
	if topK <= 0 {
		topK = kbDefaultTopK
	}
	if topK > kbMaxTopK {
		topK = kbMaxTopK
	}

	for _, s := range selected {
		k, err := kb.Load(e.kbDir(s.Dir))
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("kb %q: %v", s.label(), err))
			continue
		}
		ix, err := kb.OpenIndex(k)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("kb %q: %v", s.label(), err))
			continue
		}
		// A base that was never indexed is still searchable: the index is
		// derived and disposable, so it is built on first use.
		if err := kb.EnsureIndex(ix); err != nil {
			ix.Close()
			out.Warnings = append(out.Warnings, fmt.Sprintf("kb %q: %v", s.label(), err))
			continue
		}
		results, err := ix.Search(query, topK, nil)
		ix.Close()
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("kb %q: %v", s.label(), err))
			continue
		}

		baseID := strings.TrimSpace(k.ID)
		if baseID == "" {
			baseID = s.label()
		}
		out.Bases = append(out.Bases, baseID)
		for _, r := range results {
			out.Hits = append(out.Hits, kbHit{
				KB:      baseID,
				ID:      r.Chunk.ID,
				Title:   r.Chunk.Title,
				Score:   roundScore(r.Score),
				Excerpt: kbExcerpt(r.Chunk),
				Source:  kbProvenance(r.Chunk.Source, k.Source),
				License: kbProvenance(r.Chunk.License, k.License),
			})
		}
	}

	// Best match first across bases: bm25 is ascending (more negative is a
	// better match). The tie-breakers keep the order stable.
	sort.SliceStable(out.Hits, func(i, j int) bool {
		if out.Hits[i].Score != out.Hits[j].Score {
			return out.Hits[i].Score < out.Hits[j].Score
		}
		if out.Hits[i].KB != out.Hits[j].KB {
			return out.Hits[i].KB < out.Hits[j].KB
		}
		return out.Hits[i].ID < out.Hits[j].ID
	})
	if len(out.Hits) > topK {
		out.Hits = out.Hits[:topK]
	}
	if len(out.Hits) == 0 && len(out.Warnings) == 0 {
		out.Note = "no chunk matched the query"
	}

	return kbSearchResult(out)
}

// kbSearchResult renders the result body. It never fails: a base that cannot
// be searched is reported as a warning, not as a failed turn.
func kbSearchResult(out kbSearchOutput) Result {
	b, err := json.Marshal(out)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: string(b)}
}

// kbConfigured returns the configured bases that name a directory, in the
// order they were configured.
func (e *Executor) kbConfigured() []KBSource {
	out := make([]KBSource, 0, len(e.KBs))
	for _, s := range e.KBs {
		if strings.TrimSpace(s.Dir) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// kbDir resolves a configured base directory: absolute paths are used as-is,
// relative paths resolve against the workspace.
func (e *Executor) kbDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if filepath.IsAbs(dir) || e.Workspace == "" {
		return dir
	}
	return filepath.Join(e.Workspace, dir)
}

// kbAvailableIDs lists the ids of the configured bases, used when the model
// asks for a base that does not exist.
func kbAvailableIDs(sources []KBSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			id = s.label()
		}
		out = append(out, id)
	}
	return out
}

// label is the human-readable name of a source: its id, or the directory name
// when no id was configured.
func (s KBSource) label() string {
	if id := strings.TrimSpace(s.ID); id != "" {
		return id
	}
	return filepath.Base(strings.TrimSpace(s.Dir))
}

// kbExcerpt is the short body preview carried by a hit. The body is the
// chunk text; the adversary prompt is the fallback for a chunk with no body.
func kbExcerpt(c kb.Chunk) string {
	body := strings.TrimSpace(c.Body)
	if body == "" {
		body = strings.TrimSpace(c.AdversaryPrompt)
	}
	return clip(body, kbExcerptMax)
}

// kbProvenance prefers the chunk's own value and falls back to the base's.
func kbProvenance(chunkValue, baseValue string) string {
	if v := strings.TrimSpace(chunkValue); v != "" {
		return v
	}
	return strings.TrimSpace(baseValue)
}

// roundScore keeps the bm25 value short enough to read while preserving the
// ordering between hits.
func roundScore(score float64) float64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return math.Round(score*10000) / 10000
}
