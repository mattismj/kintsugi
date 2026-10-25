// Package mods implements the constrained extension API (M12.5).
//
// A mod is an **external process** that contributes tools, slash commands
// and lifecycle events to the harness. There is no in-process script
// runtime and no dynamic library loading: the process boundary *is* the
// trust boundary, so the operator can read what a mod is (a program on
// disk) instead of what it can do (whatever a script engine decides).
//
// # The wire
//
// The harness and the mod speak newline-delimited JSON-RPC over stdin and
// stdout: one JSON object per line, in both directions. stderr is the mod's
// own log and is never part of the protocol.
//
//	Harness → mod (initialize):
//	{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-01-01","client":{"name":"kintsugi","version":"0.7"},"mod":{"name":"hello","version":"0.1.0"},"capabilities":{}}}
//
//	Mod → harness (the handshake answers with what it provides):
//	{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-01-01","tools":[{"name":"echo","description":"Echo text.","inputSchema":{"type":"object"}}],"commands":[{"name":"hello","description":"Say hello.","argumentHint":"[name]"}]}}
//
//	Harness → mod (the handshake is done):
//	{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}
//
//	Harness → mod (a tool call):
//	{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}
//
//	Mod → harness (the tool answered):
//	{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"echo: hi"}],"isError":false}}
//
//	Harness → mod (a slash command):
//	{"jsonrpc":"2.0","id":3,"method":"commands/call","params":{"name":"hello","arguments":"world"}}
//
//	Harness → mod (a lifecycle event the mod asked to observe):
//	{"jsonrpc":"2.0","method":"events/notify","params":{"event":"PreToolUse","data":{"tool":"read"}}}
//
//	Mod → harness (a failure):
//	{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"unknown tool"}}
//
// # The trust model
//
// Mods live in the workspace (.kintsugi/mods by default), so they are
// repository-controlled code: a cloned repository ships them. They therefore
// run only in a workspace the operator marked as trusted — the same marker
// that governs the project hooks (`kintsugi mods trust`, see internal/hooks).
// An untrusted mod is listed and reported, never started.
//
// # The capability model
//
// The manifest is the contract: it declares the tools and the commands the
// mod provides. The harness exposes exactly those, never more, and a mod
// that advertises a tool the manifest does not declare is ignored. A mod
// that declares a tool it does not answer is dropped with an error.
//
// A mod's tools are tools. They pass through the same permission matrix as
// the built-ins, so a mod can never widen what the operator allowed: the
// permission layer remains the only authority.
package mods

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"kintsugi/internal/hooks"
	"kintsugi/internal/provider"
)

const (
	// clientName and clientVersion identify the harness to the mod.
	clientName    = "kintsugi"
	clientVersion = "0.7"
	// protocolVersion is the mod protocol the harness speaks.
	protocolVersion = "2026-01-01"

	// MethodInitialize starts the handshake.
	MethodInitialize = "initialize"
	// MethodInitialized is the notification that ends the handshake.
	MethodInitialized = "notifications/initialized"
	// MethodToolsCall invokes one of the mod's tools.
	MethodToolsCall = "tools/call"
	// MethodCommandsCall invokes one of the mod's slash commands.
	MethodCommandsCall = "commands/call"
	// MethodEvent delivers a lifecycle event to the mods that observe it.
	MethodEvent = "events/notify"

	// DefaultDir is the workspace-relative directory the mods live in.
	DefaultDir = ".kintsugi/mods"
	// DefaultTimeout bounds one mod request.
	DefaultTimeout = 30 * time.Second
	// maxToolNameChars caps an exposed tool name, like the MCP names.
	maxToolNameChars = 64
)

// ErrUntrusted is the reason a mod was listed but not started.
var ErrUntrusted = errors.New("the workspace is not trusted: project mods are skipped (run `kintsugi mods trust`)")

