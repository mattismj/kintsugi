package session

// Per-model reliability: what the session telemetry says about how each
// model actually behaves on this operator's work. Per MODEL, not per
// provider — refusal behaviour and code quality are properties of the model,
// and a provider hosts many models.
//
// Everything here is counters and class labels: no phrase, no transcript. The
// refusal text lives only in the session store and the operator's exports.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ModelReliability is one (model, persona) row aggregated across sessions.
type ModelReliability struct {
	Model   string `json:"model"`
	Persona string `json:"persona"`
	// Turns is the operator turn count (user events): the denominator.
	Turns int `json:"turns"`
	// Refusals is the interception count (all classes).
	Refusals int `json:"refusals"`
	// RefusalRate is Refusals/Turns as a percentage, one decimal.
	RefusalRate float64 `json:"refusal_rate"`
	// TopClass is the most frequent refusal class, empty when none.
	TopClass string `json:"top_class,omitempty"`
	// HardRefusals is the count of outcomes that ended in handoff or that
	// the hard classifier marked; a stonewall is a stronger signal than a
	// recoverable refusal.
	HardRefusals int `json:"hard_refusals"`
	// Repairs is the tool-call repair count.
	Repairs int `json:"repairs"`
	// PromptTokens and CompletionTokens come from logged usage events.
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ModelReliabilityStats aggregates the per-model rows across every session
// store in dir. Sessions without a meta model are attributed to "unknown".
// The rows sort by refusal rate descending, then model.
func ModelReliabilityStats(dir string) ([]ModelReliability, error) {
	return ModelReliabilityStatsFiltered(dir, nil)
}

// ModelReliabilityStatsFiltered is ModelReliabilityStats with a session
// predicate: a nil keep means every session. The windowed report uses it so
// --since filters honestly at the source, not after aggregation.
func ModelReliabilityStatsFiltered(dir string, keep func(sessionID string) bool) ([]ModelReliability, error) {
	rows := map[modelKey]*ModelReliability{}
	classCounts := map[modelKey]map[string]int{}

	eachSessionID(dir, func(sessionID string, entries []entry) {
		if keep != nil && !keep(sessionID) {
			return
		}
		model, persona := "unknown", "unknown"
		for _, e := range entries {
			v, err := json.Marshal(e.V)
			if err != nil {
				continue
			}
			switch e.Kind {
			case "meta":
				var meta map[string]any
				if json.Unmarshal(v, &meta) == nil {
					if m, ok := meta["model"].(string); ok && m != "" {
						model = m
					}
					if p, ok := meta["persona"].(string); ok && p != "" {
						persona = p
					}
				}
			case "user":
				k := modelKey{model, persona}
				rows[k] = ensureRow(rows, k)
				rows[k].Turns++
			case "refusal":
				var ev struct {
					Class   string `json:"class"`
					Outcome string `json:"outcome"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				k := modelKey{model, persona}
				rows[k] = ensureRow(rows, k)
				rows[k].Refusals++
				if ev.Outcome == "handoff" || ev.Outcome == "hard" {
					rows[k].HardRefusals++
				}
				if ev.Class != "" {
					if classCounts[k] == nil {
						classCounts[k] = map[string]int{}
					}
					classCounts[k][ev.Class]++
				}
			case "tool_repair":
				k := modelKey{model, persona}
				rows[k] = ensureRow(rows, k)
				rows[k].Repairs++
			case "usage":
				var ev struct {
					Prompt     int `json:"prompt_tokens"`
					Completion int `json:"completion_tokens"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				k := modelKey{model, persona}
				rows[k] = ensureRow(rows, k)
				rows[k].PromptTokens += ev.Prompt
				rows[k].CompletionTokens += ev.Completion
			}
		}
	})

	out := make([]ModelReliability, 0, len(rows))
	for k, r := range rows {
		if cc := classCounts[k]; len(cc) > 0 {
			top, n := "", 0
			for class, count := range cc {
				if count > n || (count == n && class < top) {
					top, n = class, count
				}
			}
			r.TopClass = top
		}
		if r.Turns > 0 {
			r.RefusalRate = float64(r.Refusals) / float64(r.Turns) * 100
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RefusalRate != out[j].RefusalRate {
			return out[i].RefusalRate > out[j].RefusalRate
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Persona < out[j].Persona
	})
	return out, nil
}

// modelKey groups the aggregation by model and persona: the same model under
// two personas has two behaviours, and the rows say so.
type modelKey struct{ model, persona string }

func ensureRow(rows map[modelKey]*ModelReliability, k modelKey) *ModelReliability {
	if rows[k] == nil {
		rows[k] = &ModelReliability{Model: k.model, Persona: k.persona}
	}
	return rows[k]
}

// FormatModelReliability renders the table. It states its denominators: a
// model with two turns is not a ranking, and the header says so.
func FormatModelReliability(rows []ModelReliability) string {
	if len(rows) == 0 {
		return "no model telemetry yet — run a task first"
	}
	var b strings.Builder
	b.WriteString("model                          persona               turns  refus%  hard  top class        repairs   tokens in/out\n")
	for _, r := range rows {
		top := r.TopClass
		if top == "" {
			top = "—"
		}
		fmt.Fprintf(&b, "%-30s %-21s %5d %6.1f%% %5d  %-16s %7d  %d/%d\n",
			clipCol(r.Model, 30), clipCol(r.Persona, 21), r.Turns, r.RefusalRate, r.HardRefusals,
			clipCol(top, 16), r.Repairs, r.PromptTokens, r.CompletionTokens)
	}
	b.WriteString("\nrefus% is interceptions per operator turn; hard is stonewalls that ended in handoff.\n")
	b.WriteString("A model with very few turns is not a ranking — the denominator is in the row.\n")
	return strings.TrimRight(b.String(), "\n")
}

// clip shortens s to width runes, with an ellipsis when it must cut. The
// table's columns are fixed, and a long local model id must not break them.
func clipCol(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}
