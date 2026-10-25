package main

// `kintsugi taste` is the operator's view of the local taste store (M10-B,
// DESIGN §16.15).
//
// Taste is the record of the operator's own micro-decisions: every approval
// granted, every approval denied, every file the operator re-edited after the
// agent wrote it. It is Markdown in two append-only files — this workspace's
// and the operator's global one — and it is context, never policy: the compiled
// behavior layer may append a bounded rendering of it as prompt text, and
// nothing about it can widen a permission.
//
//	kintsugi taste list            the bullets, grouped project / global
//	kintsugi taste open            the store paths and their contents
//	kintsugi taste lint            reject bullets without a date or a kind
//	kintsugi taste block           the ~300-token block a compile would inject
//	kintsugi taste add <kind> …    append a bullet by hand (--supersedes <date>)

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"kintsugi/internal/taste"
)

// errTasteLint is what `kintsugi taste lint` returns when it found grammar
// errors: the findings are already printed, so the caller only needs the
// non-zero exit.
var errTasteLint = errors.New("taste lint found errors")

func cmdTaste(ws string, args []string) {
	err := tasteCommand(ws, args, os.Stdout)
	switch {
	case err == nil:
	case errors.Is(err, errTasteLint):
		os.Exit(1)
	default:
		fail("%v", err)
	}
}

// tasteCommand is the testable body of `kintsugi taste`.
func tasteCommand(ws string, args []string, out io.Writer) error {
	sub := "list"
	rest := args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	store := taste.StoreFor(ws)

	switch sub {
	case "list":
		return tasteList(store, rest, out)
	case "open":
		return tasteOpen(store, rest, out)
	case "lint":
		return tasteLint(store, out)
	case "block":
		return tasteBlock(store, rest, out)
	case "add":
		return tasteAdd(store, rest, out)
	case "help", "-h", "--help":
		fmt.Fprint(out, tasteUsageText())
		return nil
	default:
		fmt.Fprint(out, tasteUsageText())
		return fmt.Errorf("unknown taste subcommand %q", sub)
	}
}

func tasteUsageText() string {
	return `kintsugi taste — local operator taste (M10-B)

  kintsugi taste list                 bullets, grouped project / global (--kind, --json)
  kintsugi taste open                 the store paths and their contents (--global)
  kintsugi taste lint                 reject bullets without a date or a kind
  kintsugi taste block                the bounded block a compile would inject
  kintsugi taste add <kind> <subject> "<note>"
                                   append a bullet by hand
                                   flags: --global · --supersedes <YYYY-MM-DD> · --date <YYYY-MM-DD>

  kind is accept | reject | edit; subject is a tool name or a file class.
  Taste is prompt text, never policy: it grants no permission and widens no
  origin. The files are append-only and 0600 — a changed mind is a new bullet
  that names the old date, and superseded bullets stay in the file as history.
`
}

