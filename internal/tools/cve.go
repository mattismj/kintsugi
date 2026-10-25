// The cve_lookup tool: the research surface's security half (M8, IDEAS I-19).
//
// One vulnerability id, three independent databases, merged into one answer:
//
//   - NVD 2.0 for the description, the CVSS scores and the CWE list,
//   - OSV for the affected packages, their ecosystems and the fixed versions,
//   - the CISA Known Exploited Vulnerabilities catalogue for the question an
//     operator actually asks — "is this one exploited?".
//
// The three sources are independent and are reported independently. A source
// that could not be reached, or that does not know the id, is null with a
// note: the tool never guesses a severity, never infers a fix and never
// fills a missing field from another source.
//
// The CISA catalogue is large (~1 MiB) and changes slowly, so it is fetched
// at most once a day and cached under the workspace. A stale cache is still
// better than no answer, and it is always labelled with the date it was
// fetched.
//
// The bounds, the offline guard and the permission gate are the ones
// websearch and webfetch already use: a timeout, a small redirect cap, a
// response cap, a fixed user agent, no non-loopback host while
// core.mode = "offline", and an approval for every egress.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
	"kintsugi/internal/redact"
)

const (
	// kevCacheTTL is how long a fetched catalogue stays fresh. The feed is
	// fetched once a day and cached in between.
	kevCacheTTL = 24 * time.Hour
	// kevCachePath is where the catalogue is cached, relative to the
	// workspace.
	kevCachePath = ".kintsugi/cache/kev.json"
	// kevFeedMaxBytes caps the catalogue download. The feed is about a
	// megabyte, which is over the general response cap, so it gets its own.
	kevFeedMaxBytes = 8 << 20
	// cveMaxBytes caps one API response.
	cveMaxBytes = 4 << 20
	// cveMaxReferences caps the reference list carried per source.
	cveMaxReferences = 20
	// cveMaxDetails clips a long advisory body.
	cveMaxDetails = 2000
)

// The source endpoints. Like the search backends they are fixed in this
// file: the id chooses the record, never the host.
var (
	nvdAPIEndpoint = "https://services.nvd.nist.gov/rest/json/cves/2.0"
	osvAPIEndpoint = "https://api.osv.dev/v1"
	kevFeedURL     = "https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json"
)

var (
	reCVEID  = regexp.MustCompile(`^CVE-\d{4}-\d{4,}$`)
	reGHSAID = regexp.MustCompile(`(?i)^GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$`)
)

// cveLookupTool is the schema for the vulnerability-lookup tool.
func cveLookupTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "cve_lookup",
			Description: "Look up a vulnerability id (CVE-… or GHSA-…) in NVD, OSV and the CISA Known Exploited Vulnerabilities catalogue, and return the merged record with per-source attribution. A source that is unavailable or does not know the id is reported as null with a note — never guessed. Network access requires operator approval and is blocked for non-loopback hosts in offline mode.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"id"},
				Properties: map[string]provider.ToolProp{
					"id": {Type: "string", Description: "Vulnerability id, e.g. \"CVE-2021-44228\" or \"GHSA-jfh8-c2jp-5v3q\"."},
				},
			},
		},
	}
}

// cveLookupResult is the tool's answer.
type cveLookupResult struct {
	ID      string     `json:"id"`
	Sources cveSources `json:"sources"`
	Notes   []string   `json:"notes"`
}

// cveSources holds one entry per source. A nil entry is null in JSON: the
// source did not answer.
type cveSources struct {
	NVD *nvdSource `json:"nvd"`
	OSV *osvSource `json:"osv"`
	KEV *kevSource `json:"kev"`
}

// cvssScore is one severity record.
type cvssScore struct {
	Score    float64 `json:"score"`
	Vector   string  `json:"vector"`
	Severity string  `json:"severity,omitempty"`
	Version  string  `json:"version,omitempty"`
	Source   string  `json:"source,omitempty"`
}

// nvdRef is one reference URL.
type nvdRef struct {
	URL  string   `json:"url"`
	Tags []string `json:"tags,omitempty"`
}

