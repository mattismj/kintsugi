// Site-aware extraction (M8-B, IDEAS I-19, DESIGN §16.13).
//
// webfetch reduces any page to readable text. That stays the default, and this
// file is the layer above it for the sites security work actually reads, where
// "readable text" loses exactly the part that matters: a README's headings, a
// blob's code, a paper's abstract, a question's accepted answer.
//
// One entry point, two shapes:
//
//	Extract.For(host, html)                    reduce a page we already hold
//	(*Executor).FetchPage(ctx, url, maxBytes)  fetch one URL, then reduce it
//
// FetchPage is webfetch's fetch, not a second one: the same bounded request (a
// timeout, a small redirect cap, a response cap, a fixed user agent), the same
// offline guard, the same scope check and the same egress permission, in the
// same order. An `@url` reference or a search extraction therefore cannot reach
// a host webfetch could not reach, and cannot reach one without the operator's
// answer.
//
// Three properties hold for every reducer:
//
//   - Bounded. One page reduces to at most ExtractCap (24 KiB) of markdown and
//     says so when it was cut. A reducer never follows a link.
//   - Attributed. The link targets a reduction kept come back as citations —
//     absolute, de-duplicated, navigation and badge images dropped — and are
//     rendered at the end of the markdown, so an injected page carries its
//     sources with it.
//   - Honest. A reducer that finds nothing in a page produces nothing, and the
//     caller falls back to the generic reduction instead of inventing
//     structure.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

const (
	// ExtractCap is the default cap of one extraction: 24 KiB of markdown,
	// the truncation marker included.
	ExtractCap = 24 << 10
	// extractCodeCap bounds one code block inside an extraction, so a single
	// vendored blob cannot eat the whole budget.
	extractCodeCap = 8 << 10
	// extractMaxCitations caps one extraction's citation list.
	extractMaxCitations = 24
	// extractMaxTreeEntries caps a GitHub file tree.
	extractMaxTreeEntries = 200
	// extractMaxSections caps an arXiv section outline.
	extractMaxSections = 60
	// extractFetchBytes is the download cap of one FetchPage call: webfetch's
	// response cap, raised because a repository page is mostly chrome around
	// the part that is kept.
	extractFetchBytes = 512 << 10
	// extractFetchTimeout bounds one FetchPage request. It is webfetch's.
	extractFetchTimeout = 20 * time.Second
	// extractPhaseTimeout bounds the whole extraction phase of one search,
	// however many hits it fetches.
	extractPhaseTimeout = 25 * time.Second

	// webSearchExtractHits is how many hits of one search may be fetched and
	// reduced. A search stays a search: it never becomes a crawl.
	webSearchExtractHits = 3
	// webSearchExtractBytes caps one hit's download.
	webSearchExtractBytes = 256 << 10
	// webSearchExtractCap bounds the markdown carried per hit.
	webSearchExtractCap = 3072
	// webSearchExtractCitations bounds the citations carried per hit.
	webSearchExtractCitations = 8
)

// The reducers, named after what they reduce. A host no reducer owns is
// reduced generically.
const (
	siteGitHub        = "github"
	siteArxiv         = "arxiv"
	siteStackOverflow = "stackoverflow"
	siteDocs          = "docs"
	siteGeneric       = "generic"
)

// siteHostRule binds one reducer to the hosts it owns.
type siteHostRule struct {
	site  string
	hosts []string
}

// siteHosts is the dispatch table. Like the search endpoints it is a package
// variable rather than a configuration item: a test points a reducer at a local
// server, and no workspace file chooses where a reducer lives.
var siteHosts = []siteHostRule{
	{site: siteGitHub, hosts: []string{"github.com"}},
	{site: siteArxiv, hosts: []string{"arxiv.org"}},
	{site: siteStackOverflow, hosts: []string{
		"stackoverflow.com", "stackexchange.com", "askubuntu.com",
		"superuser.com", "serverfault.com", "mathoverflow.net",
	}},
}

// SiteOf names the reducer that owns a host, or "" when the host is unknown
// and today's generic reduction applies. Subdomains inherit their parent's
// reducer, so gist.github.com and security.stackexchange.com are covered; a
// lookalike ("evil-github.com") is not. A documentation host — one whose first
// label says so, or one of the documentation hosts below — is owned by the
// documentation reducer; an arbitrary unknown host is not, because an
// extraction that claims every page tells the model nothing.
//
// A rule may also name a host:port exactly. That exists so a test can point one
// reducer at one local server without claiming every other local server with
// it; the production rules name hosts alone.
func SiteOf(host string) string {
	raw := strings.ToLower(strings.TrimSpace(host))
	// A rule naming a host:port matches exactly. That is what lets a test
	// point one reducer at one local server without claiming every other
	// local server (they share 127.0.0.1); the production rules below name
	// hosts alone.
	if raw != "" {
		for _, rule := range siteHosts {
			for _, owned := range rule.hosts {
				if owned == raw {
					return rule.site
				}
			}
		}
	}
	h := normalizeHost(host)
	if h == "" {
		return ""
	}
	for _, rule := range siteHosts {
		for _, owned := range rule.hosts {
			if strings.Contains(owned, ":") {
				continue // a port-exact rule; already handled above
			}
			owned = normalizeHost(owned)
			if owned == "" {
				continue
			}
			if h == owned || strings.HasSuffix(h, "."+owned) {
				return rule.site
			}
		}
	}
	if docsHost(h) {
		return siteDocs
	}
	return ""
}

// docsLabels are the first labels that name a documentation site.
var docsLabels = map[string]bool{
	"docs": true, "doc": true, "documentation": true, "guides": true,
	"guide": true, "manual": true, "handbook": true, "learn": true,
}

// docsHosts are the documentation hosts whose labels do not say so.
var docsHosts = []string{"readthedocs.io", "readthedocs.org", "gitbook.io", "man7.org"}

// docsHost reports whether a host is a documentation site.
func docsHost(h string) bool {
	for _, owned := range docsHosts {
		if h == owned || strings.HasSuffix(h, "."+owned) {
			return true
		}
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		return docsLabels[h[:i]]
	}
	return false
}

// normalizeHost lower-cases a host and drops the port, the IPv6 brackets, a
// trailing dot and a "www." prefix.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if bare, _, err := net.SplitHostPort(h); err == nil {
		h = bare
	}
	h = strings.Trim(h, "[]")
	h = strings.TrimSuffix(h, ".")
	return strings.TrimPrefix(h, "www.")
}

