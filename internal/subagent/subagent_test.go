package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"kintsugi/internal/config"
	"kintsugi/internal/hooks"
	"kintsugi/internal/mcp"
	"kintsugi/internal/provider"
	"kintsugi/internal/tools"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@local",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@local",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	return dir
}

func mockClient(t *testing.T, content func(n int) string) (*provider.Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req["messages"])
		if !strings.Contains(string(raw), "bounded subagent") {
			t.Errorf("worker contract missing from request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content(int(n))}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(srv.Close)
	c, err := provider.New(config.Provider{BaseURL: srv.URL, Wire: "openai-completions", Offline: true}, "subagent-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestFanOutProducesTypedResults(t *testing.T) {
	repo := gitRepo(t)
	client, calls := mockClient(t, func(int) string {
		return "```json\n{\"status\":\"done\",\"summary\":\"implemented fixture\",\"findings\":[{\"severity\":\"medium\",\"file\":\"a.txt\",\"line\":2,\"note\":\"noisy log\"}],\"changed_files\":[\"a.txt\"]}\n```"
	})
	m := &Manager{Cfg: Config{
		Workspace: repo, Client: client, SystemPrompt: "parent stack",
		Policy: tools.Policy{}, Timeout: 30 * time.Second, Max: 3,
	}}
	out, err := m.RunTasks(context.Background(), []tools.TaskRequest{
		{Title: "build fixture A", Prompt: "make A"},
		{Title: "build fixture B", Prompt: "make B"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results  []Result `json:"results"`
		Findings string   `json:"findings_markdown"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, out)
	}
	if *calls != 2 || len(report.Results) != 2 {
		t.Fatalf("calls=%d results=%d", *calls, len(report.Results))
	}
	for _, r := range report.Results {
		if r.Status != "done" || len(r.Findings) != 1 {
			t.Fatalf("result = %+v", r)
		}
		if r.Worktree != "" {
			t.Fatalf("worktree not removed: %s", r.Worktree)
		}
	}
	if !strings.Contains(report.Findings, "### Findings") || !strings.Contains(report.Findings, "[medium] a.txt:2") {
		t.Fatalf("findings markdown = %q", report.Findings)
	}
	if !strings.Contains(report.Findings, "### Results") || !strings.Contains(report.Findings, "build fixture A — done") {
		t.Fatalf("results markdown = %q", report.Findings)
	}

	list, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(list), "worktree "); got != 1 {
		t.Fatalf("worktrees left behind (%d):\n%s", got, list)
	}
}

func TestInvalidWorkerResultIsFlagged(t *testing.T) {
	repo := gitRepo(t)
	client, _ := mockClient(t, func(int) string { return "I did the thing, trust me." })
	m := &Manager{Cfg: Config{Workspace: repo, Client: client, Policy: tools.Policy{}, Timeout: 30 * time.Second}}
	out, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "vague", Prompt: "do it"}})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []Result `json:"results"`
	}
	_ = json.Unmarshal([]byte(out), &report)
	if len(report.Results) != 1 || report.Results[0].Status != "invalid" {
		t.Fatalf("result = %+v", report.Results)
	}
	if !strings.Contains(report.Results[0].Error, "valid JSON") {
		t.Fatalf("error = %q", report.Results[0].Error)
	}
}

func TestRequiresGitWorkspace(t *testing.T) {
	client, _ := mockClient(t, func(int) string { return `{"status":"done","summary":"x"}` })
	m := &Manager{Cfg: Config{Workspace: t.TempDir(), Client: client}}
	if _, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "a", Prompt: "b"}}); err == nil {
		t.Fatal("non-git workspace accepted")
	}
}

func TestParseResultValidation(t *testing.T) {
	res, err := ParseResult("```json\n{\"status\":\"done\",\"summary\":\"ok\",\"findings\":[{\"severity\":\"weird\",\"note\":\"x\"}]}\n```")
	if err != nil || res.Status != "done" || res.Findings[0].Severity != "info" {
		t.Fatalf("parse = %+v, %v", res, err)
	}
	if _, err := ParseResult(`{"status":"maybe"}`); err == nil {
		t.Fatal("bad status accepted")
	}
	if _, err := ParseResult("done"); err == nil {
		t.Fatal("prose accepted")
	}
}

func TestWorkerPolicyReadOnlyRoles(t *testing.T) {
	if workerPolicy(tools.Policy{}, "opsec").Yolo {
		t.Fatal("opsec worker is writable")
	}
	if workerPolicy(tools.Policy{}, "researcher").Yolo {
		t.Fatal("researcher worker is writable")
	}
	if !workerPolicy(tools.Policy{}, "coder").Yolo || !workerPolicy(tools.Policy{}, "").Yolo {
		t.Fatal("coder worker is not writable inside its worktree")
	}
}

// ---------- WP-4.1 per-role models / WP-4.5 MCP propagation ----------

// endpoint is one mock provider that records every request body it sees, so a
// test can prove which client served which worker.
type endpoint struct {
	srv     *httptest.Server
	calls   int32
	content string
	mu      sync.Mutex
	bodies  []string
}

func newEndpoint(t *testing.T, content string) *endpoint {
	t.Helper()
	e := &endpoint{content: content}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&e.calls, 1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req["messages"])
		e.mu.Lock()
		e.bodies = append(e.bodies, string(raw))
		e.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": e.content}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *endpoint) client(t *testing.T) *provider.Client {
	t.Helper()
	c, err := provider.New(config.Provider{BaseURL: e.srv.URL, Wire: "openai-completions", Offline: true}, "role-test", "normal")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *endpoint) requestBodies() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.bodies...)
}

func (e *endpoint) count() int32 { return atomic.LoadInt32(&e.calls) }

type logRecord struct {
	kind string
	v    any
}

func captureLog() (func(kind string, v any), func() []logRecord) {
	var mu sync.Mutex
	var recs []logRecord
	emit := func(kind string, v any) {
		mu.Lock()
		recs = append(recs, logRecord{kind: kind, v: v})
		mu.Unlock()
	}
	snapshot := func() []logRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]logRecord(nil), recs...)
	}
	return emit, snapshot
}

