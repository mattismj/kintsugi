package tools

// FuzzRepair: the repair layer must never panic and must always return
// valid JSON for known tools (unknown tools pass their raw args through).
// Seeds are the real shapes that have broken before.

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzRepair(f *testing.F) {
	seeds := []struct{ tool, args string }{
		{"edit", `{"path":"a.go","old":"x","new":"y"}`},
		{"edit", `{"path":"a.go","old":"x","new":"y"`},
		{"edit", `{"old":"x"}`},
		{"edit", `{"path":"a.go","old":"x","new":1}`},
		{"edit", `null`},
		{"edit", `[]`},
		{"edit", `{}`},
		{"write", `{"path":"a.go","content":"x"}`},
		{"write", `{"path":"a.go","content":123}`},
		{"bash", `{"command":"echo hi"}`},
		{"bash", `{"command":"echo hi","timeout_s":"60"}`},
		{"task", `{"tasks":[{"title":"t","prompt":"p"}]}`},
		{"task", `{"tasks":"[{\"title\":\"t\",\"prompt\":\"p\"}]"}`},
		{"apply_patch", `{"patch":"*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch\n"}`},
		{"glob", `{"pattern":"**/*.go"}`},
		{"grep", `{"pattern":"foo","path":"."}`},
		{"webfetch", `{"url":"https://example.com"}`},
	}
	for _, s := range seeds {
		f.Add(s.tool, s.args)
	}
	f.Fuzz(func(t *testing.T, tool, args string) {
		rep, err := Repair(tool, args)
		if err != nil {
			return
		}
		// A known tool always produces a non-empty argument string. An
		// unknown tool (MCP, mods, or a whitespace-only name) passes its
		// raw args through, and a blank input is blank.
		if _, known := specs[rep.Tool]; known && len(rep.Args) == 0 {
			t.Fatal("repair returned an empty argument string")
		}
		// Known tools always produce valid JSON. Unknown tools (MCP, mods)
		// pass their raw args through untouched — that is by design.
		if _, known := specs[rep.Tool]; known && !json.Valid([]byte(rep.Args)) {
			t.Fatalf("repair returned invalid JSON for known tool %q: %q", rep.Tool, rep.Args)
		}
		// An unknown tool (including an empty one) is returned unchanged;
		// only assert the name survived when the input named one.
		if strings.TrimSpace(tool) != "" && rep.Tool == "" {
			t.Fatal("repair dropped a non-empty tool name")
		}
	})
}
