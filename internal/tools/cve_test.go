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
	"time"
)

// withCVEEndpoints points the security-database endpoints at local servers
// for the duration of the test.
func withCVEEndpoints(t *testing.T, nvd, osv, kev string) {
	t.Helper()
	oldNVD, oldOSV, oldKEV := nvdAPIEndpoint, osvAPIEndpoint, kevFeedURL
	nvdAPIEndpoint, osvAPIEndpoint, kevFeedURL = nvd, osv, kev
	t.Cleanup(func() {
		nvdAPIEndpoint, osvAPIEndpoint, kevFeedURL = oldNVD, oldOSV, oldKEV
	})
}

const fakeNVDBody = `{
  "vulnerabilities": [
    {
      "cve": {
        "id": "CVE-2021-44228",
        "published": "2021-12-10T10:15:09.143",
        "lastModified": "2023-11-07T03:39:22.213",
        "vulnStatus": "Analyzed",
        "descriptions": [
          {"lang": "en", "value": "Apache Log4j2 JNDI features do not protect against attacker controlled LDAP endpoints."}
        ],
        "metrics": {
          "cvssMetricV31": [
            {"source": "nvd@nist.gov", "type": "Primary", "cvssData": {"version": "3.1", "vectorString": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H", "baseScore": 10.0, "baseSeverity": "CRITICAL"}}
          ],
          "cvssMetricV40": [
            {"source": "nvd@nist.gov", "type": "Primary", "cvssData": {"version": "4.0", "vectorString": "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H", "baseScore": 9.3}}
          ]
        },
        "weaknesses": [
          {"source": "nvd@nist.gov", "type": "Primary", "description": [{"lang": "en", "value": "CWE-502"}]}
        ],
        "references": [
          {"url": "https://logging.apache.org/log4j/2.x/security.html", "source": "cve@mitre.org", "tags": ["Vendor Advisory"]}
        ]
      }
    }
  ]
}`

const fakeOSVBody = `{
  "id": "CVE-2021-44228",
  "aliases": ["GHSA-jfh8-c2jp-5v3q", "GHSA-3t8m-7267-9v6q"],
  "summary": "Remote code injection in Log4j",
  "details": "Log4j2 JNDI features are vulnerable to attacker controlled LDAP endpoints.",
  "published": "2021-12-10T00:00:00Z",
  "modified": "2023-01-01T00:00:00Z",
  "severity": [{"type": "CVSS_V3", "score": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H"}],
  "affected": [
    {
      "package": {"ecosystem": "Maven", "name": "org.apache.logging.log4j:log4j-core", "purl": "pkg:maven/org.apache.logging.log4j/log4j-core"},
      "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "2.0"}, {"fixed": "2.3.1"}, {"introduced": "2.4"}, {"fixed": "2.15.0"}]}]
    }
  ],
  "references": [{"type": "ADVISORY", "url": "https://nvd.nist.gov/vuln/detail/CVE-2021-44228"}]
}`

const fakeKEVBody = `{
  "title": "CISA Catalog of Known Exploited Vulnerabilities",
  "catalogVersion": "2026.01.02",
  "dateReleased": "2026-01-02T12:00:00.0000Z",
  "count": 2,
  "vulnerabilities": [
    {"cveID": "CVE-2021-44228", "vendorProject": "Apache", "product": "Log4j2", "vulnerabilityName": "Apache Log4j2 RCE", "dateAdded": "2021-12-10", "dueDate": "2021-12-24", "knownRansomwareCampaignUse": "Known", "requiredAction": "Apply updates", "notes": "n"},
    {"cveID": "CVE-2024-3094", "vendorProject": "xz", "product": "XZ Utils", "vulnerabilityName": "XZ Utils backdoor", "dateAdded": "2024-03-29", "dueDate": "2024-04-19", "knownRansomwareCampaignUse": "Unknown", "requiredAction": "Apply updates", "notes": ""}
  ]
}`

