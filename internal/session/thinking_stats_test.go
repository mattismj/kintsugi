package session

import (
	"testing"
)

// TestThinkingStatsCountsVolume pins the report's thinking row: per-session
// turn and byte counts, never content.
func TestThinkingStatsCountsVolume(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("user", "task")
	s.Log("thinking", "a reasoning line that is fairly long")
	s.Log("assistant", "answer")
	s.Log("thinking", "more reasoning")
	s.Close()

	stats, err := ThinkingStats("")
	_ = stats
	_ = err
	// Dir() resolves the redirected cache; use it explicitly.
	dir, _ := Dir()
	stats, err = ThinkingStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Session != s.ID {
		t.Fatalf("stats = %+v", stats)
	}
	if stats[0].Turns != 2 {
		t.Errorf("turns = %d, want 2", stats[0].Turns)
	}
	if stats[0].Bytes == 0 {
		t.Error("bytes = 0")
	}
}

func TestThinkingStatsSkipsEmpty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("user", "task")
	s.Log("assistant", "answer")
	s.Close()
	dir, _ := Dir()
	stats, _ := ThinkingStats(dir)
	for _, st := range stats {
		if st.Turns == 0 {
			t.Errorf("a session with no thinking appeared: %+v", st)
		}
	}
}
