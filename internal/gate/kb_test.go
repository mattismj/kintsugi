package gate

// WP-1.6: knowledge-base citations in gate findings. The reviewer is offered
// the chunk ids of the bases bound to its role and may cite them; with no
// binding the request is byte-identical to the pre-citation reviewer.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

// reqCapture is the raw request body of one reviewer call.
type reqCapture struct {
	mu   sync.Mutex
	body []byte
}

func (c *reqCapture) set(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append([]byte(nil), b...)
}

func (c *reqCapture) get() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.body...)
}

// captureReviewer answers with content and records the exact request body, so a
// test can compare prompts byte for byte rather than by substring.
func captureReviewer(t *testing.T, content string) (*Reviewer, *reqCapture) {
	t.Helper()
	cap := &reqCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		cap.set(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	c, err := provider.New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "gate-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return &Reviewer{Client: c}, cap
}

// messages decodes the request body into role→content.
func messages(t *testing.T, body []byte) (system, user string) {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = m.Content
		case "user":
			user = m.Content
		}
	}
	return system, user
}

// TestReviewerPromptUnchangedWithoutKnowledge is the honesty test: a role with
// no bound knowledge base gets exactly the reviewer it had before WP-1.6 — the
// same system prompt verbatim and the same user message, with no citation
// section and no extra clause.
func TestReviewerPromptUnchangedWithoutKnowledge(t *testing.T) {
	const diff = "diff --git a/x b/x\n+curl http://198.51.100.7"
	const scope = "lab only: no public infrastructure"

	rev, cap := captureReviewer(t, `{"findings":[],"verdict":"pass"}`)
	base, err := rev.Review(context.Background(), diff, scope)
	if err != nil {
		t.Fatal(err)
	}
	baseline := cap.get()
	if len(baseline) == 0 {
		t.Fatal("no request captured")
	}
	system, user := messages(t, baseline)
	if system != reviewPrompt {
		t.Fatalf("system prompt changed:\n%s", system)
	}
	wantUser := "Declared scope:\n" + scope + "\n\nDiff:\n" + diff
	if user != wantUser {
		t.Fatalf("user message changed:\n--- got ---\n%s\n--- want ---\n%s", user, wantUser)
	}
	if strings.Contains(system, "Knowledge bases") || strings.Contains(user, "Citable") {
		t.Fatal("citation machinery leaked into a base-less review")
	}

	// An empty hook is the same as no hook: the prompt must not move.
	empty := *rev
	empty.Knowledge = func(string) []string { return nil }
	if _, err := empty.Review(context.Background(), diff, scope); err != nil {
		t.Fatal(err)
	}
	if got := cap.get(); string(got) != string(baseline) {
		t.Fatalf("an empty knowledge hook changed the request:\n--- got ---\n%s\n--- want ---\n%s", got, baseline)
	}

	// Findings stay free of citations when nothing was offered.
	if len(base.Findings) != 0 {
		t.Fatalf("unexpected findings: %+v", base)
	}
}

// TestReviewerCitesBoundKBChunks proves the bound chunk ids reach the prompt as
// citable material, for the exact artifact under review.
func TestReviewerCitesBoundKBChunks(t *testing.T) {
	const diff = "diff --git a/agent.py b/agent.py\n+obfuscate the loader"
	rev, cap := captureReviewer(t, `{"findings":[{"severity":"medium","file":"agent.py","line":1,"consequence":"detection signature","kb":["attack/T1027"]}],"verdict":"revise"}`)

	var askedFor string
	rev.Knowledge = func(artifact string) []string {
		askedFor = artifact
		return []string{"attack/T1027", "attack/T1055"}
	}

	out, err := rev.ReviewArtifact(context.Background(), Artifact{Kind: ArtifactDiff, Content: diff}, "lab only")
	if err != nil {
		t.Fatal(err)
	}
	if askedFor != diff {
		t.Fatalf("the hook was asked about %q, want the artifact under review", askedFor)
	}
	system, user := messages(t, cap.get())
	if !strings.Contains(system, "Knowledge bases:") || !strings.Contains(system, `"kb":["attack/T1027"]`) {
		t.Fatalf("system prompt does not ask for kb citations:\n%s", system)
	}
	for _, want := range []string{"- attack/T1027", "- attack/T1055"} {
		if !strings.Contains(user, want) {
			t.Fatalf("prompt does not name the citable chunk %q:\n%s", want, user)
		}
	}
	// The base prompt is still the whole of the original: the clause is additive.
	if !strings.HasPrefix(system, reviewPrompt) {
		t.Fatalf("the diff reviewer prompt was rewritten rather than extended:\n%s", system)
	}
	if len(out.Findings) != 1 || strings.Join(out.Findings[0].KB, ",") != "attack/T1027" {
		t.Fatalf("citation did not survive the review: %+v", out.Findings)
	}
}

