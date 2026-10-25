package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------- handshake ----------

func TestHandshake(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	ctx := context.Background()

	session, err := c.Handshake(ctx, "/tmp/ws")
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if session != "sess-fake" {
		t.Fatalf("session = %q, want sess-fake", session)
	}
	if got := c.SessionID(); got != session {
		t.Fatalf("SessionID = %q, want %q", got, session)
	}

	seen := f.methodsSeen()
	if !contains(seen, "initialize") || !contains(seen, "session/new") {
		t.Fatalf("methods seen = %v", seen)
	}
	// The handshake must advertise the read-only posture, not a filesystem or
	// terminal the harness does not have.
	init := string(f.lastParams("initialize"))
	if !strings.Contains(init, `"protocolVersion":1`) {
		t.Errorf("initialize params missing protocolVersion: %s", init)
	}
	if !strings.Contains(init, `"readTextFile":false`) || !strings.Contains(init, `"terminal":false`) {
		t.Errorf("initialize advertised client services we refuse to provide: %s", init)
	}
	newSess := string(f.lastParams("session/new"))
	if !strings.Contains(newSess, `"cwd":"/tmp/ws"`) {
		t.Errorf("session/new missing cwd: %s", newSess)
	}
	if len(c.AgentCapabilities()) == 0 {
		t.Error("agent capabilities not recorded")
	}
}

func TestHandshakeProtocolVersionMismatchIsLoggedNotFatal(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.setProtocolVersion(99)
	rec := &recordingLogger{}
	c.log = rec

	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !rec.hasKind("acp_protocol_version") {
		t.Fatalf("version mismatch not logged: %v", rec.kinds())
	}
	if _, err := c.NewSession(context.Background(), ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	c, f := newFakeAgent(t, Options{Timeout: 120 * time.Millisecond})
	f.setControlDelay(2 * time.Second)

	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("initialize against a stalled agent succeeded")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	c.Close()
}

func TestSessionNewWithoutID(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.setSessionID("")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.NewSession(context.Background(), ""); err == nil {
		t.Fatal("session/new without a sessionId accepted")
	}
}

func TestPromptWithoutSession(t *testing.T) {
	c, _ := newFakeAgent(t, Options{})
	if _, err := c.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("prompt without a session accepted")
	}
}

// ---------- prompt → result ----------

func TestPromptReturnsAgentMessage(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.onPrompt(func(f *fakeAgent, text string) (any, *RPCError) {
		f.agentMessageChunk(f.sessionID, "part one. ")
		f.notify("session/update", map[string]any{
			"sessionId":     f.sessionID,
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": "thinking about it"},
		})
		f.agentMessageChunk(f.sessionID, "part two.")
		return map[string]any{"stopReason": "end_turn"}, nil
	})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	res, err := c.Prompt(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stopReason = %q", res.StopReason)
	}
	if res.Text != "part one. part two." {
		t.Errorf("text = %q", res.Text)
	}
	// The prompt text must never be echoed back as the result.
	if strings.Contains(res.Text, "do the thing") {
		t.Errorf("result echoed the prompt: %q", res.Text)
	}
	if len(res.Final) != 2 {
		t.Errorf("final blocks = %d, want 2", len(res.Final))
	}
	// Thought chunks are observed but are not the agent's answer.
	if strings.Contains(res.Text, "thinking about it") {
		t.Errorf("thought chunk leaked into the result: %q", res.Text)
	}
	var sawThought bool
	for _, u := range res.Updates {
		if u.SessionUpdate == "agent_thought_chunk" {
			sawThought = true
		}
	}
	if !sawThought {
		t.Errorf("thought chunk not observed: %+v", res.Updates)
	}

	// The prompt frame carries the task text as an ACP content block.
	if p := string(f.lastParams("session/prompt")); !strings.Contains(p, `"do the thing"`) || !strings.Contains(p, `"sessionId":"sess-fake"`) {
		t.Errorf("prompt params = %s", p)
	}
}

func TestPromptEmptyAgentMessageIsAnError(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.onPrompt(func(*fakeAgent, string) (any, *RPCError) {
		return map[string]any{"stopReason": "end_turn"}, nil
	})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if _, err := c.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("a turn with no agent message reported success")
	}
}

