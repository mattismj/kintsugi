// Package acp is the Agent Client Protocol client: it lets an external agent
// (Claude Code, Codex, Gemini CLI, or any ACP server) be used as a DAG worker
// instead of re-implementing its harness. The wire is JSON-RPC 2.0 over stdio,
// one JSON object per line, no framing headers and no length prefixes.
//
// The client owns the child's whole lifecycle: start, handshake
// (`initialize`, `session/new`), prompt (`session/prompt`), collect the agent's
// final message, cancel (`session/cancel`), and kill the process if it will
// not stop being a child.
//
// SECURITY POSTURE — an external agent is a worker, not a deputy.
//
// An ACP server may ask the *client* to do things: execute a command, read or
// write a file, show a permission dialog. This harness executes NOTHING on the
// agent's behalf. Every incoming tool call is mapped to a READ-ONLY observer:
//
//   - the request is recorded verbatim (kind, title, target, arguments) so the
//     audit trail shows what the external agent wanted and what it was told;
//   - the answer is a refusal/observation payload carrying the reason and the
//     recorded arguments — never a command's output, never a file's contents,
//     never a permission decision;
//   - an explicit `outcome` field is included alongside `permissionOutcome` so
//     a server that only reads one of the two still learns the call did not run.
//
// Nothing here is configurable and nothing here is a hook: the read-only
// posture is the contract, so an operator cannot accidentally turn a DAG node
// into a shell for an agent they do not control. Agent-side events
// (`session/update`) are likewise observed, truncated, and kept in memory —
// they are never executed, never written to disk, and never fed back into a
// tool executor.
//
// The client is written against an io.ReadWriteCloser transport, so it is
// testable in-process without spawning anything. [Start] builds the stdio
// transport from a command line; [Run] does start → prompt → result → close in
// one call.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol constants. ProtocolVersion is what we advertise; a server that
// answers with another version is recorded and still used, because ACP servers
// in the wild disagree about the number they echo back.
const (
	ProtocolVersion = 1
	ClientName      = "kintsugi"
	ClientVersion   = "0.1"

	// DefaultTimeout bounds a single request (initialize, session/new) and,
	// with DefaultPromptTimeout, a whole prompt turn.
	DefaultTimeout = 60 * time.Second
	// DefaultPromptTimeout bounds one prompt turn. An external agent may run
	// for a long time; the DAG node's context is still the harder bound.
	DefaultPromptTimeout = 30 * time.Minute
	// CancelTimeout bounds the session/cancel notification round before the
	// process is killed.
	CancelTimeout = 5 * time.Second

	// maxLineBytes caps one framed message. A longer line is reported as a
	// framing error rather than grown into an unbounded allocation, and the
	// reader skips the rest of it and keeps reading: a chatty server cannot
	// wedge the connection.
	maxLineBytes = 4 << 20

	// maxEventText caps a single agent message/update chunk we keep in memory.
	maxEventText = 1 << 12
	// maxUpdates caps how many session/update notifications are retained per
	// prompt turn; older ones are dropped, the last ones are what a reviewer
	// cares about.
	maxUpdates = 256
	// maxStderrTail is how many stderr lines are retained for diagnostics.
	maxStderrTail = 50
	// stderrLineChars truncates one retained stderr line.
	stderrLineChars = 400
)

// JSON-RPC error codes we care about. CodeCancelled is what a server returns
// for a prompt turn that was cancelled.
const (
	CodeParseError     = -32700
	CodeMethodNotFound = -32601
	CodeCancelled      = -32800
)

// ToolCallKind classifies what an agent asked the client to do. ACP servers use
// a small vocabulary here; anything unrecognised is reported as "unknown",
// which is refused like everything else.
const (
	KindRead         = "read"
	KindEdit         = "edit"
	KindDelete       = "delete"
	KindMove         = "move"
	KindSearch       = "search"
	KindExecute      = "execute"
	KindThink        = "think"
	KindFetch        = "fetch"
	KindOther        = "other"
	KindSwitchMode   = "switch_mode"
	KindUnknown      = "unknown"
	KindPermissionUI = "permission_ui"
)

// ErrCancelled reports a prompt turn that ended because the run was cancelled
// (context done or an explicit Cancel). It is returned wrapped, so tests and
// callers should use errors.Is.
var ErrCancelled = errors.New("acp: cancelled")

// RefusalReason is returned by Observer.Refuse. Returning "" accepts the
// built-in text.
type Observer interface {
	// Observe is called for every tool call the agent asked the client to
	// perform. It runs while the turn is live and must not block.
	Observe(call ToolCall)
	// Refuse decides what the agent is told. It must not perform the call.
	Refuse(call ToolCall) string
}