// Extractor reduces a fetched page to markdown for the site it came from. Cap
// is the output cap in bytes; zero means ExtractCap.
type Extractor struct{ Cap int }

// Extract is the reducer the research surfaces share: Extract.For(host, html).
var Extract = Extractor{Cap: ExtractCap}

// For reduces one page for its host and returns the markdown plus the link
// targets the reduction preserved. The markdown is capped at x.Cap with a
// truncation marker.
//
// The dispatch is: the site reducer that owns the host, else the documentation
// reducer when the page has structure (a heading or a code block), else
// webfetch's generic reduction — so an unknown host reads exactly as it does
// today.
func (x Extractor) For(host, page string) (markdown string, citations []string) {
	_, markdown, citations, _ = x.reduce(host, "", page)
	return markdown, citations
}

// reduce is For with the bookkeeping a caller that fetched the page needs: the
// reducer that applied and whether a cap was hit. pageURL is the address the
// page came from, or "" when the caller only holds markup: it lets the generic
// reduction resolve a relative link to the target a browser would show.
func (x Extractor) reduce(host, pageURL, page string) (site, markdown string, citations []string, truncated bool) {
	limit := x.Cap
	if limit <= 0 {
		limit = ExtractCap
	}
	clean := dropNoise(page)
	site = SiteOf(host)
	switch site {
	case siteGitHub:
		markdown, citations = reduceGitHub(host, clean)
	case siteArxiv:
		markdown, citations = reduceArxiv(host, clean)
	case siteStackOverflow:
		markdown, citations = reduceStackOverflow(host, clean)
	case siteDocs:
		markdown, citations = reduceDocs(host, clean)
	}
	if strings.TrimSpace(markdown) == "" {
		// Either no reducer owns this host or its reducer found nothing to
		// keep. The generic reduction answers, so an unknown page reads
		// exactly as it does today — never a structure invented from thin
		// air.
		site = siteGeneric
		markdown = htmlToMarkdown(page, pageURL)
		citations = harvestCitations(host, clean)
	}
	markdown = normalizeMarkdown(markdown)
	markdown, truncated = capExtraction(markdown, limit)
	return site, markdown, citations, truncated
}

// capExtraction bounds one extraction. The marker counts towards the cap, so a
// caller can rely on len(markdown) <= limit.
func capExtraction(text string, limit int) (string, bool) {
	text = strings.TrimRight(text, "\n")
	if limit <= 0 || len(text) <= limit {
		return text, false
	}
	marker := fmt.Sprintf("\n…[truncated at %d bytes]", limit)
	keep := limit - len(marker)
	if keep < 0 {
		keep = 0
	}
	cut := strings.ToValidUTF8(text[:keep], "")
	if i := strings.LastIndexByte(cut, '\n'); i > keep/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, "\n") + marker, true
}

// ---------- one fetched page ----------

// Page is one URL, fetched and reduced.
type Page struct {
	URL         string   // the URL that was fetched
	Host        string   // its host, port included
	Site        string   // the reducer that applied: github | arxiv | stackoverflow | docs | generic
	Extracted   bool     // a reducer applied; false means the generic reduction
	Status      int      // the HTTP status
	ContentType string   // what the body turned out to be
	Markdown    string   // the reduction, capped at ExtractCap
	Citations   []string // the absolute link targets it preserved
	Bytes       int      // body bytes read
	Truncated   bool     // the body or the extraction hit a cap
}

// parseFetchURL validates a URL the way webfetch validates one: absolute,
// http(s), a host, no credentials.
func parseFetchURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("invalid absolute URL %q", strings.TrimSpace(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q is not allowed (use http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid absolute URL %q", strings.TrimSpace(raw))
	}
	if u.User != nil {
		return nil, fmt.Errorf("credentials in URLs are not allowed")
	}
	return u, nil
}

// FetchPage fetches one URL through webfetch's bounds and reduces it for the
// site it came from. It is the read path an `@url` reference and a search
// extraction share: the offline guard, the declared scope and the egress
// permission are webfetch's, checked in webfetch's order, before a byte is
// requested.
//
// maxBytes caps the download; zero means extractFetchBytes. The markdown that
// comes back is capped separately, at ExtractCap.
func (e *Executor) FetchPage(ctx context.Context, raw string, maxBytes int) (Page, error) {
	u, err := parseFetchURL(raw)
	if err != nil {
		return Page{}, err
	}
	if e.Offline && !isLoopbackHost(u.Hostname()) {
		return Page{}, fmt.Errorf("offline mode: %s is not loopback; set core.mode = \"normal\" to allow web egress", u.Host)
	}
	// The scope check is webfetch's, so a declared scope and a sandbox host
	// allowlist bound this fetch too.
	if err := e.scopeURL(fmt.Sprintf(`{"url":%q}`, u.String())); err != nil {
		return Page{}, err
	}
	if err := e.authorize("webfetch", u.String(), "GET "+u.String()); err != nil {
		return Page{}, err
	}
	if maxBytes <= 0 {
		maxBytes = extractFetchBytes
	}
	if maxBytes > 2<<20 {
		maxBytes = 2 << 20
	}

	status, body, ferr := e.researchFetch(ctx, http.MethodGet, u.String(), nil, nil, extractFetchTimeout, int64(maxBytes))
	// researchFetch answers an over-cap body with the bytes it did read plus
	// an error: the page is still reducible, it is only marked truncated.
	overCap := ferr != nil && status >= 200 && status < 300 && len(body) > 0
	if ferr != nil && !overCap {
		return Page{}, ferr
	}
	if status < 200 || status >= 300 {
		return Page{}, fmt.Errorf("GET %s: status %d", u.String(), status)
	}

	page := Page{
		URL: u.String(), Host: u.Host, Status: status,
		Bytes: len(body), Truncated: overCap,
	}
	if looksLikeHTML(body) {
		page.ContentType = "text/html"
		var truncated bool
		page.Site, page.Markdown, page.Citations, truncated = Extract.reduce(u.Host, u.String(), string(body))
		page.Truncated = page.Truncated || truncated
		page.Extracted = page.Site != siteGeneric
	} else {
		page.ContentType = "text/plain"
		page.Site = siteGeneric
		text, truncated := capExtraction(plainBody(body), ExtractCap)
		page.Markdown = text
		page.Truncated = page.Truncated || truncated
	}
	if e.Log != nil {
		e.Log("webfetch", map[string]any{
			"url": page.URL, "status": page.Status, "bytes": page.Bytes,
			"site": page.Site, "truncated": page.Truncated,
		})
	}
	return page, nil
}

// looksLikeHTML reports whether a body is markup rather than text or JSON. It
// sniffs instead of trusting a content type, because the bounded fetch reports
// the body, not the headers.
func looksLikeHTML(body []byte) bool {
	head := strings.ToLower(string(body[:min(len(body), 4096)]))
	for _, marker := range []string{"<!doctype html", "<html", "<head", "<body", "<div", "<article", "<pre", "<h1", "<h2", "<p>", "<p "} {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// plainBody is a non-HTML response: JSON is pretty-printed, anything else is
// the text as it came back.
func plainBody(body []byte) string {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		if json.Unmarshal(body, &v) == nil {
			if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
				return string(pretty)
			}
		}
	}
	return string(body)
}

// URLFetcher builds the fetcher an `@url` reference uses when the caller does
// not wire one of its own. The workspace's configuration decides the offline
// posture, the declared scope and the sandbox allowlist; the session policy and
// the approver decide the egress permission.
//
// A configuration that cannot be read fails closed: the fetcher is offline, so
// only a loopback host is reachable.
func URLFetcher(workspace string, policy Policy, approve Approver) *Executor {
	ex := &Executor{Workspace: workspace, Policy: policy, Approver: approve, Offline: true}
	cfg, _, err := config.Load(workspace)
	if err == nil {
		ex.Offline = cfg.Core.Mode == "offline"
		ex.ApplySecurity(cfg)
	}
	return ex
}

// ---------- the websearch hook ----------

// fetchHost is the host a URL fetches against, port included, so a port-exact
// dispatch rule matches. It never fails: a URL that will not parse is simply
// not a host any reducer owns.
func fetchHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// applyExtraction reduces the hits whose host a site reducer owns. A hit is
// fetched through FetchPage — the same bounds, the same guards, the same
// permission as webfetch — and carries the markdown and the citations it kept.
//
// It stays a search, not a crawl: at most webSearchExtractHits pages are
// fetched, each capped, the whole phase bounded by extractPhaseTimeout, and a
// hit that cannot be fetched simply keeps the snippet the backend returned. An
// unknown host is never fetched at all.
func (e *Executor) applyExtraction(ctx context.Context, hits []searchHit) []searchHit {
	fetched := 0
	phase, cancel := context.WithTimeout(ctx, extractPhaseTimeout)
	defer cancel()
	for i := range hits {
		if fetched >= webSearchExtractHits {
			break
		}
		if SiteOf(fetchHost(hits[i].URL)) == "" {
			continue
		}
		page, err := e.FetchPage(phase, hits[i].URL, webSearchExtractBytes)
		if err != nil || !page.Extracted || strings.TrimSpace(page.Markdown) == "" {
			continue
		}
		fetched++
		hits[i].Site = page.Site
		hits[i].Extracted = true
		hits[i].Extract = excerpt(page.Markdown, webSearchExtractCap)
		hits[i].Citations = firstCitations(page.Citations, webSearchExtractCitations)
	}
	return hits
}

// excerpt bounds the markdown one hit carries.
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := strings.ToValidUTF8(s[:n], "")
	if i := strings.LastIndexByte(cut, '\n'); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, "\n") + "\n…[truncated]"
}

