// Package mcp bridges Model Context Protocol servers into the tool
// layer. Two transports are supported: stdio (a local child process, one
// JSON-RPC message per line) and HTTP (the streamable HTTP shape, see
// http.go). Servers are isolated from each other; a server that fails to
// start or initialize is logged and skipped rather than taking the session
// down.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/provider"
)

const (
	clientName       = "kintsugi"
	clientVersion    = "0.1"
	protocolVersion  = "2024-11-05"
	defaultTimeout   = 60 * time.Second
	maxToolNameChars = 64
)

type route struct {
	client *Client
	tool   string
}

// Manager owns every configured server connection and the tool routes.
type Manager struct {
	clients []*Client
	defs    []provider.Tool
	routes  map[string]route
	log     func(kind string, v any)
}

// Start launches and initializes configured servers. It never returns an error:
// per-server failures are logged and the remaining servers stay usable.
func Start(cfg config.MCP, log func(kind string, v any)) *Manager {
	m := &Manager{routes: map[string]route{}, log: log}
	if !cfg.Enabled || len(cfg.Servers) == 0 {
		return m
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sc := cfg.Servers[name]
		if strings.TrimSpace(sc.Command) == "" && !isHTTPTransport(sc.Transport) {
			m.warn(name, "command is empty")
			continue
		}
		timeout := time.Duration(sc.TimeoutS) * time.Second
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		c, err := newClient(name, sc, timeout, log)
		if err != nil {
			m.warn(name, err.Error())
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err = c.initialize(ctx)
		cancel()
		if err != nil {
			c.Close()
			m.warn(name, "initialize: "+err.Error())
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		tools, err := c.listTools(ctx)
		cancel()
		if err != nil {
			c.Close()
			m.warn(name, "tools/list: "+err.Error())
			continue
		}
		// The allowlist is applied after discovery, so a server still starts
		// and still answers calls the operator allowed — it simply stops
		// advertising what the operator hid.
		exposed, hidden := filterTools(name, tools, sc.AllowTools)
		m.clients = append(m.clients, c)
		for _, t := range exposed {
			full := uniqueToolName(m.defs, name, t.Name)
			m.defs = append(m.defs, toProviderTool(full, t))
			m.routes[full] = route{client: c, tool: t.Name}
		}
		if log != nil {
			log("mcp_server", map[string]any{
				"server": name, "tools": len(exposed), "hidden": len(hidden),
			})
			if len(hidden) > 0 {
				log("mcp_tools_filtered", map[string]any{
					"server": name,
					"hidden": len(hidden),
					"tools":  toolNames(hidden),
				})
			}
		}
	}
	return m
}

// filterTools splits a server's discovered tools into the ones the allowlist
// keeps and the ones it hides. An empty allowlist keeps everything.
func filterTools(server string, tools []mcpTool, allow []string) (kept, hidden []mcpTool) {
	patterns := allowPatterns(allow)
	for _, t := range tools {
		if len(patterns) == 0 || matchesAny(patterns, server, t.Name) {
			kept = append(kept, t)
			continue
		}
		hidden = append(hidden, t)
	}
	return kept, hidden
}

// toolAllowed reports whether one discovered tool survives the server's
// allowlist. Patterns are matched against both the server's raw tool name
// ("web_search") and the namespaced name the model sees
// ("mcp__docs__web_search"), so an operator can write either form. An empty
// allowlist allows every tool the server advertises.
func toolAllowed(allow []string, server, tool string) bool {
	patterns := allowPatterns(allow)
	if len(patterns) == 0 {
		return true
	}
	return matchesAny(patterns, server, tool)
}

// matchesAny reports whether one pattern matches the tool under either of its
// names.
func matchesAny(patterns []string, server, tool string) bool {
	names := []string{tool, namespacedToolName(server, tool)}
	for _, p := range patterns {
		for _, n := range names {
			if matchToolPattern(p, n) {
				return true
			}
		}
	}
	return false
}

// allowPatterns drops blank entries. A list that holds nothing usable is an
// empty allowlist (everything allowed), so a stray empty string never hides a
// whole server's tools.
func allowPatterns(allow []string) []string {
	out := make([]string, 0, len(allow))
	for _, p := range allow {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// matchToolPattern reports whether a tool name matches one allowlist pattern.
// The pattern language is glob-ish: "*" matches any run of characters, so
// "web_search" matches that tool exactly and "mcp__x__*" matches every tool
// of server x. Matching is case-insensitive.
func matchToolPattern(pattern, name string) bool {
	pattern = strings.ToLower(pattern)
	name = strings.ToLower(name)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(name) {
		switch {
		case pi < len(pattern) && pattern[pi] == '*':
			star, pi = pi, pi+1
			mark = si
		case pi < len(pattern) && pattern[pi] == name[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// toolNames lists the raw names of the given tools, for reporting.
func toolNames(tools []mcpTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func (m *Manager) warn(server, msg string) {
	if m.log != nil {
		m.log("mcp_error", map[string]any{"server": server, "error": msg})
	}
}

// Definitions returns the namespaced tool schemas.
func (m *Manager) Definitions() []provider.Tool {
	if m == nil {
		return nil
	}
	return append([]provider.Tool(nil), m.defs...)
}

// Has reports whether a namespaced MCP tool is routed.
func (m *Manager) Has(name string) bool {
	if m == nil {
		return false
	}
	_, ok := m.routes[name]
	return ok
}

// Call invokes one tool. The returned bool is the server's isError flag.
func (m *Manager) Call(ctx context.Context, name, args string) (string, bool, error) {
	if m == nil {
		return "", true, fmt.Errorf("mcp is not configured")
	}
	r, ok := m.routes[name]
	if !ok {
		return "", true, fmt.Errorf("unknown mcp tool %q", name)
	}
	var payload any = map[string]any{}
	if strings.TrimSpace(args) != "" {
		var v any
		if err := json.Unmarshal([]byte(args), &v); err != nil {
			return "", true, fmt.Errorf("mcp %s: invalid arguments: %w", name, err)
		}
		payload = v
	}
	return r.client.callTool(ctx, r.tool, payload)
}

// Close shuts down every child process. Safe to call twice.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, c := range m.clients {
		c.Close()
	}
	m.clients = nil
}

// ---------- client ----------

type Client struct {
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	timeout time.Duration
	log     func(kind string, v any)
	// http is set for a server that speaks the HTTP transport (M12.1). The
	// stdio fields stay empty in that case: there is no child process.
	http *httpTransport

	mu     sync.Mutex // writes + request ids
	nextID int

	pmu     sync.Mutex
	pending map[int]chan rpcResponse

	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	err       error
}

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("rpc error %d", e.Code)
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

func newClient(name string, sc config.MCPServer, timeout time.Duration, log func(string, any)) (*Client, error) {
	if isHTTPTransport(sc.Transport) {
		return newHTTPClient(name, sc, timeout, log)
	}
	cmd := exec.Command(sc.Command, sc.Args...)
	if sc.CWD != "" {
		cmd.Dir = config.ExpandHome(sc.CWD)
	}
	env := os.Environ()
	keys := make([]string, 0, len(sc.Env))
	for k := range sc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+sc.Env[k])
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		name: name, cmd: cmd, stdin: stdin, reader: bufio.NewReaderSize(stdout, 1<<20),
		timeout: timeout, log: log, pending: map[int]chan rpcResponse{}, done: make(chan struct{}),
	}
	go c.readLoop()
	go c.drainStderr(stderr)
	return c, nil
}

func (c *Client) initialize(ctx context.Context) error {
	_, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	})
	if err != nil {
		return err
	}
	return c.notify("notifications/initialized", map[string]any{})
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (c *Client) listTools(ctx context.Context) ([]mcpTool, error) {
	var out []mcpTool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.request(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode tools/list: %w", err)
		}
		out = append(out, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

func (c *Client) callTool(ctx context.Context, name string, args any) (string, bool, error) {
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", true, err
	}
	var res struct {
		Content []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", true, fmt.Errorf("decode tools/call: %w", err)
	}
	var parts []string
	for _, b := range res.Content {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s content omitted: %d bytes]", b.Type, len(b.Data)))
		default:
			if len(b.Data) > 0 {
				parts = append(parts, string(b.Data))
			} else if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	out := strings.TrimRight(strings.Join(parts, "\n"), "\n")
	if out == "" {
		out = "(mcp tool returned no text content)"
	}
	return out, res.IsError, nil
}

func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.http != nil {
		return c.http.request(ctx, method, params)
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	ch := make(chan rpcResponse, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()

	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	select {
	case resp := <-ch:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		if c.err != nil {
			return nil, c.err
		}
		return nil, fmt.Errorf("mcp server %s exited", c.name)
	}
}

func (c *Client) notify(method string, params any) error {
	if c.http != nil {
		return c.http.notify(context.Background(), method, params)
	}
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write(b); err != nil {
		return fmt.Errorf("mcp %s: write: %w", c.name, err)
	}
	return nil
}

func (c *Client) readLoop() {
	defer c.finish(nil)
	for {
		line, err := c.reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if err != io.EOF {
				c.finish(err)
			}
			return
		}
	}
}

func (c *Client) finish(err error) {
	c.doneOnce.Do(func() {
		if err != nil && c.err == nil {
			c.err = fmt.Errorf("mcp %s: %w", c.name, err)
		}
		close(c.done)
	})
}

func (c *Client) handleLine(line []byte) {
	var env struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Result json.RawMessage  `json:"result"`
		Error  *rpcError        `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	if env.ID == nil {
		return // notification
	}
	var id int
	_ = json.Unmarshal(*env.ID, &id)
	if env.Method != "" {
		// Server-initiated request (sampling/roots). Not supported yet; answer
		// with a JSON-RPC error instead of hanging the server.
		_ = c.write(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(*env.ID),
			"error": map[string]any{"code": -32601, "message": "method not supported by kintsugi"},
		})
		return
	}
	c.pmu.Lock()
	ch, ok := c.pending[id]
	c.pmu.Unlock()
	if ok {
		select {
		case ch <- rpcResponse{result: env.Result, err: env.Error}:
		default:
		}
	}
}

func (c *Client) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if c.log == nil {
			continue
		}
		line := strings.TrimSpace(sc.Text())
		if len(line) > 500 {
			line = line[:500] + "…"
		}
		if line != "" {
			c.log("mcp_stderr", map[string]any{"server": c.name, "line": line})
		}
	}
}

// Close closes stdin, waits briefly for the read loop to finish, then kills the
// process if it is still alive. Wait is called only after the reader has
// stopped so the stdout pipe is not closed underneath it.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.http != nil {
			// No child process: the transport only has connections to drop.
			c.http.close()
			c.finish(nil)
			return
		}
		_ = c.stdin.Close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-c.done
		}
		wait := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(wait)
		}()
		select {
		case <-wait:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-wait
		}
		c.finish(nil)
	})
}

// namespacedToolName is the canonical name a server's tool is exposed under.
// It is the name the allowlist patterns are matched against, and the base for
// the uniqueness pass in uniqueToolName.
func namespacedToolName(server, tool string) string {
	base := "mcp__" + sanitize(server, 20) + "__" + sanitize(tool, 38)
	if len(base) > maxToolNameChars {
		base = base[:maxToolNameChars]
	}
	return base
}

func uniqueToolName(existing []provider.Tool, server, tool string) string {
	base := namespacedToolName(server, tool)
	name := base
	for i := 2; ; i++ {
		taken := false
		for _, d := range existing {
			if d.Function.Name == name {
				taken = true
				break
			}
		}
		if !taken {
			return name
		}
		suffix := fmt.Sprintf("_%d", i)
		name = base
		if len(name)+len(suffix) > maxToolNameChars {
			name = name[:maxToolNameChars-len(suffix)]
		}
		name += suffix
	}
}

func sanitize(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "srv"
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// ---------- schema translation ----------

func toProviderTool(name string, t mcpTool) provider.Tool {
	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = "MCP tool " + t.Name
	}
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  convertSchema(t.InputSchema),
		},
	}
}

func convertSchema(raw json.RawMessage) provider.ToolParameters {
	out := provider.ToolParameters{Type: "object", Properties: map[string]provider.ToolProp{}}
	if len(raw) == 0 {
		return out
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	if t, ok := m["type"].(string); ok && t != "" {
		out.Type = t
	}
	if req, ok := m["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for k, v := range props {
			out.Properties[k] = convertProp(v)
		}
	}
	return out
}

func convertProp(v any) provider.ToolProp {
	obj, ok := v.(map[string]any)
	if !ok {
		return provider.ToolProp{Type: "string"}
	}
	p := provider.ToolProp{Type: stringType(obj["type"]), Description: stringValue(obj["description"])}
	if items, ok := obj["items"]; ok {
		ip := convertProp(items)
		p.Items = &ip
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		p.Properties = map[string]provider.ToolProp{}
		for k, v := range props {
			p.Properties[k] = convertProp(v)
		}
	}
	if req, ok := obj["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				p.Required = append(p.Required, s)
			}
		}
	}
	if enum, ok := obj["enum"].([]any); ok {
		p.Enum = enum
	}
	if d, ok := obj["default"]; ok {
		p.Default = d
	}
	return p
}

func stringType(v any) string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return t
		}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
	}
	return "string"
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