func TestPromptStopReasonCancelledIsErrCancelled(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.onPrompt(func(*fakeAgent, string) (any, *RPCError) {
		return map[string]any{"stopReason": "cancelled"}, nil
	})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if _, err := c.Prompt(context.Background(), "hi"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
}

func TestPromptRPCError(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.onPrompt(func(*fakeAgent, string) (any, *RPCError) {
		return nil, &RPCError{Code: -32603, Message: "agent exploded"}
	})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	_, err := c.Prompt(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "agent exploded") {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, ErrCancelled) {
		t.Fatalf("a plain rpc failure reported as cancelled: %v", err)
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32603 {
		t.Fatalf("rpc error not surfaced: %v", err)
	}
}

// ---------- cancellation ----------

func TestCancelMidPrompt(t *testing.T) {
	c, f := newFakeAgent(t, Options{PromptTimeout: 10 * time.Second})
	f.setDelay(5 * time.Second) // the turn only ends on cancel
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res Result
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		res, err := c.Prompt(ctx, "long task")
		out <- outcome{res, err}
	}()

	waitFor(t, "prompt to reach the agent", func() bool {
		return contains(f.methodsSeen(), "session/prompt")
	})
	cancel()

	select {
	case o := <-out:
		if !errors.Is(o.err, ErrCancelled) {
			t.Fatalf("err = %v, want ErrCancelled", o.err)
		}
		if !errors.Is(o.err, context.Canceled) {
			t.Fatalf("err = %v, want it to wrap context.Canceled", o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not return after the context was cancelled")
	}

	// The agent must have been told to stop, and must have stopped.
	waitFor(t, "session/cancel", func() bool {
		return contains(f.methodsSeen(), "session/cancel")
	})
	select {
	case <-f.cancelled():
	case <-time.After(2 * time.Second):
		t.Fatal("fake agent never saw the cancel")
	}
	c.Close()
}

func TestCancelAndWaitSendsSessionCancel(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run is already over; the cancel frame still has to go out

	c.CancelAndWait(ctx, 50*time.Millisecond)

	if !contains(f.methodsSeen(), "session/cancel") {
		t.Fatalf("session/cancel never sent: %v", f.methodsSeen())
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("client not finished after CancelAndWait")
	}
}

func TestServerReportsCancelledTurn(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.setDelay(3 * time.Second)
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	errs := make(chan error, 1)
	go func() {
		_, err := c.Prompt(context.Background(), "hi")
		errs <- err
	}()
	waitFor(t, "prompt to reach the agent", func() bool {
		return contains(f.methodsSeen(), "session/prompt")
	})
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("err = %v, want ErrCancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return after session/cancel")
	}
}

// ---------- read-only observer ----------

func TestToolCallRequestIsRefusedReadOnly(t *testing.T) {
	obs := &ReadOnlyObserver{}
	c, f := newFakeAgent(t, Options{Observer: obs})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	f.onPrompt(func(f *fakeAgent, _ string) (any, *RPCError) {
		resp, err := f.askPermission(map[string]any{
			"id":       "tc-1",
			"title":    "Run rm -rf /tmp/ws",
			"kind":     "execute",
			"status":   "pending",
			"rawInput": map[string]any{"command": "rm -rf /tmp/ws"},
		})
		if err != nil {
			t.Errorf("askPermission: %v", err)
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}
		body, _ := json.Marshal(resp.Result)
		f.agentMessageChunk(f.sessionID, "answer: "+string(body))
		return map[string]any{"stopReason": "end_turn"}, nil
	})

	res, err := c.Prompt(context.Background(), "clean up the workspace")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	calls := obs.Calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(calls))
	}
	call := calls[0]
	if !call.Refused {
		t.Error("tool call not marked refused")
	}
	if call.Kind != KindExecute {
		t.Errorf("kind = %q, want execute", call.Kind)
	}
	if call.Target() != "rm -rf /tmp/ws" {
		t.Errorf("target = %q", call.Target())
	}
	if !strings.Contains(call.Reason, "executes nothing") {
		t.Errorf("reason = %q", call.Reason)
	}
	if call.PermissionID != "reject" {
		t.Errorf("permission option = %q, want the reject option", call.PermissionID)
	}

	// What the agent was told: a refusal, on both spellings, with no output of
	// any kind — nothing ran, so there is nothing to hand back.
	if !strings.Contains(res.Text, `"outcome":"refused"`) {
		t.Errorf("refusal not delivered to the agent: %q", res.Text)
	}
	if !strings.Contains(res.Text, `"permissionOutcome"`) {
		t.Errorf("permissionOutcome spelling missing: %q", res.Text)
	}
	if strings.Contains(res.Text, "allow_once") {
		t.Errorf("agent was offered an allow option: %q", res.Text)
	}
	// The refusal is part of the observed turn too.
	var sawRefusal bool
	for _, u := range res.Updates {
		if u.SessionUpdate == "tool_call_refused" {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Errorf("refusal not in the turn record: %+v", res.Updates)
	}
}

func TestServerInitiatedFSAndTerminalRequestsAreRefused(t *testing.T) {
	obs := &ReadOnlyObserver{}
	c, f := newFakeAgent(t, Options{Observer: obs})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	f.onPrompt(func(f *fakeAgent, _ string) (any, *RPCError) {
		read, err := f.request("fs/read_text_file", map[string]any{"sessionId": f.sessionID, "path": "/etc/passwd"})
		if err != nil {
			t.Errorf("fs/read_text_file: %v", err)
		} else if read.Error == nil || read.Error.Code != CodeMethodNotFound {
			t.Errorf("fs/read_text_file was not refused: %+v", read)
		}
		write, err := f.request("fs/write_text_file", map[string]any{"sessionId": f.sessionID, "path": "/etc/passwd", "content": "x"})
		if err != nil {
			t.Errorf("fs/write_text_file: %v", err)
		} else if write.Error == nil {
			t.Errorf("fs/write_text_file was not refused: %+v", write)
		}
		term, err := f.request("terminal/create", map[string]any{"sessionId": f.sessionID, "command": "rm -rf /"})
		if err != nil {
			t.Errorf("terminal/create: %v", err)
		} else if term.Error == nil {
			t.Errorf("terminal/create was not refused: %+v", term)
		}
		unknown, err := f.request("some/future_method", map[string]any{})
		if err != nil {
			t.Errorf("unknown request: %v", err)
		} else if unknown.Error == nil || unknown.Error.Code != CodeMethodNotFound {
			t.Errorf("unknown request was not answered with method-not-found: %+v", unknown)
		}
		f.agentMessageChunk(f.sessionID, "nothing was executed for me")
		return map[string]any{"stopReason": "end_turn"}, nil
	})

	if _, err := c.Prompt(context.Background(), "escalate"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	kinds := map[string]int{}
	for _, call := range obs.Calls() {
		if !call.Refused {
			t.Errorf("recorded call not refused: %+v", call)
		}
		kinds[call.Kind]++
	}
	if kinds[KindRead] == 0 || kinds[KindEdit] == 0 || kinds[KindExecute] == 0 {
		t.Errorf("not every refused request was recorded: %+v", obs.Calls())
	}
}

func TestObserverFuncReceivesRefusals(t *testing.T) {
	var seen []ToolCall
	c, f := newFakeAgent(t, Options{Observer: ObserverFunc(func(call ToolCall) {
		seen = append(seen, call)
	})})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	f.onPrompt(func(f *fakeAgent, _ string) (any, *RPCError) {
		if _, err := f.askPermission(map[string]any{"id": "x", "kind": "read", "title": "read /etc/shadow"}); err != nil {
			t.Errorf("askPermission: %v", err)
		}
		f.agentMessageChunk(f.sessionID, "ok")
		return map[string]any{"stopReason": "end_turn"}, nil
	})
	if _, err := c.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(seen) != 1 || seen[0].Kind != KindRead {
		t.Fatalf("observer saw %+v", seen)
	}
	if seen[0].Reason == "" {
		t.Error("observer path produced no refusal reason")
	}
}

func TestAgentSideToolCallIsObservedNotExecuted(t *testing.T) {
	obs := &ReadOnlyObserver{}
	c, f := newFakeAgent(t, Options{Observer: obs})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	f.onPrompt(func(f *fakeAgent, _ string) (any, *RPCError) {
		// The agent reports a tool *it* ran in its own sandbox. That is an
		// observation, not a request: it must not be recorded as a refusal.
		f.notify("session/update", map[string]any{
			"sessionId":     f.sessionID,
			"sessionUpdate": "tool_call",
			"toolCall": map[string]any{
				"id": "own", "title": "grep -r TODO", "kind": "search", "status": "completed",
			},
		})
		f.agentMessageChunk(f.sessionID, "done")
		return map[string]any{"stopReason": "end_turn"}, nil
	})
	res, err := c.Prompt(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(obs.Calls()) != 0 {
		t.Errorf("an agent-side tool call was treated as a request: %+v", obs.Calls())
	}
	var observed bool
	for _, u := range res.Updates {
		if u.SessionUpdate == "tool_call" && strings.Contains(u.ToolCall, "grep -r TODO") {
			observed = true
		}
	}
	if !observed {
		t.Errorf("agent-side tool call not observed: %+v", res.Updates)
	}
}

// ---------- reader robustness ----------

func TestMalformedLinesDoNotWedgeTheReader(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.queueGarbage(
		"this is not json\n",
		"\n",
		`{"jsonrpc":"2.0","result":`+"\n", // truncated object
		strings.Repeat("x", 3<<20)+"\n",   // over-long junk line
	)
	f.onPrompt(func(f *fakeAgent, _ string) (any, *RPCError) {
		f.agentMessageChunk(f.sessionID, "still here")
		return map[string]any{"stopReason": "end_turn"}, nil
	})

	rec := &recordingLogger{}
	c.log = rec
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Prompt(context.Background(), "hi")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reader wedged on malformed input")
	}
	if !rec.hasKind("acp_malformed") {
		t.Errorf("malformed frames were not reported: %v", rec.kinds())
	}
	select {
	case <-c.Done():
		t.Fatal("client died after malformed frames")
	default:
	}
}

func TestAgentClosingTheConnectionFailsPendingPrompt(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.onPrompt(func(*fakeAgent, string) (any, *RPCError) {
		// The agent drops the connection mid-turn instead of answering.
		f.close()
		return nil, nil
	})
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Prompt(context.Background(), "hi")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a vanished agent reported success")
		}
		if errors.Is(err, ErrCancelled) {
			t.Fatalf("a vanished agent reported cancellation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt hung after the agent closed the connection")
	}
}

func TestCloseIsIdempotentAndUnblocksRequests(t *testing.T) {
	c, f := newFakeAgent(t, Options{})
	f.setDelay(30 * time.Second)
	if _, err := c.Handshake(context.Background(), ""); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Prompt(context.Background(), "hi")
		done <- err
	}()
	waitFor(t, "prompt to reach the agent", func() bool {
		return contains(f.methodsSeen(), "session/prompt")
	})
	c.Close()
	c.Close() // must be safe twice
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("prompt succeeded after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock the in-flight prompt")
	}
	if err := c.notify(context.Background(), "x", nil); !errors.Is(err, errClosed) {
		t.Fatalf("notify after Close = %v, want errClosed", err)
	}
}

