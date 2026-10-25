package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddPersistAndReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.json")
	s, err := Open(path, map[string]Limit{
		"deepseek": {LimitTokens: 1000, Reset: "daily"},
		"local":    {},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	if ok, _ := s.CanFit("deepseek", 600); !ok {
		t.Fatal("600 did not fit in 1000")
	}
	if err := s.Add("deepseek", 600); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("deepseek", 100); err != nil {
		t.Fatal(err)
	}
	if rem, limited := s.Remaining("deepseek"); !limited || rem != 300 {
		t.Fatalf("remaining = %d limited=%v", rem, limited)
	}
	if ok, msg := s.CanFit("deepseek", 400); ok || msg == "" {
		t.Fatalf("400 fit when only 300 remains: %v", msg)
	}
	if ok, _ := s.CanFit("local", 1_000_000); !ok {
		t.Fatal("unlimited provider refused")
	}

	// Reload from disk: same period, same usage.
	s2, err := Open(path, s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	s2.Now = func() time.Time { return now }
	if rem, _ := s2.Remaining("deepseek"); rem != 300 {
		t.Fatalf("reloaded remaining = %d", rem)
	}

	// Next day resets the bucket.
	s2.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if rem, _ := s2.Remaining("deepseek"); rem != 1000 {
		t.Fatalf("daily reset remaining = %d", rem)
	}
	if err := s2.Add("deepseek", 10); err != nil {
		t.Fatal(err)
	}
	if e := s2.current("deepseek"); e.Period != "2026-10-22" || e.UsedTokens != 10 {
		t.Fatalf("reset entry = %+v", e)
	}

	states := s2.Snapshot()
	if len(states) != 2 || states[0].Provider != "deepseek" {
		t.Fatalf("snapshot = %+v", states)
	}
}

func TestPeriods(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := Period(now, "monthly"); got != "2026-01" {
		t.Fatalf("monthly = %s", got)
	}
	if got := Period(now, "weekly"); got != "2026-W01" {
		t.Fatalf("weekly = %s", got)
	}
	if got := Period(now, ""); got != "all" {
		t.Fatalf("none = %s", got)
	}
}

func TestCorruptLedgerErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, nil); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
}
