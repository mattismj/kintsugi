package main

// `kintsugi review [ref]`: read the change, ask the gate model what is wrong with
// it, print findings by severity and a ship/no-ship verdict (M10-E).
//
// The reviewer is read-only by construction: it is handed a diff, it returns
// findings, and it holds no tool access. `ref` selects what is reviewed — with
// no argument the working tree is judged, with one the diff against that ref
// is. Nothing here writes, stages or commits.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"kintsugi/internal/gate"
)

func cmdReview(ws string, args []string) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the typed review as JSON")
	ship := fs.Bool("ship-check", false, "exit non-zero when the verdict is no-ship")
	fs.Parse(args)
	ref := strings.TrimSpace(strings.Join(fs.Args(), " "))

	diff, kind, err := reviewDiff(ws, ref)
	if err != nil {
		fail("review: %v", err)
	}
	if strings.TrimSpace(diff) == "" {
		fmt.Println("nothing to review: the tree is clean")
		return
	}

	app := newApp(ws)
	reviewer, err := app.GateReviewer()
	if err != nil {
		fail("review: %v", err)
	}

	rev, err := reviewer.ReviewCode(context.Background(), gate.CodeReviewArtifact{
		Kind: kind, Content: diff, Ref: ref,
	})
	if err != nil {
		fail("review: %v", err)
	}

	if *asJSON {
		b, err := json.MarshalIndent(rev, "", "  ")
		if err != nil {
			fail("review: encode: %v", err)
		}
		fmt.Println(string(b))
	} else {
		fmt.Println(gate.FormatCodeReview(rev))
	}

	if *ship && rev.Verdict == "no-ship" {
		os.Exit(1)
	}
}

// reviewDiff returns the text to judge and what it came from. An empty ref means
// the working tree; otherwise the diff against the ref, which covers both a
// branch name and a commit.
func reviewDiff(ws, ref string) (diff, kind string, err error) {
	if ws == "" {
		return "", "", fmt.Errorf("no workspace")
	}
	if ref == "" {
		// Tracked changes plus what is already staged, so a half-committed tree
		// is judged as one change rather than hiding the staged half.
		d, err := runGit(ws, "diff", "HEAD")
		if err != nil {
			// No HEAD yet: the whole tree is new.
			if out, err2 := runGit(ws, "diff", "--cached"); err2 == nil {
				return out, gate.ArtifactDiff, nil
			}
			return "", "", err
		}
		return d, gate.ArtifactDiff, nil
	}
	d, err := runGit(ws, "diff", ref)
	if err != nil {
		return "", "", err
	}
	return d, gate.ArtifactDiff, nil
}

func runGit(ws string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = ws
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
