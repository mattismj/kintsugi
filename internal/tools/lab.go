// The lab tools: `run_in_lab` and the lock guard (M8 remainder, DESIGN §6.3,
// §16.9).
//
// The lab is where the work is executed. The workspace may hold real
// dual-use security work; the lab is a disposable, compose-managed fixture
// environment the agent can run tooling against. The rule this file enforces
// is simple and absolute:
//
//   - While the lab lock exists, a command may only run INSIDE the lab.
//     The host `bash` tool refuses; `run_in_lab` executes in a container.
//
// `run_in_lab` never runs the model's command on the host. The only thing
// that runs on the host is the compose client, and the command it carries is
// an argument of `sh -lc` inside a service — never an argument of a host
// shell.
//
// The compose file, the project name and the runtime come from the
// operator's configuration (and from the lock the operator's `kintsugi lab up`
// wrote). A tool argument can name the service and the command; it can
// never name where the lab is.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"kintsugi/internal/audit"
	"kintsugi/internal/lab"
	"kintsugi/internal/provider"
	"kintsugi/internal/redact"
)

const (
	// labOutputCap bounds the output one lab command may return, the same
	// bound the bash tool uses.
	labOutputCap = 32 << 10
	// labDefaultTimeout is the default timeout of a lab command, in seconds.
	labDefaultTimeout = 60
	// labMaxTimeout is the largest timeout a lab command may ask for.
	labMaxTimeout = 300
)

// labRule labels the audit records the lab boundary produces.
const labRule = "lab"

// runInLabTool is the schema the model sees.
//
// The description says the one thing that matters: the command runs in the
// lab, not on the host.
func runInLabTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "run_in_lab",
			Description: "Run a command inside the lab environment (a docker/podman compose service) and return its output and exit code. " +
				"The command never runs on the host: it runs in the lab container, with the network the lab's compose file declares. " +
				"Requires a lab that is up (`kintsugi lab up`); while a lab is up the host shell is refused.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"command"},
				Properties: map[string]provider.ToolProp{
					"command":   {Type: "string", Description: "Shell command string, run with `sh -lc` inside the lab service."},
					"service":   {Type: "string", Description: "Lab service to run in (default: the first configured service)."},
					"timeout_s": {Type: "integer", Description: "Timeout in seconds (default 60, max 300)."},
				},
			},
		},
	}
}

// labExec runs the compose argv.
//
// It is a package-level seam so the tests can assert the argv without a
// container runtime: the command that would run in the lab never runs on the
// test machine either.
var labExec = func(ctx context.Context, argv []string, dir string, env []string) (string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return string(out), 0, err
		}
	}
	return string(out), code, nil
}

// labHostGuard refuses host command execution while a lab is up.
//
// This is the lab.lock guarantee (DESIGN §6.3): the lock exists to prevent
// accidental deploy-like operations on the host. While it exists, the lab is
// the only place a command may run.
func (e *Executor) labHostGuard() error {
	if strings.TrimSpace(e.Workspace) == "" {
		return nil
	}
	lock, err := lab.ReadLock(e.Workspace)
	if err != nil {
		return err
	}
	if lock == nil {
		return nil
	}
	return fmt.Errorf(
		"a lab is up (lock %s): host commands are refused while the lab is up — run the command inside the lab with run_in_lab, or bring the lab down with `kintsugi lab down`",
		lab.LockPath(e.Workspace))
}

