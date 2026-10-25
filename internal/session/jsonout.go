// Headless JSON output (M9.5).
//
// `kintsugi run --output-format json` prints one JSON object when the run ends;
// `kintsugi run --output-format stream-json` prints one JSON object per line
// while the run happens. Both streams are machine-parseable: every line is a
// complete JSON object, there is no prose on stdout, no ANSI escape, no
// trailing comma, and every text field goes through the shared redactor
// first, so a secret a tool printed never reaches the stream.
//
// The schema is provider-agnostic on purpose. The same events and the same
// key names are produced whether the model runs on a cloud provider (DeepSeek,
// Kimi, GLM, Qwen, OpenRouter, Cline Pass, …) or on a local runner (Ollama,
// vLLM); only the "model" field of the session header differs. The operator's
// prompts, memory and logs never leave the disk either way.
package session

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"kintsugi/internal/redact"
)

// Output modes of the headless emitter.
const (
	// ModeText keeps the plain stream: the emitter writes nothing at all.
	ModeText = "text"
	// ModeJSON prints one JSON document when the run ends.
	ModeJSON = "json"
	// ModeStreamJSON prints one JSON object per line as events happen.
	ModeStreamJSON = "stream-json"
)

// Event types. They are part of the published schema: a consumer may switch
// on them, so they never change meaning.
const (
	EventSession    = "session"
	EventUser       = "user"
	EventThinking   = "thinking"
	EventAssistant  = "assistant"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventRefusal    = "refusal"
	EventStreamRule = "stream_rule"
	EventRepair     = "repair"
	EventDrift      = "drift"
	EventUsage      = "usage"
	EventFinal      = "final"
	EventError      = "error"
)

// SchemaID names the document schema version of the "json" mode.
const SchemaID = "kintsugi.run/1"

// Bounds on the text the emitter carries. A tool result is a summary, not a
// transcript: the full output stays in the session log.
const (
	maxArgsRunes   = 1000
	maxResultRunes = 2000
)

// Event is one run event. Only the fields that belong to the event's type are
// written; the others are ignored. Every text field is redacted and stripped
// of ANSI escapes before it is written.
type Event struct {
	// Type is the event name: one of the Event* constants.
	Type string

	// Session header.
	SessionID string
	Model     string
	Persona   string
	Role      string
	Stack     string
	Started   string
	Ended     string
	Mode      string

	// Text carries the user prompt, the assistant text or the final answer.
	Text string

	// Tool call and tool result.
	Name   string
	Args   string
	Result string
	Failed bool

	// Refusal, repair and drift telemetry. None of these carry the model's
	// prose: a refusal phrase is dropped at the telemetry boundary.
	Class   string
	Classes []string
	Level   int
	Outcome string
	IDs     []string

	// Stream-rule telemetry (I-70). Rule names the operator rule, Action is
	// abort|redact|inject, Reason is the operator's own label and Count is how
	// many spans a redact rule masked. There is deliberately no field for the
	// matched text: a rule must never turn model prose into a record.
	Rule   string
	Action string
	Reason string
	Count  int

	// Usage. The values written are the run's cumulative totals.
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Requests         int

	// Error is the failure message of a failed run.
	Error string
}

// Emitter renders a run as JSON.
//
// The zero value is not usable; build one with NewEmitter. It is safe for
// concurrent use: the agent loop's callbacks may fire from more than one
// goroutine.
type Emitter struct {
	mu   sync.Mutex
	w    io.Writer
	mode string

	header  runHeader
	turns   []runTurn
	curIdx  int
	counts  runCounts
	usage   runUsage
	final   string
	errText string
	ended   string
	closed  bool
}

// NewEmitter returns an emitter that writes to w.
//
// mode is "text" (the plain stream — the emitter writes nothing), "json" (one
// document when the run ends) or "stream-json" (one object per line). An
// unknown mode falls back to "text", so a typo can never turn a plain run
// into a broken JSON stream; the CLI validates the flag before it gets here.
func NewEmitter(w io.Writer, mode string) *Emitter {
	switch mode {
	case ModeJSON, ModeStreamJSON:
	default:
		mode = ModeText
	}
	return &Emitter{w: w, mode: mode}
}

// Mode returns the emitter's mode: "text", "json" or "stream-json".
func (e *Emitter) Mode() string {
	if e == nil {
		return ModeText
	}
	return e.mode
}

// Enabled reports whether the emitter writes anything. It is false in "text"
// mode, where the plain stream is printed by the caller as before.
func (e *Emitter) Enabled() bool {
	return e != nil && e.mode != ModeText
}

