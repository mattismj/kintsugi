package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestRegistryCoversOpenWeightsCloudProviders(t *testing.T) {
	cfg := Defaults()
	if RegistryVersion() < 1 {
		t.Fatalf("registry version = %d", RegistryVersion())
	}
	if len(cfg.Providers) < 25 {
		t.Fatalf("provider registry has %d entries, want >= 25", len(cfg.Providers))
	}

	seenKeyEnv := map[string]string{}
	families := map[string]bool{"": true, "deepseek": true, "kimi": true, "glm": true, "qwen": true}
	for name, p := range cfg.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			t.Fatalf("%s: invalid base_url %q", name, p.BaseURL)
		}
		if p.Wire != "openai-completions" {
			t.Fatalf("%s: unexpected wire %q", name, p.Wire)
		}
		if !families[p.Family] {
			t.Fatalf("%s: family %q has no adapter", name, p.Family)
		}
		if p.Offline {
			if !IsLoopback(p.BaseURL) {
				t.Fatalf("%s: marked offline but %q is not loopback", name, p.BaseURL)
			}
			if p.APIKeyEnv != "" {
				t.Fatalf("%s: loopback provider should not require a key", name)
			}
			continue
		}
		if !strings.HasPrefix(p.BaseURL, "https://") {
			t.Fatalf("%s: cloud provider must use https, got %q", name, p.BaseURL)
		}
		if p.APIKeyEnv == "" {
			t.Fatalf("%s: cloud provider has no api_key_env", name)
		}
		if prev, ok := seenKeyEnv[p.APIKeyEnv]; ok {
			t.Fatalf("%s and %s share key env %s", prev, name, p.APIKeyEnv)
		}
		seenKeyEnv[p.APIKeyEnv] = name
	}

	for _, want := range []string{
		"deepseek", "moonshot", "zhipu", "zai", "dashscope", "openrouter",
		"together", "groq", "cerebras", "mistral", "siliconflow", "fireworks",
		"deepinfra", "novita", "huggingface", "ollama",
	} {
		if _, ok := cfg.Providers[want]; !ok {
			t.Fatalf("missing provider %q", want)
		}
	}
}

func TestRegistryModelResolutionAndPassthrough(t *testing.T) {
	cfg := Defaults()
	if _, _, m, err := cfg.ResolveModel("deepseek/deepseek-chat"); err != nil || m.Family != "deepseek" {
		t.Fatalf("deepseek resolve: %v family=%q", err, m.Family)
	}
	if _, _, _, err := cfg.ResolveModel("moonshot/does-not-exist"); err == nil {
		t.Fatal("curated provider should reject unknown model ids")
	}
	// Aggregators publish changing catalogs, so they are passthrough until a
	// catalog import pins models.
	if _, prov, m, err := cfg.ResolveModel("groq/qwen/qwen3-32b"); err != nil || prov != "groq" || m.ID == "" {
		t.Fatalf("passthrough resolve: %v prov=%q model=%q", err, prov, m.ID)
	}
	if _, _, m, err := cfg.ResolveModel("openrouter/deepseek/deepseek-v3.1"); err != nil || m.ID == "" {
		t.Fatalf("openrouter passthrough resolve: %v", err)
	}
}

// TestRegistryPinsSubscriptionProviders pins the flat-plan subscription
// endpoints researched in DESIGN §5.3 so a bad edit can never silently point a
// configured key at the wrong host.
func TestRegistryPinsSubscriptionProviders(t *testing.T) {
	cfg := Defaults()
	for _, want := range []struct {
		name      string
		baseURL   string
		apiKeyEnv string
	}{
		{"cline-pass", "https://api.cline.bot/api/v1", "CLINE_PASS_API_KEY"},
		{"opencode-go", "https://opencode.ai/zen/go/v1", "OPENCODE_GO_API_KEY"},
		{"commandcode", "https://api.commandcode.ai/provider", "COMMANDCODE_API_KEY"},
	} {
		prov, name, m, err := cfg.ResolveModel(want.name + "/some-model-id")
		if err != nil {
			t.Fatalf("%s: resolve: %v", want.name, err)
		}
		if name != want.name {
			t.Fatalf("%s: resolved provider name %q", want.name, name)
		}
		if prov.BaseURL != want.baseURL {
			t.Fatalf("%s: base_url = %q, want %q", want.name, prov.BaseURL, want.baseURL)
		}
		if prov.APIKeyEnv != want.apiKeyEnv {
			t.Fatalf("%s: api_key_env = %q, want %q", want.name, prov.APIKeyEnv, want.apiKeyEnv)
		}
		if prov.Wire != "openai-completions" || m.Wire != "openai-completions" {
			t.Fatalf("%s: wire = %q/%q, want openai-completions", want.name, prov.Wire, m.Wire)
		}
		if prov.Offline {
			t.Fatalf("%s: subscription endpoint must not be marked offline", want.name)
		}
	}
}

