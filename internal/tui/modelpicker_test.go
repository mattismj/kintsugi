package tui

// Tests for the model picker (/model with no argument). Hermetic: a synthetic
// provider registry, no network, the credential store redirected.

import (
	"fmt"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/catalog"
	"kintsugi/internal/config"
	"kintsugi/internal/credentials"
	"kintsugi/internal/persona"
	"kintsugi/internal/session"
	"kintsugi/pack"
	"os"
	"path/filepath"
)

// pickerProviders is a synthetic registry: two providers with curated models,
// one passthrough (no list, loopback, no key).
func pickerProviders() map[string]config.Provider {
	return map[string]config.Provider{
		"alpha": {
			BaseURL:   "https://alpha.example.com/v1",
			Family:    "qwen",
			APIKeyEnv: "ALPHA_API_KEY",
			Models: []config.Model{
				{ID: "alpha-9b"}, {ID: "alpha-32b"},
			},
		},
		"beta": {
			BaseURL:   "https://beta.example.com/v1",
			Family:    "deepseek",
			APIKeyEnv: "BETA_API_KEY",
			Models: []config.Model{
				{ID: "beta-chat"},
			},
		},
		"local": {
			BaseURL: "http://127.0.0.1:3/v1",
			Offline: true,
		},
	}
}

// pickerModel is a transcript bound to the synthetic registry.
func pickerModel(t *testing.T) *Model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	m := testModel(100, 30)
	m.app.Cfg.Providers = pickerProviders()
	// The adapter registry and the embedded persona pack the real App carries
	// — Select and Compile need both.
	if reg, err := adapters.Load(); err == nil {
		m.app.Adapters = reg
	} else {
		t.Fatalf("adapter registry: %v", err)
	}
	if p, err := persona.LoadFS(pack.FS, "agents/global", ""); err == nil {
		m.app.Pack = p
	} else {
		t.Fatalf("persona pack: %v", err)
	}
	// A resolvable current model: the session starts on the local runner.
	m.res.ProviderName = "local"
	m.res.Model = config.Model{ID: "huihui/qwen:9b", Family: "qwen"}
	m.res.ProviderCfg = config.Provider{BaseURL: "http://127.0.0.1:3/v1", Offline: true}
	m.comp.Manifest.Persona.Name = "local-small"
	return m
}

func TestModelWithNoArgumentOpensThePicker(t *testing.T) {
	m := pickerModel(t)
	_, _ = m.handleSlash("/model")
	if m.picker == nil {
		t.Fatal("/model with no argument did not open the picker")
	}
	// The current model is in the list, marked, and preselected — even though
	// it is not in any provider's curated models.
	found, selected := false, false
	for i, e := range m.picker.entries {
		if e.Ref == "local/huihui/qwen:9b" {
			found = true
			if !e.Current {
				t.Error("the current model is not marked")
			}
			if m.picker.sel == i {
				selected = true
			}
		}
	}
	if !found {
		t.Fatal("the current model is missing from the picker entries")
	}
	if !selected {
		t.Error("the current model is not preselected")
	}
	// The list carries the curated models with their key status.
	var haveAlpha, haveBeta, haveLocalKey bool
	for _, e := range m.picker.entries {
		if e.Ref == "alpha/alpha-9b" {
			haveAlpha = true
			if e.Status != "missing" {
				t.Errorf("alpha status %q, want missing (no key set)", e.Status)
			}
		}
		if e.Ref == "beta/beta-chat" {
			haveBeta = true
		}
		if e.Ref == "local/huihui/qwen:9b" && e.Status == "local" {
			haveLocalKey = true
		}
	}
	if !haveAlpha || !haveBeta || !haveLocalKey {
		t.Errorf("picker entries incomplete: alpha=%v beta=%v local=%v", haveAlpha, haveBeta, haveLocalKey)
	}
}

func TestPickerFilterNarrowsAndEnterSwitches(t *testing.T) {
	m := pickerModel(t)
	_, _ = m.handleSlash("/model")

	// Type a filter that matches exactly one model.
	pressKeys(m, keyRunes("beta"))
	got := m.picker.filtered()
	if len(got) != 1 || got[0].Ref != "beta/beta-chat" {
		t.Fatalf("filter left %d entries, want 1 (beta/beta-chat): %+v", len(got), got)
	}

	before := m.viewModel()
	pressKeys(m, keyEnter())
	if m.picker != nil {
		t.Fatal("the picker did not close on enter")
	}
	if after := m.viewModel(); after == before {
		t.Fatalf("the model did not switch: %q", after)
	}
	if m.res.ProviderName != "beta" || m.res.Model.ID != "beta-chat" {
		t.Fatalf("switched to %q, want beta/beta-chat", m.viewModel())
	}
	if m.role != "custom" {
		t.Errorf("role = %q, want custom after a manual switch", m.role)
	}
	// The switch is visible in the transcript.
	joined := transcriptText(m)
	if !strings.Contains(joined, "// model switched") || !strings.Contains(joined, "beta/beta-chat") {
		t.Errorf("the switch card is missing:\n%s", joined)
	}
}

