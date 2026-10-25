package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

// longHistory builds n alternating user/assistant turns.
func longHistory(n int) []provider.Message {
	out := make([]provider.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, provider.Message{Role: role, Content: fmt.Sprintf("turn-%02d", i)})
	}
	return out
}

// requestMessages returns the message array of the n-th request the mock
// model received.
func requestMessages(t *testing.T, srv *sseServer, n int) []map[string]any {
	t.Helper()
	if len(srv.requests) <= n {
		t.Fatalf("model got %d requests, want more than %d", len(srv.requests), n)
	}
	raw, _ := srv.requests[n]["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

// TestCompactLoopSummarizesOnceAtTheThreshold proves the in-session trigger:
// a conversation past the threshold is summarized once, the summary is a
// single system message in place of the oldest turns, and the compiled system
// prompt is not rewritten.
func TestCompactLoopSummarizesOnceAtTheThreshold(t *testing.T) {
	srv := newSSEServer(t, func(int, map[string]any) []string { return []string{"done"} })
	loop := srv.loop(t, nil, nil)

	calls := 0
	cl := &CompactLoop{
		Loop:             loop,
		CompactAfter:     10,
		CompactKeepRatio: 0.4,
		Summarize: func(_ context.Context, transcript string) (string, error) {
			calls++
			if !strings.Contains(transcript, "turn-00") {
				t.Errorf("the summarizer did not see the oldest turns:\n%s", transcript)
			}
			if strings.Contains(transcript, "turn-11") {
				t.Errorf("the summarizer saw a kept turn:\n%s", transcript)
			}
			return "SUMMARY", nil
		},
	}

	history := longHistory(12)
	final, updated, err := cl.RunConversation(context.Background(), "SYSTEM PROMPT", history, "next task")
	if err != nil {
		t.Fatal(err)
	}
	if final != "done" {
		t.Fatalf("final = %q", final)
	}
	if calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", calls)
	}

	msgs := requestMessages(t, srv, 0)
	// [system prompt][summary][4 kept][task]
	if len(msgs) != 7 {
		t.Fatalf("request carries %d messages, want 7: %v", len(msgs), msgs)
	}
	if got := fmt.Sprint(msgs[0]["content"]); got != "SYSTEM PROMPT" {
		t.Fatalf("system prompt = %q, want the compiled bytes unchanged", got)
	}
	if msgs[1]["role"] != "system" {
		t.Fatalf("the summary is not a system message: %v", msgs[1])
	}
	if got := fmt.Sprint(msgs[1]["content"]); got != "## Prior work summary\n\nSUMMARY" {
		t.Fatalf("summary message = %q", got)
	}
	if n := strings.Count(fmt.Sprint(msgs), "## Prior work summary"); n != 1 {
		t.Fatalf("the summary appears %d times, want exactly 1", n)
	}
	// The kept tail is verbatim and the covered turns are gone.
	if got := fmt.Sprint(msgs[2]["content"]); got != "turn-08" {
		t.Fatalf("kept tail starts at %q, want turn-08", got)
	}
	for _, m := range msgs {
		if strings.Contains(fmt.Sprint(m["content"]), "turn-00") {
			t.Fatalf("a covered turn survived: %v", m)
		}
	}

	// The next turn is under the threshold again: the compaction fires once,
	// not on every turn.
	if _, _, err := cl.RunConversation(context.Background(), "SYSTEM PROMPT", updated, "another task"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("summarizer calls = %d after the second turn, want 1", calls)
	}
}

// TestCompactLoopUnderThresholdLeavesHistoryAlone proves a conversation under
// the threshold is never summarized.
func TestCompactLoopUnderThresholdLeavesHistoryAlone(t *testing.T) {
	srv := newSSEServer(t, func(int, map[string]any) []string { return []string{"done"} })
	loop := srv.loop(t, nil, nil)

	calls := 0
	cl := &CompactLoop{
		Loop:         loop,
		CompactAfter: 10,
		Summarize: func(context.Context, string) (string, error) {
			calls++
			return "SUMMARY", nil
		},
	}
	if _, _, err := cl.RunConversation(context.Background(), "SYSTEM PROMPT", longHistory(10), "task"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", calls)
	}
	msgs := requestMessages(t, srv, 0)
	if strings.Contains(fmt.Sprint(msgs), "## Prior work summary") {
		t.Fatalf("a summary was injected under the threshold:\n%v", msgs)
	}
}

// TestCompactLoopFailingSummarizerKeepsHistory proves a failed summary never
// costs the thread: the turn continues on the full history.
func TestCompactLoopFailingSummarizerKeepsHistory(t *testing.T) {
	srv := newSSEServer(t, func(int, map[string]any) []string { return []string{"done"} })
	loop := srv.loop(t, nil, nil)

	cl := &CompactLoop{
		Loop:         loop,
		CompactAfter: 4,
		Summarize: func(context.Context, string) (string, error) {
			return "", errors.New("model unavailable")
		},
	}
	history := longHistory(6)
	final, updated, err := cl.RunConversation(context.Background(), "SYSTEM PROMPT", history, "task")
	if err != nil {
		t.Fatalf("a failed compaction failed the turn: %v", err)
	}
	if final != "done" {
		t.Fatalf("final = %q", final)
	}
	msgs := requestMessages(t, srv, 0)
	if strings.Contains(fmt.Sprint(msgs), "## Prior work summary") {
		t.Fatalf("a failed compaction still injected a summary:\n%v", msgs)
	}
	// Every turn is still there: [system][6 history][task]
	if len(msgs) != 8 {
		t.Fatalf("request carries %d messages, want 8: %v", len(msgs), msgs)
	}
	if got := fmt.Sprint(msgs[1]["content"]); got != "turn-00" {
		t.Fatalf("the oldest turn is gone: %q", got)
	}
	if len(updated) != len(history)+1 {
		t.Fatalf("history = %d messages, want %d", len(updated), len(history)+1)
	}
}

// TestCompactNeverSplitsAnAssistantToolPair proves the kept suffix starts at
// the assistant message that owns the tool call, so no tool result is left
// without its call.
func TestCompactNeverSplitsAnAssistantToolPair(t *testing.T) {
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
	res, err := Compact(context.Background(), msgs, CompactOptions{After: 4, KeepRatio: 0.8},
		func(context.Context, string) (string, error) { return "SUM", nil })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("changed = false, want true")
	}
	kept := res.Messages[1:] // the summary message comes first
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

// TestCompactEmptySummaryKeepsHistory proves an empty summary is not a
// summary: the conversation is left alone.
func TestCompactEmptySummaryKeepsHistory(t *testing.T) {
	msgs := longHistory(6)
	res, err := Compact(context.Background(), msgs, CompactOptions{After: 4},
		func(context.Context, string) (string, error) { return "  \n", nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("an empty summary changed the history")
	}
	if len(res.Messages) != len(msgs) {
		t.Fatalf("messages = %d, want %d", len(res.Messages), len(msgs))
	}
}

// TestCompactDisabledIsANoOp proves a negative threshold turns the compaction
// off.
func TestCompactDisabledIsANoOp(t *testing.T) {
	msgs := longHistory(50)
	res, err := Compact(context.Background(), msgs, CompactOptions{After: -1},
		func(context.Context, string) (string, error) {
			t.Fatal("the summarizer ran while compaction was disabled")
			return "", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || len(res.Messages) != len(msgs) {
		t.Fatalf("changed=%v len=%d", res.Changed, len(res.Messages))
	}
}

// TestCompactionSummaryPromptIsStructured pins the sections a continued
// agent needs: the decisions, the file states, the outcomes, and the next
// action — not a prose retelling.
func TestCompactionSummaryPromptIsStructured(t *testing.T) {
	for _, want := range []string{"## Asked", "## Decided", "## Files touched", "## Outcomes", "## Open"} {
		if !strings.Contains(summaryPrompt, want) {
			t.Errorf("the summary prompt is missing the %q section:\n%s", want, summaryPrompt)
		}
	}
	if strings.Contains(summaryPrompt, "Write plain prose") {
		t.Error("the prompt still asks for prose instead of the structured sections")
	}
}

// TestCompactionKeepsTheWorkingSetVerbatim: the tail of the conversation —
// the tool calls and results the agent is working with — is kept, not
// summarized, so an in-flight edit survives the compaction.
func TestCompactionKeepsTheWorkingSetVerbatim(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "old task"},
		{Role: "assistant", Content: "old answer"},
	}
	// Fill the middle so the threshold is crossed.
	for i := 0; i < 30; i++ {
		msgs = append(msgs, provider.Message{Role: "assistant", Content: "filler"})
	}
	// The working set: a tool call and its result.
	msgs = append(msgs,
		provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{
			Type: "function", Function: provider.Function{Name: "edit", Arguments: `{"path":"x.go"}`},
		}}},
		provider.Message{Role: "tool", Name: "edit", Content: `{"ok":true}`},
	)

	comp, err := Compact(context.Background(), msgs, CompactOptions{}, func(_ context.Context, transcript string) (string, error) {
		// The working set is kept verbatim, so the summary correctly covers
		// the OLDER turns only — the tool call is not summarized away.
		return "## Asked\nthe task\n\n## Open\ncontinue the edit", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !comp.Changed {
		t.Fatal("the conversation was not compacted")
	}
	// The last message (the tool result) is kept byte-identical.
	last := comp.Messages[len(comp.Messages)-1]
	if last.Role != "tool" || last.Content != `{"ok":true}` {
		t.Fatalf("the working set was summarized away: %+v", last)
	}
}
