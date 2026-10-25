// Live catalog fetch (WP-4.6).
//
// `kintsugi models fetch <provider>` reads the provider's own /models endpoint
// instead of a file on disk. This is an explicit operator action, so no offline
// gate is applied here: the CLI decides whether egress is acceptable (it can
// check core.mode / provider.Offline). What we *do* keep strict is the response
// handling — anything unparseable is an error, never a silently empty catalog.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"kintsugi/internal/config"
)

// LookupProvider returns the configured provider called name, with the list of
// configured names in the error so a typo is self-correcting.
func LookupProvider(cfg config.File, name string) (config.Provider, error) {
	p, ok := cfg.Providers[name]
	if !ok {
		names := cfg.ProviderNames()
		sort.Strings(names)
		list := "none"
		if len(names) > 0 {
			list = strings.Join(names, ", ")
		}
		return config.Provider{}, fmt.Errorf("unknown provider %q (configured: %s)", name, list)
	}
	return p, nil
}

// Fetch GETs <base_url>/models with the provider's own credentials and maps the
// answer to config.Model entries. Accepted shapes:
//
//	{"data": [{"id": "...", ...}]}   OpenAI-compatible (the common case)
//	{"models": [{"id": "...", ...}]} / {"result": [...]}
//	[{"id": "..."}, ...]             flat array
//
// id/name/model/slug, context_length/context_window/context/max_context_tokens
// and reasoning/is_reasoning are all understood; reasoning is also inferred for
// ids containing "reasoner" or "thinking".
func Fetch(ctx context.Context, p config.Provider, timeout time.Duration) ([]config.Model, error) {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("models fetch: provider has no base_url")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, "GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	// Same auth style as provider.setAuth, replicated locally: the provider
	// package keeps it unexported and this package must not depend on the wire
	// code just to send one GET.
	if p.APIKeyEnv != "" {
		key := strings.TrimSpace(os.Getenv(p.APIKeyEnv))
		if key == "" {
			return nil, fmt.Errorf("provider needs %s (set it in the environment)", p.APIKeyEnv)
		}
		setModelsAuth(req, p, key)
	}
	return doFetch(ctx, req, base)
}

// FetchWithKey is Fetch with the key supplied by the caller: the TUI resolves
// env-first, then the 0600 credential store, so a provider signed in through
// /connect lists its models without the key ever being exported. A provider
// with no api_key_env takes an empty key and sends no auth header.
func FetchWithKey(ctx context.Context, p config.Provider, key string, timeout time.Duration) ([]config.Model, error) {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("models fetch: provider has no base_url")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if p.APIKeyEnv != "" && strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("provider needs %s (set it in the environment)", p.APIKeyEnv)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, "GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKeyEnv != "" {
		setModelsAuth(req, p, strings.TrimSpace(key))
	}
	return doFetch(ctx, req, base)
}

// setModelsAuth sets the auth headers for a /models GET in the provider's own
// wire style.
func setModelsAuth(req *http.Request, p config.Provider, key string) {
	if p.Wire == "anthropic-messages" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func doFetch(ctx context.Context, req *http.Request, base string) ([]config.Model, error) {
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("provider %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(b)), 300))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	models, err := ParseModels(body)
	if err != nil {
		return nil, fmt.Errorf("%s/models: %w", base, err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s/models: provider returned no model ids", base)
	}
	return models, nil
}

// ParseModels maps a /models body to config.Model entries. Unknown fields are
// ignored; entries without an id are dropped.
func ParseModels(body []byte) ([]config.Model, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var list []any
	switch t := doc.(type) {
	case []any:
		list = t
	case map[string]any:
		for _, key := range []string{"data", "models", "result", "results"} {
			if arr, ok := t[key].([]any); ok {
				list = arr
				break
			}
		}
		if list == nil {
			// Object map form: {"deepseek-chat": {...}, "x": {}}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				obj, ok := t[k].(map[string]any)
				if !ok {
					continue
				}
				if _, has := obj["id"]; !has {
					obj["id"] = k
				}
				list = append(list, obj)
			}
		}
	default:
		return nil, fmt.Errorf("parse: expected an object or array, got %T", doc)
	}
	if list == nil {
		return nil, fmt.Errorf("parse: no model array in response")
	}

	out := make([]config.Model, 0, len(list))
	seen := map[string]bool{}
	for _, e := range list {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id := firstString(obj, "id", "model", "name", "slug")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m := config.Model{
			ID:           id,
			Family:       firstString(obj, "family", "family_id"),
			Context:      firstNumber(obj, "context_length", "context_window", "context", "max_context", "max_context_tokens"),
			Reasoning:    firstBool(obj, "reasoning", "is_reasoning", "supports_reasoning"),
			Capabilities: stringList(obj["capabilities"]),
		}
		if !m.Reasoning {
			low := strings.ToLower(id)
			if strings.Contains(low, "reasoner") || strings.Contains(low, "thinking") {
				m.Reasoning = true
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func firstString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstNumber(obj map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := obj[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			n, _ := v.Int64()
			return int(n)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}

func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
