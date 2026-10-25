// The websearch tool: the research surface's search half (M8, IDEAS I-19).
//
// It is deliberately a thin, bounded client rather than a crawler: one query,
// one response per backend, a short list of hits, no cookies, no JavaScript,
// no crawling. The bounds are the ones webfetch already uses — a timeout, a
// small redirect cap, a response cap, a fixed user agent — and the offline
// guard and the permission gate are the same ones webfetch goes through.
//
// Three properties matter more than the feature set:
//
//   - Bounded. A search cannot become a download: the response is capped and
//     the hit list is capped.
//   - Honest. The answer names the backend that produced it. A backend that
//     fails is reported with its error, a backend that returns nothing is
//     reported as an empty result. Nothing is invented.
//   - Local-friendly. The first backend in the chain needs no API key, so a
//     local runner works with no account and no egress beyond the operator's
//     own approval.
//
// The chain is ordered by the [research] block of the configuration:
// search_backends = ["duckduckgo", "brave", "tavily"]. The first backend
// that answers wins; the next one is tried when a backend fails or returns
// nothing. The endpoints themselves are fixed in this file — the operator
// chooses which providers are reached, never where they live.

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
	"kintsugi/internal/redact"
	"kintsugi/internal/scope"
)

const (
	// webSearchDefaultLimit is the number of hits returned when the model
	// does not ask for a count.
	webSearchDefaultLimit = 5
	// webSearchMaxLimit is the hard cap on hits per call.
	webSearchMaxLimit = 20
	// webSearchMaxBytes caps one backend response.
	webSearchMaxBytes = 1 << 20 // 1 MiB
	// webSearchTimeout bounds one backend request when the configuration is
	// silent.
	webSearchTimeout = 15 * time.Second
	// researchRedirectCap is the number of redirects one research request may
	// follow (the same cap webfetch uses).
	researchRedirectCap = 5
)

// researchUserAgent is the same agent string webfetch sends: the harness
// identifies itself and nothing else.
const researchUserAgent = "kintsugi/0.1 (+https://localhost; local harness)"

// The backend endpoints. They are package variables so the tests can point
// the chain at a local server; the configuration chooses which backends are
// tried, not where they live, so a workspace file cannot redirect the
// harness to a host of its choosing.
var (
	duckduckgoHTMLEndpoint = "https://html.duckduckgo.com/html/"
	braveSearchEndpoint    = "https://api.search.brave.com/res/v1/web/search"
	tavilySearchEndpoint   = "https://api.tavily.com/search"
)

// searchHit is one result row: a title, the URL it points at and a short
// snippet. The extraction fields are filled by the site-aware reducers
// (extract.go) when the hit's host is one a reducer owns; a hit with no
// reducer applied carries the snippet alone.
type searchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	// Site names the reducer that produced Extract (github, arxiv, ...).
	Site string `json:"site,omitempty"`
	// Extracted is true when a site reducer applied to the hit's page.
	Extracted bool `json:"extracted,omitempty"`
	// Extract is the reduced markdown, bounded.
	Extract string `json:"extract,omitempty"`
	// Citations are the absolute link targets the reduction preserved.
	Citations []string `json:"citations,omitempty"`
}

// webSearchError records one backend's failure so a broken chain can be
// reported honestly instead of collapsing into a bare error.
type webSearchError struct {
	Backend string `json:"backend"`
	Error   string `json:"error"`
}

// webSearchResult is the tool's answer.
type webSearchResult struct {
	Query string `json:"query"`
	// Backend names the backend that answered. It is null when every
	// backend failed.
	Backend *string          `json:"backend"`
	Hits    []searchHit      `json:"hits"`
	Notes   []string         `json:"notes,omitempty"`
	Errors  []webSearchError `json:"errors,omitempty"`
}

// webSearchTool is the schema for the web-search tool.
func webSearchTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "websearch",
			Description: "Search the web and return a short list of hits (title, url, snippet). Hits from the sites this harness reads closely (GitHub, arXiv, Stack Overflow) also carry a site-aware markdown extraction and its citations. Backends are tried in the configured order and the result names the one that answered; a query that finds nothing returns an empty list rather than an invented answer. Network access requires operator approval and is blocked for non-loopback hosts in offline mode.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"query"},
				Properties: map[string]provider.ToolProp{
					"query": {Type: "string", Description: "Search query."},
					"limit": {Type: "integer", Description: "Maximum number of hits to return (default 5, max 20).", Default: webSearchDefaultLimit},
				},
			},
		},
	}
}