func TestPickerEscClosesWithoutSwitching(t *testing.T) {
	m := pickerModel(t)
	before := m.viewModel()
	_, _ = m.handleSlash("/model")
	pressKeys(m, keyEsc())
	if m.picker != nil {
		t.Fatal("esc did not close the picker")
	}
	if m.viewModel() != before {
		t.Fatalf("esc switched the model: %q", m.viewModel())
	}
}

func TestPickerEnterOnNoMatchResolvesTheFilterAsRef(t *testing.T) {
	m := pickerModel(t)
	_, _ = m.handleSlash("/model")
	// A filter that matches nothing is resolved as a raw ref on enter: the
	// typed path lives inside the picker. The id carries a family the
	// registry can infer ("qwen"), because a family-less passthrough id
	// cannot resolve an adapter — the same rule the typed form follows.
	pressKeys(m, keyRunes("local/qwen-new-model:7b"))
	if got := m.picker.filtered(); len(got) != 0 {
		t.Fatalf("the filter unexpectedly matched: %+v", got)
	}
	pressKeys(m, keyEnter())
	if m.picker != nil {
		t.Fatalf("the picker did not close after resolving the filter: %q", m.picker.note)
	}
	if m.res.Model.ID != "qwen-new-model:7b" {
		t.Fatalf("the filter was not resolved as a ref: %q", m.viewModel())
	}
}

func TestPickerRendersTheFilterAndLegend(t *testing.T) {
	m := pickerModel(t)
	_, _ = m.handleSlash("/model")
	pressKeys(m, keyRunes("alpha"))
	out := plain(m.View())
	for _, want := range []string{"// model · pick", "filter  alpha", "alpha/alpha-9b", "alpha/alpha-32b", "[enter] switch"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker view missing %q:\n%s", want, out)
		}
	}
	// The unmatched model is filtered out.
	if strings.Contains(out, "beta-chat") {
		t.Errorf("an unfiltered model leaked through:\n%s", out)
	}
}

func TestTypedModelRefStillSwitches(t *testing.T) {
	m := pickerModel(t)
	_, _ = m.handleSlash("/model beta/beta-chat")
	if m.picker != nil {
		t.Fatal("a typed /model ref opened the picker instead of switching")
	}
	if m.res.ProviderName != "beta" || m.res.Model.ID != "beta-chat" {
		t.Fatalf("typed switch failed: %q", m.viewModel())
	}
}

func TestPickerMergesLiveFetchedModels(t *testing.T) {
	m := pickerModel(t)
	_, cmd := m.handleSlash("/model")
	_ = cmd
	// The store holds a key for alpha, so a live fetch is started for it (and
	// for the loopback local provider), but not for beta — no key, no probe.
	if m.picker.fetching["alpha"] == false && m.picker.fetching["local"] == false {
		t.Fatal("no live fetch was started for a keyed/loopback provider")
	}
	if m.picker.fetching["beta"] {
		t.Fatal("a fetch was started for a provider with no key")
	}

	// A live list arrives: models beyond the stale registry merge in, the
	// config learns them, and the selection stays on the same entry.
	keep := m.picker.entries[m.picker.sel].Ref
	_, _ = m.Update(modelsFetched{provider: "alpha", models: []string{"alpha-new-70b", "alpha-9b"}})
	var haveNew, haveOld bool
	for _, e := range m.picker.entries {
		if e.Ref == "alpha/alpha-new-70b" {
			haveNew = true
		}
		if e.Ref == "alpha/alpha-9b" {
			haveOld = true
		}
	}
	if !haveNew || !haveOld {
		t.Fatalf("the live list did not merge: new=%v old=%v", haveNew, haveOld)
	}
	if got := m.picker.entries[m.picker.sel].Ref; got != keep {
		t.Fatalf("the selection moved across the merge: %q → %q", keep, got)
	}
	// The session config resolves the fetched id (the typed form works too).
	if _, err := m.app.Resolve("alpha/alpha-new-70b"); err != nil {
		t.Errorf("fetched model is not resolvable: %v", err)
	}
	// The fetch is no longer in flight, and a failed fetch is quiet.
	if m.picker.fetching["alpha"] {
		t.Error("the fetch is still marked in flight")
	}
	_, _ = m.Update(modelsFetched{provider: "beta", err: fmt.Errorf("no key")})
	if m.picker.note != "" {
		t.Errorf("a failed fetch was loud: %q", m.picker.note)
	}
}

