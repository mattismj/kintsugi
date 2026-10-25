package main

import (
	"encoding/json"
	"flag"
	"fmt"
)

func cmdQuota(ws string, args []string) {
	fs := flag.NewFlagSet("quota", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the ledger as JSON")
	fs.Parse(args)

	app := newApp(ws)
	store, err := app.QuotaStore()
	if err != nil {
		fail("quota: %v", err)
	}
	states := store.Snapshot()
	if *asJSON {
		b, _ := json.MarshalIndent(states, "", "  ")
		fmt.Println(string(b))
		return
	}
	if len(states) == 0 {
		fmt.Println("no quota configured (set [quota.<provider>] in config; usage is recorded per turn)")
		return
	}
	fmt.Printf("%-12s %12s %12s %12s  %-8s %s\n", "provider", "used", "limit", "remaining", "period", "reset")
	for _, s := range states {
		limit := "unlimited"
		remaining := "-"
		if s.Limit > 0 {
			limit = fmt.Sprintf("%d", s.Limit)
			remaining = fmt.Sprintf("%d", s.Remaining)
		}
		fmt.Printf("%-12s %12d %12s %12s  %-8s %s\n", s.Provider, s.Used, limit, remaining, s.Period, s.Reset)
	}
	fmt.Println("\nusage is approximate unless the provider returns exact token counts")
}
