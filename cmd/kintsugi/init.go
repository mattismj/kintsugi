package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kintsugi/internal/initgen"
	"kintsugi/internal/rules"
)

// cmdInit scaffolds a workspace: a CONTEXT.md draft (DESIGN §4.2) and a minimal
// kintsugi.toml, both from what the working tree already says about itself.
// It then reports what the rules scanner found — report only; importing stays
// an explicit `kintsugi rules import --apply`.
func cmdInit(ws string, args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing CONTEXT.md / kintsugi.toml")
	fs.Parse(args)

	abs, err := filepath.Abs(ws)
	if err != nil {
		fail("init: %v", err)
	}
	name := filepath.Base(abs)

	d, err := initgen.DraftProject(abs, name, os.Getenv)
	if err != nil {
		fail("init: %v", err)
	}

	fmt.Printf("kintsugi init · %s\n\n", abs)
	if len(d.Stack.Files) > 0 {
		fmt.Printf("stack    %-24s %s\n", d.Stack.Name, strings.Join(d.Stack.Files, ", "))
	} else {
		fmt.Printf("stack    %-24s (no manifest detected)\n", d.Stack.Name)
	}

	written, skipped, err := initgen.Write(abs, d, *force)
	if err != nil {
		fail("init: %v", err)
	}
	if len(written) > 0 {
		fmt.Printf("wrote    %s\n", strings.Join(written, ", "))
	} else {
		fmt.Println("wrote    (nothing)")
	}
	if len(skipped) > 0 {
		fmt.Printf("skipped  %s — already present, left untouched (pass --force to overwrite)\n", strings.Join(skipped, ", "))
	}

	if d.Persona != "" {
		fmt.Printf("persona  %s (suggested from core.default_model)\n", d.Persona)
	} else {
		fmt.Println("persona  — (no family in core.default_model; the pack will fall back)")
	}

	fmt.Println()
	fmt.Println("Next: complete the Authorization & scope section yourself — the harness never writes it,")
	fmt.Println("      and it is injected on every turn. Review the rest with `kintsugi context show`.")

	printRulesReport(abs)
}

// printRulesReport prints the rules scanner's finding. It never imports:
// `kintsugi rules import --apply` stays an explicit operator decision.
func printRulesReport(ws string) {
	rep, err := rules.Scan(ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: rules scan: %v\n", err)
		return
	}
	if len(rep.Sources) == 0 {
		fmt.Println("\nrules    no inherited rule sources found")
		return
	}
	fmt.Printf("\nrules    %d source(s) found (report only):\n", len(rep.Sources))
	for _, s := range rep.Sources {
		fmt.Printf("  %-10s %-44s %6d bytes\n", s.Kind, s.Path, s.Bytes)
	}
	for _, w := range rep.Warnings {
		fmt.Println("  ! " + w)
	}
	fmt.Println("  import with: kintsugi rules import --apply")
}