// TestPerRoleClientSelection proves each worker reaches the endpoint bound to
// its role, and that a task item without a role is served by the coder role.
func TestPerRoleClientSelection(t *testing.T) {
	repo := gitRepo(t)
	coderEP := newEndpoint(t, `{"status":"done","summary":"coder ran"}`)
	researchEP := newEndpoint(t, `{"status":"done","summary":"researcher ran"}`)
	coderClient, researchClient := coderEP.client(t), researchEP.client(t)

	var mu sync.Mutex
	var roles []string
	emit, logs := captureLog()

	m := &Manager{Cfg: Config{
		Workspace: repo, Client: coderClient, SystemPrompt: "parent stack",
		Policy: tools.Policy{}, Timeout: 30 * time.Second, Max: 3,
		ResolveClient: func(role string) (*provider.Client, error) {
			mu.Lock()
			roles = append(roles, role)
			mu.Unlock()
			if role == "researcher" {
				return researchClient, nil
			}
			return coderClient, nil
		},
		Log: emit,
	}}
	out, err := m.RunTasks(context.Background(), []tools.TaskRequest{
		{Title: "alpha", Prompt: "do coder work", Role: "coder"},
		{Title: "beta", Prompt: "do research work", Role: "researcher"},
		{Title: "gamma", Prompt: "unlabelled work"}, // no role -> coder
	})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []Result `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, out)
	}
	if len(report.Results) != 3 {
		t.Fatalf("results = %+v", report.Results)
	}

	if got := coderEP.count(); got != 2 {
		t.Fatalf("coder endpoint calls = %d, want 2 (coder + default)", got)
	}
	if got := researchEP.count(); got != 1 {
		t.Fatalf("researcher endpoint calls = %d, want 1", got)
	}
	for _, body := range coderEP.requestBodies() {
		if !strings.Contains(body, "do coder work") && !strings.Contains(body, "unlabelled work") {
			t.Fatalf("coder endpoint saw a foreign task: %s", body)
		}
		if strings.Contains(body, "do research work") {
			t.Fatalf("researcher task leaked to the coder endpoint: %s", body)
		}
	}
	if body := researchEP.requestBodies()[0]; !strings.Contains(body, "do research work") {
		t.Fatalf("researcher endpoint body: %s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	sawDefaultRole := false
	for _, r := range roles {
		if r == "coder" {
			sawDefaultRole = true
		}
	}
	if !sawDefaultRole {
		t.Fatalf("resolved roles = %v, want a coder role for the role-less task", roles)
	}
	for _, rec := range logs() {
		if rec.kind == "subagent_role_fallback" {
			t.Fatalf("unexpected fallback event: %+v", rec)
		}
	}
}

// TestRoleFallbackUsesParentClient covers the degradation path: a role that
// cannot be resolved must not fail the task, and must be visible in the log.
func TestRoleFallbackUsesParentClient(t *testing.T) {
	repo := gitRepo(t)
	parentEP := newEndpoint(t, `{"status":"done","summary":"parent model ran"}`)
	emit, logs := captureLog()

	m := &Manager{Cfg: Config{
		Workspace: repo, Client: parentEP.client(t), SystemPrompt: "parent stack",
		Policy: tools.Policy{}, Timeout: 30 * time.Second, Max: 3,
		ResolveClient: func(role string) (*provider.Client, error) {
			return nil, fmt.Errorf("roles.researcher is not bound to a model")
		},
		Log: emit,
	}}
	out, err := m.RunTasks(context.Background(), []tools.TaskRequest{
		{Title: "research", Prompt: "dig into the logs", Role: "researcher"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []Result `json:"results"`
	}
	_ = json.Unmarshal([]byte(out), &report)
	if len(report.Results) != 1 || report.Results[0].Status != "done" {
		t.Fatalf("results = %+v", report.Results)
	}
	if parentEP.count() != 1 {
		t.Fatalf("parent endpoint calls = %d, want 1", parentEP.count())
	}

	found := false
	for _, rec := range logs() {
		if rec.kind != "subagent_role_fallback" {
			continue
		}
		fields, ok := rec.v.(map[string]any)
		if !ok {
			t.Fatalf("fallback payload = %#v", rec.v)
		}
		if fields["role"] != "researcher" {
			t.Fatalf("fallback role = %v", fields["role"])
		}
		if !strings.Contains(fmt.Sprint(fields["error"]), "researcher") {
			t.Fatalf("fallback error = %v", fields["error"])
		}
		found = true
	}
	if !found {
		t.Fatalf("no subagent_role_fallback event in log: %+v", logs())
	}
}

// TestNilClientFromResolverFallsBack covers a resolver that reports success but
// returns no client.
func TestNilClientFromResolverFallsBack(t *testing.T) {
	repo := gitRepo(t)
	parentEP := newEndpoint(t, `{"status":"done","summary":"ok"}`)
	emit, logs := captureLog()
	m := &Manager{Cfg: Config{
		Workspace: repo, Client: parentEP.client(t), Timeout: 30 * time.Second,
		ResolveClient: func(string) (*provider.Client, error) { return nil, nil },
		Log:           emit,
	}}
	if _, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "a", Prompt: "b", Role: "opsec"}}); err != nil {
		t.Fatal(err)
	}
	if parentEP.count() != 1 {
		t.Fatalf("parent endpoint calls = %d", parentEP.count())
	}
	if len(logs()) == 0 || logs()[0].kind != "subagent_role_fallback" {
		t.Fatalf("logs = %+v", logs())
	}
}

// TestWorkerExecutorReceivesMCP proves the worker executor carries the parent's
// MCP manager (shared, not owned) and that a nil manager disables it.
func TestWorkerExecutorReceivesMCP(t *testing.T) {
	mgr := &mcp.Manager{}
	ex := newWorkerExecutor(Config{MCP: mgr}, "/tmp/worktree", tools.Policy{Yolo: true})
	if ex.MCP != mgr {
		t.Fatal("worker executor did not receive the parent MCP manager")
	}
	if ex.Workspace != "/tmp/worktree" {
		t.Fatalf("workspace = %q", ex.Workspace)
	}
	if !ex.Policy.Yolo {
		t.Fatal("worker policy not applied")
	}
	bare := newWorkerExecutor(Config{}, "/tmp/worktree", tools.Policy{})
	if bare.MCP != nil {
		t.Fatal("nil config MCP produced a non-nil executor MCP")
	}
	if bare.Workspace != "/tmp/worktree" {
		t.Fatalf("workspace = %q", bare.Workspace)
	}
}

// ---------- M8.5.5 per-worker context bundles ----------

func sampleBundle() ContextBundle {
	return ContextBundle{
		Authorization: "Authorized dual-use R&D in an isolated local sandbox.",
		Domain:        "AgentForge is a local-first coding harness.",
		Scope:         "Defensive work only; no egress by default.",
		PriorWork: []PriorWorkRef{
			{
				ID: "L-7", Date: "2026-02-01", Kind: "plan",
				Summary:  "Operator accepted the worker bundle plan.",
				Evidence: []string{"sha256:aaa", "sha256:bbb"},
			},
		},
		KB:      []KBExcerpt{{ID: "kb-1", Title: "Prompt layers", Body: "Layers 0-5 are cached; layer 5 stays static."}},
		Handoff: "Keep edits inside internal/subagent.",
	}
}

// TestContextBundleHasNoRefusalField proves the invariant by construction: the
// bundle types have no field that could carry refusal text or any other
// negative outcome into a worker.
func TestContextBundleHasNoRefusalField(t *testing.T) {
	for _, typ := range []any{ContextBundle{}, PriorWorkRef{}, KBExcerpt{}} {
		v := reflect.ValueOf(typ)
		for i := 0; i < v.NumField(); i++ {
			name := strings.ToLower(v.Type().Field(i).Name)
			for _, banned := range []string{"refus", "denial", "decline", "blocked", "reject", "negative"} {
				if strings.Contains(name, banned) {
					t.Fatalf("%T.%s looks like a refusal field; a refusal is never memory",
						typ, v.Type().Field(i).Name)
				}
			}
		}
	}
	if !(ContextBundle{}).Empty() {
		t.Fatal("zero bundle is not empty")
	}
	if got := RenderBundle(ContextBundle{}, 600); got != "" {
		t.Fatalf("empty bundle rendered %q", got)
	}
}

func TestRenderBundleSectionsAndOrder(t *testing.T) {
	out := RenderBundle(sampleBundle(), 600)
	if out == "" {
		t.Fatal("bundle rendered empty")
	}
	if !strings.Contains(out, BundleFraming) {
		t.Fatalf("framing line missing:\n%s", out)
	}
	if !strings.Contains(out, "context, not permission") || !strings.Contains(out, "its absence is not evidence of refusal") {
		t.Fatalf("framing wording changed:\n%s", out)
	}
	order := []string{
		"## Worker context bundle",
		"### Authorization",
		"### Domain",
		"### Scope",
		"## Prior accepted work",
		"## Knowledge",
		"## Handoff",
	}
	last := -1
	for _, want := range order {
		idx := strings.Index(out, want)
		if idx < 0 {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
		if idx < last {
			t.Fatalf("%q is out of order in:\n%s", want, out)
		}
		last = idx
	}
	for _, want := range []string{
		"Authorized dual-use R&D", "local-first coding harness", "Defensive work only",
		"2026-02-01 plan L-7 — Operator accepted the worker bundle plan.",
		"evidence: sha256:aaa, sha256:bbb",
		"Prompt layers (kb-1)", "Layers 0-5 are cached",
		"Keep edits inside internal/subagent",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Deterministic: the same bundle renders identically.
	if again := RenderBundle(sampleBundle(), 600); again != out {
		t.Fatal("RenderBundle is not deterministic")
	}
}

func TestRenderBundleOmitsEmptySections(t *testing.T) {
	out := RenderBundle(ContextBundle{Handoff: "only a handoff"}, 600)
	if !strings.Contains(out, "## Worker context bundle") || !strings.Contains(out, "## Handoff") {
		t.Fatalf("out = %q", out)
	}
	for _, absent := range []string{"### Authorization", "### Domain", "### Scope", "## Prior accepted work", "## Knowledge"} {
		if strings.Contains(out, absent) {
			t.Fatalf("%q rendered for an empty section:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, BundleFraming) {
		t.Fatalf("framing missing:\n%s", out)
	}
}

func TestRenderBundleTokenCap(t *testing.T) {
	big := ContextBundle{
		Authorization: strings.Repeat("- authorization line of project context\n", 400),
		Domain:        strings.Repeat("- domain line\n", 400),
		Scope:         strings.Repeat("- scope line\n", 400),
		PriorWork: []PriorWorkRef{
			{ID: "L-1", Summary: strings.Repeat("accepted work ", 200)},
			{ID: "L-2", Summary: strings.Repeat("accepted work ", 200)},
			{ID: "L-3", Summary: strings.Repeat("accepted work ", 200)},
			{ID: "L-4", Summary: strings.Repeat("accepted work ", 200)},
			{ID: "L-5", Summary: strings.Repeat("accepted work ", 200)},
			{ID: "L-6", Summary: strings.Repeat("accepted work ", 200)},
		},
		KB:      []KBExcerpt{{ID: "k", Title: "t", Body: strings.Repeat("chunk body ", 500)}},
		Handoff: strings.Repeat("handoff line\n", 400),
	}
	for _, cap := range []int{600, 120, 30} {
		out := RenderBundle(big, cap)
		if len(out) > cap*4 {
			t.Fatalf("cap %d: rendered %d bytes, want <= %d", cap, len(out), cap*4)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("cap %d: truncation split a rune", cap)
		}
	}
	// A normal cap keeps the frame that makes the bundle safe and stays bounded.
	out := RenderBundle(big, 600)
	if !strings.Contains(out, BundleFraming) || !strings.Contains(out, "### Authorization") {
		t.Fatalf("bounded bundle lost its frame:\n%.400s", out)
	}
	// Zero means the default cap, not "unlimited".
	if len(RenderBundle(big, 0)) > BundleTokensDefault*4 {
		t.Fatal("zero cap did not apply the default")
	}
}

func TestRenderBundleCapsEntries(t *testing.T) {
	b := ContextBundle{}
	for i := 0; i < BundleMaxEntries+3; i++ {
		b.PriorWork = append(b.PriorWork, PriorWorkRef{ID: fmt.Sprintf("L-%d", i)})
		b.KB = append(b.KB, KBExcerpt{ID: fmt.Sprintf("k-%d", i), Body: "body"})
	}
	out := RenderBundle(b, 600)
	if n := strings.Count(out, "- L-"); n > BundleMaxEntries {
		t.Fatalf("prior work entries = %d, want <= %d\n%s", n, BundleMaxEntries, out)
	}
	if strings.Contains(out, "L-5") || strings.Contains(out, "k-5") {
		t.Fatalf("entry beyond the cap rendered:\n%s", out)
	}
}

func TestWorkerSystemPromptComposition(t *testing.T) {
	req := tools.TaskRequest{Title: "build fixture", Prompt: "make A", Agent: "researcher"}
	m := &Manager{Cfg: Config{SystemPrompt: "parent stack", ContextBundle: sampleBundle()}}
	got := m.workerSystem(req, "researcher")
	if !strings.HasPrefix(got, "parent stack\n\n## Worker context bundle") {
		t.Fatalf("bundle is not directly after the parent stack:\n%.200s", got)
	}
	for _, want := range []string{"parent stack", BundleFraming, "### Authorization", "## Prior accepted work", workerContract, "Subagent assignment: build fixture", "Worker role: researcher", "Model role: researcher"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in worker system prompt:\n%s", want, got)
		}
	}
	if strings.Index(got, "## Worker context bundle") > strings.Index(got, "bounded subagent") {
		t.Fatalf("bundle must precede the worker contract:\n%s", got)
	}
}

// TestNilBundleKeepsWorkerPromptIdentical pins the pre-M8.5.5 prompt byte for
// byte: a manager without bundles must behave exactly as before.
func TestNilBundleKeepsWorkerPromptIdentical(t *testing.T) {
	req := tools.TaskRequest{Title: "t", Prompt: "p", Agent: "coder"}
	want := strings.TrimSpace("parent stack") + "\n\n" + workerContract +
		"\n\nSubagent assignment: t\nWorker role: coder"
	m := &Manager{Cfg: Config{SystemPrompt: "parent stack"}}
	if got := m.workerSystem(req, DefaultRole); got != want {
		t.Fatalf("nil bundle changed the worker prompt:\ngot  %q\nwant %q", got, want)
	}
	// A role line is still appended exactly as before when the role is not the
	// default coder role.
	if got := m.workerSystem(req, "researcher"); got != want+"\nModel role: researcher" {
		t.Fatalf("role line changed:\n%q", got)
	}
	// The empty-prompt case is preserved too (it deliberately keeps the leading
	// blank line).
	bare := (&Manager{}).workerSystem(tools.TaskRequest{Title: "t"}, DefaultRole)
	if bare != "\n\n"+workerContract+"\n\nSubagent assignment: t" {
		t.Fatalf("empty parent prompt changed: %q", bare)
	}
}

func TestBundleForResolvesPerTaskAndRole(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	m := &Manager{Cfg: Config{
		SystemPrompt: "parent",
		BundleFor: func(task, role string) ContextBundle {
			mu.Lock()
			seen = append(seen, task+"/"+role)
			mu.Unlock()
			return ContextBundle{Domain: "domain for " + task, Handoff: "role " + role}
		},
	}}
	a := m.workerSystem(tools.TaskRequest{Title: "alpha", Prompt: "p"}, "coder")
	b := m.workerSystem(tools.TaskRequest{Title: "beta", Prompt: "p"}, "researcher")
	if !strings.Contains(a, "domain for alpha") || !strings.Contains(a, "role coder") {
		t.Fatalf("alpha prompt:\n%s", a)
	}
	if !strings.Contains(b, "domain for beta") || !strings.Contains(b, "role researcher") {
		t.Fatalf("beta prompt:\n%s", b)
	}
	if strings.Contains(a, "domain for beta") || strings.Contains(b, "domain for alpha") {
		t.Fatal("bundles leaked between tasks")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "alpha/coder" || seen[1] != "beta/researcher" {
		t.Fatalf("BundleFor calls = %v", seen)
	}
}

func TestTaskHandoffReachesWorkerPrompt(t *testing.T) {
	m := &Manager{Cfg: Config{SystemPrompt: "parent", ContextBundle: ContextBundle{Domain: "d", Handoff: "bundle handoff"}}}
	got := m.workerSystem(tools.TaskRequest{Title: "t", Context: "task handoff"}, DefaultRole)
	if !strings.Contains(got, "bundle handoff") || !strings.Contains(got, "task handoff") {
		t.Fatalf("handoffs missing:\n%s", got)
	}
	if strings.Index(got, "bundle handoff") > strings.Index(got, "task handoff") {
		t.Fatal("the task-level handoff must be the final word")
	}
	// A handoff alone is enough to produce a bundle.
	only := (&Manager{Cfg: Config{SystemPrompt: "parent"}}).workerSystem(
		tools.TaskRequest{Title: "t", Context: "only this"}, DefaultRole)
	if !strings.Contains(only, "## Handoff") || !strings.Contains(only, "only this") {
		t.Fatalf("task handoff alone did not render:\n%s", only)
	}
}

// TestBundleIsRedactedBeforeUse proves secrets never reach a worker prompt.
func TestBundleIsRedactedBeforeUse(t *testing.T) {
	secret := "sk-" + strings.Repeat("a", 24)
	m := &Manager{Cfg: Config{
		SystemPrompt:  "parent",
		ContextBundle: ContextBundle{Domain: "domain", Handoff: "token " + secret},
	}}
	req := tools.TaskRequest{Title: "t"}
	rendered := RenderBundle(m.bundleFor(req, DefaultRole), m.Cfg.BundleTokens)
	if !strings.Contains(rendered, secret) {
		t.Fatal("fixture secret was not in the raw bundle")
	}
	prompt := m.workerSystem(req, DefaultRole)
	if strings.Contains(prompt, secret) {
		t.Fatalf("secret reached the worker prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[REDACTED]") {
		t.Fatalf("redaction marker missing:\n%s", prompt)
	}
}

// TestBundleReachesWorkerRequest is the end-to-end path: a worker's model
// request must carry the rendered bundle, and a role-less manager must not grow
// a bundle section.
func TestBundleReachesWorkerRequest(t *testing.T) {
	repo := gitRepo(t)
	ep := newEndpoint(t, `{"status":"done","summary":"ok"}`)
	m := &Manager{Cfg: Config{
		Workspace: repo, Client: ep.client(t), SystemPrompt: "parent stack",
		Policy: tools.Policy{}, Timeout: 30 * time.Second, Max: 2,
		BundleFor: func(task, role string) ContextBundle {
			return ContextBundle{
				Authorization: "authorized sandbox work",
				Domain:        "domain for " + task,
				PriorWork:     []PriorWorkRef{{ID: "L-1", Date: "2026-02-02", Kind: "plan", Summary: "accepted plan for " + task}},
			}
		},
	}}
	if _, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "alpha", Prompt: "do alpha", Context: "handoff for alpha"}}); err != nil {
		t.Fatal(err)
	}
	body := ep.requestBodies()[0]
	for _, want := range []string{"parent stack", "## Worker context bundle", BundleFraming, "authorized sandbox work",
		"domain for alpha", "accepted plan for alpha", "## Handoff", "handoff for alpha", "bounded subagent"} {
		if !strings.Contains(body, want) {
			t.Fatalf("worker request missing %q:\n%s", want, body)
		}
	}
}

// ---------- M9.1 subagent lifecycle events ----------

// hookRunner builds a runner for one event whose hook writes its stdin to a
// file in dir.
func hookRunner(dir, event string) *hooks.Runner {
	r := hooks.New(map[string][]config.HookRule{
		event: {{Command: "sh", Args: []string{"-c", "cat > payload.json"}}},
	}, nil)
	r.Dir = dir
	return r
}

// readPayload reads the JSON object a hook wrote.
func readPayload(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "payload.json"))
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the hook payload is not one JSON object: %v (%s)", err, raw)
	}
	return out
}

// TestSubagentStartDenyStopsTheWorker proves the deny is a real gate: the
// worker never runs, and the parent gets a failed result that names the
// reason.
func TestSubagentStartDenyStopsTheWorker(t *testing.T) {
	repo := gitRepo(t)
	client, calls := mockClient(t, func(int) string { return `{"status":"done","summary":"ok"}` })

	r := hooks.New(map[string][]config.HookRule{
		hooks.SubagentStart: {{
			Command: "sh",
			Args:    []string{"-c", `printf '%s' '{"decision":"deny","reason":"workers are disabled in this repository"}'`},
		}},
	}, nil)

	m := &Manager{Cfg: Config{
		Workspace: repo, Client: client, Policy: tools.Policy{}, Timeout: 30 * time.Second,
		Hooks: r, SessionID: "S-1",
		ResolveModel: func(role string) string { return "mock/mock-chat" },
	}}

	out, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "build A", Prompt: "do it"}})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Fatalf("the worker reached the model despite the deny (%d calls)", *calls)
	}

	var report struct {
		Results []Result `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, out)
	}
	if len(report.Results) != 1 {
		t.Fatalf("results = %+v", report.Results)
	}
	res := report.Results[0]
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Error, "workers are disabled in this repository") {
		t.Fatalf("error = %q, want the deny reason", res.Error)
	}
	if res.Worktree != "" {
		t.Fatalf("a denied worker left a worktree: %q", res.Worktree)
	}

	// No worktree was created either.
	list, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(list), "worktree "); got != 1 {
		t.Fatalf("worktrees left behind (%d):\n%s", got, list)
	}
}