// firstCitations bounds a citation list.
func firstCitations(cites []string, n int) []string {
	if n <= 0 || len(cites) <= n {
		return cites
	}
	return cites[:n]
}

// extractedHits counts the hits a reducer applied to.
func extractedHits(hits []searchHit) int {
	n := 0
	for _, h := range hits {
		if h.Extracted {
			n++
		}
	}
	return n
}

// ---------- shared reduction ----------

// element is one located HTML element.
type element struct {
	attrs string // the start tag's attribute list
	inner string // the markup between the start tag and the closing tag
	start int    // offset of '<'
	end   int    // offset just after the element
}

var (
	reStartTag    = mustRegexp(`(?is)<([a-zA-Z][a-zA-Z0-9]*)\b([^>]*)>`)
	reNextBlock   = mustRegexp(`(?is)<(?:p|div|h[1-6]|pre|ul|ol|table|section|article|blockquote|li|main|figure)\b`)
	reBlockTag    = mustRegexp(`(?is)<(h[1-6]|p|pre|ul|ol|blockquote|table|figure)\b([^>]*)>`)
	reAttrID      = mustRegexp(`(?is)\bid\s*=\s*("([^"]*)"|'([^']*)')`)
	reAttrRole    = mustRegexp(`(?is)\brole\s*=\s*("([^"]*)"|'([^']*)')`)
	reAttrContent = mustRegexp(`(?is)\bcontent\s*=\s*("([^"]*)"|'([^']*)')`)
	reInlineCode  = mustRegexp(`(?is)<code\b[^>]*>(.*?)</code>`)
	reStrong      = mustRegexp(`(?is)<(?:strong|b)\b[^>]*>(.*?)</(?:strong|b)>`)
	reEm          = mustRegexp(`(?is)<(?:em|i)\b[^>]*>(.*?)</(?:em|i)>`)
	reBR          = mustRegexp(`(?i)<br\s*/?>`)
	// reCodeLine is where a line of code ends in markup that carries no
	// newlines of its own: a table row, a break, a block.
	reCodeLine  = mustRegexp(`(?i)</tr>|<br\s*/?>|</(?:div|p|li|pre)>`)
	reLangClass = mustRegexp(`(?i)\bclass\s*=\s*["'][^"']*(?:highlight-source-|language-|lang-)([a-z0-9+#]+)`)
)

// voidElements never carry a closing tag.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// scannedTags are the tags the reducers locate by name. The scanners are
// compiled once, at init, and only read at runtime: the reducers may run
// concurrently.
var scannedTags = []string{
	"a", "article", "aside", "blockquote", "body", "code", "dd", "div", "dl",
	"figure", "h1", "h2", "h3", "h4", "h5", "h6", "header", "li", "main",
	"ol", "p", "pre", "section", "span", "table", "tbody", "td", "th",
	"thead", "tr", "ul",
}

// reTagScan holds one scanner per scanned tag.
var reTagScan = func() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(scannedTags))
	for _, name := range scannedTags {
		out[name] = mustRegexp(`(?is)<(/?)` + regexp.QuoteMeta(name) + `\b([^>]*)>`)
	}
	return out
}()

// tagScan is the scanner for one tag name: its closing slash and its attribute
// list.
func tagScan(name string) *regexp.Regexp {
	if re, ok := reTagScan[name]; ok {
		return re
	}
	return mustRegexp(`(?is)<(/?)` + regexp.QuoteMeta(name) + `\b([^>]*)>`)
}

