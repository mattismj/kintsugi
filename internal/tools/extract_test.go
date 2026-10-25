package tools

import (
	"context"
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
	"kintsugi/internal/scope"
)

// ---------- fixtures ----------

// extractFixture reads one page fixture. The fixtures are the shapes the
// reducers are written against, not scraped pages: each carries the markup a
// site actually renders for the part that matters, plus the chrome around it
// (sign-in links, badge pixels, search boxes) that a reduction must drop.
func extractFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "extract", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// withSiteHosts points a reducer at hosts for the duration of the test, the
// way withSearchEndpoints points the search backends at local servers. The
// dispatch table is a package variable for exactly this reason.
func withSiteHosts(t *testing.T, site string, hosts ...string) {
	t.Helper()
	old := siteHosts
	siteHosts = append(append([]siteHostRule{}, old...), siteHostRule{site: site, hosts: hosts})
	t.Cleanup(func() { siteHosts = old })
}

// hostOf is a local server's host:port, the form the dispatch table matches
// exactly when a test registers one.
func hostOf(rawURL string) string {
	return strings.TrimPrefix(rawURL, "http://")
}

// ---------- dispatch ----------

func TestSiteOfDispatch(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"github.com", siteGitHub},
		{"www.github.com", siteGitHub},
		{"GIST.GITHUB.COM", siteGitHub}, // case and subdomains inherit
		{"github.com:8443", siteGitHub}, // the port is not part of the host
		{"evil-github.com", ""},         // a lookalike is not GitHub
		{"github.com.example", ""},      // neither is a suffix match
		{"arxiv.org", siteArxiv},
		{"export.arxiv.org", siteArxiv},
		{"stackoverflow.com", siteStackOverflow},
		{"security.stackexchange.com", siteStackOverflow},
		{"serverfault.com", siteStackOverflow},
		{"docs.example.com", siteDocs},        // a documentation label names the site
		{"kintsugi.readthedocs.io", siteDocs}, // ... as does a known docs host
		{"blog.example.org", ""},              // an arbitrary host stays generic
		{"docshop.example.com", ""},           // and a label that only starts like one
		{"127.0.0.1", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := SiteOf(tc.host); got != tc.want {
			t.Errorf("SiteOf(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestExtractDispatchesPerSite(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		fixture string
		site    string
	}{
		{"github repo", "github.com", "github_repo.html", siteGitHub},
		{"github blob", "github.com", "github_blob.html", siteGitHub},
		{"arxiv abs", "arxiv.org", "arxiv_abs.html", siteArxiv},
		{"arxiv html", "arxiv.org", "arxiv_html.html", siteArxiv},
		{"stack overflow", "stackoverflow.com", "stackoverflow.html", siteStackOverflow},
		{"generic docs", "docs.example.com", "docs.html", siteDocs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page := extractFixture(t, tc.fixture)
			site, markdown, citations, _ := Extract.reduce(tc.host, "", page)
			if site != tc.site {
				t.Fatalf("site = %q, want %q", site, tc.site)
			}
			if strings.TrimSpace(markdown) == "" {
				t.Fatal("the reducer produced nothing")
			}
			if len(citations) == 0 {
				t.Fatal("the reducer kept no citations")
			}
			if len(markdown) > ExtractCap {
				t.Fatalf("markdown is %d bytes, want at most %d", len(markdown), ExtractCap)
			}
		})
	}
}

// ---------- GitHub ----------

func TestExtractGitHubKeepsTreeReadmeAndLinks(t *testing.T) {
	markdown, citations := Extract.For("github.com", extractFixture(t, "github_repo.html"))

	for _, want := range []string{
		"# operator/kintsugi", // the repo, not the page's chrome
		"## Files",
		"- internal/",
		"- internal/tools/",
		"- go.mod",
		"- README.md",
		"## README",
		"# kintsugi",
		"## Install",
		"```",                       // the code block survived as a fence
		"go install ./cmd/kintsugi", // ... with its contents
		"- bounded fetches",         // the list survived
		"**Warning:**",              // emphasis survived
		"[design notes](https://example.com/design)",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "other-repo") {
		t.Fatalf("another repository's paths leaked into the tree:\n%s", markdown)
	}
	if strings.Contains(markdown, "Sign in") || strings.Contains(markdown, "Settings") {
		t.Fatalf("the page chrome survived:\n%s", markdown)
	}
	if !strings.Contains(markdown, "## Citations") {
		t.Fatalf("the citation list is missing:\n%s", markdown)
	}

	joined := strings.Join(citations, "\n")
	if !strings.Contains(joined, "https://example.com/design") {
		t.Fatalf("the README's outbound link was not kept as a citation: %v", citations)
	}
	if !strings.Contains(joined, "https://github.com/operator/kintsugi") {
		t.Fatalf("the page's own address was not kept: %v", citations)
	}
	if strings.Contains(joined, "/login") || strings.Contains(joined, "/settings") {
		t.Fatalf("navigation was kept as a citation: %v", citations)
	}
	if strings.Contains(joined, "camo.githubusercontent.com") {
		t.Fatalf("a badge pixel was kept as a citation: %v", citations)
	}
}