// TestSubagentStopFiresAfterTheWorker proves the stop event fires once the
// worker has finished, and that it carries the task, the role, the model and
// the worker's status.
func TestSubagentStopFiresAfterTheWorker(t *testing.T) {
	repo := gitRepo(t)
	ep := newEndpoint(t, `{"status":"done","summary":"worker done"}`)
	dir := t.TempDir()

	m := &Manager{Cfg: Config{
		Workspace: repo, Client: ep.client(t), Policy: tools.Policy{}, Timeout: 30 * time.Second,
		Hooks: hookRunner(dir, hooks.SubagentStop), SessionID: "S-2",
		ResolveModel: func(role string) string { return "mock/mock-chat" },
	}}

	if _, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "build B", Prompt: "do it"}}); err != nil {
		t.Fatal(err)
	}

	payload := readPayload(t, dir)
	if payload["event"] != hooks.SubagentStop {
		t.Fatalf("event = %v, want %s", payload["event"], hooks.SubagentStop)
	}
	if payload["title"] != "build B" {
		t.Fatalf("title = %v", payload["title"])
	}
	if payload["role"] != DefaultRole {
		t.Fatalf("role = %v, want %s", payload["role"], DefaultRole)
	}
	if payload["model"] != "mock/mock-chat" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["status"] != "done" {
		t.Fatalf("status = %v, want the worker's status", payload["status"])
	}
	if payload["session_id"] != "S-2" {
		t.Fatalf("session_id = %v", payload["session_id"])
	}
}