// Manifest is what a mod declares about itself.
//
// It is read from a mod.toml or a mod.json file. It names capabilities; it
// never carries a secret. Env holds environment variable *names* only: the
// values stay in the operator's environment.
type Manifest struct {
	Name        string `toml:"name" json:"name"`
	Version     string `toml:"version" json:"version"`
	Description string `toml:"description" json:"description"`
	// Command is the executable to run. A relative path is resolved
	// against the manifest's own directory.
	Command string `toml:"command" json:"command"`
	// Args is the argv after the command.
	Args []string `toml:"args" json:"args"`
	// Tools are the tools the mod provides.
	Tools []ToolSpec `toml:"tools" json:"tools"`
	// Commands are the slash commands the mod provides.
	Commands []CommandSpec `toml:"commands" json:"commands"`
	// Events are the lifecycle events the mod observes.
	Events []string `toml:"events" json:"events"`
	// Env names the environment variables the mod process receives. The
	// key is the name the mod sees; the value is the name of the
	// operator's environment variable that holds the value. An empty value
	// means the names are the same. A variable that is not set is not
	// passed on.
	Env map[string]string `toml:"env" json:"env"`
	// Dir is the directory the manifest was found in. The mod process runs
	// there.
	Dir string `toml:"-" json:"-"`
}

// ToolSpec is one tool the manifest declares.
type ToolSpec struct {
	Name        string `toml:"name" json:"name"`
	Description string `toml:"description" json:"description"`
	// Parameters is the JSON schema of the tool's arguments.
	Parameters map[string]any `toml:"parameters" json:"parameters"`
}

// CommandSpec is one slash command the manifest declares.
type CommandSpec struct {
	Name        string `toml:"name" json:"name"`
	Description string `toml:"description" json:"description"`
	// ArgumentHint describes the arguments, for the operator's help line.
	ArgumentHint string `toml:"argument_hint" json:"argument_hint"`
}

// Status is the state of a mod.
type Status string

const (
	// StatusRunning means the mod answered the handshake.
	StatusRunning Status = "running"
	// StatusUntrusted means the mod was listed but never started.
	StatusUntrusted Status = "untrusted"
	// StatusFailed means the mod could not be started or did not answer.
	StatusFailed Status = "failed"
	// StatusStopped means the mod was shut down.
	StatusStopped Status = "stopped"
)

// Dir resolves the configured mods directory. An empty configuration means
// DefaultDir; a relative path is resolved against the workspace.
func Dir(workspace, configured string) string {
	p := strings.TrimSpace(configured)
	if p == "" {
		p = DefaultDir
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	return p
}

// Trusted reports whether the workspace carries the trust marker.
//
// The marker is the one the project hooks use, so one decision governs both
// surfaces: `kintsugi hooks trust` (or `kintsugi mods trust`) enables the
// repository's hooks and its mods together.
func Trusted(workspace string) bool {
	return hooks.IsTrusted(workspace)
}

// Load reads the manifests under dir.
//
// A manifest is a mod.toml or a mod.json file; the directory that holds it
// is the mod's home, and the mod process runs there. mod.toml wins over
// mod.json in the same directory.
//
// A manifest that cannot be read or parsed is reported as an error and
// skipped: one broken mod never hides the working ones. A missing
// directory is not an error — it simply holds no mods.
func Load(dir string) ([]Manifest, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mods %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("mods %s: not a directory", dir)
	}

	// One manifest per directory: mod.toml wins over mod.json.
	homes := map[string]map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name != "mod.toml" && name != "mod.json" {
			return nil
		}
		home := filepath.Dir(path)
		if homes[home] == nil {
			homes[home] = map[string]string{}
		}
		homes[home][name] = path
		return nil
	})

	dirs := make([]string, 0, len(homes))
	for home := range homes {
		dirs = append(dirs, home)
	}
	sort.Strings(dirs)

	var out []Manifest
	var errs []error
	for _, home := range dirs {
		files := homes[home]
		path, ok := files["mod.toml"]
		if !ok {
			path = files["mod.json"]
		}
		mf, err := readManifest(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errors.Join(errs...)
}

// readManifest decodes one manifest file.
func readManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("mods %s: %w", path, err)
	}
	var mf Manifest
	if strings.HasSuffix(path, ".json") {
		if err := json.Unmarshal(raw, &mf); err != nil {
			return Manifest{}, fmt.Errorf("mods %s: malformed JSON: %w", path, err)
		}
	} else {
		if _, err := toml.Decode(string(raw), &mf); err != nil {
			return Manifest{}, fmt.Errorf("mods %s: malformed TOML: %w", path, err)
		}
	}
	mf.Dir = filepath.Dir(path)
	if err := mf.validate(); err != nil {
		return Manifest{}, fmt.Errorf("mods %s: %w", path, err)
	}
	return mf, nil
}

