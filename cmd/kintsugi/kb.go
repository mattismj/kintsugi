// `kintsugi kb` — the ATT&CK knowledge-base CLI (WP-1.5).
//
// The package command is not registered in main.go's switch yet: the Wave-1
// owner of cmd/kintsugi/main.go wires `case "kb": cmdKB(ws, rest)` plus one usage
// line, so this file only has to expose a shape that matches the rest of the CLI
// (cmdSkills / cmdModels): a ws-prefixed entry point, subcommand dispatch, flag
// parsing that tolerates flags after a positional, friendly errors, and fail()
// for the fatal ones.
//
// Subcommands:
//
//	kintsugi kb build <stix-file> <out-dir> [--license "CC BY 4.0"] [--retrieved YYYY-MM-DD]
//	kintsugi kb reindex <kb-dir>
//	kintsugi kb search <kb-dir> "<query>" [--top 8]
//	kintsugi kb list <kb-dir>
//
// All paths resolve against the workspace, so `kintsugi kb list knowledgebases/attack`
// does what it reads like from inside a project.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kintsugi/internal/kb"
)

// kbUsage is the text printed by `kintsugi kb` with no arguments and by
// `kintsugi kb help`.
const kbUsage = `kintsugi kb — ATT&CK knowledge base

  kintsugi kb build <stix-file> <out-dir>  build a KB from a MITRE CTI STIX bundle
      [--license "CC BY 4.0"]           license stamped on every chunk
      [--retrieved YYYY-MM-DD]          retrieval date (default: today)
  kintsugi kb reindex <kb-dir>             rebuild the derived BM25 index
  kintsugi kb search <kb-dir> "<query>"    rank chunks against a query
      [--top 8]                         number of hits to print
  kintsugi kb list <kb-dir>                base id/version/title and chunk count`

// cmdKB is the entry point for `kintsugi kb ...`.
func cmdKB(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi kb build|reindex|search|list (run `kintsugi kb help` for detail)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "build":
		cmdKBBuild(ws, rest)
	case "reindex":
		cmdKBReindex(ws, rest)
	case "search":
		cmdKBSearch(ws, rest)
	case "list":
		cmdKBList(ws, rest)
	case "help", "-h", "--help":
		fmt.Println(kbUsage)
	default:
		fail("kintsugi kb: unknown subcommand %q\nusage: kintsugi kb build|reindex|search|list (run `kintsugi kb help` for detail)", sub)
	}
}

// cmdKBBuild materializes a knowledge base from a MITRE CTI STIX bundle.
// Per-object problems are warnings printed after the summary; only an
// unreadable bundle or an unwritable out directory is fatal.
func cmdKBBuild(ws string, args []string) {
	license := kb.DefaultSTIXLicense
	retrieved := ""
	var pos []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--license" || a == "-license":
			if i+1 >= len(args) {
				fail("kb build: --license needs a value")
			}
			i++
			license = args[i]
		case strings.HasPrefix(a, "--license="):
			license = strings.TrimPrefix(a, "--license=")
		case a == "--retrieved" || a == "-retrieved":
			if i+1 >= len(args) {
				fail("kb build: --retrieved needs a value")
			}
			i++
			retrieved = args[i]
		case strings.HasPrefix(a, "--retrieved="):
			retrieved = strings.TrimPrefix(a, "--retrieved=")
		case a == "-h" || a == "--help":
			fmt.Println(kbUsage)
			return
		case strings.HasPrefix(a, "-") && a != "-":
			fail("kb build: unknown flag %q\nusage: kintsugi kb build <stix-file> <out-dir> [--license \"CC BY 4.0\"] [--retrieved YYYY-MM-DD]", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		fail("usage: kintsugi kb build <stix-file> <out-dir> [--license \"CC BY 4.0\"] [--retrieved YYYY-MM-DD]")
	}
	if len(pos) > 2 {
		fail("kb build: unexpected argument %q (quote a path with spaces)", pos[2])
	}
	if retrieved == "" {
		retrieved = time.Now().UTC().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", retrieved); err != nil {
		fail("kb build: --retrieved %q is not a date (want YYYY-MM-DD, e.g. 2026-10-21)", retrieved)
	}

	stixPath := kbPath(ws, pos[0])
	outDir := kbPath(ws, pos[1])

	f, err := os.Open(stixPath)
	if err != nil {
		fail("kb build: %v", err)
	}
	defer f.Close()

	written, warnings, err := kb.BuildFromSTIX(f, outDir, license, retrieved)
	if err != nil {
		fail("kb build: %v", err)
	}

	fmt.Printf("built %d chunk(s) → %s\n", written, outDir)
	fmt.Printf("  base   %s\n", filepath.Join(outDir, kb.MetaFile))
	fmt.Printf("  chunks %s\n", filepath.Join(outDir, kb.ChunksDir))
	fmt.Printf("  license %s · retrieved_at %s\n", license, retrieved)
	if written == 0 {
		fmt.Println("\nno techniques imported: does the bundle contain attack-pattern objects with a mitre-attack external_id?")
	}
	kbWarnings(warnings)
}