// runInLab runs a command inside the lab.
//
// The posture it inherits:
//
//   - The sandbox. The command runs inside a container, which is a stronger
//     boundary than bubblewrap for that command; the compose client on the
//     host is not wrapped, because it needs the runtime socket and starts
//     nothing of the model's.
//   - The network. The lab's network is whatever the compose file declares.
//     The harness adds no egress of its own, and the host's offline mode
//     governs the harness, not the lab.
//   - The scope. A command outside the operator's declared scope does not run
//     in the lab either.
//   - The permission matrix. The lab is not a place where permission stops
//     mattering.
func (e *Executor) runInLab(ctx context.Context, args string) Result {
	var a struct {
		Command  string `json:"command"`
		Service  string `json:"service"`
		TimeoutS int    `json:"timeout_s"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	command := strings.TrimSpace(a.Command)
	if command == "" {
		return Result{Content: `{"error":"run_in_lab: missing required property 'command'"}`, Failed: true}
	}

	// The lock is the harness's own record that a lab is up. Without it
	// there is nowhere to run: the command must not fall back to the host.
	lock, err := lab.ReadLock(e.Workspace)
	if err != nil {
		return failJSON(err)
	}
	if lock == nil {
		msg := "no lab is up — start one with `kintsugi lab up`"
		e.audit("run_in_lab", command, audit.DecisionDeny, msg, labRule)
		return Result{
			Content: fmt.Sprintf(`{"error":%q}`, msg),
			Failed:  true,
		}
	}
	if !lab.SameWorkspace(lock.Workspace, e.Workspace) {
		msg := fmt.Sprintf("the lab lock at %s belongs to another workspace (%s)",
			lab.LockPath(e.Workspace), lock.Workspace)
		e.audit("run_in_lab", command, audit.DecisionDeny, msg, labRule)
		return failJSON(errors.New(msg))
	}

	service, err := e.labService(a.Service, lock)
	if err != nil {
		return failJSON(err)
	}

	// The declared scope is a floor everywhere in the harness, the lab
	// included: a command outside the declared scope does not run anywhere.
	if err := e.scopeCommand(command); err != nil {
		e.audit("run_in_lab", command, audit.DecisionDeny, err.Error(), audit.RuleScope)
		return failJSON(err)
	}

	// The permission matrix still applies: the lab is not a place where
	// permission stops mattering.
	if err := e.authorize("run_in_lab", command, "run_in_lab: "+service+": "+clip(command, 120)); err != nil {
		return failJSON(err)
	}

	timeout := a.TimeoutS
	if timeout <= 0 {
		timeout = labDefaultTimeout
	}
	if timeout > labMaxTimeout {
		timeout = labMaxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// The argv is fixed by the lab, not by the model: runtime, project and
	// service are the operator's, and the command is the argument of `sh -lc`
	// inside the service.
	argv := lab.ExecArgv(lock.Runtime, lock.Project, service, command)
	composeFile := lab.ComposePath(e.Workspace, lock.ComposeFile)
	env := lab.ExecEnv(composeFile, lock.Project)

	out, code, err := labExec(runCtx, argv, e.Workspace, env)
	if err != nil {
		return failJSON(fmt.Errorf("run_in_lab: %v", err))
	}
	body := redact.Redact(out)
	if len(body) > labOutputCap {
		body = body[:labOutputCap] + "\n…[truncated]"
	}
	if e.Log != nil {
		e.Log("run_in_lab", map[string]any{
			"service": service, "project": lock.Project, "runtime": string(lock.Runtime), "exit": code,
		})
	}
	res := fmt.Sprintf("%s\n[exit %d]", strings.TrimRight(body, "\n"), code)
	return Result{Content: res, Failed: code != 0}
}

// labService resolves the service a lab command runs in.
//
// The operator's configuration decides: an explicit service must be one of
// the configured services, and the default is the first configured one.
func (e *Executor) labService(requested string, lock *lab.Lock) (string, error) {
	configured := []string{}
	if e.Lab != nil {
		configured = e.Lab.Services
	}
	service := strings.TrimSpace(requested)
	if service == "" {
		switch {
		case len(configured) > 0:
			service = configured[0]
		case len(lock.Services) > 0:
			service = lock.Services[0]
		default:
			return "", fmt.Errorf("no lab service is configured — set [lab] services in kintsugi.toml")
		}
	}
	if !validServiceName(service) {
		return "", fmt.Errorf("invalid lab service name %q", service)
	}
	if len(configured) > 0 && !containsString(configured, service) {
		return "", fmt.Errorf("lab service %q is not one of the configured services (%s)",
			service, strings.Join(configured, ", "))
	}
	return service, nil
}

// validServiceName reports whether a service name is a plain compose service
// name. The name reaches an argv, so it may not carry anything else.
func validServiceName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// containsString reports whether a list holds a value.
func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
