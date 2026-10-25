package doctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"kintsugi/internal/config"
)

var sweepNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// writeSession drops a fixture file into dir and backdates it.
func writeSession(t *testing.T, dir, name string, ageDays float64, size int) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mt := sweepNow.Add(-time.Duration(ageDays * 24 * float64(time.Hour)))
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

func ids(cands []RetentionCandidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.ID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSweepAgePolicy(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "20260101-090000-aaa.sqlite", 40, 100) // delete
	writeSession(t, dir, "20260220-090000-bbb.sqlite", 9, 200)  // keep
	writeSession(t, dir, "20260115-090000-ccc.jsonl", 45, 50)   // delete (legacy)

	rep, err := Sweep(dir, 0, 30, sweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, want := ids(rep.Delete), []string{"20260101-090000-aaa", "20260115-090000-ccc"}; !equalStrings(got, want) {
		t.Fatalf("delete = %v, want %v", got, want)
	}
	if got, want := ids(rep.Keep), []string{"20260220-090000-bbb"}; !equalStrings(got, want) {
		t.Fatalf("keep = %v, want %v", got, want)
	}
	if rep.BytesReclaimed != 150 {
		t.Fatalf("bytes reclaimed = %d, want 150", rep.BytesReclaimed)
	}
}

func TestSweepCountPolicy(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "20260101-090000-aaa.sqlite", 20, 10)
	writeSession(t, dir, "20260105-090000-bbb.sqlite", 15, 20)
	writeSession(t, dir, "20260110-090000-ccc.sqlite", 10, 30)
	writeSession(t, dir, "20260120-090000-ddd.sqlite", 5, 40)

	rep, err := Sweep(dir, 2, 0, sweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, want := ids(rep.Keep), []string{"20260120-090000-ddd", "20260110-090000-ccc"}; !equalStrings(got, want) {
		t.Fatalf("keep = %v, want %v", got, want)
	}
	if got, want := ids(rep.Delete), []string{"20260105-090000-bbb", "20260101-090000-aaa"}; !equalStrings(got, want) {
		t.Fatalf("delete = %v, want %v", got, want)
	}
	if rep.BytesReclaimed != 30 {
		t.Fatalf("bytes reclaimed = %d, want 30", rep.BytesReclaimed)
	}

	// A count policy larger than the store deletes nothing.
	if rep, err = Sweep(dir, 10, 0, sweepNow); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(rep.Delete) != 0 || len(rep.Keep) != 4 {
		t.Fatalf("delete=%d keep=%d, want 0/4", len(rep.Delete), len(rep.Keep))
	}
}

func TestSweepBothPolicies(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "20251201-090000-aaa.sqlite", 90, 100) // age
	writeSession(t, dir, "20260210-090000-bbb.sqlite", 19, 200) // count
	writeSession(t, dir, "20260220-090000-ccc.sqlite", 9, 300)  // keep
	writeSession(t, dir, "20260228-090000-ddd.sqlite", 1, 400)  // keep

	// maxSessions=2 keeps ccc+ddd; aaa is also older than 30 days, so the
	// union is aaa+bbb and no candidate is counted twice.
	rep, err := Sweep(dir, 2, 30, sweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, want := ids(rep.Delete), []string{"20260210-090000-bbb", "20251201-090000-aaa"}; !equalStrings(got, want) {
		t.Fatalf("delete = %v, want %v", got, want)
	}
	if got, want := ids(rep.Keep), []string{"20260228-090000-ddd", "20260220-090000-ccc"}; !equalStrings(got, want) {
		t.Fatalf("keep = %v, want %v", got, want)
	}
	if rep.BytesReclaimed != 300 {
		t.Fatalf("bytes reclaimed = %d, want 300", rep.BytesReclaimed)
	}
}

func TestSweepNeitherPolicy(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "20240101-090000-aaa.sqlite", 400, 10)
	writeSession(t, dir, "20240102-090000-bbb.jsonl", 399, 10)

	rep, err := Sweep(dir, 0, 0, sweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(rep.Delete) != 0 {
		t.Fatalf("delete = %v, want empty (0/0 keeps forever)", ids(rep.Delete))
	}
	if len(rep.Keep) != 2 {
		t.Fatalf("keep = %d files, want 2", len(rep.Keep))
	}
	if rep.BytesReclaimed != 0 {
		t.Fatalf("bytes reclaimed = %d, want 0", rep.BytesReclaimed)
	}
}

func TestSweepIgnoresSidecarsAndJunk(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "20260101-090000-aaa.sqlite", 60, 100)
	writeSession(t, dir, "20260101-090000-aaa.sqlite-wal", 60, 1000)
	writeSession(t, dir, "20260101-090000-aaa.sqlite-shm", 60, 2000)
	writeSession(t, dir, "20260102-090000-bbb.sqlite.migrating", 60, 3000)
	writeSession(t, dir, "notes.txt", 60, 4000)
	writeSession(t, dir, "20260103-090000-ccc.jsonl", 60, 5000)

	if err := os.Mkdir(filepath.Join(dir, "20260104-090000-ddd.sqlite"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rep, err := Sweep(dir, 0, 30, sweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, want := ids(rep.Delete), []string{"20260101-090000-aaa", "20260103-090000-ccc"}; !equalStrings(got, want) {
		t.Fatalf("delete = %v, want %v (sidecars, scratch, dirs and junk ignored)", got, want)
	}
	// Only the two real store files count toward the reclaim total.
	if rep.BytesReclaimed != 5100 {
		t.Fatalf("bytes reclaimed = %d, want 5100", rep.BytesReclaimed)
	}
}

func TestSweepNewestFirstOrdering(t *testing.T) {
	dir := t.TempDir()
	// Written out of order on purpose.
	writeSession(t, dir, "z-late.sqlite", 2, 10)
	writeSession(t, dir, "a-early.jsonl", 40, 10)
	writeSession(t, dir, "m-mid.sqlite", 20, 10)

	rep, err := Sweep(dir, 2, 0, sweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, want := ids(rep.Keep), []string{"z-late", "m-mid"}; !equalStrings(got, want) {
		t.Fatalf("keep order = %v, want %v (newest first)", got, want)
	}
	if got, want := ids(rep.Delete), []string{"a-early"}; !equalStrings(got, want) {
		t.Fatalf("delete order = %v, want %v", got, want)
	}
}

func TestSweepMissingDirIsEmpty(t *testing.T) {
	rep, err := Sweep(filepath.Join(t.TempDir(), "does-not-exist"), 5, 7, sweepNow)
	if err != nil {
		t.Fatalf("Sweep on missing dir: %v", err)
	}
	if len(rep.Delete) != 0 || len(rep.Keep) != 0 || rep.BytesReclaimed != 0 {
		t.Fatalf("want empty report, got %+v", rep)
	}
}

func TestSweepNeverDeletes(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "20250101-090000-aaa.sqlite", 400, 10)

	if _, err := Sweep(dir, 0, 30, sweepNow); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "20250101-090000-aaa.sqlite")); err != nil {
		t.Fatalf("Sweep removed a file: %v", err)
	}
}

// The sweep is policy-driven, so the [retention] table must actually bind.
// KINTSUGI_CONFIG is last in the precedence list, which keeps the assertion
// independent of whatever global config exists on the test machine.
func TestRetentionConfigDecodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kintsugi.toml")
	body := "[retention]\nmax_sessions = 200\nmax_age_days = 7\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("KINTSUGI_CONFIG", path)

	cfg, _, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Retention.MaxSessions != 200 || cfg.Retention.MaxAgeDays != 7 {
		t.Fatalf("retention = %+v, want max_sessions=200 max_age_days=7", cfg.Retention)
	}
	if d := config.Defaults().Retention; d.MaxSessions != 0 || d.MaxAgeDays != 0 {
		t.Fatalf("default retention = %+v, want zero (keep forever)", d)
	}
}