func TestExtractGitHubBlobKeepsTheCodeWithoutLineNumbers(t *testing.T) {
	markdown, citations := Extract.For("github.com", extractFixture(t, "github_blob.html"))

	if !strings.Contains(markdown, "## Source") {
		t.Fatalf("the code view was not kept:\n%s", markdown)
	}
	if !strings.Contains(markdown, "```go") {
		t.Fatalf("the fence does not name the language:\n%s", markdown)
	}
	if !strings.Contains(markdown, "package tools") ||
		!strings.Contains(markdown, `Result{Content: "bounded"}`) {
		t.Fatalf("the source was not kept:\n%s", markdown)
	}
	// The line-number column is chrome: it must not appear as source.
	if strings.Contains(markdown, "js-line-number") || strings.Contains(markdown, "\n1\n") {
		t.Fatalf("line numbers leaked into the code:\n%s", markdown)
	}
	for _, want := range []string{"path: internal/tools/web.go", "ref: main"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, markdown)
		}
	}
	if !strings.Contains(markdown, "## README") {
		t.Fatalf("the blob page's README was dropped:\n%s", markdown)
	}
	if len(citations) == 0 || !strings.Contains(strings.Join(citations, "\n"), "https://example.com/bounds") {
		t.Fatalf("citations = %v, want the README's link", citations)
	}
}

// ---------- arXiv ----------

func TestExtractArxivKeepsAbstractAuthorsAndAnchors(t *testing.T) {
	markdown, citations := Extract.For("arxiv.org", extractFixture(t, "arxiv_abs.html"))

	for _, want := range []string{
		"# Refusal-resistant local agents",
		"Authors: Ada Jones, Bea Smith",
		"arXiv: 2401.00042v2",
		"DOI: 10.1000/xyz123",
		"Subjects: cs.CR; cs.AI",
		"Comments: 14 pages, 3 figures",
		"## Abstract",
		"continuity beats prompt tricks",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Title:") || strings.Contains(markdown, "Abstract:") {
		t.Fatalf("the scraped field labels survived:\n%s", markdown)
	}

	joined := strings.Join(citations, "\n")
	for _, want := range []string{
		"https://arxiv.org/abs/2401.00042",
		"https://arxiv.org/pdf/2401.00042v2",
		"https://arxiv.org/a/jones_a_1",
		"https://doi.org/10.1000/xyz123",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("citation %q is missing: %v", want, citations)
		}
	}
	if strings.Contains(joined, "arxiv.org/help") || strings.Contains(joined, "/list/") {
		t.Fatalf("site navigation was kept as a citation: %v", citations)
	}
}

func TestExtractArxivKeepsTheSectionOutline(t *testing.T) {
	markdown, _ := Extract.For("arxiv.org", extractFixture(t, "arxiv_html.html"))

	if !strings.Contains(markdown, "## Sections") {
		t.Fatalf("the outline is missing:\n%s", markdown)
	}
	for _, want := range []string{"- 1 Introduction", "  - 1.1 Contributions", "- 2 Method"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, markdown)
		}
	}
	if !strings.Contains(markdown, "We study continuity") {
		t.Fatalf("the abstract was dropped:\n%s", markdown)
	}
}

// ---------- Stack Overflow ----------

