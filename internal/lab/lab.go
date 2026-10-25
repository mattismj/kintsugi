// Package lab is the disposable lab harness (M8 remainder, DESIGN §6.3).
//
// The lab is a safety boundary first and a convenience second. The workspace
// it serves may hold real dual-use security work; the lab is where that work
// is *executed* — a compose-managed fixture environment the agent can run
// tooling against, without the tooling ever touching the host. Everything
// stays local: the lab has no egress beyond what the operator's compose file
// declares.
//
// Three pieces make the guarantee hold:
//
//   - Up brings the compose project up and writes a lock file
//     (<workspace>/.kintsugi/lab.lock) naming the runtime, the project and
//     the compose file.
//   - While the lock exists, the tool layer refuses to run commands on the
//     host: the lab is the only place a command may run.
//   - Down tears the project down (containers and volumes) and removes the
//     lock, so the boundary is lifted only when the lab is really gone.
//
// The runtime, the compose file and the project name come from the operator's
// configuration. A model never names them: the tool arguments that reach a
// container are the command and the service, nothing else.
package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kintsugi/internal/blueprints"
	"kintsugi/internal/config"
)

// Runtime names a container runtime the lab can drive.
type Runtime string

const (
	// RuntimeDocker is docker with its compose plugin.
	RuntimeDocker Runtime = "docker"
	// RuntimePodman is podman with its compose plugin.
	RuntimePodman Runtime = "podman"
	// RuntimeNone is reported when no runtime is available.
	RuntimeNone Runtime = "none"
)

// RuntimeAuto is the configuration value that asks for detection.
const RuntimeAuto = "auto"

const (
	// DefaultComposeFile is the compose file the lab uses when the operator
	// does not name one.
	DefaultComposeFile = "lab/docker-compose.yml"
	// DefaultProject is the compose project name the lab uses when the
	// operator does not name one.
	DefaultProject = "kintsugi-lab"
)

const (
	// composeTimeout bounds one compose call.
	composeTimeout = 180 * time.Second
	// detectTimeout bounds the "does this runtime answer compose version"
	// probe.
	detectTimeout = 15 * time.Second
	// lockDir is the workspace directory the harness owns.
	lockDir = ".kintsugi"
	// lockFile is the lock's name inside that directory.
	lockFile = "lab.lock"
)

// Config is the operator's lab configuration. Every field comes from the
// [lab] config block; nothing here is ever taken from a tool argument.
type Config struct {
	// Runtime is the container runtime: "docker", "podman" or "auto".
	Runtime Runtime
	// ComposeFile is the compose file, relative to the workspace.
	ComposeFile string
	// Project is the compose project name.
	Project string
	// Services are the services to bring up. Empty means every service the
	// compose file declares.
	Services []string
	// Network is the lab network name. It is recorded in the lock; the
	// network itself is whatever the compose file declares.
	Network string
}

// State is what the harness knows about the lab.
type State struct {
	// Up reports whether a lab is up (the lock exists).
	Up bool
	// Runtime is the runtime the lab runs on.
	Runtime Runtime
	// Project is the compose project name.
	Project string
	// Services are the services the lab runs.
	Services []string
	// StartedAt is when the lab came up.
	StartedAt time.Time
	// Locked reports whether the lock file exists.
	Locked bool
}

// Lock is the record `kintsugi lab up` leaves behind. It is the harness's
// memory of a live lab: while it exists, commands may only run inside the
// lab.
type Lock struct {
	// Project is the compose project name.
	Project string `json:"project"`
	// Runtime is the container runtime.
	Runtime Runtime `json:"runtime"`
	// ComposeFile is the compose file, relative to the workspace.
	ComposeFile string `json:"compose_file"`
	// StartedAt is when the lab came up (UTC).
	StartedAt time.Time `json:"started_at"`
	// Workspace is the absolute workspace the lock belongs to. A lock is
	// never removed for another workspace.
	Workspace string `json:"workspace"`
	// Services are the services the lab runs.
	Services []string `json:"services,omitempty"`
	// Network is the lab network name, when the operator named one.
	Network string `json:"network,omitempty"`
}

// Report describes what a teardown removed.
type Report struct {
	// LockPath is where the lock file is (or was).
	LockPath string
	// LockFound reports whether a lock file was present.
	LockFound bool
	// LockRemoved reports whether the lock file was removed.
	LockRemoved bool
	// Project is the compose project that was torn down.
	Project string
	// Services are the services the lab was running.
	Services []string
	// Runtime is the runtime the lab was running on.
	Runtime Runtime
	// ComposeFile is the compose file the lab was using.
	ComposeFile string
	// Teardown reports whether the compose project was brought down.
	Teardown bool
	// SkipReason explains why the compose project was not brought down, when
	// it was not.
	SkipReason string
}

