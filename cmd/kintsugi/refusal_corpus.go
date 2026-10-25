package main

// `kintsugi refusal corpus` — the operator's own refusal corpus, and the
// loop that turns it into lexicon patterns.
//
// The corpus is local and personal: it lives in .kintsugi/refusal-corpus/,
// is never uploaded, and exists so the operator's own refusal data improves
// their own detector. Texts come from `kintsugi refusal export` reports or
// pasted directly. Nothing here is community infrastructure.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/intercept"
)

// corpusRecord is one stored refusal text.
type corpusRecord struct {
	Text  string `json:"text"`
	Class string `json:"class,omitempty"`
	Added string `json:"added,omitempty"`
}

// corpusPath is the per-workspace corpus file.
func corpusPath(ws string) string {
	return filepath.Join(ws, ".kintsugi", "refusal-corpus", "corpus.jsonl")
}

// loadCorpus reads the texts (deduplicated, order preserved).
func loadCorpus(path string) ([]corpusRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []corpusRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r corpusRecord
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		key := strings.TrimSpace(r.Text)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out, sc.Err()
}

// saveCorpus rewrites the corpus file (temp + rename).
func saveCorpus(path string, recs []corpusRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// extractReportTexts pulls the "refusal text" fenced blocks out of an export
// report; anything else is treated as one text per non-empty line.
func extractReportTexts(data string) []string {
	if strings.Contains(data, "**refusal text**") {
		var out []string
		inFence, afterMarker := false, false
		for _, line := range strings.Split(data, "\n") {
			t := strings.TrimSpace(line)
			switch {
			case t == "**refusal text**":
				afterMarker = true
			case afterMarker && strings.HasPrefix(t, "```"):
				if inFence {
					inFence, afterMarker = false, false
				} else {
					inFence = true
				}
			case inFence:
				out = append(out, line)
			}
		}
		var texts []string
		for _, t := range out {
			if strings.TrimSpace(t) != "" {
				texts = append(texts, t)
			}
		}
		if len(texts) > 0 {
			// Fenced blocks are one text each; coalesce per block by joining
			// with newlines via a second pass is unnecessary at this scale.
			return texts
		}
	}
	var texts []string
	for _, line := range strings.Split(data, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			texts = append(texts, t)
		}
	}
	return texts
}

// tokenize lowers and splits a text into word tokens for clustering.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '\'':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// gramCounts counts contiguous n-word phrases across the texts.
func gramCounts(texts []string, n int) map[string]int {
	counts := map[string]int{}
	for _, tx := range texts {
		toks := tokenize(tx)
		seen := map[string]bool{}
		for i := 0; i+n <= len(toks); i++ {
			phrase := strings.Join(toks[i:i+n], " ")
			if seen[phrase] {
				continue
			}
			seen[phrase] = true
			counts[phrase]++
		}
	}
	return counts
}

// proposal is one suggested pattern with its evidence.
type proposal struct {
	Phrase  string
	Texts   int
	Pattern string
}