// TestRegistryPinsAlibabaPlanProviders pins the two Alibaba plan endpoints.
// They are separate subscriptions with separate keys but the same catalogue, so
// a bad edit that swaps the host or the key env is the failure worth catching:
// a coding-plan key sent to the token-plan host (or vice versa) fails with an
// opaque 401 at request time.
func TestRegistryPinsAlibabaPlanProviders(t *testing.T) {
	cfg := Defaults()
	for _, want := range []struct {
		name      string
		baseURL   string
		apiKeyEnv string
	}{
		{
			"alibaba-coding-plan",
			"https://coding-intl.dashscope.aliyuncs.com/v1",
			"ALIBABA_CODING_PLAN_API_KEY",
		},
		{
			"alibaba-token-plan",
			"https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
			"ALIBABA_TOKEN_PLAN_API_KEY",
		},
	} {
		prov, ok := cfg.Providers[want.name]
		if !ok {
			t.Fatalf("%s: not in the registry", want.name)
		}
		if prov.BaseURL != want.baseURL {
			t.Fatalf("%s: base_url = %q, want %q", want.name, prov.BaseURL, want.baseURL)
		}
		if prov.APIKeyEnv != want.apiKeyEnv {
			t.Fatalf("%s: api_key_env = %q, want %q", want.name, prov.APIKeyEnv, want.apiKeyEnv)
		}
		if prov.Wire != "openai-completions" {
			t.Fatalf("%s: wire = %q, want openai-completions", want.name, prov.Wire)
		}
		if prov.Family != "qwen" {
			t.Fatalf("%s: family = %q, want qwen", want.name, prov.Family)
		}
		if prov.Offline {
			t.Fatalf("%s: a cloud plan endpoint must not be marked offline", want.name)
		}
	}
	// The two plans must never share a key env: one subscription key is not the
	// other's credential.
	if cfg.Providers["alibaba-coding-plan"].APIKeyEnv == cfg.Providers["alibaba-token-plan"].APIKeyEnv {
		t.Fatal("alibaba plan providers share api_key_env")
	}
	// dashscope stays the pre-existing entry: the plans are additions, not a
	// rename of the metered compatible-mode endpoint.
	if got := cfg.Providers["dashscope"].BaseURL; got != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("dashscope base_url = %q", got)
	}
}

// TestRegistryAlibabaPlanModelResolution covers both plans as curated
// providers: the advertised ids resolve with the family and wire inherited from
// the provider, and a typo fails loudly instead of silently passing through.
func TestRegistryAlibabaPlanModelResolution(t *testing.T) {
	cfg := Defaults()
	for _, name := range []string{"alibaba-coding-plan", "alibaba-token-plan"} {
		for _, id := range []string{"qwen3-coder-plus", "qwen3-max", "qwen-plus"} {
			prov, gotName, m, err := cfg.ResolveModel(name + "/" + id)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, id, err)
			}
			if gotName != name {
				t.Fatalf("%s/%s: provider name %q", name, id, gotName)
			}
			if m.ID != id {
				t.Fatalf("%s/%s: model id %q", name, id, m.ID)
			}
			if m.Family != "qwen" {
				t.Fatalf("%s/%s: family = %q, want qwen", name, id, m.Family)
			}
			if m.Wire != "openai-completions" {
				t.Fatalf("%s/%s: wire = %q, want openai-completions", name, id, m.Wire)
			}
			if prov.APIKeyEnv != cfg.Providers[name].APIKeyEnv {
				t.Fatalf("%s/%s: provider config not returned", name, id)
			}
		}
		if _, _, _, err := cfg.ResolveModel(name + "/qwen-not-a-model"); err == nil {
			t.Fatalf("%s: curated provider accepted an unlisted model id", name)
		}
	}
}

// TestRegistryPassthroughSubscriptions keeps the passthrough promise: these
// providers publish changing catalogues, so any model id resolves until a
// catalog import pins it.
func TestRegistryPassthroughSubscriptions(t *testing.T) {
	cfg := Defaults()
	for _, name := range []string{"cline-pass", "opencode-go", "commandcode"} {
		_, prov, m, err := cfg.ResolveModel(name + "/some-model-id")
		if err != nil {
			t.Fatalf("%s passthrough resolve: %v", name, err)
		}
		if prov != name || m.ID != "some-model-id" {
			t.Fatalf("%s passthrough resolve: prov=%q model=%q", name, prov, m.ID)
		}
	}
	// Family inference still applies to arbitrary ids on passthrough providers.
	if _, _, m, err := cfg.ResolveModel("cline-pass/anthropic/claude-sonnet-4.5"); err != nil || m.ID == "" {
		t.Fatalf("cline-pass nested id resolve: %v", err)
	}

	// Curated providers keep failing loudly on typos.
	for _, ref := range []string{"deepseek/not-a-model", "moonshot/does-not-exist"} {
		if _, _, _, err := cfg.ResolveModel(ref); err == nil {
			t.Fatalf("%q should error: curated provider has a fixed model list", ref)
		}
	}
}
