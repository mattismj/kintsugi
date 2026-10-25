package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

// conversation returns a numbered transcript so summarizer input is easy to
// assert on.
func conversation(n int) []provider.Message {
	out := make([]provider.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		switch i % 3 {
		case 1:
			role = "assistant"
		case 2:
			role = "assistant"
		}
		out = append(out, provider.Message{Role: role, Content: "message-" + string(rune('a'+i))})
	}
	return out
}

func TestNeedsCompactionBoundaries(t *testing.T) {
	opts := CompactOptions{WindowTokens: 1000} // threshold defaults to 80%
	if opts.thresholdPct() != DefaultThresholdPct {
		t.Fatalf("default threshold = %d", opts.thresholdPct())
	}

	under := []provider.Message{{Role: "user", Content: strings.Repeat("x", 3196)}} // 799 tokens
	if ok, tokens := NeedsCompaction(under, opts); ok || tokens != 799 {
		t.Fatalf("under threshold: ok=%v tokens=%d, want false/799", ok, tokens)
	}

	at := []provider.Message{{Role: "user", Content: strings.Repeat("x", 3200)}} // 800 tokens
	if ok, tokens := NeedsCompaction(at, opts); !ok || tokens != 800 {
		t.Fatalf("at threshold: ok=%v tokens=%d, want true/800", ok, tokens)
	}

	// An explicit threshold is honoured.
	if ok, _ := NeedsCompaction(at, CompactOptions{WindowTokens: 1000, ThresholdPct: 90}); ok {
		t.Fatal("threshold 90% triggered at 80%")
	}
	// An unknown window can never trigger.
	if ok, tokens := NeedsCompaction(at, CompactOptions{}); ok || tokens != 800 {
		t.Fatalf("no window: ok=%v tokens=%d", ok, tokens)
	}
	// Empty history is never over threshold.
	if ok, tokens := NeedsCompaction(nil, opts); ok || tokens != 0 {
		t.Fatalf("empty history: ok=%v tokens=%d", ok, tokens)
	}
}

func TestMessageTokensCountsToolCalls(t *testing.T) {
	m := provider.Message{
		Role: "assistant",
		ToolCalls: []provider.ToolCall{{
			Function: provider.Function{Name: "read", Arguments: strings.Repeat("y", 400)},
		}},
	}
	if got, want := MessageTokens(m), ApproxTokens("read")+ApproxTokens(strings.Repeat("y", 400)); got != want {
		t.Fatalf("MessageTokens = %d, want %d", got, want)
	}
	if HistoryTokens(conversation(3)) == 0 {
		t.Fatal("HistoryTokens = 0 for a non-empty transcript")
	}
}

