package kb

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexFixture loads testdata/indexkb: three chunks, of which T1055 carries a
// proprietary license and is therefore not injectable.
func indexFixture(t *testing.T) *KB {
	t.Helper()
	k, err := Load(filepath.Join("testdata", "indexkb"))
	if err != nil {
		t.Fatalf("Load(testdata/indexkb): %v", err)
	}
	if len(k.Chunks) != 3 {
		t.Fatalf("fixture has %d chunks, want 3", len(k.Chunks))
	}
	return k
}

// newTestIndex opens a fresh index over the fixture in a temp directory.
func newTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := OpenIndexAt(indexFixture(t), filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatalf("OpenIndexAt: %v", err)
	}
	t.Cleanup(func() {
		if err := ix.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return ix
}

// copyTree copies a directory tree (fixtures into a temp dir) so a test can
// write an index next to the KB without polluting testdata.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func ids(results []Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Chunk.ID
	}
	return out
}

// TestReindexCountIdempotent covers requirement 1: the rebuild counts what it
// indexed, skips (and reports) the non-injectable chunk, and is idempotent.
func TestReindexCountIdempotent(t *testing.T) {
	ix := newTestIndex(t)

	n, err := ix.Reindex()
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n != 2 {
		t.Fatalf("Reindex indexed %d chunks, want 2 (3 chunks, 1 non-injectable)", n)
	}
	if !strings.Contains(fmt.Sprint(ix.Warnings), "T1055") {
		t.Errorf("warnings should name the skipped chunk, got %v", ix.Warnings)
	}

	rows := 0
	if err := ix.DB.QueryRow(`SELECT COUNT(*) FROM kb_chunks`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != n {
		t.Fatalf("table holds %d rows, Reindex reported %d", rows, n)
	}

	n2, err := ix.Reindex()
	if err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	if n2 != n {
		t.Fatalf("second Reindex indexed %d chunks, want %d (must be idempotent)", n2, n)
	}
	if err := ix.DB.QueryRow(`SELECT COUNT(*) FROM kb_chunks`).Scan(&rows); err != nil {
		t.Fatalf("count rows after second rebuild: %v", err)
	}
	if rows != n {
		t.Fatalf("after second rebuild the table holds %d rows, want %d", rows, n)
	}
}

// TestOpenIndexUsesKBDirectory checks the default location and that a closed
// index reopens with its schema and rows intact.
func TestOpenIndexUsesKBDirectory(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "indexkb"), dir)
	k, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ix, err := OpenIndex(k)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	if _, err := ix.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IndexFile)); err != nil {
		t.Fatalf("index file not at <kb>/%s: %v", IndexFile, err)
	}

	path := filepath.Join(dir, IndexFile)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenIndexAt(k, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.Search("powershell", 3, nil)
	if err != nil {
		t.Fatalf("search after reopen: %v", err)
	}
	if len(got) == 0 || got[0].Chunk.ID != "T1059.001" {
		t.Fatalf("search after reopen: %v", ids(got))
	}
	if n, err := reopened.Reindex(); err != nil || n != 2 {
		t.Fatalf("Reindex after reopen = (%d, %v), want (2, nil)", n, err)
	}
}

// TestSearchRanking covers requirement 2: the obvious match ranks first, and a
// technique id is itself a usable query.
func TestSearchRanking(t *testing.T) {
	ix := newTestIndex(t)
	if _, err := ix.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	got, err := ix.Search("powershell", 5, nil)
	if err != nil {
		t.Fatalf("Search(powershell): %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Search(powershell) returned nothing")
	}
	if got[0].Chunk.ID != "T1059.001" {
		t.Fatalf("Search(powershell) ranked %v, want T1059.001 first", ids(got))
	}
	if got[0].Score >= 0 {
		t.Errorf("bm25 score = %v, want a negative value (ascending = best first)", got[0].Score)
	}
	if len(got) > 1 && got[0].Score > got[1].Score {
		t.Errorf("results not ordered by ascending bm25: %v then %v", got[0].Score, got[1].Score)
	}
	if got[0].Chunk.Title == "" || got[0].Chunk.Body == "" {
		t.Errorf("result chunk is not fully hydrated: %+v", got[0].Chunk)
	}

	byID, err := ix.Search("T1059.001", 5, nil)
	if err != nil {
		t.Fatalf("Search(T1059.001): %v", err)
	}
	if len(byID) == 0 || byID[0].Chunk.ID != "T1059.001" {
		t.Fatalf("technique id query ranked %v, want T1059.001", ids(byID))
	}

	// Hostile FTS syntax must not error.
	if _, err := ix.Search(`"powershell" AND ( OR *`, 5, nil); err != nil {
		t.Fatalf("hostile query errored: %v", err)
	}
	if _, err := ix.Search("   ", 5, nil); err != nil {
		t.Fatalf("blank query errored: %v", err)
	}
	if got, err := ix.Search("   ", 5, nil); err != nil || len(got) != 0 {
		t.Fatalf("blank query = (%v, %v), want no results", ids(got), err)
	}
}

