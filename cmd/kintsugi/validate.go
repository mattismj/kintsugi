package main

// `kintsugi validate`: a scripted live battery per provider/model with telemetry
// that feeds `kintsugi repair stats` and `kintsugi refusal stats` (WP-0.2).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kintsugi/internal/validate"
)

// modelFlags collects a repeatable --model flag. Comma-separated values are
// accepted too, so `--model a/b,c/d` works.
type modelFlags []string

func (m *modelFlags) String() string { return strings.Join(*m, ",") }

func (m *modelFlags) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

func cmdValidate(ws string, args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var models modelFlags
	fs.Var(&models, "model", "provider/model to validate (repeatable; default: every curated model)")
	provider := fs.String("provider", "", "restrict the battery to one provider")
	timeout := fs.Duration("timeout", 0, "overall battery timeout, e.g. 5m (0 = no bound)")
	fs.Parse(args)

	app := newApp(ws)
	targets, notes := validate.Targets(app, *provider, models)
	for _, n := range notes {
		fmt.Println("skip: " + n)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "no targets to validate")
		fmt.Fprintln(os.Stderr, "usage: kintsugi validate [--provider <name>] [--model provider/model ...] [--timeout 5m]")
		os.Exit(2)
	}

	// --timeout also caps a single reachability probe, never the other way
	// around: a short budget must not be stretched by the 15s default.
	if *timeout > 0 && *timeout < validate.PingTimeout {
		validate.PingTimeout = *timeout
	}
	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	rows := validate.Run(ctx, app, targets, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "· "+format+"\n", a...)
	})

	fmt.Printf("%-12s %-30s %-10s %-10s %8s %7s %-22s %s\n",
		"PROVIDER", "MODEL", "PING", "TOOL", "REFUSALS", "REPAIRS", "STACK", "VERDICT")
	for _, r := range rows {
		fmt.Printf("%-12s %-30s %-10s %-10s %8d %7d %-22s %s\n",
			r.Provider, vfit(r.Model, 30), pingCell(r), toolCell(r),
			r.Refusals, r.Repairs, stackCell(r), r.Verdict)
	}

	path, err := writeValidationRows(app.Workspace, rows)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: summary not written: %v\n", err)
	}

	pingFailed, ok := 0, 0
	for _, r := range rows {
		if !r.PingOK {
			pingFailed++
		}
		if r.Verdict == validate.VerdictOK {
			ok++
		}
	}
	summary := fmt.Sprintf("\n%d target(s) · %d ok · %d ping failed", len(rows), ok, pingFailed)
	if path != "" {
		summary += " · jsonl " + path
	}
	fmt.Println(summary)
	if pingFailed > 0 {
		os.Exit(1)
	}
}

// writeValidationRows appends one JSON object per row to
// <workspace>/.kintsugi/validate/<UTC timestamp>.jsonl.
func writeValidationRows(workspace string, rows []validate.Row) (string, error) {
	dir := filepath.Join(workspace, ".kintsugi", "validate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Two runs in the same second must not collide.
	stamp := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(dir, stamp+".jsonl")
	for i := 0; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.jsonl", stamp, i+1))
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return path, err
		}
	}
	return path, nil
}

func pingCell(r validate.Row) string {
	if r.PingOK {
		return fmt.Sprintf("ok %dms", r.PingMS)
	}
	if r.Err == "" {
		return "—"
	}
	return "fail"
}

func toolCell(r validate.Row) string {
	mark := "fail"
	if r.ToolOK {
		mark = "ok"
	}
	return fmt.Sprintf("%s(%d)", mark, r.ToolCalls)
}

func stackCell(r validate.Row) string {
	if r.StackHash == "" {
		return "—"
	}
	h := strings.TrimPrefix(r.StackHash, "sha256:")
	if len(h) > 8 {
		h = h[:8]
	}
	if r.StackTokens > 0 {
		return fmt.Sprintf("%d/%s", r.StackTokens, h)
	}
	return h
}

func vfit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