// nvdSource is the NVD half of the answer.
type nvdSource struct {
	Provider    string     `json:"provider"`
	URL         string     `json:"url"`
	ID          string     `json:"id"`
	Description string     `json:"description,omitempty"`
	Published   string     `json:"published,omitempty"`
	Modified    string     `json:"modified,omitempty"`
	Status      string     `json:"status,omitempty"`
	CVSSv3      *cvssScore `json:"cvss_v3,omitempty"`
	CVSSv4      *cvssScore `json:"cvss_v4,omitempty"`
	CWEs        []string   `json:"cwes"`
	References  []nvdRef   `json:"references"`
}

// osvAffected is one affected package.
type osvAffected struct {
	Package    string   `json:"package"`
	Ecosystem  string   `json:"ecosystem,omitempty"`
	PURL       string   `json:"purl,omitempty"`
	Introduced string   `json:"introduced,omitempty"`
	Fixed      []string `json:"fixed,omitempty"`
}

// osvRef is one reference URL.
type osvRef struct {
	Type string `json:"type,omitempty"`
	URL  string `json:"url"`
}

// osvSource is the OSV half of the answer.
type osvSource struct {
	Provider   string        `json:"provider"`
	URL        string        `json:"url"`
	ID         string        `json:"id"`
	Summary    string        `json:"summary,omitempty"`
	Details    string        `json:"details,omitempty"`
	Aliases    []string      `json:"aliases"`
	Published  string        `json:"published,omitempty"`
	Modified   string        `json:"modified,omitempty"`
	Withdrawn  string        `json:"withdrawn,omitempty"`
	Severity   []string      `json:"severity,omitempty"`
	Affected   []osvAffected `json:"affected"`
	References []osvRef      `json:"references,omitempty"`
}

// kevSource is the CISA KEV half of the answer: whether the id is in the
// known-exploited catalogue, and when the operator has to act.
type kevSource struct {
	Provider      string `json:"provider"`
	Source        string `json:"source"`
	Known         bool   `json:"known"`
	CVEID         string `json:"cve_id,omitempty"`
	DueDate       string `json:"dueDate,omitempty"`
	DateAdded     string `json:"dateAdded,omitempty"`
	Ransomware    string `json:"ransomware,omitempty"`
	VendorProject string `json:"vendorProject,omitempty"`
	Product       string `json:"product,omitempty"`
	CatalogueDate string `json:"catalogueDate,omitempty"`
	CacheDate     string `json:"cacheDate,omitempty"`
	FromCache     bool   `json:"fromCache,omitempty"`
}

