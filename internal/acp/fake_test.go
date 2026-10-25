package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent is an in-process ACP server: it speaks the same line-delimited
// JSON-RPC as a real agent over one half of a net.Pipe, which keeps the client
// tests fast and hermetic (no processes, no filesystem, no network).
//
// Outgoing frames go through an outbox drained by its own goroutine: net.Pipe
// is synchronous, and a server that blocked writing while it waited for the
// client's answer would deadlock against a client that blocked writing an
// answer while the server was not reading.
//
// Every script knob is mutex-guarded because the tests set them from the test
// goroutine while the server goroutine reads them.
//
// The standalone counterpart used by the process and DAG tests lives in
// testdata/fakeserver.
type fakeAgent struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader

	outbox chan []byte

	mu     sync.Mutex
	seen   []string
	params map[string]json.RawMessage

	// knobs: prompt, sessionID, protocolVersion, delay, garbage, hook,
	// controlDelay.
	kmu             sync.Mutex
	promptFn        func(f *fakeAgent, text string) (any, *RPCError)
	sessionID       string
	protocolVersion int
	delay           time.Duration
	controlDelay    time.Duration
	garbage         []string
	hook            func(env envelope) bool

	amu      sync.Mutex
	nextID   int
	awaiting map[int]chan envelope

	cancel     chan struct{}
	cancelOnce sync.Once
}

func newFakeAgent(t *testing.T, opts Options) (*Client, *fakeAgent) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	f := &fakeAgent{
		t:               t,
		conn:            serverSide,
		r:               bufio.NewReader(serverSide),
		outbox:          make(chan []byte, 64),
		awaiting:        map[int]chan envelope{},
		cancel:          make(chan struct{}),
		sessionID:       "sess-fake",
		protocolVersion: ProtocolVersion,
	}
	f.promptFn = f.defaultPrompt
	go f.writer()
	go f.run()
	if opts.Name == "" {
		opts.Name = "fake"
	}
	c := NewClient(clientSide, opts)
	t.Cleanup(func() {
		c.Close()
		f.close()
	})
	return c, f
}

// ---------- knobs ----------

// onPrompt installs the turn script: it runs in its own goroutine and its
// return value answers session/prompt.
func (f *fakeAgent) onPrompt(fn func(f *fakeAgent, text string) (any, *RPCError)) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.promptFn = fn
}

func (f *fakeAgent) promptScript() func(*fakeAgent, string) (any, *RPCError) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	return f.promptFn
}

// setDelay makes a turn wait before answering, or end early on cancel.
func (f *fakeAgent) setDelay(d time.Duration) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.delay = d
}

func (f *fakeAgent) promptDelay() time.Duration {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	return f.delay
}

// setControlDelay stalls the handshake answers, for timeout tests.
func (f *fakeAgent) setControlDelay(d time.Duration) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.controlDelay = d
}

func (f *fakeAgent) controlStall() time.Duration {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	return f.controlDelay
}

func (f *fakeAgent) setProtocolVersion(v int) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.protocolVersion = v
}

func (f *fakeAgent) version() int {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	return f.protocolVersion
}

// setSessionID changes the session handed out; "" makes session/new answer
// with an empty result.
func (f *fakeAgent) setSessionID(id string) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.sessionID = id
}

func (f *fakeAgent) session() string {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	return f.sessionID
}

// queueGarbage makes the next frames be replaced by verbatim junk, to exercise
// the client's reader.
func (f *fakeAgent) queueGarbage(lines ...string) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.garbage = append(f.garbage, lines...)
}

func (f *fakeAgent) takeGarbage() []string {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	g := f.garbage
	f.garbage = nil
	return g
}

// setHook intercepts client frames; returning true swallows the frame.
func (f *fakeAgent) setHook(fn func(env envelope) bool) {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	f.hook = fn
}

func (f *fakeAgent) hookFn() func(env envelope) bool {
	f.kmu.Lock()
	defer f.kmu.Unlock()
	return f.hook
}

// ---------- observation ----------

func (f *fakeAgent) close() {
	f.cancelOnce.Do(func() { close(f.cancel) })
	_ = f.conn.Close()
}

// cancelled is closed once session/cancel arrives.
func (f *fakeAgent) cancelled() <-chan struct{} { return f.cancel }

// methodsSeen lists every method the client sent, in order.
func (f *fakeAgent) methodsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeAgent) record(method string, params json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, method)
	if f.params == nil {
		f.params = map[string]json.RawMessage{}
	}
	f.params[method] = params
}

// lastParams returns the params of the most recent call to method.
func (f *fakeAgent) lastParams(method string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.params[method]
}

// ---------- wire ----------

func (f *fakeAgent) writer() {
	for b := range f.outbox {
		if _, err := f.conn.Write(b); err != nil {
			return
		}
	}
}

// emit queues one frame (or the queued garbage, for reader-tolerance tests).
func (f *fakeAgent) emit(v any) {
	for _, g := range f.takeGarbage() {
		f.queue([]byte(g))
	}
	if v == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fakeAgent: encode: %v", err)
		return
	}
	f.queue(append(b, '\n'))
}