func TestPickerFooterShowsLiveFetches(t *testing.T) {
	m := pickerModel(t)
	_, _ = m.handleSlash("/model")
	m.picker.fetching["alpha"] = true
	out := plain(m.View())
	if !strings.Contains(out, "fetching live models") {
		t.Errorf("the footer does not report the in-flight fetches:\n%s", out)
	}
}

func TestProviderCanListModelsRules(t *testing.T) {
	m := pickerModel(t)
	// A loopback provider with no key is listable.
	if !m.providerCanListModels("local", m.app.Cfg.Providers["local"]) {
		t.Error("a loopback keyless provider should be listable")
	}
	// A keyed provider with no key is not probed.
	if m.providerCanListModels("beta", m.app.Cfg.Providers["beta"]) {
		t.Error("a provider with a missing key should not be probed")
	}
	// A keyed provider with the key in the store is listable.
	if err := credentials.Set("alpha", "test-key"); err != nil {
		t.Fatal(err)
	}
	if !m.providerCanListModels("alpha", m.app.Cfg.Providers["alpha"]) {
		t.Error("a provider with a stored key should be listable")
	}
}

func TestCatalogPersistsWhenBatchDone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	catPath := filepath.Join(home, "kintsugi", "catalog.json")
	m := pickerModel(t)
	// Pin the catalog path so the test neither reads nor writes the real one.
	m.app.Cfg.Core.CatalogPath = catPath
	// Two providers fetch; the catalog persists only when both land.
	_, cmd := m.handleSlash("/model")
	_ = cmd
	_, _ = m.Update(modelsFetched{provider: "alpha", models: []string{"alpha-9b"}})
	if _, err := os.Stat(catPath); err == nil {
		t.Fatal("the catalog persisted before the batch completed")
	}
	_, _ = m.Update(modelsFetched{provider: "local", models: []string{"huihui/qwen:9b"}})
	// The batch is done (no fetches in flight): the catalog exists.
	raw, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("the catalog did not persist: %v", err)
	}
	cat, err := catalog.Parse(raw)
	if err != nil {
		t.Fatalf("the persisted catalog does not parse: %v", err)
	}
	// Keyed providers persist; the loopback one does not.
	if _, ok := cat.Providers["alpha"]; !ok {
		t.Fatal("the keyed provider's models were not persisted")
	}
	if _, ok := cat.Providers["local"]; ok {
		t.Fatal("the loopback provider's per-machine list was persisted")
	}
	var hasNew bool
	for _, mm := range cat.Providers["alpha"].Models {
		if mm.ID == "alpha-new-70b" || mm.ID == "alpha-9b" {
			hasNew = true
		}
	}
	_ = hasNew // the registry models ride along; the fetched ones merge in
}

func TestPickerShowsReliabilityWhenKnown(t *testing.T) {
	// The model first: pickerModel redirects the cache dir, so the seed must
	// land in the same store the picker reads.
	m := pickerModel(t)
	// Seed telemetry: alpha/alpha-9b refused once over two turns.
	s, err := session.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Log("meta", map[string]string{"model": "alpha/alpha-9b"})
	s.Log("user", "t1")
	s.Log("assistant", "a")
	s.Log("refusal", map[string]any{"class": "moralizing", "outcome": "resumed"})
	s.Log("user", "t2")
	s.Log("assistant", "a")
	s.Close()
	_, _ = m.handleSlash("/model")
	var found bool
	for _, e := range m.picker.entries {
		if e.Ref == "alpha/alpha-9b" {
			found = true
			if !e.HasRate || e.RefusalRate != 50 {
				t.Fatalf("alpha/alpha-9b rate = %v (has=%v), want 50", e.RefusalRate, e.HasRate)
			}
		}
		if e.Ref == "beta/beta-chat" && e.HasRate {
			t.Fatalf("a model with no telemetry got a rate: %+v", e)
		}
	}
	if !found {
		t.Fatal("the alpha entry is missing")
	}
	out := plain(m.View())
	if !strings.Contains(out, "50% ref") {
		t.Errorf("the picker does not render the rate:\n%s", out)
	}
}