// validate checks that a manifest is usable and normalizes it.
func (m *Manifest) validate() error {
	m.Name = strings.TrimSpace(m.Name)
	m.Command = strings.TrimSpace(m.Command)
	if m.Name == "" {
		return errors.New("the manifest has no name")
	}
	if m.Command == "" {
		return errors.New("the manifest has no command")
	}
	seen := map[string]bool{}
	for i := range m.Tools {
		m.Tools[i].Name = strings.TrimSpace(m.Tools[i].Name)
		if m.Tools[i].Name == "" {
			return fmt.Errorf("tool %d has no name", i)
		}
		if seen[m.Tools[i].Name] {
			return fmt.Errorf("tool %q is declared twice", m.Tools[i].Name)
		}
		seen[m.Tools[i].Name] = true
	}
	cmds := map[string]bool{}
	for i := range m.Commands {
		m.Commands[i].Name = strings.TrimSpace(m.Commands[i].Name)
		if m.Commands[i].Name == "" {
			return fmt.Errorf("command %d has no name", i)
		}
		if cmds[m.Commands[i].Name] {
			return fmt.Errorf("command %q is declared twice", m.Commands[i].Name)
		}
		cmds[m.Commands[i].Name] = true
	}
	return nil
}

// commandPath resolves the executable to run. A relative command is
// resolved against the manifest's directory, so "./run.sh" works.
func (m Manifest) commandPath() string {
	cmd := strings.TrimSpace(m.Command)
	if cmd == "" {
		return ""
	}
	if strings.ContainsAny(cmd, "/\\") && !filepath.IsAbs(cmd) {
		return filepath.Join(m.Dir, cmd)
	}
	return cmd
}

// ToolName is the name a mod's tool is exposed under: mod__<mod>__<tool>.
func ToolName(mod, tool string) string {
	base := "mod__" + sanitize(mod, 20) + "__" + sanitize(tool, 38)
	if len(base) > maxToolNameChars {
		base = base[:maxToolNameChars]
	}
	return base
}

// sanitize keeps a name to the characters a tool name may carry.
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
		out = "mod"
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// Mod is one mod: its manifest, its process when it runs, and the outcome of
// the handshake.
type Mod struct {
	// Manifest is what the operator's manifest declared.
	Manifest Manifest
	// Tools are the tools the mod actually provides. It is a subset of the
	// manifest's declarations.
	Tools []string
	// Status is the mod's state.
	Status Status
	// Err is why the mod is not running.
	Err error

	stateMu sync.Mutex // protects Status and Err
	idMu    sync.Mutex // protects nextID
	pendMu  sync.Mutex // protects pending
	writeMu sync.Mutex // protects the stdin write

	nextID  int
	pending map[int]chan rpcResponse

	proc   *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	done   chan struct{}

	doneOnce  sync.Once
	closeOnce sync.Once
	timeout   time.Duration
	log       func(kind string, v any)
}

// Name is the mod's name.
func (m *Mod) Name() string {
	if m == nil {
		return ""
	}
	return m.Manifest.Name
}

// setState records the mod's state.
func (m *Mod) setState(s Status, err error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.Status = s
	m.Err = err
}

// State returns the mod's current state and the reason for it.
func (m *Mod) State() (Status, error) {
	if m == nil {
		return StatusFailed, errors.New("no mod")
	}
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.Status, m.Err
}

// Definitions returns the tool schemas the mod provides.
//
// The names are namespaced (mod__<mod>__<tool>) and the set is exactly the
// manifest's declarations: a mod never gets more surface than it declared.
func (m *Mod) Definitions() []provider.Tool {
	if m == nil {
		return nil
	}
	status, _ := m.State()
	if status != StatusRunning {
		return nil
	}
	out := make([]provider.Tool, 0, len(m.Manifest.Tools))
	for _, spec := range m.Manifest.Tools {
		desc := strings.TrimSpace(spec.Description)
		if desc == "" {
			desc = "mod tool " + spec.Name
		}
		out = append(out, provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        ToolName(m.Manifest.Name, spec.Name),
				Description: desc,
				Parameters:  schemaOf(spec.Parameters),
			},
		})
	}
	return out
}