// cveLookup merges the security databases for one vulnerability id.
func (e *Executor) cveLookup(ctx context.Context, args string) Result {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	raw := strings.TrimSpace(a.ID)
	if raw == "" {
		return failJSON(fmt.Errorf("cve_lookup: missing required property 'id'"))
	}
	id, ok := normalizeVulnID(raw)
	if !ok {
		return failJSON(fmt.Errorf("cve_lookup: %q is not a CVE or GHSA id", raw))
	}

	cfg := researchConfigFor(e.Workspace)
	nvdHost := endpointHost(nvdAPIEndpoint)
	osvHost := endpointHost(osvAPIEndpoint)
	kevHost := endpointHost(kevFeedURL)
	nvdAllowed := hostInScope(cfg.Scope, nvdHost)
	osvAllowed := hostInScope(cfg.Scope, osvHost)
	kevAllowed := hostInScope(cfg.Scope, kevHost)

	// The offline guard runs before any request: while core.mode is
	// "offline" a non-loopback host is refused, exactly as webfetch refuses
	// one. Every source is checked, so nothing is contacted before the
	// refusal.
	if e.Offline {
		for _, host := range []string{nvdHost, osvHost, kevHost} {
			if host == "" {
				continue
			}
			if !isLoopbackHost(host) {
				return failJSON(fmt.Errorf("offline mode: %s is not loopback; set core.mode = \"normal\" to allow web egress", host))
			}
		}
	}

	// Egress is a permission event, on the same path webfetch uses.
	if err := e.authorize("cve_lookup", id, "cve_lookup: "+id); err != nil {
		return failJSON(err)
	}

	out := cveLookupResult{ID: id, Notes: []string{}}
	isGHSA := strings.HasPrefix(id, "GHSA-")
	cveID := ""

	// A CVE id is what NVD indexes, so NVD is asked first. A GHSA id is
	// what OSV indexes, so OSV is asked first there and its CVE alias
	// feeds the other two sources.
	if !isGHSA && nvdAllowed {
		cveID = id
		nvd, note := e.queryNVD(ctx, cveID, cfg.Research)
		if nvd != nil {
			out.Sources.NVD = nvd
		}
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
	} else if !nvdAllowed {
		out.Notes = append(out.Notes, "NVD is outside the declared scope and was not contacted")
	}

	if osvAllowed {
		osv, osvNote := e.queryOSV(ctx, id, cfg.Research)
		if osv != nil {
			out.Sources.OSV = osv
		}
		if osvNote != "" {
			out.Notes = append(out.Notes, osvNote)
		}

		if cveID == "" {
			cveID = cveAliasFromOSV(osv)
			switch {
			case cveID == "":
				out.Notes = append(out.Notes, "NVD indexes CVE ids only; no CVE id could be resolved for "+id)
			case !nvdAllowed:
				out.Notes = append(out.Notes, "NVD is outside the declared scope and was not contacted")
			default:
				nvd, note := e.queryNVD(ctx, cveID, cfg.Research)
				if nvd != nil {
					out.Sources.NVD = nvd
				}
				if note != "" {
					out.Notes = append(out.Notes, note)
				}
			}
		}
	} else {
		out.Notes = append(out.Notes, "OSV is outside the declared scope and was not contacted")
	}

	kev, kevNote := e.lookupKEV(ctx, cveID, id, cfg.Research, kevAllowed)
	if kev != nil {
		out.Sources.KEV = kev
	}
	if kevNote != "" {
		out.Notes = append(out.Notes, kevNote)
	}

	b, err := json.Marshal(out)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: redact.Redact(string(b))}
}

// normalizeVulnID canonicalizes a vulnerability id. CVE ids are
// upper-cased; a GHSA id keeps its lower-case suffix, which is how OSV
// indexes it.
func normalizeVulnID(raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", false
	}
	if upper := strings.ToUpper(id); reCVEID.MatchString(upper) {
		return upper, true
	}
	if lower := strings.ToLower(id); reGHSAID.MatchString(lower) {
		return "GHSA-" + lower[len("ghsa-"):], true
	}
	return "", false
}

// cveAliasFromOSV returns the CVE id an OSV record aliases, if any.
func cveAliasFromOSV(osv *osvSource) string {
	if osv == nil {
		return ""
	}
	for _, alias := range osv.Aliases {
		if upper := strings.ToUpper(alias); reCVEID.MatchString(upper) {
			return upper
		}
	}
	if upper := strings.ToUpper(osv.ID); reCVEID.MatchString(upper) {
		return upper
	}
	return ""
}

// ---------- NVD ----------

type nvdResponse struct {
	Vulnerabilities []struct {
		CVE nvdCVE `json:"cve"`
	} `json:"vulnerabilities"`
}

type nvdCVE struct {
	ID           string `json:"id"`
	Published    string `json:"published"`
	LastModified string `json:"lastModified"`
	VulnStatus   string `json:"vulnStatus"`
	Descriptions []struct {
		Lang  string `json:"lang"`
		Value string `json:"value"`
	} `json:"descriptions"`
	Metrics struct {
		V31 []nvdMetric `json:"cvssMetricV31"`
		V30 []nvdMetric `json:"cvssMetricV30"`
		V40 []nvdMetric `json:"cvssMetricV40"`
	} `json:"metrics"`
	Weaknesses []struct {
		Description []struct {
			Lang  string `json:"lang"`
			Value string `json:"value"`
		} `json:"description"`
	} `json:"weaknesses"`
	References []struct {
		URL  string   `json:"url"`
		Tags []string `json:"tags"`
	} `json:"references"`
}

type nvdMetric struct {
	Source   string `json:"source"`
	Type     string `json:"type"`
	CvssData struct {
		Version      string  `json:"version"`
		VectorString string  `json:"vectorString"`
		BaseScore    float64 `json:"baseScore"`
		BaseSeverity string  `json:"baseSeverity"`
	} `json:"cvssData"`
}

