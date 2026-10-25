package report

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/session"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixture is the canonical report input: findings text, two repairs, two
// refusals, two quota rows. Rows are deliberately declared out of order to
// prove Render sorts deterministically.
func fixture() Inputs {
	return Inputs{
		Project:  "kintsugi",
		Findings: "## findings\n\n- `kintsugi doctor` reports a clean environment\n- session store has 4 stores, 2 legacy JSONL files\n",
		Repairs: []session.RepairStat{
			{Model: "qwen/qwen3-coder", Tool: "bash", Class: "arg_quotes", Count: 4},
			{Model: "deepseek/deepseek-chat", Tool: "edit", Class: "unrepaired", Count: 1},
		},
		Refusals: []session.RefusalStat{
			{Model: "qwen/qwen3-coder", Persona: "_base", Class: "policy", Outcome: "retried", Count: 1},
			{Model: "deepseek/deepseek-chat", Persona: "deepseek-flash41", Class: "safety", Outcome: "handoff", Count: 3},
		},
		Quota: []QuotaRow{
			{Provider: "moonshot", Used: 12000, Limit: 0, Reset: "", Note: ""},
			{Provider: "deepseek", Used: 250000, Limit: 2000000, Reset: "daily", Note: "flat plan"},
		},
	}
}

func golden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test ./internal/report -update`)", path, err)
	}
	if got != string(want) {
		t.Fatalf("render mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestRenderGolden(t *testing.T) {
	golden(t, "golden_full.md", Render(fixture()))
}

func TestRenderEmptySkeleton(t *testing.T) {
	out := Render(Inputs{})
	golden(t, "golden_empty.md", out)

	// A skeleton is still a valid report: every section is present and says
	// so, and nothing panics on nil slices.
	for _, want := range []string{"// 01 summary", "// 02 findings", "// 03 refusal telemetry", "// 04 repair telemetry", "// 05 quota", noData, "(unset)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("empty skeleton missing %q:\n%s", want, out)
		}
	}
	// Five summary rows (project is "(unset)", the rest no-data) plus the
	// four detail bodies.
	if n := strings.Count(out, noData); n != 9 {
		t.Fatalf("want 9 no-data mentions, got %d:\n%s", n, out)
	}
}

func TestRenderSectionOrder(t *testing.T) {
	out := Render(fixture())
	idx := make([]int, 0, 5)
	for _, h := range []string{titleSummary, titleFindings, titleRefusals, titleRepairs, titleQuota} {
		i := strings.Index(out, h)
		if i < 0 {
			t.Fatalf("missing section %q", h)
		}
		idx = append(idx, i)
	}
	for i := 1; i < len(idx); i++ {
		if idx[i-1] >= idx[i] {
			t.Fatalf("sections out of order: %v", idx)
		}
	}
}

func TestRenderDeterministicSort(t *testing.T) {
	a := fixture()
	// Same content, reversed declaration order.
	b := fixture()
	b.Repairs[0], b.Repairs[1] = b.Repairs[1], b.Repairs[0]
	b.Refusals[0], b.Refusals[1] = b.Refusals[1], b.Refusals[0]
	b.Quota[0], b.Quota[1] = b.Quota[1], b.Quota[0]

	if Render(a) != Render(b) {
		t.Fatalf("Render is order-dependent:\n--- a ---\n%s\n--- b ---\n%s", Render(a), Render(b))
	}

	out := Render(a)
	// Repairs by count desc, refusals by count desc, quota by provider asc.
	if strings.Index(out, "arg_quotes") > strings.Index(out, "unrepaired") {
		t.Fatalf("repairs not sorted by count desc:\n%s", out)
	}
	if strings.Index(out, "deepseek/deepseek-chat") > strings.Index(out, "qwen/qwen3-coder") {
		t.Fatalf("refusals not sorted by count desc:\n%s", out)
	}
	if strings.Index(out, "flat plan") > strings.Index(out, "unlimited") {
		t.Fatalf("quota rows not sorted by provider:\n%s", out)
	}
}

func TestRenderDoesNotMutateInputs(t *testing.T) {
	in := fixture()
	first := in.Repairs[0]
	_ = Render(in)
	if in.Repairs[0] != first {
		t.Fatalf("Render reordered the caller's slice: %+v", in.Repairs)
	}
}

func TestRenderQuotaUnlimited(t *testing.T) {
	out := Render(Inputs{Project: "kintsugi", Quota: []QuotaRow{{Provider: "moonshot", Used: 500}}})
	if !strings.Contains(out, "unlimited") {
		t.Fatalf("limit 0 should render as unlimited:\n%s", out)
	}
}