// Commands returns the slash commands the mod provides.
func (m *Mod) Commands() []CommandSpec {
	if m == nil {
		return nil
	}
	return append([]CommandSpec(nil), m.Manifest.Commands...)
}

// Events returns the lifecycle events the mod observes.
func (m *Mod) Events() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.Manifest.Events...)
}

// Call runs one of the mod's tools.
//
// The name may be the raw tool name ("echo") or the namespaced one
// ("mod__hello__echo"). A tool the manifest did not declare is refused:
// the harness does not hand a mod more than it declared.
func (m *Mod) Call(ctx context.Context, name string, args string) (string, bool, error) {
	if m == nil {
		return "", true, errors.New("no mod")
	}
	status, err := m.State()
	if status != StatusRunning {
		if err != nil {
			return "", true, fmt.Errorf("mod %q is not running: %w", m.Manifest.Name, err)
		}
		return "", true, fmt.Errorf("mod %q is not running", m.Manifest.Name)
	}
	spec, ok := m.toolSpec(name)
	if !ok {
		return "", true, fmt.Errorf("mod %q does not provide the tool %q", m.Manifest.Name, name)
	}
	payload := map[string]any{}
	if strings.TrimSpace(args) != "" {
		var v any
		if err := json.Unmarshal([]byte(args), &v); err != nil {
			return "", true, fmt.Errorf("mod %q: invalid arguments: %w", m.Manifest.Name, err)
		}
		payload, _ = v.(map[string]any)
		if payload == nil {
			payload = map[string]any{}
		}
	}
	raw, err := m.request(ctx, MethodToolsCall, map[string]any{
		"name":      spec.Name,
		"arguments": payload,
	})
	if err != nil {
		return "", true, err
	}
	return decodeContent(raw)
}

// CallCommand runs one of the mod's slash commands.
func (m *Mod) CallCommand(ctx context.Context, name string, args string) (string, error) {
	if m == nil {
		return "", errors.New("no mod")
	}
	status, err := m.State()
	if status != StatusRunning {
		if err != nil {
			return "", fmt.Errorf("mod %q is not running: %w", m.Manifest.Name, err)
		}
		return "", fmt.Errorf("mod %q is not running", m.Manifest.Name)
	}
	found := false
	for _, c := range m.Manifest.Commands {
		if c.Name == name {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("mod %q does not provide the command %q", m.Manifest.Name, name)
	}
	raw, err := m.request(ctx, MethodCommandsCall, map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	text, _, err := decodeContent(raw)
	return text, err
}

// toolSpec finds a declared tool by its raw or namespaced name.
func (m *Mod) toolSpec(name string) (ToolSpec, bool) {
	name = strings.TrimSpace(name)
	for _, spec := range m.Manifest.Tools {
		if spec.Name == name || ToolName(m.Manifest.Name, spec.Name) == name {
			return spec, true
		}
	}
	return ToolSpec{}, false
}

// NotifyEvent hands a lifecycle event to the mod.
//
// The event is a notification: the mod observes it, it does not answer it,
// so a mod that ignores it (or crashes on it) never blocks the session.
func (m *Mod) NotifyEvent(event string, data map[string]any) error {
	if m == nil {
		return errors.New("no mod")
	}
	status, err := m.State()
	if status != StatusRunning {
		if err != nil {
			return fmt.Errorf("mod %q is not running: %w", m.Manifest.Name, err)
		}
		return fmt.Errorf("mod %q is not running", m.Manifest.Name)
	}
	if data == nil {
		data = map[string]any{}
	}
	return m.notify(MethodEvent, map[string]any{"event": event, "data": data})
}

// ---------- the mod process ----------

// rpcResponse is one answer from the mod.
type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

// rpcError is the error object of a JSON-RPC answer.
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

// start launches the mod's process.
func start(mf Manifest, timeout time.Duration, log func(string, any)) (*Mod, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	path := mf.commandPath()
	cmd := exec.Command(path, mf.Args...)
	if mf.Dir != "" {
		cmd.Dir = mf.Dir
	}
	cmd.Env = modEnv(mf)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mod %q: %w", mf.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mod %q: %w", mf.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mod %q: %w", mf.Name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mod %q: %w", mf.Name, err)
	}
	m := &Mod{
		Manifest: mf,
		Status:   StatusRunning,
		proc:     cmd,
		stdin:    stdin,
		reader:   bufio.NewReaderSize(stdout, 1<<20),
		pending:  map[int]chan rpcResponse{},
		done:     make(chan struct{}),
		timeout:  timeout,
		log:      log,
	}
	go m.readLoop()
	go m.drainStderr(stderr)
	return m, nil
}