// queryNVD asks NVD for one CVE record. It returns the record, or a note
// explaining why there is none.
func (e *Executor) queryNVD(ctx context.Context, cveID string, cfg config.Research) (*nvdSource, string) {
	u, err := url.Parse(nvdAPIEndpoint)
	if err != nil {
		return nil, "NVD endpoint is not a URL"
	}
	q := u.Query()
	q.Set("cveId", cveID)
	u.RawQuery = q.Encode()

	status, body, err := e.researchFetch(ctx, http.MethodGet, u.String(), nil, nil, cfg.Timeout(), cveMaxBytes)
	if err != nil {
		return nil, "NVD unavailable: " + err.Error()
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Sprintf("NVD unavailable: status %d", status)
	}
	var resp nvdResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "NVD returned a malformed response"
	}
	if len(resp.Vulnerabilities) == 0 {
		return nil, "NVD has no record for " + cveID
	}
	cve := resp.Vulnerabilities[0].CVE

	src := &nvdSource{
		Provider:   "NVD",
		URL:        u.String(),
		ID:         firstNonEmpty(cve.ID, cveID),
		Published:  cve.Published,
		Modified:   cve.LastModified,
		Status:     cve.VulnStatus,
		CWEs:       []string{},
		References: []nvdRef{},
	}
	src.Description = clip(nvdDescription(cve.Descriptions), cveMaxDetails)
	src.CVSSv3 = pickCVSS(cve.Metrics.V31)
	if src.CVSSv3 == nil {
		src.CVSSv3 = pickCVSS(cve.Metrics.V30)
	}
	src.CVSSv4 = pickCVSS(cve.Metrics.V40)

	seen := map[string]bool{}
	for _, w := range cve.Weaknesses {
		for _, d := range w.Description {
			v := strings.TrimSpace(d.Value)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			src.CWEs = append(src.CWEs, v)
		}
	}
	for _, r := range cve.References {
		if len(src.References) >= cveMaxReferences {
			break
		}
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		src.References = append(src.References, nvdRef{URL: r.URL, Tags: r.Tags})
	}
	return src, ""
}

// nvdDescription picks the English description, falling back to the first.
func nvdDescription(descriptions []struct {
	Lang  string `json:"lang"`
	Value string `json:"value"`
}) string {
	for _, d := range descriptions {
		if strings.EqualFold(d.Lang, "en") && strings.TrimSpace(d.Value) != "" {
			return d.Value
		}
	}
	for _, d := range descriptions {
		if strings.TrimSpace(d.Value) != "" {
			return d.Value
		}
	}
	return ""
}

// pickCVSS returns the first usable severity record of a metric list.
func pickCVSS(metrics []nvdMetric) *cvssScore {
	for _, m := range metrics {
		if m.CvssData.VectorString == "" && m.CvssData.BaseScore == 0 {
			continue
		}
		return &cvssScore{
			Score:    m.CvssData.BaseScore,
			Vector:   m.CvssData.VectorString,
			Severity: m.CvssData.BaseSeverity,
			Version:  m.CvssData.Version,
			Source:   m.Source,
		}
	}
	return nil
}

// ---------- OSV ----------

type osvResponse struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases"`
	Summary   string   `json:"summary"`
	Details   string   `json:"details"`
	Published string   `json:"published"`
	Modified  string   `json:"modified"`
	Withdrawn string   `json:"withdrawn"`
	Severity  []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
			PURL      string `json:"purl"`
		} `json:"package"`
		Ranges []struct {
			Type   string `json:"type"`
			Events []struct {
				Introduced   string `json:"introduced"`
				Fixed        string `json:"fixed"`
				LastAffected string `json:"last_affected"`
			} `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
	References []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"references"`
}

