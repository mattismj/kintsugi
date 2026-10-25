package mcp

// Tests for the HTTP transport (M12.1).
//
// Every test runs against a real httptest server, so the wire is exercised
// end to end: the POST, the Accept header, the JSON body and the SSE
// variant, the allowlist, the failure isolation and the secret handling.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kintsugi/internal/config"
)

// httpMCPRequest is the JSON-RPC request the harness posts.
type httpMCPRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// httpRecorder remembers what the fake server saw.
type httpRecorder struct {
	mu      sync.Mutex
	headers []http.Header
}

func (r *httpRecorder) record(h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers = append(r.headers, h.Clone())
}

func (r *httpRecorder) all() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]http.Header, len(r.headers))
	copy(out, r.headers)
	return out
}

// fakeHTTPTools is the tool set the fake server advertises.
func fakeHTTPTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "echo",
			"description": "Echo text.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "text to echo"},
				},
				"required": []any{"text"},
			},
		},
		{
			"name":        "fail",
			"description": "Return an error result.",
			"inputSchema": map[string]any{"type": "object"},
		},
	}
}

// newFakeHTTPServer starts an MCP server that speaks the streamable HTTP
// shape. mode is "json" (a JSON body) or "sse" (an event stream).
func newFakeHTTPServer(t *testing.T, mode string) (*httptest.Server, *httpRecorder) {
	t.Helper()
	rec := &httpRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req httpMCPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-http", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": fakeHTTPTools()}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "fail" {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}}
				break
			}
			text, _ := p.Arguments["text"].(string)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + text}}}
		default:
			if len(req.ID) == 0 {
				// A notification: the server accepts it and says nothing.
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if mode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// httpServerCfg builds a config for one HTTP server.
func httpServerCfg(name, endpoint string, allow []string) config.MCP {
	return config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		name: {Transport: "http", URL: endpoint, TimeoutS: 5, AllowTools: allow},
	}}
}

// TestHTTPTransportDiscoversAndCallsTool proves the JSON variant: the
// server's tools are discovered, exposed under the namespaced name, and the
// call reaches the server.
func TestHTTPTransportDiscoversAndCallsTool(t *testing.T) {
	srv, _ := newFakeHTTPServer(t, "json")
	m := Start(httpServerCfg("web", srv.URL, nil), nil)
	defer m.Close()

	names := defNames(m.Definitions())
	if len(names) != 2 || names[0] != "mcp__web__echo" || names[1] != "mcp__web__fail" {
		t.Fatalf("definitions = %v, want the namespaced tools", names)
	}
	for _, d := range m.Definitions() {
		if d.Function.Name == "mcp__web__echo" {
			if d.Function.Parameters.Properties["text"].Type != "string" {
				t.Fatalf("schema not translated: %+v", d.Function.Parameters)
			}
		}
	}

	out, isErr, err := m.Call(context.Background(), "mcp__web__echo", `{"text":"hello"}`)
	if err != nil || isErr || out != "echo: hello" {
		t.Fatalf("call = %q isErr=%v err=%v", out, isErr, err)
	}
	out, isErr, err = m.Call(context.Background(), "mcp__web__fail", `{}`)
	if err != nil || !isErr || !strings.Contains(out, "boom") {
		t.Fatalf("fail call = %q isErr=%v err=%v", out, isErr, err)
	}
	if !m.Has("mcp__web__echo") || m.Has("mcp__web__nope") {
		t.Fatal("Has routing mismatch")
	}
}

// TestHTTPTransportEventStreamVariant proves the SSE variant: the same
// handshake and call, answered with `data:` frames instead of a JSON body.
func TestHTTPTransportEventStreamVariant(t *testing.T) {
	srv, _ := newFakeHTTPServer(t, "sse")
	m := Start(httpServerCfg("sse", srv.URL, nil), nil)
	defer m.Close()

	names := defNames(m.Definitions())
	if len(names) != 2 {
		t.Fatalf("definitions = %v, want both tools", names)
	}
	out, isErr, err := m.Call(context.Background(), "mcp__sse__echo", `{"text":"hi"}`)
	if err != nil || isErr || out != "echo: hi" {
		t.Fatalf("call = %q isErr=%v err=%v", out, isErr, err)
	}
}

