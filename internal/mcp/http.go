package mcp

// The HTTP transport for MCP servers (M12.1).
//
// A stdio server is a child process on this machine; an HTTP server is not.
// The extension point is the same — a server advertises tools, the harness
// namespaces them mcp__<server>__<tool> and gates them through the normal
// permission matrix — but the wire differs:
//
//   - one HTTP POST per JSON-RPC message (the streamable HTTP shape);
//   - the answer is either a JSON body or an SSE stream whose `data:` frames
//     carry the JSON-RPC message;
//   - the client asks for both with "Accept: application/json,
//     text/event-stream", so the server picks.
//
// Failure isolation is the point of this file. A server that is down, slow or
// answers with garbage disables *that server only*: the manager warns, skips
// it, and the rest of the session stays usable. Timeouts are mandatory —
// an HTTP server that never answers must not wedge the session.
//
// Secrets never appear in the config and never appear in a log line or an
// error message. A server entry names the environment variables that hold
// its bearer token and its header values; the values are read from the
// process environment at request time and are never echoed back.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/config"
)

const (
	// httpProtocolHeader carries the negotiated protocol version on every
	// request after initialize (the streamable HTTP rule).
	httpProtocolHeader = "MCP-Protocol-Version"
	// httpSessionHeader carries the session id a server hands out on
	// initialize. A server that uses sessions expects it back on every
	// later request; a server that does not, never sends it.
	httpSessionHeader = "Mcp-Session-Id"
	// maxHTTPBody caps one response body. A tool answer that needs more is
	// a broken server, not a reason to exhaust the operator's memory.
	maxHTTPBody = 4 << 20
	// maxHTTPErrorBody caps the snippet of an error body that is quoted
	// back to the operator.
	maxHTTPErrorBody = 2048
)

// isHTTPTransport reports whether a server entry selects the HTTP transport.
//
// "http" is the documented name; "streamable-http" and "sse" are accepted
// spellings of the same thing, because the server may answer with an SSE
// stream either way.
func isHTTPTransport(transport string) bool {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "http", "https", "streamable-http", "streamable_http", "sse":
		return true
	}
	return false
}

// httpTransport speaks one MCP server over HTTP.
type httpTransport struct {
	name   string
	url    string
	client *http.Client
	// timeout bounds one request (connect, headers and body).
	timeout time.Duration
	// bearerEnv names the environment variable holding the bearer token.
	bearerEnv string
	// headerEnv maps a header name to the environment variable holding its
	// value. Only the names are stored; the values are read per request.
	headerEnv map[string]string
	log       func(kind string, v any)

	mu     sync.Mutex
	nextID int
	// sessionID is the session the server handed out on initialize, if any.
	sessionID string
}

// newHTTPTransport validates one HTTP server entry.
func newHTTPTransport(name string, sc config.MCPServer, timeout time.Duration, log func(string, any)) (*httpTransport, error) {
	endpoint := strings.TrimSpace(sc.URL)
	if endpoint == "" {
		return nil, fmt.Errorf("transport is http but url is empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	tr := &httpTransport{
		name:      name,
		url:       endpoint,
		client:    &http.Client{Timeout: timeout},
		timeout:   timeout,
		bearerEnv: strings.TrimSpace(sc.BearerEnv),
		headerEnv: map[string]string{},
		log:       log,
	}
	for h, env := range sc.HeadersEnv {
		h = strings.TrimSpace(h)
		env = strings.TrimSpace(env)
		if h == "" || env == "" {
			continue
		}
		tr.headerEnv[h] = env
	}
	return tr, nil
}

// newHTTPClient builds the client for an HTTP server. The stdio fields stay
// empty: there is no child process to talk to.
func newHTTPClient(name string, sc config.MCPServer, timeout time.Duration, log func(string, any)) (*Client, error) {
	tr, err := newHTTPTransport(name, sc, timeout, log)
	if err != nil {
		return nil, err
	}
	return &Client{
		name:    name,
		timeout: timeout,
		log:     log,
		http:    tr,
		pending: map[int]chan rpcResponse{},
		done:    make(chan struct{}),
	}, nil
}

// request performs one JSON-RPC request and returns its result.
func (t *httpTransport) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	t.mu.Unlock()

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	raw, err := t.post(ctx, body, false, method == "initialize")
	if err != nil {
		return nil, err
	}
	resp, err := decodeRPCResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %s: %w", t.name, method, err)
	}
	if resp.err != nil {
		return nil, resp.err
	}
	if len(resp.result) == 0 {
		return nil, fmt.Errorf("mcp %s: %s returned no result", t.name, method)
	}
	return resp.result, nil
}

// notify posts a JSON-RPC notification. A server that does not accept
// notifications is still usable: the request/response path is what the
// harness needs, so a rejection is reported, not fatal.
func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := t.post(ctx, body, true, false); err != nil {
		return err
	}
	return nil
}

