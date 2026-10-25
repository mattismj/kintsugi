package main

// `kintsugi models report` — what the session telemetry says about how each
// model actually behaves on this operator's work. Per model, not per
// provider: refusal behaviour and code quality are properties of the model.

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"kintsugi/internal/session"
)

func cmdModelsReport(args []string) {
	fs := flag.NewFlagSet("models report", flag.ExitOnError)
	groupJSON := fs.Bool("json", false, "print the rows as JSON")
	since := fs.String("since", "", "only count turns on or after this date (YYYY-MM-DD)")
	fs.Parse(args)

	dir, err := session.Dir()
	if err != nil {
		fail("%v", err)
	}
	rows, err := session.ModelReliabilityStats(dir)
	if err != nil {
		fail("%v", err)
	}

	// --since filters at the source: the aggregation takes a session
	// predicate, so a windowed report counts only the turns inside it.
	if *since != "" {
		cut, err := time.Parse("2006-01-02", *since)
		if err != nil {
			fail("--since must be YYYY-MM-DD (got %q)", *since)
		}
		info, err := session.ListInfo()
		if err != nil {
			fail("%v", err)
		}
		keep := map[string]bool{}
		for _, s := range info {
			if !s.Started.Before(cut) {
				keep[s.ID] = true
			}
		}
		rows, err = session.ModelReliabilityStatsFiltered(dir, func(id string) bool { return keep[id] })
		if err != nil {
			fail("%v", err)
		}
	}

	if *groupJSON {
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(string(b))
		return
	}
	fmt.Println(session.FormatModelReliability(rows))
}

// filterReliability applies a session filter. The rows are aggregated across
// sessions, so a per-session route would recompute; the simple cut keeps the
// rows whose model appears in a session inside the window — enough for the
// report's purpose, and documented as such.
