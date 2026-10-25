// Command fakeserver is a standalone fake ACP agent used by the internal/acp
// and internal/dag tests. It speaks line-delimited JSON-RPC 2.0 on stdio and
// covers the behaviours the client has to survive: a normal prompt turn, an
// agent that asks the client to execute something, a turn that only stops on
// session/cancel, an agent that emits garbage, and an agent that produces no
// final message.
//
// It is built by the tests' TestMain with `go build` and then run as a child
// process, so the tests exercise the real stdio transport rather than an
// in-process pipe.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	mode      = flag.String("mode", "ok", "ok | toolcall | hang | cancel | garbage | empty | serverreq | exit | stubborn")
	final     = flag.String("final", "done: the fake agent finished the node", "final agent message text")
	sessionID = flag.String("session", "sess-fake", "session id to hand out")
	delay     = flag.Duration("delay", 0, "delay before answering a prompt")
	toolKind  = flag.String("tool-kind", "execute", "tool-call kind used by mode=toolcall")
	pidFile   = flag.String("pidfile", "", "write this process's pid here (kill assertions in tests)")
)

func main() {
	flag.Parse()
	if *pidFile != "" {
		_ = os.WriteFile(*pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
	}
	s := &server{out: bufio.NewWriter(os.Stdout), awaiting: map[int]chan rawMessage{}, cancel: make(chan struct{})}
	if *mode == "exit" {
		// Die immediately without speaking: tests the client's handling of an
		// agent that never handshakes.
		os.Exit(3)
	}
	s.loop()
}

type rawMessage = json.RawMessage

type server struct {
	out *bufio.Writer
	mu  sync.Mutex
	id  int

	amu      sync.Mutex
	awaiting map[int]chan rawMessage

	cancelOnce sync.Once
	cancel     chan struct{}
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *server) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeserver: encode: %v\n", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

func (s *server) result(id json.RawMessage, v any) {
	b, _ := json.Marshal(v)
	s.write(envelope{JSONRPC: "2.0", ID: id, Result: b})
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	s.write(envelope{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *server) notify(method string, params any) {
	b, _ := json.Marshal(params)
	s.write(envelope{JSONRPC: "2.0", Method: method, Params: b})
}

// request is a server-initiated request; it returns the client's answer.
func (s *server) request(method string, params any) (envelope, error) {
	s.mu.Lock()
	s.id++
	id := s.id
	s.mu.Unlock()

	ch := make(chan rawMessage, 1)
	s.amu.Lock()
	s.awaiting[id] = ch
	s.amu.Unlock()
	defer func() {
		s.amu.Lock()
		delete(s.awaiting, id)
		s.amu.Unlock()
	}()

	b, _ := json.Marshal(params)
	s.write(envelope{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: method, Params: b})

	select {
	case raw := <-ch:
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return envelope{}, err
		}
		return env, nil
	case <-s.cancel:
		return envelope{}, io.EOF
	case <-time.After(20 * time.Second):
		return envelope{}, fmt.Errorf("fakeserver: no answer to %s", method)
	}
}

func (s *server) chunk(text string) {
	s.notify("session/update", map[string]any{
		"sessionId":     *sessionID,
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

func (s *server) loop() {
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := s.readLine(in)
		if len(line) > 0 {
			s.handle(line)
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "fakeserver: read: %v\n", err)
			}
			if *mode == "stubborn" {
				// A well-behaved agent exits when its stdin closes. This one
				// does not, so the client has to prove it kills what it owns.
				select {}
			}
			return
		}
	}
}

func (s *server) readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(buf) > 0 {
			return buf, nil
		}
		return buf, err
	}
}

func (s *server) handle(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return
	}
	var env envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		fmt.Fprintf(os.Stderr, "fakeserver: bad frame from client: %v\n", err)
		return
	}
	if env.Method == "" {
		id := 0
		_, _ = fmt.Sscan(string(env.ID), &id)
		s.amu.Lock()
		ch := s.awaiting[id]
		s.amu.Unlock()
		if ch != nil {
			raw, _ := json.Marshal(env)
			ch <- raw
		}
		return
	}
	fmt.Fprintf(os.Stderr, "fakeserver: <- %s\n", env.Method)
	switch env.Method {
	case "initialize":
		s.result(env.ID, map[string]any{
			"protocolVersion":   1,
			"agentInfo":         map[string]any{"name": "fakeserver", "version": "0"},
			"agentCapabilities": map[string]any{"promptCapabilities": map[string]any{"image": false}},
			"authMethods":       []any{},
		})
	case "session/new":
		s.result(env.ID, map[string]any{"sessionId": *sessionID})
	case "session/prompt":
		go s.prompt(env.ID, env.Params)
	case "session/cancel":
		fmt.Fprintf(os.Stderr, "fakeserver: <- session/cancel\n")
		s.cancelOnce.Do(func() { close(s.cancel) })
	default:
		s.fail(env.ID, -32601, "fakeserver does not implement "+env.Method)
	}
}