// selector names one element to look for: a tag plus an id, a class or a role
// it must carry. A zero field is not matched on.
type selector struct {
	tag   string
	id    string
	class string
	role  string
}

func (s selector) matches(attrs string) bool {
	if s.id != "" && !strings.EqualFold(attrValue(reAttrID, attrs), s.id) {
		return false
	}
	if s.class != "" && !classIs(attrs, s.class) {
		return false
	}
	if s.role != "" && !strings.EqualFold(attrValue(reAttrRole, attrs), s.role) {
		return false
	}
	return true
}

// classIs reports whether an attribute list carries this class.
func classIs(attrs, class string) bool {
	for _, c := range strings.Fields(attrValue(reAttrClass, attrs)) {
		if strings.EqualFold(c, class) {
			return true
		}
	}
	return false
}

// idIs reports whether an attribute list carries this id.
func idIs(attrs, id string) bool {
	return strings.EqualFold(strings.TrimSpace(attrValue(reAttrID, attrs)), id)
}

// walkElements finds the elements with this tag name whose attributes satisfy
// keep, in document order. An element that matches owns its contents, so a
// nested element of the same name is not reported twice. limit <= 0 means no
// limit.
func walkElements(s, name string, keep func(attrs string) bool, limit int) []element {
	var out []element
	lname := strings.ToLower(name)
	pos := 0
	for _, m := range reStartTag.FindAllStringSubmatchIndex(s, -1) {
		if m[0] < pos {
			continue // inside an element already taken
		}
		if strings.ToLower(s[m[2]:m[3]]) != lname {
			continue
		}
		attrs := s[m[4]:m[5]]
		if keep != nil && !keep(attrs) {
			// Not this one — but it may contain one, so scanning continues
			// inside it.
			continue
		}
		inner, end := elementInner(s, m[1], lname, attrs)
		out = append(out, element{attrs: attrs, inner: inner, start: m[0], end: end})
		if end > pos {
			pos = end
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// findElement is the first matching element.
func findElement(s, name string, keep func(attrs string) bool) (element, bool) {
	els := walkElements(s, name, keep, 1)
	if len(els) == 0 {
		return element{}, false
	}
	return els[0], true
}

// findElements is every matching element.
func findElements(s, name string, keep func(attrs string) bool) []element {
	return walkElements(s, name, keep, 0)
}

// elementInner returns the markup inside the element whose start tag ends at
// from, plus the offset just after that element. An element with no closing tag
// ends at the next block-level tag, because HTML omits those for paragraphs and
// list items all the time.
func elementInner(s string, from int, name, attrs string) (string, int) {
	if voidElements[name] || strings.HasSuffix(strings.TrimSpace(attrs), "/") {
		return "", from
	}
	re := tagScan(name)
	depth := 1
	for cursor := from; cursor < len(s); {
		m := re.FindStringSubmatchIndex(s[cursor:])
		if m == nil {
			break
		}
		at := cursor + m[0]
		closing := m[3] > m[2]
		tagAttrs := s[cursor+m[4] : cursor+m[5]]
		cursor += m[1] // always past this tag, however it was matched
		if closing {
			depth--
			if depth == 0 {
				return s[from:at], cursor
			}
		} else if !strings.HasSuffix(strings.TrimSpace(tagAttrs), "/") {
			depth++
		}
	}
	if m := reNextBlock.FindStringIndex(s[from:]); m != nil {
		return strings.TrimSpace(s[from : from+m[0]]), from + m[0]
	}
	return strings.TrimSpace(s[from:]), len(s)
}

// firstElement returns the first element matching any of the selectors.
func firstElement(s string, sels ...selector) (element, bool) {
	for _, sel := range sels {
		if el, ok := findElement(s, sel.tag, sel.matches); ok {
			return el, true
		}
	}
	return element{}, false
}

// firstOf is firstElement's markup.
func firstOf(s string, sels ...selector) (string, bool) {
	el, ok := firstElement(s, sels...)
	return el.inner, ok
}

// textOf is the readable text of the first element matching any selector.
func textOf(s string, sels ...selector) string {
	if inner, ok := firstOf(s, sels...); ok {
		return cleanHTMLText(inner)
	}
	return ""
}

// dropNoise removes the markup that carries no readable content: scripts,
// styles, comments and the JSON a modern page embeds inside them. Every
// reducer starts here, so a tag inside an embedded payload can never be
// mistaken for the page's structure.
func dropNoise(raw string) string {
	for _, re := range reHTMLDrop {
		raw = re.ReplaceAllString(raw, "")
	}
	return raw
}

// renderBlocks walks a fragment in document order and renders the blocks worth
// keeping: headings, paragraphs, lists, code, quotes and tables. Markup it does
// not recognise is dropped, which is the point — a reducer is a filter.
func renderBlocks(fragment, host string) string {
	var b strings.Builder
	pos := 0
	for _, m := range reBlockTag.FindAllStringSubmatchIndex(fragment, -1) {
		if m[0] < pos {
			continue
		}
		name := strings.ToLower(fragment[m[2]:m[3]])
		attrs := fragment[m[4]:m[5]]
		inner, end := elementInner(fragment, m[1], name, attrs)
		pos = end
		switch {
		case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
			text := inlineMarkdown(inner, host)
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "\n%s %s\n", strings.Repeat("#", int(name[1]-'0')), text)
		case name == "pre":
			b.WriteString(fence(codeText(inner), codeLang(attrs+codeAttrs(inner), "")))
		case name == "ul" || name == "ol":
			b.WriteString(renderList(inner, name == "ol", host))
		case name == "blockquote":
			if text := inlineMarkdown(inner, host); text != "" {
				for _, line := range strings.Split(text, "\n") {
					b.WriteString("\n> " + line)
				}
				b.WriteString("\n")
			}
		case name == "table":
			b.WriteString(renderTable(inner, host))
		default: // p, figure
			if text := inlineMarkdown(inner, host); text != "" {
				b.WriteString("\n" + text + "\n")
			}
		}
	}
	return normalizeMarkdown(b.String())
}

// renderList renders one list. A nested list is flattened: the items are what
// matter, not the indentation.
func renderList(inner string, ordered bool, host string) string {
	var b strings.Builder
	n := 0
	for _, li := range findElements(inner, "li", nil) {
		text := inlineMarkdown(li.inner, host)
		if text == "" {
			continue
		}
		n++
		marker := "-"
		if ordered {
			marker = fmt.Sprintf("%d.", n)
		}
		b.WriteString("\n" + marker + " " + strings.ReplaceAll(text, "\n", " "))
	}
	if n == 0 {
		return ""
	}
	return b.String() + "\n"
}

// renderTable renders one table as markdown rows; the first row is the header.
func renderTable(inner string, host string) string {
	var b strings.Builder
	rows := 0
	for _, tr := range findElements(inner, "tr", nil) {
		var cells []element
		cells = append(cells, findElements(tr.inner, "th", nil)...)
		cells = append(cells, findElements(tr.inner, "td", nil)...)
		var parts []string
		for _, cell := range cells {
			if text := inlineMarkdown(cell.inner, host); text != "" {
				parts = append(parts, strings.ReplaceAll(text, "\n", " "))
			}
		}
		if len(parts) == 0 {
			continue
		}
		rows++
		b.WriteString("\n| " + strings.Join(parts, " | ") + " |")
		if rows == 1 {
			b.WriteString("\n|" + strings.Repeat(" --- |", len(parts)))
		}
	}
	if rows == 0 {
		return ""
	}
	return b.String() + "\n"
}

// inlineMarkdown renders one inline fragment. Anchors keep their target (that
// is where citations come from), code keeps its backticks, emphasis keeps its
// markers, every other tag disappears.
func inlineMarkdown(s, host string) string {
	for _, re := range reHTMLDrop {
		s = re.ReplaceAllString(s, "")
	}
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		sub := reInlineCode.FindStringSubmatch(m)
		if sub == nil {
			return ""
		}
		text := strings.TrimSpace(codeText(sub[1]))
		if text == "" {
			return ""
		}
		return "`" + strings.ReplaceAll(text, "`", "'") + "`"
	})
	s = reAnchor.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAnchor.FindStringSubmatch(m)
		if sub == nil {
			return ""
		}
		label := strings.TrimSpace(cleanHTMLText(sub[2]))
		target := resolveCitation(host, sub[1])
		if target == "" {
			return label
		}
		if label == "" {
			label = target
		}
		return "[" + label + "](" + target + ")"
	})
	s = reStrong.ReplaceAllStringFunc(s, func(m string) string {
		sub := reStrong.FindStringSubmatch(m)
		if sub == nil || strings.TrimSpace(cleanHTMLText(sub[1])) == "" {
			return cleanHTMLText(m)
		}
		return "**" + strings.TrimSpace(cleanHTMLText(sub[1])) + "**"
	})
	s = reEm.ReplaceAllStringFunc(s, func(m string) string {
		sub := reEm.FindStringSubmatch(m)
		if sub == nil || strings.TrimSpace(cleanHTMLText(sub[1])) == "" {
			return cleanHTMLText(m)
		}
		return "_" + strings.TrimSpace(cleanHTMLText(sub[1])) + "_"
	})
	s = reBR.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpace.ReplaceAllString(s, " ")
	s = reBlank.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// codeText reduces a code region to the source it carries. Unlike
