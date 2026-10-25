package acp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdioGrace is how long Close waits for an agent to exit after its stdin is
// closed before the process is killed.
const stdioGrace = 2 * time.Second

// Start launches cmd as an ACP server and returns a client speaking to it over
// its stdin/stdout. The client owns the child from here on: Close (or a Run
// that returns) always terminates it. The handshake is NOT performed here —
// call Handshake, or use Run for the whole lifecycle.
func Start(cmd *exec.Cmd, opts Options) (*Client, error) {
	if cmd == nil {
		return nil, fmt.Errorf("acp: no command")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %s: %w", strings.Join(cmd.Args, " "), err)
	}
	if opts.Name == "" {
		opts.Name = cmdLabel(cmd)
	}
	tr := &stdioTransport{cmd: cmd, in: in, out: out}
	c := NewClient(tr, opts)
	go c.drainStderr(errPipe)
	return c, nil
}

// StartCommand parses an `acp:`-style command line and starts it.
func StartCommand(cmdline string, opts Options) (*Client, error) {
	cmd, err := ParseCommand(cmdline)
	if err != nil {
		return nil, err
	}
	return Start(cmd, opts)
}

// RunOptions configures one complete agent run.
type RunOptions struct {
	Options
	// Dir is the child's working directory and the session cwd. Empty means
	// the caller's.
	Dir string
	// Env adds environment variables for the child, on top of os.Environ().
	Env map[string]string
	// CancelGrace is how long a cancelled run waits after session/cancel
	// before the process is killed. Default CancelTimeout.
	CancelGrace time.Duration
}

// Run is the whole worker lifecycle in one call: start the agent, handshake,
// send one prompt, return the agent's final message. The child process is
// always stopped on the way out — politely on success, with session/cancel
// then kill when ctx is cancelled or the agent misbehaves.
func Run(ctx context.Context, cmdline, prompt string, opts RunOptions) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	cmd, err := ParseCommand(cmdline)
	if err != nil {
		return Result{}, err
	}
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	if len(opts.Env) > 0 {
		env := os.Environ()
		for k, v := range opts.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	if opts.Name == "" {
		opts.Name = cmdLabel(cmd)
	}
	c, err := Start(cmd, opts.Options)
	if err != nil {
		return Result{}, err
	}

	grace := opts.CancelGrace
	if grace <= 0 {
		grace = CancelTimeout
	}

	// Watchdog: a cancelled run tells the agent to stop before the process is
	// killed, so an agent that respects session/cancel gets to wind its own
	// turn down. A turn that finished on its own never triggers it.
	runCtx, stopRun := context.WithCancel(ctx)
	var turnDone atomic.Bool
	settled := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		select {
		case <-settled:
		case <-runCtx.Done():
			if turnDone.Load() {
				return
			}
			c.CancelAndWait(ctx, grace)
		}
	}()
	defer func() {
		turnDone.Store(true)
		close(settled)
		stopRun()
		<-watchdogDone
		c.Close()
	}()

	if _, err := c.Handshake(runCtx, opts.Dir); err != nil {
		return Result{}, err
	}
	return c.Prompt(runCtx, prompt)
}

// ParseCommand splits an ACP command line (`acp:<command> [args...]` minus the
// prefix) into an exec.Cmd. Quoting follows shell-ish rules: single and double
// quotes group, a backslash escapes the next character. The base command must
// resolve on PATH — an unresolvable agent is an error, not a spawn of
// something arbitrary.
func ParseCommand(cmdline string) (*exec.Cmd, error) {
	fields, err := splitCommand(cmdline)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("acp: empty command")
	}
	path, err := exec.LookPath(fields[0])
	if err != nil {
		return nil, fmt.Errorf("acp: agent command %q not found on PATH: %w", fields[0], err)
	}
	return exec.Command(path, fields[1:]...), nil
}

func splitCommand(cmdline string) ([]string, error) {
	runes := []rune(cmdline)
	var (
		fields  []string
		cur     strings.Builder
		quote   rune
		started bool
	)
	flush := func() {
		if started {
			fields = append(fields, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
			case r == '\\' && quote == '"' && i+1 < len(runes):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			started = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("acp: unbalanced quote in command %q", cmdline)
	}
	flush()
	return fields, nil
}

// cmdLabel is the human name of an agent: the command and its arguments.
func cmdLabel(cmd *exec.Cmd) string {
	if cmd == nil || len(cmd.Args) == 0 {
		return "agent"
	}
	return strings.Join(cmd.Args, " ")
}

// ---------- stdio transport ----------

// stdioTransport is the child-process transport: writes go to the agent's
// stdin, reads come from its stdout. It implements the processTransport
// interface Close looks for, so the client can shut the child down politely
// (stdin close → wait → kill) instead of just dropping the connection.
type stdioTransport struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.Reader
	once sync.Once
}

func (t *stdioTransport) Read(p []byte) (int, error)  { return t.out.Read(p) }
func (t *stdioTransport) Write(p []byte) (int, error) { return t.in.Write(p) }

// Close closes the agent's stdin, which is the polite way to tell it the
// session is over. The client escalates to Process/Kill/Wait itself.
func (t *stdioTransport) Close() error { return t.closeStdin() }

func (t *stdioTransport) closeStdin() error {
	var err error
	t.once.Do(func() { err = t.in.Close() })
	return err
}

func (t *stdioTransport) process() *os.Process { return t.cmd.Process }

func (t *stdioTransport) kill() {
	if p := t.cmd.Process; p != nil {
		_ = p.Kill()
	}
}

func (t *stdioTransport) wait() error { return t.cmd.Wait() }

// processTransport is implemented by transports that own a child process.
type processTransport interface {
	Transport
	process() *os.Process
	kill()
	wait() error
}

// drainStderr keeps the agent's stderr from blocking on a full pipe and keeps
// the last few lines for diagnostics. Stderr is never parsed and never
// executed: it is a log.
func (c *Client) drainStderr(r io.Reader) {
	if r == nil {
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		c.appendStderr(truncate(line, stderrLineChars))
		c.logf("acp_stderr", map[string]any{"agent": c.name, "line": truncate(line, stderrLineChars)})
	}
}

// closeStdio is Close's path for a child-process transport: stdin first, kill
// only if the agent will not go away.
func (c *Client) closeStdio(pt processTransport) {
	_ = pt.Close()
	select {
	case <-c.done:
	case <-time.After(stdioGrace):
		pt.kill()
		select {
		case <-c.done:
		case <-time.After(stdioGrace):
		}
	}
	c.finish(nil)
	waited := make(chan struct{})
	go func() {
		_ = pt.wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(stdioGrace):
		pt.kill()
		<-waited
	}
}
