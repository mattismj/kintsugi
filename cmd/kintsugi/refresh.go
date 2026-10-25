package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/harness"
)

// cmdContextRefresh implements `kintsugi context refresh [--apply]`: it rebuilds
// the Current state section of CONTEXT.md from deterministic workspace signals
// (build status, git state, recently touched files) and — only when the
// operator asks for it, or when the config already auto-accepts the state
// class — writes the new body back. Authorization, scope and every other
// section are never touched.
func cmdContextRefresh(ws string, args []string) {
	fs := flag.NewFlagSet("context refresh", flag.ExitOnError)
	apply := fs.Bool("apply", false, "write the refreshed Current state section")
	fs.Parse(args)

	app, err := harness.New(ws)
	if err != nil {
		fail("%v", err)
	}
	if p := app.Cfg.Context.Path; p != "" && p != fctx.StateFileName {
		fmt.Fprintf(os.Stderr, "warn: context refresh rewrites %s; configured context.path is %q\n", fctx.StateFileName, p)
	}

	res, err := fctx.Refresh(app.Workspace)
	if err != nil {
		fail("%v", err)
	}
	path := fctx.ContextFile(app.Workspace)
	fmt.Printf("context refresh · state · %s\n", path)

	if !res.Changed {
		fmt.Println("Current state is already current — nothing to do")
		return
	}
	fmt.Println()
	printStateDiff(res.Old, res.New)
	fmt.Println()

	if *apply || stateAutoAccepted(app.Cfg.Context.AutoAcceptClasses) {
		if err := fctx.ApplyRefresh(app.Workspace, res); err != nil {
			fail("%v", err)
		}
		reason := "--apply"
		if !*apply {
			reason = "context.auto_accept_classes"
		}
		fmt.Printf("wrote %s (%s)\n", path, reason)
		return
	}
	fmt.Println(`dry run — re-run with --apply to write, or add "state" to context.auto_accept_classes`)
}

func stateAutoAccepted(classes []string) bool {
	for _, c := range classes {
		if strings.EqualFold(strings.TrimSpace(c), fctx.ClassState) {
			return true
		}
	}
	return false
}

// printStateDiff renders the old and new state bodies line by line.
func printStateDiff(oldBody, newBody string) {
	a, b := bodyLines(oldBody), bodyLines(newBody)

	// Longest-common-subsequence table, so an unchanged bullet stays put and
	// only real edits are marked.
	table := make([][]int, len(a)+1)
	for i := range table {
		table[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				table[i][j] = table[i+1][j+1] + 1
			case table[i+1][j] >= table[i][j+1]:
				table[i][j] = table[i+1][j]
			default:
				table[i][j] = table[i][j+1]
			}
		}
	}

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			fmt.Printf("  %s\n", a[i])
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			fmt.Printf("  - %s\n", a[i])
			i++
		default:
			fmt.Printf("  + %s\n", b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		fmt.Printf("  - %s\n", a[i])
	}
	for ; j < len(b); j++ {
		fmt.Printf("  + %s\n", b[j])
	}
}

func bodyLines(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}
