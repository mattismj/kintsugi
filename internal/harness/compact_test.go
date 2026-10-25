package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

// compactServer is a fake curator: it streams a fixed summary and records what
// it was asked to summarise.
type compactServer struct {
	*httptest.Server
	requests []map[string]any
	reply    string
}

func newCompactServer(t *testing.T, reply string) *compactServer {
	t.Helper()
	s := &compactServer{reply: reply}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		s.requests = append(s.requests, req)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": s.reply}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// testApp builds an App whose curator model points at baseURL with the given
// context window. Adapters are loaded from the embedded registry, as in a real
// boot.
func testApp(t *testing.T, baseURL string, window int) *App {
	t.Helper()
	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Adapters: reg, Workspace: t.TempDir()}
	app.Cfg.Providers = map[string]config.Provider{
		"mock": {
			BaseURL: baseURL,
			Wire:    "openai-completions",
			Models:  []config.Model{{ID: "curator", Family: "deepseek", Context: window}},
		},
	}
	app.Cfg.Core.DefaultModel = "mock/curator"
	app.Cfg.Context.CuratorModel = "mock/curator"
	return app
}

// transcript builds n messages of ~tokens tokens each.
func transcript(n, tokens int) []provider.Message {
	body := strings.Repeat("x", tokens*4)
	out := make([]provider.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, provider.Message{Role: role, Content: body})
	}
	return out
}

func TestCompactHistoryWithoutCuratorModelIsNoOp(t *testing.T) {
	msgs := transcript(10, 1000)
	app := &App{Cfg: config.File{}}
	out, changed, err := app.CompactHistory(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(out) != len(msgs) {
		t.Fatalf("changed=%v len=%d, want false/%d", changed, len(out), len(msgs))
	}

	var nilApp *App
	if _, changed, err := nilApp.CompactHistory(context.Background(), msgs); err != nil || changed {
		t.Fatalf("nil app: changed=%v err=%v", changed, err)
	}
}

func TestCompactHistoryUnderThresholdDoesNotCallTheModel(t *testing.T) {
	srv := newCompactServer(t, "SUM")
	app := testApp(t, srv.URL, 100000)

	msgs := transcript(4, 10)
	out, changed, err := app.CompactHistory(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(out) != len(msgs) {
		t.Fatalf("changed=%v len=%d", changed, len(out))
	}
	if len(srv.requests) != 0 {
		t.Fatalf("curator called %d times under threshold", len(srv.requests))
	}
}

func TestCompactHistorySummarizesWithCuratorModel(t *testing.T) {
	srv := newCompactServer(t, "SUM")
	app := testApp(t, srv.URL, 100) // 80-token threshold

	msgs := transcript(10, 50) // ~500 tokens
	out, changed, err := app.CompactHistory(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if len(out) != 1+4 {
		t.Fatalf("out = %d messages, want 5", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "## Prior work summary\n\nSUM" {
		t.Fatalf("summary message = %+v", out[0])
	}
	for i := 1; i < len(out); i++ {
		if out[i].Content != msgs[i+5].Content {
			t.Fatalf("kept message %d differs from the recent tail", i)
		}
	}

	if len(srv.requests) != 1 {
		t.Fatalf("curator requests = %d, want 1", len(srv.requests))
	}
	req := srv.requests[0]
	if req["model"] != "curator" {
		t.Fatalf("model = %v", req["model"])
	}
	msgsIn, _ := req["messages"].([]any)
	if len(msgsIn) != 2 {
		t.Fatalf("prompt = %d messages: %+v", len(msgsIn), msgsIn)
	}
	system, _ := msgsIn[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(system["content"]), "compress a long agent session") {
		t.Fatalf("system prompt missing: %v", system["content"])
	}
	user, _ := msgsIn[1].(map[string]any)
	body := fmt.Sprint(user["content"])
	if !strings.Contains(body, "Transcript to compress") {
		t.Fatalf("user prompt not framed:\n%s", body)
	}
	if got := strings.Count(body, "OPERATOR:"); got != 3 {
		t.Fatalf("transcript holds %d operator turns, want the 3 summarised ones:\n%s", got, body)
	}
}

func TestCompactHistorySurfacesCuratorErrors(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:1", 10)
	msgs := transcript(10, 50)
	out, changed, err := app.CompactHistory(context.Background(), msgs)
	if err == nil {
		t.Fatal("expected a curator transport error")
	}
	if changed || len(out) != len(msgs) {
		t.Fatalf("failed compaction mutated history: changed=%v len=%d", changed, len(out))
	}

	bad := &App{Cfg: config.File{}}
	bad.Cfg.Context.CuratorModel = "nope/model"
	if _, _, err := bad.CompactHistory(context.Background(), msgs); err == nil ||
		!strings.Contains(err.Error(), "curator") {
		t.Fatalf("unresolvable curator error = %v", err)
	}
}
