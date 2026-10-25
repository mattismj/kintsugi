package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"kintsugi/internal/harness"
	"kintsugi/internal/ledger"
)

// Context ledger commands (M8.5.1).
//
// The ledger is the operator-visible record of *positive*, accepted work in this
// workspace: plans, artifacts, decisions and acceptances, plus the
// non-recallable drift metadata the continuity guard writes. A refusal is never
// memory — a refusal is not a ledger kind at all, so there is no command here
// that could put one in the file, and `ledger list` renders drift rows as
// ids/class/outcome with no prose.

func cmdLedger(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi ledger list|add|export")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		cmdLedgerList(ws, rest)
	case "add":
		cmdLedgerAdd(ws, rest)
	case "export":
		cmdLedgerExport(ws, rest)
	default:
		fail("usage: kintsugi ledger list|add|export")
	}
}

// cmdLedgerList prints entries newest first: id, timestamp, kind, summary and
// the evidence count. --json prints the entries themselves.
func cmdLedgerList(ws string, args []string) {
	fs := flag.NewFlagSet("ledger list", flag.ExitOnError)
	kind := fs.String("kind", "", "plan|artifact|decision|acceptance|drift")
	asJSON := fs.Bool("json", false, "print the entries as JSON")
	fs.Parse(args)

	app := newApp(ws)
	entries, err := ledgerEntries(app, *kind, 0)
	if err != nil {
		fail("%v", err)
	}
	if *asJSON {
		b, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(string(b))
		return
	}
	if len(entries) == 0 {
		if strings.TrimSpace(*kind) != "" {
			fmt.Printf("no %s entries under %s\n", strings.TrimSpace(*kind), app.LedgerDir())
			return
		}
		fmt.Printf("no ledger entries under %s\n", app.LedgerDir())
		return
	}
	fmt.Printf("%-17s %-20s %-10s %4s  %s\n", "ID", "TS", "KIND", "EVID", "SUMMARY")
	for _, e := range entries {
		fmt.Printf("%-17s %-20s %-10s %4d  %s\n", e.ID, e.TS, e.Kind, len(e.Evidence), ledgerLine(e))
	}
}

// cmdLedgerAdd appends one accepted entry. Only the positive kinds are
// accepted: drift is the guard's own metadata event and a refusal is not a
// ledger kind, so the ledger package rejects both.
func cmdLedgerAdd(ws string, args []string) {
	fs := flag.NewFlagSet("ledger add", flag.ExitOnError)
	kind := fs.String("kind", "", "plan|artifact|decision|acceptance")
	summary := fs.String("summary", "", "what was accepted (operator-visible)")
	task := fs.String("task", "", "the task this work belongs to")
	operator := fs.String("operator", ledger.OperatorImplicit, "implicit|attested")
	var evidence repeatFlag
	fs.Var(&evidence, "evidence", "evidence path or sha256 hash (repeatable)")
	fs.Parse(args)

	switch strings.ToLower(strings.TrimSpace(*kind)) {
	case ledger.KindPlan, ledger.KindArtifact, ledger.KindDecision, ledger.KindAcceptance:
	default:
		fail("ledger add: --kind must be plan|artifact|decision|acceptance " +
			"(drift is recorded by the continuity guard; a refusal is never memory)")
	}
	if strings.TrimSpace(*summary) == "" {
		fail("ledger add: --summary is required")
	}

	app := newApp(ws)
	st, err := app.LedgerStore()
	if err != nil {
		fail("%v", err)
	}
	defer st.Close()

	entry := ledger.Entry{
		Kind:     *kind,
		Summary:  *summary,
		Task:     *task,
		Operator: *operator,
	}
	for _, p := range evidence {
		entry.Evidence = append(entry.Evidence, ledger.Evidence{Path: p})
	}
	added, err := st.Append(entry)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("added %s · %s · %d evidence\n", added.ID, added.Kind, len(added.Evidence))
	fmt.Println("· the ledger is context, never policy: it records accepted work, not permissions")
}

// cmdLedgerExport writes the ledger as JSONL, to stdout or to --file.
func cmdLedgerExport(ws string, args []string) {
	fs := flag.NewFlagSet("ledger export", flag.ExitOnError)
	file := fs.String("file", "", "write to this path instead of stdout")
	fs.Parse(args)

	app := newApp(ws)
	st, err := app.LedgerStore()
	if err != nil {
		fail("%v", err)
	}
	defer st.Close()

	if strings.TrimSpace(*file) == "" {
		if err := st.ExportJSONL(os.Stdout); err != nil {
			fail("%v", err)
		}
		return
	}
	f, err := os.Create(*file)
	if err != nil {
		fail("%v", err)
	}
	defer f.Close()
	if err := st.ExportJSONL(f); err != nil {
		fail("%v", err)
	}
	fmt.Printf("exported %s\n", *file)
}

// ledgerEntries opens the workspace ledger and returns its entries newest
// first, optionally filtered by kind and capped at limit (0 = every entry).
// List is the administrative view: superseded and drift rows are included.
func ledgerEntries(app *harness.App, kind string, limit int) ([]ledger.Entry, error) {
	st, err := app.LedgerStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()

	all, err := st.List(kind)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Entry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, all[i])
	}
	return out, nil
}

// ledgerLine is the one-line operator view of an entry. A drift row carries
// metadata only — matched ids, class, outcome — and is rendered as such, so no
// prose can reach the terminal through this path.
func ledgerLine(e ledger.Entry) string {
	if e.Kind == ledger.KindDrift {
		matched := "none"
		if len(e.MatchedIDs) > 0 {
			matched = strings.Join(e.MatchedIDs, ", ")
		}
		return fmt.Sprintf("drift · matched %s · class %s · outcome %s",
			matched, orDash(e.Class), orDash(e.Outcome))
	}
	s := strings.TrimSpace(e.Summary)
	if s == "" {
		s = strings.TrimSpace(e.Task)
	}
	if s == "" {
		s = "(no summary)"
	}
	s = flattenLine(s)
	if e.Status == ledger.StatusSuperseded {
		s += "  [superseded]"
	}
	if e.Operator == ledger.OperatorAttested {
		s += "  [attested]"
	}
	return s
}

// flattenLine collapses whitespace so one entry stays one line.
func flattenLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// idsLabel renders the ledger ids a block cites for an operator note.
func idsLabel(ids []string) string {
	if len(ids) == 0 {
		return "no matched ids"
	}
	return "matched " + strings.Join(ids, ", ")
}

// repeatFlag collects a repeatable string flag (--evidence a --evidence b).
type repeatFlag []string

func (r *repeatFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}
