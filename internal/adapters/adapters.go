// Package adapters holds per-model compile data (DESIGN §3.7): capabilities,
// quirks and instructions that are *true about the model*, loaded from data
// files so adding a model is adding a YAML, not a code branch.
package adapters

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed data/*.yaml
var dataFS embed.FS

type Adapter struct {
	ID    string `yaml:"id"`
	Match struct {
		Family    []string `yaml:"family"`
		Reasoning *bool    `yaml:"reasoning"`
		IDGlob    []string `yaml:"id_glob"`
	} `yaml:"match"`
	Wire string `yaml:"wire"`
	// Caps are the capabilities the harness can actually honour for this model.
	//
	// There is deliberately no parallel_tool_calls field. The turn loop runs a
	// turn's tool calls one at a time, in the order the model emitted them
	// (internal/agent/agent.go), because the permission gate is interactive, a
	// mutating call must not race another call on the same path, and the tool
	// results must be appended in call order. Advertising the capability would
	// promise a behaviour the harness does not have, so the field is gone
	// rather than rendered as a constant false. Add it back only together with
	// concurrent execution in the loop — never before.
	Caps struct {
		Tools            bool   `yaml:"tools"`
		StrictJSONSchema string `yaml:"strict_json_schema"`
		DeveloperRole    bool   `yaml:"developer_role"`
	} `yaml:"capabilities"`
	Compat struct {
		MultipleSystemMessages           *bool  `yaml:"multiple_system_messages"`
		AssistantAfterToolResult         *bool  `yaml:"assistant_after_tool_result"`
		ThinkingAsText                   bool   `yaml:"thinking_as_text"`
		ReasoningContentField            string `yaml:"reasoning_content_field"`
		RetryWithoutStrictOnGrammarError bool   `yaml:"retry_without_strict_on_grammar_error"`
		DisableReasoningWithTools        bool   `yaml:"disable_reasoning_with_tools"`
		ThinkingFormat                   string `yaml:"thinking_format"`
		FilterReasoningHistory           *bool  `yaml:"filter_reasoning_history"`
		OpenAIToolCallIDLimit            bool   `yaml:"openai_tool_call_id_limit"`
	} `yaml:"compat"`

	Quirks       []string `yaml:"quirks"`
	Instructions []string `yaml:"instructions"`
	Reasoning    struct {
		Mode      string `yaml:"mode"`
		MaxTokens int    `yaml:"max_tokens"`
	} `yaml:"reasoning"`
}

// Registry is the loaded adapter set, stable-sorted by id.
type Registry struct {
	all []Adapter
}

func Load() (*Registry, error) {
	ents, err := dataFS.ReadDir("data")
	if err != nil {
		return nil, err
	}
	var all []Adapter
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := dataFS.ReadFile("data/" + e.Name())
		if err != nil {
			return nil, err
		}
		a, err := decode(b)
		if err != nil {
			return nil, fmt.Errorf("adapter %s: %w", e.Name(), err)
		}
		if a.ID == "" {
			a.ID = strings.TrimSuffix(e.Name(), ".yaml")
		}
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return &Registry{all: all}, nil
}

// decode parses one adapter data file with unknown fields rejected.
//
// Strictness is the point: a capability the harness cannot honour must fail
// loudly at load time, not be dropped on the floor and re-advertised by a later
// reader. The most concrete case is `parallel_tool_calls` — the harness executes
// tool calls serially, so a data file claiming otherwise is a lie, and the fix
// has to be a code change to the turn loop plus this struct, never a YAML edit.
func decode(b []byte) (Adapter, error) {
	var a Adapter
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return Adapter{}, err
	}
	return a, nil
}

// Select returns the most specific adapter for a model. Specificity:
// explicit reasoning match > family match > generic fallback.
func (r *Registry) Select(family, modelID string, reasoning bool) (*Adapter, error) {
	type scored struct {
		a     *Adapter
		score int
	}
	var best scored
	for i := range r.all {
		a := &r.all[i]
		score := 0
		if a.Match.Reasoning != nil {
			if *a.Match.Reasoning != reasoning {
				continue
			}
			score += 4
		}
		famMatch := len(a.Match.Family) == 0
		for _, f := range a.Match.Family {
			if f == family {
				famMatch = true
				break
			}
		}
		if !famMatch {
			continue
		}
		if len(a.Match.Family) > 0 {
			score += 2
		}
		globMatch := false
		for _, g := range a.Match.IDGlob {
			if ok, _ := glob(g, modelID); ok {
				globMatch = true
				break
			}
		}
		if globMatch {
			score += 3
		}
		if score > best.score {
			best = scored{a, score}
		}
	}
	if best.a == nil {
		return nil, fmt.Errorf("no adapter matches family=%q id=%q", family, modelID)
	}
	return best.a, nil
}

func (r *Registry) All() []Adapter { return r.all }

// Render materializes the adapter as layer-1 prompt text (deterministic and
// cache-stable by construction: fixed field order, sorted where needed).
func (a *Adapter) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Model adapter — %s\n\n", a.ID)
	fmt.Fprintf(&b, "Wire: %s\n\n", a.Wire)
	fmt.Fprintf(&b, "Capabilities: tools=%t strict_json_schema=%s developer_role=%t\n",
		a.Caps.Tools, orNone(a.Caps.StrictJSONSchema), a.Caps.DeveloperRole)
	// The wire has no such flag to set, but the prompt must state it: a model
	// that batches calls makes the harness answer them one at a time anyway, so
	// the honest line is the serial one. It is emitted for every adapter, from
	// the harness, not from the YAML — the YAML cannot claim otherwise.
	b.WriteString("Tool-call execution: serial. One tool call per turn; a batch of " +
		"calls is executed one at a time, in the order given.\n")

	b.WriteString("\nCompat fingerprint:\n")
	fmt.Fprintf(&b, "- multiple_system_messages=%t assistant_after_tool_result=%t reasoning_content_field=%s\n",
		boolDefault(a.Compat.MultipleSystemMessages, true),
		boolDefault(a.Compat.AssistantAfterToolResult, true),
		orNone(a.Compat.ReasoningContentField))
	fmt.Fprintf(&b, "- thinking_as_text=%t filter_reasoning_history=%t openai_tool_call_id_limit=%t\n",
		a.Compat.ThinkingAsText, boolDefault(a.Compat.FilterReasoningHistory, true), a.Compat.OpenAIToolCallIDLimit)

	if len(a.Quirks) > 0 {
		b.WriteString("\nModel quirks (facts about this family; do not argue with the harness):\n")
		for _, q := range a.Quirks {
			fmt.Fprintf(&b, "- %s\n", q)
		}
	}
	if len(a.Instructions) > 0 {
		b.WriteString("\nTool-call discipline for this model:\n")
		for _, q := range a.Instructions {
			fmt.Fprintf(&b, "- %s\n", q)
		}
	}
	if a.Reasoning.MaxTokens > 0 {
		fmt.Fprintf(&b, "\nReasoning: mode=%s budget=%d tokens\n", orNone(a.Reasoning.Mode), a.Reasoning.MaxTokens)
	}
	return b.String()
}

func boolDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// MergeSystemMessages reports whether the wire client must fold consecutive
// system messages (compat field), defaulting to supported when unset.
func (a *Adapter) MergeSystemMessages() bool {
	return !boolDefault(a.Compat.MultipleSystemMessages, true)
}

func orNone(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

func glob(pattern, s string) (bool, error) {
	// filepath.Match semantics without importing path/filepath just for this:
	// patterns used here are simple prefix* forms.
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*")), nil
	}
	return pattern == s, nil
}
