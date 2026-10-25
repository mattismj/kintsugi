package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RepairStat is one (model, tool, class) counter from the repair telemetry
// stream (DESIGN I-9). Sessions live in per-session SQLite stores (legacy JSONL
// files are imported on touch); the model comes from the session's meta entry,
// so counters survive without a cross-session index.
type RepairStat struct {
	Model string `json:"model"`
	Tool  string `json:"tool"`
	Class string `json:"class"`
	Count int    `json:"count"`
}

// RepairStats aggregates tool_repair events across all sessions in dir.
// tool_repair_failed events are counted under class "unrepaired".
func RepairStats(dir string) ([]RepairStat, error) {
	counts := map[RepairStat]int{}

	eachSession(dir, func(entries []entry) {
		model := "unknown"
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
				}
			case "tool_repair":
				var ev struct {
					Tool    string   `json:"tool"`
					Classes []string `json:"classes"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				for _, c := range ev.Classes {
					counts[RepairStat{Model: model, Tool: ev.Tool, Class: c}]++
				}
			case "tool_repair_failed":
				var ev struct {
					Tool string `json:"tool"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				counts[RepairStat{Model: model, Tool: ev.Tool, Class: "unrepaired"}]++
			}
		}
	})

	out := make([]RepairStat, 0, len(counts))
	for k, n := range counts {
		k.Count = n
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		return out[i].Class < out[j].Class
	})
	return out, nil
}

// FormatRepairStats renders the counters as an aligned table.
func FormatRepairStats(stats []RepairStat) string {
	if len(stats) == 0 {
		return "no repairs recorded"
	}
	var b strings.Builder
	b.WriteString("count  model                          tool            class\n")
	for _, s := range stats {
		fmt.Fprintf(&b, "%-6d %-30s %-15s %s\n", s.Count, s.Model, s.Tool, s.Class)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RefusalStat is one (model, persona, class, outcome) counter from the
// stream-level interception telemetry stream (DESIGN §6.7). Session is the id
// the counter came from: counts are per session so a caller can tell a fresh
// session from a resumed one. It carries no phrase and no text — the phrase is
// dropped at the telemetry boundary and a refusal is never memory.
type RefusalStat struct {
	Model   string `json:"model"`
	Persona string `json:"persona"`
	Class   string `json:"class"`
	Outcome string `json:"outcome"`
	Session string `json:"session,omitempty"`
	Count   int    `json:"count"`
}

// RefusalStats aggregates refusal events across all sessions in dir.
func RefusalStats(dir string) ([]RefusalStat, error) {
	counts := map[RefusalStat]int{}

	eachSessionID(dir, func(id string, entries []entry) {
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
			case "refusal":
				var ev struct {
					Class   string `json:"class"`
					Outcome string `json:"outcome"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				counts[RefusalStat{Model: model, Persona: persona, Class: ev.Class, Outcome: ev.Outcome, Session: id}]++
			}
		}
	})

	out := make([]RefusalStat, 0, len(counts))
	for k, n := range counts {
		k.Count = n
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		if out[i].Persona != out[j].Persona {
			return out[i].Persona < out[j].Persona
		}
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		if out[i].Outcome != out[j].Outcome {
			return out[i].Outcome < out[j].Outcome
		}
		return out[i].Session < out[j].Session
	})
	return out, nil
}

// FormatRefusalStats renders the per-(model, persona, class) rate table.
func FormatRefusalStats(stats []RefusalStat) string {
	if len(stats) == 0 {
		return "no refusal interceptions recorded"
	}
	var b strings.Builder
	b.WriteString("count  model                          persona               class               outcome\n")
	for _, s := range stats {
		fmt.Fprintf(&b, "%-6d %-30s %-21s %-19s %s\n", s.Count, s.Model, s.Persona, s.Class, s.Outcome)
	}
	return strings.TrimRight(b.String(), "\n")
}

// DriftStat is one (model, persona, class, outcome) counter of continuity drift
// events. A drift event is metadata only — a task hash, matched ledger ids, a
// class label and an outcome — so there is nothing here that could carry a
// phrase or a transcript.
type DriftStat struct {
	Model   string `json:"model"`
	Persona string `json:"persona"`
	Class   string `json:"class"`
	Outcome string `json:"outcome"`
	Count   int    `json:"count"`
}

// DriftStats aggregates kind=="drift" events across all sessions in dir. Drift
// is written by the continuity guard through the session logger; the ledger
// keeps its own copy of the same metadata for recall accounting.
func DriftStats(dir string) ([]DriftStat, error) {
	counts := map[DriftStat]int{}

	eachSessionID(dir, func(_ string, entries []entry) {
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
			case "drift":
				// Only the labels are read. A drift payload has no text field at
				// all, and anything unrecognised is ignored rather than kept.
				var ev struct {
					Class   string `json:"class"`
					Outcome string `json:"outcome"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				counts[DriftStat{Model: model, Persona: persona, Class: ev.Class, Outcome: ev.Outcome}]++
			}
		}
	})

	out := make([]DriftStat, 0, len(counts))
	for k, n := range counts {
		k.Count = n
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		if out[i].Persona != out[j].Persona {
			return out[i].Persona < out[j].Persona
		}
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Outcome < out[j].Outcome
	})
	return out, nil
}

