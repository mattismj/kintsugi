package main

// Tests for `kintsugi connect`. The credential store is faked and the provider
// endpoint is an httptest server, so nothing here reads or writes the real
// 0600 file and nothing leaves the loopback interface.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kintsugi/internal/config"
)

// fakeStore is an in-memory credStore.
type fakeStore struct {
	saved   map[string]string
	deleted []string
	status  map[string]string
	failDel error
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: map[string]string{}, status: map[string]string{}}
}

func (f *fakeStore) Path() string { return "/fake/credentials.json" }

func (f *fakeStore) Set(provider, key string) error {
	f.saved[provider] = key
	return nil
}

func (f *fakeStore) Delete(provider string) error {
	if f.failDel != nil {
		return f.failDel
	}
	f.deleted = append(f.deleted, provider)
	delete(f.saved, provider)
	return nil
}

func (f *fakeStore) Status(provider, envName string) string {
	if s, ok := f.status[provider]; ok {
		return s
	}
	return "missing"
}

// connectTestConfig is a two-provider config: a cloud endpoint that needs a key
// and a loopback runner that does not.
func connectTestConfig(baseURL string) config.File {
	cfg := config.Defaults()
	cfg.Core.Mode = "normal"
	cfg.Core.DefaultModel = "mock/mock-chat"
	cfg.Providers = map[string]config.Provider{
		"mock": {
			BaseURL:   baseURL,
			APIKeyEnv: "MOCK_API_KEY",
			Family:    "mock",
			Models:    []config.Model{{ID: "mock-chat"}, {ID: "mock-pro"}},
		},
		"local": {
			BaseURL:   "http://127.0.0.1:11434/v1",
			APIKeyEnv: "LOCAL_API_KEY",
			Family:    "local",
		},
	}
	return cfg
}

// keyServer answers /models, accepting only the exact bearer token.
func keyServer(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-chat"}]}`))
	}))
}

func TestConnectVerifySuccessSaves(t *testing.T) {
	srv := keyServer(t, "good-key")
	defer srv.Close()

	cfg := connectTestConfig(srv.URL)
	p := cfg.Providers["mock"]
	store := newFakeStore()

	if err := connectApply(cfg, "mock", p, "  good-key \n", true, store, 2*time.Second); err != nil {
		t.Fatalf("connectApply: %v", err)
	}
	if got := store.saved["mock"]; got != "good-key" {
		t.Fatalf("saved key = %q, want %q (trimmed)", got, "good-key")
	}
}

func TestConnectVerifyFailureDoesNotSave(t *testing.T) {
	srv := keyServer(t, "good-key")
	defer srv.Close()

	cfg := connectTestConfig(srv.URL)
	store := newFakeStore()

	err := connectApply(cfg, "mock", cfg.Providers["mock"], "bad-key", true, store, 2*time.Second)
	if err == nil {
		t.Fatal("verify failure was not reported")
	}
	if _, ok := store.saved["mock"]; ok {
		t.Fatalf("key was saved despite a failed verification: %q", store.saved["mock"])
	}
	// The provider's own message is surfaced; the candidate key never is.
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error does not carry the provider status: %v", err)
	}
	if strings.Contains(err.Error(), "bad-key") {
		t.Errorf("error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "--no-verify") {
		t.Errorf("error does not point at --no-verify: %v", err)
	}
}

func TestConnectNoVerifySavesUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // endpoint is now unreachable

	cfg := connectTestConfig(url)
	store := newFakeStore()

	if err := connectApply(cfg, "mock", cfg.Providers["mock"], "unverified-key", false, store, 500*time.Millisecond); err != nil {
		t.Fatalf("--no-verify should save without a probe: %v", err)
	}
	if got := store.saved["mock"]; got != "unverified-key" {
		t.Fatalf("saved key = %q", got)
	}
}

func TestConnectEmptyKeyNotSaved(t *testing.T) {
	cfg := connectTestConfig("https://example.invalid/v1")
	store := newFakeStore()
	for _, key := range []string{"", "   ", "\n"} {
		if err := connectApply(cfg, "mock", cfg.Providers["mock"], key, false, store, time.Second); err == nil {
			t.Fatalf("empty key %q was accepted", key)
		}
	}
	if len(store.saved) != 0 {
		t.Fatalf("nothing should be saved: %v", store.saved)
	}
}