// webSearch runs one search over the configured backend chain.
func (e *Executor) webSearch(ctx context.Context, args string) Result {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return failJSON(fmt.Errorf("websearch: missing required property 'query'"))
	}

	limit := a.Limit
	if limit <= 0 {
		limit = webSearchDefaultLimit
	}
	if limit > webSearchMaxLimit {
		limit = webSearchMaxLimit
	}

	cfg := researchConfigFor(e.Workspace)
	chain, notes := searchBackendChain(cfg.Research)
	if len(chain) == 0 {
		return failJSON(fmt.Errorf("websearch: no search backend is configured"))
	}

	// A backend outside the operator's declared scope is never contacted.
	// It is skipped with a note, and when nothing is left the call is
	// refused rather than silently answered from somewhere else.
	reachable := make([]string, 0, len(chain))
	for _, name := range chain {
		spec, ok := searchBackendSpec(name)
		if !ok {
			continue
		}
		if !hostInScope(cfg.Scope, spec.host) {
			notes = append(notes, fmt.Sprintf("%s is outside the declared scope and was not contacted", name))
			continue
		}
		reachable = append(reachable, name)
	}
	if len(reachable) == 0 {
		return failJSON(fmt.Errorf("websearch: every configured search backend is outside the declared scope"))
	}

	// The offline guard runs before any request: while core.mode is
	// "offline" a non-loopback host is refused, exactly as webfetch refuses
	// one. It is checked for the whole chain, so no backend is contacted
	// before the refusal.
	if e.Offline {
		for _, name := range reachable {
			spec, _ := searchBackendSpec(name)
			if !isLoopbackHost(spec.host) {
				return failJSON(fmt.Errorf("offline mode: %s is not loopback; set core.mode = \"normal\" to allow web egress", spec.host))
			}
		}
	}

	// Egress is a permission event. The approval path is the one webfetch
	// uses: the matrix decides first, the operator is asked when the matrix
	// says "ask", and the answer is remembered for the session.
	if err := e.authorize("websearch", query, "websearch: "+query); err != nil {
		return failJSON(err)
	}

	out := webSearchResult{Query: query, Hits: []searchHit{}, Notes: notes}
	var errs []webSearchError
	answered := ""
	var hits []searchHit

	for _, name := range reachable {
		spec, ok := searchBackendSpec(name)
		if !ok {
			continue
		}
		found, err := e.runSearchBackend(ctx, spec, query, limit, cfg.Research)
		if err != nil {
			errs = append(errs, webSearchError{Backend: name, Error: err.Error()})
			continue
		}
		if len(found) == 0 {
			out.Notes = append(out.Notes, fmt.Sprintf("%s returned no results", name))
			if answered == "" {
				answered = name
			}
			continue
		}
		answered = name
		hits = found
		break
	}

	if answered != "" {
		backend := answered
		out.Backend = &backend
		if len(hits) > 0 {
			// Site-aware extraction (M8-B): a hit whose host a reducer owns
			// is fetched through webfetch's own bounds and guards and carries
			// the markdown that reduction produced. A hit that cannot be
			// fetched keeps the snippet the backend returned, and a host no
			// reducer owns is never fetched at all.
			hits = e.applyExtraction(ctx, hits)
			if n := extractedHits(hits); n > 0 {
				out.Notes = append(out.Notes, fmt.Sprintf("site-aware extraction applied to %d hit(s)", n))
			}
			out.Hits = hits
		}
	}
	if len(out.Hits) == 0 {
		out.Notes = append(out.Notes, "no search backend returned results")
	}
	out.Errors = errs

	b, err := json.Marshal(out)
	if err != nil {
		return failJSON(err)
	}
	// A failed chain is a failed turn, but the body still carries the
	// per-backend errors so the model can see what was tried.
	return Result{
		Content: redact.Redact(string(b)),
		Failed:  answered == "" && len(errs) > 0,
	}
}

// searchBackend is one entry of the chain: a name and the host its endpoint
// lives on (the offline guard needs the host before any request).
type searchBackend struct {
	name string
	host string
}

// searchBackendSpec resolves a backend name to its endpoint host.
func searchBackendSpec(name string) (searchBackend, bool) {
	switch name {
	case "duckduckgo":
		return searchBackend{name: name, host: endpointHost(duckduckgoHTMLEndpoint)}, true
	case "brave":
		return searchBackend{name: name, host: endpointHost(braveSearchEndpoint)}, true
	case "tavily":
		return searchBackend{name: name, host: endpointHost(tavilySearchEndpoint)}, true
	}
	return searchBackend{}, false
}