// queryOSV asks OSV about one vulnerability id.
func (e *Executor) queryOSV(ctx context.Context, id string, cfg config.Research) (*osvSource, string) {
	endpoint := strings.TrimRight(osvAPIEndpoint, "/") + "/vulns/" + url.PathEscape(id)
	status, body, err := e.researchFetch(ctx, http.MethodGet, endpoint, nil, nil, cfg.Timeout(), cveMaxBytes)
	if err != nil {
		return nil, "OSV unavailable: " + err.Error()
	}
	if status == http.StatusNotFound {
		return nil, "OSV has no record for " + id
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Sprintf("OSV unavailable: status %d", status)
	}
	var resp osvResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "OSV returned a malformed response"
	}
	if strings.TrimSpace(resp.ID) == "" {
		return nil, "OSV has no record for " + id
	}

	src := &osvSource{
		Provider:  "OSV",
		URL:       endpoint,
		ID:        resp.ID,
		Summary:   strings.TrimSpace(resp.Summary),
		Details:   clip(strings.TrimSpace(resp.Details), cveMaxDetails),
		Aliases:   resp.Aliases,
		Published: resp.Published,
		Modified:  resp.Modified,
		Withdrawn: resp.Withdrawn,
		Affected:  []osvAffected{},
	}
	if src.Aliases == nil {
		src.Aliases = []string{}
	}
	for _, s := range resp.Severity {
		if strings.TrimSpace(s.Score) == "" {
			continue
		}
		src.Severity = append(src.Severity, s.Type+": "+s.Score)
	}
	for _, a := range resp.Affected {
		entry := osvAffected{
			Package:   a.Package.Name,
			Ecosystem: a.Package.Ecosystem,
			PURL:      a.Package.PURL,
		}
		fixed := map[string]bool{}
		for _, r := range a.Ranges {
			for _, ev := range r.Events {
				if ev.Introduced != "" && entry.Introduced == "" {
					entry.Introduced = ev.Introduced
				}
				if ev.Fixed != "" && !fixed[ev.Fixed] {
					fixed[ev.Fixed] = true
					entry.Fixed = append(entry.Fixed, ev.Fixed)
				}
			}
		}
		src.Affected = append(src.Affected, entry)
	}
	for _, r := range resp.References {
		if len(src.References) >= cveMaxReferences {
			break
		}
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		src.References = append(src.References, osvRef{Type: r.Type, URL: r.URL})
	}
	return src, ""
}

// ---------- CISA KEV ----------

// kevEntry is one record of the known-exploited catalogue.
type kevEntry struct {
	CVEID                      string `json:"cveID"`
	VendorProject              string `json:"vendorProject"`
	Product                    string `json:"product"`
	VulnerabilityName          string `json:"vulnerabilityName"`
	DateAdded                  string `json:"dateAdded"`
	DueDate                    string `json:"dueDate"`
	KnownRansomwareCampaignUse string `json:"knownRansomwareCampaignUse"`
	RequiredAction             string `json:"requiredAction"`
	Notes                      string `json:"notes"`
}

// kevCatalogue is the cached feed plus the provenance of the copy.
type kevCatalogue struct {
	FetchedAt       time.Time  `json:"fetched_at"`
	Source          string     `json:"source"`
	CatalogVersion  string     `json:"catalog_version,omitempty"`
	DateReleased    string     `json:"date_released,omitempty"`
	Vulnerabilities []kevEntry `json:"vulnerabilities"`
}

// lookupKEV answers whether the id is in the known-exploited catalogue.
// allowed reports whether the feed host may be contacted under the
// operator's declared scope; a cached catalogue is still usable without it.
func (e *Executor) lookupKEV(ctx context.Context, cveID, rawID string, cfg config.Research, allowed bool) (*kevSource, string) {
	cat, note, fromCache, _ := e.loadKEVCatalogue(ctx, cfg, allowed)
	if cat == nil {
		return nil, note
	}
	base := &kevSource{
		Provider:      "CISA KEV",
		Source:        cat.Source,
		CatalogueDate: cat.DateReleased,
		CacheDate:     cat.FetchedAt.Format(time.RFC3339),
		FromCache:     fromCache,
	}
	ids := []string{}
	if cveID != "" {
		ids = append(ids, cveID)
	}
	if rawID != "" {
		ids = append(ids, rawID)
	}
	for _, id := range ids {
		for _, v := range cat.Vulnerabilities {
			if !strings.EqualFold(strings.TrimSpace(v.CVEID), id) {
				continue
			}
			found := *base
			found.Known = true
			found.CVEID = v.CVEID
			found.DueDate = v.DueDate
			found.DateAdded = v.DateAdded
			found.Ransomware = v.KnownRansomwareCampaignUse
			found.VendorProject = v.VendorProject
			found.Product = v.Product
			return &found, note
		}
	}
	return base, note
}

