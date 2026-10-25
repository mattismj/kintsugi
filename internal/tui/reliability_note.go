package tui

// The model-change recommendation on a hard-refusal card: what the session
// telemetry says has handled this operator's work with fewer refusals. It is
// a suggestion with its evidence, never an order — the operator switches.

import (
	"fmt"
	"strings"

	"kintsugi/internal/session"
)

// modelRecommendation names up to three models with a clean refusal record
// (at least two turns, zero refusals), excluding the current model. With no
// data it says so honestly instead of inventing a pick.
func (m *Model) modelRecommendation() string {
	dir, err := session.Dir()
	if err != nil {
		return "· switch with /model when you are ready"
	}
	rows, err := session.ModelReliabilityStats(dir)
	if err != nil || len(rows) == 0 {
		return "· switch with /model when you are ready · no reliability data yet"
	}
	current := ""
	if m.res != nil {
		current = m.res.ProviderName + "/" + m.res.Model.ID
	}
	var picks []string
	for _, r := range rows {
		if r.Model == current || r.RefusalRate > 0 || r.Turns < 2 {
			continue
		}
		picks = append(picks, fmt.Sprintf("%s (%d turns, 0 refusals)", r.Model, r.Turns))
		if len(picks) >= 3 {
			break
		}
	}
	if len(picks) == 0 {
		return "· no model in your telemetry has a clean record yet · switch with /model and compare"
	}
	return "· handled this work cleanly: " + strings.Join(picks, "; ")
}
