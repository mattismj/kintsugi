// STIX 2.x importer (WP-1.5).
//
// MITRE publishes ATT&CK as a STIX 2.1 bundle (github.com/mitre/cti). This file
// turns such a bundle into the on-disk format kb.go loads: one Markdown chunk
// per technique — frontmatter carrying provenance plus the `adversary_prompt`,
// body carrying the plain-text description — and a kb.yaml base descriptor.
//
// # Content model
//
// One `attack-pattern` object becomes one chunk with chunking strategy
// "per-technique": the technique id and name from the `mitre-attack` external
// reference, the first kill-chain phase as the tactic, `x_mitre_platforms` as
// platforms, the reference URL as source, and the description as the body with
// STIX markup removed. The `adversary_prompt` is derived, not copied: it is the
// review question the agent should be asking when it reads a plan, a diff, or an
// artifact, which is what makes the chunk usable at layer 3 rather than just
// readable (DESIGN §6.3).
//
// # Tolerance doctrine
//
// The same line as kb.Load: a bundle is content, not control flow. Errors are
// reserved for the two things that make a build impossible — a bundle that is
// not readable JSON, and an out directory that cannot be written. Everything per
// object (no ATT&CK id, an id that cannot be a file name, a revoked or
// deprecated object, a duplicate id) is a warning plus a skip, because one odd
// object in a 700-technique bundle must not cost the operator the other 699.
//
// Output is deterministic: objects are read in bundle order (so "first wins" for
// duplicates is well defined) and the chunks are written in technique-id order.
package kb

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultSTIXLicense is the license assumed when BuildFromSTIX is called
	// with an empty one. ATT&CK content is published under CC BY 4.0.
	DefaultSTIXLicense = "CC BY 4.0"

	// stixDefaultSource is the fallback source URL for a technique whose
	// mitre-attack reference carries no url field.
	stixDefaultSource = "https://attack.mitre.org/"

	// Base descriptor written by BuildFromSTIX. version is "stix" because the
	// build is not a numbered ATT&CK release: the retrieved_at on every chunk
	// says when the source bundle was pulled.
	stixBaseID      = "attack"
	stixBaseVersion = "stix"
	stixBaseTitle   = "MITRE ATT&CK (Enterprise)"
	stixBaseSource  = "mitre/cti"

	// stixChunkStrategy is the chunking strategy recorded in kb.yaml.
	stixChunkStrategy = "per-technique"

	// stixRetrievedLayout is the date layout used for retrieved_at.
	stixRetrievedLayout = "2006-01-02"
)

// stixBundle is the subset of a STIX 2.x bundle the importer looks at.
type stixBundle struct {
	Type    string       `json:"type"`
	Objects []stixObject `json:"objects"`
}

// stixObject is the subset of an attack-pattern the importer looks at. Unknown
// members are dropped by encoding/json, which is exactly what we want: the
// bundle format gains fields over time and none of them are our business.
type stixObject struct {
	Type             string               `json:"type"`
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	Revoked          bool                 `json:"revoked"`
	KillChainPhases  []stixKillChainPhase `json:"kill_chain_phases"`
	ExternalRefs     []stixExternalRef    `json:"external_references"`
	XMitrePlatforms  []string             `json:"x_mitre_platforms"`
	XMitreDeprecated bool                 `json:"x_mitre_deprecated"`
}

// stixKillChainPhase is one phase entry on an attack-pattern.
type stixKillChainPhase struct {
	KillChainName string `json:"kill_chain_name"`
	PhaseName     string `json:"phase_name"`
}