// loadKEVCatalogue returns the catalogue, from the local cache when it is
// fresh and from CISA otherwise. A stale cache is used when the feed cannot
// be reached, and it is always labelled with the date it was fetched.
func (e *Executor) loadKEVCatalogue(ctx context.Context, cfg config.Research, allowed bool) (*kevCatalogue, string, bool, error) {
	path := ""
	if e.Workspace != "" {
		path = filepath.Join(e.Workspace, filepath.FromSlash(kevCachePath))
	}
	cached, cacheErr := readKEVCache(path)
	if cacheErr == nil && cached != nil {
		if time.Since(cached.FetchedAt) < kevCacheTTL {
			return cached, fmt.Sprintf("CISA KEV catalogue served from the local cache, fetched %s", cached.FetchedAt.Format(time.RFC3339)), true, nil
		}
	}
	if !allowed {
		if cached != nil {
			return cached, fmt.Sprintf("CISA KEV feed is outside the declared scope; using the cached catalogue from %s", cached.FetchedAt.Format(time.RFC3339)), true, nil
		}
		return nil, "CISA KEV is outside the declared scope and was not contacted", false, nil
	}

	status, body, err := e.researchFetch(ctx, http.MethodGet, kevFeedURL, nil, nil, cfg.Timeout(), kevFeedMaxBytes)
	if err != nil {
		if cached != nil {
			return cached, fmt.Sprintf("CISA KEV feed unavailable (%v); using the cached catalogue from %s", err, cached.FetchedAt.Format(time.RFC3339)), true, nil
		}
		return nil, fmt.Sprintf("CISA KEV unavailable: %v", err), false, err
	}
	if status < 200 || status >= 300 {
		if cached != nil {
			return cached, fmt.Sprintf("CISA KEV feed returned status %d; using the cached catalogue from %s", status, cached.FetchedAt.Format(time.RFC3339)), true, nil
		}
		return nil, fmt.Sprintf("CISA KEV unavailable: status %d", status), false, nil
	}
	cat, err := parseKEVFeed(body)
	if err != nil {
		if cached != nil {
			return cached, fmt.Sprintf("CISA KEV feed could not be read (%v); using the cached catalogue from %s", err, cached.FetchedAt.Format(time.RFC3339)), true, nil
		}
		return nil, fmt.Sprintf("CISA KEV feed could not be read: %v", err), false, err
	}
	if path != "" {
		// A cache that cannot be written is not a failed lookup.
		_ = writeKEVCache(path, cat)
	}
	return cat, fmt.Sprintf("CISA KEV catalogue fetched %s", cat.FetchedAt.Format(time.RFC3339)), false, nil
}

// parseKEVFeed reads the catalogue feed.
func parseKEVFeed(body []byte) (*kevCatalogue, error) {
	var feed struct {
		Title           string     `json:"title"`
		CatalogVersion  string     `json:"catalogVersion"`
		DateReleased    string     `json:"dateReleased"`
		Count           int        `json:"count"`
		Vulnerabilities []kevEntry `json:"vulnerabilities"`
	}
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("feed is not valid JSON")
	}
	return &kevCatalogue{
		FetchedAt:       time.Now().UTC(),
		Source:          kevFeedURL,
		CatalogVersion:  feed.CatalogVersion,
		DateReleased:    feed.DateReleased,
		Vulnerabilities: feed.Vulnerabilities,
	}, nil
}

// readKEVCache reads the cached catalogue, if there is one.
func readKEVCache(path string) (*kevCatalogue, error) {
	if path == "" {
		return nil, fmt.Errorf("no cache path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cat kevCatalogue
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	if cat.FetchedAt.IsZero() {
		return nil, fmt.Errorf("cached catalogue has no date")
	}
	return &cat, nil
}

// writeKEVCache stores the catalogue under the workspace. The write is
// atomic: a half-written cache would be worse than no cache.
func writeKEVCache(path string, cat *kevCatalogue) error {
	if path == "" {
		return fmt.Errorf("no cache path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
