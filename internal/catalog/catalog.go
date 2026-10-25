// Package catalog imports model metadata from a versioned JSON document.
// The canonical schema is ours; the parser is deliberately tolerant of
// flat model lists emitted by other harnesses. No third-party data is bundled.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kintsugi/internal/config"
)

const Version = 1

type Model struct {
	ID           string   `json:"id"`
	Family       string   `json:"family,omitempty"`
	Context      int      `json:"context,omitempty"`
	Reasoning    bool     `json:"reasoning,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type Provider struct {
	Models []Model `json:"models"`
}

type Catalog struct {
	Version   int                 `json:"version"`
	Providers map[string]Provider `json:"providers"`
}

// Parse accepts the canonical schema or a flat array / {"models": [...]}
// shape with common aliases. Unknown fields are ignored.
func Parse(data []byte) (*Catalog, error) {
	var canon Catalog
	if err := json.Unmarshal(data, &canon); err == nil && canon.Providers != nil {
		if canon.Version == 0 {
			canon.Version = Version
		}
		normalize(&canon)
		return &canon, nil
	}

	var wrapper struct {
		Models    json.RawMessage `json:"models"`
		Providers json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(data, &wrapper)
	if len(wrapper.Providers) > 0 {
		var byProvider map[string]json.RawMessage
		if err := json.Unmarshal(wrapper.Providers, &byProvider); err == nil {
			out := &Catalog{Version: Version, Providers: map[string]Provider{}}
			names := make([]string, 0, len(byProvider))
			for name := range byProvider {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				body := byProvider[name]
				var holder struct {
					Models json.RawMessage `json:"models"`
				}
				if json.Unmarshal(body, &holder) == nil && len(holder.Models) > 0 {
					body = holder.Models
				}
				ms, err := parseModelList(body)
				if err != nil {
					return nil, fmt.Errorf("provider %s: %w", name, err)
				}
				out.Providers[name] = Provider{Models: flatToModels(ms)}
			}
			normalize(out)
			return out, nil
		}
	}
	raw := data
	if len(wrapper.Models) > 0 {
		raw = wrapper.Models
	}
	models, err := parseModelList(raw)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("catalog: no models found")
	}
	out := &Catalog{Version: Version, Providers: map[string]Provider{}}
	for _, m := range models {
		name := m.provider
		if name == "" {
			name = "unknown"
		}
		out.Providers[name] = Provider{Models: append(out.Providers[name].Models, flatToModel(m))}
	}
	normalize(out)
	return out, nil
}

type flatModel struct {
	provider     string
	id           string
	family       string
	context      int
	reasoning    bool
	capabilities []string
}

func parseModelList(raw json.RawMessage) ([]flatModel, error) {
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		var m map[string]map[string]any
		if err2 := json.Unmarshal(raw, &m); err2 != nil {
			return nil, fmt.Errorf("catalog: expected a model array: %w", err)
		}
		for id, v := range m {
			if v == nil {
				v = map[string]any{}
			}
			if _, ok := v["id"]; !ok {
				v["id"] = id
			}
			list = append(list, v)
		}
		sort.Slice(list, func(i, j int) bool { return str(list[i], "id") < str(list[j], "id") })
	}
	var out []flatModel
	for _, obj := range list {
		m := flatModel{
			id:        firstStr(obj, "id", "model", "name", "slug"),
			provider:  firstStr(obj, "provider", "provider_id", "providerID"),
			family:    firstStr(obj, "family", "family_id"),
			context:   firstInt(obj, "context", "context_window", "context_length", "max_context"),
			reasoning: firstBool(obj, "reasoning", "is_reasoning"),
		}
		m.capabilities = strList(obj["capabilities"])
		if m.id == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func flatToModels(in []flatModel) []Model {
	out := make([]Model, 0, len(in))
	for _, m := range in {
		out = append(out, flatToModel(m))
	}
	return out
}

func flatToModel(m flatModel) Model {
	return Model{
		ID: m.id, Family: m.family, Context: m.context,
		Reasoning: m.reasoning, Capabilities: m.capabilities,
	}
}

func normalize(c *Catalog) {
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	for name, p := range c.Providers {
		seen := map[string]bool{}
		var models []Model
		for _, m := range p.Models {
			if strings.TrimSpace(m.ID) == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			models = append(models, m)
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		c.Providers[name] = Provider{Models: models}
	}
}

// MergeInto adds catalog models to configured providers without overwriting
// existing entries and without inventing base URLs or keys.
func MergeInto(cfg *config.File, cat *Catalog) int {
	add := 0
	for name, p := range cat.Providers {
		pc, ok := cfg.Providers[name]
		if !ok {
			continue
		}
		have := map[string]bool{}
		for _, m := range pc.Models {
			have[m.ID] = true
		}
		for _, m := range p.Models {
			if have[m.ID] {
				continue
			}
			model := config.Model{ID: m.ID, Family: m.Family, Context: m.Context, Reasoning: m.Reasoning, Capabilities: m.Capabilities}
			if model.Family == "" {
				model.Family = pc.Family
			}
			pc.Models = append(pc.Models, model)
			have[m.ID] = true
			add++
		}
		cfg.Providers[name] = pc
	}
	return add
}

func Save(path string, cat *Catalog) error {
	if cat.Version == 0 {
		cat.Version = Version
	}
	b, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func firstStr(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstInt(obj map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := obj[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			n, _ := v.Int64()
			return int(n)
		}
	}
	return 0
}

func firstBool(obj map[string]any, keys ...string) bool {
	for _, k := range keys {
		if b, ok := obj[k].(bool); ok {
			return b
		}
	}
	return false
}

func strList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func str(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return s
}