// proposePatterns finds the phrases that recur across missed texts and turns
// them into patterns. The clustering is deliberately simple — shared 3-word
// phrases, no ML — and the operator reviews every proposal: --apply is the
// only thing that writes.
func proposePatterns(missed []string, minTexts int) []proposal {
	if len(missed) == 0 {
		return nil
	}
	if minTexts < 1 {
		minTexts = 1
	}
	counts := gramCounts(missed, 3)
	var out []proposal
	for phrase, n := range counts {
		if n < minTexts {
			continue
		}
		// A phrase made of very common words matches everything; require at
		// least one token of four or more characters.
		meaningful := false
		for _, t := range strings.Fields(phrase) {
			if len(t) >= 4 {
				meaningful = true
			}
		}
		if !meaningful {
			continue
		}
		out = append(out, proposal{
			Phrase:  phrase,
			Texts:   n,
			Pattern: `\b` + regexpQuoteMetaMain(phrase) + `\b`,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Texts != out[j].Texts {
			return out[i].Texts > out[j].Texts
		}
		return out[i].Phrase < out[j].Phrase
	})
	// Overlapping phrases (a 3-gram and its neighbours) would add redundant
	// patterns. Keep a proposal only when it matches a text no earlier
	// proposal already covers.
	covered := make([]bool, len(missed))
	kept := make([]proposal, 0, len(out))
	for _, p := range out {
		phraseTokens := strings.Fields(p.Phrase)
		newly := false
		for i, tx := range missed {
			if covered[i] {
				continue
			}
			if containsPhrase(tx, phraseTokens) {
				covered[i] = true
				newly = true
			}
		}
		if newly {
			kept = append(kept, p)
		}
		if len(kept) >= 12 {
			break
		}
	}
	return kept
}

// cmdRefusalCorpus dispatches `refusal corpus`.
func cmdRefusalCorpus(ws string, args []string) {
	sub := "stats"
	if len(args) > 0 {
		sub = args[0]
	}
	path := corpusPath(ws)
	switch sub {
	case "add":
		fs := flag.NewFlagSet("refusal corpus add", flag.ExitOnError)
		file := fs.String("file", "", "read texts from this file (an export report or one text per line)")
		class := fs.String("class", "", "label the texts with this class")
		fs.Parse(args[1:])
		texts := fs.Args()
		if *file != "" {
			raw, err := os.ReadFile(*file)
			if err != nil {
				fail("%v", err)
			}
			texts = append(texts, extractReportTexts(string(raw))...)
		}
		if len(texts) == 0 {
			fail("usage: kintsugi refusal corpus add \"<text>\" | --file <report>")
		}
		recs, err := loadCorpus(path)
		if err != nil {
			fail("%v", err)
		}
		have := map[string]bool{}
		for _, r := range recs {
			have[r.Text] = true
		}
		added := 0
		for _, t := range texts {
			t = strings.TrimSpace(t)
			if t == "" || have[t] {
				continue
			}
			have[t] = true
			recs = append(recs, corpusRecord{Text: t, Class: *class})
			added++
		}
		if err := saveCorpus(path, recs); err != nil {
			fail("%v", err)
		}
		fmt.Printf("corpus %s · %d text(s) · added %d\n", path, len(recs), added)

	case "stats":
		recs, err := loadCorpus(path)
		if err != nil {
			fail("%v", err)
		}
		if len(recs) == 0 {
			fmt.Println("no corpus yet — add texts with `kintsugi refusal corpus add` or from an export report")
			return
		}
		app := newApp(ws)
		lx, err := app.RefusalLexicon()
		if err != nil {
			fail("%v", err)
		}
		matched, missed := 0, 0
		for _, r := range recs {
			if len(lx.ScanAll(r.Text)) > 0 {
				matched++
			} else {
				missed++
			}
		}
		fmt.Printf("corpus %s\n  %d text(s) · %d matched · %d missed (%.0f%% coverage)\n",
			path, len(recs), matched, missed, float64(matched)/float64(len(recs))*100)

	case "propose":
		fs := flag.NewFlagSet("refusal corpus propose", flag.ExitOnError)
		minTexts := fs.Int("min", 1, "minimum texts a proposed phrase must appear in")
		apply := fs.Bool("apply", false, "write the proposals through the lexicon validation path")
		fs.Parse(args[1:])
		recs, err := loadCorpus(path)
		if err != nil {
			fail("%v", err)
		}
		if len(recs) == 0 {
			fmt.Println("no corpus yet — add texts first")
			return
		}
		app := newApp(ws)
		lx, err := app.RefusalLexicon()
		if err != nil {
			fail("%v", err)
		}
		var missed []string
		for _, r := range recs {
			if len(lx.ScanAll(r.Text)) == 0 {
				missed = append(missed, r.Text)
			}
		}
		if len(missed) == 0 {
			fmt.Println("the current lexicon covers every text in the corpus")
			return
		}
		props := proposePatterns(missed, *minTexts)
		if len(props) == 0 {
			fmt.Printf("%d missed text(s); no recurring phrase to propose — add a pattern by hand with `kintsugi refusal add`\n", len(missed))
			return
		}
		fmt.Printf("%d missed text(s); %d proposal(s):\n\n", len(missed), len(props))
		for i, p := range props {
			fmt.Printf("%d. %s\n   appears in %d text(s) · pattern %s\n   kintsugi refusal add <class> %q\n",
				i+1, p.Phrase, p.Texts, p.Pattern, p.Pattern)
		}
		if !*apply {
			fmt.Println("\nreview, then re-run with --apply to write them through the lexicon validation path")
			return
		}
		lxPath := app.EffectiveLexiconPath()
		overrides, err := intercept.OverrideClasses(lxPath)
		if err != nil {
			fail("%v", err)
		}
		class := strings.TrimSpace(recs[0].Class)
		if class == "" {
			class = "moralizing"
		}
		added := 0
		for _, p := range props {
			if err := intercept.ValidatePattern(p.Pattern); err != nil {
				fmt.Printf("skip %q: %v\n", p.Pattern, err)
				continue
			}
			overrides[class] = append(overrides[class], p.Pattern)
			added++
		}
		if err := intercept.WriteOverride(lxPath, overrides); err != nil {
			fail("%v", err)
		}
		fmt.Printf("applied %d pattern(s) to %s\n", added, class)
		// The new coverage, measured honestly.
		if lx2, err := app.RefusalLexicon(); err == nil {
			m := 0
			for _, r := range recs {
				if len(lx2.ScanAll(r.Text)) > 0 {
					m++
				}
			}
			fmt.Printf("coverage now %d/%d\n", m, len(recs))
		}

	default:
		fail("usage: kintsugi refusal corpus add|stats|propose [--apply]")
	}
}

// regexpQuoteMetaMain escapes regex metacharacters for a phrase pattern; the
// export command has its own copy for the same reason (no shared unexported
// helper between the two files).
func regexpQuoteMetaMain(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// containsPhrase reports whether the text contains the contiguous token
// phrase, compared token-wise (the same tokenizer the grams use).
func containsPhrase(text string, phraseTokens []string) bool {
	toks := tokenize(text)
	if len(phraseTokens) == 0 || len(toks) < len(phraseTokens) {
		return false
	}
	for i := 0; i+len(phraseTokens) <= len(toks); i++ {
		match := true
		for j := range phraseTokens {
			if toks[i+j] != phraseTokens[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