// endpointHost returns the host of an endpoint URL, or "" when it cannot be
// parsed.
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// searchBackendChain normalizes the configured chain: names are lower-cased,
// duplicates and blanks are dropped, and an unknown name is reported instead
// of being silently ignored.
func searchBackendChain(cfg config.Research) ([]string, []string) {
	var chain []string
	var notes []string
	seen := map[string]bool{}
	for _, raw := range cfg.Backends() {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		if _, ok := searchBackendSpec(name); !ok {
			notes = append(notes, fmt.Sprintf("unknown search backend %q (skipped)", raw))
			continue
		}
		seen[name] = true
		chain = append(chain, name)
	}
	return chain, notes
}

// runSearchBackend dispatches one backend.
func (e *Executor) runSearchBackend(ctx context.Context, b searchBackend, query string, limit int, cfg config.Research) ([]searchHit, error) {
	switch b.name {
	case "duckduckgo":
		return e.searchDuckDuckGo(ctx, query, limit, cfg)
	case "brave":
		return e.searchBrave(ctx, query, limit, cfg)
	case "tavily":
		return e.searchTavily(ctx, query, limit, cfg)
	}
	return nil, fmt.Errorf("unknown search backend %q", b.name)
}

// searchDuckDuckGo queries the keyless HTML endpoint and reduces the page to
// title/url/snippet rows.
func (e *Executor) searchDuckDuckGo(ctx context.Context, query string, limit int, cfg config.Research) ([]searchHit, error) {
	u, err := url.Parse(duckduckgoHTMLEndpoint)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo endpoint is not a URL")
	}
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()

	status, body, err := e.researchFetch(ctx, http.MethodGet, u.String(), nil, nil, cfg.Timeout(), webSearchMaxBytes)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("duckduckgo returned status %d", status)
	}
	return parseDuckDuckGoHTML(string(body), limit), nil
}