// tasteList prints the bullets grouped by store, oldest first, with the
// superseded ones marked so the history is visible without being mistaken for
// a live preference.
func tasteList(store *taste.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("taste list", flag.ContinueOnError)
	fs.SetOutput(out)
	kind := fs.String("kind", "", "accept|reject|edit")
	asJSON := fs.Bool("json", false, "print the bullets as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	entries, err := store.List(taste.OriginAny)
	if err != nil {
		return err
	}
	if want := strings.ToLower(strings.TrimSpace(*kind)); want != "" {
		filtered := entries[:0:0]
		for _, e := range entries {
			if e.Kind == want {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if *asJSON {
		return printTasteJSON(out, entries)
	}
	if !store.Enabled() {
		fmt.Fprintln(out, "· taste is disabled ([taste] enabled = false): nothing new is recorded or injected")
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "no taste bullets yet")
		fmt.Fprintf(out, "· project store %s\n", orDash(store.Path(taste.OriginProject)))
		fmt.Fprintf(out, "· global store  %s\n", orDash(store.Path(taste.OriginGlobal)))
		return nil
	}
	for _, origin := range []taste.Origin{taste.OriginProject, taste.OriginGlobal} {
		group := entriesOfOrigin(entries, origin)
		fmt.Fprintf(out, "\n%s · %s\n", originLabel(origin), orDash(store.Path(origin)))
		if len(group) == 0 {
			fmt.Fprintln(out, "  (no bullets)")
			continue
		}
		fmt.Fprintf(out, "  %-10s  %-7s  %-16s  %s\n", "DATE", "KIND", "SUBJECT", "NOTE")
		for _, e := range group {
			fmt.Fprintf(out, "  %-10s  %-7s  %-16s  %s%s\n",
				e.Date, e.Kind, fit(e.Subject, 16), tasteNote(e), supersededMark(e))
		}
	}
	fmt.Fprintf(out, "\n· %d bullet(s) · append-only; supersede with `kintsugi taste add … --supersedes <date>`\n", len(entries))
	fmt.Fprintln(out, "· taste is context, never policy: it grants no permission and widens no scope")
	return nil
}

// tasteOpen prints the store paths and their contents. It is the escape hatch
// for an operator who wants to read or hand-edit the file: nothing here
// rewrites it.
func tasteOpen(store *taste.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("taste open", flag.ContinueOnError)
	fs.SetOutput(out)
	global := fs.Bool("global", false, "the operator-global store only")
	project := fs.Bool("project", false, "this workspace's store only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	scopes := []taste.Origin{taste.OriginProject, taste.OriginGlobal}
	switch {
	case *global && !*project:
		scopes = []taste.Origin{taste.OriginGlobal}
	case *project && !*global:
		scopes = []taste.Origin{taste.OriginProject}
	}

	for _, origin := range scopes {
		path := store.Path(origin)
		fmt.Fprintf(out, "%s · %s\n", originLabel(origin), orDash(path))
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintln(out, "  (no store yet — it is created on the first recorded signal)")
				continue
			}
			return err
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, strings.TrimRight(string(b), "\n"))
		fmt.Fprintln(out)
	}
	return nil
}

// tasteLint is the grammar check. Errors exit non-zero; warnings do not.
func tasteLint(store *taste.Store, out io.Writer) error {
	findings, err := store.Lint()
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Fprintln(out, "taste lint: clean")
		fmt.Fprintf(out, "· project store %s\n", orDash(store.Path(taste.OriginProject)))
		fmt.Fprintf(out, "· global store  %s\n", orDash(store.Path(taste.OriginGlobal)))
		return nil
	}
	errs := taste.Errors(findings)
	for _, f := range findings {
		fmt.Fprintln(out, f.String())
	}
	fmt.Fprintf(out, "\n%d finding(s): %d error(s), %d warning(s)\n",
		len(findings), len(errs), len(findings)-len(errs))
	fmt.Fprintln(out, "· taste is append-only: fix a bullet by adding one that supersedes it, never by rewriting the file")
	if len(errs) > 0 {
		return errTasteLint
	}
	return nil
}

// tasteBlock prints exactly what a compile would append to the behavior layer,
// plus its token estimate. It is the operator's view of requirement 4: the
// injection is bounded, labelled, and empty when there is nothing to say.
func tasteBlock(store *taste.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("taste block", flag.ContinueOnError)
	fs.SetOutput(out)
	origin := fs.String("origin", "", "project|global (default: both)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	want := taste.OriginAny
	switch strings.ToLower(strings.TrimSpace(*origin)) {
	case "", "all", "both":
	case "project":
		want = taste.OriginProject
	case "global":
		want = taste.OriginGlobal
	default:
		return fmt.Errorf("taste block: --origin must be project|global, got %q", *origin)
	}

	if !store.Enabled() {
		fmt.Fprintln(out, "taste is disabled ([taste] enabled = false): no block is injected")
		return nil
	}
	block, ok := store.BlockOrigin(want)
	if !ok {
		fmt.Fprintln(out, "no taste to inject (no bullets, or every bullet is superseded)")
		return nil
	}
	fmt.Fprintln(out, block)
	cfg := store.Config()
	fmt.Fprintf(out, "\n· label %q · %d/%d tokens · budget %d (taste.budget_tokens) · max %d bullets (taste.max_entries)\n",
		taste.Label, taste.Tokens(block), cfg.Budget(), cfg.Budget(), cfg.Entries())
	fmt.Fprintln(out, "· the compiled behavior layer appends this when it is non-empty")
	return nil
}

// tasteAdd appends an operator-authored bullet. It is the manual half of the
// store: the signals API writes the automatic ones, and this is how an operator
// records a preference the harness could not infer — or supersedes one.
func tasteAdd(store *taste.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("taste add", flag.ContinueOnError)
	fs.SetOutput(out)
	global := fs.Bool("global", false, "write to the operator-global store")
	supersedes := fs.String("supersedes", "", "the date of the bullet this one replaces (YYYY-MM-DD)")
	date := fs.String("date", "", "the bullet's date (default: today)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: kintsugi taste add <accept|reject|edit> <subject> \"<note>\" [--global] [--supersedes <date>]")
	}
	kind, subject := rest[0], rest[1]
	note := strings.TrimSpace(strings.Join(rest[2:], " "))

	origin := taste.OriginProject
	if *global {
		origin = taste.OriginGlobal
	}
	e := taste.Entry{Kind: kind, Subject: subject, Note: note, Date: *date, Supersedes: *supersedes}

	var (
		added taste.Entry
		err   error
	)
	if sup := strings.TrimSpace(*supersedes); sup != "" {
		added, err = store.Supersede(origin, sup, kind, subject, note)
	} else {
		added, err = store.RecordEntry(origin, e)
	}
	if err != nil {
		return err
	}
	if added.Kind == "" {
		fmt.Fprintln(out, "taste is disabled ([taste] enabled = false): nothing was written")
		return nil
	}
	fmt.Fprintf(out, "added %s\n", added)
	fmt.Fprintf(out, "· %s · %s\n", originLabel(added.Origin), orDash(added.Path))
	fmt.Fprintln(out, "· taste is context, never policy: this bullet is prompt text, not a permission")
	return nil
}

// printTasteJSON writes the bullets as a JSON array (never null).
func printTasteJSON(out io.Writer, entries []taste.Entry) error {
	if entries == nil {
		entries = []taste.Entry{}
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(b))
	return nil
}

func entriesOfOrigin(entries []taste.Entry, origin taste.Origin) []taste.Entry {
	var out []taste.Entry
	for _, e := range entries {
		if e.Origin == origin {
			out = append(out, e)
		}
	}
	return out
}

func originLabel(origin taste.Origin) string {
	if origin == taste.OriginGlobal {
		return "global"
	}
	return "project"
}

// supersededMark flags a bullet a later one named. It stays in the file and in
// `list`, and never reaches the injected block.
func supersededMark(e taste.Entry) string {
	if e.Superseded {
		return "  [superseded]"
	}
	if e.Supersedes != "" {
		return "  [supersedes " + e.Supersedes + "]"
	}
	return ""
}

// tasteNote is the note column: one line, truncated for the table.
func tasteNote(e taste.Entry) string {
	n := strings.Join(strings.Fields(e.Note), " ")
	if n == "" {
		return "—"
	}
	return fit(n, 60)
}
