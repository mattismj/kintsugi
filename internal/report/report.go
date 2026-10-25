// Package report renders the workspace findings report: a plain-text, no-color
// Markdown summary of findings, refusal/repair telemetry and quota state.
//
// Output follows the harness eyebrow style (`// 01 summary`): zero-padded
// section numbers, plain-text tables, no ANSI. Every source is optional — a
// missing input renders a "no data" line instead of failing, so `kintsugi report`
// works on a fresh checkout.
package report

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/config"
	"kintsugi/internal/quota"
	"kintsugi/internal/session"
)

// QuotaRow is one provider's usage line in the report. Used/Limit are tokens;
// Limit of 0 means unlimited.
type QuotaRow struct {
	Provider string
	Used     int64
	Limit    int64
	Reset    string
	Note     string
}

// Inputs is everything Render needs. All fields are optional.
type Inputs struct {
	Project  string
	Findings string // raw markdown from .kintsugi/findings.md, empty when absent
	Repairs  []session.RepairStat
	Refusals []session.RefusalStat
	Thinking []session.ThinkingStat
	Quota    []QuotaRow
}

// Logf is where Load reports a source it could not read. Tests swap it.
var Logf = func(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// Section titles, in report order.
const (
	titleSummary  = "// 01 summary"
	titleFindings = "// 02 findings"
	titleRefusals = "// 03 refusal telemetry"
	titleRepairs  = "// 04 repair telemetry"
	titleQuota    = "// 05 quota"
)

// noData is the placeholder for a source with nothing to show.
const noData = "no data"

// Render returns the report as plain Markdown. Ordering is fully deterministic:
// sections are fixed, telemetry rows sort by count then keys, quota rows sort
// by provider.
func Render(in Inputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# kintsugi report · %s\n\n", projectLabel(in.Project))

	writeSection(&b, titleSummary, summaryBody(in))
	writeSection(&b, titleFindings, findingsBody(in))
	writeSection(&b, titleRefusals, refusalBody(in))
	writeSection(&b, titleRepairs, repairBody(in))
	writeSection(&b, titleQuota, quotaBody(in))
	return b.String()
}

func writeSection(b *strings.Builder, title, body string) {
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\n")
}

func projectLabel(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "(unset)"
	}
	return p
}

// summaryBody is the at-a-glance block: counts only, so it never hides the
// detail sections below it.
func summaryBody(in Inputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %s\n", "project", projectLabel(in.Project))

	switch n := findingsLines(in.Findings); {
	case n == 0:
		fmt.Fprintf(&b, "%-16s %s\n", "findings", noData)
	default:
		fmt.Fprintf(&b, "%-16s %d lines\n", "findings", n)
	}

	if len(in.Repairs) == 0 {
		fmt.Fprintf(&b, "%-16s %s\n", "repairs", noData)
	} else {
		fmt.Fprintf(&b, "%-16s %d rows, %d events\n", "repairs", len(in.Repairs), countRepairs(in.Repairs))
	}

	if len(in.Refusals) == 0 {
		fmt.Fprintf(&b, "%-16s %s\n", "refusals", noData)
	} else {
		fmt.Fprintf(&b, "%-16s %d rows, %d events\n", "refusals", len(in.Refusals), countRefusals(in.Refusals))
	}

	if len(in.Quota) == 0 {
		fmt.Fprintf(&b, "%-16s %s\n", "quota", noData)
	} else {
		fmt.Fprintf(&b, "%-16s %d provider(s)\n", "quota", len(in.Quota))
	}

	// The thinking volume: reasoning bytes the models produced, per session.
	// A count, never the content — the text stays in the session store.
	turns, bytes := 0, 0
	for _, t := range in.Thinking {
		turns += t.Turns
		bytes += t.Bytes
	}
	if turns == 0 {
		fmt.Fprintf(&b, "%-16s %s\n", "thinking", noData)
	} else {
		fmt.Fprintf(&b, "%-16s %d turn(s), %d bytes\n", "thinking", turns, bytes)
	}
	return b.String()
}

func findingsLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// findingsBody passes the operator-authored markdown through verbatim.
func findingsBody(in Inputs) string {
	body := strings.TrimRight(in.Findings, "\n")
	if strings.TrimSpace(body) == "" {
		return noData + " (write .kintsugi/findings.md to populate this section)"
	}
	return body
}

func sortRepairs(in []session.RepairStat) []session.RepairStat {
	out := append([]session.RepairStat(nil), in...)
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
	return out
}

func sortRefusals(in []session.RefusalStat) []session.RefusalStat {
	out := append([]session.RefusalStat(nil), in...)
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
	return out
}

func countRepairs(in []session.RepairStat) int {
	n := 0
	for _, s := range in {
		n += s.Count
	}
	return n
}

func countRefusals(in []session.RefusalStat) int {
	n := 0
	for _, s := range in {
		n += s.Count
	}
	return n
}