func (f *fakeAgent) queue(b []byte) {
	// Prefer the outbox when it has room: after a cancel the final response
	// (CodeCancelled) must still reach the client — dropping it on the closed
	// cancel channel would leave the caller waiting for a frame that was
	// already produced.
	select {
	case f.outbox <- b:
		return
	default:
	}
	select {
	case f.outbox <- b:
	case <-f.cancel:
	case <-time.After(5 * time.Second):
		f.t.Errorf("fakeAgent: outbox stalled, connection not being read")
	}
}

func (f *fakeAgent) respondResult(id json.RawMessage, result any) {
	f.emit(envelope{JSONRPC: "2.0", ID: id, Result: mustMarshal(result)})
}

func (f *fakeAgent) respondError(id json.RawMessage, code int, msg string) {
	f.emit(envelope{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

func (f *fakeAgent) notify(method string, params any) {
	f.emit(envelope{JSONRPC: "2.0", Method: method, Params: mustMarshal(params)})
}

// request sends a server-initiated request and waits for the client's answer.
func (f *fakeAgent) request(method string, params any) (envelope, error) {
	f.amu.Lock()
	f.nextID++
	id := f.nextID
	ch := make(chan envelope, 1)
	f.awaiting[id] = ch
	f.amu.Unlock()
	defer func() {
		f.amu.Lock()
		delete(f.awaiting, id)
		f.amu.Unlock()
	}()

	f.emit(envelope{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: method, Params: mustMarshal(params)})
	select {
	case env := <-ch:
		return env, nil
	case <-f.cancel:
		return envelope{}, io.EOF
	case <-time.After(10 * time.Second):
		return envelope{}, fmt.Errorf("fakeAgent: no answer to %s", method)
	}
}

// agentMessageChunk emits one piece of agent output.
func (f *fakeAgent) agentMessageChunk(session, text string) {
	f.notify("session/update", map[string]any{
		"sessionId":     session,
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

// askPermission sends a session/request_permission for a tool call the agent
// wants the client to perform, and returns the client's answer.
func (f *fakeAgent) askPermission(call map[string]any) (envelope, error) {
	return f.request("session/request_permission", map[string]any{
		"sessionId": f.session(),
		"toolCall":  call,
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
		},
	})
}

func (f *fakeAgent) defaultPrompt(_ *fakeAgent, text string) (any, *RPCError) {
	f.agentMessageChunk(f.session(), "worked on: "+text)
	return map[string]any{"stopReason": "end_turn"}, nil
}

// ---------- loop ----------

func (f *fakeAgent) run() {
	for {
		line, err := readFakeLine(f.r)
		if len(line) > 0 {
			if !f.handle(line) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func readFakeLine(r *bufio.Reader) ([]byte, error) {
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

// handle dispatches one frame. It returns false when the connection is done.
func (f *fakeAgent) handle(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return true
	}
	var env envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		f.t.Errorf("fakeAgent: client sent non-JSON: %q", trimmed)
		return true
	}
	if env.Method == "" {
		// A response to one of our requests.
		id, ok := decodeID(env.ID)
		if !ok {
			return true
		}
		f.amu.Lock()
		ch := f.awaiting[id]
		f.amu.Unlock()
		if ch != nil {
			ch <- env
		}
		return true
	}

	f.record(env.Method, env.Params)
	if hook := f.hookFn(); hook != nil && hook(env) {
		return true
	}
	switch env.Method {
	case "initialize":
		if d := f.controlStall(); d > 0 {
			time.Sleep(d)
		}
		f.respondResult(env.ID, map[string]any{
			"protocolVersion":   f.version(),
			"agentInfo":         map[string]any{"name": "fake-agent", "version": "0"},
			"agentCapabilities": map[string]any{"promptCapabilities": map[string]any{"image": false}},
			"authMethods":       []any{},
		})
	case "session/new":
		if f.session() == "" {
			f.respondResult(env.ID, map[string]any{})
			return true
		}
		f.respondResult(env.ID, map[string]any{"sessionId": f.session()})
	case "session/prompt":
		go f.runPrompt(env.ID, env.Params)
	case "session/cancel":
		f.cancelOnce.Do(func() { close(f.cancel) })
	default:
		f.respondError(env.ID, CodeMethodNotFound, "fake agent does not implement "+env.Method)
	}
	return true
}

func (f *fakeAgent) runPrompt(id json.RawMessage, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	var text string
	for _, b := range p.Prompt {
		if b.Type == "text" {
			text += b.Text
		}
	}
	session := f.session()
	if session != "" && p.SessionID != session {
		f.respondError(id, CodeMethodNotFound, "unknown session "+p.SessionID)
		return
	}
	if d := f.promptDelay(); d > 0 {
		select {
		case <-time.After(d):
		case <-f.cancel:
			f.respondError(id, CodeCancelled, "cancelled")
			return
		}
	}
	result, rpcErr := f.promptScript()(f, text)
	if rpcErr != nil {
		f.respondError(id, rpcErr.Code, rpcErr.Message)
		return
	}
	if result != nil {
		f.respondResult(id, result)
	}
}

// ---------- test helpers ----------

// recordingLogger captures the client's diagnostics.
type recordingLogger struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingLogger) Log(kind string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, kind)
}

func (r *recordingLogger) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recordingLogger) hasKind(kind string) bool {
	return contains(r.kinds(), kind)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// contains reports whether want is among the values.
func contains(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}