// ToolCall is one recorded, refused request from the agent. It is the audit
// record: what an external agent wanted, and what it was told.
type ToolCall struct {
	ID           string          `json:"id,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	Kind         string          `json:"kind"`
	Title        string          `json:"title,omitempty"`
	Status       string          `json:"status,omitempty"`
	Locations    []ToolLocation  `json:"locations,omitempty"`
	RawInput     json.RawMessage `json:"raw_input,omitempty"`
	RawOutput    json.RawMessage `json:"raw_output,omitempty"`
	Content      []ContentBlock  `json:"content,omitempty"`
	Refused      bool            `json:"refused"`
	Reason       string          `json:"reason,omitempty"`
	ReceivedAt   time.Time       `json:"received_at"`
	ParamsRaw    json.RawMessage `json:"params,omitempty"`
	PermissionID string          `json:"permission_id,omitempty"`
}

// Target is the best-effort subject of a tool call: a path, a command line, a
// URL. It is recorded, never executed.
func (c ToolCall) Target() string {
	var m map[string]any
	if len(c.RawInput) > 0 {
		if err := json.Unmarshal(c.RawInput, &m); err == nil {
			for _, key := range []string{"path", "file_path", "filePath", "uri", "url", "location", "command", "cmd", "query", "pattern"} {
				if s := jsonValueString(m[key]); s != "" {
					return s
				}
			}
			// A command may arrive as an argv array.
			if argv, ok := m["command"].([]any); ok {
				parts := make([]string, 0, len(argv))
				for _, v := range argv {
					parts = append(parts, jsonValueString(v))
				}
				if s := strings.TrimSpace(strings.Join(parts, " ")); s != "" {
					return s
				}
			}
		}
	}
	for _, l := range c.Locations {
		if l.Path != "" {
			return l.Path
		}
	}
	return c.Title
}

// Summary is a one-line audit rendering of the recorded request.
func (c ToolCall) Summary() string {
	kind := c.Kind
	if kind == "" {
		kind = KindUnknown
	}
	target := c.Target()
	if target == "" {
		target = c.Title
	}
	if target == "" {
		return kind
	}
	return kind + ": " + target
}

// ToolLocation is where a tool call points, if the agent said.
type ToolLocation struct {
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
}

// ContentBlock is the agent-message content we keep from a turn.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ObserverFunc adapts a plain function to Observer, using the default refusal.
type ObserverFunc func(ToolCall)

func (f ObserverFunc) Observe(c ToolCall)     { f(c) }
func (f ObserverFunc) Refuse(ToolCall) string { return "" }

// RefusalText is what an agent is told when it asks the client to do something.
// It is deliberately explicit: the agent should understand that it is a worker
// inside someone else's harness, not a deputy with someone else's hands.
const RefusalText = "refused: the AgentForge harness executes nothing on an external agent's behalf. " +
	"Tool calls from an ACP agent are observed read-only: the request was recorded for the audit trail and no " +
	"command ran, no file was read or written, and no permission was granted. Do the work yourself inside your own " +
	"sandbox and report the result in your final message."

// ReadOnlyObserver records every tool call and refuses it with RefusalText. It
// is the default and the recommended posture.
type ReadOnlyObserver struct {
	mu    sync.Mutex
	calls []ToolCall
}

func (o *ReadOnlyObserver) Observe(c ToolCall) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.calls) >= maxUpdates*4 {
		// Keep the tail: the last requests are the interesting ones.
		o.calls = append(o.calls[len(o.calls)/2:], c)
		return
	}
	o.calls = append(o.calls, c)
}

func (o *ReadOnlyObserver) Refuse(ToolCall) string { return RefusalText }

// Calls returns a copy of the recorded tool calls.
func (o *ReadOnlyObserver) Calls() []ToolCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ToolCall(nil), o.calls...)
}

// Update is one session/update notification: agent output observed, not acted
// on.
type Update struct {
	SessionUpdate string `json:"session_update"`
	Text          string `json:"text,omitempty"`
	ToolCall      string `json:"tool_call,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	ReceivedAt    time.Time
}

// Transport is the wire the client speaks over. Anything that carries
// line-delimited JSON both ways works: a child process's stdio (see Start) or
// an in-process pipe (see tests).
type Transport interface {
	io.ReadWriteCloser
}

// Logger receives diagnostics: agent stderr lines, malformed frames, the
// handshake result. The shape matches internal/session and internal/dag.
type Logger interface {
	Log(kind string, v any)
}

type loggerFunc func(kind string, v any)

func (f loggerFunc) Log(kind string, v any) { f(kind, v) }

type nopLogger struct{}

func (nopLogger) Log(string, any) {}

