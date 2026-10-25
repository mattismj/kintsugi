package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServer_Stdio_Handshake(t *testing.T) {
	srv := NewServer()
	srv.Register("initialize", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]string{"status": "ok"}, nil
	})

	input := `{"jsonrpc":"2.0","method":"initialize","id":1}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	ctx := context.Background()
	if err := srv.ServeStdio(ctx, reader, &output); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if resp.ID != float64(1) {
		t.Errorf("id = %v, want 1", resp.ID)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
}

func TestServer_Stdio_MethodNotFound(t *testing.T) {
	srv := NewServer()

	input := `{"jsonrpc":"2.0","method":"unknown","id":1}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	ctx := context.Background()
	if err := srv.ServeStdio(ctx, reader, &output); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error, got nil")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, CodeMethodNotFound)
	}
}

func TestServer_Stdio_InvalidJSON(t *testing.T) {
	srv := NewServer()

	input := `{invalid json}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	ctx := context.Background()
	if err := srv.ServeStdio(ctx, reader, &output); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error, got nil")
	}
	if resp.Error.Code != CodeParseError {
		t.Errorf("error code = %d, want %d", resp.Error.Code, CodeParseError)
	}
}

func TestServer_Stdio_InvalidVersion(t *testing.T) {
	srv := NewServer()
	srv.Register("test", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "ok", nil
	})

	input := `{"jsonrpc":"1.0","method":"test","id":1}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	ctx := context.Background()
	if err := srv.ServeStdio(ctx, reader, &output); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error, got nil")
	}
	if resp.Error.Code != CodeInvalidRequest {
		t.Errorf("error code = %d, want %d", resp.Error.Code, CodeInvalidRequest)
	}
}

func TestServer_Stdio_Notification(t *testing.T) {
	srv := NewServer()
	called := false
	srv.Register("notify", func(ctx context.Context, params json.RawMessage) (any, error) {
		called = true
		return nil, nil
	})

	// Notification has no id field.
	input := `{"jsonrpc":"2.0","method":"notify"}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	ctx := context.Background()
	if err := srv.ServeStdio(ctx, reader, &output); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	if !called {
		t.Error("handler not called")
	}
	if output.Len() != 0 {
		t.Errorf("expected no response for notification, got %q", output.String())
	}
}

func TestServer_TCP_LoopbackOnly(t *testing.T) {
	srv := NewServer()
	srv.SetAuth("secret123")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start server on loopback.
	go func() {
		srv.ServeTCP(ctx, "127.0.0.1:0")
	}()

	time.Sleep(100 * time.Millisecond)

	// Try to connect to non-loopback address (should fail).
	err := srv.ServeTCP(ctx, "0.0.0.0:9999")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("expected loopback error, got %v", err)
	}
}

func TestServer_TCP_AuthRequired(t *testing.T) {
	srv := NewServer()
	srv.SetAuth("secret123")
	srv.Register("test", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "ok", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start server on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handleTCPConn(ctx, conn)
		}
	}()

	// Connect without auth.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send request without auth line.
	req := Request{JSONRPC: "2.0", Method: "test", ID: 1}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Read response (should be auth error).
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected auth error, got nil")
	}
	if resp.Error.Code != CodeAuthRequired {
		t.Errorf("error code = %d, want %d", resp.Error.Code, CodeAuthRequired)
	}
}

func TestServer_TCP_AuthSuccess(t *testing.T) {
	srv := NewServer()
	srv.SetAuth("secret123")
	srv.Register("test", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "success", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handleTCPConn(ctx, conn)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send auth line.
	if _, err := conn.Write([]byte("AUTH secret123\n")); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	// Send request.
	req := Request{JSONRPC: "2.0", Method: "test", ID: 1}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Read response.
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if resp.Result != "success" {
		t.Errorf("result = %v, want 'success'", resp.Result)
	}
}

func TestServer_HTTP_LoopbackOnly(t *testing.T) {
	srv := NewServer()
	srv.Register("test", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "ok", nil
	})

	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// HTTP test server is on loopback, should work.
	req := Request{JSONRPC: "2.0", Method: "test", ID: 1}
	body, _ := json.Marshal(req)

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestServer_HTTP_AuthRequired(t *testing.T) {
	srv := NewServer()
	srv.SetAuth("secret123")
	srv.Register("test", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "ok", nil
	})

	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req := Request{JSONRPC: "2.0", Method: "test", ID: 1}
	body, _ := json.Marshal(req)

	// Request without auth header.
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServer_HTTP_AuthSuccess(t *testing.T) {
	srv := NewServer()
	srv.SetAuth("secret123")
	srv.Register("test", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "success", nil
	})

	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req := Request{JSONRPC: "2.0", Method: "test", ID: 1}
	body, _ := json.Marshal(req)

	httpReq, _ := http.NewRequest("POST", ts.URL, bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer secret123")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var rpcResp Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if rpcResp.Error != nil {
		t.Errorf("unexpected error: %v", rpcResp.Error)
	}
	if rpcResp.Result != "success" {
		t.Errorf("result = %v, want 'success'", rpcResp.Result)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9876", true},
		{"127.0.0.1", true},
		{"::1:9876", true},
		{"::1", true},
		{"[::1]:9876", true},
		{"[::1]", true},
		{"localhost:9876", true},
		{"localhost", true},
		{"Localhost", true},
		{"0.0.0.0:9876", false},
		{"192.168.1.1:9876", false},
		// An empty host is every interface: exactly what the guard refuses.
		{":9876", false},
		// A public IPv6 must not be mistaken for a host:port split.
		{"2001:db8::1", false},
		{"[2001:db8::1]:9876", false},
		{"not-an-address", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := isLoopbackAddr(tt.addr)
			if got != tt.want {
				t.Errorf("isLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

// A guard that accepts an unbracketed IPv6 must also hand net.Listen the
// bracketed form it requires, or the server passes the check and dies at bind.
func TestBindAddrCanonicalisesIPv6(t *testing.T) {
	tests := []struct {
		addr, want string
	}{
		{"::1:9876", "[::1]:9876"},
		{"[::1]:9876", "[::1]:9876"},
		{"127.0.0.1:9876", "127.0.0.1:9876"},
		{"localhost:9876", "localhost:9876"},
		{"127.0.0.1", "127.0.0.1"},
	}
	for _, tt := range tests {
		if got := bindAddr(tt.addr); got != tt.want {
			t.Errorf("bindAddr(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

func TestServer_Stdio_MultipleRequests(t *testing.T) {
	srv := NewServer()
	callCount := 0
	srv.Register("increment", func(ctx context.Context, params json.RawMessage) (any, error) {
		callCount++
		return map[string]int{"count": callCount}, nil
	})

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"increment","id":1}`,
		`{"jsonrpc":"2.0","method":"increment","id":2}`,
		`{"jsonrpc":"2.0","method":"increment","id":3}`,
	}, "\n") + "\n"

	reader := strings.NewReader(input)
	var output bytes.Buffer

	ctx := context.Background()
	if err := srv.ServeStdio(ctx, reader, &output); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	scanner := bufio.NewScanner(&output)
	count := 0
	for scanner.Scan() {
		count++
		var resp Response
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal line %d: %v", count, err)
		}
		if resp.Error != nil {
			t.Errorf("line %d: unexpected error: %v", count, resp.Error)
		}
	}

	if count != 3 {
		t.Errorf("got %d responses, want 3", count)
	}
	if callCount != 3 {
		t.Errorf("handler called %d times, want 3", callCount)
	}
}