// cmdKBReindex loads the KB and rebuilds its derived index, reporting what was
// indexed and what the license policy kept out.
func cmdKBReindex(ws string, args []string) {
	dir, ok := kbOneDir(ws, args, "reindex")
	if !ok {
		return
	}
	k, err := kb.Load(dir)
	if err != nil {
		fail("kb reindex: %v", err)
	}
	ix, err := kb.OpenIndex(k)
	if err != nil {
		fail("kb reindex: %v", err)
	}
	defer ix.Close()

	n, err := ix.Reindex()
	if err != nil {
		fail("kb reindex: %v", err)
	}
	fmt.Printf("indexed %d of %d chunk(s) from %s\n", n, len(k.Chunks), k.Dir)
	fmt.Printf("  index %s\n", filepath.Join(k.Dir, kb.IndexFile))
	kbWarnings(k.Warnings)
	kbWarnings(ix.Warnings)
}

// cmdKBSearch ranks the KB's chunks against a free-text query and prints the
// score, the chunk identity, and the opening of the body — enough to judge a hit
// without dumping the chunk.
func cmdKBSearch(ws string, args []string) {
	top := 0
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--top" || a == "-top":
			if i+1 >= len(args) {
				fail("kb search: --top needs a value")
			}
			i++
			top = kbAtoi("--top", args[i])
		case strings.HasPrefix(a, "--top="):
			top = kbAtoi("--top", strings.TrimPrefix(a, "--top="))
		case a == "-h" || a == "--help":
			fmt.Println(kbUsage)
			return
		case strings.HasPrefix(a, "-") && a != "-":
			fail("kb search: unknown flag %q\nusage: kintsugi kb search <kb-dir> \"<query>\" [--top 8]", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		fail("usage: kintsugi kb search <kb-dir> \"<query>\" [--top 8]")
	}
	dir := kbPath(ws, pos[0])
	query := strings.TrimSpace(strings.Join(pos[1:], " "))

	k, err := kb.Load(dir)
	if err != nil {
		fail("kb search: %v", err)
	}
	if len(k.Chunks) == 0 {
		fail("kb search: %s has no chunks", k.Dir)
	}
	ix, err := kb.OpenIndex(k)
	if err != nil {
		fail("kb search: %v", err)
	}
	defer ix.Close()

	// Search over a stale or missing index yields nothing, which reads like "no
	// match" — so index first: the index is derived and cheap to rebuild.
	if _, err := ix.Reindex(); err != nil {
		fail("kb search: %v", err)
	}

	hits, err := ix.Search(query, top, nil)
	if err != nil {
		fail("kb search: %v", err)
	}
	if len(hits) == 0 {
		fmt.Printf("no chunk in %s matches %q\n", k.Dir, query)
		return
	}
	fmt.Printf("%s · %q · %d hit(s)\n\n", k.ID, query, len(hits))
	for _, h := range hits {
		fmt.Printf("%8.3f · %s · %s\n", h.Score, h.Chunk.ID, h.Chunk.Title)
		if s := kbSnippet(h.Chunk.Body, 200); s != "" {
			fmt.Printf("           %s\n", s)
		}
	}
	kbWarnings(k.Warnings)
}

// cmdKBList prints the base descriptor and the chunk count, plus whatever the
// loader had to skip — the fastest way to see whether a KB is clean.
func cmdKBList(ws string, args []string) {
	dir, ok := kbOneDir(ws, args, "list")
	if !ok {
		return
	}
	k, err := kb.Load(dir)
	if err != nil {
		fail("kb list: %v", err)
	}
	fmt.Printf("%s · %s · %s\n", k.ID, k.Version, k.Title)
	fmt.Printf("  source  %s\n", orDash(k.Source))
	fmt.Printf("  license %s\n", orDash(k.License))
	fmt.Printf("  chunks  %d (%d injectable)\n", len(k.Chunks), len(k.InjectionCandidates()))
	if k.Chunking.Strategy != "" {
		fields := ""
		if len(k.Chunking.Fields) > 0 {
			fields = " over " + strings.Join(k.Chunking.Fields, ",")
		}
		fmt.Printf("  chunking %s%s\n", k.Chunking.Strategy, fields)
	}
	if k.Index != "" {
		fmt.Printf("  index   %s (derived)\n", k.Index)
	}
	fmt.Printf("  dir     %s\n", k.Dir)
	if len(k.Warnings) > 0 {
		kbWarnings(k.Warnings)
	}
}

// kbOneDir parses a single-directory subcommand argument list.
func kbOneDir(ws string, args []string, sub string) (string, bool) {
	for _, a := range args {
		switch a {
		case "-h", "--help":
			fmt.Println(kbUsage)
			return "", false
		}
	}
	var pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			fail("kb %s: unknown flag %q\nusage: kintsugi kb %s <kb-dir>", sub, a, sub)
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 {
		fail("usage: kintsugi kb %s <kb-dir>", sub)
	}
	if len(pos) > 1 {
		fail("kb %s: unexpected argument %q (quote a path with spaces)", sub, pos[1])
	}
	return kbPath(ws, pos[0]), true
}

// kbPath resolves a KB path against the workspace. Absolute paths pass through.
func kbPath(ws, p string) string {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(ws, p)
}

// kbWarnings prints loader/build warnings, prefixed so they are not mistaken for
// findings of the command itself.
func kbWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Println("warn: " + w)
	}
}

// kbAtoi parses a positive integer flag value.
func kbAtoi(flagName, v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		fail("kb: %s %q is not a positive integer", flagName, v)
	}
	return n
}

// kbSnippet collapses whitespace and cuts s to at most n characters, adding an
// ellipsis when it had to cut.
func kbSnippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
