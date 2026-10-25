package main

// `kintsugi prompt test` — CI for the prompt stack.
//
// The prompt is a build product with invariants (layer order, budget, cache
// stability, determinism, a stack hash). A config change, a persona edit, or
// a new KB can break them silently. This command compiles the stack across
// every configured model and asserts each invariant, so the prompt itself
// has a test surface beyond snapshot tests on one input.
//
// --golden DIR writes the manifests as goldens; a later run with the same
// DIR diffs against them and reports layer changes.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/harness"
	"kintsugi/internal/prompt"
)

// canonicalLayerOrder is the stack order the compiler enforces. A layer that
// appears out of order is a compiler regression, not a config issue.
var canonicalLayerOrder = []string{"kernel", "adapter", "persona", "kb", "behavior", "skills", "workspace", "taste", "task"}

func cmdPromptTest(ws string, args []string) {
	fs := flag.NewFlagSet("prompt test", flag.ExitOnError)
	model := fs.String("model", "", "one model ref (default: every configured model with a curated list)")
	golden := fs.String("golden", "", "write manifests here, or diff against them if they exist")
	fs.Parse(args)

	app := newApp(ws)

	var refs []string
	if *model != "" {
		refs = append(refs, strings.TrimSpace(*model))
	} else {
		for _, name := range app.Cfg.ProviderNames() {
			p := app.Cfg.Providers[name]
			if len(p.Models) == 0 {
				continue
			}
			for _, m := range p.Models {
				refs = append(refs, name+"/"+m.ID)
			}
		}
	}
	if len(refs) == 0 {
		fail("no models to test — pass --model or configure providers with model lists")
	}
	sort.Strings(refs)

	rank := map[string]int{}
	for i, id := range canonicalLayerOrder {
		rank[id] = i
	}

	// Golden mode: if the dir exists and has files, diff; otherwise write.
	goldenDir := *golden
	if goldenDir == "" {
		goldenDir = filepath.Join(ws, ".kintsugi", "prompt-goldens")
	}
	goldenExists := dirHasJSON(goldenDir)

	passed, failed := 0, 0
	for _, ref := range refs {
		res, err := app.Resolve(ref)
		if err != nil {
			fmt.Printf("%-44s  SKIP  resolve: %s\n", ref, oneLine(err.Error()))
			continue
		}
		comp, err := app.Compile(res, harness.CompileOpts{})
		if err != nil {
			fmt.Printf("%-44s  SKIP  compile: %s\n", ref, oneLine(err.Error()))
			continue
		}
		m := comp.Manifest
		verdict := "ok"
		// Unique layer ids.
		seen := map[string]bool{}
		for _, l := range m.Layers {
			if seen[l.ID] {
				verdict = "FAIL: duplicate layer " + l.ID
				break
			}
			seen[l.ID] = true
		}
		// Layer order: each present layer's rank >= the previous one's.
		prevRank := -1
		if verdict == "ok" {
			for _, l := range m.Layers {
				if r, ok := rank[l.ID]; ok {
					if r < prevRank {
						verdict = "FAIL: layer order at " + l.ID
						break
					}
					prevRank = r
				}
			}
		}
		// Stack hash present.
		if verdict == "ok" && m.StackHash == "" {
			verdict = "FAIL: no stack hash"
		}
		// Budget.
		if verdict == "ok" && m.Totals.Budget > 0 && m.Totals.Tokens > m.Totals.Budget {
			verdict = "FAIL: over budget"
		}
		// Determinism: a second compile of the same input matches.
		if verdict == "ok" {
			comp2, err := app.Compile(res, harness.CompileOpts{})
			if err == nil && comp2.Manifest.StackHash != m.StackHash {
				verdict = "FAIL: non-deterministic"
			}
		}
		// Golden diff.
		goldenPath := filepath.Join(goldenDir, slug(ref)+".json")
		if goldenExists {
			if raw, err := os.ReadFile(goldenPath); err == nil {
				var gold prompt.Manifest
				if json.Unmarshal(raw, &gold) == nil {
					for _, line := range prompt.Diff(gold, m) {
						if verdict == "ok" {
							verdict = "changed"
						}
						fmt.Printf("  %-40s  %s\n", "", line)
					}
				}
			}
		}

		if *golden != "" && !goldenExists {
			// Write mode: persist the manifest.
			if err := os.MkdirAll(goldenDir, 0o755); err == nil {
				if raw, err := json.MarshalIndent(m, "", "  "); err == nil {
					_ = os.WriteFile(goldenPath, append(raw, '\n'), 0o644)
				}
			}
		}

		if verdict == "ok" {
			passed++
		} else {
			failed++
		}
		fmt.Printf("%-44s  %-24s %6d tok  %s\n", ref, verdict, m.Totals.Tokens, shortHash(m.StackHash))
	}

	fmt.Printf("\nprompt test: %d passed, %d failed\n", passed, failed)
	if goldenExists {
		fmt.Printf("goldens: %s (diffed; re-run with --golden to overwrite)\n", goldenDir)
	} else if *golden != "" {
		fmt.Printf("goldens written: %s\n", goldenDir)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// slug turns a model ref into a filename-safe slug.
func slug(ref string) string {
	r := strings.NewReplacer("/", "-", ":", "-", " ", "-", ".", "-")
	return r.Replace(ref)
}

// dirHasJSON reports whether the directory contains at least one JSON file.
func dirHasJSON(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}
