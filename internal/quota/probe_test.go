package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kintsugi/internal/config"
)

func providerFor(baseURL string) config.Provider {
	return config.Provider{BaseURL: baseURL, Wire: "openai-completions", Offline: true}
}

func TestProbeParsesTokensShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"used": 1200, "limit": 10000, "reset": "2026-11-01"}`)
	}))
	defer srv.Close()

	u, err := Probe(context.Background(), providerFor(srv.URL), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if u.Used != 1200 || u.Limit != 10000 || u.Reset != "2026-11-01" {
		t.Fatalf("usage = %+v", u)
	}
	if !strings.Contains(u.Source, "/usage") || !strings.Contains(u.Source, "tokens") {
		t.Fatalf("source = %q", u.Source)
	}
	if u.Provider == "" {
		t.Fatal("provider label is empty")
	}
}

func TestProbeParsesSnakeCaseShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"used_tokens": 42, "limit_tokens": 100, "resets_at": "2026-12-01T00:00:00Z"}}`)
	}))
	defer srv.Close()

	u, err := Probe(context.Background(), providerFor(srv.URL), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if u.Used != 42 || u.Limit != 100 {
		t.Fatalf("usage = %+v", u)
	}
	if u.Reset != "2026-12-01T00:00:00Z" {
		t.Fatalf("reset = %q", u.Reset)
	}
}

func TestProbeParsesBalanceShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			http.NotFound(w, r) // no /usage on this provider
			return
		}
		fmt.Fprint(w, `{"is_available":true,"balance": 88000, "credits": 88000}`)
	}))
	defer srv.Close()

	u, err := Probe(context.Background(), providerFor(srv.URL), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if u.Limit != 88000 || u.Used != 0 {
		t.Fatalf("usage = %+v", u)
	}
	if !strings.Contains(u.Source, "/user/balance") || !strings.Contains(u.Source, "balance") {
		t.Fatalf("source = %q", u.Source)
	}
}

func TestProbeAllEndpoints404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such route", http.StatusNotFound)
	}))
	defer srv.Close()

	u, err := Probe(context.Background(), providerFor(srv.URL), 2*time.Second)
	if err == nil {
		t.Fatalf("404 should be an error, got %+v", u)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeUnparseableBodyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"message":"nothing to see"}`)
	}))
	defer srv.Close()
	if _, err := Probe(context.Background(), providerFor(srv.URL), 2*time.Second); err == nil {
		t.Fatal("a body without usage fields should error")
	}
}

func TestProbeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the client gives up
	}))
	defer srv.Close()

	start := time.Now()
	_, err := Probe(context.Background(), providerFor(srv.URL), 40*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %v, timeout ignored", elapsed)
	}
}

func TestProbeSendsProviderKey(t *testing.T) {
	t.Setenv("KINTSUGI_TEST_PROBE_KEY", "sekret")
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		fmt.Fprint(w, `{"used": 1, "limit": 2}`)
	}))
	defer srv.Close()

	p := providerFor(srv.URL)
	p.APIKeyEnv = "KINTSUGI_TEST_PROBE_KEY"
	if _, err := Probe(context.Background(), p, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/usage" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestProbeAnthropicWireUsesAPIKeyHeader(t *testing.T) {
	t.Setenv("KINTSUGI_TEST_PROBE_KEY", "sekret")
	var gotKey, gotVersion, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"used": 1, "limit": 2}`)
	}))
	defer srv.Close()

	p := providerFor(srv.URL)
	p.Wire = "anthropic-messages"
	p.APIKeyEnv = "KINTSUGI_TEST_PROBE_KEY"
	if _, err := Probe(context.Background(), p, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if gotKey != "sekret" || gotVersion == "" || gotAuth != "" {
		t.Fatalf("headers: key=%q version=%q auth=%q", gotKey, gotVersion, gotAuth)
	}
}

func TestProbeMissingKeyStillReportsMissingEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := providerFor(srv.URL)
	p.APIKeyEnv = "KINTSUGI_TEST_PROBE_ABSENT"
	if _, err := Probe(context.Background(), p, 2*time.Second); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "KINTSUGI_TEST_PROBE_ABSENT") {
		t.Fatalf("err should name the missing env var: %v", err)
	}
}

func TestParseLiveUsageLimitAndBalance(t *testing.T) {
	// A cap plus a remaining amount implies used = limit - balance.
	u, err := parseLiveUsage([]byte(`{"limit_tokens": 100, "balance": 30}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.Limit != 100 || u.Used != 70 {
		t.Fatalf("usage = %+v", u)
	}
	if _, err := parseLiveUsage([]byte(`not json`)); err == nil {
		t.Fatal("bad json accepted")
	}
}
