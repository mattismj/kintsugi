package tools

import (
	"reflect"
	"strings"
	"testing"

	"kintsugi/internal/config"
)

// TestEvaluateModeMatrix is the full mode x tool matrix. Every mode is
// checked against a read-only tool, an edit tool, the shell tool and an MCP
// tool (which the matrix does not know, so it behaves like any other
// mutating tool).
func TestEvaluateModeMatrix(t *testing.T) {
	cases := []struct {
		mode string
		tool string
		want string
	}{
		// default: reads run, mutations are asked about.
		{"default", "read", ActionAllow},
		{"default", "list", ActionAllow},
		{"default", "glob", ActionAllow},
		{"default", "grep", ActionAllow},
		{"default", "memory_search", ActionAllow},
		{"default", "kb_search", ActionAllow},
		{"default", "skill", ActionAllow},
		{"default", "edit", ActionAsk},
		{"default", "write", ActionAsk},
		{"default", "apply_patch", ActionAsk},
		{"default", "bash", ActionAsk},
		{"default", "todo", ActionAllow},
		{"default", "task", ActionAsk},
		{"default", "webfetch", ActionAsk},
		{"default", "mcp__github__create_issue", ActionAsk},

		// accept-edits: file edits run, the shell still asks.
		{"accept-edits", "read", ActionAllow},
		{"accept-edits", "edit", ActionAllow},
		{"accept-edits", "write", ActionAllow},
		{"accept-edits", "apply_patch", ActionAllow},
		{"accept-edits", "bash", ActionAsk},
		{"accept-edits", "todo", ActionAllow},
		{"accept-edits", "task", ActionAsk},
		{"accept-edits", "webfetch", ActionAsk},
		{"accept-edits", "mcp__github__create_issue", ActionAsk},

		// plan: read-only, edits refused.
		{"plan", "read", ActionAllow},
		{"plan", "glob", ActionAllow},
		{"plan", "edit", ActionDeny},
		{"plan", "write", ActionDeny},
		{"plan", "apply_patch", ActionDeny},
		{"plan", "bash", ActionAsk},

		// yolo: everything runs unless a deny rule says otherwise.
		{"yolo", "read", ActionAllow},
		{"yolo", "edit", ActionAllow},
		{"yolo", "bash", ActionAllow},
		{"yolo", "mcp__github__create_issue", ActionAllow},

		// dont-ask: never prompts, so anything unlisted is denied.
		{"dont-ask", "read", ActionAllow},
		{"dont-ask", "list", ActionAllow},
		{"dont-ask", "edit", ActionDeny},
		{"dont-ask", "bash", ActionDeny},
		{"dont-ask", "mcp__github__create_issue", ActionDeny},
	}
	for _, c := range cases {
		d := Evaluate(c.mode, nil, c.tool, "")
		if d.Action != c.want {
			t.Errorf("mode %q tool %q = %q, want %q (reason %q)", c.mode, c.tool, d.Action, c.want, d.Reason)
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("mode %q tool %q: decision has no reason", c.mode, c.tool)
		}
	}
}

// TestEvaluateUnknownModeFallsBackToDefault checks that a bogus or empty
// mode name behaves like the default mode instead of failing.
func TestEvaluateUnknownModeFallsBackToDefault(t *testing.T) {
	for _, mode := range []string{"", "   ", "sudo", "not-a-mode"} {
		if got := Evaluate(mode, nil, "read", "").Action; got != ActionAllow {
			t.Errorf("mode %q read = %q, want allow", mode, got)
		}
		if got := Evaluate(mode, nil, "edit", "").Action; got != ActionAsk {
			t.Errorf("mode %q edit = %q, want ask", mode, got)
		}
	}
}

