package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

func TestAdvisorProfileDefault(t *testing.T) {
	f := Defaults()
	if f.Advisor.Active("plain") {
		t.Fatal("advisor on by default for plain")
	}
	if !f.Advisor.Active("lab") {
		t.Fatal("advisor off by default for lab")
	}
	var explicit File
	if _, err := toml.Decode("[advisor]\nenabled = false\n", &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Advisor.Active("lab") {
		t.Fatal("explicit disable ignored for lab")
	}
	if _, err := toml.Decode("[advisor]\nenabled = true\nmax_tokens = 5\n", &explicit); err != nil {
		t.Fatal(err)
	}
	if !explicit.Advisor.Active("plain") || explicit.Advisor.MaxTokens != 5 {
		t.Fatalf("explicit enable ignored: %+v", explicit.Advisor)
	}
}

func TestRolesDecodeAndFallback(t *testing.T) {
	f := Defaults()
	body := `
[roles]
default = "mock/deepseek-chat"
coder = "mock/deepseek-coder"
researcher = "mock/fast"
opsec = "mock/reviewer"

[roles.personas]
coder = "deepseek-flash41"
opsec = "deepseek-flash41"
`
	if _, err := toml.Decode(body, &f); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"":           "mock/deepseek-chat",
		"default":    "mock/deepseek-chat",
		"coder":      "mock/deepseek-coder",
		"researcher": "mock/fast",
		"opsec":      "mock/reviewer",
		"reviewer":   "mock/reviewer",
		"unknown":    "mock/deepseek-chat",
	}
	for role, want := range cases {
		if got := f.Roles.Model(role); got != want {
			t.Errorf("Model(%q) = %q, want %q", role, got, want)
		}
	}
	if got := f.Roles.Persona("coder"); got != "deepseek-flash41" {
		t.Errorf("coder persona = %q", got)
	}
	if got := f.Roles.Persona("researcher"); got != "" {
		t.Errorf("unset persona = %q", got)
	}
}

func TestQuotaAndAgentsDecode(t *testing.T) {
	f := Defaults()
	body := `
[quota.deepseek]
limit_tokens = 1234
reset = "weekly"

[agents]
subagents = false
max_subagents = 2

[gates]
blocking = true
max_revisions = 3
`
	if _, err := toml.Decode(body, &f); err != nil {
		t.Fatal(err)
	}
	q, ok := f.Quota["deepseek"]
	if !ok || q.LimitTokens != 1234 || q.Reset != "weekly" {
		t.Fatalf("quota = %+v", f.Quota)
	}
	if f.Agents.Subagents || f.Agents.MaxSubagents != 2 {
		t.Fatalf("agents = %+v", f.Agents)
	}
	if !f.Gates.Blocking || f.Gates.MaxRevisions != 3 {
		t.Fatalf("gates = %+v", f.Gates)
	}
}

// TestMCPAllowToolsDecode proves the per-server tool allowlist decodes, that a
// server without one keeps an empty list (every tool allowed), and that the
// allowlist is per server rather than global.
func TestMCPAllowToolsDecode(t *testing.T) {
	f := Defaults()
	body := `
[mcp]
enabled = true

[mcp.servers.docs]
command = "docs-server"
args = ["--stdio"]
allow_tools = ["web_search", "mcp__docs__*"]

[mcp.servers.shell]
command = "shell-server"
`
	if _, err := toml.Decode(body, &f); err != nil {
		t.Fatal(err)
	}
	docs, ok := f.MCP.Servers["docs"]
	if !ok {
		t.Fatalf("mcp servers = %+v", f.MCP.Servers)
	}
	if len(docs.AllowTools) != 2 ||
		docs.AllowTools[0] != "web_search" || docs.AllowTools[1] != "mcp__docs__*" {
		t.Fatalf("allow_tools = %+v", docs.AllowTools)
	}
	if docs.Command != "docs-server" {
		t.Fatalf("command = %q", docs.Command)
	}
	shell, ok := f.MCP.Servers["shell"]
	if !ok {
		t.Fatalf("mcp servers = %+v", f.MCP.Servers)
	}
	if len(shell.AllowTools) != 0 {
		t.Fatalf("unset allow_tools = %+v, want empty", shell.AllowTools)
	}
}

// TestUIDefaults pins the UI switches a fresh install gets: mouse capture on.
func TestUIDefaults(t *testing.T) {
	f := Defaults()
	if !f.UI.Mouse {
		t.Error("ui.mouse = false, want true by default")
	}
	if !f.UI.Banner || !f.UI.AltScreen || !f.UI.Ornament {
		t.Errorf("existing UI defaults changed: %+v", f.UI)
	}
	if f.UI.Plain {
		t.Errorf("existing UI defaults changed: %+v", f.UI)
	}
}

// TestUIDecodeSwitches checks that [ui] mouse/boot decode both ways and that a
// partial [ui] table leaves the other switches at their defaults (Load decodes
// into Defaults()).
func TestUIDecodeSwitches(t *testing.T) {
	f := Defaults()
	if _, err := toml.Decode("[ui]\nmouse = false\n", &f); err != nil {
		t.Fatal(err)
	}
	if f.UI.Mouse {
		t.Fatalf("explicit disable ignored: %+v", f.UI)
	}
	if !f.UI.Banner || !f.UI.AltScreen {
		t.Fatalf("partial [ui] table dropped other defaults: %+v", f.UI)
	}

	explicit := Defaults()
	if _, err := toml.Decode("[ui]\nmouse = true\n", &explicit); err != nil {
		t.Fatal(err)
	}
	if !explicit.UI.Mouse {
		t.Fatalf("explicit enable ignored: %+v", explicit.UI)
	}
}
