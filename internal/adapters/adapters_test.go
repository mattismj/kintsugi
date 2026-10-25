package adapters

import (
	"strings"
	"testing"
)

// TestEveryAdapterRendersSerialExecution is the honesty guard: the loop runs a
// turn's tool calls one at a time (internal/agent), so no adapter may tell a
// model otherwise. Every shipped adapter must render the serial contract, and
// none may mention a parallel capability at all.
func TestEveryAdapterRendersSerialExecution(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	all := reg.All()
	if len(all) == 0 {
		t.Fatal("no adapters loaded")
	}
	for _, a := range all {
		text := a.Render()
		if !strings.Contains(text, "Tool-call execution: serial") {
			t.Errorf("adapter %q does not state serial tool-call execution:\n%s", a.ID, text)
		}
		if strings.Contains(text, "parallel_tool_calls") {
			t.Errorf("adapter %q still advertises parallel_tool_calls:\n%s", a.ID, text)
		}
	}
}

// TestAdapterDataRefusesParallelToolCalls proves the removal is structural, not
// a coincidence of today's YAML. Load() decodes every data file through
// decode(), which rejects unknown fields, so a file that reintroduces
// `parallel_tool_calls` fails to load: the capability cannot come back as a
// data edit, only as a code change that also has to touch the turn loop.
func TestAdapterDataRefusesParallelToolCalls(t *testing.T) {
	const legacy = `
id: ghost
wire: openai-completions
capabilities:
  tools: true
  parallel_tool_calls: true
`
	if _, err := decode([]byte(legacy)); err == nil {
		t.Fatal("a data file claiming parallel_tool_calls was accepted; the capability is advertising-able again")
	}
}

// TestShippedDataDecodesStrictly is the other half: every adapter actually
// shipped is clean under the strict decoder, so strictness never breaks a real
// model. A typo in a YAML key is now a load error rather than a silently
// ignored field.
func TestShippedDataDecodesStrictly(t *testing.T) {
	ents, err := dataFS.ReadDir("data")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := dataFS.ReadFile("data/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decode(b); err != nil {
			t.Errorf("%s does not decode strictly: %v", e.Name(), err)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no adapter data files were checked")
	}
}