// searchBrave queries the Brave Search API with the operator's key.
func (e *Executor) searchBrave(ctx context.Context, query string, limit int, cfg config.Research) ([]searchHit, error) {
	env := cfg.BraveKeyEnv()
	key := strings.TrimSpace(os.Getenv(env))
	if key == "" {
		// The key is named, never printed.
		return nil, fmt.Errorf("brave backend has no API key: set %s", env)
	}
	u, err := url.Parse(braveSearchEndpoint)
	if err != nil {
		return nil, fmt.Errorf("brave endpoint is not a URL")
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(limit))
	u.RawQuery = q.Encode()

	hdr := map[string]string{
		"Accept":               "application/json",
		"X-Subscription-Token": key,
	}
	status, body, err := e.researchFetch(ctx, http.MethodGet, u.String(), hdr, nil, cfg.Timeout(), webSearchMaxBytes)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("brave returned status %d", status)
	}
	var payload struct {
		Web struct {
			Results []struct {
				Title       string   `json:"title"`
				URL         string   `json:"url"`
				Description string   `json:"description"`
				Extra       []string `json:"extra_snippets"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("brave returned a malformed response")
	}
	hits := make([]searchHit, 0, len(payload.Web.Results))
	for _, r := range payload.Web.Results {
		snippet := r.Description
		if snippet == "" && len(r.Extra) > 0 {
			snippet = r.Extra[0]
		}
		hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: snippet})
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// searchTavily queries the Tavily search API with the operator's key.
func (e *Executor) searchTavily(ctx context.Context, query string, limit int, cfg config.Research) ([]searchHit, error) {
	env := cfg.TavilyKeyEnv()
	key := strings.TrimSpace(os.Getenv(env))
	if key == "" {
		return nil, fmt.Errorf("tavily backend has no API key: set %s", env)
	}
	payload, err := json.Marshal(map[string]any{
		"api_key":             key,
		"query":               query,
		"max_results":         limit,
		"search_depth":        "basic",
		"include_answer":      false,
		"include_raw_content": false,
	})
	if err != nil {
		return nil, fmt.Errorf("tavily request could not be built")
	}
	hdr := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}
	status, body, err := e.researchFetch(ctx, http.MethodPost, tavilySearchEndpoint, hdr, payload, cfg.Timeout(), webSearchMaxBytes)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		// The body is not echoed: an upstream error must not be able to
		// print the key back to the model.
		return nil, fmt.Errorf("tavily returned status %d", status)
	}
	var resp struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("tavily returned a malformed response")
	}
	hits := make([]searchHit, 0, len(resp.Results))
	for _, r := range resp.Results {
		hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Content})
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// The DuckDuckGo HTML endpoint reduces to anchors with two classes:
// result__a for the title and result__snippet for the excerpt. The parser
// walks the anchors in document order and pairs each title with the excerpt
// that follows it.
var (
	reDDGAnchor = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	reAttrClass = regexp.MustCompile(`(?is)\bclass\s*=\s*("([^"]*)"|'([^']*)')`)
	reAttrHref  = regexp.MustCompile(`(?is)\bhref\s*=\s*("([^"]*)"|'([^']*)')`)
)

// parseDuckDuckGoHTML reduces a results page to hits.
func parseDuckDuckGoHTML(page string, limit int) []searchHit {
	type anchor struct {
		class string
		href  string
		text  string
	}
	var anchors []anchor
	for _, m := range reDDGAnchor.FindAllStringSubmatch(page, -1) {
		anchors = append(anchors, anchor{
			class: attrValue(reAttrClass, m[1]),
			href:  attrValue(reAttrHref, m[1]),
			text:  cleanHTMLText(m[2]),
		})
	}

	var hits []searchHit
	cur := -1
	for _, a := range anchors {
		switch {
		case strings.Contains(a.class, "result__a"):
			hits = append(hits, searchHit{Title: a.text, URL: normalizeSearchURL(a.href)})
			cur = len(hits) - 1
		case strings.Contains(a.class, "result__snippet") && cur >= 0:
			if hits[cur].Snippet == "" {
				hits[cur].Snippet = a.text
			}
		}
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// attrValue extracts the value of an attribute from a tag's attribute list.
func attrValue(re *regexp.Regexp, attrs string) string {
	m := re.FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	if m[2] != "" {
		return m[2]
	}
	return m[3]
}

// normalizeSearchURL turns a backend link into the URL it points at. The
// keyless endpoint wraps results in a redirect whose target is carried in
// the "uddg" parameter.
func normalizeSearchURL(href string) string {
	raw := strings.TrimSpace(html.UnescapeString(href))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	if u.IsAbs() {
		return u.String()
	}
	base, err := url.Parse("https://html.duckduckgo.com")
	if err != nil {
		return raw
	}
	if abs, err := base.Parse(raw); err == nil {
		return abs.String()
	}
	return raw
}

// ---------- shared research plumbing ----------

// researchConfig is the resolved configuration of one research call: the
// [research] block and the operator's declared scope.
type researchConfig struct {
	Research config.Research
	Scope    scope.Rules
}

// researchConfigFor loads the configuration for a workspace. The operator's
// own configuration and the workspace's kintsugi.toml are merged the way
// the rest of the harness merges them; a file that cannot be read leaves the
// built-in defaults in place, because a broken config must not silently
// disable the research tools.
func researchConfigFor(workspace string) researchConfig {
	cfg, _, err := config.Load(workspace)
	if err != nil {
		// The zero value carries the built-in defaults: an empty backend
		// list falls back to the default chain, an empty key env name falls
		// back to the documented one.
		return researchConfig{}
	}
	return researchConfig{Research: cfg.Research, Scope: scope.FromConfig(cfg)}
}

// hostInScope reports whether a host may be contacted under the operator's
// declared scope. A scope that declares no hosts constrains nothing.
func hostInScope(rules scope.Rules, host string) bool {
	if rules.Empty() || host == "" {
		return true
	}
	return rules.AllowsHost(host)
}

// researchFetch performs one bounded HTTP request for the research tools.
// It mirrors webfetch's bounds: a timeout, a small redirect cap, a response
// cap and a fixed user agent. It never retries and never follows a chain —
// the caller owns the chain.
func (e *Executor) researchFetch(ctx context.Context, method, rawURL string, hdr map[string]string, body []byte, timeout time.Duration, maxBytes int64) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, fmt.Errorf("invalid request URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, nil, fmt.Errorf("scheme %q is not allowed (use http or https)", u.Scheme)
	}
	if u.Host == "" {
		return 0, nil, fmt.Errorf("invalid request URL")
	}
	if timeout <= 0 {
		timeout = webSearchTimeout
	}
	if maxBytes <= 0 {
		maxBytes = webSearchMaxBytes
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var payload io.Reader
	if len(body) > 0 {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", researchUserAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= researchRedirectCap {
				return fmt.Errorf("too many redirects")
			}
			if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
				return fmt.Errorf("redirect to disallowed scheme %q", r.URL.Scheme)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if int64(len(data)) > maxBytes {
		return resp.StatusCode, data[:maxBytes], fmt.Errorf("response exceeded the %d byte cap", maxBytes)
	}
	return resp.StatusCode, data, nil
}
