package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

// TestHelperProcess is the fake MCP server. It is re-executed as a child by
// TestStartListsAndCallsTool; the env guard keeps it out of normal test runs.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCP_HELPER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil || req.Method == "" {
			continue
		}
		reply := func(result any) {
			if len(req.ID) == 0 {
				return
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})
		case "notifications/initialized":
			// notification: no reply
		case "tools/list":
			reply(map[string]any{"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "Echo text.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string", "description": "text to echo"},
						},
						"required": []string{"text"},
					},
				},
				map[string]any{
					"name":        "fail",
					"description": "Return an error result.",
					"inputSchema": map[string]any{"type": "object"},
				},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "fail" {
				reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}})
				continue
			}
			text, _ := p.Arguments["text"].(string)
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + text}}})
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no"}})
		}
	}
}

func TestStartListsAndCallsTool(t *testing.T) {
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"fake": {
			Command:  os.Args[0],
			Args:     []string{"-test.run=TestHelperProcess", "--"},
			Env:      map[string]string{"MCP_HELPER": "1"},
			TimeoutS: 10,
		},
		"broken": {Command: "/nonexistent/mcp-server"},
	}}
	m := Start(cfg, nil)
	defer m.Close()

	defs := m.Definitions()
	var echo, fail bool
	for _, d := range defs {
		if d.Function.Name == "mcp__fake__echo" {
			echo = true
			if d.Function.Parameters.Properties["text"].Type != "string" {
				t.Fatalf("schema not translated: %+v", d.Function.Parameters)
			}
		}
		if d.Function.Name == "mcp__fake__fail" {
			fail = true
		}
	}
	if !echo || !fail {
		t.Fatalf("definitions = %+v", defs)
	}

	out, isErr, err := m.Call(context.Background(), "mcp__fake__echo", `{"text":"hello"}`)
	if err != nil || isErr || out != "echo: hello" {
		t.Fatalf("call = %q isErr=%v err=%v", out, isErr, err)
	}
	out, isErr, err = m.Call(context.Background(), "mcp__fake__fail", `{}`)
	if err != nil || !isErr || !strings.Contains(out, "boom") {
		t.Fatalf("fail call = %q isErr=%v err=%v", out, isErr, err)
	}
	if m.Has("mcp__fake__echo") != true || m.Has("mcp__fake__nope") {
		t.Fatal("Has routing mismatch")
	}
}

func TestStartDisabledAndNoServers(t *testing.T) {
	if m := Start(config.MCP{Enabled: false, Servers: map[string]config.MCPServer{
		"x": {Command: "/nonexistent"},
	}}, nil); len(m.Definitions()) != 0 {
		t.Fatal("disabled manager registered definitions")
	}
	if m := Start(config.MCP{Enabled: true}, nil); len(m.Definitions()) != 0 {
		t.Fatal("empty manager registered definitions")
	}
}

// ---- per-server tool allowlist --------------------------------------------

// eventLog collects the manager's log events so a test can assert what the
// operator would have seen.
type eventLog struct {
	mu     sync.Mutex
	events []loggedEvent
}

type loggedEvent struct {
	kind string
	val  map[string]any
}

func (e *eventLog) log(kind string, v any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, _ := v.(map[string]any)
	e.events = append(e.events, loggedEvent{kind: kind, val: m})
}

// find returns the payload of the first event of the given kind.
func (e *eventLog) find(kind string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.kind == kind {
			return ev.val
		}
	}
	return nil
}

// count reports how many events of the given kind were logged.
func (e *eventLog) count(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.kind == kind {
			n++
		}
	}
	return n
}

// fakeServerCfg builds a config for the helper MCP server with the given
// allowlist.
func fakeServerCfg(allow []string) config.MCP {
	return config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"fake": {
			Command:    os.Args[0],
			Args:       []string{"-test.run=TestHelperProcess", "--"},
			Env:        map[string]string{"MCP_HELPER": "1"},
			TimeoutS:   10,
			AllowTools: allow,
		},
	}}
}

func defNames(defs []provider.Tool) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Function.Name)
	}
	return out
}

// TestAllowlistFiltersTools proves the allowlist hides the tools it does not
// match, that the hidden ones are reported with a count, and that a hidden
// tool cannot be called.
func TestAllowlistFiltersTools(t *testing.T) {
	log := &eventLog{}
	m := Start(fakeServerCfg([]string{"echo"}), log.log)
	defer m.Close()

	names := defNames(m.Definitions())
	if len(names) != 1 || names[0] != "mcp__fake__echo" {
		t.Fatalf("definitions = %v, want only mcp__fake__echo", names)
	}
	if m.Has("mcp__fake__fail") {
		t.Fatal("hidden tool is still routed")
	}
	if _, _, err := m.Call(context.Background(), "mcp__fake__fail", `{}`); err == nil {
		t.Fatal("hidden tool is still callable")
	}

	ev := log.find("mcp_tools_filtered")
	if ev == nil {
		t.Fatalf("no mcp_tools_filtered event: %+v", log.events)
	}
	if ev["server"] != "fake" {
		t.Fatalf("filtered event server = %v", ev["server"])
	}
	if n, _ := ev["hidden"].(int); n != 1 {
		t.Fatalf("filtered event hidden count = %v, want 1", ev["hidden"])
	}
	tools, _ := ev["tools"].([]string)
	if len(tools) != 1 || tools[0] != "fail" {
		t.Fatalf("filtered event tools = %v, want [fail]", tools)
	}
	srv := log.find("mcp_server")
	if srv == nil {
		t.Fatal("no mcp_server event")
	}
	if n, _ := srv["tools"].(int); n != 1 {
		t.Fatalf("mcp_server tools = %v, want 1", srv["tools"])
	}
	if n, _ := srv["hidden"].(int); n != 1 {
		t.Fatalf("mcp_server hidden = %v, want 1", srv["hidden"])
	}
}