// modEnv builds the mod's environment: the harness's environment plus the
// variables the manifest names. Only names are in the manifest; the values
// come from the operator's environment.
func modEnv(mf Manifest) []string {
	env := os.Environ()
	extra := make([]string, 0, len(mf.Env))
	for name, src := range mf.Env {
		name = strings.TrimSpace(name)
		src = strings.TrimSpace(src)
		if name == "" {
			continue
		}
		if src == "" {
			src = name
		}
		value, ok := os.LookupEnv(src)
		if !ok || value == "" {
			continue
		}
		extra = append(extra, name+"="+value)
	}
	sort.Strings(extra)
	return append(env, extra...)
}

// request performs one JSON-RPC request and waits for the answer.
func (m *Mod) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	m.idMu.Lock()
	m.nextID++
	id := m.nextID
	m.idMu.Unlock()

	ch := make(chan rpcResponse, 1)
	m.pendMu.Lock()
	m.pending[id] = ch
	m.pendMu.Unlock()
	defer func() {
		m.pendMu.Lock()
		delete(m.pending, id)
		m.pendMu.Unlock()
	}()

	if err := m.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(m.timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-timer.C:
		// The stream is desynchronized: the process has to go, or the next
		// answer would be read as the answer to this one.
		err := fmt.Errorf("mod %q: %s timed out after %s", m.Manifest.Name, method, m.timeout)
		m.setState(StatusFailed, err)
		m.kill()
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, fmt.Errorf("mod %q exited", m.Manifest.Name)
	}
}

// notify sends a JSON-RPC notification.
func (m *Mod) notify(method string, params any) error {
	return m.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// write sends one line to the mod.
func (m *Mod) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if m.stdin == nil {
		return fmt.Errorf("mod %q is not running", m.Manifest.Name)
	}
	if _, err := m.stdin.Write(b); err != nil {
		return fmt.Errorf("mod %q: write: %w", m.Manifest.Name, err)
	}
	return nil
}

// readLoop reads the mod's answers, one JSON object per line.
func (m *Mod) readLoop() {
	defer m.finish(nil)
	for {
		line, err := m.reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			m.handleLine(line)
		}
		if err != nil {
			if err != io.EOF {
				m.finish(fmt.Errorf("mod %q: %w", m.Manifest.Name, err))
			}
			return
		}
	}
}

// finish closes the done channel once.
func (m *Mod) finish(err error) {
	m.doneOnce.Do(func() {
		close(m.done)
	})
}

