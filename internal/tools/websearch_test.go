package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kintsugi/internal/config"
)

// researchWorkspace builds a workspace whose configuration names the given
// search backends. The operator-level configuration is pointed at an empty
// directory so the test sees only what it wrote itself.
func researchWorkspace(t *testing.T, backends []string) string {
	t.Helper()
	return researchWorkspaceTimeout(t, backends, 5)
}

// researchWorkspaceTimeout is researchWorkspace with an explicit request
// timeout, so a test that expects a dead host does not wait long.
func researchWorkspaceTimeout(t *testing.T, backends []string, timeoutS int) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KINTSUGI_CONFIG", "")
	ws := t.TempDir()
	var b strings.Builder
	b.WriteString("[research]\n")
	if len(backends) > 0 {
		b.WriteString("search_backends = [")
		for i, name := range backends {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", name)
		}
		b.WriteString("]\n")
	}
	fmt.Fprintf(&b, "timeout_s = %d\n", timeoutS)
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// withSearchEndpoints points the backend endpoints at local servers for the
// duration of the test.
func withSearchEndpoints(t *testing.T, ddg, brave, tavily string) {
	t.Helper()
	oldDDG, oldBrave, oldTavily := duckduckgoHTMLEndpoint, braveSearchEndpoint, tavilySearchEndpoint
	duckduckgoHTMLEndpoint, braveSearchEndpoint, tavilySearchEndpoint = ddg, brave, tavily
	t.Cleanup(func() {
		duckduckgoHTMLEndpoint, braveSearchEndpoint, tavilySearchEndpoint = oldDDG, oldBrave, oldTavily
	})
}

// searchResponse is the decoded body of a websearch result.
type searchResponse struct {
	Query   string  `json:"query"`
	Backend *string `json:"backend"`
	Hits    []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"hits"`
	Notes  []string `json:"notes"`
	Errors []struct {
		Backend string `json:"backend"`
		Error   string `json:"error"`
	} `json:"errors"`
}

func decodeSearch(t *testing.T, res Result) searchResponse {
	t.Helper()
	var out searchResponse
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, res.Content)
	}
	return out
}

// ddgPage builds a keyless results page with n rows.
func ddgPage(n int) string {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<div class="result results_links">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%%3A%%2F%%2Fexample.com%%2F%%23d&amp;rut=x">Title %d</a></h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%%3A%%2F%%2Fexample.com%%2F%%23d">Snippet %d</a>
</div>`, i, i)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func TestWebSearchParsesDuckDuckGoResults(t *testing.T) {
	page := `<html><body>
<div class="result results_links">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fone&amp;rut=abc">First <b>hit</b></a></h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fone">Snippet <b>one</b></a>
</div>
<div class="result results_links">
  <h2 class="result__title"><a class="result__a" href="https://example.org/two">Second hit</a></h2>
  <a class="result__snippet" href="https://example.org/two">Snippet two</a>
</div>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, srv.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"log4j"}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	body := decodeSearch(t, res)
	if body.Query != "log4j" {
		t.Fatalf("query = %q", body.Query)
	}
	if body.Backend == nil || *body.Backend != "duckduckgo" {
		t.Fatalf("backend = %v, want duckduckgo", body.Backend)
	}
	if len(body.Hits) != 2 {
		t.Fatalf("hits = %d, want 2: %+v", len(body.Hits), body.Hits)
	}
	if body.Hits[0].Title != "First hit" {
		t.Errorf("title = %q", body.Hits[0].Title)
	}
	if body.Hits[0].URL != "https://example.com/one" {
		t.Errorf("url = %q, want the redirect target", body.Hits[0].URL)
	}
	if body.Hits[0].Snippet != "Snippet one" {
		t.Errorf("snippet = %q", body.Hits[0].Snippet)
	}
	if body.Hits[1].URL != "https://example.org/two" {
		t.Errorf("second url = %q", body.Hits[1].URL)
	}
}

func TestWebSearchFallsThroughToNextBackend(t *testing.T) {
	var braveHits atomic.Int32
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend is down", http.StatusInternalServerError)
	}))
	defer ddg.Close()
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		braveHits.Add(1)
		if r.Header.Get("X-Subscription-Token") != "brave-secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[{"title":"Brave hit","url":"https://brave.example/1","description":"Brave description"}]}}`)
	}))
	defer brave.Close()

	t.Setenv("BRAVE_SEARCH_API_KEY", "brave-secret")
	ws := researchWorkspace(t, []string{"duckduckgo", "brave"})
	withSearchEndpoints(t, ddg.URL+"/html/", brave.URL+"/search", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"log4j"}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	body := decodeSearch(t, res)
	if body.Backend == nil || *body.Backend != "brave" {
		t.Fatalf("backend = %v, want brave", body.Backend)
	}
	if len(body.Hits) != 1 || body.Hits[0].Title != "Brave hit" {
		t.Fatalf("hits = %+v", body.Hits)
	}
	if body.Hits[0].Snippet != "Brave description" {
		t.Errorf("snippet = %q", body.Hits[0].Snippet)
	}
	if len(body.Errors) != 1 || body.Errors[0].Backend != "duckduckgo" {
		t.Fatalf("the failed backend was not reported: %+v", body.Errors)
	}
	if !strings.Contains(body.Errors[0].Error, "500") {
		t.Errorf("backend error = %q", body.Errors[0].Error)
	}
	if braveHits.Load() != 1 {
		t.Errorf("brave hits = %d, want 1", braveHits.Load())
	}
}

func TestWebSearchHonoursAndCapsLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPage(30))
	}))
	defer srv.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, srv.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"x","limit":2}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	if hits := decodeSearch(t, res).Hits; len(hits) != 2 {
		t.Fatalf("hits = %d, want the requested 2", len(hits))
	}

	res = dispatch(t, ex, "websearch", `{"query":"x","limit":100}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	if hits := decodeSearch(t, res).Hits; len(hits) != webSearchMaxLimit {
		t.Fatalf("hits = %d, want the cap of %d", len(hits), webSearchMaxLimit)
	}

	res = dispatch(t, ex, "websearch", `{"query":"x"}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	if hits := decodeSearch(t, res).Hits; len(hits) != webSearchDefaultLimit {
		t.Fatalf("hits = %d, want the default of %d", len(hits), webSearchDefaultLimit)
	}
}

func TestWebSearchEmptyResultsAreReportedHonestly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><div class="no-results">No results found for that query.</div></body></html>`)
	}))
	defer srv.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, srv.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"nothing here"}`)
	if res.Failed {
		t.Fatalf("an empty result set must not fail the turn: %+v", res)
	}
	body := decodeSearch(t, res)
	if len(body.Hits) != 0 {
		t.Fatalf("hits = %+v, want none", body.Hits)
	}
	if !strings.Contains(res.Content, `"hits":[]`) {
		t.Fatalf("an empty result set must be an empty list, not null: %s", res.Content)
	}
	if body.Backend == nil || *body.Backend != "duckduckgo" {
		t.Fatalf("backend = %v, want the backend that answered", body.Backend)
	}
	joined := strings.Join(body.Notes, " | ")
	if !strings.Contains(joined, "no results") {
		t.Fatalf("notes = %v, want an honest no-results note", body.Notes)
	}
}

func TestWebSearchAllBackendsFail(t *testing.T) {
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer ddg.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, ddg.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"x"}`)
	if !res.Failed {
		t.Fatalf("a broken chain must fail the turn: %+v", res)
	}
	body := decodeSearch(t, res)
	if body.Backend != nil {
		t.Fatalf("backend = %v, want null when nothing answered", *body.Backend)
	}
	if len(body.Errors) != 1 || body.Errors[0].Backend != "duckduckgo" {
		t.Fatalf("per-backend errors missing: %+v", body.Errors)
	}
}

func TestWebSearchOfflineGuardRefusesBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPage(1))
	}))
	defer srv.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	// A non-loopback host: the guard must refuse it before the request, so
	// the answer is the refusal and not a transport error.
	withSearchEndpoints(t, "http://192.0.2.1:9/html/", "", "")
	offline := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, Offline: true}

	res := dispatch(t, offline, "websearch", `{"query":"x"}`)
	if !res.Failed || !strings.Contains(res.Content, "offline mode") {
		t.Fatalf("offline guard: %+v", res)
	}
	if strings.Contains(res.Content, "dial tcp") || strings.Contains(res.Content, "192.0.2.1:9") {
		t.Fatalf("a request was attempted before the refusal: %s", res.Content)
	}

	// A loopback host stays reachable offline, exactly as webfetch allows.
	withSearchEndpoints(t, srv.URL+"/html/", "", "")
	res = dispatch(t, offline, "websearch", `{"query":"x"}`)
	if res.Failed {
		t.Fatalf("loopback should work offline: %+v", res)
	}
	if hits.Load() != 1 {
		t.Fatalf("loopback fetch never reached the server (%d hits)", hits.Load())
	}
}

func TestWebSearchRequiresApprovalAndHidesKeys(t *testing.T) {
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[{"title":"Brave hit","url":"https://brave.example/1","description":"d"}]}}`)
	}))
	defer brave.Close()

	t.Setenv("BRAVE_SEARCH_API_KEY", "brave-secret")
	ws := researchWorkspace(t, []string{"brave"})
	withSearchEndpoints(t, "", brave.URL+"/search", "")

	// No approver: the call is refused.
	ex := &Executor{Workspace: ws, Policy: Policy{}}
	res := dispatch(t, ex, "websearch", `{"query":"x"}`)
	if !res.Failed || !strings.Contains(res.Content, "requires approval") {
		t.Fatalf("approval guard: %+v", res)
	}

	// With an approver the call runs, and neither the answer nor the
	// approval prompt may carry the key.
	asked := 0
	var detail string
	ex2 := &Executor{
		Workspace: ws,
		Policy:    Policy{},
		Approver: ApproverFunc(func(ar ApprovalRequest) bool {
			asked++
			detail = ar.Detail
			return true
		}),
	}
	res = dispatch(t, ex2, "websearch", `{"query":"x"}`)
	if res.Failed {
		t.Fatalf("approved websearch failed: %+v", res)
	}
	if asked != 1 {
		t.Fatalf("approval asked %d times, want 1", asked)
	}
	if strings.Contains(res.Content, "brave-secret") {
		t.Fatalf("the key leaked into the result: %s", res.Content)
	}
	if strings.Contains(detail, "brave-secret") {
		t.Fatalf("the key leaked into the approval prompt: %q", detail)
	}
}