// TestHTTPTransportAllowlistFiltersTools proves the per-server allowlist and
// the namespacing still apply to an HTTP server.
func TestHTTPTransportAllowlistFiltersTools(t *testing.T) {
	srv, _ := newFakeHTTPServer(t, "json")
	log := &eventLog{}
	m := Start(httpServerCfg("web", srv.URL, []string{"echo"}), log.log)
	defer m.Close()

	names := defNames(m.Definitions())
	if len(names) != 1 || names[0] != "mcp__web__echo" {
		t.Fatalf("definitions = %v, want only mcp__web__echo", names)
	}
	if m.Has("mcp__web__fail") {
		t.Fatal("hidden tool is still routed")
	}
	if _, _, err := m.Call(context.Background(), "mcp__web__fail", `{}`); err == nil {
		t.Fatal("hidden tool is still callable")
	}
	ev := log.find("mcp_tools_filtered")
	if ev == nil {
		t.Fatal("no mcp_tools_filtered event")
	}
	if n, _ := ev["hidden"].(int); n != 1 {
		t.Fatalf("hidden count = %v, want 1", ev["hidden"])
	}
}

// TestHTTPTransportServerErrorIsIsolated proves a server that answers 500
// disables only itself: the healthy server stays usable.
func TestHTTPTransportServerErrorIsIsolated(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream is on fire"))
	}))
	defer broken.Close()
	good, _ := newFakeHTTPServer(t, "json")

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"broken": {Transport: "http", URL: broken.URL, TimeoutS: 5},
		"good":   {Transport: "http", URL: good.URL, TimeoutS: 5},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	names := defNames(m.Definitions())
	if len(names) != 2 {
		t.Fatalf("definitions = %v, want the healthy server's tools", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "mcp__broken__") {
			t.Fatalf("the broken server exposed %s", n)
		}
	}
	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event for the broken server")
	}
	if ev["server"] != "broken" {
		t.Fatalf("error event server = %v, want broken", ev["server"])
	}
	msg, _ := ev["error"].(string)
	if !strings.Contains(msg, "500") {
		t.Fatalf("error message = %q, want the status code", msg)
	}
	// The healthy server still answers.
	if _, _, err := m.Call(context.Background(), "mcp__good__echo", `{"text":"x"}`); err != nil {
		t.Fatalf("the healthy server stopped working: %v", err)
	}
}

// TestHTTPTransportTimesOut proves a slow server is dropped instead of
// wedging the session.
func TestHTTPTransportTimesOut(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		slow.Close()
	})

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"slow": {Transport: "http", URL: slow.URL, TimeoutS: 1},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	if names := defNames(m.Definitions()); len(names) != 0 {
		t.Fatalf("definitions = %v, want none", names)
	}
	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event for the slow server")
	}
	msg, _ := ev["error"].(string)
	if !strings.Contains(msg, "timed out") {
		t.Fatalf("error message = %q, want a timeout", msg)
	}
}

// TestHTTPTransportMalformedBodyIsSkipped proves a server that answers with
// garbage is dropped with a clear error.
func TestHTTPTransportMalformedBodyIsSkipped(t *testing.T) {
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer garbage.Close()

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"garbage": {Transport: "http", URL: garbage.URL, TimeoutS: 5},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	if names := defNames(m.Definitions()); len(names) != 0 {
		t.Fatalf("definitions = %v, want none", names)
	}
	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event for the malformed server")
	}
	msg, _ := ev["error"].(string)
	if !strings.Contains(msg, "malformed") {
		t.Fatalf("error message = %q, want a malformed-response error", msg)
	}
}

// TestHTTPTransportEmptyStreamIsSkipped proves an event stream without a
// JSON-RPC message is an error, not an empty success.
func TestHTTPTransportEmptyStreamIsSkipped(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n")
	}))
	defer empty.Close()

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"empty": {Transport: "http", URL: empty.URL, TimeoutS: 5},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	if names := defNames(m.Definitions()); len(names) != 0 {
		t.Fatalf("definitions = %v, want none", names)
	}
	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event")
	}
	msg, _ := ev["error"].(string)
	if !strings.Contains(msg, "JSON-RPC") {
		t.Fatalf("error message = %q, want a missing-response error", msg)
	}
}