// ---------- protocol envelopes ----------

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "acp: unknown rpc error"
	}
	if e.Message == "" {
		return fmt.Sprintf("acp: rpc error %d", e.Code)
	}
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// IsCancelled reports whether the server said the turn was cancelled.
func (e *RPCError) IsCancelled() bool { return e != nil && e.Code == CodeCancelled }

type promptResult struct {
	StopReason string          `json:"stopReason"`
	Raw        json.RawMessage `json:"-"`
}

// ---------- client ----------

// Client speaks ACP to one agent over one transport. It is safe for concurrent
// use: requests are serialised by id, notifications are handled by the read
// loop.
type Client struct {
	tr     Transport
	reader *bufio.Reader

	observer Observer
	log      Logger

	writeTimeout  time.Duration
	timeout       time.Duration
	promptTimeout time.Duration
	name          string

	// outbox carries every outgoing frame to the single writer goroutine. It
	// is buffered so a caller answering an agent's request never blocks the
	// read loop on a full stdin pipe.
	outbox chan writeJob

	idmu   sync.Mutex // guards nextID
	nextID int

	pmu     sync.Mutex
	pending map[int]chan response

	smu      sync.Mutex
	session  string
	updates  []Update
	final    []ContentBlock
	agentCap json.RawMessage

	emu        sync.Mutex // guards err and stderrTail
	stderrTail []string
	err        error

	done     chan struct{}
	doneOnce sync.Once

	closeOnce sync.Once
	closed    chan struct{}

	// killing is set by Close and tells the writer and the reader that a
	// broken pipe is expected rather than a fault.
	killing atomic.Bool
}

type response struct {
	result json.RawMessage
	err    *RPCError
	// fatal is set when the connection died before the answer arrived.
	fatal error
}