func TestExtractStackOverflowKeepsQuestionAndTopAnswerWithCode(t *testing.T) {
	markdown, citations := Extract.For("stackoverflow.com", extractFixture(t, "stackoverflow.html"))

	for _, want := range []string{
		"# How do I bound an HTTP fetch in Go",
		"score: 17",
		"answers: 2",
		"tags: go, http",
		"## Question",
		"cap the bytes",
		"## Top answer (accepted)",
		"io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))",
		"`defer resp.Body.Close()`",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, markdown)
		}
	}
	// The second answer is not the one that settled it: it stays out.
	if strings.Contains(markdown, "stalled server") {
		t.Fatalf("a lower answer was kept:\n%s", markdown)
	}
	if strings.Contains(markdown, "Log in") || strings.Contains(markdown, "Help") {
		t.Fatalf("the page chrome survived:\n%s", markdown)
	}
	if !strings.Contains(markdown, "```go") {
		t.Fatalf("the answer's code lost its fence:\n%s", markdown)
	}

	joined := strings.Join(citations, "\n")
	if !strings.Contains(joined, "https://pkg.go.dev/io") {
		t.Fatalf("the question's reference was not kept as a citation: %v", citations)
	}
	if !strings.Contains(joined, "https://stackoverflow.com/questions/12345") {
		t.Fatalf("the canonical question URL was not kept: %v", citations)
	}
	if strings.Contains(joined, "/users/login") || strings.Contains(joined, "/help/") {
		t.Fatalf("navigation was kept as a citation: %v", citations)
	}
}

// ---------- generic documentation ----------

func TestExtractDocsKeepsHeadingsParagraphsAndCode(t *testing.T) {
	markdown, citations := Extract.For("docs.example.com", extractFixture(t, "docs.html"))

	for _, want := range []string{
		"# Sandbox modes",
		"## readonly",
		"## workspace",
		"A sandbox binds what a command can touch.",
		"```toml",
		`mode = "workspace"`,
		"| value | meaning |",
		"| none | no egress |",
		"- a promised sandbox is never silently dropped",
		"> An unavailable backend refuses the command",
		"[network guide](https://docs.example.com/docs/network)",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Sign in") || strings.Contains(markdown, "Terms") {
		t.Fatalf("the page's navigation survived the reduction:\n%s", markdown)
	}

	joined := strings.Join(citations, "\n")
	if !strings.Contains(joined, "https://example.com/spec") {
		t.Fatalf("an outbound reference was dropped: %v", citations)
	}
	if strings.Contains(joined, "/search?") || strings.Contains(joined, "/login") || strings.Contains(joined, "/site/terms") {
		t.Fatalf("navigation was kept as a citation: %v", citations)
	}
}

// ---------- unknown hosts ----------

func TestExtractUnknownHostKeepsTheGenericReduction(t *testing.T) {
	// A page with no structure at all: an unknown host reads exactly the way
	// webfetch reads it today, plus the links it kept.
	page := `<html><head><title>Plain</title></head><body><p>Nothing here but prose and
		<a href="/next">a link</a>.</p></body></html>`

	site, markdown, citations, _ := Extract.reduce("blog.example.org", "", page)
	if site != siteGeneric {
		t.Fatalf("site = %q, want %q", site, siteGeneric)
	}
	if markdown != htmlToMarkdown(page, "") {
		t.Fatalf("an unknown host was not reduced generically:\n%s", markdown)
	}
	if len(citations) != 1 || citations[0] != "https://blog.example.org/next" {
		t.Fatalf("citations = %v, want the one relative link resolved", citations)
	}
	// With the fetched page's own URL the generic reduction resolves the link
	// in the text too, not just in the citation list: an injected page must not
	// carry a bare "/next" the reader cannot follow.
	_, withURL, _, _ := Extract.reduce("blog.example.org", "https://blog.example.org/current", page)
	if !strings.Contains(withURL, "[a link](https://blog.example.org/next)") {
		t.Fatalf("the relative href was not resolved against the page URL:\n%s", withURL)
	}
}

func TestExtractUnstructuredKnownHostFallsBack(t *testing.T) {
	// A known host whose page carries nothing the reducer recognises falls
	// back rather than inventing structure.
	page := `<html><body><p>a bare page</p></body></html>`
	site, markdown, _, _ := Extract.reduce("github.com", "", page)
	if site == siteGitHub {
		t.Fatalf("the GitHub reducer claimed a page with nothing to keep: %q", site)
	}
	if !strings.Contains(markdown, "a bare page") {
		t.Fatalf("the fallback dropped the prose:\n%s", markdown)
	}
}

// ---------- citations ----------

