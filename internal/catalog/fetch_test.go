package catalog

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

func fetchProvider(baseURL string) config.Provider {
	return config.Provider{BaseURL: baseURL, Wire: "openai-completions", Offline: true}
}

func TestFetchOpenAIShape(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"deepseek-chat","object":"model","owned_by":"deepseek"},
			{"id":"deepseek-reasoner","object":"model","context_length":65536},
			{"id":"qwen3-thinking","context_window":131072}
		]}`)
	}))
	defer srv.Close()

	models, err := Fetch(context.Background(), fetchProvider(srv.URL), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/models" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "" {
		t.Fatalf("no api_key_env should mean no auth header, got %q", gotAuth)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	// Sorted by id.
	if models[0].ID != "deepseek-chat" || models[1].ID != "deepseek-reasoner" || models[2].ID != "qwen3-thinking" {
		t.Fatalf("order = %+v", models)
	}
	if models[1].Context != 65536 || !models[1].Reasoning {
		t.Fatalf("reasoner = %+v (context from context_length, reasoning inferred from the id)", models[1])
	}
	if models[2].Context != 131072 || !models[2].Reasoning {
		t.Fatalf("thinking model = %+v", models[2])
	}
	if models[0].Reasoning {
		t.Fatalf("plain chat model marked as reasoning: %+v", models[0])
	}
}

func TestParseModelsTolerantShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"flat array", `[{"id":"a"},{"id":"b"}]`, []string{"a", "b"}},
		{"models key", `{"models":[{"id":"a"},{"name":"b"}]}`, []string{"a", "b"}},
		{"result key", `{"result":[{"slug":"a"}]}`, []string{"a"}},
		{"object map", `{"a":{"context":8},"b":{}}`, []string{"a", "b"}},
		{"explicit reasoning", `{"data":[{"id":"m","is_reasoning":true},{"id":"n","reasoning":true}]}`, []string{"m", "n"}},
	}
	for _, c := range cases {
		models, err := ParseModels([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, m := range models {
			got = append(got, m.ID)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s: ids = %v, want %v", c.name, got, c.want)
		}
		if c.name == "explicit reasoning" {
			for _, m := range models {
				if !m.Reasoning {
					t.Fatalf("%s: %+v lost the reasoning flag", c.name, m)
				}
			}
		}
	}
	if _, err := ParseModels([]byte(`{"nope":true}`)); err == nil {
		t.Fatal("a body without a model list should error")
	}
	if _, err := ParseModels([]byte(`"a string"`)); err == nil {
		t.Fatal("a scalar body should error")
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := Fetch(context.Background(), fetchProvider(srv.URL), 2*time.Second)
	if err == nil {
		t.Fatal("500 should fail")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchSentinelKey(t *testing.T) {
	t.Setenv("KINTSUGI_TEST_FETCH_KEY", "sekret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sekret" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv.Close()

	p := fetchProvider(srv.URL)
	p.APIKeyEnv = "KINTSUGI_TEST_FETCH_KEY"
	if _, err := Fetch(context.Background(), p, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	// Anthropic-wire providers authenticate with x-api-key instead.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sekret" {
			t.Errorf("x-api-key = %q", got)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected bearer token on the anthropic wire")
		}
		fmt.Fprint(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv2.Close()
	p2 := fetchProvider(srv2.URL)
	p2.Wire = "anthropic-messages"
	p2.APIKeyEnv = "KINTSUGI_TEST_FETCH_KEY"
	if _, err := Fetch(context.Background(), p2, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestFetchMissingKeyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv.Close()
	p := fetchProvider(srv.URL)
	p.APIKeyEnv = "KINTSUGI_TEST_FETCH_ABSENT"
	if _, err := Fetch(context.Background(), p, 2*time.Second); err == nil {
		t.Fatal("a configured api_key_env that is unset should error before egress")
	} else if !strings.Contains(err.Error(), "KINTSUGI_TEST_FETCH_ABSENT") {
		t.Fatalf("err should name the env var: %v", err)
	}
}

func TestFetchEmptyListErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), fetchProvider(srv.URL), 2*time.Second); err == nil {
		t.Fatal("an empty list should error, not silently produce an empty catalog")
	}
}

func TestLookupProviderUnknown(t *testing.T) {
	cfg := config.Defaults()
	if _, err := LookupProvider(cfg, "deepseek"); err != nil {
		t.Fatalf("configured provider failed lookup: %v", err)
	}
	_, err := LookupProvider(cfg, "no-such-provider")
	if err == nil {
		t.Fatal("unknown provider accepted")
	}
	if !strings.Contains(err.Error(), "unknown provider") || !strings.Contains(err.Error(), "deepseek") {
		t.Fatalf("err should name the provider and list configured ones: %v", err)
	}
}

// Fetched models land in the same merge path as `models import`.
func TestFetchedModelsMergeIntoConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"deepseek-chat"},{"id":"brand-new-model","context_length":4096}]}`)
	}))
	defer srv.Close()
	p, err := LookupProvider(config.Defaults(), "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	p.BaseURL = srv.URL
	p.APIKeyEnv = "" // the test server needs no key
	models, err := Fetch(context.Background(), p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cat := &Catalog{Version: Version, Providers: map[string]Provider{"deepseek": {Models: toCatalogModels(models)}}}
	added := MergeInto(&cfg, cat)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (deepseek-chat already configured)", added)
	}
	found := false
	for _, m := range cfg.Providers["deepseek"].Models {
		if m.ID == "brand-new-model" && m.Context == 4096 && m.Family == "deepseek" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fetched model not merged: %+v", cfg.Providers["deepseek"].Models)
	}
}

func toCatalogModels(in []config.Model) []Model {
	out := make([]Model, 0, len(in))
	for _, m := range in {
		out = append(out, Model{ID: m.ID, Family: m.Family, Context: m.Context, Reasoning: m.Reasoning, Capabilities: m.Capabilities})
	}
	return out
}