// TestHTTPTransportUnreachableServerIsSkipped proves a server that is down
// disables only itself.
func TestHTTPTransportUnreachableServerIsSkipped(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close() // nothing listens there any more

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"dead": {Transport: "http", URL: url, TimeoutS: 2},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	if names := defNames(m.Definitions()); len(names) != 0 {
		t.Fatalf("definitions = %v, want none", names)
	}
	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event for the unreachable server")
	}
}

// TestHTTPTransportBearerTokenComesFromEnv proves the token is read from the
// named environment variable, sent as a bearer token, and never echoed.
func TestHTTPTransportBearerTokenComesFromEnv(t *testing.T) {
	const token = "s3cr3t-bearer-token"
	t.Setenv("DOCS_MCP_TOKEN", token)
	srv, rec := newFakeHTTPServer(t, "json")

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"docs": {Transport: "http", URL: srv.URL, TimeoutS: 5, BearerEnv: "DOCS_MCP_TOKEN"},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	if names := defNames(m.Definitions()); len(names) != 2 {
		t.Fatalf("definitions = %v, want both tools", names)
	}
	var saw string
	for _, h := range rec.all() {
		if h.Get("Authorization") != "" {
			saw = h.Get("Authorization")
		}
	}
	if saw != "Bearer "+token {
		t.Fatalf("Authorization header = %q, want the bearer token", saw)
	}
	// The token must not appear in any log record.
	for _, ev := range log.events {
		if strings.Contains(fmt.Sprint(ev.val), token) {
			t.Fatalf("the token leaked into a log record: %+v", ev.val)
		}
	}
}

// TestHTTPTransportMissingBearerEnvIsAnError proves a missing secret is a
// clear error, not a silent anonymous request.
func TestHTTPTransportMissingBearerEnvIsAnError(t *testing.T) {
	t.Setenv("DOCS_MCP_TOKEN", "")
	srv, rec := newFakeHTTPServer(t, "json")

	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"docs": {Transport: "http", URL: srv.URL, TimeoutS: 5, BearerEnv: "DOCS_MCP_TOKEN"},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	if names := defNames(m.Definitions()); len(names) != 0 {
		t.Fatalf("definitions = %v, want none", names)
	}
	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event")
	}
	msg, _ := ev["error"].(string)
	if !strings.Contains(msg, "DOCS_MCP_TOKEN") {
		t.Fatalf("error message = %q, want the name of the missing variable", msg)
	}
	if len(rec.all()) != 0 {
		t.Fatal("the server was contacted without a token")
	}
}

// TestHTTPTransportHeaderEnv proves a configured header takes its value
// from the named environment variable.
func TestHTTPTransportHeaderEnv(t *testing.T) {
	t.Setenv("DOCS_MCP_KEY", "header-value")
	srv, rec := newFakeHTTPServer(t, "json")

	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"docs": {Transport: "http", URL: srv.URL, TimeoutS: 5, HeadersEnv: map[string]string{"X-Api-Key": "DOCS_MCP_KEY"}},
	}}
	m := Start(cfg, nil)
	defer m.Close()

	var saw string
	for _, h := range rec.all() {
		if h.Get("X-Api-Key") != "" {
			saw = h.Get("X-Api-Key")
		}
	}
	if saw != "header-value" {
		t.Fatalf("X-Api-Key header = %q, want the value from the environment", saw)
	}
}

// TestHTTPTransportWithoutURLIsSkipped proves a half-configured HTTP server
// is reported instead of silently ignored.
func TestHTTPTransportWithoutURLIsSkipped(t *testing.T) {
	log := &eventLog{}
	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"broken": {Transport: "http"},
	}}
	m := Start(cfg, log.log)
	defer m.Close()

	ev := log.find("mcp_error")
	if ev == nil {
		t.Fatal("no mcp_error event")
	}
	msg, _ := ev["error"].(string)
	if !strings.Contains(msg, "url") {
		t.Fatalf("error message = %q, want it to name the missing url", msg)
	}
}