// blockedTransport stops reading after the first frame, so writes fill the
// pipe: the client must time out rather than deadlock.
type blockedTransport struct {
	mu       chan struct{}
	closed   chan struct{}
	closeErr error
}

func (b *blockedTransport) Read(p []byte) (int, error) {
	<-b.mu // never signalled: the agent stopped reading
	return 0, io.EOF
}

func (b *blockedTransport) Write(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, errors.New("transport closed")
	case <-time.After(30 * time.Second):
		return 0, errors.New("write stalled")
	}
}

func (b *blockedTransport) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return b.closeErr
}

func TestWriteTimeoutWhenAgentStopsReading(t *testing.T) {
	tr := &blockedTransport{mu: make(chan struct{}), closed: make(chan struct{})}
	c := NewClient(tr, Options{WriteTimeout: 150 * time.Millisecond, Timeout: 5 * time.Second, Name: "blocked"})
	t.Cleanup(c.Close)

	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("initialize succeeded against an agent that never reads")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a write timeout", err)
	}
	c.Close()
}

// pipeTransport is the in-process stand-in for a child's stdio.
type pipeTransport struct {
	net.Conn
}

func TestTransportInterfaceIsSatisfiedByAConn(t *testing.T) {
	// net.Conn (what net.Pipe and a child's stdio both give us) is an
	// io.ReadWriteCloser; pin the documented contract down.
	var _ Transport = pipeTransport{}
	var tr Transport = pipeTransport{nil}
	if tr == nil {
		t.Fatal("a pipeTransport value must not compare nil as a Transport")
	}
}

func TestCancelBeforeHandshake(t *testing.T) {
	c, _ := newFakeAgent(t, Options{})
	if err := c.Cancel(context.Background()); err == nil {
		t.Fatal("cancel without a session accepted")
	}
}