// TestNonInjectableNeverReturned covers requirement 3.
func TestNonInjectableNeverReturned(t *testing.T) {
	ix := newTestIndex(t)
	if _, err := ix.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// "injection" only appears in T1055, which must not be indexed at all.
	got, err := ix.Search("injection", 8, nil)
	if err != nil {
		t.Fatalf("Search(injection): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Search(injection) = %v, want no results (T1055 is not injectable)", ids(got))
	}

	// Naming it in mustInclude does not smuggle it in either.
	got, err = ix.Search("powershell", 8, []string{"T1055", "t1055"})
	if err != nil {
		t.Fatalf("Search with must-include T1055: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Search(powershell) returned nothing")
	}
	for _, r := range got {
		if r.Chunk.ID == "T1055" || !r.Chunk.Injectable {
			t.Fatalf("non-injectable chunk returned: %v", ids(got))
		}
	}

	var rows int
	if err := ix.DB.QueryRow(`SELECT COUNT(*) FROM kb_chunks WHERE id = 'T1055'`).Scan(&rows); err != nil {
		t.Fatalf("count T1055: %v", err)
	}
	if rows != 0 {
		t.Fatalf("T1055 is in the index %d time(s), want 0", rows)
	}
}

// TestTopK covers requirement 4, including the default of 8.
func TestTopK(t *testing.T) {
	ix := newTestIndex(t)
	if _, err := ix.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	one, err := ix.Search("powershell", 1, nil)
	if err != nil {
		t.Fatalf("Search(topK=1): %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("Search(topK=1) returned %d results, want 1", len(one))
	}

	// A synthetic KB with more chunks than the default, to prove topK <= 0
	// means exactly 8.
	synthetic := &KB{ID: "synthetic", Version: "1", Title: "synthetic", Dir: t.TempDir()}
	for i := 0; i < 12; i++ {
		synthetic.Chunks = append(synthetic.Chunks, Chunk{
			ID:         fmt.Sprintf("C%02d", i),
			Title:      "Alpha technique",
			Tactic:     "execution",
			Body:       "alpha beta gamma payload staging",
			Source:     "synthetic",
			License:    "CC BY 4.0",
			Injectable: true,
		})
	}
	big, err := OpenIndexAt(synthetic, filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatalf("OpenIndexAt(synthetic): %v", err)
	}
	defer big.Close()
	if n, err := big.Reindex(); err != nil || n != 12 {
		t.Fatalf("Reindex(synthetic) = (%d, %v), want (12, nil)", n, err)
	}

	def, err := big.Search("alpha", 0, nil)
	if err != nil {
		t.Fatalf("Search(topK=0): %v", err)
	}
	if len(def) != defaultTopK {
		t.Fatalf("Search(topK=0) returned %d results, want %d", len(def), defaultTopK)
	}
	three, err := big.Search("alpha", 3, nil)
	if err != nil {
		t.Fatalf("Search(topK=3): %v", err)
	}
	if len(three) != 3 {
		t.Fatalf("Search(topK=3) returned %d results, want 3", len(three))
	}
	all, err := big.Search("alpha", 100, nil)
	if err != nil {
		t.Fatalf("Search(topK=100): %v", err)
	}
	if len(all) != 12 {
		t.Fatalf("Search(topK=100) returned %d results, want 12", len(all))
	}
}

// TestMustInclude covers requirement 5: prepended, deduped, unknown ignored.
func TestMustInclude(t *testing.T) {
	ix := newTestIndex(t)
	if _, err := ix.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// T1027 does not match "encodedcommand" at all.
	plain, err := ix.Search("encodedcommand", 8, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range plain {
		if r.Chunk.ID == "T1027" {
			t.Fatalf("fixture assumption broken: T1027 matched the query: %v", ids(plain))
		}
	}

	got, err := ix.Search("encodedcommand", 8, []string{" t1027 ", "T1027", "", "T9999", "T1055"})
	if err != nil {
		t.Fatalf("Search with must-include: %v", err)
	}
	if len(got) == 0 || got[0].Chunk.ID != "T1027" {
		t.Fatalf("must-include not prepended: %v", ids(got))
	}
	if got[0].Chunk.Body == "" || !got[0].Chunk.Injectable {
		t.Errorf("must-include chunk is not hydrated: %+v", got[0].Chunk)
	}
	seen := map[string]int{}
	for _, r := range got {
		seen[r.Chunk.ID]++
	}
	if seen["T1027"] != 1 {
		t.Errorf("T1027 appeared %d times, want 1 (deduped)", seen["T1027"])
	}
	for _, unknown := range []string{"T9999", "T1055"} {
		if seen[unknown] != 0 {
			t.Errorf("%s appeared in results, want ignored", unknown)
		}
	}

	// A must-include that also matches the BM25 query is still returned once,
	// and first.
	both, err := ix.Search("base64", 8, []string{"T1027"})
	if err != nil {
		t.Fatalf("Search(base64): %v", err)
	}
	if len(both) == 0 || both[0].Chunk.ID != "T1027" {
		t.Fatalf("must-include not first: %v", ids(both))
	}
	seen = map[string]int{}
	for _, r := range both {
		seen[r.Chunk.ID]++
	}
	if seen["T1027"] != 1 {
		t.Fatalf("T1027 appeared %d times, want 1: %v", seen["T1027"], ids(both))
	}

	// Order among several must-includes is preserved.
	order, err := ix.Search("base64", 8, []string{"T1059.001", "T1027"})
	if err != nil {
		t.Fatalf("Search with two must-includes: %v", err)
	}
	if len(order) < 2 || order[0].Chunk.ID != "T1059.001" || order[1].Chunk.ID != "T1027" {
		t.Fatalf("must-include order not preserved: %v", ids(order))
	}
}

// TestSearchBudget covers requirement 6.
func TestSearchBudget(t *testing.T) {
	ix := newTestIndex(t)
	if _, err := ix.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	all, err := ix.Search("powershell base64", 8, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("fixture assumption broken: want >= 2 matches, got %v", ids(all))
	}
	size := func(r Result) int {
		return len(r.Chunk.Title) + len(r.Chunk.Body) + len(r.Chunk.AdversaryPrompt)
	}
	total := func(rs []Result) int {
		sum := 0
		for _, r := range rs {
			sum += size(r)
		}
		return sum
	}

	// No cap at all: identical to Search.
	for _, maxBytes := range []int{0, -1} {
		got, err := ix.SearchBudget("powershell base64", 8, maxBytes, nil)
		if err != nil {
			t.Fatalf("SearchBudget(maxBytes=%d): %v", maxBytes, err)
		}
		if len(got) != len(all) {
			t.Fatalf("SearchBudget(maxBytes=%d) returned %d results, want %d (no cap)",
				maxBytes, len(got), len(all))
		}
	}

	// Room for exactly the first hit: the second is dropped rather than
	// overflowing the budget.
	one := size(all[0])
	got, err := ix.SearchBudget("powershell base64", 8, one, nil)
	if err != nil {
		t.Fatalf("SearchBudget(tight): %v", err)
	}
	if len(got) != 1 || got[0].Chunk.ID != all[0].Chunk.ID {
		t.Fatalf("SearchBudget(cap=%d) = %v, want just %s", one, ids(got), all[0].Chunk.ID)
	}
	if got := total(got); got > one {
		t.Fatalf("budget overflowed: %d > %d bytes", got, one)
	}

	// One byte short of the second hit: still just the first.
	got, err = ix.SearchBudget("powershell base64", 8, size(all[0])+size(all[1])-1, nil)
	if err != nil {
		t.Fatalf("SearchBudget(near): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SearchBudget(one byte short of two hits) = %v, want 1 result", ids(got))
	}

	// Room for both.
	roomy := size(all[0]) + size(all[1])
	got, err = ix.SearchBudget("powershell base64", 8, roomy, nil)
	if err != nil {
		t.Fatalf("SearchBudget(roomy): %v", err)
	}
	if len(got) != 2 || got[0].Chunk.ID != all[0].Chunk.ID || got[1].Chunk.ID != all[1].Chunk.ID {
		t.Fatalf("SearchBudget(cap=%d) = %v, want %v", roomy, ids(got), ids(all[:2]))
	}
	if got := total(got); got > roomy {
		t.Fatalf("budget overflowed: %d > %d bytes", got, roomy)
	}

	// Must-include chunks survive a budget of one byte, and optional hits that
	// cannot fit are dropped.
	got, err = ix.SearchBudget("powershell base64", 8, 1, []string{"T1027"})
	if err != nil {
		t.Fatalf("SearchBudget(must-include, tight): %v", err)
	}
	if len(got) != 1 || got[0].Chunk.ID != "T1027" {
		t.Fatalf("SearchBudget(cap=1, must= T1027) = %v, want [T1027]", ids(got))
	}

	// topK still bounds the optional hits.
	got, err = ix.SearchBudget("powershell base64", 1, 0, []string{"T1027"})
	if err != nil {
		t.Fatalf("SearchBudget(topK=1): %v", err)
	}
	if len(got) != 2 || got[0].Chunk.ID != "T1027" {
		t.Fatalf("SearchBudget(topK=1, must=T1027) = %v, want [T1027 <1 hit>]", ids(got))
	}
}

// TestNilAndEmptyIndex covers nil-safety: every method is a no-op, not a panic.
func TestNilAndEmptyIndex(t *testing.T) {
	var ix *Index
	if got, err := ix.Search("powershell", 0, []string{"T1027"}); err != nil || len(got) != 0 {
		t.Fatalf("nil Search = (%v, %v), want empty", ids(got), err)
	}
	if got, err := ix.SearchBudget("powershell", 0, 10, []string{"T1027"}); err != nil || len(got) != 0 {
		t.Fatalf("nil SearchBudget = (%v, %v), want empty", ids(got), err)
	}
	if n, err := ix.Reindex(); err != nil || n != 0 {
		t.Fatalf("nil Reindex = (%d, %v), want (0, nil)", n, err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}

	// A zero Index (no DB yet) behaves the same way.
	empty := &Index{}
	if got, err := empty.Search("powershell", 0, nil); err != nil || len(got) != 0 {
		t.Fatalf("zero Search = (%v, %v), want empty", ids(got), err)
	}
	if _, err := empty.SearchBudget("powershell", 0, 10, nil); err != nil {
		t.Fatalf("zero SearchBudget: %v", err)
	}
	if _, err := empty.Reindex(); err == nil {
		t.Fatal("zero Reindex: want an error, got nil")
	}
	if err := empty.Close(); err != nil {
		t.Fatalf("zero Close: %v", err)
	}
}

// TestOpenIndexErrors covers the argument checks.
func TestOpenIndexErrors(t *testing.T) {
	if _, err := OpenIndex(nil); err == nil {
		t.Fatal("OpenIndex(nil): want an error")
	}
	if _, err := OpenIndexAt(nil, filepath.Join(t.TempDir(), "i.sqlite")); err == nil {
		t.Fatal("OpenIndexAt(nil): want an error")
	}
	if _, err := OpenIndexAt(&KB{Dir: t.TempDir()}, ""); err == nil {
		t.Fatal("OpenIndexAt(empty path): want an error")
	}
	if _, err := OpenIndex(&KB{}); err == nil {
		t.Fatal("OpenIndex(KB without Dir): want an error")
	}
}