// TestHTTPTransportDoesNotAffectStdioServers proves the two transports live
// side by side: a broken HTTP server does not disturb a healthy stdio one.
func TestHTTPTransportDoesNotAffectStdioServers(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close()

	cfg := config.MCP{Enabled: true, Servers: map[string]config.MCPServer{
		"dead": {Transport: "http", URL: url, TimeoutS: 2},
		"fake": {
			Command:    os.Args[0],
			Args:       []string{"-test.run=TestHelperProcess", "--"},
			Env:        map[string]string{"MCP_HELPER": "1"},
			TimeoutS:   10,
			AllowTools: []string{"echo"},
		},
	}}
	m := Start(cfg, nil)
	defer m.Close()

	names := defNames(m.Definitions())
	if len(names) != 1 || names[0] != "mcp__fake__echo" {
		t.Fatalf("definitions = %v, want only the stdio tool", names)
	}
}

// TestHTTPTransportCarriesTheSessionID proves a server that hands out a
// session on initialize gets it back on every later request.
func TestHTTPTransportCarriesTheSessionID(t *testing.T) {
	rec := &httpRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		raw, _ := io.ReadAll(r.Body)
		var req httpMCPRequest
		_ = json.Unmarshal(raw, &req)
		result := map[string]any{"tools": fakeHTTPTools()}
		if req.Method == "initialize" {
			result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-http", "version": "1"},
			}
			w.Header().Set("Mcp-Session-Id", "session-42")
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := Start(httpServerCfg("web", srv.URL, nil), nil)
	defer m.Close()
	if len(m.Definitions()) == 0 {
		t.Fatal("the server was not discovered")
	}
	headers := rec.all()
	if len(headers) == 0 {
		t.Fatal("the server saw no request")
	}
	if headers[0].Get("Mcp-Session-Id") != "" {
		t.Fatal("the initialize request carried a session id")
	}
	for i, h := range headers[1:] {
		if h.Get("Mcp-Session-Id") != "session-42" {
			t.Fatalf("request %d did not carry the session id: %v", i+1, h.Get("Mcp-Session-Id"))
		}
	}
}

// TestIsHTTPTransport pins the transport spellings.
func TestIsHTTPTransport(t *testing.T) {
	for _, in := range []string{"http", "HTTP", " https ", "streamable-http", "sse"} {
		if !isHTTPTransport(in) {
			t.Errorf("isHTTPTransport(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "stdio", "tcp", "websocket"} {
		if isHTTPTransport(in) {
			t.Errorf("isHTTPTransport(%q) = true, want false", in)
		}
	}
}

// TestDecodeRPCResponse covers the body shapes a server may answer with.
func TestDecodeRPCResponse(t *testing.T) {
	if _, err := decodeRPCResponse(nil); err == nil {
		t.Error("an empty body was accepted")
	}
	if _, err := decodeRPCResponse([]byte("not json")); err == nil {
		t.Error("garbage was accepted")
	}
	resp, err := decodeRPCResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	if err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if string(resp.result) != `{"ok":true}` {
		t.Fatalf("result = %s", resp.result)
	}
	resp, err = decodeRPCResponse([]byte(`[{"jsonrpc":"2.0","id":1,"result":{"ok":true}}]`))
	if err != nil {
		t.Fatalf("batch rejected: %v", err)
	}
	if string(resp.result) != `{"ok":true}` {
		t.Fatalf("batch result = %s", resp.result)
	}
	resp, err = decodeRPCResponse([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`))
	if err != nil {
		t.Fatalf("error response rejected: %v", err)
	}
	if resp.err == nil || resp.err.Message != "boom" {
		t.Fatalf("error = %+v", resp.err)
	}
}

// TestHTTPMissingURLIsAnError proves the transport refuses a half-configured
// server before it is ever contacted.
func TestHTTPMissingURLIsAnError(t *testing.T) {
	if _, err := newHTTPTransport("x", config.MCPServer{Transport: "http"}, time.Second, nil); err == nil {
		t.Fatal("a server without a url was accepted")
	}
	if _, err := newHTTPTransport("x", config.MCPServer{Transport: "http", URL: "ftp://x/y"}, time.Second, nil); err == nil {
		t.Fatal("a non-http url was accepted")
	}
}
