package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kintsugi/internal/adapters"
	"kintsugi/internal/persona"
)

// Layer is one materialized prompt-stack layer (DESIGN §3.1).
type Layer struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"` // builtin | file | context | user
	Path            string   `json:"path,omitempty"`
	Text            string   `json:"-"`
	Hash            string   `json:"hash"`
	Tokens          int      `json:"tokens"`
	Dropped         bool     `json:"dropped,omitempty"`
	DropEligible    []string `json:"-"`
	DroppedSections []string `json:"dropped_sections,omitempty"`
}

// Input is everything compile needs. Pure data: no I/O happens here.
type Input struct {
	Provider     string
	ModelID      string
	Family       string
	Reasoning    bool
	Capabilities []string
	WindowTokens int

	Adapter *adapters.Adapter
	Persona *persona.Selection

	// KBText is the rendered layer-3 knowledge section (KB chunks, or an
	// index-only view under the on-demand/summary strategies). KBMeta describes
	// which base contributed which chunk ids, for the manifest.
	KBText string
	KBMeta []KBLayer

	BehaviorText string
	BehaviorPath string

	SkillsText string // compact index only; bodies load through the skill tool
	SkillsPath string

	WorkspaceDir string
	ContextPath  string // absolute or relative to WorkspaceDir
	ContextText  string // pre-read by the caller (keeps compile pure)

	TasteText string // operator taste block (M10-B), injected after context

	// ScopeText is the rendered declared-scope block (scope.Rules.PromptBlock):
	// the boundary the tool layer enforces, stated for the model. ScopePath
	// names the sources it was rendered from (config [scope], CONTEXT.md
	// "Scope") so the manifest says where the rule set came from. Both come
	// from scope.FromWorkspace, the same call the tool layer enforces, so the
	// prompt cannot describe a boundary the executor does not apply.
	ScopeText string
	ScopePath string

	Task         string
	BudgetTokens int
}

// KBLayer records what one configured knowledge base contributed to layer 3.
// Chunks holds the chunk ids that were actually materialized into the prompt
// (empty for index-only strategies, which inject no bodies).
type KBLayer struct {
	ID     string
	Path   string
	Chunks []string
}

// Compiled is the result of a compilation.
type Compiled struct {
	Layers   []Layer
	Text     string
	Manifest Manifest
	Persona  *persona.Selection // retained for stream-reminder composition
}

type Manifest struct {
	Model struct {
		ID       string `json:"id"`
		Family   string `json:"family"`
		Provider string `json:"provider"`
	} `json:"model"`
	Persona struct {
		Name string `json:"name"`
		Rule string `json:"rule"`
		Path string `json:"path"`
	} `json:"persona"`
	Adapter   string          `json:"adapter"`
	Layers    []ManifestLayer `json:"layers"`
	Totals    Totals          `json:"totals"`
	Warnings  []string        `json:"warnings,omitempty"`
	DiffFrom  string          `json:"diff_from,omitempty"`
	StackHash string          `json:"stack_hash"`
}

type ManifestLayer struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Path            string   `json:"path,omitempty"`
	Hash            string   `json:"hash"`
	Tokens          int      `json:"tokens"`
	Dropped         bool     `json:"dropped,omitempty"`
	DroppedSections []string `json:"dropped_sections,omitempty"`
	// Chunks lists the knowledge-base chunk ids materialized into this layer
	// (layer 3 only), in injection order.
	Chunks []string `json:"chunks,omitempty"`
}

type Totals struct {
	Tokens        int     `json:"tokens"`
	Budget        int     `json:"budget"`
	WindowUsedPct float64 `json:"window_used_pct"`
}