// TestSubagentEventsCarryTheTaskRoleAndModel proves the start event carries
// the same metadata the stop event does.
func TestSubagentEventsCarryTheTaskRoleAndModel(t *testing.T) {
	repo := gitRepo(t)
	ep := newEndpoint(t, `{"status":"done","summary":"ok"}`)
	dir := t.TempDir()

	m := &Manager{Cfg: Config{
		Workspace: repo, Client: ep.client(t), Policy: tools.Policy{}, Timeout: 30 * time.Second,
		Hooks: hookRunner(dir, hooks.SubagentStart), SessionID: "S-3",
		ResolveModel: func(role string) string { return "mock/mock-chat" },
	}}

	if _, err := m.RunTasks(context.Background(), []tools.TaskRequest{
		{Title: "research C", Prompt: "look", Role: "researcher"},
	}); err != nil {
		t.Fatal(err)
	}
	payload := readPayload(t, dir)
	if payload["event"] != hooks.SubagentStart {
		t.Fatalf("event = %v", payload["event"])
	}
	if payload["title"] != "research C" {
		t.Fatalf("title = %v", payload["title"])
	}
	if payload["role"] != "researcher" {
		t.Fatalf("role = %v", payload["role"])
	}
	if payload["model"] != "mock/mock-chat" {
		t.Fatalf("model = %v", payload["model"])
	}
}

// TestNoHooksMeansNoEvents proves a manager without a runner behaves exactly
// as it did before hooks existed.
func TestNoHooksMeansNoEvents(t *testing.T) {
	repo := gitRepo(t)
	ep := newEndpoint(t, `{"status":"done","summary":"ok"}`)
	m := &Manager{Cfg: Config{
		Workspace: repo, Client: ep.client(t), Policy: tools.Policy{}, Timeout: 30 * time.Second,
	}}
	if _, err := m.RunTasks(context.Background(), []tools.TaskRequest{{Title: "plain", Prompt: "go"}}); err != nil {
		t.Fatal(err)
	}
	if ep.count() != 1 {
		t.Fatalf("model calls = %d, want 1", ep.count())
	}
}