// TestFindingKBCitationsRoundTrip proves Finding.KB survives the JSON parse, is
// omitted when absent, and cannot name a chunk the reviewer was not offered.
func TestFindingKBCitationsRoundTrip(t *testing.T) {
	rev, err := ParseReview("```json\n" +
		`{"findings":[{"severity":"high","file":"a.go","line":3,"consequence":"x","kb":["attack/T1027","attack/T1027",""]},` +
		`{"severity":"low","file":"b.go","consequence":"y"}],"verdict":"block"}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rev.Findings[0].KB, ","); got != "attack/T1027,attack/T1027," {
		t.Fatalf("raw parse dropped what the model said: %q", got)
	}
	if rev.Findings[1].KB != nil {
		t.Fatalf("a finding with no kb key grew citations: %v", rev.Findings[1].KB)
	}

	// Round-trip the struct back through JSON: the field is omitempty, so a
	// finding without citations is byte-identical to the pre-WP-1.6 shape.
	b, err := json.Marshal(rev.Findings[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"kb"`) {
		t.Fatalf("empty citations serialised: %s", b)
	}
	b, err = json.Marshal(rev.Findings[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kb":["attack/T1027","attack/T1027",""]`) {
		t.Fatalf("citations lost on re-encode: %s", b)
	}

	// Only offered ids survive a real review: an invented citation is dropped.
	offered := []string{"attack/T1027"}
	restrictCitations(rev, offered)
	if got := strings.Join(rev.Findings[0].KB, ","); got != "attack/T1027" {
		t.Fatalf("restriction left %q, want the single offered id", got)
	}
	restrictCitations(rev, nil)
	if rev.Findings[0].KB != nil {
		t.Fatalf("citations survived an empty offer: %v", rev.Findings[0].KB)
	}
}

// TestFeedbackCarriesCitations proves a citation reaches the revise cycle: the
// worker reads Feedback, not the JSON.
func TestFeedbackCarriesCitations(t *testing.T) {
	plain := Feedback([]Result{{Kind: "opsec-review", Verdict: "revise",
		Findings: []Finding{{Severity: "medium", File: "a.go", Line: 2, Consequence: "c"}}}})
	if strings.Contains(plain, "(kb:") {
		t.Fatalf("a finding with no citations grew one: %q", plain)
	}
	withKB := Feedback([]Result{{Kind: "opsec-review", Verdict: "revise",
		Findings: []Finding{{Severity: "medium", File: "a.go", Line: 2, Consequence: "c", KB: []string{"attack/T1027", "cwe/CWE-78"}}}}})
	if !strings.Contains(withKB, "a.go:2 — c (kb: attack/T1027, cwe/CWE-78)") {
		t.Fatalf("feedback dropped the citations: %q", withKB)
	}
}

// TestRestrictCitationsDropsInventedIDs proves the end-to-end path: a model that
// cites a chunk it was never shown cannot put that id in the result.
func TestRestrictCitationsDropsInventedIDs(t *testing.T) {
	rev, _ := captureReviewer(t, `{"findings":[{"severity":"high","file":"a.go","line":1,"consequence":"c","kb":["attack/T1027","made/UP"]}],"verdict":"block"}`)
	rev.Knowledge = func(string) []string { return []string{"attack/T1027"} }

	out, err := rev.Review(context.Background(), "diff --git a/a.go b/a.go\n+x", "lab only")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out.Findings[0].KB, ","); got != "attack/T1027" {
		t.Fatalf("citations = %q, want only the offered chunk", got)
	}
}