// tokenEstimate is intentionally conservative: ~4 bytes per token. It is
// stable across runs (the property cache-stability tests rely on), and later
// swaps to a per-family tokenizer without changing layer bytes.
func tokenEstimate(s string) int {
	return (len(s) + 3) / 4
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// Compile builds the prompt stack deterministically: same Input → same bytes,
// same hashes, same manifest. This is the property the M1 snapshot test pins.
func Compile(in Input) *Compiled {
	var layers []Layer

	layers = append(layers, Layer{ID: KernelLayerID, Kind: "builtin", Text: Kernel})

	if in.Adapter != nil {
		layers = append(layers, Layer{
			ID:   "adapter",
			Kind: "builtin",
			Path: "adapters/" + in.Adapter.ID + ".yaml",
			Text: in.Adapter.Render(),
		})
	}

	if in.Persona != nil {
		var sb strings.Builder
		var drop []string
		for _, sec := range in.Persona.Sections() {
			sb.WriteString(sec.Text)
			sb.WriteString("\n\n")
			if sec.Kind != "core" {
				drop = append(drop, sec.Kind)
			}
		}
		layers = append(layers, Layer{
			ID:           "persona",
			Kind:         "file",
			Path:         in.Persona.Persona.Path,
			Text:         strings.TrimRight(sb.String(), "\n"),
			DropEligible: drop, // section kinds that MAY be dropped by budget
		})
	}

	// Layer 3 (knowledge) sits between persona (2) and behavior (4): retrieved
	// domain knowledge is model-keyed doctrine's junior and role policy's senior.
	if kbText := strings.TrimSpace(in.KBText); kbText != "" {
		layers = append(layers, Layer{
			ID:   "kb",
			Kind: "kb",
			Path: kbPath(in.KBMeta),
			Text: kbText,
		})
	}

	if in.BehaviorText != "" {
		layers = append(layers, Layer{
			ID:   "behavior",
			Kind: "file",
			Path: in.BehaviorPath,
			Text: in.BehaviorText,
		})
	}

	if strings.TrimSpace(in.SkillsText) != "" {
		layers = append(layers, Layer{
			ID:   "skills",
			Kind: "file",
			Path: in.SkillsPath,
			Text: strings.TrimSpace(in.SkillsText),
		})
	}

	if strings.TrimSpace(in.ContextText) != "" {
		layers = append(layers, Layer{
			ID:   "workspace",
			Kind: "context",
			Path: in.ContextPath,
			Text: strings.TrimSpace(in.ContextText),
		})
	}

	if strings.TrimSpace(in.TasteText) != "" {
		layers = append(layers, Layer{
			ID:   "taste",
			Kind: "context",
			Path: "operator taste (local)",
			Text: strings.TrimSpace(in.TasteText),
		})
	}

	// Declared scope (M11.2): the boundary the tool layer enforces, stated for
	// the model. It sits directly under the workspace context it is partly
	// derived from (the CONTEXT.md "Scope" section is merged with [scope] by
	// scope.FromWorkspace, the same call the executor makes), so the model reads
	// the project's memory and then the machine-checked rule set extracted from
	// it. It is session-scoped, not per-turn: like every layer above the task it
	// is byte-stable turn to turn (DESIGN §3.6), so it never invalidates the
	// provider's cache prefix on its own. It is deliberately absent from the
	// budget drop order: this block is a boundary statement, not content, and
	// dropping it is the very "enforced but invisible" gap it exists to close —
	// under a tight budget the model would be held to rules it cannot see.
	if scopeText := strings.TrimSpace(in.ScopeText); scopeText != "" {
		layers = append(layers, Layer{
			ID:   "scope",
			Kind: "context",
			Path: in.ScopePath,
			Text: scopeText,
		})
	}

	if strings.TrimSpace(in.Task) != "" {
		layers = append(layers, Layer{
			ID:   "task",
			Kind: "user",
			Text: "## Task\n\n" + strings.TrimSpace(in.Task),
		})
	}

	for i := range layers {
		layers[i].Hash = hashText(layers[i].Text)
		layers[i].Tokens = tokenEstimate(layers[i].Text)
	}

	warnings := budget(layers, in.BudgetTokens)

	// Recompute hashes/tokens after drops so the manifest reflects what the
	// model actually sees.
	for i := range layers {
		layers[i].Hash = hashText(layers[i].Text)
		layers[i].Tokens = tokenEstimate(layers[i].Text)
	}

	text := assemble(layers)

	m := Manifest{}
	m.Model.ID = in.ModelID
	m.Model.Family = in.Family
	m.Model.Provider = in.Provider
	if in.Persona != nil {
		m.Persona.Name = in.Persona.Persona.Name
		m.Persona.Rule = in.Persona.Rule
		m.Persona.Path = in.Persona.Persona.Path
	}
	if in.Adapter != nil {
		m.Adapter = in.Adapter.ID
	}
	for _, l := range layers {
		ml := ManifestLayer{
			ID: l.ID, Kind: l.Kind, Path: relativePath(in.WorkspaceDir, l.Path),
			Hash: l.Hash, Tokens: l.Tokens, Dropped: l.Dropped, DroppedSections: l.DroppedSections,
		}
		if l.ID == "kb" {
			ml.Chunks = kbChunkIDs(in.KBMeta)
		}
		m.Layers = append(m.Layers, ml)
	}
	total := 0
	for _, l := range layers {
		total += l.Tokens
	}
	m.Totals.Tokens = total
	m.Totals.Budget = in.BudgetTokens
	if in.WindowTokens > 0 {
		m.Totals.WindowUsedPct = float64(total) / float64(in.WindowTokens)
	}
	m.Warnings = warnings
	m.StackHash = stackHash(layers)

	return &Compiled{Layers: layers, Text: text, Manifest: m, Persona: in.Persona}
}

// budget applies the DESIGN §3.6 drop order. Kernel and persona core never
// drop; optional layers/sections drop whole, never mid-sentence. Layer 3 (kb)
// is special: it drops chunk blocks from the tail, because a knowledge section
// is a list of independent blocks and the front of it ranks best.
//
// The declared scope layer also never drops, for the same reason the kernel
// does not: it is a boundary the tool layer enforces unconditionally. Dropping
// it would leave the model held to rules it cannot see, which is the exact gap
// that layer closes — so a budget too tight to hold it is reported as a warning
// rather than resolved by hiding the boundary.
func budget(layers []Layer, limit int) []string {
	if limit <= 0 {
		return nil
	}
	total := 0
	for _, l := range layers {
		total += l.Tokens
	}
	if total <= limit {
		return nil
	}

	dropOrder := []string{"workspace", "kb", "skills", "behavior", "persona:triggers", "persona:notes"}
	for _, id := range dropOrder {
		if total <= limit {
			break
		}
		if strings.HasPrefix(id, "persona:") {
			kind := strings.TrimPrefix(id, "persona:")
			for i := range layers {
				if layers[i].ID != "persona" || layers[i].Dropped {
					continue
				}
				before := tokenEstimate(layers[i].Text)
				trimmed, removed := dropPersonaSection(&layers[i], kind)
				if removed {
					layers[i].Text = trimmed
					total -= before - tokenEstimate(trimmed)
				}
			}
			continue
		}
		if id == "kb" {
			for i := range layers {
				if layers[i].ID != "kb" || layers[i].Dropped {
					continue
				}
				for total > limit {
					before := tokenEstimate(layers[i].Text)
					trimmed, dropped, ok := dropLastKBBlock(&layers[i])
					if !ok {
						break
					}
					layers[i].Text = trimmed
					layers[i].DroppedSections = append(layers[i].DroppedSections, dropped)
					total -= before - tokenEstimate(trimmed)
				}
			}
			continue
		}
		for i := range layers {
			if layers[i].ID == id && !layers[i].Dropped {
				total -= layers[i].Tokens
				layers[i].Text = ""
				layers[i].Dropped = true
			}
		}
	}

	if total > limit {
		return []string{fmt.Sprintf(
			"prompt is over budget after all optional content was dropped (%d > %d tokens): kernel/adapter/persona core and the declared scope block exceed the configured budget",
			total, limit)}
	}
	return nil
}

func dropPersonaSection(l *Layer, kind string) (string, bool) {
	// Persona attachments are appended after the bodies with a known header at
	// the start of a line.
	var marker string
	switch kind {
	case "notes":
		marker = "— model notes"
	case "triggers":
		marker = "## Trigger map for "
	default:
		return l.Text, false
	}

	idx := strings.Index(l.Text, marker)
	if idx < 0 {
		return l.Text, false
	}
	start := idx
	for start > 0 && l.Text[start-1] != '\n' {
		start--
	}
	l.DroppedSections = append(l.DroppedSections, kind)
	return strings.TrimRight(l.Text[:start], "\n"), true
}

// kbBlockMarker prefixes the provenance comment that opens every injected chunk
// block (`<!-- kb:<kbID> chunk:<chunkID> -->`). It is the block boundary the
// budget trims at.
const kbBlockMarker = "<!-- kb:"

// dropLastKBBlock removes the last chunk block from a knowledge layer and
// returns the trimmed text plus the dropped chunk id. It refuses to remove the
// final block: layer 3 degrades to its best-ranked chunk, never to nothing.
// Search results are best-first and must-include chunks are already at the
// front, so trimming from the tail can never drop a must-include chunk.
func dropLastKBBlock(l *Layer) (string, string, bool) {
	prefix, blocks, ids := splitKBBlocks(l.Text)
	if len(blocks) <= 1 {
		return l.Text, "", false
	}
	kept := blocks[:len(blocks)-1]
	return strings.TrimRight(prefix+strings.Join(kept, "\n\n"), "\n"), ids[len(ids)-1], true
}

// splitKBBlocks splits a knowledge layer at its block boundaries. Each block
// starts at the `## Knowledge:` header line that precedes a `<!-- kb:` marker,
// so a block is always self-describing (title, provenance, body, source).
// Returned blocks carry no trailing newline; ids are the chunk (or base) ids in
// block order.
func splitKBBlocks(text string) (prefix string, blocks []string, ids []string) {
	var starts []int
	for off := 0; ; {
		j := strings.Index(text[off:], kbBlockMarker)
		if j < 0 {
			break
		}
		pos := off + j
		start := strings.LastIndexByte(text[:pos], '\n') + 1
		// Pull in the header line directly above the marker, if there is one.
		if start > 0 {
			prevEnd := start - 1
			prevStart := strings.LastIndexByte(text[:prevEnd], '\n') + 1
			if strings.HasPrefix(text[prevStart:prevEnd], "## Knowledge: ") {
				start = prevStart
			}
		}
		if len(starts) == 0 || start > starts[len(starts)-1] {
			starts = append(starts, start)
		}
		off = pos + len(kbBlockMarker)
	}
	if len(starts) == 0 {
		return text, nil, nil
	}

	prefix = text[:starts[0]]
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		block := strings.TrimRight(text[start:end], "\n")
		blocks = append(blocks, block)
		ids = append(ids, kbBlockID(block))
	}
	return prefix, blocks, ids
}