// handleLine routes one answer to the waiting request.
func (m *Mod) handleLine(line []byte) {
	var env struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Result json.RawMessage  `json:"result"`
		Error  *rpcError        `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &env); err != nil {
		// A malformed answer is an error, never a panic and never a hang.
		m.failPending(fmt.Errorf("mod %q answered with a malformed line", m.Manifest.Name))
		return
	}
	if env.ID == nil {
		// A notification from the mod: nothing is waiting for it.
		return
	}
	var id int
	if err := json.Unmarshal(*env.ID, &id); err != nil {
		m.failPending(fmt.Errorf("mod %q answered with an invalid id", m.Manifest.Name))
		return
	}
	m.pendMu.Lock()
	ch, ok := m.pending[id]
	m.pendMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- rpcResponse{result: env.Result, err: env.Error}:
	default:
	}
}

// failPending answers every waiting request with an error.
func (m *Mod) failPending(err error) {
	m.pendMu.Lock()
	waiters := make([]chan rpcResponse, 0, len(m.pending))
	for _, ch := range m.pending {
		waiters = append(waiters, ch)
	}
	m.pendMu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- rpcResponse{err: &rpcError{Code: -32603, Message: err.Error()}}:
		default:
		}
	}
}

// drainStderr logs the mod's stderr. The mod's own log is never part of the
// protocol and never reaches the model.
func (m *Mod) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if m.log == nil {
			continue
		}
		line := strings.TrimSpace(sc.Text())
		if len(line) > 500 {
			line = line[:500] + "…"
		}
		if line != "" {
			m.log("mod_stderr", map[string]any{"mod": m.Manifest.Name, "line": line})
		}
	}
}

// kill stops the process.
func (m *Mod) kill() {
	if m.proc != nil && m.proc.Process != nil {
		_ = m.proc.Process.Kill()
	}
}

// Close stops the mod. Safe to call twice.
func (m *Mod) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		if status, _ := m.State(); status == StatusRunning {
			m.setState(StatusStopped, nil)
		}
		if m.proc == nil {
			// The mod never started: it was untrusted, or it failed before
			// the process existed. There is nothing to stop.
			return
		}
		if m.stdin != nil {
			_ = m.stdin.Close()
		}
		select {
		case <-m.done:
		case <-time.After(2 * time.Second):
			m.kill()
			<-m.done
		}
		wait := make(chan struct{})
		go func() {
			_ = m.proc.Wait()
			close(wait)
		}()
		select {
		case <-wait:
		case <-time.After(2 * time.Second):
			m.kill()
			<-wait
		}
		m.finish(nil)
	})
}

// handshake runs the initialize exchange and checks the mod against the
// manifest.
func (m *Mod) handshake(ctx context.Context) error {
	raw, err := m.request(ctx, MethodInitialize, map[string]any{
		"protocolVersion": protocolVersion,
		"client":          map[string]any{"name": clientName, "version": clientVersion},
		"mod":             map[string]any{"name": m.Manifest.Name, "version": m.Manifest.Version},
		"capabilities":    map[string]any{},
	})
	if err != nil {
		return err
	}
	var res struct {
		ProtocolVersion string              `json:"protocolVersion"`
		Tools           []advertisedTool    `json:"tools"`
		Commands        []advertisedCommand `json:"commands"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("mod %q answered initialize with a malformed result: %w", m.Manifest.Name, err)
	}
	if err := m.verifyTools(res.Tools); err != nil {
		return err
	}
	if err := m.verifyCommands(res.Commands); err != nil {
		return err
	}
	return m.notify(MethodInitialized, map[string]any{})
}

// advertisedTool is a tool the mod claims in the handshake.
type advertisedTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// advertisedCommand is a command the mod claims in the handshake.
type advertisedCommand struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argumentHint"`
}

// verifyTools checks the mod's advertised tools against the manifest.
//
// The manifest is the contract: a declared tool the mod does not provide is
// a broken mod (it is dropped), and a tool the mod offers but the manifest
// does not declare is never exposed.
func (m *Mod) verifyTools(advertised []advertisedTool) error {
	declared := map[string]bool{}
	for _, spec := range m.Manifest.Tools {
		declared[spec.Name] = true
	}
	have := map[string]bool{}
	for _, t := range advertised {
		have[t.Name] = true
	}
	if len(m.Manifest.Tools) > 0 {
		for _, spec := range m.Manifest.Tools {
			if !have[spec.Name] {
				return fmt.Errorf("the manifest declares the tool %q but the mod does not provide it", spec.Name)
			}
		}
	}
	for _, t := range advertised {
		if !declared[t.Name] {
			m.logf("mod_extra_tool", map[string]any{
				"mod": m.Manifest.Name, "tool": t.Name,
				"reason": "not declared in the manifest: not exposed",
			})
		}
	}
	m.Tools = make([]string, 0, len(m.Manifest.Tools))
	for _, spec := range m.Manifest.Tools {
		m.Tools = append(m.Tools, spec.Name)
	}
	return nil
}