// TestAllowlistEmptyKeepsEverything proves the default (no allowlist) keeps
// every discovered tool and reports nothing as hidden.
func TestAllowlistEmptyKeepsEverything(t *testing.T) {
	for _, allow := range [][]string{nil, {}} {
		log := &eventLog{}
		m := Start(fakeServerCfg(allow), log.log)
		names := defNames(m.Definitions())
		m.Close()
		if len(names) != 2 {
			t.Fatalf("allow %v: definitions = %v, want both tools", allow, names)
		}
		if log.count("mcp_tools_filtered") != 0 {
			t.Fatalf("allow %v: unexpected filter event", allow)
		}
	}
}

// TestAllowlistWildcard proves glob patterns work: the namespaced prefix form
// keeps a whole server, and a narrower pattern keeps a single tool.
func TestAllowlistWildcard(t *testing.T) {
	m := Start(fakeServerCfg([]string{"mcp__fake__*"}), nil)
	defer m.Close()
	if names := defNames(m.Definitions()); len(names) != 2 {
		t.Fatalf("server wildcard: definitions = %v, want both tools", names)
	}

	m2 := Start(fakeServerCfg([]string{"mcp__fake__ec*"}), nil)
	defer m2.Close()
	names := defNames(m2.Definitions())
	if len(names) != 1 || names[0] != "mcp__fake__echo" {
		t.Fatalf("tool wildcard: definitions = %v, want only mcp__fake__echo", names)
	}

	m3 := Start(fakeServerCfg([]string{"*echo*"}), nil)
	defer m3.Close()
	names = defNames(m3.Definitions())
	if len(names) != 1 || names[0] != "mcp__fake__echo" {
		t.Fatalf("raw-name wildcard: definitions = %v, want only mcp__fake__echo", names)
	}
}

// TestAllowlistMatchingNothing proves an allowlist that matches nothing
// leaves the server with no tools at all.
func TestAllowlistMatchingNothing(t *testing.T) {
	log := &eventLog{}
	m := Start(fakeServerCfg([]string{"no_such_tool"}), log.log)
	defer m.Close()
	if names := defNames(m.Definitions()); len(names) != 0 {
		t.Fatalf("definitions = %v, want none", names)
	}
	ev := log.find("mcp_tools_filtered")
	if ev == nil {
		t.Fatal("no mcp_tools_filtered event")
	}
	if n, _ := ev["hidden"].(int); n != 2 {
		t.Fatalf("hidden count = %v, want 2", ev["hidden"])
	}
}

// TestAllowlistBlankEntriesKeepEverything proves a stray empty pattern is not
// treated as an instruction to hide a server's tools.
func TestAllowlistBlankEntriesKeepEverything(t *testing.T) {
	m := Start(fakeServerCfg([]string{"", "  "}), nil)
	defer m.Close()
	if names := defNames(m.Definitions()); len(names) != 2 {
		t.Fatalf("definitions = %v, want both tools", names)
	}
}

// TestMatchToolPattern pins the pattern language.
func TestMatchToolPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"web_search", "web_search", true},
		{"web_search", "web_search_extra", false},
		{"web_search", "other", false},
		{"mcp__x__*", "mcp__x__anything", true},
		{"mcp__x__*", "mcp__y__anything", false},
		{"*", "anything", true},
		{"*search*", "web_search_v2", true},
		{"mcp__x__exact", "mcp__x__exact", true},
		{"MCP__X__*", "mcp__x__tool", true},
	}
	for _, c := range cases {
		if got := matchToolPattern(c.pattern, c.name); got != c.want {
			t.Errorf("matchToolPattern(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// TestToolAllowedUsesRawAndNamespacedNames proves the allowlist matches both
// the server's raw tool name and the namespaced name the model sees.
func TestToolAllowedUsesRawAndNamespacedNames(t *testing.T) {
	if !toolAllowed([]string{"web_search"}, "docs", "web_search") {
		t.Error("raw name did not match")
	}
	if !toolAllowed([]string{"mcp__docs__web_search"}, "docs", "web_search") {
		t.Error("namespaced name did not match")
	}
	if toolAllowed([]string{"mcp__other__*"}, "docs", "web_search") {
		t.Error("foreign pattern matched")
	}
	if !toolAllowed(nil, "docs", "web_search") {
		t.Error("empty allowlist did not allow the tool")
	}
}

func TestSanitizeAndUniqueNames(t *testing.T) {
	if got := sanitize("My Server!", 20); got != "My_Server_" {
		t.Fatalf("sanitize = %q", got)
	}
	first := uniqueToolName(nil, "srv", "tool")
	if first != "mcp__srv__tool" {
		t.Fatalf("first = %q", first)
	}
	conflict := uniqueToolName([]provider.Tool{{Function: provider.ToolFunction{Name: first}}}, "srv", "tool")
	if conflict == first || !strings.HasPrefix(conflict, first) {
		t.Fatalf("conflict = %q", conflict)
	}
}

func TestCallUnknownTool(t *testing.T) {
	m := &Manager{routes: map[string]route{}}
	if _, _, err := m.Call(context.Background(), "mcp__none__x", `{}`); err == nil {
		t.Fatal("expected error")
	}
}
