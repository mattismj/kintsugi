package main

// `kintsugi commit`: split a working tree into atomic commits (M10-E).
//
// The tool reads what changed, groups unrelated work, orders the groups so
// source lands before the tests and docs that describe it, and writes a message
// per group from the diff. Dry-run is the default and the only mode that needs
// no trust: nothing is staged, nothing is committed, git is not written to.
//
// `--apply` is the deliberate step. It asks for confirmation first and never
// pushes; publishing stays the operator's own decision.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"kintsugi/internal/tools"
)

func cmdCommit(ws string, args []string) {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	apply := fs.Bool("apply", false, "actually run git commit per group (asks first)")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	yes := fs.Bool("yes", false, "skip the confirmation prompt (only with --apply)")
	fs.Parse(args)

	plan, err := tools.PlanCommits(tools.CommitOptions{Workspace: ws})
	if err != nil {
		fail("commit: %v", err)
	}
	if len(plan.Groups) == 0 {
		fmt.Println("nothing to commit")
		return
	}

	if *asJSON {
		b, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			fail("commit: encode plan: %v", err)
		}
		fmt.Println(string(b))
	} else {
		fmt.Print(plan.Format())
	}

	if !*apply {
		fmt.Println("\ndry run: nothing was staged or committed. Re-run with --apply.")
		return
	}

	err = tools.ApplyCommit(plan, tools.CommitOptions{
		Workspace: ws,
		Apply:     true,
		Confirm:   commitConfirm(ws, plan, *yes),
	})
	if err != nil {
		fail("commit: %v", err)
	}
	fmt.Printf("committed %d group(s). Nothing was pushed.\n", len(plan.Groups))
}

// commitConfirm returns the confirmation gate for --apply. A --yes skips the
// prompt; otherwise the operator reads the plan and types the answer, and
// anything but an explicit affirmative aborts.
func commitConfirm(ws string, plan tools.CommitPlan, skip bool) func() bool {
	if skip {
		return func() bool { return true }
	}
	return func() bool {
		fmt.Printf("\nAbout to create %d commit(s) in %s.\n", len(plan.Groups), ws)
		fmt.Print("Type 'commit' to go ahead, anything else to abort: ")
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(strings.ToLower(line)) == "commit"
	}
}