// stixExternalRef is one external reference on an attack-pattern. Only the
// mitre-attack entry matters: it is where the T-number lives.
type stixExternalRef struct {
	SourceName  string `json:"source_name"`
	ExternalID  string `json:"external_id"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// stixBaseMeta is kb.yaml as written by the importer. Field order is the output
// order, so the descriptor reads like the documented format.
type stixBaseMeta struct {
	ID       string           `yaml:"id"`
	Version  string           `yaml:"version"`
	Title    string           `yaml:"title"`
	Source   string           `yaml:"source"`
	License  string           `yaml:"license"`
	Chunking stixChunkingMeta `yaml:"chunking"`
}

// stixChunkingMeta is the chunking block of kb.yaml.
type stixChunkingMeta struct {
	Strategy string   `yaml:"strategy"`
	Fields   []string `yaml:"fields,omitempty"`
}

// stixChunkFront holds one chunk's frontmatter, in documented field order.
type stixChunkFront struct {
	ID              string   `yaml:"id"`
	Title           string   `yaml:"title"`
	Tactic          string   `yaml:"tactic"`
	Platforms       []string `yaml:"platforms,omitempty"`
	Source          string   `yaml:"source"`
	License         string   `yaml:"license"`
	RetrievedAt     string   `yaml:"retrieved_at"`
	AdversaryPrompt string   `yaml:"adversary_prompt"`
}

// technique is one importer output row, before it is rendered to Markdown.
type technique struct {
	id        string
	title     string
	tactic    string
	platforms []string
	source    string
	body      string
	prompt    string
}

// stixCitationRE matches the "(Citation: ...)" markers MITRE puts inline in
// descriptions. They point at their own reference list, which we do not carry,
// so leaving them in would be noise in an injected prompt.
var stixCitationRE = regexp.MustCompile(`\s*\(\s*Citation:[^)]*\)`)

// stixCodeTagRE matches the <code> tags MITRE wraps inline code in. The inner
// text is kept — it is the part that matters ("powershell.exe -EncodedCommand").
var stixCodeTagRE = regexp.MustCompile(`(?i)</?code>`)

// stixSpaceRE collapses whitespace runs so a body is one tidy paragraph.
var stixSpaceRE = regexp.MustCompile(`\s+`)

// tacticSmallWords stay lower-case when a kill-chain phase is title-cased, so
// "command-and-control" reads as "Command and Control".
var tacticSmallWords = map[string]bool{
	"and": true, "or": true, "of": true, "the": true, "to": true,
	"in": true, "on": true, "over": true, "via": true, "with": true,
}

// sentenceAbbrevs are trailing words that do not end a sentence, so a
// description like "... used, e.g. a base64 blob." is not cut after "e.g.".
var sentenceAbbrevs = map[string]bool{
	"e.g": true, "i.e": true, "etc": true, "vs": true, "cf": true,
	"mr": true, "ms": true, "dr": true, "fig": true, "inc": true, "no": true,
}

// BuildFromSTIX reads a STIX 2.x bundle from r and materializes a knowledge base
// in outDir: <outDir>/kb.yaml plus <outDir>/chunks/<ID>.md, one file per
// attack-pattern that carries a mitre-attack external id.
//
// license is written to every chunk and to kb.yaml, and an empty one defaults
// to DefaultSTIXLicense. retrievedAt is the date stamped on every chunk; an
// empty one defaults to today (UTC) — pass it explicitly for reproducible
// builds. written is the number of chunk files created.
//
// warnings reports per-object problems that were skipped rather than fatal: an
// attack-pattern with no ATT&CK id, an id unusable as a file name, a revoked or
// deprecated object, and a duplicate id (the first occurrence wins). err is
// returned only for an unreadable reader, a bundle that is not valid JSON, or an
// out directory that cannot be created or written.
func BuildFromSTIX(r io.Reader, outDir, license, retrievedAt string) (written int, warnings []string, err error) {
	if r == nil {
		return 0, nil, fmt.Errorf("kb: build from STIX: nil reader")
	}
	if strings.TrimSpace(outDir) == "" {
		return 0, nil, fmt.Errorf("kb: build from STIX: empty output directory")
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return 0, nil, fmt.Errorf("kb: build from STIX: read bundle: %w", err)
	}
	var bundle stixBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return 0, nil, fmt.Errorf("kb: build from STIX: invalid JSON bundle: %w", err)
	}

	license = strings.TrimSpace(license)
	if license == "" {
		license = DefaultSTIXLicense
	}
	retrievedAt = strings.TrimSpace(retrievedAt)
	if retrievedAt == "" {
		retrievedAt = time.Now().UTC().Format(stixRetrievedLayout)
	}

	techniques, warnings := collectTechniques(bundle)

	if err := os.MkdirAll(filepath.Join(outDir, ChunksDir), 0o755); err != nil {
		return 0, warnings, fmt.Errorf("kb: build from STIX: %w", err)
	}

	meta := stixBaseMeta{
		ID:       stixBaseID,
		Version:  stixBaseVersion,
		Title:    stixBaseTitle,
		Source:   stixBaseSource,
		License:  license,
		Chunking: stixChunkingMeta{Strategy: stixChunkStrategy},
	}
	metaYAML, err := yaml.Marshal(meta)
	if err != nil {
		return 0, warnings, fmt.Errorf("kb: build from STIX: encode %s: %w", MetaFile, err)
	}
	if err := os.WriteFile(filepath.Join(outDir, MetaFile), metaYAML, 0o644); err != nil {
		return 0, warnings, fmt.Errorf("kb: build from STIX: %w", err)
	}

	for _, t := range techniques {
		front := stixChunkFront{
			ID:              t.id,
			Title:           t.title,
			Tactic:          t.tactic,
			Platforms:       t.platforms,
			Source:          t.source,
			License:         license,
			RetrievedAt:     retrievedAt,
			AdversaryPrompt: t.prompt,
		}
		frontYAML, err := yaml.Marshal(front)
		if err != nil {
			return written, warnings, fmt.Errorf("kb: build from STIX: encode chunk %s: %w", t.id, err)
		}
		var b strings.Builder
		b.WriteString("---\n")
		b.Write(frontYAML)
		b.WriteString("---\n\n")
		b.WriteString(t.body)
		b.WriteString("\n")

		path := filepath.Join(outDir, ChunksDir, t.id+".md")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			return written, warnings, fmt.Errorf("kb: build from STIX: %w", err)
		}
		written++
	}
	return written, warnings, nil
}

// collectTechniques walks the bundle in object order and returns the techniques
// to write, sorted by id, plus the skip warnings in encounter order.
func collectTechniques(bundle stixBundle) ([]technique, []string) {
	var warnings []string
	var out []technique
	seen := make(map[string]string, len(bundle.Objects))

	for i, obj := range bundle.Objects {
		if !strings.EqualFold(strings.TrimSpace(obj.Type), "attack-pattern") {
			// intrusion-set, malware, tool, relationship, ...: not our unit.
			continue
		}
		label := stixObjectLabel(obj, i)

		id, url, ok := stixAttackRef(obj)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: no mitre-attack external id, skipped", label))
			continue
		}
		if !stixIDIsFilename(id) {
			warnings = append(warnings, fmt.Sprintf("%s: id %q is not usable as a file name, skipped", label, id))
			continue
		}
		if obj.Revoked || obj.XMitreDeprecated {
			warnings = append(warnings, fmt.Sprintf("%s (%s): %s, skipped", label, id, stixSkipReason(obj)))
			continue
		}
		key := strings.ToLower(id)
		if prev, dup := seen[key]; dup {
			warnings = append(warnings, fmt.Sprintf("%s: duplicate technique id %q (already imported as %q), skipped", label, id, prev))
			continue
		}
		seen[key] = id

		title := strings.TrimSpace(obj.Name)
		if title == "" {
			title = id
		}
		source := strings.TrimSpace(url)
		if source == "" {
			source = stixDefaultSource
		}
		body := stixPlainText(obj.Description)
		out = append(out, technique{
			id:        id,
			title:     title,
			tactic:    stixTactic(obj),
			platforms: stixPlatforms(obj.XMitrePlatforms),
			source:    source,
			body:      body,
			prompt:    stixAdversaryPrompt(title, body),
		})
	}

	// Bundle order is input-dependent; chunk order should not be.
	sort.SliceStable(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, warnings
}

// stixAttackRef returns the ATT&CK id and url from the mitre-attack external
// reference. Other references (research papers, MSS advisories) carry no
// external_id and are ignored.
func stixAttackRef(obj stixObject) (id, url string, ok bool) {
	for _, ref := range obj.ExternalRefs {
		if !strings.EqualFold(strings.TrimSpace(ref.SourceName), "mitre-attack") {
			continue
		}
		refID := strings.TrimSpace(ref.ExternalID)
		if refID == "" {
			continue
		}
		return refID, strings.TrimSpace(ref.URL), true
	}
	return "", "", false
}

// stixTactic maps the first kill-chain phase to a title-cased tactic name. The
// mitre-attack phase wins if the object lists several chains; otherwise the
// first usable phase is used. No phases means no tactic ("").
func stixTactic(obj stixObject) string {
	for _, kc := range obj.KillChainPhases {
		if strings.EqualFold(strings.TrimSpace(kc.KillChainName), "mitre-attack") {
			if t := titleTacticPhrase(kc.PhaseName); t != "" {
				return t
			}
		}
	}
	for _, kc := range obj.KillChainPhases {
		if t := titleTacticPhrase(kc.PhaseName); t != "" {
			return t
		}
	}
	return ""
}

// titleTacticPhrase turns "command-and-control" into "Command and Control".
func titleTacticPhrase(phase string) string {
	phase = strings.ToLower(strings.TrimSpace(phase))
	if phase == "" {
		return ""
	}
	parts := strings.FieldsFunc(phase, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == ' '
	})
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i > 0 && tacticSmallWords[p] {
			out = append(out, p)
			continue
		}
		out = append(out, upperFirst(p))
	}
	return strings.Join(out, " ")
}

// upperFirst upper-cases the first rune of s.
func upperFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}

// stixPlatforms copies x_mitre_platforms, trimming blanks and duplicates while
// keeping MITRE's order (deterministic, and it groups related platforms).
func stixPlatforms(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stixPlainText renders a STIX description as plain text: citation markers and
// <code> tags gone, whitespace collapsed to single spaces.
func stixPlainText(desc string) string {
	s := stixCitationRE.ReplaceAllString(desc, " ")
	s = stixCodeTagRE.ReplaceAllString(s, "")
	s = stixSpaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// stixAdversaryPrompt builds the review question a chunk is injected for:
// "does this exhibit <name>?" plus the description's first sentence as the
// concrete shape to look for.
func stixAdversaryPrompt(name, body string) string {
	prompt := "When reviewing a plan, diff, or artifact, ask: does this exhibit " + strings.TrimSpace(name) + "?"
	if s := firstSentence(body); s != "" {
		prompt += " " + s
	}
	return prompt
}

// firstSentence returns text up to and including the first sentence-ending
// period, or the whole text when there is none. Common abbreviations do not end
// a sentence.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] != '.' {
			continue
		}
		if i+1 < len(s) && s[i+1] != ' ' {
			continue // "T1059.001", "cmd.exe", a decimal: not a sentence end
		}
		if sentenceAbbrevs[lastWord(s[:i])] {
			continue
		}
		return strings.TrimSpace(s[:i+1])
	}
	return s
}

// lastWord returns the lower-cased word immediately before position i.
func lastWord(s string) string {
	s = strings.TrimRight(s, " ")
	start := strings.LastIndexAny(s, " \t\n(\"'")
	if start < 0 {
		return strings.ToLower(s)
	}
	return strings.ToLower(s[start+1:])
}

// stixIDIsFilename reports whether id can name a chunk file. ATT&CK ids are
// "T1059" or "T1059.001"; anything with a separator or a traversal would escape
// chunks/.
func stixIDIsFilename(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return !strings.ContainsAny(id, `/\`)
}

// stixSkipReason describes why an object was skipped.
func stixSkipReason(obj stixObject) string {
	switch {
	case obj.Revoked && obj.XMitreDeprecated:
		return "revoked and deprecated"
	case obj.Revoked:
		return "revoked"
	default:
		return "deprecated"
	}
}

// stixObjectLabel names an object in a warning: its technique name when it has
// one, else its STIX id, else its position in the bundle.
func stixObjectLabel(obj stixObject, index int) string {
	if name := strings.TrimSpace(obj.Name); name != "" {
		return fmt.Sprintf("attack-pattern %q", name)
	}
	if id := strings.TrimSpace(obj.ID); id != "" {
		return "attack-pattern " + id
	}
	return fmt.Sprintf("attack-pattern #%d", index)
}
