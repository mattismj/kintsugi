package main

// Tests for `kintsugi plan`. The command functions stay thin so they can be
// exercised here: the usage text, the plan naming, the plan head and the
// read-only executor are all checked without a model or a network.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/harness"
	"kintsugi/internal/plan"
	"kintsugi/internal/provider"
)

func TestPlanUsageNamesEverySubcommand(t *testing.T) {
	u := planUsage()
	for _, want := range []string{
		`kintsugi plan "<task>"`,
		"kintsugi plan list",
		"kintsugi plan show <name>",
		"--model",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("the usage text does not mention %q:\n%s", want, u)
		}
	}
	// The usage must say what the turn may not do.
	if !strings.Contains(u, "refuses every mutating tool") {
		t.Errorf("the usage text does not state the read-only rule:\n%s", u)
	}
}

func TestPlanTitle(t *testing.T) {
	cases := map[string]string{
		"Add OAuth login":            "Add OAuth login",
		"  one line  \nsecond line ": "one line",
		"":                           "plan",
	}
	for in, want := range cases {
		if got := planTitle(in); got != want {
			t.Errorf("planTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanHeadDropsTheMarker(t *testing.T) {
	doc := plan.Document("A plan", "# A plan\n\nfirst step\nsecond step\n")
	head := planHead(doc, 3)
	if len(head) != 3 {
		t.Fatalf("planHead returned %d lines, want 3: %q", len(head), head)
	}
	for _, line := range head {
		if strings.Contains(line, plan.Marker) {
			t.Fatalf("the marker leaked into the printed head: %q", head)
		}
	}
	if head[0] != "# A plan" {
		t.Fatalf("head[0] = %q, want the title", head[0])
	}
}

// TestPlanListAndShowReadTheWorkspace exercises the two read-only
// subcommands against a real plan directory.
func TestPlanListAndShowReadTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	art, err := plan.Save(ws, "Add OAuth login", "1. read the config\n2. add the callback\n")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	list, err := plan.List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != art.Name {
		t.Fatalf("List returned %+v, want the saved plan", list)
	}

	loaded, err := plan.Load(ws, art.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(loaded.Content, "read the config") {
		t.Fatalf("the loaded plan lost its body:\n%s", loaded.Content)
	}
}

// TestPlanExecutorRefusesMutatingTools is the safety property of plan mode:
// the planning turn may read, and it may not change the workspace.
func TestPlanExecutorRefusesMutatingTools(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := harness.New(ws)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	ex := planExecutor(app, nil)

	call := func(name, args string) provider.ToolCall {
		return provider.ToolCall{Type: "function", Function: provider.Function{Name: name, Arguments: args}}
	}

	// The read tools work.
	res := ex.Dispatch(call("read", `{"path":"keep.txt"}`))
	if res.Failed {
		t.Fatalf("read failed in plan mode: %s", res.Content)
	}
	res = ex.Dispatch(call("list", `{}`))
	if res.Failed {
		t.Fatalf("list failed in plan mode: %s", res.Content)
	}

	// Every mutating tool is refused.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"write", `{"path":"new.txt","content":"nope"}`},
		{"edit", `{"path":"keep.txt","old":"original","new":"changed"}`},
		{"apply_patch", `{"patch":"*** Begin Patch\n*** Add File: new.txt\n+nope\n*** End Patch"}`},
		{"bash", `{"command":"echo nope"}`},
	} {
		res := ex.Dispatch(call(tc.name, tc.args))
		if !res.Failed {
			t.Errorf("%s was allowed in plan mode: %s", tc.name, res.Content)
			continue
		}
		if !strings.Contains(res.Content, "denied") {
			t.Errorf("%s failed for the wrong reason: %s", tc.name, res.Content)
		}
	}

	// And the workspace is untouched.
	if _, err := os.Stat(filepath.Join(ws, "new.txt")); err == nil {
		t.Fatal("plan mode created a file")
	}
	body, err := os.ReadFile(filepath.Join(ws, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original\n" {
		t.Fatalf("plan mode changed a file: %q", string(body))
	}
}

// planProvider is a fake OpenAI-compatible endpoint: it answers one chat
// completion with the given text.
func planProvider(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", body)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// planWorkspace writes a workspace config pointing at the fake provider.
func planWorkspace(t *testing.T, baseURL string) string {
	t.Helper()
	ws := t.TempDir()
	cfg := fmt.Sprintf(`[core]
mode = "normal"
default_model = "mock/mock-chat"

[providers.mock]
base_url = %q
api_key_env = "MOCK_API_KEY"
family = "qwen"
models = ["mock-chat"]
`, baseURL)
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// captureStdout runs fn with stdout redirected into a pipe.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// TestPlanCommandWritesThePlan runs the whole command against a fake
// provider: the planning turn runs, the plan is written and the path is
// printed.
func TestPlanCommandWritesThePlan(t *testing.T) {
	srv := planProvider(t, "# Add a health endpoint\n\n1. read the router\n2. add the handler\n")
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MOCK_API_KEY", "test-key")
	ws := planWorkspace(t, srv.URL+"/v1")

	out := captureStdout(t, func() { cmdPlan(ws, []string{"add a health endpoint"}) })

	list, err := plan.List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("the command wrote %d plans, want 1:\n%s", len(list), out)
	}
	art := list[0]
	if !strings.Contains(art.Content, "# Add a health endpoint") {
		t.Fatalf("the plan lost the model's document:\n%s", art.Content)
	}
	if !strings.Contains(art.Content, "add the handler") {
		t.Fatalf("the plan lost the steps:\n%s", art.Content)
	}
	if !strings.Contains(out, art.Path) {
		t.Fatalf("the command did not print the plan path:\n%s", out)
	}
}