func TestCompactSummarizesOldestAndKeepsRecent(t *testing.T) {
	msgs := conversation(10)
	opts := CompactOptions{WindowTokens: 10, ThresholdPct: 80, KeepRecentPct: 40}

	var calls int
	var seen string
	out, changed, err := Compact(context.Background(), msgs, opts, func(_ context.Context, text string) (string, error) {
		calls++
		seen = text
		return "SUM", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if calls != 1 {
		t.Fatalf("summarizer calls = %d", calls)
	}
	// 10 messages, 40% recent → the last 4 are kept verbatim.
	if len(out) != 5 {
		t.Fatalf("out = %d messages: %+v", len(out), out)
	}
	if out[0].Role != "system" || !strings.HasPrefix(out[0].Content, "## Prior work summary\n\n") {
		t.Fatalf("summary message = %+v", out[0])
	}
	if out[0].Content != "## Prior work summary\n\nSUM" {
		t.Fatalf("summary message = %q", out[0].Content)
	}
	for i := 1; i < len(out); i++ {
		if out[i].Content != msgs[i+5].Content {
			t.Fatalf("kept message %d = %q, want %q", i, out[i].Content, msgs[i+5].Content)
		}
	}
	// The summarizer saw the cut prefix only.
	if !strings.Contains(seen, "message-a") || !strings.Contains(seen, "message-f") {
		t.Fatalf("summarizer input missing old messages:\n%s", seen)
	}
	if strings.Contains(seen, "message-g") || strings.Contains(seen, "message-j") {
		t.Fatalf("summarizer input leaked kept messages:\n%s", seen)
	}
	if !strings.Contains(seen, "OPERATOR:") || !strings.Contains(seen, "ASSISTANT:") {
		t.Fatalf("summarizer input is not role-tagged:\n%s", seen)
	}
}

func TestCompactDefaultsKeepRecent(t *testing.T) {
	msgs := conversation(10)
	out, changed, err := Compact(context.Background(), msgs,
		CompactOptions{WindowTokens: 1, ThresholdPct: 0, KeepRecentPct: 0},
		func(context.Context, string) (string, error) { return "SUM", nil })
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	// Default keep-recent is 40% of 10 messages.
	if len(out) != 1+4 {
		t.Fatalf("out = %d messages, want 5", len(out))
	}
}

func TestCompactKeepsToolPairTogether(t *testing.T) {
	// The 40% cut would land on the tool result at index 6; the kept suffix has
	// to start at the assistant message that issued the call.
	msgs := []provider.Message{
		{Role: "user", Content: "m0"},
		{Role: "assistant", Content: "m1"},
		{Role: "user", Content: "m2"},
		{Role: "assistant", Content: "m3"},
		{Role: "user", Content: "m4"},
		{Role: "assistant", Content: "m5", ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: provider.Function{Name: "read", Arguments: `{"path":"a"}`},
		}}},
		{Role: "tool", Content: "result-1", ToolCallID: "call-1", Name: "read"},
		{Role: "assistant", Content: "m7"},
		{Role: "user", Content: "m8"},
		{Role: "assistant", Content: "m9"},
	}
	out, changed, err := Compact(context.Background(), msgs,
		CompactOptions{WindowTokens: 10}, func(context.Context, string) (string, error) { return "SUM", nil })
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(out) != 1+5 {
		t.Fatalf("out = %d messages, want 6: %+v", len(out), out)
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("kept suffix does not start at the assistant call: %+v", out[1])
	}
	if out[2].Role != "tool" || out[2].ToolCallID != "call-1" {
		t.Fatalf("tool result lost its call: %+v", out[2])
	}
}

func TestCompactUnderThresholdIsNoOp(t *testing.T) {
	msgs := conversation(4)
	out, changed, err := Compact(context.Background(), msgs,
		CompactOptions{WindowTokens: 100000},
		func(context.Context, string) (string, error) {
			t.Fatal("summarizer called under threshold")
			return "", nil
		})
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("out = %d messages, want %d", len(out), len(msgs))
	}
}

func TestCompactSummarizerErrorIsReturned(t *testing.T) {
	boom := errors.New("model unavailable")
	out, changed, err := Compact(context.Background(), conversation(10),
		CompactOptions{WindowTokens: 1},
		func(context.Context, string) (string, error) { return "", boom })
	if err == nil {
		t.Fatal("summarizer error swallowed")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want wrapped %v", err, boom)
	}
	if changed || len(out) != 10 {
		t.Fatalf("failed compaction mutated the history: changed=%v len=%d", changed, len(out))
	}
}

func TestCompactEmptySummaryKeepsHistory(t *testing.T) {
	out, changed, err := Compact(context.Background(), conversation(10),
		CompactOptions{WindowTokens: 1},
		func(context.Context, string) (string, error) { return "   \n", nil })
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(out) != 10 {
		t.Fatalf("empty summary changed the history: changed=%v len=%d", changed, len(out))
	}
	if _, _, err := Compact(context.Background(), conversation(10),
		CompactOptions{WindowTokens: 1}, nil); err == nil {
		t.Fatal("nil summarizer accepted")
	}
}

func TestCompactSingleMessageGroupIsNotSplit(t *testing.T) {
	// Two messages with a 40% keep rate would keep zero; at least one survives
	// so the in-flight turn is never summarised away.
	msgs := []provider.Message{
		{Role: "user", Content: strings.Repeat("x", 4000)},
		{Role: "assistant", Content: strings.Repeat("y", 4000)},
	}
	out, changed, err := Compact(context.Background(), msgs,
		CompactOptions{WindowTokens: 10},
		func(context.Context, string) (string, error) { return "SUM", nil })
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(out) != 2 || out[1].Role != "assistant" {
		t.Fatalf("out = %+v", out)
	}
}

// ---- mid-session compaction (M10.3) ---------------------------------------