// TestEvaluateRulePrecedence covers the rule ordering: deny beats ask beats
// allow, and inside one action class the last matching rule wins.
func TestEvaluateRulePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		rules     []config.PermRule
		tool      string
		spec      string
		want      string
		wantRule  string
		wantInRes string
	}{
		{
			name:      "deny beats allow",
			rules:     []config.PermRule{{Tool: "bash", Action: "allow"}, {Tool: "bash", Action: "deny"}},
			tool:      "bash",
			spec:      "git status",
			want:      ActionDeny,
			wantRule:  "bash",
			wantInRes: "bash",
		},
		{
			name:      "ask beats allow",
			rules:     []config.PermRule{{Tool: "bash", Action: "allow"}, {Tool: "bash", Action: "ask"}},
			tool:      "bash",
			spec:      "git status",
			want:      ActionAsk,
			wantRule:  "bash",
			wantInRes: "asks for approval",
		},
		{
			name:      "deny beats ask",
			rules:     []config.PermRule{{Tool: "bash", Action: "ask"}, {Tool: "bash", Action: "deny"}},
			tool:      "bash",
			spec:      "git status",
			want:      ActionDeny,
			wantRule:  "bash",
			wantInRes: "denied by rule",
		},
		{
			name: "last matching deny wins",
			rules: []config.PermRule{
				{Tool: "bash", Action: "deny"},
				{Tool: "bash", Spec: "git push", Action: "deny"},
			},
			tool:      "bash",
			spec:      "git push --force",
			want:      ActionDeny,
			wantRule:  "bash(git push)",
			wantInRes: "bash(git push)",
		},
		{
			name: "last matching allow wins",
			rules: []config.PermRule{
				{Tool: "bash", Action: "allow"},
				{Tool: "bash", Spec: "git status", Action: "allow"},
			},
			tool:      "bash",
			spec:      "git status --short",
			want:      ActionAllow,
			wantRule:  "bash(git status)",
			wantInRes: "bash(git status)",
		},
		{
			name:      "a rule that does not match is ignored",
			rules:     []config.PermRule{{Tool: "edit", Action: "deny"}},
			tool:      "bash",
			spec:      "git status",
			want:      ActionAsk,
			wantRule:  "",
			wantInRes: "default mode",
		},
		{
			name:      "an unknown action fails closed",
			rules:     []config.PermRule{{Tool: "bash", Action: "maybe"}},
			tool:      "bash",
			spec:      "git status",
			want:      ActionDeny,
			wantRule:  "bash",
			wantInRes: "failing closed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Evaluate("default", c.rules, c.tool, c.spec)
			if d.Action != c.want {
				t.Fatalf("action = %q, want %q (reason %q)", d.Action, c.want, d.Reason)
			}
			if d.Rule != c.wantRule {
				t.Errorf("rule = %q, want %q", d.Rule, c.wantRule)
			}
			if !strings.Contains(d.Reason, c.wantInRes) {
				t.Errorf("reason %q does not mention %q", d.Reason, c.wantInRes)
			}
		})
	}
}

// TestEvaluateWildcardTool covers wildcard tool names.
func TestEvaluateWildcardTool(t *testing.T) {
	rules := []config.PermRule{{Tool: "mcp__github__*", Action: "deny"}}

	if got := Evaluate("default", rules, "mcp__github__create_issue", "").Action; got != ActionDeny {
		t.Errorf("mcp__github__create_issue = %q, want deny", got)
	}
	if got := Evaluate("default", rules, "mcp__gitlab__create_issue", "").Action; got != ActionAsk {
		t.Errorf("mcp__gitlab__create_issue = %q, want ask", got)
	}
	// Tool names are matched case-insensitively.
	if got := Evaluate("default", rules, "MCP__GITHUB__create_issue", "").Action; got != ActionDeny {
		t.Errorf("case-insensitive tool match = %q, want deny", got)
	}
	// A plain name matches only itself.
	exact := []config.PermRule{{Tool: "read", Action: "deny"}}
	if got := Evaluate("default", exact, "read", "").Action; got != ActionDeny {
		t.Errorf("read = %q, want deny", got)
	}
	// A plain name does not match a longer one: "read" is not "read_file".
	// An unknown tool is treated as mutating, so it is asked about.
	if got := Evaluate("default", exact, "read_file", "").Action; got != ActionAsk {
		t.Errorf("read_file = %q, want ask", got)
	}
}