// Options configure a Client. Zero values mean the package defaults.
type Options struct {
	// Timeout bounds one control request (initialize, session/new,
	// session/cancel). Default DefaultTimeout.
	Timeout time.Duration
	// PromptTimeout bounds one prompt turn. Default DefaultPromptTimeout.
	PromptTimeout time.Duration
	// WriteTimeout bounds a single write to the transport. Default 10s.
	WriteTimeout time.Duration
	// Observer records and refuses the agent's tool calls. Default
	// &ReadOnlyObserver{}.
	Observer Observer
	// Log receives diagnostics. Default: discarded.
	Log Logger
	// Name labels the agent in logs and errors.
	Name string
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.PromptTimeout <= 0 {
		o.PromptTimeout = DefaultPromptTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.Observer == nil {
		o.Observer = &ReadOnlyObserver{}
	}
	if o.Log == nil {
		o.Log = nopLogger{}
	}
	if o.Name == "" {
		o.Name = "agent"
	}
	return o
}

// NewClient wraps an existing transport. The client takes ownership of it:
// Close closes the transport.
func NewClient(tr Transport, opts Options) *Client {
	opts = opts.withDefaults()
	c := &Client{
		tr:            tr,
		reader:        bufio.NewReaderSize(tr, 1<<20),
		observer:      opts.Observer,
		log:           opts.Log,
		writeTimeout:  opts.WriteTimeout,
		timeout:       opts.Timeout,
		promptTimeout: opts.PromptTimeout,
		name:          opts.Name,
		pending:       map[int]chan response{},
		outbox:        make(chan writeJob, 16),
		done:          make(chan struct{}),
		closed:        make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

func (c *Client) logf(kind string, v any) {
	if c.log != nil {
		c.log.Log(kind, v)
	}
}

// SessionID is the session the handshake opened, "" before NewSession.
func (c *Client) SessionID() string {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.session
}

// AgentCapabilities is the raw `agentCapabilities` object from initialize.
func (c *Client) AgentCapabilities() json.RawMessage {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.agentCap
}

// Done is closed when the read loop stops (transport EOF/error or Close).
func (c *Client) Done() <-chan struct{} { return c.done }

// Observer returns the observer recording this agent's tool calls.
func (c *Client) Observer() Observer { return c.observer }

// StderrTail returns the last stderr lines collected from the child process,
// for diagnostics. Always empty for an in-process transport.
func (c *Client) StderrTail() []string {
	c.emu.Lock()
	defer c.emu.Unlock()
	return append([]string(nil), c.stderrTail...)
}

func (c *Client) appendStderr(line string) {
	c.emu.Lock()
	defer c.emu.Unlock()
	c.stderrTail = append(c.stderrTail, line)
	if len(c.stderrTail) > maxStderrTail {
		c.stderrTail = c.stderrTail[len(c.stderrTail)-maxStderrTail:]
	}
}

// ---------- handshake ----------

// Initialize performs the ACP handshake. It never hard-fails on an unexpected
// protocolVersion: a server that disagrees is logged and still used, because
// refusing to talk to a working agent over an echoed integer is a worse
// failure mode than proceeding.
func (c *Client) Initialize(ctx context.Context) error {
	raw, err := c.request(ctx, c.timeout, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			// We can receive and record agent output; we execute nothing.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": ClientName, "version": ClientVersion},
	})
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	var res struct {
		ProtocolVersion   int             `json:"protocolVersion"`
		AgentInfo         json.RawMessage `json:"agentInfo"`
		AgentCapabilities json.RawMessage `json:"agentCapabilities"`
		AuthMethods       json.RawMessage `json:"authMethods"`
	}
	_ = json.Unmarshal(raw, &res)
	c.smu.Lock()
	c.agentCap = res.AgentCapabilities
	c.smu.Unlock()
	if res.ProtocolVersion != 0 && res.ProtocolVersion != ProtocolVersion {
		c.logf("acp_protocol_version", map[string]any{
			"agent": c.name, "advertised": ProtocolVersion, "reported": res.ProtocolVersion,
		})
	}
	return nil
}

// NewSession opens a session and remembers its id. cwd may be empty.
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	params := map[string]any{
		// The harness executes nothing on the agent's behalf, so a session
		// that expects client-side filesystem or terminal services is told so
		// up front instead of at the first refusal.
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	raw, err := c.request(ctx, c.timeout, "session/new", params)
	if err != nil {
		return "", fmt.Errorf("acp session/new: %w", err)
	}
	var res struct {
		SessionID string `json:"sessionId"`
		// Some servers spell it differently; accept both.
		Session string `json:"session"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("acp session/new: decode: %w", err)
	}
	id := res.SessionID
	if id == "" {
		id = res.Session
	}
	if id == "" {
		// Older shape: {"session": {"id": "..."}}
		var nested struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		}
		if json.Unmarshal(raw, &nested) == nil {
			id = nested.Session.ID
		}
	}
	if id == "" {
		return "", fmt.Errorf("acp session/new: response carried no sessionId: %s", truncate(string(raw), 200))
	}
	c.smu.Lock()
	c.session = id
	c.smu.Unlock()
	return id, nil
}

// Handshake runs Initialize and NewSession in order.
func (c *Client) Handshake(ctx context.Context, cwd string) (string, error) {
	if err := c.Initialize(ctx); err != nil {
		return "", err
	}
	return c.NewSession(ctx, cwd)
}

// ---------- prompt turn ----------

// Prompt sends one turn and waits for the agent's final message. The result
// carries the stop reason, the collected agent messages and every tool call the
// agent asked for (each refused and recorded).
func (c *Client) Prompt(ctx context.Context, text string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	session := c.SessionID()
	if session == "" {
		return Result{}, errors.New("acp: no session (call Handshake first)")
	}

	c.smu.Lock()
	c.updates = nil
	c.final = nil
	c.smu.Unlock()

	timeout := c.promptTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := c.request(pctx, 0, "session/prompt", map[string]any{
		"sessionId": session,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	})

	res := Result{SessionID: session}
	res.ToolCalls, res.Updates, res.Final = c.drain()
	// The result text comes only from what the agent actually said. Falling
	// back to the prompt would hand a DAG node its own task back as output.
	res.Text = lastText(res.Final)

	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.IsCancelled() {
			return res, fmt.Errorf("%w: %v", ErrCancelled, rpcErr)
		}
		if errors.Is(pctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return res, fmt.Errorf("acp: prompt timed out after %s: %w", timeout, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The caller gave up: tell the agent to abandon the turn too, on a
			// detached context (ctx is already dead). Best-effort — a server
			// that ignores it gets killed by Close.
			cctx, ccancel := context.WithTimeout(context.Background(), CancelTimeout)
			_ = c.Cancel(cctx)
			ccancel()
			return res, fmt.Errorf("%w: %w", ErrCancelled, ctxErr)
		}
		return res, fmt.Errorf("acp session/prompt: %w", err)
	}

	var pr promptResult
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &pr)
		pr.Raw = raw
	}
	res.StopReason = pr.StopReason
	if pr.StopReason == "cancelled" {
		return res, fmt.Errorf("%w: agent reported a cancelled turn", ErrCancelled)
	}
	if res.Text == "" {
		return res, fmt.Errorf("acp: agent %s produced no final message (stopReason=%q)", c.name, pr.StopReason)
	}
	return res, nil
}

// Cancel tells the agent to abandon the turn in flight. It is a notification:
// ACP defines session/cancel as one-way, so this returns as soon as the frame
// is on the wire. If the server does not stop, Close kills the process.
func (c *Client) Cancel(ctx context.Context) error {
	session := c.SessionID()
	if session == "" {
		return errors.New("acp: no session to cancel")
	}
	timeout := c.timeout
	if timeout > CancelTimeout {
		timeout = CancelTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return c.notify(ctx, "session/cancel", map[string]any{"sessionId": session})
}

// Result is one completed (or cancelled) prompt turn.
type Result struct {
	SessionID  string         `json:"session_id,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
	Text       string         `json:"text"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	Updates    []Update       `json:"updates,omitempty"`
	Final      []ContentBlock `json:"final,omitempty"`
}

func (c *Client) drain() (calls []ToolCall, updates []Update, final []ContentBlock) {
	c.smu.Lock()
	defer c.smu.Unlock()
	updates = c.updates
	final = c.final
	c.updates, c.final = nil, nil
	if o, ok := c.observer.(interface{ Calls() []ToolCall }); ok {
		calls = o.Calls()
	}
	return calls, updates, final
}

// ---------- request plumbing ----------

var errClosed = errors.New("acp: client closed")

func (c *Client) request(ctx context.Context, timeout time.Duration, method string, params any) (json.RawMessage, error) {
	c.idmu.Lock()
	c.nextID++
	id := c.nextID
	c.idmu.Unlock()

	ch := make(chan response, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()

	if err := c.writeFrame(ctx, envelope{
		JSONRPC: "2.0",
		ID:      json.RawMessage(strconv.Itoa(id)),
		Method:  method,
		Params:  mustMarshal(params),
	}, true); err != nil {
		return nil, err
	}

	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case resp := <-ch:
		if resp.fatal != nil {
			return nil, resp.fatal
		}
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-deadline:
		return nil, fmt.Errorf("acp: %s timed out after %s", method, timeout)
	case <-c.done:
		if c.error() != nil {
			return nil, c.error()
		}
		return nil, fmt.Errorf("acp: agent %s stopped responding during %s", c.name, method)
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.writeFrame(ctx, envelope{JSONRPC: "2.0", Method: method, Params: mustMarshal(params)}, true)
}

// writeJob is one queued frame plus, for a caller that waits, the channel its
// outcome arrives on.
type writeJob struct {
	data []byte
	done chan error // nil = fire and forget
}

// writeLoop drains the outbox, one frame at a time, so no caller ever holds a
// lock across a blocking write. That matters: an agent that stops reading its
// stdin would otherwise wedge the read loop too (it answers the agent's
// requests from the same goroutine), and a wedged read loop means nobody drains
// the agent's stdout — a deadlock neither side can report.
func (c *Client) writeLoop() {
	for {
		select {
		case job := <-c.outbox:
			c.performWrite(job)
		case <-c.closed:
			for {
				select {
				case job := <-c.outbox:
					if job.done != nil {
						select {
						case job.done <- errClosed:
						default:
						}
					}
					continue
				default:
					return
				}
			}
		}
	}
}

func (c *Client) performWrite(job writeJob) {
	_, err := c.tr.Write(job.data)
	if err == nil {
		if f, ok := c.tr.(interface{ Flush() error }); ok {
			err = f.Flush()
		}
	}
	if job.done != nil {
		select {
		case job.done <- err:
		default:
		}
		return
	}
	if err != nil && !c.isKilling() {
		c.logf("acp_write_error", map[string]any{"agent": c.name, "error": err.Error()})
	}
}

// writeFrame marshals one frame and hands it to the write loop.
//
// wait=true blocks until the frame is on the wire, bounded by the write timeout
// and ctx; it is what requests and notifications use. wait=false never blocks:
// an answer to a server-initiated request is queued or dropped, because the
// read loop must keep reading no matter how full the agent's stdin is.
func (c *Client) writeFrame(ctx context.Context, env envelope, wait bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.closed:
		return errClosed
	default:
	}
	b, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("acp: encode %s: %w", env.Method, err)
	}
	b = append(b, '\n')

	var done chan error
	if wait {
		done = make(chan error, 1)
	} else {
		select {
		case c.outbox <- writeJob{data: b}:
		default:
			c.logf("acp_write_dropped", map[string]any{
				"agent": c.name, "method": env.Method, "reason": "write queue full",
			})
		}
		return nil
	}

	timer := time.NewTimer(c.writeTimeout)
	defer timer.Stop()
	select {
	case c.outbox <- writeJob{data: b, done: done}:
	case <-c.closed:
		return errClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return c.writeFailure(fmt.Errorf("acp: write %s timed out after %s (agent not reading its stdin)", env.Method, c.writeTimeout))
	}

	select {
	case werr := <-done:
		if werr != nil {
			return c.writeFailure(fmt.Errorf("acp: write %s: %w", env.Method, werr))
		}
		return nil
	case <-c.closed:
		return errClosed
	case <-c.done:
		return c.writeFailure(fmt.Errorf("acp: agent %s stopped reading during %s", c.name, env.Method))
	case <-timer.C:
		return c.writeFailure(fmt.Errorf("acp: write %s timed out after %s (agent not reading its stdin)", env.Method, c.writeTimeout))
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) writeFailure(err error) error {
	if c.isKilling() {
		return errClosed
	}
	return err
}

func mustMarshal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ---------- read loop ----------

func (c *Client) readLoop() {
	defer c.finish(nil)
	for {
		line, err := c.readLine()
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || c.isKilling() {
				return
			}
			c.finish(fmt.Errorf("acp: agent %s: %w", c.name, err))
			return
		}
	}
}

// readLine reads one framed message. A line longer than maxLineBytes is
// discarded (the rest of it is drained so the framing stays aligned) and
// reported as a malformed frame instead of growing without bound; a line that
// is not JSON at all is logged and skipped by the caller. Neither wedges the
// reader.
func (c *Client) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := c.reader.ReadSlice('\n')
		overflow := errors.Is(err, bufio.ErrBufferFull)
		if len(chunk) > 0 {
			if len(buf)+len(chunk) > maxLineBytes {
				c.malformed(chunk, "message longer than the 4 MiB frame limit; discarded")
				if derr := c.skipLine(overflow); derr != nil {
					return nil, derr
				}
				buf = nil
				continue
			}
			buf = append(buf, chunk...)
		}
		if overflow {
			continue // line longer than the read buffer; keep accumulating
		}
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return buf, nil // last line without a trailing newline
			}
			return buf, err
		}
		return buf, nil
	}
}

// skipLine drains the remainder of an over-long line so the next read starts
// on a frame boundary. alreadyOverflow says whether the discarded chunk itself
// was cut short by the read buffer.
func (c *Client) skipLine(alreadyOverflow bool) error {
	if !alreadyOverflow {
		return nil
	}
	for {
		_, err := c.reader.ReadSlice('\n')
		switch {
		case err == nil:
			return nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return io.EOF
		default:
			return err
		}
	}
}

func (c *Client) malformed(raw []byte, reason string) {
	c.logf("acp_malformed", map[string]any{
		"agent":  c.name,
		"reason": reason,
		"line":   truncate(strings.TrimSpace(string(raw)), 200),
	})
}

func (c *Client) handleLine(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return
	}
	var env envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		c.malformed(line, "not JSON: "+err.Error())
		return
	}
	if env.Method != "" {
		c.handleServerMessage(env)
		return
	}
	if len(env.ID) == 0 {
		return // a response without an id cannot be routed; drop it
	}
	id, ok := decodeID(env.ID)
	if !ok {
		c.malformed(line, "response id is neither a number nor a string")
		return
	}
	c.pmu.Lock()
	ch, found := c.pending[id]
	c.pmu.Unlock()
	if !found {
		return // late or duplicate response
	}
	select {
	case ch <- response{result: env.Result, err: env.Error}:
	default:
	}
}

// handleServerMessage dispatches an agent-initiated frame: a notification we
// observe, or a request we refuse.
func (c *Client) handleServerMessage(env envelope) {
	switch env.Method {
	case "session/request_permission":
		c.handlePermissionRequest(env)
	case "session/update":
		c.handleUpdate(env.Params)
	case "fs/read_text_file", "fs/write_text_file":
		c.refuseServerRequest(env, kindForMethod(env.Method), "filesystem access")
	case "terminal/create", "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		c.refuseServerRequest(env, KindExecute, "terminal access")
	case "session/cancel":
		// A server echoing cancel back at us: nothing to do.
	default:
		if len(env.ID) == 0 {
			// Unknown notification: observed, not acted on.
			c.logf("acp_notification", map[string]any{"agent": c.name, "method": env.Method})
			return
		}
		// Unknown request: answer with method-not-found so the agent is not
		// left hanging on a promise we never make.
		c.respondError(env.ID, CodeMethodNotFound, "method not supported by kintsugi: "+env.Method)
	}
}

type permissionParams struct {
	SessionID string       `json:"sessionId"`
	ToolCall  *toolCall    `json:"toolCall"`
	Options   []permOption `json:"options"`
}

type permOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func (c *Client) handlePermissionRequest(env envelope) {
	var p permissionParams
	_ = json.Unmarshal(env.Params, &p)
	tc := p.ToolCall
	if tc == nil {
		tc = &toolCall{}
	}
	call := tc.toToolCall(p.SessionID)
	call.PermissionID = firstOption(p.Options)
	call.Refused = true
	reason := c.refusal(call)
	call.Reason = reason
	c.observer.Observe(call)
	c.logf("acp_permission_refused", map[string]any{
		"agent": c.name, "session": call.SessionID, "tool_call": call.Summary(), "reason": reason,
	})

	result := map[string]any{
		"outcome": map[string]any{
			"outcome":  "refused",
			"optionId": call.PermissionID,
		},
		// Same shape under the name other ACP builds use, so a server that
		// reads only one of the two still learns nothing ran.
		"permissionOutcome": map[string]any{
			"outcome":  "refused",
			"optionId": call.PermissionID,
		},
		"refused": true,
		"reason":  reason,
	}
	if len(env.ID) > 0 {
		c.respondResult(env.ID, result)
	}
	c.recordCall(call)
}

func (c *Client) refuseServerRequest(env envelope, kind, what string) {
	call := ToolCall{
		Kind: kind, Title: what, Refused: true, ReceivedAt: time.Now(),
		ParamsRaw: env.Params,
	}
	call.Reason = c.refusal(call)
	c.observer.Observe(call)
	c.recordCall(call)
	c.logf("acp_request_refused", map[string]any{
		"agent": c.name, "method": env.Method, "kind": kind, "reason": call.Reason,
	})
	if len(env.ID) == 0 {
		return
	}
	c.respondError(env.ID, CodeMethodNotFound, "refused: the AgentForge harness is read-only towards external agents and performs no "+what)
}

func (c *Client) refusal(call ToolCall) string {
	if c.observer == nil {
		return RefusalText
	}
	if r := c.observer.Refuse(call); r != "" {
		return r
	}
	return RefusalText
}

func (c *Client) recordCall(call ToolCall) {
	c.smu.Lock()
	defer c.smu.Unlock()
	if len(c.updates) >= maxUpdates {
		return
	}
	c.updates = append(c.updates, Update{
		SessionUpdate: "tool_call_refused",
		ToolCall:      call.Summary(),
		Text:          call.Reason,
		ReceivedAt:    call.ReceivedAt,
	})
}

type sessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       *contentBlock   `json:"content"`
	ToolCall      *toolCall       `json:"toolCall"`
	ToolCallID    string          `json:"toolCallId"`
	Usage         json.RawMessage `json:"usage"`
	Entries       json.RawMessage `json:"entries"`
	RawTitle      string          `json:"title"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCall struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Locations []ToolLocation  `json:"locations"`
	RawInput  json.RawMessage `json:"rawInput"`
	RawOutput json.RawMessage `json:"rawOutput"`
	Content   []contentBlock  `json:"content"`
	SessionID string          `json:"sessionId"`
}

func (t *toolCall) toToolCall(session string) ToolCall {
	call := ToolCall{
		ID: t.ID, Kind: classifyKind(t.Kind), Title: t.Title, Status: t.Status,
		Locations: t.Locations, RawInput: t.RawInput, RawOutput: t.RawOutput,
		ReceivedAt: time.Now(), SessionID: session,
	}
	for _, b := range t.Content {
		call.Content = append(call.Content, ContentBlock{Type: b.Type, Text: b.Text})
	}
	return call
}

func (c *Client) handleUpdate(params json.RawMessage) {
	var u sessionUpdate
	if err := json.Unmarshal(params, &u); err != nil {
		c.malformed(params, "session/update did not decode: "+err.Error())
		return
	}
	switch u.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		if u.Content == nil {
			return
		}
		text := u.Content.Text
		if text == "" {
			return
		}
		truncated := false
		if len(text) > maxEventText {
			text, truncated = text[:maxEventText], true
		}
		c.smu.Lock()
		defer c.smu.Unlock()
		if u.SessionUpdate == "agent_message_chunk" {
			c.final = append(c.final, ContentBlock{Type: "text", Text: text})
		}
		if len(c.updates) < maxUpdates {
			c.updates = append(c.updates, Update{
				SessionUpdate: u.SessionUpdate, Text: text, Truncated: truncated, ReceivedAt: time.Now(),
			})
		}
	case "tool_call", "tool_call_update":
		if u.ToolCall == nil {
			return
		}
		call := u.ToolCall.toToolCall("")
		call.ID = firstNonEmpty(call.ID, u.ToolCallID)
		// Observed only. A tool the *agent* ran inside its own sandbox is its
		// business; a tool it asks *us* to run arrives as a request and is
		// refused there.
		c.logf("acp_tool_call_observed", map[string]any{
			"agent": c.name, "tool_call": call.Summary(), "status": call.Status,
		})
		c.smu.Lock()
		defer c.smu.Unlock()
		if len(c.updates) < maxUpdates {
			c.updates = append(c.updates, Update{
				SessionUpdate: u.SessionUpdate, ToolCall: call.Summary(), ReceivedAt: time.Now(),
			})
		}
	case "plan":
		c.logf("acp_plan", map[string]any{"agent": c.name})
	default:
		c.smu.Lock()
		defer c.smu.Unlock()
		if len(c.updates) < maxUpdates {
			c.updates = append(c.updates, Update{SessionUpdate: u.SessionUpdate, ReceivedAt: time.Now()})
		}
	}
}

func (c *Client) respondResult(id json.RawMessage, result any) {
	c.respondRaw(envelope{JSONRPC: "2.0", ID: id, Result: mustMarshal(result)})
}

func (c *Client) respondError(id json.RawMessage, code int, msg string) {
	c.respondRaw(envelope{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

// respondRaw answers a server-initiated request. The answer is queued for the
// write loop and never blocks the read loop: if the agent's stdin is full, the
// reply is dropped and logged rather than wedging the reader that is the only
// thing draining the agent's stdout.
func (c *Client) respondRaw(env envelope) {
	if c.isKilling() {
		return
	}
	_ = c.writeFrame(context.WithoutCancel(context.Background()), env, false)
}

// ---------- lifecycle ----------

func (c *Client) error() error {
	c.emu.Lock()
	defer c.emu.Unlock()
	return c.err
}

// isKilling reports whether Close has started tearing the client down, in
// which case a broken pipe or a stopped reader is expected rather than a fault.
func (c *Client) isKilling() bool { return c.killing.Load() }

func (c *Client) finish(err error) {
	c.doneOnce.Do(func() {
		if err != nil {
			c.emu.Lock()
			if c.err == nil {
				c.err = err
			}
			c.emu.Unlock()
			c.logf("acp_error", map[string]any{"agent": c.name, "error": err.Error()})
		}
		cause := c.error()
		if cause == nil {
			if c.isKilling() {
				cause = errClosed
			} else {
				cause = fmt.Errorf("acp: agent %s closed the connection", c.name)
			}
		}
		// Wake every in-flight request so nobody waits on a dead transport.
		c.pmu.Lock()
		for id, ch := range c.pending {
			select {
			case ch <- response{fatal: cause}:
			default:
			}
			delete(c.pending, id)
		}
		c.pmu.Unlock()
		close(c.done)
	})
}

// Close stops the read loop, closes the transport and kills the child process
// if one is attached and still alive. Safe to call more than once. It never
// returns an error: a process that will not die politely is killed, and the
// caller's context is already the authority on whether the turn succeeded.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.killing.Store(true)
		close(c.closed)
		if pt, ok := c.tr.(processTransport); ok {
			c.closeStdio(pt)
			return
		}
		// In-process transport: closing it is what makes the read loop stop.
		_ = c.tr.Close()
		c.finish(nil)
	})
}

// CancelAndWait sends session/cancel, waits for grace, then closes (which
// kills the child if it is still alive). This is what a cancelled DAG node
// does: ask first, kill second.
func (c *Client) CancelAndWait(ctx context.Context, grace time.Duration) {
	cctx := ctx
	if cctx == nil {
		cctx = context.Background()
	}
	// A cancelled context cannot carry the cancel frame, so detach: the point
	// of session/cancel is precisely to run after the caller gave up.
	_ = c.Cancel(context.WithoutCancel(cctx))
	if grace <= 0 {
		grace = CancelTimeout
	}
	select {
	case <-c.done:
	case <-time.After(grace):
	}
	c.Close()
}

// ---------- helpers ----------

func decodeID(raw json.RawMessage) (int, bool) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
	}
	return 0, false
}

func firstOption(opts []permOption) string {
	for _, o := range opts {
		switch o.Kind {
		case "reject_once", "reject_always":
			return o.OptionID
		}
	}
	if len(opts) > 0 {
		return opts[len(opts)-1].OptionID
	}
	return ""
}

// classifyKind maps an ACP tool-call kind onto the small vocabulary the
// observer records. Unknown kinds stay unknown — and are still refused.
func classifyKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "read":
		return KindRead
	case "edit":
		return KindEdit
	case "delete":
		return KindDelete
	case "move":
		return KindMove
	case "search":
		return KindSearch
	case "execute":
		return KindExecute
	case "think":
		return KindThink
	case "fetch":
		return KindFetch
	case "other", "":
		if kind == "" {
			return KindUnknown
		}
		return KindOther
	case "switch_mode":
		return KindSwitchMode
	default:
		return KindUnknown
	}
}

func kindForMethod(method string) string {
	switch method {
	case "fs/read_text_file":
		return KindRead
	case "fs/write_text_file":
		return KindEdit
	default:
		return KindExecute
	}
}

func lastText(blocks []ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" || blk.Type == "" {
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func jsonValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