// CacheStat is one model's prompt-cache token accounting (M10-A): how many
// tokens the provider reused from the stable prefix (Read) and how many it had
// to write into the cache first (Written). It is metadata only — counters and a
// model id — so it carries no prompt text.
type CacheStat struct {
	Model   string `json:"model"`
	Read    int64  `json:"read"`
	Written int64  `json:"written"`
}

// CacheStats aggregates prompt-cache token counts per model across every
// session in dir. It reads a `cache` event of the shape
//
//	{"model":"claude-…","read":80,"written":15}
//
// and sums Read/Written per model, attributing each event to the model named in
// its own payload (falling back to the session's `meta` model when the event
// omits one). Unreadable sessions and events are skipped, exactly like the
// other aggregators here.
//
// NOTE ON EMISSION: the agent loop's OnUsage callback is
// `func(promptTokens, completionTokens int)` — it does not carry the cache
// counters the provider now parses into Usage.CacheReadTokens/CacheWriteTokens,
// and no caller logs a `cache` event yet. Until the coordinator widens that
// callback and emits the event (see the WP M10-A handoff), CacheStats returns
// an empty slice for a live store. It is correct and fully tested against the
// event shape; it simply has nothing to read until an emitter exists.
func CacheStats(dir string) ([]CacheStat, error) {
	totals := map[string]*CacheStat{}

	eachSession(dir, func(entries []entry) {
		metaModel := "unknown"
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
						metaModel = m
					}
				}
			case "cache":
				var ev struct {
					Model   string `json:"model"`
					Read    int64  `json:"read"`
					Written int64  `json:"written"`
				}
				if json.Unmarshal(v, &ev) != nil {
					continue
				}
				model := ev.Model
				if model == "" {
					model = metaModel
				}
				st, ok := totals[model]
				if !ok {
					st = &CacheStat{Model: model}
					totals[model] = st
				}
				st.Read += ev.Read
				st.Written += ev.Written
			}
		}
	})

	out := make([]CacheStat, 0, len(totals))
	for _, st := range totals {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// FormatCacheStats renders the per-model cache accounting as an aligned table.
func FormatCacheStats(stats []CacheStat) string {
	if len(stats) == 0 {
		return "no cache events recorded"
	}
	var b strings.Builder
	b.WriteString("model                          read       written\n")
	for _, s := range stats {
		fmt.Fprintf(&b, "%-30s %-10d %d\n", s.Model, s.Read, s.Written)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SessionTurns returns the operator-turn count per session id in dir. It is the
// same number Info.Turns reports (InfoOf), scoped to an explicit directory so
// analytics can run over a fixture or an imported cache instead of only the
// live session store. A session with at most one turn never got a second task,
// which is what makes a refusal there "fresh".
func SessionTurns(dir string) (map[string]int, error) {
	out := map[string]int{}
	ids, err := listIDs(dir)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		entries, err := readEntriesAt(dir, id)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Kind == "user" {
				out[id]++
			}
		}
	}
	return out, nil
}

// eachSessionID is eachSession with the session id exposed, so per-session
// counters (refusals, drift) can be attributed without a second directory scan.
func eachSessionID(dir string, fn func(id string, entries []entry)) {
	ids, err := listIDs(dir)
	if err != nil {
		return
	}
	for _, id := range ids {
		entries, err := readEntriesAt(dir, id)
		if err != nil {
			continue
		}
		fn(id, entries)
	}
}

// ThinkingStat is the reasoning volume one session produced across all its
// turns: the thinking is the operator's data, and this is its size. It is a
// counter, not content — the text stays in the session store.
type ThinkingStat struct {
	Session string `json:"session"`
	Turns   int    `json:"turns"`
	Bytes   int    `json:"bytes"`
}

// ThinkingStats aggregates per-session thinking volume across every store in
// dir. A store that cannot be read is skipped, never fatal.
func ThinkingStats(dir string) ([]ThinkingStat, error) {
	var out []ThinkingStat
	eachSessionID(dir, func(id string, entries []entry) {
		st := ThinkingStat{Session: id}
		for _, e := range entries {
			if e.Kind != "thinking" {
				continue
			}
			if s, ok := e.V.(string); ok {
				st.Turns++
				st.Bytes += len(s)
			}
		}
		if st.Turns > 0 {
			out = append(out, st)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out, nil
}