// Emit records one event.
//
// In "stream-json" mode the event is written immediately, one JSON object on
// one line. In "json" mode it is folded into the document that Close prints.
// In "text" mode it is ignored.
func (e *Emitter) Emit(ev Event) error {
	if e == nil || e.mode == ModeText {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	ev = sanitizeEvent(ev)
	e.fold(ev)
	if ev.Type == EventUsage {
		// A usage event carries the run's totals, not the last request's
		// delta: the last usage line of a stream is the run's ledger.
		ev.PromptTokens = e.usage.PromptTokens
		ev.CompletionTokens = e.usage.CompletionTokens
		ev.TotalTokens = e.usage.TotalTokens
		ev.Requests = e.usage.Requests
	}
	if e.mode != ModeStreamJSON {
		return nil
	}
	b, err := marshalPairs(eventPairs(ev))
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = e.w.Write(b)
	return err
}

// Close writes the final document. In "json" mode it prints the single JSON
// object; in "stream-json" mode there is nothing left to print. Calling it
// more than once is safe: only the first call writes.
func (e *Emitter) Close() error {
	if e == nil || e.mode != ModeJSON {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true

	if e.header.Ended == "" {
		e.header.Ended = e.ended
	}
	if e.header.Ended == "" {
		e.header.Ended = stampNow()
	}
	turns := e.turns
	if turns == nil {
		turns = []runTurn{}
	}
	for i := range turns {
		if turns[i].ToolCalls == nil {
			turns[i].ToolCalls = []runTool{}
		}
	}
	doc := runDocument{
		Schema:   SchemaID,
		Session:  e.header,
		Turns:    turns,
		Counters: e.counts,
		Usage:    e.usage,
		Final:    e.final,
		Error:    e.errText,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = e.w.Write(b)
	return err
}

// fold updates the emitter's state from one event. It runs in both JSON
// modes: the counters and the totals are the same either way.
func (e *Emitter) fold(ev Event) {
	switch ev.Type {
	case EventSession:
		e.header = runHeader{
			ID:      ev.SessionID,
			Model:   ev.Model,
			Persona: ev.Persona,
			Role:    ev.Role,
			Stack:   ev.Stack,
			Started: ev.Started,
			Ended:   ev.Ended,
			Mode:    ev.Mode,
		}
	case EventUser:
		e.turns = append(e.turns, runTurn{User: ev.Text, ToolCalls: []runTool{}})
		e.curIdx = len(e.turns) - 1
	case EventThinking:
		t := e.currentTurn()
		if ev.Text == "" {
			return
		}
		if t.Thinking == "" {
			t.Thinking = ev.Text
		} else {
			t.Thinking = t.Thinking + "\n" + ev.Text
		}
	case EventAssistant:
		t := e.currentTurn()
		if ev.Text == "" {
			return
		}
		if t.Assistant == "" {
			t.Assistant = ev.Text
		} else {
			t.Assistant = t.Assistant + "\n" + ev.Text
		}
	case EventToolCall:
		t := e.currentTurn()
		t.ToolCalls = append(t.ToolCalls, runTool{Name: ev.Name, Args: ev.Args})
	case EventToolResult:
		t := e.currentTurn()
		if n := len(t.ToolCalls); n > 0 {
			t.ToolCalls[n-1].Result = ev.Result
			t.ToolCalls[n-1].Failed = ev.Failed
			return
		}
		t.ToolCalls = append(t.ToolCalls, runTool{Name: ev.Name, Args: ev.Args, Result: ev.Result, Failed: ev.Failed})
	case EventRefusal:
		e.counts.Refusals++
	case EventStreamRule:
		// A rule application is reported, not counted with refusals: an operator
		// redaction is not a model refusal, and adding it to that counter would
		// make the refusal trend lie.
	case EventRepair:
		e.counts.Repairs++
	case EventDrift:
		e.counts.Drifts++
	case EventUsage:
		total := ev.TotalTokens
		if total == 0 {
			total = ev.PromptTokens + ev.CompletionTokens
		}
		e.usage.Requests++
		e.usage.PromptTokens += ev.PromptTokens
		e.usage.CompletionTokens += ev.CompletionTokens
		e.usage.TotalTokens += total
	case EventFinal:
		e.final = ev.Text
		if ev.Ended != "" {
			e.ended = ev.Ended
		}
	case EventError:
		e.errText = ev.Error
		if ev.Ended != "" {
			e.ended = ev.Ended
		}
	}
}

// currentTurn returns the turn the events belong to, creating one when the
// run produced output before any user turn (a resumed session, for example).
func (e *Emitter) currentTurn() *runTurn {
	if len(e.turns) == 0 {
		e.turns = append(e.turns, runTurn{ToolCalls: []runTool{}})
	}
	if e.curIdx < 0 || e.curIdx >= len(e.turns) {
		e.curIdx = len(e.turns) - 1
	}
	return &e.turns[e.curIdx]
}

// ---- schema -----------------------------------------------------------------

// runHeader is the session header of the JSON document.
type runHeader struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Persona string `json:"persona"`
	Role    string `json:"role"`
	Stack   string `json:"stack"`
	Started string `json:"started"`
	Ended   string `json:"ended"`
	Mode    string `json:"mode"`
}

// runTurn is one operator turn: the prompt and what the assistant did for it.
type runTurn struct {
	User      string    `json:"user"`
	Thinking  string    `json:"thinking"`
	Assistant string    `json:"assistant"`
	ToolCalls []runTool `json:"tool_calls"`
}

// runTool is one tool call with the summary of its result.
type runTool struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
	Failed bool   `json:"failed"`
}

// runCounts are the refusal, repair and drift counters of the run.
type runCounts struct {
	Refusals int `json:"refusals"`
	Repairs  int `json:"repairs"`
	Drifts   int `json:"drifts"`
}

// runUsage are the token totals of the run.
type runUsage struct {
	Requests         int `json:"requests"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// runDocument is the single JSON object the "json" mode prints. Its key
// order is the struct order and never changes.
type runDocument struct {
	Schema   string    `json:"schema"`
	Session  runHeader `json:"session"`
	Turns    []runTurn `json:"turns"`
	Counters runCounts `json:"counters"`
	Usage    runUsage  `json:"usage"`
	Final    string    `json:"final"`
	Error    string    `json:"error"`
}

// ---- event rendering --------------------------------------------------------

// jsonPair is one key/value pair of a JSON object.
type jsonPair struct {
	key string
	val any
}

// marshalPairs renders pairs as one JSON object, keys in the given order.
func marshalPairs(pairs []jsonPair) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(p.key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(p.val)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// eventPairs renders one event as an ordered list of key/value pairs. The
// key set of an event type never changes: a consumer can rely on it.
func eventPairs(ev Event) []jsonPair {
	p := []jsonPair{{key: "type", val: ev.Type}}
	switch ev.Type {
	case EventSession:
		p = append(p,
			jsonPair{key: "session_id", val: ev.SessionID},
			jsonPair{key: "model", val: ev.Model},
			jsonPair{key: "persona", val: ev.Persona},
			jsonPair{key: "role", val: ev.Role},
			jsonPair{key: "stack", val: ev.Stack},
			jsonPair{key: "started", val: ev.Started},
		)
		if ev.Ended != "" {
			p = append(p, jsonPair{key: "ended", val: ev.Ended})
		}
		p = append(p, jsonPair{key: "mode", val: ev.Mode})
	case EventUser, EventAssistant:
		p = append(p, jsonPair{key: "text", val: ev.Text})
	case EventToolCall:
		p = append(p,
			jsonPair{key: "name", val: ev.Name},
			jsonPair{key: "args", val: ev.Args},
		)
	case EventToolResult:
		p = append(p,
			jsonPair{key: "name", val: ev.Name},
			jsonPair{key: "result", val: ev.Result},
			jsonPair{key: "failed", val: ev.Failed},
		)
	case EventRefusal:
		p = append(p,
			jsonPair{key: "class", val: ev.Class},
			jsonPair{key: "level", val: ev.Level},
			jsonPair{key: "outcome", val: ev.Outcome},
		)
	case EventRepair:
		p = append(p,
			jsonPair{key: "name", val: ev.Name},
			jsonPair{key: "classes", val: ev.Classes},
			jsonPair{key: "failed", val: ev.Failed},
		)
		if ev.Error != "" {
			p = append(p, jsonPair{key: "error", val: ev.Error})
		}
	case EventDrift:
		p = append(p,
			jsonPair{key: "class", val: ev.Class},
			jsonPair{key: "outcome", val: ev.Outcome},
			jsonPair{key: "ids", val: ev.IDs},
		)
	case EventUsage:
		p = append(p,
			jsonPair{key: "prompt_tokens", val: ev.PromptTokens},
			jsonPair{key: "completion_tokens", val: ev.CompletionTokens},
			jsonPair{key: "total_tokens", val: ev.TotalTokens},
			jsonPair{key: "requests", val: ev.Requests},
		)
	case EventFinal:
		p = append(p, jsonPair{key: "text", val: ev.Text})
		if ev.Ended != "" {
			p = append(p, jsonPair{key: "ended", val: ev.Ended})
		}
	case EventError:
		p = append(p, jsonPair{key: "error", val: ev.Error})
		if ev.Ended != "" {
			p = append(p, jsonPair{key: "ended", val: ev.Ended})
		}
	}
	return p
}

// ---- hygiene ----------------------------------------------------------------

// ansiRe matches the escape sequences a terminal would interpret: CSI
// sequences, OSC strings and the two-character escapes.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")

// sanitizeEvent scrubs one event before it is written: secrets go through the
// shared redactor, escape sequences are dropped and long tool payloads are
// cut down to a summary.
func sanitizeEvent(ev Event) Event {
	ev.Text = sanitizeText(ev.Text)
	ev.Args = clipText(sanitizeText(ev.Args), maxArgsRunes)
	ev.Result = clipText(sanitizeText(ev.Result), maxResultRunes)
	ev.Error = sanitizeText(ev.Error)
	if ev.Classes == nil {
		ev.Classes = []string{}
	}
	if ev.IDs == nil {
		ev.IDs = []string{}
	}
	return ev
}

// sanitizeText redacts secrets and strips ANSI escapes from one string.
func sanitizeText(s string) string {
	if s == "" {
		return ""
	}
	return stripANSI(redact.Redact(s))
}

// stripANSI removes terminal escape sequences from s.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// clipText cuts s to at most n runes, marking the cut with an ellipsis.
func clipText(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

// stampNow is the timestamp format used in the header: RFC 3339 with
// nanoseconds, in UTC, so two runs never share a stamp.
func stampNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