// cveResponse is the decoded body of a cve_lookup result.
type cveResponse struct {
	ID      string `json:"id"`
	Sources struct {
		NVD *struct {
			Provider    string `json:"provider"`
			ID          string `json:"id"`
			Description string `json:"description"`
			Published   string `json:"published"`
			Modified    string `json:"modified"`
			CVSSv3      *struct {
				Score  float64 `json:"score"`
				Vector string  `json:"vector"`
			} `json:"cvss_v3"`
			CVSSv4 *struct {
				Score  float64 `json:"score"`
				Vector string  `json:"vector"`
			} `json:"cvss_v4"`
			CWEs       []string `json:"cwes"`
			References []struct {
				URL string `json:"url"`
			} `json:"references"`
		} `json:"nvd"`
		OSV *struct {
			Provider string   `json:"provider"`
			ID       string   `json:"id"`
			Summary  string   `json:"summary"`
			Aliases  []string `json:"aliases"`
			Affected []struct {
				Package   string   `json:"package"`
				Ecosystem string   `json:"ecosystem"`
				Fixed     []string `json:"fixed"`
			} `json:"affected"`
		} `json:"osv"`
		KEV *struct {
			Provider   string `json:"provider"`
			Known      bool   `json:"known"`
			CVEID      string `json:"cve_id"`
			DueDate    string `json:"dueDate"`
			DateAdded  string `json:"dateAdded"`
			Ransomware string `json:"ransomware"`
			CacheDate  string `json:"cacheDate"`
			FromCache  bool   `json:"fromCache"`
		} `json:"kev"`
	} `json:"sources"`
	Notes []string `json:"notes"`
}

func decodeCVE(t *testing.T, res Result) cveResponse {
	t.Helper()
	var out cveResponse
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, res.Content)
	}
	return out
}

// fakeSecurityDBs starts one server per source and points the tools at it.
// The OSV handler answers 404 for ids it does not know.
func fakeSecurityDBs(t *testing.T, kevHits *atomic.Int32) (nvd, osv, kev string) {
	t.Helper()
	nvdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("cveId"), "CVE-") {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fakeNVDBody)
	}))
	t.Cleanup(nvdSrv.Close)
	osvSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/CVE-2021-44228") && !strings.HasSuffix(r.URL.Path, "/GHSA-jfh8-c2jp-5v3q") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fakeOSVBody)
	}))
	t.Cleanup(osvSrv.Close)
	kevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kevHits != nil {
			kevHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fakeKEVBody)
	}))
	t.Cleanup(kevSrv.Close)
	return nvdSrv.URL + "/rest/json/cves/2.0", osvSrv.URL + "/v1", kevSrv.URL + "/kev.json"
}

func TestCVELookupMergesTheThreeSources(t *testing.T) {
	var kevHits atomic.Int32
	nvd, osv, kev := fakeSecurityDBs(t, &kevHits)
	ws := researchWorkspace(t, nil)
	withCVEEndpoints(t, nvd, osv, kev)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	body := decodeCVE(t, res)
	if body.ID != "CVE-2021-44228" {
		t.Fatalf("id = %q", body.ID)
	}
	if body.Sources.NVD == nil {
		t.Fatalf("nvd source missing: %s", res.Content)
	}
	if !strings.Contains(body.Sources.NVD.Description, "JNDI") {
		t.Errorf("nvd description = %q", body.Sources.NVD.Description)
	}
	if body.Sources.NVD.CVSSv3 == nil || body.Sources.NVD.CVSSv3.Score != 10.0 {
		t.Errorf("cvss v3 = %+v", body.Sources.NVD.CVSSv3)
	}
	if body.Sources.NVD.CVSSv4 == nil || body.Sources.NVD.CVSSv4.Score != 9.3 {
		t.Errorf("cvss v4 = %+v", body.Sources.NVD.CVSSv4)
	}
	if len(body.Sources.NVD.CWEs) != 1 || body.Sources.NVD.CWEs[0] != "CWE-502" {
		t.Errorf("cwes = %v", body.Sources.NVD.CWEs)
	}
	if body.Sources.NVD.Published == "" || body.Sources.NVD.Modified == "" {
		t.Errorf("dates missing: %+v", body.Sources.NVD)
	}
	if len(body.Sources.NVD.References) != 1 {
		t.Errorf("references = %+v", body.Sources.NVD.References)
	}
	if body.Sources.OSV == nil {
		t.Fatalf("osv source missing: %s", res.Content)
	}
	if len(body.Sources.OSV.Affected) != 1 {
		t.Fatalf("osv affected = %+v", body.Sources.OSV.Affected)
	}
	affected := body.Sources.OSV.Affected[0]
	if affected.Package != "org.apache.logging.log4j:log4j-core" || affected.Ecosystem != "Maven" {
		t.Errorf("affected package = %+v", affected)
	}
	if len(affected.Fixed) != 2 || affected.Fixed[1] != "2.15.0" {
		t.Errorf("fixed versions = %v", affected.Fixed)
	}
	if body.Sources.KEV == nil {
		t.Fatalf("kev source missing: %s", res.Content)
	}
	if !body.Sources.KEV.Known {
		t.Errorf("kev known = false, want true")
	}
	if body.Sources.KEV.DueDate != "2021-12-24" {
		t.Errorf("kev due date = %q", body.Sources.KEV.DueDate)
	}
	if body.Sources.KEV.Ransomware != "Known" {
		t.Errorf("kev ransomware = %q", body.Sources.KEV.Ransomware)
	}
	if body.Notes == nil {
		t.Error("notes must be an array, not null")
	}
}

