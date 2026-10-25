package continuity

import (
	"fmt"
	"sort"
	"strings"

	"kintsugi/internal/session"
)

// Continuity trends (M8.5.7): one deterministic table over the interception
// telemetry and the non-recallable drift events. The point is to let an
// operator see where a model or persona stalls — and its own footprint — from
// counters alone.
//
// A refusal is never memory, and that holds here too: a row is
// (model, persona, class, outcome) plus integers. The phrase that produced a
// counter was dropped at the telemetry boundary, no drift event carries prose,
// and nothing in this file reads or prints a transcript.

// TrendRow is one aggregated line of the trends table: refusal interceptions
// grouped by (model, persona, class, outcome), split by whether the session was
// fresh (a single operator turn) or resumed, plus the drift events recorded for
// the same key.
type TrendRow struct {
	Model   string
	Persona string
	Class   string
	Outcome string
	Count   int // refusal interceptions
	Fresh   int // … in sessions with at most one operator turn
	Resumed int // … in sessions that were resumed (more than one turn)
	Drift   int // non-recallable drift events with this key
}

// Trends aggregates the session telemetry in dir into rows. A refusal is
// "fresh" when it happened in a session that only ever saw one operator turn:
// that is the case a first-task preflight can prevent, whereas a resumed
// session already carried authorization context. Drift events are counted, never
// rendered: they are metadata.
//
// The result is sorted deterministically: count desc, then model, persona,
// class, outcome.
func Trends(dir string) ([]TrendRow, error) {
	stats, err := session.RefusalStats(dir)
	if err != nil {
		return nil, err
	}
	turns, err := session.SessionTurns(dir)
	if err != nil {
		return nil, err
	}
	drifts, err := session.DriftStats(dir)
	if err != nil {
		return nil, err
	}

	type key struct{ model, persona, class, outcome string }
	rows := map[key]*TrendRow{}
	row := func(k key) *TrendRow {
		r, ok := rows[k]
		if !ok {
			r = &TrendRow{Model: k.model, Persona: k.persona, Class: k.class, Outcome: k.outcome}
			rows[k] = r
		}
		return r
	}

	for _, s := range stats {
		r := row(key{s.Model, s.Persona, s.Class, s.Outcome})
		r.Count += s.Count
		// Info.Turns counts operator turns; <=1 means the session never got as
		// far as a second task.
		if turns[s.Session] <= 1 {
			r.Fresh += s.Count
		} else {
			r.Resumed += s.Count
		}
	}
	for _, d := range drifts {
		row(key{d.Model, d.Persona, d.Class, d.Outcome}).Drift += d.Count
	}

	out := make([]TrendRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sortTrendRows(out)
	return out, nil
}

// sortTrendRows is the deterministic order of the table: count desc, then
// model, persona, class, outcome. It is a separate function so the order can be
// tested without manufacturing a session fixture.
func sortTrendRows(rows []TrendRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		if rows[i].Persona != rows[j].Persona {
			return rows[i].Persona < rows[j].Persona
		}
		if rows[i].Class != rows[j].Class {
			return rows[i].Class < rows[j].Class
		}
		return rows[i].Outcome < rows[j].Outcome
	})
}

// FormatTrends renders the rows as a plain, aligned table. Empty output is a
// sentence, not a header with no rows.
func FormatTrends(rows []TrendRow) string {
	if len(rows) == 0 {
		return "no continuity trends recorded"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-30s %-21s %-19s %-10s %6s %6s %7s %6s\n",
		"model", "persona", "class", "outcome", "count", "fresh", "resumed", "drift")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-30s %-21s %-19s %-10s %6d %6d %7d %6d\n",
			trendField(r.Model), trendField(r.Persona), trendField(r.Class), trendField(r.Outcome),
			r.Count, r.Fresh, r.Resumed, r.Drift)
	}
	return strings.TrimRight(b.String(), "\n")
}

// trendField keeps a column readable when a key is missing.
func trendField(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