// verifyCommands checks the mod's advertised commands against the manifest.
func (m *Mod) verifyCommands(advertised []advertisedCommand) error {
	declared := map[string]bool{}
	for _, c := range m.Manifest.Commands {
		declared[c.Name] = true
	}
	have := map[string]bool{}
	for _, c := range advertised {
		have[c.Name] = true
	}
	if len(m.Manifest.Commands) > 0 {
		for _, c := range m.Manifest.Commands {
			if !have[c.Name] {
				return fmt.Errorf("the manifest declares the command %q but the mod does not provide it", c.Name)
			}
		}
	}
	for _, c := range advertised {
		if !declared[c.Name] {
			m.logf("mod_extra_command", map[string]any{
				"mod": m.Manifest.Name, "command": c.Name,
				"reason": "not declared in the manifest: not exposed",
			})
		}
	}
	return nil
}

// logf records one mod event.
func (m *Mod) logf(kind string, v any) {
	if m == nil || m.log == nil {
		return
	}
	m.log(kind, v)
}

// ---------- the registry ----------

// ModCommand is one slash command, with the mod that provides it.
type ModCommand struct {
	// Mod is the name of the mod that provides the command.
	Mod string
	// Command is the command itself.
	Command CommandSpec
}

// Manager owns the mods that are running.
type Manager struct {
	mods    []*Mod
	timeout time.Duration
	log     func(kind string, v any)
}

// NewManager returns an empty manager. A zero timeout means DefaultTimeout.
func NewManager(timeout time.Duration, log func(kind string, v any)) *Manager {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Manager{timeout: timeout, log: log}
}

// active is the process-wide registry the package-level Discover uses.
var active = NewManager(0, nil)

// Active returns the process-wide mod registry.
func Active() *Manager { return active }

// logf records one registry event.
func (m *Manager) logf(kind string, v any) {
	if m == nil || m.log == nil {
		return
	}
	m.log(kind, v)
}