// FromConfig converts the [lab] config block into a lab configuration.
func FromConfig(c config.Lab) Config {
	return Config{
		Runtime:     Runtime(strings.ToLower(strings.TrimSpace(c.Runtime))),
		ComposeFile: strings.TrimSpace(c.ComposeFile),
		Project:     strings.TrimSpace(c.Project),
		Services:    append([]string(nil), c.Services...),
		Network:     strings.TrimSpace(c.Network),
	}.normalized()
}

// normalized returns a copy with the defaults applied.
func (c Config) normalized() Config {
	c.Runtime = Runtime(strings.ToLower(strings.TrimSpace(string(c.Runtime))))
	if c.Runtime == "" {
		c.Runtime = RuntimeAuto
	}
	c.ComposeFile = strings.TrimSpace(c.ComposeFile)
	if c.ComposeFile == "" {
		c.ComposeFile = DefaultComposeFile
	}
	c.Project = strings.TrimSpace(c.Project)
	if c.Project == "" {
		c.Project = DefaultProject
	}
	c.Network = strings.TrimSpace(c.Network)
	services := make([]string, 0, len(c.Services))
	for _, s := range c.Services {
		if s = strings.TrimSpace(s); s != "" {
			services = append(services, s)
		}
	}
	c.Services = services
	return c
}

// LockPath is the lock file's path for a workspace.
func LockPath(ws string) string {
	return filepath.Join(ws, lockDir, lockFile)
}

// SameWorkspace reports whether two workspace paths name the same directory.
func SameWorkspace(a, b string) bool {
	return filepath.Clean(absWorkspace(a)) == filepath.Clean(absWorkspace(b))
}

// absWorkspace returns the absolute form of a workspace path.
func absWorkspace(ws string) string {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return ""
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return filepath.Clean(ws)
	}
	return abs
}

// ReadLock reads the lock file.
//
// A missing lock is not an error: it is the ordinary "no lab is up" answer.
func ReadLock(ws string) (*Lock, error) {
	path := LockPath(ws)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lab: read lock %s: %w", path, err)
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("lab: parse lock %s: %w", path, err)
	}
	return &l, nil
}

// WriteLock writes the lock file. The write is atomic (temp sibling plus
// rename), so a reader never sees half a lock.
func WriteLock(ws string, l Lock) error {
	path := LockPath(ws)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("lab: %w", err)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("lab: encode lock: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	// The lock names the workspace and the runtime: keep it to the operator.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("lab: write lock: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("lab: write lock: %w", err)
	}
	return nil
}

// RemoveLock removes the lock file. A missing lock is not an error: Down is
// safe to call twice.
func RemoveLock(ws string) error {
	if err := os.Remove(LockPath(ws)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lab: remove lock: %w", err)
	}
	return nil
}

// Detect finds the container runtime the lab can drive.
//
// "auto" tries docker first, then podman. A runtime counts as available when
// its binary is on PATH *and* it answers `compose version`: the compose
// plugin is what the lab actually needs, and a docker binary without it is
// not a lab.
//
// The error is the reason, always: the lab never pretends it can bring
// something up.
func Detect(cfg Config) (Runtime, error) {
	want := strings.ToLower(strings.TrimSpace(string(cfg.Runtime)))
	var candidates []Runtime
	switch want {
	case "", string(RuntimeAuto):
		candidates = []Runtime{RuntimeDocker, RuntimePodman}
	case string(RuntimeDocker), string(RuntimePodman):
		candidates = []Runtime{Runtime(want)}
	default:
		return RuntimeNone, fmt.Errorf("lab: unknown runtime %q (want auto|docker|podman)", cfg.Runtime)
	}

	var reasons []string
	for _, rt := range candidates {
		path, err := exec.LookPath(string(rt))
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s is not on PATH", rt))
			continue
		}
		if err := composeAvailable(path); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s has no compose support (%v)", rt, err))
			continue
		}
		return rt, nil
	}
	return RuntimeNone, fmt.Errorf("lab: no container runtime with compose support found: %s",
		strings.Join(reasons, "; "))
}

// composeAvailable asks the runtime whether its compose plugin answers.
func composeAvailable(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "compose", "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", strings.TrimSpace(msg))
	}
	return nil
}

