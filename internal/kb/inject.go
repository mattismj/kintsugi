// Prompt rendering for the knowledge-base layer (WP-1.3, DESIGN §3.1 layer 3).
//
// kb.go materializes the on-disk format and index.go ranks chunks over it; this
// file is the last step: turning selected chunks into the bytes that actually
// land in the prompt. It is deliberately boring. The tier-3 injection format is
// fixed — a `## Knowledge:` header carrying the base title and version, an HTML
// comment carrying provenance (base id + chunk id) so a later pass can find and
// trim the block, the chunk title as a sub-heading, the body verbatim, and a
// `Source:` line with the license. Provenance comments double as block
// boundaries: the prompt budget drops whole chunk blocks from the tail and
// records which chunk ids went away.
//
// Nothing here decides *what* to inject. The app picks a strategy (always,
// retrieval, on-demand, summary) and the chunks; this package only renders.

package kb

import (
	"fmt"
	"strings"
)

// Injection strategies (config `[kb.<name>] strategy`). Retrieval is the
// default: rank chunks against the task, inject the best of them.
const (
	StrategyRetrieval = "retrieval"
	StrategyAlways    = "always"
	StrategyOnDemand  = "on-demand"
	StrategySummary   = "summary"
)

// Strategy normalizes a configured strategy name. The second result is false
// for an unknown name, so a typo can be reported instead of silently changing
// behavior. An empty name is retrieval.
func Strategy(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", StrategyRetrieval:
		return StrategyRetrieval, true
	case StrategyAlways:
		return StrategyAlways, true
	case StrategyOnDemand, "on_demand", "ondemand", "on demand":
		return StrategyOnDemand, true
	case StrategySummary:
		return StrategySummary, true
	default:
		return StrategyRetrieval, false
	}
}

// KnowledgeHeader opens every injected chunk block and every index-only
// section. It is also the first thing the budget code looks for when it has to
// describe what it trimmed.
const KnowledgeHeader = "## Knowledge: "

// SelectCandidates returns the injectable chunks of k in load order, capped at
// topK (topK <= 0 means 8). It is the "always" strategy: the operator decided
// this whole base belongs in the prompt, so no ranking happens.
func SelectCandidates(k *KB, topK int) []Chunk {
	if k == nil {
		return nil
	}
	if topK <= 0 {
		topK = defaultTopK
	}
	all := k.InjectionCandidates()
	if len(all) > topK {
		all = all[:topK]
	}
	return all
}

// RenderBlock renders one chunk in the fixed tier-3 format:
//
//	## Knowledge: <title> v<version>
//	<!-- kb:<kbID> chunk:<chunkID> -->
//	### <chunk title>
//	<chunk body>
//
//	Source: <source> (<license>)
//
// The chunk's own source/license win; the base's are the fallback for a chunk
// that did not carry them (the loader requires both, so this only matters for
// hand-built chunks in tests).
func RenderBlock(kbID, kbTitle, kbVersion string, c Chunk) string {
	if strings.TrimSpace(kbTitle) == "" {
		kbTitle = kbID
	}
	var b strings.Builder
	b.WriteString(KnowledgeHeader)
	b.WriteString(kbTitle)
	if v := strings.TrimSpace(kbVersion); v != "" {
		b.WriteString(" v")
		b.WriteString(v)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "<!-- kb:%s chunk:%s -->\n", kbID, c.ID)
	b.WriteString("### ")
	b.WriteString(c.Title)
	b.WriteString("\n")
	if body := strings.TrimSpace(c.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	src := strings.TrimSpace(c.Source)
	if src == "" {
		src = "(unknown source)"
	}
	fmt.Fprintf(&b, "\nSource: %s (%s)\n", src, strings.TrimSpace(c.License))
	return b.String()
}

// Render renders the full-body injection section for one knowledge base.
// Chunks are rendered in the order given (for retrieval that is best-first,
// with must-include chunks already at the front). A nil or empty chunk list
// renders "" so callers can skip the base silently.
func Render(k *KB, chunks []Chunk) string {
	if k == nil || len(chunks) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if !c.Injectable {
			continue
		}
		blocks = append(blocks, strings.TrimRight(RenderBlock(k.ID, k.Title, k.Version, c), "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

// RenderOnDemand renders the index-only section for the "on-demand" strategy:
// one header line, a provenance comment, and a pointer at the search tool. No
// chunk bodies — the agent pays for the base only when it searches it.
func RenderOnDemand(k *KB, retrievable int) string {
	if k == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(KnowledgeHeader)
	b.WriteString(k.Title)
	if v := strings.TrimSpace(k.Version); v != "" {
		b.WriteString(" v")
		b.WriteString(v)
	}
	b.WriteString(" (on demand)\n")
	fmt.Fprintf(&b, "<!-- kb:%s -->\n", k.ID)
	fmt.Fprintf(&b, "%d chunks; retrieval via the kb_search tool\n", retrievable)
	return b.String()
}

// RenderSummary renders the titles-only section for the "summary" strategy:
// what the base covers, with no bodies.
func RenderSummary(k *KB, chunks []Chunk) string {
	if k == nil || len(chunks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(KnowledgeHeader)
	b.WriteString(k.Title)
	if v := strings.TrimSpace(k.Version); v != "" {
		b.WriteString(" v")
		b.WriteString(v)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "<!-- kb:%s -->\n", k.ID)
	for _, c := range chunks {
		if !c.Injectable {
			continue
		}
		fmt.Fprintf(&b, "- %s · %s\n", c.ID, c.Title)
	}
	return b.String()
}

// Join joins rendered sections with a blank line, dropping empty ones. This is
// how multiple configured bases become one layer-3 block.
func Join(sections ...string) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if t := strings.TrimRight(strings.TrimSpace(s), "\n"); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// EnsureIndex builds the derived index when it holds no rows yet, so a base
// that has never been indexed is still retrievable on first use. An index that
// already has rows is left untouched (the file is the cache; Reindex is the
// explicit rebuild). A nil index is a no-op.
func EnsureIndex(ix *Index) error {
	if ix == nil || ix.DB == nil {
		return nil
	}
	var n int
	if err := ix.DB.QueryRow(`SELECT count(*) FROM kb_chunks`).Scan(&n); err != nil {
		return fmt.Errorf("kb: count index rows: %w", err)
	}
	if n > 0 {
		return nil
	}
	_, err := ix.Reindex()
	return err
}