func refusalBody(in Inputs) string {
	if len(in.Refusals) == 0 {
		return noData + " (no refusal interceptions recorded)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-6s %-30s %-21s %-19s %s\n", "count", "model", "persona", "class", "outcome")
	for _, s := range sortRefusals(in.Refusals) {
		fmt.Fprintf(&b, "%-6d %-30s %-21s %-19s %s\n", s.Count, dash(s.Model), dash(s.Persona), dash(s.Class), dash(s.Outcome))
	}
	return b.String()
}

func repairBody(in Inputs) string {
	if len(in.Repairs) == 0 {
		return noData + " (no tool-call repairs recorded)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-6s %-30s %-15s %s\n", "count", "model", "tool", "class")
	for _, s := range sortRepairs(in.Repairs) {
		fmt.Fprintf(&b, "%-6d %-30s %-15s %s\n", s.Count, dash(s.Model), dash(s.Tool), dash(s.Class))
	}
	return b.String()
}

func sortQuota(in []QuotaRow) []QuotaRow {
	out := append([]QuotaRow(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Used != out[j].Used {
			return out[i].Used < out[j].Used
		}
		return out[i].Limit < out[j].Limit
	})
	return out
}

func quotaBody(in Inputs) string {
	if len(in.Quota) == 0 {
		return noData + " (no provider quota configured or recorded)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %12s %12s %-8s %s\n", "provider", "used", "limit", "reset", "note")
	for _, r := range sortQuota(in.Quota) {
		limit := fmt.Sprintf("%d", r.Limit)
		if r.Limit <= 0 {
			limit = "unlimited"
		}
		fmt.Fprintf(&b, "%-16s %12d %12s %-8s %s\n", dash(r.Provider), r.Used, limit, dash(r.Reset), dash(r.Note))
	}
	return b.String()
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// Load gathers the report inputs for a workspace. It never fails: any source
// that cannot be read is logged through Logf and left empty, so a partial
// environment still renders a valid report.
func Load(workspace string) (Inputs, error) {
	in := Inputs{Project: projectName(workspace)}

	// 1. Operator findings (workspace-authored markdown).
	if workspace != "" {
		p := filepath.Join(workspace, ".kintsugi", "findings.md")
		b, err := os.ReadFile(p)
		switch {
		case err == nil:
			in.Findings = string(b)
		case os.IsNotExist(err):
			// Absent is normal; the section renders its "no data" line.
		default:
			Logf("report: findings: %v", err)
		}
	}

	// 2. Session telemetry. RepairStats/RefusalStats take the session dir and
	// aggregate every store in it.
	if dir, err := session.Dir(); err != nil {
		Logf("report: sessions: %v", err)
	} else {
		if reps, err := session.RepairStats(dir); err != nil {
			Logf("report: repair stats: %v", err)
		} else {
			in.Repairs = reps
		}
		if refs, err := session.RefusalStats(dir); err != nil {
			Logf("report: refusal stats: %v", err)
		} else {
			in.Refusals = refs
		}
		if think, err := session.ThinkingStats(dir); err != nil {
			Logf("report: thinking stats: %v", err)
		} else {
			in.Thinking = think
		}
	}

	// 3. Quota ledger. Limits/reset/note come from config (the ledger itself
	// only stores usage); without config the store opens with no limits.
	limits := quotaLimits(workspace)
	store, err := quota.Open("", limits)
	if err != nil {
		Logf("report: quota: %v", err)
		return in, nil
	}
	for _, s := range store.Snapshot() {
		in.Quota = append(in.Quota, QuotaRow{
			Provider: s.Provider,
			Used:     s.Used,
			Limit:    s.Limit,
			Reset:    s.Reset,
			Note:     limits[s.Provider].Note,
		})
	}
	return in, nil
}

// quotaLimits maps config's quota table onto the ledger's limit type. A config
// error is not fatal here: the report degrades to usage-only rows.
func quotaLimits(workspace string) map[string]quota.Limit {
	cfg, _, err := config.Load(workspace)
	if err != nil {
		Logf("report: config: %v", err)
		return nil
	}
	if len(cfg.Quota) == 0 {
		return nil
	}
	out := make(map[string]quota.Limit, len(cfg.Quota))
	for name, q := range cfg.Quota {
		out[name] = quota.Limit{LimitTokens: q.LimitTokens, Reset: q.Reset, Note: q.Note}
	}
	return out
}

func projectName(workspace string) string {
	if strings.TrimSpace(workspace) == "" {
		return ""
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		abs = workspace
	}
	base := filepath.Base(abs)
	if base == "." || base == string(filepath.Separator) {
		return abs
	}
	return base
}

// Write renders in and writes it to w.
func Write(w io.Writer, in Inputs) error {
	_, err := io.WriteString(w, Render(in))
	return err
}
