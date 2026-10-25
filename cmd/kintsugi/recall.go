package main

import (
	"flag"
	"fmt"
	"strings"
)

// `kintsugi recall` is the operator-side view of the same recall the agent loop
// performs before a task: positive, operator-accepted prior work only. The
// ledger's Search never returns drift or superseded rows, so nothing here can
// surface a refusal.

func cmdRecall(ws string, args []string) {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	top := fs.Int("top", 0, "maximum entries to recall (0 = continuity.prior_work_top_k)")
	fs.Parse(args)

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fail(`usage: kintsugi recall "<query>" [--top 5]`)
	}

	app := newApp(ws)
	if !app.Cfg.Continuity.Enabled {
		fmt.Println("continuity is disabled: set [continuity] enabled = true to recall accepted work")
		return
	}
	if *top > 0 {
		// The recall width is config, but --top is a per-invocation override of
		// this process's copy of it; nothing is written back.
		app.Cfg.Continuity.PriorWorkTopK = *top
	}

	block, ids, err := app.RecallPriorWork(query)
	if err != nil {
		fail("%v", err)
	}
	if strings.TrimSpace(block) == "" {
		fmt.Printf("no prior accepted work matches %q\n", query)
		fmt.Printf("· ledger %s\n", app.LedgerDir())
		return
	}
	fmt.Println(block)
	fmt.Printf("\n· %d item(s) · ids %s\n", len(ids), strings.Join(ids, ", "))
	fmt.Printf("· budget %d tok (continuity.prior_work_budget_tokens) · ledger %s\n",
		app.Cfg.Continuity.PriorWorkBudgetTokens, app.LedgerDir())
}

// classLabel renders the telemetry classes a preflight scored. Classes are
// labels: the phrase that produced a counter was dropped at the telemetry
// boundary, so nothing here can quote a refusal.
func classLabel(classes []string) string {
	if len(classes) == 0 {
		return "none"
	}
	return strings.Join(classes, ", ")
}
