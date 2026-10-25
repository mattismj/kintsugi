package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"kintsugi/internal/rules"
)

func cmdRules(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi rules scan|import [--apply] [--json]")
	}
	mode := args[0]
	fs := flag.NewFlagSet("rules "+mode, flag.ExitOnError)
	apply := fs.Bool("apply", false, "write .kintsugi/rules/inherited.md and the manifest")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Parse(args[1:])

	var (
		rep rules.Report
		err error
	)
	switch mode {
	case "scan":
		rep, err = rules.Scan(ws)
	case "import":
		rep, err = rules.Import(ws, *apply)
	default:
		fail("usage: kintsugi rules scan|import [--apply] [--json]")
	}
	if err != nil {
		fail("rules %s: %v", mode, err)
	}

	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	if len(rep.Sources) == 0 {
		fmt.Println("no inherited rule sources found")
		return
	}
	fmt.Printf("found %d source(s):\n", len(rep.Sources))
	for _, s := range rep.Sources {
		fmt.Printf("  %-10s %-40s %6d bytes  sha256:%s\n", s.Kind, s.Path, s.Bytes, s.SHA256[:12])
	}
	for _, w := range rep.Warnings {
		fmt.Println("  ! " + w)
	}
	if mode == "scan" {
		fmt.Println("\nreport only; run `kintsugi rules import --apply` to add these as a behavior layer")
		return
	}
	if rep.Applied {
		fmt.Printf("\napplied → %s\nmanifest → %s\n", rep.Output, rules.ManifestRel)
	} else {
		fmt.Println("\nnot applied (pass --apply to write the inherited layer)")
	}
}