// cleanHTMLText it never collapses whitespace: code is whitespace.
func codeText(s string) string {
	for _, re := range reHTMLDrop {
		s = re.ReplaceAllString(s, "")
	}
	s = reCodeLine.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// codeLangByExt names the fence language a file extension implies.
var codeLangByExt = map[string]string{
	"go": "go", "py": "python", "js": "javascript", "ts": "typescript",
	"rs": "rust", "c": "c", "h": "c", "cc": "cpp", "cpp": "cpp", "hpp": "cpp",
	"sh": "bash", "bash": "bash", "yml": "yaml", "yaml": "yaml", "json": "json",
	"toml": "toml", "md": "markdown", "html": "html", "rb": "ruby",
	"java": "java", "sql": "sql", "ps1": "powershell", "php": "php",
	"pl": "perl", "lua": "lua", "r": "r", "cs": "csharp", "kt": "kotlin",
	"swift": "swift", "dockerfile": "docker",
}

// codeLang guesses a fence language from the block's classes or, failing that,
// from the file a code view is showing.
func codeLang(attrs, path string) string {
	if m := reLangClass.FindStringSubmatch(attrs); m != nil {
		switch lang := strings.ToLower(m[1]); lang {
		case "sh", "shell", "console":
			return "bash"
		case "py":
			return "python"
		case "golang":
			return "go"
		default:
			return lang
		}
	}
	if i := strings.LastIndexByte(path, '.'); i >= 0 && i+1 < len(path) {
		if lang, ok := codeLangByExt[strings.ToLower(path[i+1:])]; ok {
			return lang
		}
	}
	return ""
}

// codeAttrs is the attribute list of a block's inner <code>: a highlighter
// names the language there, not on the <pre> around it.
func codeAttrs(inner string) string {
	if el, ok := findElement(inner, "code", nil); ok {
		return " " + el.attrs
	}
	return ""
}

// fence renders one code block. The fence grows until the code cannot close it,
// so a block that contains backticks stays one block.
func fence(code, lang string) string {
	code = strings.TrimRight(code, "\n")
	if strings.TrimSpace(code) == "" {
		return ""
	}
	if len(code) > extractCodeCap {
		code = strings.TrimRight(strings.ToValidUTF8(code[:extractCodeCap], ""), "\n") +
			fmt.Sprintf("\n…[code truncated at %d bytes]", extractCodeCap)
	}
	marker := "```"
	for strings.Contains(code, marker) {
		marker += "`"
	}
	return "\n" + marker + lang + "\n" + code + "\n" + marker + "\n"
}

// normalizeMarkdown tidies rendered output: no trailing spaces, no runs of
// blank lines, a blank line before a heading or a fence. It is fence-aware —
// blank lines inside code are code.
func normalizeMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		if inFence {
			out = append(out, line)
			if strings.HasPrefix(line, "```") {
				inFence = false
			}
			continue
		}
		if strings.HasPrefix(line, "```") {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			out = append(out, line)
			inFence = true
			continue
		}
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if len(out) == 0 || out[len(out)-1] == "" {
				continue
			}
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(line, "#") && len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, line)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// ---------- citations ----------

// citationJunk are the paths that are navigation rather than a source.
var citationJunk = []string{
	"/login", "/logout", "/signup", "/settings", "/cart", "/search",
	"/site/", "/help", "/policies", "/features/", "/sponsors", "/favicon",
	"/assets/", "/images/", "/list/", "/contact", "/about", "/user/",
}

// citationImageExts are the targets that are pictures, not references.
var citationImageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".bmp"}

