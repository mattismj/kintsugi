package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/doctor"
	"kintsugi/internal/report"
	"kintsugi/internal/session"
)

// cmdDoctorRetention is the `kintsugi doctor --retention` path: it sweeps the
// session store against config's [retention] policy, prints what it would drop
// and only removes files with --apply. Kept separate from cmdDoctor so the
// readiness checks stay read-only.
func cmdDoctorRetention(args []string) {
	fs := flag.NewFlagSet("doctor retention", flag.ExitOnError)
	apply := fs.Bool("apply", false, "delete the sessions listed (default: report only)")
	// Accepted (and ignored) so cmdDoctor can forward its args verbatim: the
	// flag is what routed here in the first place.
	fs.Bool("retention", true, "no-op: selects the retention sweep")
	fs.Parse(args)

	dir, err := session.Dir()
	if err != nil {
		fail("doctor retention: %v", err)
	}

	// The coordinator's cmdDoctor already resolved the workspace; resolve it
	// the same way main does so the workspace kintsugi.toml is honoured.
	ws, _ := os.Getwd()
	if v := os.Getenv("KINTSUGI_WORKSPACE"); v != "" {
		ws = v
	}
	cfg, _, err := config.Load(ws)
	if err != nil {
		fail("doctor retention: %v", err)
	}
	policy := cfg.Retention

	now := time.Now()
	rep, err := doctor.Sweep(dir, policy.MaxSessions, policy.MaxAgeDays, now)
	if err != nil {
		fail("doctor retention: %v", err)
	}

	fmt.Printf("session store  %s\n", dir)
	fmt.Printf("policy         max_sessions=%s max_age_days=%s\n",
		policyCount(policy.MaxSessions), policyCount(policy.MaxAgeDays))
	fmt.Printf("found          %d file(s), %d kept\n", len(rep.Delete)+len(rep.Keep), len(rep.Keep))

	if len(rep.Delete) == 0 {
		if policy.MaxSessions == 0 && policy.MaxAgeDays == 0 {
			fmt.Println("\nno session expires: retention is unlimited (set [retention] in config.toml)")
		} else {
			fmt.Println("\nnothing to delete · retention policy satisfied")
		}
		return
	}

	fmt.Printf("\n%-26s %-10s %10s  %s\n", "id", "age", "size", "path")
	for _, c := range rep.Delete {
		fmt.Printf("%-26s %-10s %10d  %s\n", c.ID, humanAge(now.Sub(c.Modified)), c.Bytes, c.Path)
	}
	fmt.Printf("\nreclaim %d bytes across %d file(s)\n", rep.BytesReclaimed, len(rep.Delete))

	if !*apply {
		fmt.Println("report only; pass --apply to delete these files")
		return
	}

	var removed, freed int
	for _, c := range rep.Delete {
		if err := os.Remove(c.Path); err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", c.Path, err)
			continue
		}
		removed++
		freed += int(c.Bytes)
	}
	fmt.Printf("deleted %d file(s), reclaimed %d bytes\n", removed, freed)
}

func policyCount(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}

func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours() / 24)
	switch {
	case days >= 1:
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// cmdReport is `kintsugi report`: render the workspace findings report to stdout or
// to --out. Load never fails outright, so a partial environment still renders.
func cmdReport(ws string, args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	out := fs.String("out", "", "write the report to this path instead of stdout")
	fs.Parse(args)

	in, err := report.Load(ws)
	if err != nil {
		fail("report: %v", err)
	}
	md := report.Render(in)

	if *out == "" {
		fmt.Print(md)
		return
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		fail("report: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
}