func (s *server) prompt(id json.RawMessage, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	text := ""
	for _, b := range p.Prompt {
		if b.Type == "text" {
			text += b.Text
		}
	}
	if p.SessionID != *sessionID {
		s.fail(id, -32602, "unknown session "+p.SessionID)
		return
	}
	if *delay > 0 {
		select {
		case <-time.After(*delay):
		case <-s.cancel:
			s.fail(id, -32800, "cancelled")
			return
		}
	}

	switch *mode {
	case "hang":
		// Never answer on our own: only session/cancel (or a closed stdin)
		// ends the turn.
		s.chunk("still thinking…")
		<-s.cancel
		s.fail(id, -32800, "cancelled")
		return
	case "cancel":
		<-s.cancel
		s.fail(id, -32800, "cancelled")
		return
	case "garbage":
		s.raw("this is not json at all\n")
		s.raw("{\"jsonrpc\":\"2.0\",\"result\":")
		s.raw("\n")
		s.raw(strings.Repeat("x", 300000) + "\n")
		s.chunk("recovered")
		s.result(id, map[string]any{"stopReason": "end_turn"})
		return
	case "empty":
		// A turn with no agent message at all.
		s.result(id, map[string]any{"stopReason": "end_turn"})
		return
	case "serverreq":
		// Ask the client to touch the filesystem and run a terminal command.
		// Both must come back refused.
		fsResp, err := s.request("fs/read_text_file", map[string]any{"sessionId": *sessionID, "path": "/etc/passwd"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeserver: fs/read_text_file: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "fakeserver: fs answer: %s\n", string(mustMarshal(fsResp)))
		}
		termResp, err := s.request("terminal/create", map[string]any{"sessionId": *sessionID, "command": "rm -rf /"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeserver: terminal/create: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "fakeserver: terminal answer: %s\n", string(mustMarshal(termResp)))
		}
		s.chunk("the harness refused to run anything for me")
		s.result(id, map[string]any{"stopReason": "end_turn"})
		return
	case "toolcall":
		s.chunk("first I need to run something: " + firstLine(text))
		// An agent-side tool call is merely observed…
		s.notify("session/update", map[string]any{
			"sessionId":     *sessionID,
			"sessionUpdate": "tool_call",
			"toolCall": map[string]any{
				"id": "tc-own", "title": "agent ran its own sandboxed grep",
				"kind": "search", "status": "completed",
				"rawInput": map[string]any{"pattern": "TODO"},
			},
		})
		// …but a tool call the agent wants *us* to run is refused.
		answer, err := s.request("session/request_permission", map[string]any{
			"sessionId": *sessionID,
			"toolCall": map[string]any{
				"id":       "tc-1",
				"title":    "Run rm -rf /tmp/whatever",
				"kind":     *toolKind,
				"status":   "pending",
				"rawInput": map[string]any{"command": "rm -rf /tmp/whatever", "path": "/tmp/whatever"},
				"locations": []map[string]any{
					{"path": "/tmp/whatever", "line": 1},
				},
			},
			"options": []map[string]any{
				{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
				{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
			},
		})
		refused := "unknown"
		if err == nil {
			refused = string(mustMarshal(answer))
		} else {
			refused = err.Error()
		}
		fmt.Fprintf(os.Stderr, "fakeserver: permission answer: %s\n", refused)
		s.chunk("permission answer: " + refused)
		s.chunk(*final)
		s.result(id, map[string]any{"stopReason": "end_turn"})
		return
	default: // "ok"
		s.chunk("working on: " + firstLine(text))
		s.chunk(*final)
		s.result(id, map[string]any{"stopReason": "end_turn"})
	}
}

func (s *server) raw(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.WriteString(line)
	_ = s.out.Flush()
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