// harvestCitations collects the absolute link targets a fragment carries:
// de-duplicated, in document order, navigation and badge images dropped,
// capped.
func harvestCitations(host, fragment string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reAnchor.FindAllStringSubmatch(fragment, -1) {
		target := resolveCitation(host, m[1])
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
		if len(out) >= extractMaxCitations {
			break
		}
	}
	return out
}

// resolveCitation turns one href into an absolute http(s) URL against the
// page's host, or "" when the target is not worth citing.
func resolveCitation(host, href string) string {
	raw := strings.TrimSpace(html.UnescapeString(href))
	if raw == "" {
		return ""
	}
	low := strings.ToLower(raw)
	for _, bad := range []string{"javascript:", "mailto:", "data:", "tel:", "#"} {
		if strings.HasPrefix(low, bad) {
			return ""
		}
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !u.IsAbs() {
		if host == "" {
			return "" // nothing to resolve a relative target against
		}
		base, err := url.Parse("https://" + strings.TrimSuffix(host, "/"))
		if err != nil {
			return ""
		}
		if u, err = base.Parse(raw); err != nil {
			return ""
		}
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	path := strings.ToLower(u.Path)
	for _, junk := range citationJunk {
		if strings.HasPrefix(path, junk) {
			return ""
		}
	}
	for _, ext := range citationImageExts {
		if strings.HasSuffix(path, ext) {
			return ""
		}
	}
	if strings.HasSuffix(normalizeHost(u.Hostname()), "camo.githubusercontent.com") {
		return "" // a badge proxy target is a pixel, not a reference
	}
	return u.String()
}

// dedupeCitations removes repeats while keeping the order, and caps the list.
func dedupeCitations(cites []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(cites))
	for _, c := range cites {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
		if len(out) >= extractMaxCitations {
			break
		}
	}
	return out
}

// renderCitations is the trailing anchor list: an injected page carries its
// sources with it, so a claim can be traced to where it came from.
func renderCitations(cites []string) string {
	if len(cites) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Citations\n")
	for _, c := range cites {
		b.WriteString("\n- " + c)
	}
	return b.String() + "\n"
}

// ---------- page furniture ----------

var (
	reCanonical   = mustRegexp(`(?is)<link\b[^>]*>`)
	reMetaOGURL   = mustRegexp(`(?is)<meta\b[^>]*property\s*=\s*["']og:url["'][^>]*>`)
	reMetaDesc    = mustRegexp(`(?is)<meta\b[^>]*(?:name|property)\s*=\s*["'](?:description|og:description)["'][^>]*>`)
	reCiteTitle   = mustRegexp(`(?is)<meta\b[^>]*name\s*=\s*["']citation_title["'][^>]*>`)
	reCiteAuthor  = mustRegexp(`(?is)<meta\b[^>]*name\s*=\s*["']citation_author["'][^>]*>`)
	reCiteDate    = mustRegexp(`(?is)<meta\b[^>]*name\s*=\s*["']citation_(?:date|online_date)["'][^>]*>`)
	reCiteDOI     = mustRegexp(`(?is)<meta\b[^>]*name\s*=\s*["']citation_doi["'][^>]*>`)
	reCitePDF     = mustRegexp(`(?is)<meta\b[^>]*name\s*=\s*["']citation_pdf_url["'][^>]*>`)
	reCiteArxivID = mustRegexp(`(?is)<meta\b[^>]*name\s*=\s*["']citation_arxiv_id["'][^>]*>`)
	reArxivPrefix = mustRegexp(`^\s*\[[0-9]{4}\.[0-9]{4,5}(?:v[0-9]+)?\]\s*`)
	reSectionHead = mustRegexp(`(?is)<h([2-6])\b[^>]*>(.*?)</h[2-6]>`)
	reGitHubPath  = mustRegexp(`(?i)href\s*=\s*["']/([^/"']+/[^/"']+)/(tree|blob)/([^/"']+)(?:/([^"']*?))?\s*["']`)
	reTitleGitHub = mustRegexp(`(?i)\s+[·|‐–—-]\s+github\s*$`)
	reTitleQASite = mustRegexp(`(?i)\s+[-–|]\s+[^-–|]*(?:stack overflow|stack exchange|server fault|super user|ask ubuntu|mathoverflow)[^-–|]*$`)
	reTitleArxiv  = mustRegexp(`(?i)\s+[-–|]\s+arxiv\s*$`)
)

// pageTitle is the document title with the site's own suffix dropped.
func pageTitle(raw string) string {
	m := reTitle.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	title := cleanHTMLText(m[1])
	for _, re := range []*regexp.Regexp{reTitleGitHub, reTitleQASite, reTitleArxiv} {
		title = strings.TrimSpace(re.ReplaceAllString(title, ""))
	}
	return title
}

// metaContent reads one meta tag's content attribute.
func metaContent(raw string, re *regexp.Regexp) string {
	tag := re.FindString(raw)
	if tag == "" {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(attrValue(reAttrContent, tag)))
}

// metaList reads every content attribute a meta pattern matches.
func metaList(raw string, re *regexp.Regexp) []string {
	var out []string
	for _, tag := range re.FindAllString(raw, -1) {
		if v := strings.TrimSpace(html.UnescapeString(attrValue(reAttrContent, tag))); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// canonicalURL is the page's own address as the page declares it.
func canonicalURL(raw string) string {
	for _, tag := range reCanonical.FindAllString(raw, -1) {
		low := strings.ToLower(tag)
		if !strings.Contains(low, `rel="canonical"`) && !strings.Contains(low, `rel='canonical'`) {
			continue
		}
		if href := attrValue(reAttrHref, tag); href != "" {
			return strings.TrimSpace(html.UnescapeString(href))
		}
	}
	return metaContent(raw, reMetaOGURL)
}

// fieldLabels are the labels a scraped field carries in the page itself.
var fieldLabels = []string{
	"title:", "authors:", "author:", "abstract:", "subjects:", "subject:",
	"comments:", "comment:", "journal ref:", "journal reference:", "doi:",
	"submitted:", "primary class:", "report number:", "msc:", "acm class:",
}

// stripFieldLabel drops a leading "Label:" from a scraped field.
func stripFieldLabel(s string) string {
	t := strings.TrimSpace(s)
	low := strings.ToLower(t)
	for _, label := range fieldLabels {
		if strings.HasPrefix(low, label) {
			return strings.TrimSpace(t[len(label):])
		}
	}
	return t
}

// splitPath splits a URL path into its non-empty segments.
func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// ---------- GitHub ----------

// reduceGitHub keeps what a repository page is read for: the file tree, the
// README, and — for a code view — the file itself in a fence.
func reduceGitHub(host, raw string) (string, []string) {
	repo, kind, ref, path := githubLocation(raw)
	title := firstNonEmpty(repo, pageTitle(raw), canonicalURL(raw))
	if title == "" {
		return "", nil
	}

	var b strings.Builder
	var cites []string
	b.WriteString("# " + title + "\n")
	if desc := metaContent(raw, reMetaDesc); desc != "" {
		b.WriteString("\n" + desc + "\n")
	}
	if ref != "" {
		fmt.Fprintf(&b, "\nref: %s\n", ref)
	}
	if path != "" {
		fmt.Fprintf(&b, "path: %s\n", path)
	}
	if target := resolveCitation(host, canonicalURL(raw)); target != "" {
		cites = append(cites, target)
	}
	kept := false

	if kind == "blob" {
		if code := githubBlobCode(raw); code != "" {
			b.WriteString("\n## Source\n")
			b.WriteString(fence(code, codeLang("", path)))
			kept = true
		}
	}
	if tree := githubTree(raw, repo); len(tree) > 0 {
		b.WriteString("\n## Files\n")
		for _, entry := range tree {
			b.WriteString("\n- " + entry)
		}
		b.WriteString("\n")
		kept = true
	}
	if readme, ok := firstOf(raw,
		selector{tag: "article", class: "markdown-body"},
		selector{tag: "div", id: "readme"},
		selector{tag: "div", class: "markdown-body"},
	); ok {
		if body := renderBlocks(readme, host); body != "" {
			b.WriteString("\n## README\n\n" + body + "\n")
			cites = append(cites, harvestCitations(host, readme)...)
			kept = true
		}
	}
	if !kept {
		return "", nil
	}
	cites = dedupeCitations(cites)
	b.WriteString(renderCitations(cites))
	return b.String(), cites
}

// githubLocation resolves what a GitHub page is: the repository, the kind of
// view and, for a tree or blob view, the ref and the path.
func githubLocation(raw string) (repo, kind, ref, path string) {
	target := canonicalURL(raw)
	if target == "" {
		return "", "", "", ""
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", "", "", ""
	}
	seg := splitPath(u.Path)
	if len(seg) < 2 {
		return "", "", "", ""
	}
	repo = seg[0] + "/" + seg[1]
	if len(seg) >= 4 && (seg[2] == "tree" || seg[2] == "blob") {
		kind, ref = seg[2], seg[3]
		if len(seg) > 4 {
			path = strings.Join(seg[4:], "/")
		}
	}
	return repo, kind, ref, path
}

// githubTree lists the paths a repository view links to. The tree is read from
// the tree and blob links the page carries, which survive any redesign of the
// markup around them.
func githubTree(raw, repo string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reGitHubPath.FindAllStringSubmatch(raw, -1) {
		if repo != "" && !strings.EqualFold(m[1], repo) {
			continue
		}
		entry := strings.Trim(m[4], "/")
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		if strings.EqualFold(m[2], "tree") {
			entry += "/"
		}
		out = append(out, entry)
		if len(out) >= extractMaxTreeEntries {
			break
		}
	}
	return out
}

// githubBlobCode is a code view's source. GitHub renders a blob either as a
// highlighted <pre> or as a table of numbered lines; the numbered cells are
// dropped, because a line number is not source.
func githubBlobCode(raw string) string {
	if lines := findElements(raw, "td", func(attrs string) bool { return classIs(attrs, "blob-code") }); len(lines) > 0 {
		parts := make([]string, 0, len(lines))
		for _, td := range lines {
			parts = append(parts, codeText(td.inner))
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	if el, ok := firstElement(raw,
		selector{tag: "div", id: "highlighted-code"},
		selector{tag: "table", class: "js-file-line-container"},
		selector{tag: "div", class: "highlight"},
		selector{tag: "pre"},
	); ok {
		return codeText(el.inner)
	}
	return ""
}

// ---------- arXiv ----------

// reduceArxiv keeps what an arXiv page is read for: the title, the authors, the
// abstract, the paper's own metadata and, for an HTML paper, the section
// outline.
func reduceArxiv(host, raw string) (string, []string) {
	title := firstNonEmpty(
		stripFieldLabel(metaContent(raw, reCiteTitle)),
		stripFieldLabel(textOf(raw, selector{tag: "h1", class: "title"})),
		strings.TrimSpace(reArxivPrefix.ReplaceAllString(pageTitle(raw), "")),
	)
	abstract := stripFieldLabel(firstNonEmpty(
		textOf(raw,
			selector{tag: "blockquote", class: "abstract"},
			selector{tag: "div", class: "abstract"},
			selector{tag: "div", class: "ltx_abstract"},
		),
		metaContent(raw, reMetaDesc),
	))
	if title == "" && abstract == "" {
		return "", nil
	}

	var b strings.Builder
	var cites []string
	if title != "" {
		b.WriteString("# " + title + "\n")
	}
	if authors := metaList(raw, reCiteAuthor); len(authors) > 0 {
		b.WriteString("\nAuthors: " + strings.Join(authors, ", ") + "\n")
	} else if line := stripFieldLabel(textOf(raw, selector{tag: "div", class: "authors"})); line != "" {
		b.WriteString("\nAuthors: " + line + "\n")
	}
	for _, f := range []struct {
		re    *regexp.Regexp
		label string
	}{
		{reCiteArxivID, "arXiv"},
		{reCiteDate, "Submitted"},
		{reCiteDOI, "DOI"},
	} {
		if v := metaContent(raw, f.re); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", f.label, v)
		}
	}
	for _, f := range []struct {
		class string
		label string
	}{
		{"subjects", "Subjects"},
		{"comments", "Comments"},
		{"jref", "Journal ref"},
		{"msc", "MSC"},
		{"acm", "ACM class"},
	} {
		if v := stripFieldLabel(textOf(raw, selector{tag: "td", class: f.class})); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", f.label, v)
		}
	}
	if abstract != "" {
		b.WriteString("\n## Abstract\n\n" + abstract + "\n")
	}
	if sections := arxivSections(raw); len(sections) > 0 {
		b.WriteString("\n## Sections\n")
		for _, s := range sections {
			b.WriteString("\n" + s)
		}
		b.WriteString("\n")
	}
	// The citations come from the abstract region when there is one, so the
	// page's own navigation does not drown the paper's links.
	region := raw
	if inner, ok := firstOf(raw, selector{tag: "div", id: "abs"}, selector{tag: "div", class: "abs"}); ok {
		region = inner
	}
	for _, target := range []string{canonicalURL(raw), metaContent(raw, reCitePDF)} {
		if c := resolveCitation(host, target); c != "" {
			cites = append(cites, c)
		}
	}
	cites = append(cites, harvestCitations(host, region)...)
	cites = dedupeCitations(cites)
	b.WriteString(renderCitations(cites))
	return b.String(), cites
}

// arxivSections is an HTML paper's outline: its section headings, indented by
// level, in document order.
func arxivSections(raw string) []string {
	var out []string
	for _, m := range reSectionHead.FindAllStringSubmatch(raw, -1) {
		text := cleanHTMLText(m[2])
		if text == "" {
			continue
		}
		level := int(m[1][0] - '2')
		if level < 0 {
			level = 0
		}
		out = append(out, strings.Repeat("  ", level)+"- "+text)
		if len(out) >= extractMaxSections {
			break
		}
	}
	return out
}

// ---------- Stack Overflow ----------

// reduceStackOverflow keeps the question and the answer that settled it, with
// their code blocks — the two things a search result is opened for.
func reduceStackOverflow(host, raw string) (string, []string) {
	question, ok := findElement(raw, "div", func(attrs string) bool {
		return idIs(attrs, "question") || classIs(attrs, "question")
	})
	if !ok {
		return "", nil
	}
	title := firstNonEmpty(
		textOf(raw, selector{tag: "a", class: "question-hyperlink"}),
		pageTitle(raw),
		textOf(raw, selector{tag: "h1"}),
	)
	if title == "" {
		return "", nil
	}

	var b strings.Builder
	var cites []string
	b.WriteString("# " + title + "\n")

	var facts []string
	if score := firstNonEmpty(
		textOf(question.inner, selector{tag: "div", class: "vote-count-post"}),
		textOf(question.inner, selector{tag: "span", class: "js-vote-count"}),
	); score != "" {
		facts = append(facts, "score: "+score)
	}
	answers := findElements(raw, "div", func(attrs string) bool {
		return classIs(attrs, "answer") || classIs(attrs, "accepted-answer")
	})
	if len(answers) > 0 {
		facts = append(facts, fmt.Sprintf("answers: %d", len(answers)))
	}
	if tags := stackOverflowTags(question.inner); len(tags) > 0 {
		facts = append(facts, "tags: "+strings.Join(tags, ", "))
	}
	if len(facts) > 0 {
		b.WriteString("\n" + strings.Join(facts, " · ") + "\n")
	}

	b.WriteString("\n## Question\n\n" + stackOverflowBody(question.inner, host) + "\n")
	if c := resolveCitation(host, canonicalURL(raw)); c != "" {
		cites = append(cites, c)
	}
	cites = append(cites, harvestCitations(host, question.inner)...)

	if answer, accepted := topAnswer(answers); answer != nil {
		heading := "## Top answer"
		if accepted {
			heading += " (accepted)"
		}
		b.WriteString("\n" + heading + "\n\n" + stackOverflowBody(answer.inner, host) + "\n")
		cites = append(cites, harvestCitations(host, answer.inner)...)
	}
	cites = dedupeCitations(cites)
	b.WriteString(renderCitations(cites))
	return b.String(), cites
}

// stackOverflowTags lists a post's tags.
func stackOverflowTags(region string) []string {
	var out []string
	for _, a := range findElements(region, "a", func(attrs string) bool { return classIs(attrs, "post-tag") }) {
		if text := cleanHTMLText(a.inner); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// stackOverflowBody renders a post's prose, code blocks included.
func stackOverflowBody(region, host string) string {
	prose, ok := firstOf(region,
		selector{tag: "div", class: "s-prose"},
		selector{tag: "div", class: "post-text"},
		selector{tag: "div", class: "postcell"},
	)
	if !ok {
		prose = region
	}
	return renderBlocks(prose, host)
}

// topAnswer is the accepted answer when there is one, else the first answer the
// page carries — which is the top-scoring one on Stack Overflow's default sort.
func topAnswer(answers []element) (*element, bool) {
	for i := range answers {
		if classIs(answers[i].attrs, "accepted-answer") {
			return &answers[i], true
		}
	}
	if len(answers) == 0 {
		return nil, false
	}
	return &answers[0], false
}

// ---------- generic documentation ----------

// hasStructure reports whether a page carries the markup the documentation
// reducer reads: a heading or a code block. A page with neither is reduced
// generically, exactly as webfetch reduces it today.
func hasStructure(raw string) bool {
	return reHead.MatchString(raw) || strings.Contains(strings.ToLower(raw), "<pre")
}

// reduceDocs is the generic documentation reducer: the page's main region, its
// headings, paragraphs, lists and code, and the links it kept.
func reduceDocs(host, raw string) (string, []string) {
	if !hasStructure(raw) {
		return "", nil
	}
	region, ok := firstOf(raw,
		selector{tag: "main"},
		selector{tag: "div", role: "main"},
		selector{tag: "div", id: "content"},
		selector{tag: "div", class: "content"},
		selector{tag: "article"},
		selector{tag: "body"},
	)
	if !ok {
		region = raw
	}
	body := renderBlocks(region, host)
	if body == "" {
		body = renderBlocks(raw, host)
	}
	if body == "" {
		return "", nil
	}
	var b strings.Builder
	if title := firstNonEmpty(pageTitle(raw), textOf(raw, selector{tag: "h1"})); title != "" {
		b.WriteString("# " + title + "\n")
	}
	b.WriteString("\n" + body)
	cites := dedupeCitations(harvestCitations(host, region))
	b.WriteString(renderCitations(cites))
	return b.String(), cites
}

// ---------- tool definition ----------

func extractTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "extract",
			Description: "Fetch a URL and extract site-aware structured content. For GitHub repos, papers, Stack Overflow, and docs, returns markdown with preserved structure and citations. Use instead of webfetch for research URLs.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"url"},
				Properties: map[string]provider.ToolProp{
					"url":       {Type: "string", Description: "URL to fetch and extract"},
					"max_bytes": {Type: "integer", Description: "Maximum bytes to download (default 512KB)"},
				},
			},
		},
	}
}

func (e *Executor) extract(ctx context.Context, args string) Result {
	var a struct {
		URL      string `json:"url"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if strings.TrimSpace(a.URL) == "" {
		return failJSON(fmt.Errorf("extract: missing required property 'url'"))
	}
	if a.MaxBytes <= 0 {
		a.MaxBytes = 512 * 1024
	}
	page, err := e.FetchPage(ctx, a.URL, a.MaxBytes)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: page.Markdown}
}