// Up brings the lab up.
//
// The compose file and the project name come from the operator's
// configuration. The compose client is the only thing that runs on the host;
// the fixtures live in the containers it starts.
func Up(ctx context.Context, ws string, cfg Config) (State, error) {
	cfg = cfg.normalized()

	rt, err := Detect(cfg)
	if err != nil {
		return State{}, err
	}

	composePath, _, err := resolveComposeFile(ws, cfg.ComposeFile)
	if err != nil {
		return State{}, err
	}

	argv := []string{string(rt), "compose", "-p", cfg.Project, "-f", composePath, "up", "-d"}
	argv = append(argv, cfg.Services...)
	out, err := runCompose(ctx, argv, ws)
	if err != nil {
		return State{}, fmt.Errorf("lab: compose up failed: %s", strings.TrimSpace(out+"\n"+err.Error()))
	}

	started := time.Now().UTC()
	// An already-running lab keeps its start time: the second `up` did not
	// restart it.
	if prev, err := ReadLock(ws); err == nil && prev != nil && prev.Project == cfg.Project && !prev.StartedAt.IsZero() {
		started = prev.StartedAt
	}
	lock := Lock{
		Project:     cfg.Project,
		Runtime:     rt,
		ComposeFile: cfg.ComposeFile,
		StartedAt:   started,
		Workspace:   absWorkspace(ws),
		Services:    cfg.Services,
		Network:     cfg.Network,
	}
	if err := WriteLock(ws, lock); err != nil {
		return State{}, err
	}
	return State{
		Up:        true,
		Runtime:   rt,
		Project:   cfg.Project,
		Services:  cfg.Services,
		StartedAt: started,
		Locked:    true,
	}, nil
}

// Down tears the lab down: the compose project goes away with its volumes,
// and the lock goes with it.
//
// Down is safe to call twice, and it refuses to remove a lock that belongs
// to another workspace.
func Down(ctx context.Context, ws string, cfg Config) error {
	_, err := DownReport(ctx, ws, cfg)
	return err
}

// DownReport is Down with the details of what it removed, so the CLI can
// report the teardown instead of guessing.
func DownReport(ctx context.Context, ws string, cfg Config) (Report, error) {
	cfg = cfg.normalized()
	rep := Report{
		LockPath:    LockPath(ws),
		Project:     cfg.Project,
		ComposeFile: cfg.ComposeFile,
		Runtime:     RuntimeNone,
	}

	lock, err := ReadLock(ws)
	if err != nil {
		return rep, err
	}
	if lock != nil {
		rep.LockFound = true
		if !SameWorkspace(lock.Workspace, ws) {
			return rep, fmt.Errorf("lab: the lock at %s belongs to another workspace (%s); refusing to remove it",
				rep.LockPath, lock.Workspace)
		}
		rep.Project = lock.Project
		rep.Runtime = lock.Runtime
		rep.ComposeFile = lock.ComposeFile
		rep.Services = lock.Services
	}

	rt := rep.Runtime
	if rt == "" || rt == RuntimeNone {
		if detected, derr := Detect(cfg); derr == nil {
			rt = detected
		}
	}
	rep.Runtime = rt

	composePath := ComposePath(ws, rep.ComposeFile)
	switch {
	case rt == RuntimeNone:
		rep.SkipReason = "no container runtime with compose support is available"
	case rt != RuntimeNone && !runtimeOnPath(rt):
		rep.SkipReason = fmt.Sprintf("the %s runtime is not on PATH any more", rt)
	case composePath == "":
		rep.SkipReason = "no compose file is configured"
	case !fileExists(composePath):
		rep.SkipReason = "the compose file " + composePath + " is gone"
	default:
		argv := []string{string(rt), "compose", "-p", rep.Project, "-f", composePath, "down", "-v"}
		out, terr := runCompose(ctx, argv, ws)
		rep.Teardown = true
		if terr != nil {
			// The lock still goes: a failed teardown must not leave the
			// workspace locked to the lab for good. The error says what is
			// left running.
			if rerr := RemoveLock(ws); rerr == nil {
				rep.LockRemoved = rep.LockFound
			}
			return rep, fmt.Errorf("lab: compose down failed: %s", strings.TrimSpace(out+"\n"+terr.Error()))
		}
	}

	if err := RemoveLock(ws); err != nil {
		return rep, err
	}
	rep.LockRemoved = rep.LockFound
	return rep, nil
}