// kbBlockID reads the chunk id out of a block's provenance comment. An
// index-only marker (`<!-- kb:<kbID> -->`) yields the base id.
func kbBlockID(block string) string {
	i := strings.Index(block, kbBlockMarker)
	if i < 0 {
		return ""
	}
	rest := block[i+len(kbBlockMarker):]
	if j := strings.Index(rest, "-->"); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	if i := strings.Index(rest, "chunk:"); i >= 0 {
		return strings.TrimSpace(rest[i+len("chunk:"):])
	}
	return rest
}

// kbPath is the layer path shown in the manifest: the base/chunk pairs layer 3
// was built from. A base that injected no bodies contributes just its id.
func kbPath(meta []KBLayer) string {
	var out []string
	for _, k := range meta {
		id := strings.TrimSpace(k.ID)
		if len(k.Chunks) == 0 {
			if id != "" {
				out = append(out, id)
			}
			continue
		}
		for _, c := range k.Chunks {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if id == "" {
				out = append(out, c)
				continue
			}
			out = append(out, id+"/"+c)
		}
	}
	return strings.Join(out, ",")
}

// kbChunkIDs aggregates the chunk ids of every contributing base, in injection
// order, for the manifest.
func kbChunkIDs(meta []KBLayer) []string {
	var out []string
	for _, k := range meta {
		for _, c := range k.Chunks {
			if c = strings.TrimSpace(c); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

func assemble(layers []Layer) string {
	var parts []string
	for _, l := range layers {
		if strings.TrimSpace(l.Text) == "" {
			continue
		}
		parts = append(parts, strings.TrimRight(l.Text, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

func stackHash(layers []Layer) string {
	var sb strings.Builder
	for _, l := range layers {
		sb.WriteString(l.ID)
		sb.WriteString("=")
		sb.WriteString(l.Hash)
		sb.WriteString(";")
	}
	return hashText(sb.String())
}

func relativePath(ws, p string) string {
	if ws == "" || p == "" {
		return p
	}
	if rel, err := filepath.Rel(ws, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// ---------- manifest persistence + diff ----------

const manifestDirName = ".kintsugi/manifests"

func ManifestPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, manifestDirName, "last.json")
}

func SaveManifest(workspaceDir string, m Manifest) error {
	dir := filepath.Join(workspaceDir, manifestDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "last.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "last.json"))
}

func LoadManifest(workspaceDir string) (*Manifest, error) {
	b, err := os.ReadFile(ManifestPath(workspaceDir))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// PrefixChanged lists the cache-prefix layers (kernel, adapter, persona, scope)
// whose bytes changed between two compiles. A non-empty result means the
// provider's cached prefix is cold for the next request; task-layer edits alone
// leave it intact.
//
// Scope is in this list because it is a compiled layer above the task, not
// because a scope edit is routine: editing [scope] or the CONTEXT.md "Scope"
// section changes bytes the provider already cached, and saying so is the
// honest report. With no declared scope there is no scope layer at all, so a
// workspace that declares none is unaffected either way.
func PrefixChanged(old, cur Manifest) []string {
	oldL := layerMap(old)
	curL := layerMap(cur)
	var out []string
	for _, id := range []string{"kernel", "adapter", "persona", "scope"} {
		o, okO := oldL[id]
		c, okC := curL[id]
		if okO != okC || (okO && okC && (o.Hash != c.Hash || o.Dropped != c.Dropped)) {
			out = append(out, id)
		}
	}
	return out
}

// Diff returns human-readable per-layer deltas between two manifests.
func Diff(old, cur Manifest) []string {
	var out []string
	if old.Model.ID != cur.Model.ID || old.Model.Provider != cur.Model.Provider {
		out = append(out, fmt.Sprintf("model: %s/%s → %s/%s",
			old.Model.Provider, old.Model.ID, cur.Model.Provider, cur.Model.ID))
	}
	if old.Persona.Name != cur.Persona.Name {
		out = append(out, fmt.Sprintf("persona: %s → %s", old.Persona.Name, cur.Persona.Name))
	}
	if old.Adapter != cur.Adapter {
		out = append(out, fmt.Sprintf("adapter: %s → %s", old.Adapter, cur.Adapter))
	}
	oldL := layerMap(old)
	curL := layerMap(cur)
	order := []string{"kernel", "adapter", "persona", "kb", "behavior", "skills", "workspace", "scope", "task"}
	for _, id := range order {
		o, okO := oldL[id]
		c, okC := curL[id]
		switch {
		case okO && !okC:
			out = append(out, "- "+id+" (removed)")
		case !okO && okC:
			out = append(out, "+ "+id+" ("+c.Hash+", "+fmt.Sprintf("%d", c.Tokens)+" tok)")
		case okO && okC && o.Hash != c.Hash:
			out = append(out, "~ "+id+" ("+o.Hash+" → "+c.Hash+", "+fmt.Sprintf("%d", c.Tokens)+" tok)")
		}
	}
	if len(out) == 0 {
		out = append(out, "no layer changes (stack hash "+cur.StackHash+")")
	}
	return out
}

func layerMap(m Manifest) map[string]ManifestLayer {
	out := map[string]ManifestLayer{}
	for _, l := range m.Layers {
		out[l.ID] = l
	}
	return out
}