func TestWebSearchRunsWhenThePolicyAllowsIt(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPage(1))
	}))
	defer srv.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, srv.URL+"/html/", "", "")
	// The permission matrix allows the tool: no approval is needed.
	ex := &Executor{
		Workspace: ws,
		Policy: Policy{Rules: []config.PermRule{
			{Tool: "websearch", Action: "allow"},
		}},
	}
	res := dispatch(t, ex, "websearch", `{"query":"x"}`)
	if res.Failed {
		t.Fatalf("an allowed websearch failed: %+v", res)
	}
	if hits.Load() != 1 {
		t.Fatalf("the backend was never reached (%d hits)", hits.Load())
	}
}

func TestWebSearchHonoursTheDeclaredHostScope(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPage(1))
	}))
	defer srv.Close()

	// A scope that names another host: the backend is out of scope and is
	// never contacted.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KINTSUGI_CONFIG", "")
	ws := t.TempDir()
	cfg := "[research]\nsearch_backends = [\"duckduckgo\"]\ntimeout_s = 5\n[scope]\nhosts = [\"example.com\"]\n"
	if err := os.WriteFile(filepath.Join(ws, "kintsugi.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	withSearchEndpoints(t, srv.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"x"}`)
	if !res.Failed || !strings.Contains(res.Content, "outside the declared scope") {
		t.Fatalf("scope refusal: %+v", res)
	}
	if hits.Load() != 0 {
		t.Fatalf("an out-of-scope backend was contacted (%d hits)", hits.Load())
	}
}

func TestResearchToolsAreRegistered(t *testing.T) {
	ex := &Executor{Workspace: t.TempDir()}
	names := map[string]bool{}
	for _, d := range ex.Definitions() {
		names[d.Function.Name] = true
	}
	if !names["websearch"] {
		t.Error("websearch is not registered in Definitions()")
	}
	if !names["cve_lookup"] {
		t.Error("cve_lookup is not registered in Definitions()")
	}
}

func TestResearchToolAliasesMapToCanonicalNames(t *testing.T) {
	for alias, want := range map[string]string{
		"search":     "websearch",
		"web_search": "websearch",
		"websearch":  "websearch",
	} {
		rep, err := Repair(alias, `{"query":"x"}`)
		if err != nil {
			t.Fatalf("Repair(%q): %v", alias, err)
		}
		if rep.Tool != want {
			t.Errorf("Repair(%q).Tool = %q, want %q", alias, rep.Tool, want)
		}
	}
	for alias, want := range map[string]string{
		"lookup_cve": "cve_lookup",
		"cve":        "cve_lookup",
		"vuln":       "cve_lookup",
	} {
		rep, err := Repair(alias, `{"id":"CVE-2021-44228"}`)
		if err != nil {
			t.Fatalf("Repair(%q): %v", alias, err)
		}
		if rep.Tool != want {
			t.Errorf("Repair(%q).Tool = %q, want %q", alias, rep.Tool, want)
		}
	}

	// Argument aliases are defaulted too.
	rep, err := Repair("websearch", `{"q":"log4j"}`)
	if err != nil {
		t.Fatalf("Repair(websearch): %v", err)
	}
	if !strings.Contains(rep.Args, `"query":"log4j"`) {
		t.Errorf("args = %s, want the query alias applied", rep.Args)
	}
	rep, err = Repair("cve_lookup", `{"cve_id":"CVE-2021-44228"}`)
	if err != nil {
		t.Fatalf("Repair(cve_lookup): %v", err)
	}
	if !strings.Contains(rep.Args, `"id":"CVE-2021-44228"`) {
		t.Errorf("args = %s, want the id alias applied", rep.Args)
	}
}