// TestEvaluateCommandSpec covers command-prefix specs on the shell tool.
func TestEvaluateCommandSpec(t *testing.T) {
	rules := []config.PermRule{{Tool: "bash", Spec: "git status", Action: "deny"}}

	cases := []struct {
		cmd  string
		want string
	}{
		{"git status", ActionDeny},
		{"git status --short", ActionDeny},
		{"git push", ActionAsk},
		{"git statusx", ActionAsk},
		{"echo git status", ActionAsk},
	}
	for _, c := range cases {
		if got := Evaluate("default", rules, "bash", c.cmd).Action; got != c.want {
			t.Errorf("bash %q = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// TestEvaluatePathSpec covers path-glob specs on file-ish tools.
func TestEvaluatePathSpec(t *testing.T) {
	rules := []config.PermRule{{Tool: "edit", Spec: "src/**", Action: "deny"}}

	cases := []struct {
		path string
		want string
	}{
		{"src/a/b.go", ActionDeny},
		{"src/main.go", ActionDeny},
		{"docs/notes.md", ActionAsk},
		{"srcx/main.go", ActionAsk},
	}
	for _, c := range cases {
		if got := Evaluate("default", rules, "edit", c.path).Action; got != c.want {
			t.Errorf("edit %q = %q, want %q", c.path, got, c.want)
		}
	}
	// A path spec does not leak onto another tool.
	if got := Evaluate("default", rules, "write", "src/a/b.go").Action; got != ActionAsk {
		t.Errorf("write with a path spec = %q, want ask", got)
	}
}

// TestEvaluateSpecForOtherTools checks the rule that a spec which cannot be
// checked (a tool with neither a command nor a path argument) does not apply,
// while a rule without a spec does.
func TestEvaluateSpecForOtherTools(t *testing.T) {
	withSpec := []config.PermRule{{Tool: "task", Spec: "src/**", Action: "deny"}}
	if got := Evaluate("default", withSpec, "task", "").Action; got != ActionAsk {
		t.Errorf("task with an unverifiable spec = %q, want ask", got)
	}
	noSpec := []config.PermRule{{Tool: "task", Action: "deny"}}
	d := Evaluate("default", noSpec, "task", "")
	if d.Action != ActionDeny {
		t.Errorf("task without a spec = %q, want deny", d.Action)
	}
	if d.Rule != "task" {
		t.Errorf("rule = %q, want %q", d.Rule, "task")
	}
}

// TestEvaluateWildcardAllowNeverGrants checks that an allow rule which does
// not name a tool never grants capability.
func TestEvaluateWildcardAllowNeverGrants(t *testing.T) {
	for _, tool := range []string{"*", "**", ""} {
		rules := []config.PermRule{{Tool: tool, Action: "allow"}}
		if got := Evaluate("default", rules, "edit", "").Action; got != ActionAsk {
			t.Errorf("allow rule with tool %q: edit = %q, want ask", tool, got)
		}
		if got := Evaluate("default", rules, "bash", "git status").Action; got != ActionAsk {
			t.Errorf("allow rule with tool %q: bash = %q, want ask", tool, got)
		}
	}
	// The same wildcard rule may still deny or ask.
	if got := Evaluate("default", []config.PermRule{{Tool: "*", Action: "deny"}}, "edit", "").Action; got != ActionDeny {
		t.Errorf("wildcard deny = %q, want deny", got)
	}
	if got := Evaluate("default", []config.PermRule{{Tool: "*", Action: "ask"}}, "edit", "").Action; got != ActionAsk {
		t.Errorf("wildcard ask = %q, want ask", got)
	}
	// A wildcard that names a family of tools may grant.
	if got := Evaluate("default", []config.PermRule{{Tool: "mcp__github__*", Action: "allow"}}, "mcp__github__x", "").Action; got != ActionAllow {
		t.Errorf("family allow = %q, want allow", got)
	}
}

// TestEvaluateYoloRespectsDeny checks that yolo still honors a deny rule.
func TestEvaluateYoloRespectsDeny(t *testing.T) {
	if got := Evaluate("yolo", nil, "bash", "rm -rf /").Action; got != ActionAllow {
		t.Errorf("yolo without rules: bash = %q, want allow", got)
	}
	rules := []config.PermRule{{Tool: "bash", Spec: "rm -rf", Action: "deny"}}
	d := Evaluate("yolo", rules, "bash", "rm -rf /tmp/x")
	if d.Action != ActionDeny {
		t.Errorf("yolo with a deny rule: bash = %q, want deny", d.Action)
	}
	if d.Rule != "bash(rm -rf)" {
		t.Errorf("rule = %q, want %q", d.Rule, "bash(rm -rf)")
	}
	// A deny rule for one tool does not deny the others.
	if got := Evaluate("yolo", rules, "edit", "src/main.go").Action; got != ActionAllow {
		t.Errorf("yolo: edit = %q, want allow", got)
	}
}

// TestEvaluatePlanMode checks that plan mode is read-only and that no rule
// lifts that floor.
func TestEvaluatePlanMode(t *testing.T) {
	d := Evaluate("plan", nil, "edit", "src/main.go")
	if d.Action != ActionDeny {
		t.Errorf("plan: edit = %q, want deny", d.Action)
	}
	if d.Reason != "plan mode is read-only" {
		t.Errorf("plan: reason = %q, want %q", d.Reason, "plan mode is read-only")
	}
	if got := Evaluate("plan", nil, "read", "src/main.go").Action; got != ActionAllow {
		t.Errorf("plan: read = %q, want allow", got)
	}
	if got := Evaluate("plan", nil, "glob", "").Action; got != ActionAllow {
		t.Errorf("plan: glob = %q, want allow", got)
	}
	// An allow rule cannot turn plan mode into a build mode.
	allow := []config.PermRule{{Tool: "edit", Action: "allow"}}
	if got := Evaluate("plan", allow, "edit", "src/main.go").Action; got != ActionDeny {
		t.Errorf("plan with an allow rule: edit = %q, want deny", got)
	}
}

// TestEvaluateDontAsk checks that dont-ask never prompts: unlisted mutating
// tools are denied, listed ones may still be allowed explicitly.
func TestEvaluateDontAsk(t *testing.T) {
	d := Evaluate("dont-ask", nil, "edit", "src/main.go")
	if d.Action != ActionDeny {
		t.Errorf("dont-ask: edit = %q, want deny", d.Action)
	}
	if d.Reason != "dont-ask never prompts; unlisted actions are denied" {
		t.Errorf("dont-ask: reason = %q", d.Reason)
	}
	if got := Evaluate("dont-ask", nil, "bash", "ls").Action; got != ActionDeny {
		t.Errorf("dont-ask: bash = %q, want deny", got)
	}
	if got := Evaluate("dont-ask", nil, "read", "src/main.go").Action; got != ActionAllow {
		t.Errorf("dont-ask: read = %q, want allow", got)
	}
	// An explicit rule still decides; an ask is refused, never prompted.
	ask := []config.PermRule{{Tool: "bash", Action: "ask"}}
	if got := Evaluate("dont-ask", ask, "bash", "ls").Action; got != ActionDeny {
		t.Errorf("dont-ask with an ask rule: bash = %q, want deny", got)
	}
	allow := []config.PermRule{{Tool: "bash", Spec: "ls", Action: "allow"}}
	if got := Evaluate("dont-ask", allow, "bash", "ls").Action; got != ActionAllow {
		t.Errorf("dont-ask with an allow rule: bash = %q, want allow", got)
	}
}

// TestParseMode covers the canonical names, the aliases and the error case.
func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"default", ModeDefault},
		{"accept-edits", ModeAcceptEdits},
		{"plan", ModePlan},
		{"yolo", ModeYolo},
		{"dont-ask", ModeDontAsk},
		{"auto-accept", ModeAcceptEdits},
		{"bypass", ModeYolo},
		{"manual", ModeDefault},
		{"acceptEdits", ModeAcceptEdits},
		{"dontAsk", ModeDontAsk},
		{"bypassPermissions", ModeYolo},
		{"ACCEPT-EDITS", ModeAcceptEdits},
		{"  yolo  ", ModeYolo},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if err != nil {
			t.Errorf("ParseMode(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := ParseMode("sudo"); err == nil {
		t.Error("ParseMode(\"sudo\") should fail")
	}
	if _, err := ParseMode("accept-everything"); err == nil {
		t.Error("ParseMode(\"accept-everything\") should fail")
	}
}

// TestEvaluateIsPureAndDeterministic checks the unit contract: the same
// inputs always give the same answer and the evaluator never mutates its
// inputs or shares state between calls.
func TestEvaluateIsPureAndDeterministic(t *testing.T) {
	rules := []config.PermRule{
		{Tool: "bash", Spec: "git push", Action: "deny"},
		{Tool: "edit", Action: "ask"},
	}
	before := append([]config.PermRule(nil), rules...)

	first := Evaluate("default", rules, "bash", "git push --force")
	second := Evaluate("default", rules, "bash", "git push --force")
	if first != second {
		t.Errorf("Evaluate is not deterministic: %+v vs %+v", first, second)
	}
	if !reflect.DeepEqual(rules, before) {
		t.Errorf("Evaluate mutated its input: %+v", rules)
	}
	// Mutating a returned slice must not leak into the next call.
	mutating, readOnly := ModeTools(ModeYolo)
	if len(mutating) == 0 || len(readOnly) == 0 {
		t.Fatalf("ModeTools(yolo) = %v, %v", mutating, readOnly)
	}
	mutating[0] = "tampered"
	mutating2, _ := ModeTools(ModeYolo)
	if mutating2[0] == "tampered" {
		t.Error("ModeTools shares state between calls")
	}
}

// TestModeTools checks the per-mode allow lists.
func TestModeTools(t *testing.T) {
	mutating, readOnly := ModeTools(ModeDefault)
	if len(mutating) != 0 {
		t.Errorf("default: mutating = %v, want none", mutating)
	}
	if len(readOnly) == 0 {
		t.Error("default: read-only list is empty")
	}

	mutating, _ = ModeTools(ModeAcceptEdits)
	want := []string{"apply_patch", "edit", "write"}
	if !reflect.DeepEqual(mutating, want) {
		t.Errorf("accept-edits: mutating = %v, want %v", mutating, want)
	}

	mutating, _ = ModeTools(ModePlan)
	if len(mutating) != 0 {
		t.Errorf("plan: mutating = %v, want none", mutating)
	}

	mutating, _ = ModeTools(ModeDontAsk)
	if len(mutating) != 0 {
		t.Errorf("dont-ask: mutating = %v, want none", mutating)
	}

	mutating, readOnly = ModeTools(ModeYolo)
	// todo is session state, not a workspace change, so it is read-only here:
	// six mutating tools and eight read-only ones.
	if len(mutating) != 6 {
		t.Errorf("yolo: mutating = %v, want the 6 known mutating tools", mutating)
	}
	if len(readOnly) != 8 {
		t.Errorf("yolo: read-only = %v, want the 8 known read-only tools", readOnly)
	}
}

// TestSpecArg checks the helper that extracts the argument a rule spec is
// matched against from a raw tool call.
func TestSpecArg(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"bash", `{"command":"git status --short"}`, "git status --short"},
		{"edit", `{"path":"src/a/b.go","old":"x","new":"y"}`, "src/a/b.go"},
		{"write", `{"path":"docs/n.md","content":"hi"}`, "docs/n.md"},
		{"read", `{"path":"README.md"}`, "README.md"},
		{"glob", `{"pattern":"src/**"}`, "src/**"},
		{"grep", `{"pattern":"TODO","path":"internal"}`, "internal"},
		{"task", `{"tasks":[{"title":"t"}]}`, ""},
		{"mcp__github__create_issue", `{"title":"t"}`, ""},
		{"edit", `{}`, ""},
		{"edit", `not json`, ""},
		{"bash", `{"command":42}`, ""},
	}
	for _, c := range cases {
		if got := SpecArg(c.tool, c.args); got != c.want {
			t.Errorf("SpecArg(%q, %q) = %q, want %q", c.tool, c.args, got, c.want)
		}
	}
}

// TestEvaluateReasonNamesModeOrRule checks that every decision explains
// itself, so the UI can show why.
func TestEvaluateReasonNamesModeOrRule(t *testing.T) {
	decisions := []Decision{
		Evaluate("default", nil, "read", ""),
		Evaluate("default", nil, "edit", ""),
		Evaluate("accept-edits", nil, "edit", ""),
		Evaluate("plan", nil, "edit", ""),
		Evaluate("yolo", nil, "bash", ""),
		Evaluate("dont-ask", nil, "edit", ""),
		Evaluate("default", []config.PermRule{{Tool: "bash", Action: "deny"}}, "bash", "ls"),
	}
	for i, d := range decisions {
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("decision %d has no reason", i)
		}
	}
}