// Discover starts the mods the manifests name and keeps the ones that
// answered the handshake.
//
// A mod only runs in a trusted workspace. An untrusted mod is kept in the
// list (with the reason) and never started. A mod that fails to start, hangs
// or lies about its tools is dropped with an error; the other mods stay
// usable.
//
// The returned error reports what went wrong. It is telemetry, not a reason
// to take the session down.
func (m *Manager) Discover(manifests []Manifest, trusted bool) error {
	if m == nil {
		return errors.New("mods: no manager")
	}
	var errs []error
	for _, mf := range manifests {
		mod, err := m.start(mf, trusted)
		if mod != nil {
			m.mods = append(m.mods, mod)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// start launches one mod and checks it against its manifest.
//
// The mod is always returned (so the caller can list it, running or not);
// the error says why it is not running.
func (m *Manager) start(mf Manifest, trusted bool) (*Mod, error) {
	if !trusted {
		mod := &Mod{Manifest: mf, Status: StatusUntrusted, Err: ErrUntrusted}
		m.logf("mod_trust_denied", map[string]any{
			"mod":    mf.Name,
			"reason": ErrUntrusted.Error(),
		})
		return mod, fmt.Errorf("mod %q: %w", mf.Name, ErrUntrusted)
	}
	mod, err := start(mf, m.timeout, m.log)
	if err != nil {
		return &Mod{Manifest: mf, Status: StatusFailed, Err: err}, err
	}
	// The mod's own request timeout already bounds the handshake (request
	// starts a timer at m.timeout), so no second context deadline is needed
	// here. Adding one created a race: with two identical deadlines, select
	// randomly reports "timed out" or "context deadline exceeded" for the same
	// hang, which made the result nondeterministic under load.
	err = mod.handshake(context.Background())
	if err != nil {
		mod.setState(StatusFailed, err)
		mod.Close()
		m.logf("mod_error", map[string]any{"mod": mf.Name, "error": err.Error()})
		return mod, fmt.Errorf("mod %q: %w", mf.Name, err)
	}
	m.logf("mod_started", map[string]any{
		"mod":   mf.Name,
		"tools": len(mf.Tools),
	})
	return mod, nil
}

// Mods returns the mods the manager knows about, running or not.
func (m *Manager) Mods() []*Mod {
	if m == nil {
		return nil
	}
	return append([]*Mod(nil), m.mods...)
}

// Definitions returns the tool schemas of every running mod.
func (m *Manager) Definitions() []provider.Tool {
	if m == nil {
		return nil
	}
	var out []provider.Tool
	for _, mod := range m.mods {
		out = append(out, mod.Definitions()...)
	}
	return out
}

// Has reports whether a namespaced mod tool is routed. Only a running mod
// routes: an untrusted or broken mod exposes nothing.
func (m *Manager) Has(name string) bool {
	if m == nil {
		return false
	}
	for _, mod := range m.mods {
		status, _ := mod.State()
		if status != StatusRunning {
			continue
		}
		if _, ok := mod.toolSpec(name); ok {
			return true
		}
	}
	return false
}

// Call runs a tool of one of the mods. The name is the namespaced one the
// model sees (mod__<mod>__<tool>).
func (m *Manager) Call(ctx context.Context, name string, args string) (string, bool, error) {
	if m == nil {
		return "", true, errors.New("mods: no manager")
	}
	for _, mod := range m.mods {
		if _, ok := mod.toolSpec(name); !ok {
			continue
		}
		return mod.Call(ctx, name, args)
	}
	return "", true, fmt.Errorf("unknown mod tool %q", name)
}

// Commands returns the slash commands of every running mod.
func (m *Manager) Commands() []ModCommand {
	if m == nil {
		return nil
	}
	var out []ModCommand
	for _, mod := range m.mods {
		status, _ := mod.State()
		if status != StatusRunning {
			continue
		}
		for _, c := range mod.Commands() {
			out = append(out, ModCommand{Mod: mod.Manifest.Name, Command: c})
		}
	}
	return out
}

// NotifyEvent hands a lifecycle event to the mods that observe it.
func (m *Manager) NotifyEvent(event string, data map[string]any) error {
	if m == nil {
		return errors.New("mods: no manager")
	}
	var errs []error
	for _, mod := range m.mods {
		status, _ := mod.State()
		if status != StatusRunning {
			continue
		}
		for _, e := range mod.Events() {
			if e == event {
				if err := mod.NotifyEvent(event, data); err != nil {
					errs = append(errs, err)
				}
				break
			}
		}
	}
	return errors.Join(errs...)
}

// Close stops every mod. Safe to call twice.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, mod := range m.mods {
		mod.Close()
	}
	m.mods = nil
}

// Discover starts the given mods in the process-wide registry. It is the
// package-level shorthand for Manager.Discover.
func Discover(manifests []Manifest, trusted bool) error {
	return active.Discover(manifests, trusted)
}

// ---------- schema translation ----------

// schemaOf turns a manifest's parameter schema into a provider schema.
func schemaOf(params map[string]any) provider.ToolParameters {
	out := provider.ToolParameters{Type: "object", Properties: map[string]provider.ToolProp{}}
	if len(params) == 0 {
		return out
	}
	if t, ok := params["type"].(string); ok && t != "" {
		out.Type = t
	}
	if req, ok := params["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
	}
	if props, ok := params["properties"].(map[string]any); ok {
		for k, v := range props {
			out.Properties[k] = propOf(v)
		}
	}
	return out
}

// propOf turns one property schema into a provider schema.
func propOf(v any) provider.ToolProp {
	obj, ok := v.(map[string]any)
	if !ok {
		return provider.ToolProp{Type: "string"}
	}
	p := provider.ToolProp{Type: propType(obj["type"]), Description: propString(obj["description"])}
	if items, ok := obj["items"]; ok {
		ip := propOf(items)
		p.Items = &ip
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		p.Properties = map[string]provider.ToolProp{}
		for k, v := range props {
			p.Properties[k] = propOf(v)
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
	if def, ok := obj["default"]; ok {
		p.Default = def
	}
	return p
}

// propType resolves a property's type, defaulting to string.
func propType(v any) string {
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

// propString resolves a property's description.
func propString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// decodeContent reads a mod's answer to a tool or command call.
func decodeContent(raw json.RawMessage) (string, bool, error) {
	var res struct {
		Content []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", true, fmt.Errorf("the mod answered with a malformed result: %w", err)
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
		out = "(the mod returned no text content)"
	}
	return out, res.IsError, nil
}
