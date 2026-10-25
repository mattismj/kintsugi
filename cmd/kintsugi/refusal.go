package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"kintsugi/internal/continuity"
	"kintsugi/internal/intercept"
	"kintsugi/internal/session"
)

// cmdRefusal is the operator-facing surface for refusal interception.
//
// Two halves of the same idea live here:
//
//   - stats/trends read the interception telemetry (counters only; a refusal is
//     never memory, so the phrase that matched is dropped at the telemetry
//     boundary and never decoded here);
//   - list/add/test/disable/restore edit the lexicon itself — the regex
//     denylist the live scanner matches against.
//
// The editing half closes the gap that used to send an operator to a TOML file
// and a config knob: a refusal the lexicon missed was, in practice, a refusal
// the operator could not teach it about without reading source. Every command
// here works against the same file the loader reads, so a pattern added at the
// CLI is in force on the next turn with no restart.
func cmdRefusal(ws string, args []string) {
	sub := "stats"
	if len(args) > 0 {
		sub = args[0]
	}
	dir, err := session.Dir()
	if err != nil {
		fail("%v", err)
	}
	switch sub {
	case "stats":
		stats, err := session.RefusalStats(dir)
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(session.FormatRefusalStats(stats))
	case "trends":
		// Continuity trends (M8.5.7): refusal interceptions split by whether the
		// session was fresh or resumed, plus the non-recallable drift events. Rows
		// are (model, persona, class, outcome) and integers — there is no phrase
		// and no transcript anywhere in this table.
		rows, err := continuity.Trends(dir)
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(continuity.FormatTrends(rows))
	case "list":
		cmdRefusalList(ws, args[1:])
	case "add":
		cmdRefusalAdd(ws, args[1:])
	case "test":
		cmdRefusalTest(ws, args[1:])
	case "disable":
		cmdRefusalDisable(ws, args[1:])
	case "restore":
		cmdRefusalRestore(ws, args[1:])
	case "export":
		cmdRefusalExport(ws, args[1:])
	case "corpus":
		cmdRefusalCorpus(ws, args[1:])
	default:
		fail("usage: kintsugi refusal stats|trends|list|add|test|disable|restore|export|corpus")
	}
}

// cmdRefusalList prints the effective lexicon: every class, every pattern in
// it, where each pattern came from (default or operator), and how many
// interceptions that class has logged in telemetry. The hit count is the
// operator's coverage signal: a class with patterns but zero hits is either
// perfect (the model never tried that move) or a dead list (the patterns no
// longer match what the model actually says); the operator has to decide which.
func cmdRefusalList(ws string, args []string) {
	if len(args) > 0 {
		fail("usage: kintsugi refusal list")
	}
	app := newApp(ws)
	lx, err := app.RefusalLexicon()
	if err != nil {
		fail("%v", err)
	}
	path := app.EffectiveLexiconPath()
	overrides, _ := intercept.OverrideClasses(path)
	classHits := refusalClassCounts()

	byClass := map[string][]intercept.Pattern{}
	for _, p := range lx.Patterns {
		byClass[p.Class] = append(byClass[p.Class], p)
	}
	classes := lx.Classes()

	fmt.Printf("lexicon  %d class(es) · %d pattern(s)\n", len(classes), len(lx.Patterns))
	fmt.Printf("file     %s\n", refusalPathStatus(path, overrides))
	fmt.Println("· classes present in the file replace the built-in class; an empty list disables it")
	fmt.Println()

	for _, c := range classes {
		pats := byClass[c]
		origin := "default"
		if _, ok := overrides[c]; ok {
			origin = "operator"
		}
		fmt.Printf("%s  %d pattern(s) · %s · %d hit(s)\n", c, len(pats), origin, classHits[c])
		for _, p := range pats {
			mark := "  "
			if p.Origin == "operator" {
				mark = "+ "
			}
			fmt.Printf("%s%s\n", mark, p.Src)
		}
		fmt.Println()
	}

	fmt.Println("rephrasings  see `kintsugi persona triggers` (the trigger map turns a refusal's term into an honest rephrasing)")
}

// cmdRefusalAdd appends a pattern to a class in the override file. The regex is
// compiled before it is written: a pattern that does not parse is refused
// outright, so the file never holds a pattern that silently no-loads at
// session start. The (?i) prefix the loader applies is added here for the test
// compile only; the file stores the operator's own string.
//
// Adding to a class that exists in the override file extends that class's
// operator list. Adding to a class the file does not name creates the class
// from scratch — the built-in patterns for that class stop applying the moment
// the operator names it, which is the override contract.
func cmdRefusalAdd(ws string, args []string) {
	if len(args) < 2 {
		fail("usage: kintsugi refusal add <class> <regex>")
	}
	class := strings.TrimSpace(args[0])
	pattern := strings.Join(args[1:], " ")
	pattern = strings.Trim(pattern, "\"")
	if class == "" || pattern == "" {
		fail("usage: kintsugi refusal add <class> <regex>")
	}
	// Validate: the loader prepends (?i) when the source does not start with a
	// (?...) group, so compile the exact form the loader will use.
	compileSrc := pattern
	if !strings.HasPrefix(compileSrc, "(?") {
		compileSrc = "(?i)" + compileSrc
	}
	if _, err := regexp.Compile(compileSrc); err != nil {
		fail("pattern does not compile: %v", err)
	}

	app := newApp(ws)
	path := app.EffectiveLexiconPath()
	overrides, err := intercept.OverrideClasses(path)
	if err != nil {
		fail("%v", err)
	}
	overrides[class] = append(overrides[class], pattern)
	if err := intercept.WriteOverride(path, overrides); err != nil {
		fail("%v", err)
	}
	fmt.Printf("added to %s in %s\n  %s\n", class, path, pattern)
	fmt.Println("· the class now replaces the built-in wholesale; re-run `kintsugi refusal list` to confirm")
	fmt.Println("· the next turn picks it up — the loader reads this file at session start")
}