func TestCVELookupMissingSourceIsNullWithANote(t *testing.T) {
	var kevHits atomic.Int32
	nvd, osv, kev := fakeSecurityDBs(t, &kevHits)
	ws := researchWorkspace(t, nil)
	withCVEEndpoints(t, nvd, osv, kev)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	// OSV does not know this id: the field is null and the note says so.
	res := dispatch(t, ex, "cve_lookup", `{"id":"CVE-2099-9999"}`)
	if res.Failed {
		t.Fatalf("a missing source must not fail the turn: %+v", res)
	}
	body := decodeCVE(t, res)
	if body.Sources.OSV != nil {
		t.Fatalf("osv must be null when the source does not know the id: %s", res.Content)
	}
	if !strings.Contains(strings.Join(body.Notes, " | "), "OSV has no record") {
		t.Fatalf("notes = %v, want an honest note about OSV", body.Notes)
	}
	// The other sources still answered.
	if body.Sources.KEV == nil {
		t.Errorf("kev should still answer: %s", res.Content)
	}
}

func TestCVELookupKEVCacheIsWrittenOnceAndReused(t *testing.T) {
	var kevHits atomic.Int32
	nvd, osv, kev := fakeSecurityDBs(t, &kevHits)
	ws := researchWorkspace(t, nil)
	withCVEEndpoints(t, nvd, osv, kev)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	if kevHits.Load() != 1 {
		t.Fatalf("kev hits = %d, want 1", kevHits.Load())
	}
	cachePath := filepath.Join(ws, ".kintsugi", "cache", "kev.json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("the catalogue was not cached: %v", err)
	}
	first := decodeCVE(t, res)
	if first.Sources.KEV == nil || first.Sources.KEV.CacheDate == "" {
		t.Fatalf("the cache date is not reported: %s", res.Content)
	}

	// The second call reuses the cache: the feed is not fetched again.
	res = dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	if kevHits.Load() != 1 {
		t.Fatalf("kev hits = %d, want the cached copy to be reused", kevHits.Load())
	}
	second := decodeCVE(t, res)
	if second.Sources.KEV == nil || !second.Sources.KEV.FromCache {
		t.Fatalf("the cached catalogue was not reported as cached: %s", res.Content)
	}
	if second.Sources.KEV.CacheDate != first.Sources.KEV.CacheDate {
		t.Errorf("cache date changed: %q vs %q", second.Sources.KEV.CacheDate, first.Sources.KEV.CacheDate)
	}

	// Once the cache is older than a day the catalogue is fetched again.
	cat, err := readKEVCache(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	cat.FetchedAt = time.Now().Add(-48 * time.Hour).UTC()
	if err := writeKEVCache(cachePath, cat); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	res = dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	if kevHits.Load() != 2 {
		t.Fatalf("kev hits = %d, want a refresh after the cache went stale", kevHits.Load())
	}
}

func TestCVELookupUsesTheStaleCacheWhenTheFeedIsDown(t *testing.T) {
	var kevHits atomic.Int32
	nvd, osv, kev := fakeSecurityDBs(t, &kevHits)
	ws := researchWorkspaceTimeout(t, nil, 1)
	withCVEEndpoints(t, nvd, osv, kev)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	if res := dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`); res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	// Age the cache, then take the feed away.
	cachePath := filepath.Join(ws, ".kintsugi", "cache", "kev.json")
	cat, err := readKEVCache(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	cat.FetchedAt = time.Now().Add(-48 * time.Hour).UTC()
	if err := writeKEVCache(cachePath, cat); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	withCVEEndpoints(t, nvd, osv, "http://192.0.2.1:9/kev.json")

	res := dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	body := decodeCVE(t, res)
	if body.Sources.KEV == nil || !body.Sources.KEV.Known {
		t.Fatalf("the stale cache should still answer: %s", res.Content)
	}
	if !strings.Contains(strings.Join(body.Notes, " | "), "cached catalogue") {
		t.Errorf("notes = %v, want the stale cache to be labelled", body.Notes)
	}
}

func TestCVELookupResolvesAGHSAId(t *testing.T) {
	var kevHits atomic.Int32
	nvd, osv, kev := fakeSecurityDBs(t, &kevHits)
	ws := researchWorkspace(t, nil)
	withCVEEndpoints(t, nvd, osv, kev)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}

	res := dispatch(t, ex, "cve_lookup", `{"id":"GHSA-jfh8-c2jp-5v3q"}`)
	if res.Failed {
		t.Fatalf("cve_lookup failed: %+v", res)
	}
	body := decodeCVE(t, res)
	if body.ID != "GHSA-jfh8-c2jp-5v3q" {
		t.Errorf("id = %q", body.ID)
	}
	if body.Sources.OSV == nil {
		t.Fatalf("osv source missing: %s", res.Content)
	}
	// The CVE alias resolved from OSV feeds the NVD and KEV lookups.
	if body.Sources.NVD == nil {
		t.Fatalf("nvd source missing for a GHSA id: %s", res.Content)
	}
	if body.Sources.KEV == nil || !body.Sources.KEV.Known {
		t.Fatalf("kev source missing for a GHSA id: %s", res.Content)
	}
}

func TestCVELookupOfflineGuardRefuses(t *testing.T) {
	_, osv, kev := fakeSecurityDBs(t, nil)
	ws := researchWorkspace(t, nil)
	withCVEEndpoints(t, "http://192.0.2.1:9/rest/json/cves/2.0", osv, kev)
	offline := &Executor{Workspace: ws, Policy: Policy{Yolo: true}, Offline: true}

	res := dispatch(t, offline, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if !res.Failed || !strings.Contains(res.Content, "offline mode") {
		t.Fatalf("offline guard: %+v", res)
	}
	if strings.Contains(res.Content, "dial tcp") {
		t.Fatalf("a request was attempted before the refusal: %s", res.Content)
	}
}

func TestCVELookupRequiresApproval(t *testing.T) {
	var kevHits atomic.Int32
	nvd, osv, kev := fakeSecurityDBs(t, &kevHits)
	ws := researchWorkspace(t, nil)
	withCVEEndpoints(t, nvd, osv, kev)

	ex := &Executor{Workspace: ws, Policy: Policy{}}
	res := dispatch(t, ex, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if !res.Failed || !strings.Contains(res.Content, "requires approval") {
		t.Fatalf("approval guard: %+v", res)
	}
	if kevHits.Load() != 0 {
		t.Fatalf("the databases were contacted without approval (%d kev hits)", kevHits.Load())
	}

	asked := 0
	ex2 := &Executor{
		Workspace: ws,
		Policy:    Policy{},
		Approver: ApproverFunc(func(ar ApprovalRequest) bool {
			asked++
			return true
		}),
	}
	res = dispatch(t, ex2, "cve_lookup", `{"id":"CVE-2021-44228"}`)
	if res.Failed {
		t.Fatalf("approved cve_lookup failed: %+v", res)
	}
	if asked != 1 {
		t.Fatalf("approval asked %d times, want 1", asked)
	}
}

func TestCVELookupRejectsAnUnusableID(t *testing.T) {
	ws := researchWorkspace(t, nil)
	ex := &Executor{Workspace: ws, Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "cve_lookup", `{"id":"not-a-vulnerability"}`)
	if !res.Failed || !strings.Contains(res.Content, "not a CVE or GHSA id") {
		t.Fatalf("id validation: %+v", res)
	}
}
