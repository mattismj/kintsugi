package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"kintsugi/internal/audit"
)

// kintsugi audit (M11.3): the workspace's local, append-only security record.
//
// The audit log is not the transcript. The session log keeps the
// conversation; the audit log keeps what was allowed or refused, with the
// reason and the rule that decided it. It lives in the workspace, it is
// never uploaded, and no prompt ever carries it.
//
//	kintsugi audit              the last 50 events as a table
//	kintsugi audit --json       the same events as JSON
//	kintsugi audit --clear      truncate the log (asks first)

func cmdAudit(ws string, args []string) {
	if err := auditCommand(ws, args, os.Stdout, os.Stdin); err != nil {
		fail("%v", err)
	}
}

// auditDefaultLimit is how many events `kintsugi audit` prints when the
// operator names no limit.
const auditDefaultLimit = 50

// auditCommand is the testable body of `kintsugi audit`.
func auditCommand(ws string, args []string, out io.Writer, in io.Reader) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(out)
	asJSON := fs.Bool("json", false, "print the events as JSON")
	clearLog := fs.Bool("clear", false, "truncate the audit log")
	limit := fs.Int("n", auditDefaultLimit, "how many events to print")
	fs.IntVar(limit, "limit", auditDefaultLimit, "how many events to print")
	yes := fs.Bool("yes", false, "do not ask before clearing the log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *clearLog {
		return clearAuditLog(ws, out, in, *yes)
	}
	events, err := audit.Read(ws)
	if err != nil {
		return err
	}
	if *asJSON {
		if events == nil {
			events = []audit.Event{}
		}
		b, err := json.MarshalIndent(events, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(b))
		return nil
	}
	printAuditTable(out, ws, events, *limit)
	return nil
}

// printAuditTable prints the last limit events: time, tool, decision and the
// reason. The newest event is last, so the table reads like the log itself.
func printAuditTable(out io.Writer, ws string, events []audit.Event, limit int) {
	if len(events) == 0 {
		fmt.Fprintf(out, "no audit events yet (%s)\n", audit.Path(ws))
		return
	}
	shown := events
	if limit > 0 && len(shown) > limit {
		shown = shown[len(shown)-limit:]
	}
	toolW := len("TOOL")
	for _, ev := range shown {
		if n := len(ev.Tool); n > toolW {
			toolW = n
		}
	}
	if toolW > 20 {
		toolW = 20
	}
	fmt.Fprintf(out, "%-19s  %-*s  %-8s  %s\n", "TIME", toolW, "TOOL", "DECISION", "REASON")
	for _, ev := range shown {
		fmt.Fprintf(out, "%-19s  %-*s  %-8s  %s\n",
			auditTime(ev.TS), toolW, auditCell(ev.Tool), auditCell(ev.Decision),
			auditReason(ev))
	}
	fmt.Fprintf(out, "\n%d event(s) in %s\n", len(events), audit.Path(ws))
}

// auditTime renders a timestamp for the table. A timestamp that does not
// parse is shown as it was written: the record is never rewritten.
func auditTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// auditCell keeps a column on one line.
func auditCell(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	return strings.Join(strings.Fields(s), " ")
}

// auditReason is the reason column: the reason, and the rule when one
// decided. The rule is the part an operator checks a policy against.
func auditReason(ev audit.Event) string {
	reason := auditCell(ev.Reason)
	rule := auditCell(ev.Rule)
	switch {
	case reason == "—" && rule == "—":
		return "—"
	case rule == "—":
		return reason
	case reason == "—":
		return "rule " + rule
	}
	return reason + "  [rule " + rule + "]"
}

// clearAuditLog truncates the audit log. It asks first: emptying the
// security record is the operator's decision, never a side effect.
func clearAuditLog(ws string, out io.Writer, in io.Reader, assumeYes bool) error {
	path := audit.Path(ws)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(out, "no audit log at %s\n", path)
		return nil
	}
	if !assumeYes {
		fmt.Fprintf(out, "truncate the audit log at %s? [y/N] ", path)
		line, err := readAuditLine(in)
		if err != nil {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "audit log not cleared (no confirmation)")
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Fprintln(out, "audit log not cleared")
			return nil
		}
	}
	if err := audit.Clear(ws); err != nil {
		return err
	}
	fmt.Fprintf(out, "cleared %s\n", path)
	return nil
}

// readAuditLine reads one answer line.
func readAuditLine(in io.Reader) (string, error) {
	if in == nil {
		return "", fmt.Errorf("no input")
	}
	return bufio.NewReader(in).ReadString('\n')
}
