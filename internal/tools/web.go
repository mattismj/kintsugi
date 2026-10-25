package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"kintsugi/internal/provider"
)

// webfetch is the read side of network access: one URL, one bounded response,
// reduced to readable text. It is a tool, not a browser: no cookies, no form
// state, no JavaScript, no redirect chains beyond a small cap.

func webFetchTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "webfetch",
			Description: "Fetch one http(s) URL and return readable text. HTML is reduced to headings, links, lists, and paragraphs; JSON is pretty-printed. Use it for docs and references, not for authenticated or interactive sites. Network access requires operator approval and is blocked for non-loopback hosts in offline mode.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"url"},
				Properties: map[string]provider.ToolProp{
					"url":       {Type: "string", Description: "Absolute http:// or https:// URL."},
					"max_bytes": {Type: "integer", Description: "Response body cap in bytes (default 131072, max 2000000)."},
				},
			},
		},
	}
}

func (e *Executor) webFetch(ctx context.Context, args string) Result {
	var a struct {
		URL      string `json:"url"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || u.Scheme == "" {
		return failJSON(fmt.Errorf("webfetch: invalid absolute URL %q", a.URL))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return failJSON(fmt.Errorf("webfetch: scheme %q is not allowed (use http or https)", u.Scheme))
	}
	if u.Host == "" {
		return failJSON(fmt.Errorf("webfetch: invalid absolute URL %q", a.URL))
	}
	if u.User != nil {
		return failJSON(fmt.Errorf("webfetch: credentials in URLs are not allowed"))
	}
	if e.Offline && !isLoopbackHost(u.Hostname()) {
		return failJSON(fmt.Errorf("offline mode: %s is not loopback; set core.mode = \"normal\" to allow web egress", u.Host))
	}
	if err := e.authorize("webfetch", u.String(), "GET "+u.String()); err != nil {
		return failJSON(err)
	}

	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 131072
	}
	if maxBytes > 2000000 {
		maxBytes = 2000000
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return failJSON(err)
	}
	req.Header.Set("User-Agent", "kintsugi/0.1 (+https://localhost; local harness)")
	req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
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
		return failJSON(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return failJSON(err)
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	mediaType, _, _ := mime.ParseMediaType(ct)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{
			Failed: true,
			Content: fmt.Sprintf(`{"error":"GET %s: status %d","content_type":%q,"body":%q}`,
				u.String(), resp.StatusCode, mediaType, clip(string(body), 500)),
		}
	}

	var text string
	switch {
	case strings.Contains(mediaType, "html"):
		text = htmlToMarkdown(string(body), u.String())
	case strings.Contains(mediaType, "json"):
		var v any
		if json.Unmarshal(body, &v) == nil {
			if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
				text = string(pretty)
			}
		}
		if text == "" {
			text = string(body)
		}
	case strings.HasPrefix(mediaType, "text/"), strings.Contains(mediaType, "xml"), mediaType == "":
		text = string(body)
	default:
		return failJSON(fmt.Errorf("webfetch: unsupported content type %q", mediaType))
	}
	text = strings.TrimSpace(text)
	const maxChars = 60000
	textClipped := false
	if len(text) > maxChars {
		text = text[:maxChars]
		textClipped = true
	}
	if e.Log != nil {
		e.Log("webfetch", map[string]any{
			"url": u.String(), "status": resp.StatusCode, "bytes": len(body),
			"truncated": truncated || textClipped,
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "url: %s\nstatus: %d\ncontent-type: %s\nbytes: %d\ntruncated: %v\n---\n", resp.Request.URL, resp.StatusCode, mediaType, len(body), truncated || textClipped)
	b.WriteString(text)
	return Result{Content: b.String()}
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var (
	reHTMLDrop = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript>`),
		regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg>`),
		regexp.MustCompile(`(?is)<template\b[^>]*>.*?</template>`),
		regexp.MustCompile(`(?is)<!--.*?-->`),
	}
	reTitle  = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
	reAnchor = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)
	reHead   = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]>`)
	reLi     = regexp.MustCompile(`(?is)<li\b[^>]*>(.*?)</li>`)
	reBlock  = regexp.MustCompile(`(?is)</?(p|div|section|article|header|footer|main|aside|table|tr|ul|ol|pre|blockquote|br)\b[^>]*>`)
	reTag    = regexp.MustCompile(`(?is)<[^>]+>`)
	reSpace  = regexp.MustCompile(`[ \t\f\v]+`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
)

// absoluteHref resolves one link target against the page it was found on. It
// returns "" when the target needs no rewriting: an absolute URL, a bare
// fragment, or anything the base cannot parse. A protocol-relative target ("//
// host/x") takes the base's scheme, which is how a browser reads it.
func absoluteHref(base *url.URL, href string) string {
	raw := strings.TrimSpace(html.UnescapeString(href))
	if raw == "" || base == nil || !base.IsAbs() {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.IsAbs() {
		return ""
	}
	ref := raw
	if strings.HasPrefix(ref, "//") {
		ref = base.Scheme + ":" + ref
	}
	u, err := base.Parse(ref)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.String()
}

// htmlToMarkdown reduces an HTML page to readable text. baseURL is the URL the
// page was fetched from; when it is not empty, a relative link target is
// resolved against it, so the text an operator injects with @url carries the
// same absolute targets the browser would show. An empty baseURL leaves the
// href as the page wrote it.
func htmlToMarkdown(s, baseURL string) string {
	base, _ := url.Parse(baseURL)
	title := ""
	if m := reTitle.FindStringSubmatch(s); m != nil {
		title = cleanHTMLText(m[1])
	}
	for _, re := range reHTMLDrop {
		s = re.ReplaceAllString(s, "")
	}
	s = reAnchor.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAnchor.FindStringSubmatch(m)
		if sub == nil {
			return ""
		}
		label := strings.TrimSpace(cleanHTMLText(sub[2]))
		href := sub[1]
		if label == "" {
			label = href
		}
		if target := absoluteHref(base, href); target != "" {
			href = target
		}
		return "[" + label + "](" + href + ")"
	})
	s = reHead.ReplaceAllStringFunc(s, func(m string) string {
		sub := reHead.FindStringSubmatch(m)
		if sub == nil {
			return ""
		}
		n := int(sub[1][0] - '0')
		return "\n" + strings.Repeat("#", n) + " " + strings.TrimSpace(cleanHTMLText(sub[2])) + "\n"
	})
	s = reLi.ReplaceAllStringFunc(s, func(m string) string {
		sub := reLi.FindStringSubmatch(m)
		if sub == nil {
			return ""
		}
		return "\n- " + strings.TrimSpace(cleanHTMLText(sub[1]))
	})
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = reSpace.ReplaceAllString(s, " ")
	s = reBlank.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.TrimSpace(strings.Join(lines, "\n"))
	if title != "" && !strings.HasPrefix(s, "# "+title) {
		s = "# " + title + "\n\n" + s
	}
	return s
}

func cleanHTMLText(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}