func TestExtractCitationsAreAbsoluteDedupedAndCapped(t *testing.T) {
	var links strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&links, `<p><a href="/p/%d">p %d</a> <a href="/p/%d">again</a>
			<a href="mailto:x@example.com">mail</a> <a href="#top">top</a>
			<a href="https://example.com/logo.png">badge</a></p>`, i, i, i)
	}
	page := fmt.Sprintf(`<html><body><h1>Many links</h1>%s</body></html>`, links.String())

	_, citations := Extract.For("example.com", page)
	if len(citations) != extractMaxCitations {
		t.Fatalf("citations = %d, want the cap of %d", len(citations), extractMaxCitations)
	}
	seen := map[string]bool{}
	for _, c := range citations {
		if !strings.HasPrefix(c, "http://") && !strings.HasPrefix(c, "https://") {
			t.Fatalf("citation %q is not absolute", c)
		}
		if strings.Contains(c, "mailto:") || strings.Contains(c, "#top") || strings.HasSuffix(c, ".png") {
			t.Fatalf("citation %q should have been dropped", c)
		}
		if seen[c] {
			t.Fatalf("citation %q appears twice", c)
		}
		seen[c] = true
	}
}

func TestResolveCitation(t *testing.T) {
	tests := []struct {
		host string
		href string
		want string
	}{
		{"example.com", "/a/b", "https://example.com/a/b"},
		{"example.com:8080", "docs/x", "https://example.com:8080/docs/x"},
		{"example.com", "//cdn.example.com/f", "https://cdn.example.com/f"},
		{"example.com", "https://other.example/g?x=1#frag", "https://other.example/g?x=1"},
		{"example.com", "/login", ""},
		{"example.com", "/search?q=x", ""},
		{"example.com", "/img/a.png", ""},
		{"example.com", "mailto:a@b.c", ""},
		{"example.com", "javascript:void(0)", ""},
		{"example.com", "#section", ""},
		{"example.com", "", ""},
		{"", "/a/b", ""},
	}
	for _, tc := range tests {
		if got := resolveCitation(tc.host, tc.href); got != tc.want {
			t.Errorf("resolveCitation(%q, %q) = %q, want %q", tc.host, tc.href, got, tc.want)
		}
	}
}

// ---------- cap + marker ----------

func TestExtractIsCappedWithATruncationMarker(t *testing.T) {
	block := strings.Repeat("x", extractCodeCap-100)
	var blocks strings.Builder
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&blocks, "<h2>Section %d</h2><pre><code>%s</code></pre>", i, block)
	}
	page := fmt.Sprintf(`<html><body><h1>Big</h1>%s</body></html>`, blocks.String())

	markdown, _ := Extract.For("example.com", page)
	if len(markdown) > ExtractCap {
		t.Fatalf("markdown is %d bytes, want at most %d", len(markdown), ExtractCap)
	}
	want := fmt.Sprintf("…[truncated at %d bytes]", ExtractCap)
	if !strings.HasSuffix(markdown, want) {
		tail := markdown
		if len(tail) > 120 {
			tail = tail[len(tail)-120:]
		}
		t.Fatalf("the truncation marker is missing; tail:\n%s", tail)
	}
	// The head of the reduction is intact: the cut is at the end, not a
	// window into the middle.
	if !strings.HasPrefix(markdown, "# Big") {
		t.Fatalf("the head was cut too:\n%.120s", markdown)
	}
}

func TestCapExtractionRespectsItsLimit(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		cut   bool
	}{
		{"short", "hello", 100, false},
		{"exact", strings.Repeat("a", 100), 100, false},
		{"over", strings.Repeat("a", 500), 100, true},
		{"zero means no cap", strings.Repeat("a", 500), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := capExtraction(tc.text, tc.limit)
			if truncated != tc.cut {
				t.Fatalf("truncated = %v, want %v", truncated, tc.cut)
			}
			if tc.limit > 0 && len(got) > tc.limit {
				t.Fatalf("%d bytes, want at most %d", len(got), tc.limit)
			}
			if tc.cut && !strings.Contains(got, "…[truncated at") {
				t.Fatalf("the marker is missing: %q", got)
			}
		})
	}
}

// ---------- FetchPage ----------

func TestFetchPageOfflineGuardRefusesBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, extractFixture(t, "docs.html"))
	}))
	defer srv.Close()
	withSiteHosts(t, siteDocs, hostOf(srv.URL))

	ex := &Executor{Policy: Policy{Yolo: true}, Offline: true}
	if _, err := ex.FetchPage(context.Background(), "https://example.com/x", 0); err == nil {
		t.Fatal("a non-loopback host was fetched offline")
	} else if !strings.Contains(err.Error(), "offline mode") {
		t.Fatalf("err = %v, want the offline refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("a request was attempted before the refusal (%d hits)", hits.Load())
	}

	// A loopback host stays reachable offline, exactly as webfetch allows.
	page, err := ex.FetchPage(context.Background(), srv.URL+"/docs", 0)
	if err != nil {
		t.Fatalf("loopback fetch failed: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("loopback fetch never reached the server (%d hits)", hits.Load())
	}
	if !page.Extracted || page.Site != siteDocs {
		t.Fatalf("page = %+v, want the docs reducer", page)
	}
	if !strings.Contains(page.Markdown, "# Sandbox modes") {
		t.Fatalf("markdown = %.200s", page.Markdown)
	}
}

func TestFetchPageGuardsMatchWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h1>ok</h1></body></html>")
	}))
	defer srv.Close()

	tests := []struct {
		name   string
		ex     *Executor
		target string
		want   string
	}{
		{
			name:   "a disallowed scheme",
			ex:     &Executor{Policy: Policy{Yolo: true}},
			target: "file:///etc/passwd",
			want:   "not allowed",
		},
		{
			name:   "credentials in the URL",
			ex:     &Executor{Policy: Policy{Yolo: true}},
			target: "http://user:pass@example.com/",
			want:   "credentials",
		},
		{
			name:   "no approver attached",
			ex:     &Executor{},
			target: srv.URL,
			want:   "requires approval",
		},
		{
			name: "denied by the matrix",
			ex: &Executor{Policy: Policy{Rules: []config.PermRule{
				{Tool: "webfetch", Action: "deny"},
			}}},
			target: srv.URL,
			want:   "denied by policy",
		},
		{
			name:   "outside the declared scope",
			ex:     &Executor{Policy: Policy{Yolo: true}, Scope: scope.Rules{Hosts: []string{"allowed.example"}}},
			target: srv.URL,
			want:   "out of scope",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ex.FetchPage(context.Background(), tc.target, 0)
			if err == nil {
				t.Fatalf("%s was not refused", tc.target)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchPageAsksForEgressTheWayWebFetchDoes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, extractFixture(t, "docs.html"))
	}))
	defer srv.Close()

	var askedTool, askedDetail string
	ex := &Executor{
		Policy: Policy{}, // default mode: an egress is asked about
		Approver: ApproverFunc(func(ar ApprovalRequest) bool {
			askedTool, askedDetail = ar.Tool, ar.Detail
			return true
		}),
	}
	page, err := ex.FetchPage(context.Background(), srv.URL+"/sandbox", 0)
	if err != nil {
		t.Fatalf("an approved fetch failed: %v", err)
	}
	if askedTool != "webfetch" {
		t.Fatalf("the permission was asked for %q, want webfetch", askedTool)
	}
	if !strings.Contains(askedDetail, srv.URL) {
		t.Fatalf("the approval prompt does not name the URL: %q", askedDetail)
	}
	if page.Status != 200 || !strings.Contains(page.Markdown, "# Sandbox modes") {
		t.Fatalf("page = %+v", page)
	}
}

func TestFetchPageAppliesTheSiteReducerForItsHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, extractFixture(t, "github_repo.html"))
	}))
	defer srv.Close()
	withSiteHosts(t, siteGitHub, hostOf(srv.URL))

	ex := &Executor{Policy: Policy{Yolo: true}, Offline: true}
	page, err := ex.FetchPage(context.Background(), srv.URL+"/operator/kintsugi", 0)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if page.Site != siteGitHub || !page.Extracted {
		t.Fatalf("page = %+v, want the github reducer", page)
	}
	if !strings.Contains(page.Markdown, "## Files") || !strings.Contains(page.Markdown, "## README") {
		t.Fatalf("markdown = %.400s", page.Markdown)
	}
	if len(page.Citations) == 0 {
		t.Fatal("no citations were kept")
	}
	if page.ContentType != "text/html" || page.Bytes == 0 {
		t.Fatalf("page = %+v", page)
	}
}

func TestFetchPageLeavesNonHTMLAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"a":1,"b":[true,false]}`)
	}))
	defer srv.Close()

	ex := &Executor{Policy: Policy{Yolo: true}}
	page, err := ex.FetchPage(context.Background(), srv.URL+"/api", 0)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if page.Extracted {
		t.Fatalf("a JSON body was claimed by a reducer: %+v", page)
	}
	if page.Site != siteGeneric || !strings.Contains(page.Markdown, `"a": 1`) {
		t.Fatalf("page = %+v", page)
	}
}

func TestFetchPageReportsAnHTTPErrorAndACap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h1>Big</h1>"+strings.Repeat("<p>word</p>", 4000)+"</body></html>")
	}))
	defer srv.Close()

	ex := &Executor{Policy: Policy{Yolo: true}}
	if _, err := ex.FetchPage(context.Background(), srv.URL+"/gone", 0); err == nil ||
		!strings.Contains(err.Error(), "status 404") {
		t.Fatalf("err = %v, want the status", err)
	}

	page, err := ex.FetchPage(context.Background(), srv.URL+"/big", 4096)
	if err != nil {
		t.Fatalf("an over-cap body is still reducible: %v", err)
	}
	if !page.Truncated {
		t.Fatalf("the download cap was not reported: %+v", page)
	}
	if page.Bytes != 4096 {
		t.Fatalf("bytes = %d, want the cap", page.Bytes)
	}
	if len(page.Markdown) > ExtractCap {
		t.Fatalf("markdown = %d bytes, want at most %d", len(page.Markdown), ExtractCap)
	}
}

// ---------- the websearch hook ----------

// searchResultForExtraction decodes a websearch answer.
type searchResultForExtraction struct {
	Notes []string `json:"notes"`
	Hits  []struct {
		Title     string   `json:"title"`
		URL       string   `json:"url"`
		Snippet   string   `json:"snippet"`
		Site      string   `json:"site"`
		Extracted bool     `json:"extracted"`
		Extract   string   `json:"extract"`
		Citations []string `json:"citations"`
	} `json:"hits"`
}

func TestWebSearchHitsFromKnownHostsAreExtracted(t *testing.T) {
	var githubHits, otherHits atomic.Int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, extractFixture(t, "github_repo.html"))
	}))
	defer github.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, extractFixture(t, "docs.html"))
	}))
	defer other.Close()
	withSiteHosts(t, siteGitHub, hostOf(github.URL))

	results := fmt.Sprintf(`<html><body>
		<div class="result"><h2 class="result__title"><a class="result__a" href="%s/operator/kintsugi">A repo</a></h2>
		  <a class="result__snippet" href="%s/operator/kintsugi">A repository.</a></div>
		<div class="result"><h2 class="result__title"><a class="result__a" href="%s/unknown">Docs</a></h2>
		  <a class="result__snippet" href="%s/unknown">Some docs.</a></div>
	</body></html>`, github.URL, github.URL, other.URL, other.URL)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, results)
	}))
	defer backend.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, backend.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"kintsugi"}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	var body searchResultForExtraction
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, res.Content)
	}
	if len(body.Hits) != 2 {
		t.Fatalf("hits = %d, want 2: %s", len(body.Hits), res.Content)
	}

	known := body.Hits[0]
	if !known.Extracted {
		t.Fatalf("the known host's hit carries no extraction: %+v", known)
	}
	if known.Site != siteGitHub {
		t.Fatalf("site = %q, want %q", known.Site, siteGitHub)
	}
	if !strings.Contains(known.Extract, "## Files") {
		t.Fatalf("extract = %.300s", known.Extract)
	}
	if len(known.Citations) == 0 {
		t.Fatal("the extraction kept no citations")
	}
	if len(known.Extract) > webSearchExtractCap {
		t.Fatalf("extract = %d bytes, want at most %d", len(known.Extract), webSearchExtractCap)
	}
	if known.Snippet != "A repository." {
		t.Fatalf("the backend's snippet was replaced: %q", known.Snippet)
	}

	unknown := body.Hits[1]
	if unknown.Extracted || unknown.Extract != "" || unknown.Site != "" {
		t.Fatalf("an unknown host was claimed by a reducer: %+v", unknown)
	}
	if unknown.Snippet != "Some docs." {
		t.Fatalf("an unknown host lost its snippet: %+v", unknown)
	}
	if otherHits.Load() != 0 {
		t.Fatalf("an unknown host was fetched (%d hits)", otherHits.Load())
	}
	if githubHits.Load() != 1 {
		t.Fatalf("the known host was fetched %d times, want 1", githubHits.Load())
	}
	if joined := strings.Join(body.Notes, " | "); !strings.Contains(joined, "extraction applied to 1") {
		t.Fatalf("notes = %v, want the extraction note", body.Notes)
	}
}

func TestWebSearchExtractionIsBoundedToAFewHits(t *testing.T) {
	var fetches atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, extractFixture(t, "github_repo.html"))
	}))
	defer target.Close()
	withSiteHosts(t, siteGitHub, hostOf(target.URL))

	// Nine hits, all on the host a reducer owns: only a few are fetched,
	// because a search is not a crawl.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPageWithHost(target.URL, 9))
	}))
	defer backend.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, backend.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"x","limit":9}`)
	if res.Failed {
		t.Fatalf("websearch failed: %+v", res)
	}
	if got := fetches.Load(); got != webSearchExtractHits {
		t.Fatalf("fetched %d pages, want %d", got, webSearchExtractHits)
	}
	var body searchResultForExtraction
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, res.Content)
	}
	if n := extractedHits(toSearchHits(body)); n != webSearchExtractHits {
		t.Fatalf("%d hits carry an extraction, want %d", n, webSearchExtractHits)
	}
	if len(res.Content) > 64<<10 {
		t.Fatalf("the answer is %d bytes: a search must stay a search", len(res.Content))
	}
}