// TestBuildSummarySplitsTheConversation proves the split the live session
// uses: the older turns are handed to a summarizer, the recent ones stay
// verbatim, and the two sides never overlap.
func TestBuildSummarySplitsTheConversation(t *testing.T) {
	msgs := conversation(10)
	sum, kept := BuildSummary(msgs, 0.4)

	if sum.Covered != 6 || sum.Kept != 4 {
		t.Fatalf("covered/kept = %d/%d, want 6/4", sum.Covered, sum.Kept)
	}
	if len(kept) != 4 {
		t.Fatalf("kept %d messages, want 4", len(kept))
	}
	for i, m := range kept {
		if m.Content != msgs[6+i].Content {
			t.Fatalf("kept message %d = %q, want %q", i, m.Content, msgs[6+i].Content)
		}
	}
	// The summarizer sees the covered turns only, role-tagged.
	if !strings.Contains(sum.Text, "message-a") || !strings.Contains(sum.Text, "message-f") {
		t.Fatalf("summary text misses the covered turns:\n%s", sum.Text)
	}
	if strings.Contains(sum.Text, "message-g") {
		t.Fatalf("summary text leaked a kept turn:\n%s", sum.Text)
	}
	if !strings.Contains(sum.Text, "OPERATOR:") {
		t.Fatalf("summary text is not role-tagged:\n%s", sum.Text)
	}
}

// TestBuildSummaryNeverSplitsAToolPair proves the cut moves back so the kept
// suffix starts at the assistant message that owns the tool call: a tool
// result without its call is invalid for every wire protocol.
func TestBuildSummaryNeverSplitsAToolPair(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "m0"},
		{Role: "assistant", Content: "m1", ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: provider.Function{Name: "read", Arguments: `{"path":"a"}`},
		}}},
		{Role: "tool", Content: "result-1", ToolCallID: "call-1", Name: "read"},
		{Role: "assistant", Content: "m3"},
		{Role: "user", Content: "m4"},
		{Role: "assistant", Content: "m5"},
		{Role: "user", Content: "m6"},
		{Role: "assistant", Content: "m7"},
		{Role: "user", Content: "m8"},
		{Role: "assistant", Content: "m9"},
	}
	// 80% of 10 messages is 8, so the cut lands on the tool result at index 2.
	sum, kept := BuildSummary(msgs, 0.8)
	if sum.Covered != 1 || sum.Kept != 9 {
		t.Fatalf("covered/kept = %d/%d, want 1/9", sum.Covered, sum.Kept)
	}
	if len(kept) != 9 {
		t.Fatalf("kept %d messages, want 9", len(kept))
	}
	if len(kept[0].ToolCalls) != 1 || kept[0].ToolCalls[0].ID != "call-1" {
		t.Fatalf("the kept suffix does not start at the assistant call: %+v", kept[0])
	}
	if kept[1].Role != "tool" || kept[1].ToolCallID != "call-1" {
		t.Fatalf("the tool result lost its call: %+v", kept[1])
	}
}

// TestBuildSummaryKeepsTheWholeShortConversation proves a conversation that
// cannot be summarized comes back whole.
func TestBuildSummaryKeepsTheWholeShortConversation(t *testing.T) {
	msgs := conversation(1)
	sum, kept := BuildSummary(msgs, 0.4)
	if sum.Covered != 0 || sum.Text != "" {
		t.Fatalf("summary = %+v, want nothing to summarize", sum)
	}
	if len(kept) != 1 || kept[0].Content != msgs[0].Content {
		t.Fatalf("kept = %+v, want the whole conversation", kept)
	}

	if sum, kept := BuildSummary(nil, 0.4); sum.Covered != 0 || len(kept) != 0 {
		t.Fatalf("empty conversation = %+v / %+v", sum, kept)
	}
}

// TestBuildSummaryDefaultsTheKeepRatio proves the documented default applies
// when no ratio is given.
func TestBuildSummaryDefaultsTheKeepRatio(t *testing.T) {
	msgs := conversation(10)
	sum, kept := BuildSummary(msgs, 0)
	if sum.Kept != 4 || len(kept) != 4 {
		t.Fatalf("kept = %d, want the default 40%% of 10", sum.Kept)
	}
}

// TestSummaryMessageCarriesTheHeading proves the summary rides in one system
// message with the documented heading.
func TestSummaryMessageCarriesTheHeading(t *testing.T) {
	m := SummaryMessage("SUM")
	if m.Role != "system" {
		t.Fatalf("role = %q, want system", m.Role)
	}
	if m.Content != "## Prior work summary\n\nSUM" {
		t.Fatalf("content = %q", m.Content)
	}
	if got := SummaryMessage("  SUM  ").Content; got != "## Prior work summary\n\nSUM" {
		t.Fatalf("content = %q", got)
	}
}