// post sends one message and returns the response body.
func (t *httpTransport) post(ctx context.Context, body []byte, notification bool, first bool) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", t.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if !first {
		req.Header.Set(httpProtocolHeader, protocolVersion)
	}
	t.mu.Lock()
	session := t.sessionID
	t.mu.Unlock()
	if session != "" {
		req.Header.Set(httpSessionHeader, session)
	}
	if err := t.applyAuth(req); err != nil {
		return nil, fmt.Errorf("mcp %s: %w", t.name, err)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp %s: timed out after %s", t.name, t.timeout)
		}
		return nil, fmt.Errorf("mcp %s: %w", t.name, err)
	}
	defer resp.Body.Close()

	// A server that hands out a session expects it back on every later
	// request. A server that does not, never sends the header.
	if sid := strings.TrimSpace(resp.Header.Get(httpSessionHeader)); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPErrorBody))
		if notification {
			// Not fatal: the server may simply not want notifications.
			t.logf("mcp_notification_rejected", map[string]any{
				"server": t.name, "status": resp.StatusCode,
			})
			return nil, nil
		}
		return nil, fmt.Errorf("mcp %s: http %d: %s", t.name, resp.StatusCode,
			strings.TrimSpace(string(snippet)))
	}
	if notification {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPErrorBody))
		return nil, nil
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return t.readEventStream(resp.Body)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return nil, fmt.Errorf("mcp %s: read response: %w", t.name, err)
	}
	return out, nil
}

// applyAuth adds the bearer token and the configured headers.
//
// The values come from the environment and are never written anywhere: not
// into an error, not into a log record, not into the request body. A named
// variable that is not set is an error — an anonymous request to a server
// that expects a token would fail later and less clearly.
func (t *httpTransport) applyAuth(req *http.Request) error {
	if t.bearerEnv != "" {
		token := strings.TrimSpace(os.Getenv(t.bearerEnv))
		if token == "" {
			return fmt.Errorf("needs %s (set it in the environment)", t.bearerEnv)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	names := make([]string, 0, len(t.headerEnv))
	for h := range t.headerEnv {
		names = append(names, h)
	}
	sort.Strings(names)
	for _, h := range names {
		env := t.headerEnv[h]
		value := strings.TrimSpace(os.Getenv(env))
		if value == "" {
			return fmt.Errorf("header %s needs %s (set it in the environment)", h, env)
		}
		req.Header.Set(h, value)
	}
	return nil
}

// readEventStream parses an SSE body into the JSON-RPC message it carries.
//
// The streamable HTTP servers answer with `data:` frames. The frame may hold
// one JSON-RPC message or a batch; the first message with a result or an
// error wins. A stream that ends without a message is an error, never an
// empty success.
func (t *httpTransport) readEventStream(r io.Reader) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxHTTPBody)
	var data []string
	flush := func() ([]byte, bool) {
		payload := strings.Join(data, "\n")
		data = data[:0]
		if strings.TrimSpace(payload) == "" {
			return nil, false
		}
		if !isRPCMessage([]byte(payload)) {
			return nil, false
		}
		return []byte(payload), true
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			if payload, ok := flush(); ok {
				return payload, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mcp %s: read event stream: %w", t.name, err)
	}
	if payload, ok := flush(); ok {
		return payload, nil
	}
	return nil, fmt.Errorf("mcp %s: the event stream carried no JSON-RPC response", t.name)
}

// close releases the transport's connections.
func (t *httpTransport) close() {
	if t == nil || t.client == nil {
		return
	}
	t.client.CloseIdleConnections()
}

// logf records one transport event, if the caller wants records.
func (t *httpTransport) logf(kind string, v any) {
	if t == nil || t.log == nil {
		return
	}
	t.log(kind, v)
}

// decodeRPCResponse turns a response body into a JSON-RPC response.
func decodeRPCResponse(body []byte) (rpcResponse, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return rpcResponse{}, fmt.Errorf("the server answered with an empty body")
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return rpcResponse{}, fmt.Errorf("malformed response: %w", err)
		}
		for _, item := range batch {
			if resp, ok := parseRPCMessage(item); ok {
				return resp, nil
			}
		}
		return rpcResponse{}, fmt.Errorf("malformed response: the batch carried no JSON-RPC message")
	}
	resp, ok := parseRPCMessage(trimmed)
	if !ok {
		return rpcResponse{}, fmt.Errorf("malformed response: not a JSON-RPC message")
	}
	return resp, nil
}

// isRPCMessage reports whether the bytes hold a JSON-RPC message.
func isRPCMessage(raw []byte) bool {
	_, ok := parseRPCMessage(raw)
	return ok
}

// parseRPCMessage decodes one JSON-RPC message.
func parseRPCMessage(raw []byte) (rpcResponse, bool) {
	var env struct {
		ID     *json.RawMessage `json:"id"`
		Result json.RawMessage  `json:"result"`
		Error  *rpcError        `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return rpcResponse{}, false
	}
	if env.ID == nil && env.Error == nil && env.Result == nil {
		return rpcResponse{}, false
	}
	return rpcResponse{result: env.Result, err: env.Error}, true
}