func TestConnectDelete(t *testing.T) {
	store := newFakeStore()
	store.saved["mock"] = "old-key"
	if err := connectDelete("mock", store); err != nil {
		t.Fatalf("connectDelete: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "mock" {
		t.Fatalf("deleted = %v", store.deleted)
	}
	if _, ok := store.saved["mock"]; ok {
		t.Fatal("stored key survived --delete")
	}

	store.failDel = errors.New("permission denied")
	if err := connectDelete("mock", store); err == nil || !strings.Contains(err.Error(), "mock") {
		t.Fatalf("delete failure not reported: %v", err)
	}
}

func TestConnectUnknownProvider(t *testing.T) {
	cfg := connectTestConfig("https://example.invalid/v1")
	if _, err := connectFindProvider(cfg, "nope"); err == nil {
		t.Fatal("unknown provider accepted")
	} else {
		if !strings.Contains(err.Error(), `"nope"`) {
			t.Errorf("error does not name the provider: %v", err)
		}
		for _, want := range []string{"local", "mock"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not list configured provider %q: %v", want, err)
			}
		}
	}
	if p, err := connectFindProvider(cfg, "mock"); err != nil || p.APIKeyEnv != "MOCK_API_KEY" {
		t.Fatalf("lookup by name failed: %+v %v", p, err)
	}
}

func TestConnectNeedsKeyAndStatus(t *testing.T) {
	cfg := connectTestConfig("https://example.invalid/v1")
	store := newFakeStore()
	store.status["mock"] = "env"

	if !connectNeedsKey(cfg.Providers["mock"]) {
		t.Error("cloud provider with api_key_env should need a key")
	}
	if connectNeedsKey(cfg.Providers["local"]) {
		t.Error("loopback provider should not need a key")
	}
	noEnv := config.Provider{BaseURL: "https://example.invalid/v1"}
	if connectNeedsKey(noEnv) {
		t.Error("provider without api_key_env should not need a key")
	}

	cases := []struct {
		name   string
		prov   config.Provider
		status string
		want   string
	}{
		{"mock", cfg.Providers["mock"], "env", "env"},
		{"mock", cfg.Providers["mock"], "stored", "stored"},
		{"mock", cfg.Providers["mock"], "missing", "missing"},
		{"local", cfg.Providers["local"], "stored", "not required"},
	}
	for _, c := range cases {
		store.status[c.name] = c.status
		if got := connectAuthStatus(c.name, c.prov, store); got != c.want {
			t.Errorf("connectAuthStatus(%s, %s) = %q, want %q", c.name, c.status, got, c.want)
		}
	}
}

func TestConnectModelChoice(t *testing.T) {
	cfg := connectTestConfig("https://example.invalid/v1")
	if got := connectModelChoice(cfg, "mock", cfg.Providers["mock"]); got != "mock-chat" {
		t.Errorf("default model for the provider = %q", got)
	}
	// A default model belonging to another provider falls back to the first
	// curated model of this one.
	cfg.Core.DefaultModel = "local/llama3"
	if got := connectModelChoice(cfg, "mock", cfg.Providers["mock"]); got != "mock-chat" {
		t.Errorf("fallback model = %q", got)
	}
	// Nothing configured at all: Ping only needs the endpoint.
	if got := connectModelChoice(cfg, "local", cfg.Providers["local"]); got != "llama3" {
		t.Errorf("default-model reuse = %q", got)
	}
}

func TestConnectReadKeyStdin(t *testing.T) {
	key, err := connectReadKey("mock", true, strings.NewReader("  stdin-key  \nignored-second-line\n"))
	if err != nil {
		t.Fatalf("connectReadKey: %v", err)
	}
	if key != "stdin-key" {
		t.Fatalf("key = %q, want %q", key, "stdin-key")
	}
	// One line may arrive without a trailing newline.
	key, err = connectReadKey("mock", true, strings.NewReader("no-newline"))
	if err != nil || key != "no-newline" {
		t.Fatalf("key = %q err = %v", key, err)
	}
	if key, err := connectReadKey("mock", true, strings.NewReader("\n")); err != nil || key != "" {
		t.Fatalf("blank line = %q err = %v", key, err)
	}
}

// TestConnectListFormatsStatuses exercises the --list rendering without any
// credentials package side effect: the fake store answers status (and Path), so
// no file is read.
func TestConnectListFormatsStatuses(t *testing.T) {
	cfg := connectTestConfig("https://example.invalid/v1")
	store := newFakeStore()
	store.status["mock"] = "stored"

	rows := map[string]string{}
	for _, name := range cfg.ProviderNames() {
		rows[name] = connectAuthStatus(name, cfg.Providers[name], store)
	}
	if rows["mock"] != "stored" {
		t.Errorf("mock status = %q", rows["mock"])
	}
	if rows["local"] != "not required" {
		t.Errorf("loopback status = %q", rows["local"])
	}
	if got := store.Path(); got == "" {
		t.Error("store path should be printable")
	}
}
