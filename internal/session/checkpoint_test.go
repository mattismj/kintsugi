package session

import (
	"reflect"
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

// checkpointMessages builds a small, realistic transcript: an operator turn, an
// assistant turn with a tool call, its result, and a closing assistant answer.
func checkpointMessages() []provider.Message {
	return []provider.Message{
		{Role: "user", Content: "read the README and summarise the build steps"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: provider.Function{Name: "read", Arguments: `{"path":"README.md"}`},
		}}},
		{Role: "tool", Content: "build: go build ./...", ToolCallID: "call-1", Name: "read"},
		{Role: "assistant", Content: "one step: go build ./..."},
	}
}

func historyMessages(t *testing.T, id string) []provider.Message {
	t.Helper()
	msgs, err := History(id)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestCheckpointRoundTrip(t *testing.T) {
	s := newTestSession(t)
	s.Log("user", "read the README and summarise the build steps")
	s.Log("assistant", "one step: go build ./...")
	msgs := checkpointMessages()

	cp, err := SaveCheckpoint(s.ID, "before refactor", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if cp.ID == "" || cp.TS == "" {
		t.Fatalf("checkpoint missing id/ts: %+v", cp)
	}
	if cp.Label != "before refactor" {
		t.Fatalf("label = %q", cp.Label)
	}
	if !reflect.DeepEqual(cp.Messages, msgs) {
		t.Fatalf("returned messages differ:\n got %+v\nwant %+v", cp.Messages, msgs)
	}

	list, err := ListCheckpoints(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != cp.ID {
		t.Fatalf("list = %+v", list)
	}

	loaded, err := LoadCheckpoint(s.ID, cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != cp.ID || loaded.Label != cp.Label || loaded.TS != cp.TS {
		t.Fatalf("loaded metadata differs: %+v vs %+v", loaded, cp)
	}
	if !reflect.DeepEqual(loaded.Messages, msgs) {
		t.Fatalf("loaded messages differ:\n got %+v\nwant %+v", loaded.Messages, msgs)
	}

	// A checkpoint is not a turn: it must not leak into the model-visible
	// history, and saving one must not disturb the events already logged.
	if hist := historyMessages(t, s.ID); len(hist) != 2 {
		t.Fatalf("checkpoint leaked into history: %+v", hist)
	}
	if hist := historyMessages(t, s.ID); !reflect.DeepEqual(hist, []provider.Message{
		{Role: "user", Content: "read the README and summarise the build steps"},
		{Role: "assistant", Content: "one step: go build ./..."},
	}) {
		t.Fatalf("history changed by checkpoint: %+v", hist)
	}
}

func TestCheckpointListIsOldestFirst(t *testing.T) {
	s := newTestSession(t)
	first, err := SaveCheckpoint(s.ID, "one", []provider.Message{{Role: "user", Content: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := SaveCheckpoint(s.ID, "two", []provider.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := ListCheckpoints(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d checkpoints", len(list))
	}
	if list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("order = %s, %s; want %s, %s", list[0].ID, list[1].ID, first.ID, second.ID)
	}
	if len(list[0].Messages) != 1 || len(list[1].Messages) != 2 {
		t.Fatalf("message counts = %d, %d", len(list[0].Messages), len(list[1].Messages))
	}
	if list[1].Messages[1].Content != "second" {
		t.Fatalf("second checkpoint content = %+v", list[1].Messages)
	}
}

func TestCheckpointUnknownID(t *testing.T) {
	s := newTestSession(t)
	s.Log("user", "hello")

	if _, err := LoadCheckpoint(s.ID, "20260101-000000-deadbe"); err == nil {
		t.Fatal("unknown checkpoint id accepted")
	} else if !strings.Contains(err.Error(), "no checkpoint") {
		t.Fatalf("error = %v", err)
	}
	if _, err := LoadCheckpoint(s.ID, ""); err == nil {
		t.Fatal("empty checkpoint id accepted")
	}
	if err := MarkRewind(s.ID, "nope"); err == nil {
		t.Fatal("MarkRewind accepted an unknown checkpoint")
	}
	if _, err := SaveCheckpoint("", "label", nil); err == nil {
		t.Fatal("empty session id accepted")
	}
	if _, err := ListCheckpoints("20990101-000000-ffffff"); err == nil {
		t.Fatal("unknown session accepted")
	}
}

func TestRewindMarker(t *testing.T) {
	s := newTestSession(t)
	s.Log("user", "start")
	if _, ok, err := LatestRewind(s.ID); err != nil || ok {
		t.Fatalf("fresh session reported a rewind: ok=%v err=%v", ok, err)
	}

	cp, err := SaveCheckpoint(s.ID, "keep this", checkpointMessages())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LatestRewind(s.ID); err != nil || ok {
		t.Fatalf("saving a checkpoint counted as a rewind: ok=%v err=%v", ok, err)
	}

	if err := MarkRewind(s.ID, cp.ID); err != nil {
		t.Fatal(err)
	}
	latest, ok, err := LatestRewind(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("rewind marker not found")
	}
	if latest.ID != cp.ID || !reflect.DeepEqual(latest.Messages, cp.Messages) {
		t.Fatalf("latest rewind = %+v, want %+v", latest, cp)
	}

	// The marker is the newest rewind, not the first one.
	second, err := SaveCheckpoint(s.ID, "second", []provider.Message{{Role: "user", Content: "later"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkRewind(s.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	latest, ok, err = LatestRewind(s.ID)
	if err != nil || !ok {
		t.Fatalf("latest rewind after second mark: ok=%v err=%v", ok, err)
	}
	if latest.ID != second.ID {
		t.Fatalf("latest rewind = %s, want %s", latest.ID, second.ID)
	}

	// Rewinding never rewrites earlier events.
	if hist := historyMessages(t, s.ID); len(hist) != 1 || hist[0].Content != "start" {
		t.Fatalf("rewind mutated history: %+v", hist)
	}
}

func TestCheckpointSurvivesReopen(t *testing.T) {
	s := newTestSession(t)
	msgs := checkpointMessages()
	cp, err := SaveCheckpoint(s.ID, "durable", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkRewind(s.ID, cp.ID); err != nil {
		t.Fatal(err)
	}
	id := s.ID
	s.Close()

	reopened, err := Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := LoadCheckpoint(id, cp.ID); err != nil {
		t.Fatal(err)
	}
	latest, ok, err := LatestRewind(id)
	if err != nil || !ok || latest.ID != cp.ID {
		t.Fatalf("rewind lost on reopen: ok=%v err=%v latest=%+v", ok, err, latest)
	}
}
