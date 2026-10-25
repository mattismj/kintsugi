package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"kintsugi/internal/config"
)

func TestParseCanonicalAndNormalize(t *testing.T) {
	cat, err := Parse([]byte(`{"version":1,"providers":{"mock":{"models":[
		{"id":"b","family":"deepseek","context":8192},
		{"id":"a","capabilities":["tools"]},
		{"id":"a"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	models := cat.Providers["mock"].Models
	if len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Fatalf("models = %+v", models)
	}
	if models[1].Context != 8192 {
		t.Fatalf("context lost: %+v", models[1])
	}
}

func TestParseFlatAliases(t *testing.T) {
	cat, err := Parse([]byte(`[
		{"id":"deepseek-chat","provider":"deepseek","family":"deepseek","context_window":131072},
		{"name":"reasoner","provider_id":"deepseek","is_reasoning":true,"context_length":64000,"capabilities":["tools","reasoning"]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	p := cat.Providers["deepseek"]
	if len(p.Models) != 2 {
		t.Fatalf("models = %+v", p.Models)
	}
	if p.Models[0].ID != "deepseek-chat" || p.Models[0].Context != 131072 {
		t.Fatalf("first = %+v", p.Models[0])
	}
	if p.Models[1].ID != "reasoner" || !p.Models[1].Reasoning || len(p.Models[1].Capabilities) != 2 {
		t.Fatalf("second = %+v", p.Models[1])
	}
}

func TestParseProviderMapForm(t *testing.T) {
	cat, err := Parse([]byte(`{"providers":{"mock":{"models":[{"id":"x"}]},"other":[{"id":"y"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Providers["mock"].Models) != 1 || len(cat.Providers["other"].Models) != 1 {
		t.Fatalf("providers = %+v", cat.Providers)
	}
	if _, err := Parse([]byte(`{"nope":true}`)); err == nil {
		t.Fatal("empty catalog accepted")
	}
}

func TestMergeIntoDoesNotOverwrite(t *testing.T) {
	cfg := config.Defaults()
	pc := cfg.Providers["deepseek"]
	pc.Models = []config.Model{{ID: "deepseek-chat", Context: 1}}
	cfg.Providers["deepseek"] = pc

	cat := &Catalog{Version: 1, Providers: map[string]Provider{
		"deepseek": {Models: []Model{
			{ID: "deepseek-chat", Context: 999999},
			{ID: "deepseek-reasoner", Reasoning: true, Context: 64000},
		}},
		"unconfigured": {Models: []Model{{ID: "z"}}},
	}}
	added := MergeInto(&cfg, cat)
	if added != 1 {
		t.Fatalf("added = %d", added)
	}
	models := cfg.Providers["deepseek"].Models
	if len(models) != 2 || models[0].Context != 1 {
		t.Fatalf("existing model overwritten: %+v", models)
	}
	if models[1].Family != "deepseek" {
		t.Fatalf("family fallback missing: %+v", models[1])
	}
	if _, ok := cfg.Providers["unconfigured"]; ok {
		t.Fatal("merge invented a provider")
	}
}

func TestSaveReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "catalog.json")
	cat := &Catalog{Providers: map[string]Provider{"mock": {Models: []Model{{ID: "a"}}}}}
	if err := Save(path, cat); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != Version || len(loaded.Providers["mock"].Models) != 1 {
		t.Fatalf("loaded = %+v", loaded)
	}
}