// ddgPageWithHost builds a keyless results page whose hits all point at one
// host, so an extraction test can count the fetches it caused.
func ddgPageWithHost(host string, n int) string {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<div class="result">
		  <h2 class="result__title"><a class="result__a" href="%s/repo%d">Hit %d</a></h2>
		  <a class="result__snippet" href="%s/repo%d">Snippet %d</a>
		</div>`, host, i, i, host, i, i)
	}
	b.WriteString("</body></html>")
	return b.String()
}

// toSearchHits turns a decoded answer back into hits for the shared counter.
func toSearchHits(body searchResultForExtraction) []searchHit {
	out := make([]searchHit, 0, len(body.Hits))
	for _, h := range body.Hits {
		out = append(out, searchHit{Title: h.Title, URL: h.URL, Extracted: h.Extracted})
	}
	return out
}

func TestWebSearchExtractionFailureKeepsTheSnippet(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	deadURL := dead.URL
	dead.Close() // the port is closed: nothing answers
	withSiteHosts(t, siteGitHub, hostOf(deadURL))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPageWithHost(deadURL, 1))
	}))
	defer backend.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, backend.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "websearch", `{"query":"x"}`)
	if res.Failed {
		t.Fatalf("a hit that cannot be fetched must not fail the search: %+v", res)
	}
	var body searchResultForExtraction
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, res.Content)
	}
	if len(body.Hits) != 1 {
		t.Fatalf("hits = %+v", body.Hits)
	}
	if body.Hits[0].Extracted {
		t.Fatalf("a dead page was reported as extracted: %+v", body.Hits[0])
	}
	if body.Hits[0].Snippet != "Snippet 0" {
		t.Fatalf("the snippet was lost: %+v", body.Hits[0])
	}
}

func TestWebSearchExtractionIsRefusedOffline(t *testing.T) {
	// The search backend is loopback, so an offline session may still search.
	// A hit that points at a known non-loopback host is not fetched: the
	// offline guard refuses the extraction's fetch before a byte is read, and
	// the hit keeps the snippet the backend returned.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ddgPageWithHost("https://github.com/operator/kintsugi", 1))
	}))
	defer backend.Close()

	ws := researchWorkspace(t, []string{"duckduckgo"})
	withSearchEndpoints(t, backend.URL+"/html/", "", "")
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, Offline: true}

	res := dispatch(t, ex, "websearch", `{"query":"x"}`)
	if res.Failed {
		t.Fatalf("a loopback search must work offline: %+v", res)
	}
	var body searchResultForExtraction
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, res.Content)
	}
	if len(body.Hits) != 1 || body.Hits[0].Extracted {
		t.Fatalf("an offline session extracted a page: %+v", body.Hits)
	}
	if body.Hits[0].Snippet != "Snippet 0" {
		t.Fatalf("the snippet was lost: %+v", body.Hits[0])
	}
	if _, err := ex.FetchPage(context.Background(), "https://github.com/x/y", 0); err == nil ||
		!strings.Contains(err.Error(), "offline mode") {
		t.Fatalf("err = %v, want the offline refusal", err)
	}
}
