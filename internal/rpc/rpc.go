// Package rpc implements JSON-RPC 2.0 over stdio and TCP with loopback-only binding.
// The harness acts as the server; external clients (IDE, web UI, another agent) drive it.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
	ID      any       `json:"id,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("RPC error %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeAuthRequired   = -32000
)

// Handler processes one RPC method call.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Server is a JSON-RPC 2.0 server that can serve over stdio or TCP.
type Server struct {
	handlers map[string]Handler
	auth     string // shared secret for TCP mode
	mu       sync.RWMutex
}

// NewServer creates a new RPC server.
func NewServer() *Server {
	return &Server{
		handlers: make(map[string]Handler),
	}
}

// Register adds a handler for a method.
func (s *Server) Register(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// SetAuth sets the shared secret required for TCP connections.
func (s *Server) SetAuth(secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = secret
}

// ServeStdio reads NDJSON requests from stdin and writes responses to stdout.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB max line
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			resp := Response{
				JSONRPC: "2.0",
				Error:   &RPCError{Code: CodeParseError, Message: "parse error"},
			}
			if err := enc.Encode(resp); err != nil {
				return err
			}
			continue
		}

		resp := s.handle(ctx, req)
		if resp.ID == nil && resp.Error == nil {
			// Notification (no id): no response.
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// handle dispatches one request.
func (s *Server) handle(ctx context.Context, req Request) Response {
	if req.JSONRPC != "2.0" {
		return Response{
			JSONRPC: "2.0",
			Error:   &RPCError{Code: CodeInvalidRequest, Message: "invalid jsonrpc version"},
			ID:      req.ID,
		}
	}

	s.mu.RLock()
	handler, ok := s.handlers[req.Method]
	s.mu.RUnlock()

	if !ok {
		return Response{
			JSONRPC: "2.0",
			Error:   &RPCError{Code: CodeMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)},
			ID:      req.ID,
		}
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		code := CodeInternalError
		if rpcErr, ok := err.(*RPCError); ok {
			code = rpcErr.Code
		}
		return Response{
			JSONRPC: "2.0",
			Error:   &RPCError{Code: code, Message: err.Error()},
			ID:      req.ID,
		}
	}

	return Response{
		JSONRPC: "2.0",
		Result:  result,
		ID:      req.ID,
	}
}

// ServeTCP listens on a TCP port, enforcing loopback-only binding and auth.
func (s *Server) ServeTCP(ctx context.Context, addr string) error {
	// Enforce loopback-only binding.
	if !isLoopbackAddr(addr) {
		return fmt.Errorf("TCP server must bind to loopback (127.0.0.1 or ::1), got %s", addr)
	}

	ln, err := net.Listen("tcp", bindAddr(addr))
	if err != nil {
		return err
	}
	defer ln.Close()

	// Accept connections in a loop, checking context cancellation.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleTCPConn(ctx, conn)
	}
}

// handleTCPConn handles one TCP connection with auth and loopback checks.
func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Verify the connection is from loopback.
	remoteAddr := conn.RemoteAddr().(*net.TCPAddr)
	if !remoteAddr.IP.IsLoopback() {
		return
	}

	// Read the first line as an HTTP-like request with Authorization header.
	// We use a simple text protocol: "AUTH <secret>\n" followed by NDJSON.
	reader := bufio.NewReader(conn)
	authLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	s.mu.RLock()
	expectedAuth := s.auth
	s.mu.RUnlock()

	if expectedAuth != "" {
		// Parse "AUTH <secret>"
		parts := strings.SplitN(strings.TrimSpace(authLine), " ", 2)
		if len(parts) != 2 || parts[0] != "AUTH" || parts[1] != expectedAuth {
			// Send error response and close.
			errResp := Response{
				JSONRPC: "2.0",
				Error:   &RPCError{Code: CodeAuthRequired, Message: "authentication required"},
			}
			json.NewEncoder(conn).Encode(errResp)
			return
		}
	}

	// Now serve NDJSON over the connection.
	if err := s.ServeStdio(ctx, reader, conn); err != nil {
		// Connection errors are expected when the client disconnects.
		return
	}
}

// IsLoopbackBindAddr reports whether an address binds loopback only. It is the
// exported form of the bind guard so every transport can refuse a
// network-reachable address before it listens, rather than relying on per-request
// checks alone: the API carries the operator's tool authority, so a bound
// non-loopback address is a leak even if each request is then refused.
func IsLoopbackBindAddr(addr string) bool {
	return isLoopbackAddr(addr)
}

// isLoopbackAddr reports whether an address names a loopback interface. It
// accepts a bare host ("::1"), a host:port ("127.0.0.1:9876"), a bracketed IPv6
// ("[::1]:9876") and the unbracketed IPv6 form ("::1:9876") an operator types
// before discovering Go wants brackets — bindAddr canonicalises the last one.
// An empty host (":9876", "0.0.0.0") is every interface, which is exactly what
// this guard exists to refuse.
func isLoopbackAddr(addr string) bool {
	_, _, ok := splitBindAddr(strings.TrimSpace(addr))
	return ok
}

// BindAddr canonicalises a bind address into the form net.Listen wants, so an
// operator who typed "::1:9876" gets "[::1]:9876" instead of a listen failure.
func BindAddr(addr string) string {
	return bindAddr(addr)
}

// bindAddr is the address to hand net.Listen, so a guard that accepts
// "::1:9876" also binds the form Go requires ("[::1]:9876") instead of passing
// a check and then failing at listen.
func bindAddr(addr string) string {
	host, port, ok := splitBindAddr(strings.TrimSpace(addr))
	if !ok || port == "" {
		return strings.TrimSpace(addr)
	}
	return net.JoinHostPort(host, port)
}

// splitBindAddr splits a bind address into host and port and reports whether
// the host is loopback. It accepts a bare host ("::1"), a host:port
// ("127.0.0.1:9876"), a bracketed IPv6 ("[::1]:9876"), and the unbracketed IPv6
// with port ("::1:9876") an operator types before discovering Go wants
// brackets. A bare unbracketed IPv6 like "2001:db8::1" has no port and is read
// as a host.
func splitBindAddr(addr string) (host, port string, loopback bool) {
	if addr == "" {
		return "", "", false
	}
	// Bracketed form first: net.SplitHostPort handles it.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// SplitHostPort failed. Either there is no port (a bare host) or an
		// unbracketed IPv6 with a trailing port. Try splitting at the last
		// colon only when the part before it parses as an IPv6 address.
		if i := strings.LastIndex(addr, ":"); i > 0 {
			h, p := addr[:i], addr[i+1:]
			if net.ParseIP(h) != nil && p != "" && isPort(p) {
				host, port = h, p
			} else {
				host, port = addr, ""
			}
		} else {
			host, port = addr, ""
		}
	}
	// Strip brackets a caller may have left on a bare host.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	host = strings.TrimSpace(host)
	if host == "" {
		return host, port, false // every interface, which the guard refuses
	}
	if strings.EqualFold(host, "localhost") {
		return host, port, true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host, port, false
	}
	return host, port, ip.IsLoopback()
}

// isPort reports whether s is a valid TCP port string.
func isPort(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0 && len(s) <= 5
}

// remoteIsLoopback reports whether a net/http RemoteAddr came from loopback.
// RemoteAddr is always host:port and brackets an IPv6 host, so a naive split on
// ":" would read "[::1]" as "[" and refuse every IPv6 client.
func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

// HTTPHandler returns an HTTP handler for the RPC server, enforcing loopback and auth.
func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Enforce loopback.
		if !remoteIsLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}

		// Check auth.
		s.mu.RLock()
		expectedAuth := s.auth
		s.mu.RUnlock()

		if expectedAuth != "" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+expectedAuth {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Only accept POST.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			resp := Response{
				JSONRPC: "2.0",
				Error:   &RPCError{Code: CodeParseError, Message: "parse error"},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		resp := s.handle(r.Context(), req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}