// cmdRefusalTest runs a sample of assistant prose through the scanner the way
// the live harness does: fenced code, inline code, blockquotes and
// double-quoted spans are scrubbed first, so quoting a refusal does not count
// as asserting it. Every pattern that matches is printed with its class, the
// matched phrase, the pattern source and whether it is a default or an
// operator pattern.
//
// A sample that matches nothing is the coverage gap: the operator's lexicon
// would have let that refusal through. That is the most useful output this
// command produces, so it is reported plainly rather than as an error.
func cmdRefusalTest(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi refusal test \"<sample assistant text>\"")
	}
	sample := strings.Join(args, " ")
	sample = strings.Trim(sample, "\"")

	app := newApp(ws)
	lx, err := app.RefusalLexicon()
	if err != nil {
		fail("%v", err)
	}
	hits := lx.ScanAll(sample)
	if len(hits) == 0 {
		fmt.Println("no patterns matched")
		fmt.Println("· the lexicon would not have intercepted this — if it is a refusal, add a pattern with `kintsugi refusal add <class> <regex>`")
		return
	}
	fmt.Printf("%d pattern(s) matched:\n\n", len(hits))
	for _, h := range hits {
		origin := h.Origin
		if origin == "" {
			origin = "default"
		}
		fmt.Printf("class   %s\nphrase  %s\npattern %s\norigin  %s\n\n",
			h.Class, h.Phrase, h.Pattern, origin)
	}
	fmt.Println("· the live scanner stops at the first match and aborts the turn; this lists every match so you can see coverage")
}

// cmdRefusalDisable empties a class in the override file, which is how a class
// is turned off without removing the operator's patterns: an empty list is the
// documented "disable" form. The built-in class still applies unless the
// operator's empty list replaces it — and it does, because a class present in
// the file replaces the built-in class wholesale.
func cmdRefusalDisable(ws string, args []string) {
	if len(args) != 1 {
		fail("usage: kintsugi refusal disable <class>")
	}
	class := strings.TrimSpace(args[0])
	if class == "" {
		fail("usage: kintsugi refusal disable <class>")
	}
	app := newApp(ws)
	path := app.EffectiveLexiconPath()
	overrides, err := intercept.OverrideClasses(path)
	if err != nil {
		fail("%v", err)
	}
	overrides[class] = []string{}
	if err := intercept.WriteOverride(path, overrides); err != nil {
		fail("%v", err)
	}
	fmt.Printf("disabled %s in %s\n", class, path)
	fmt.Println("· an empty list replaces the built-in class; restore it with `kintsugi refusal restore <class>`")
}

// cmdRefusalRestore removes a class from the override file, so the built-in
// class takes over again. Removing the last class leaves the file's [classes]
// block empty but valid; the file itself is not deleted, so the operator's
// other overrides (if any) survive.
func cmdRefusalRestore(ws string, args []string) {
	if len(args) != 1 {
		fail("usage: kintsugi refusal restore <class>")
	}
	class := strings.TrimSpace(args[0])
	if class == "" {
		fail("usage: kintsugi refusal restore <class>")
	}
	app := newApp(ws)
	path := app.EffectiveLexiconPath()
	overrides, err := intercept.OverrideClasses(path)
	if err != nil {
		fail("%v", err)
	}
	if _, ok := overrides[class]; !ok {
		fmt.Printf("· %s is not in the override file — the built-in class already applies\n", class)
		return
	}
	delete(overrides, class)
	if err := intercept.WriteOverride(path, overrides); err != nil {
		fail("%v", err)
	}
	fmt.Printf("restored %s — the built-in class applies again\n", class)
	fmt.Printf("file  %s\n", path)
}

// refusalClassCounts reads the refusal telemetry and rolls it up to a per-class
// counter. Telemetry carries no phrase; this is the integer the list command
// shows next to a class so an operator can tell a class that fires often from
// one that never has.
func refusalClassCounts() map[string]int {
	out := map[string]int{}
	dir, err := session.Dir()
	if err != nil {
		return out
	}
	stats, err := session.RefusalStats(dir)
	if err != nil {
		return out
	}
	for _, s := range stats {
		if c := strings.TrimSpace(s.Class); c != "" {
			out[c] += s.Count
		}
	}
	return out
}

// refusalPathStatus renders the override file line: where it lives and whether
// it holds anything, so an operator running `list` knows where `add` will
// write.
func refusalPathStatus(path string, overrides map[string][]string) string {
	if len(overrides) == 0 {
		if _, err := os.Stat(path); err == nil {
			return path + " (empty [classes] block)"
		}
		return path + " (will be created on first `add`)"
	}
	return path
}