// Status reports what the harness knows about the lab.
//
// The lock is the source of truth: it is the harness's own record that a lab
// is up. No runtime call is made, so the status works on a machine whose
// container runtime is gone — and says so, because the lock is still there.
func Status(ctx context.Context, ws string, cfg Config) (State, error) {
	lock, err := ReadLock(ws)
	if err != nil {
		return State{}, err
	}
	if lock == nil {
		return State{Up: false, Locked: false}, nil
	}
	return State{
		Up:        true,
		Runtime:   lock.Runtime,
		Project:   lock.Project,
		Services:  lock.Services,
		StartedAt: lock.StartedAt,
		Locked:    true,
	}, nil
}

// ComposePath resolves a compose file against the workspace.
func ComposePath(ws, rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(ws, rel)
}

// ExecArgv builds the argv that runs a command inside the lab.
//
// The command never reaches the host: it is the argument of `sh -lc` inside
// the compose service. The compose client is the only thing that runs on the
// host, and it runs no model-supplied command of its own.
func ExecArgv(rt Runtime, project, service, command string) []string {
	return []string{
		string(rt),
		"compose",
		"-p", project,
		"exec", "-T", service,
		"sh", "-lc", command,
	}
}

// ExecEnv returns the environment the compose client needs to find the lab's
// compose file.
//
// The argv stays exactly as documented; the file is selected through the
// standard compose environment variable, so the same lab is addressed whether
// the compose file is the default one or an operator's own.
func ExecEnv(composeFile, project string) []string {
	env := []string{}
	if strings.TrimSpace(composeFile) != "" {
		env = append(env, "COMPOSE_FILE="+composeFile)
	}
	if strings.TrimSpace(project) != "" {
		env = append(env, "COMPOSE_PROJECT_NAME="+project)
	}
	return env
}

// runCompose runs one compose command with a timeout and captures its
// output, so a failure is reported with the runtime's own words.
func runCompose(ctx context.Context, argv []string, ws string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// resolveComposeFile finds the compose file the lab runs.
//
// The operator's file wins: <workspace>/<compose_file>. When it is missing,
// the blueprint templates are checked — but only for a workspace that was
// itself rendered from a blueprint (the render marker says which). Without
// that gate, `kintsugi lab up` in any directory would silently materialize a lab
// the operator never asked for. When nothing applies, the lab says so plainly
// and stops: it never invents a compose file.
func resolveComposeFile(ws, rel string) (path string, fromBlueprint bool, err error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = DefaultComposeFile
	}
	abs := ComposePath(ws, rel)
	if fileExists(abs) {
		return abs, false, nil
	}
	if data, name, ok := blueprintCompose(ws, rel); ok {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", false, fmt.Errorf("lab: %w", err)
		}
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return "", false, fmt.Errorf("lab: write compose file: %w", err)
		}
		_ = name
		return abs, true, nil
	}
	return "", false, fmt.Errorf(
		"lab: no compose file at %s: the workspace has none and no blueprint ships one — write %s, or point [lab] compose_file at an existing file",
		abs, rel)
}

// blueprintCompose returns the compose file the blueprint this workspace was
// rendered from ships at rel, if any.
//
// The templates are embedded in the blueprints package, so the blueprint is
// rendered into a temporary directory and the file read from there: the lab
// never writes a blueprint's other files into the workspace. Only a workspace
// carrying a render marker qualifies (see blueprints.RenderedFrom) — a bare
// workspace never inherits a lab it did not ask for.
func blueprintCompose(ws, rel string) (data []byte, blueprint string, ok bool) {
	rel = filepath.ToSlash(rel)
	// Only a workspace that was actually rendered from a blueprint may borrow
	// that blueprint's compose file. Without this gate, `kintsugi lab up` in any
	// directory would silently materialize a lab (and a lock that refuses host
	// bash) the operator never asked for.
	name, rendered := blueprints.RenderedFrom(ws)
	if !rendered {
		return nil, "", false
	}
	dir, err := os.MkdirTemp("", "kintsugi-lab-blueprint-")
	if err != nil {
		return nil, "", false
	}
	defer os.RemoveAll(dir)
	if _, _, err := blueprints.Render(name, dir, "lab", true); err != nil {
		return nil, "", false
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil || len(body) == 0 {
		return nil, "", false
	}
	return body, name, true
}

// runtimeOnPath reports whether a runtime's binary is on PATH.
func runtimeOnPath(rt Runtime) bool {
	if rt == "" || rt == RuntimeNone {
		return false
	}
	_, err := exec.LookPath(string(rt))
	return err == nil
}

// fileExists reports whether a path exists and is a regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
