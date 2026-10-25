// Package doctor reports environment readiness. Works without a TTY (SSH).
//
// Each check carries a three-state severity — pass, warn or fail — so an
// operator running `kintsugi doctor` sees blockers before niceties. The renderer
// groups the output fail-first (see cmd/kintsugi/cmdDoctor and the stateRank
// ordering in Run), and every warn/fail detail names the missing dependency
// and how to provide it.
package doctor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kintsugi/internal/config"
	fctx "kintsugi/internal/context"
	"kintsugi/internal/credentials"
	"kintsugi/internal/harness"
	"kintsugi/internal/sandbox"
	"kintsugi/internal/session"
)

// State is the three-state verdict a []Check row carries.
type State string

const (
	// StateUnknown is the zero value; a check built without an explicit state
	// is treated as StatePass by EffectiveState for backward compatibility.
	StateUnknown State = ""

	// StatePass means the check is green.
	StatePass State = "pass"
	// StateWarn means the check found something optional-but-warnworthy: the
	// environment still works, but a missing nicety is named for the operator.
	StateWarn State = "warn"
	// StateFail means the check found a blocker: work cannot proceed until it
	// is fixed.
	StateFail State = "fail"
)

// Check is a single readiness-row verdict.
//
// State is the authoritative three-state severity. Severity is kept as an
// exported alias so callers who think in the older "severity" vocabulary keep
// building; when State is set it is ignored, and when State is empty
// EffectiveState falls back to Severity (and then to StatePass).
type Check struct {
	Name     string
	State    State
	Severity State
	Detail   string
}

// EffectiveState resolves the three-state severity, honouring State first and
// falling back to the deprecated Severity alias.
func (c Check) EffectiveState() State {
	if c.State != StateUnknown {
		return c.State
	}
	if c.Severity != StateUnknown {
		return c.Severity
	}
	return StatePass
}

// Failed and Warned are ergonomic helpers over EffectiveState.
func (c Check) Failed() bool { return c.EffectiveState() == StateFail }
func (c Check) Warned() bool { return c.EffectiveState() == StateWarn }

// pass/warn/fail are the builders Run and its helpers use so a row can never
// be created with a boolean that silently loses the warn tier.
func pass(name, detail string) Check {
	return Check{Name: name, State: StatePass, Detail: detail}
}

func warn(name, detail string) Check {
	return Check{Name: name, State: StateWarn, Detail: detail}
}

func fail(name, detail string) Check {
	return Check{Name: name, State: StateFail, Detail: detail}
}

// Run evaluates the readiness checks for an App and returns them grouped
// fail-first, then warn, then pass. Grouping here keeps the renderer dumb:
// cmdDoctor can print in the returned order and blockers always come first.
func Run(app *harness.App, netProbe bool) []Check {
	var out []Check

	// Workspace is always available (harness.New resolved it), and always green.
	out = append(out, pass("workspace", app.Workspace))

	// Config files: running from defaults with no config file is a blocker —
	// the operator has not expressed any policy.
	if len(app.ConfigPaths) == 0 {
		out = append(out, fail("config", "missing: "+config.GlobalPath()+
			" — no config file found; create it (kintsugi setup) or run from a workspace with an kintsugi.toml"))
	} else {
		out = append(out, pass("config", strings.Join(app.ConfigPaths, " ")))
	}

	out = append(out, modeCheck(app))
	out = append(out, modelChecks(app, netProbe)...)
	out = append(out, gateModelCheck(app))
	out = append(out, personaPackCheck(app))
	out = append(out, sandboxCheck(app))
	out = append(out, requiredTool("git", "git",
		"install it (e.g. `apt install git` / `brew install git`) — needed to commit, diff and track workspace changes"))
	out = append(out, optionalTool("ripgrep", "rg",
		"install it for fast grep, or the grep tool falls back to a slow walk"))

	// AGENTS.md is operator instructions; missing it does not block work.
	if p := app.AgentsMDPath(); p != "" {
		out = append(out, pass("agents-md", p))
	} else {
		out = append(out, warn("agents-md", "missing: AGENTS.md — add one at the workspace root"+
			" (or a model-keyed one under .kintsugi/) for operator instructions"))
	}

	out = append(out, continuityCheck(app))
	out = append(out, attestationCheck(app))
	out = append(out, contextCheck(app))
	out = append(out, skillsCheck(app))

	if !app.Cfg.MCP.Enabled {
		out = append(out, pass("mcp", "disabled"))
	} else if len(app.Cfg.MCP.Servers) == 0 {
		out = append(out, warn("mcp", "missing: no stdio servers configured — add [mcp.servers.<name>] entries"+
			" to expose extra tools, or set [mcp] enabled = false"))
	} else {
		out = append(out, pass("mcp", fmt.Sprintf("%d configured stdio servers", len(app.Cfg.MCP.Servers))))
	}

	// Sessions dir writable: session persistence is part of the harness, so a
	// failure here blocks work.
	if d, err := session.Dir(); err != nil {
		out = append(out, fail("sessions-dir", err.Error()))
	} else {
		probe := filepath.Join(d, ".write-probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			out = append(out, fail("sessions-dir", err.Error()))
		} else {
			os.Remove(probe)
			out = append(out, pass("sessions-dir", d))
		}
	}

	// TTY presence is informational: headless operation is supported.
	out = append(out, pass("stdin-tty", ttyDetail(os.Stdin)))
	out = append(out, pass("stdout-tty", ttyDetail(os.Stdout)))

	return orderChecks(out)
}

// stateRank is the display order. Run uses it via orderChecks so the ordering
// is exercised by every doctor test, not just the CLI renderer.
var stateRank = []State{StateFail, StateWarn, StatePass}

// orderChecks stably partitions checks into fail, warn, pass order. It is a
// stable partition over the input so the relative order within a tier is the
// append order — blockers first, each tier kept in its original sequence.
func orderChecks(in []Check) []Check {
	out := make([]Check, 0, len(in))
	for _, want := range stateRank {
		for _, c := range in {
			if c.EffectiveState() == want {
				out = append(out, c)
			}
		}
	}
	return out
}

// modeCheck reports core.mode and the model selection it relies on. A blank
// default model is a config problem, captured in its own fail row; here it only
// downgrades this informational row to a warn so the mode still reads.
func modeCheck(app *harness.App) Check {
	if strings.TrimSpace(app.Cfg.Core.DefaultModel) == "" {
		return warn("mode", app.Cfg.Core.Mode+" (no core.default_model set)")
	}
	return pass("mode", app.Cfg.Core.Mode+" (default model "+app.Cfg.Core.DefaultModel+")")
}

// modelChecks resolves core.default_model and emits the model-dependent rows
// (default-model, persona-match, provider-key, provider-ping, and the
// offline/endpoint consistency check). A blank or unresolvable default model is
// a hard fail because no turn can compile; when it fails, the later rows that
// depend on a resolved model are skipped.
func modelChecks(app *harness.App, netProbe bool) []Check {
	ref := strings.TrimSpace(app.Cfg.Core.DefaultModel)
	if ref == "" {
		return []Check{fail("default-model",
			"missing: core.default_model — set it in config.toml (or KINTSUGI_MODEL=provider/model)")}
	}
	r, err := app.Resolve(ref)
	if err != nil {
		return []Check{fail("default-model",
			"core.default_model = "+ref+" is not resolvable — "+err.Error())}
	}

	out := []Check{pass("default-model",
		fmt.Sprintf("%s/%s family=%q adapter=%s", r.ProviderName, r.Model.ID, r.Model.Family, r.Adapter.ID))}

	if app.Pack != nil {
		sel, err := app.Pack.Select("", r.Model.Family, r.Model.ID, r.Capabilities())
		if err != nil {
			out = append(out, fail("persona-match", err.Error()))
		} else {
			out = append(out, pass("persona-match", fmt.Sprintf("%s (rule: %s)", sel.Persona.Name, sel.Rule)))
		}
	}

	if env := r.ProviderCfg.APIKeyEnv; env != "" {
		switch {
		case os.Getenv(env) != "":
			out = append(out, pass("provider-key", env+" set"))
		case credentials.Status(r.ProviderName, env) == "stored":
			out = append(out, pass("provider-key", "stored locally (kintsugi connect "+r.ProviderName+")"))
		default:
			out = append(out, fail("provider-key",
				"missing: run kintsugi connect "+r.ProviderName+" (or set "+env+")"))
		}
	} else if r.ProviderCfg.Offline || config.IsLoopback(r.Client.BaseURL) {
		// A local endpoint has nothing to connect: the key check passes with its
		// own detail rather than demanding a key that does not exist.
		out = append(out, pass("provider-key", "not required (local endpoint)"))
	} else if app.Cfg.Core.Mode == "normal" {
		out = append(out, fail("provider-key",
			"missing: api_key_env for provider "+r.ProviderName+" — set [providers."+r.ProviderName+"] api_key_env, then export the key"))
	} else {
		out = append(out, pass("provider-key", "no api_key_env configured"))
	}

	if netProbe {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := r.Client.Ping(ctx); err != nil {
			out = append(out, fail("provider-ping", err.Error()))
		} else {
			out = append(out, pass("provider-ping", r.Client.BaseURL))
		}
	} else {
		out = append(out, pass("provider-ping", "skipped (use --net)"))
	}

	// Mode vs endpoint consistency.
	if app.Cfg.Core.Mode == "offline" && !config.IsLoopback(r.Client.BaseURL) {
		out = append(out, fail("mode-endpoint",
			"core.mode=offline but default model is a cloud endpoint ("+r.Client.BaseURL+"); set KINTSUGI_MODE=normal for cloud runs"))
	}

	return out
}

// gateModelCheck reports the model the opsec-review gate uses. It is only
// meaningful when gates are in play (blocking, or a configured gate), and then
// a blank gate model is a fail naming gates.model — the gate must not silently
// fall back to the working model.
func gateModelCheck(app *harness.App) Check {
	if !app.Cfg.Gates.Blocking && strings.TrimSpace(app.Cfg.Gates.Model) == "" {
		return pass("gate-model", "not used (gates.model empty, gates.blocking=false)")
	}
	ref := strings.TrimSpace(app.Cfg.Gates.Model)
	if ref == "" {
		return fail("gate-model", "missing: gates.model — set it in config.toml (e.g. gates.model = \"provider/model\")"+
			", or the review falls back to core.default_model")
	}
	if _, err := app.Resolve(ref); err != nil {
		return fail("gate-model", "gates.model = "+ref+" is not resolvable — "+err.Error())
	}
	return pass("gate-model", ref)
}

// personaPackCheck reports whether a persona pack is available. A nil pack is
// a blocker: persona selection happens on every compile.
func personaPackCheck(app *harness.App) Check {
	if app.Pack == nil {
		return fail("persona-pack", "missing: persona pack at "+config.ExpandHome(app.Cfg.PersonaPack.Dir)+
			" — set persona_pack.dir to a pack directory, or run `kintsugi setup`")
	}
	dir := config.ExpandHome(app.Cfg.PersonaPack.Dir)
	source := dir
	if app.Pack.Dir != "" && !strings.HasPrefix(app.Pack.Dir, "/") {
		source = "embedded (" + app.Pack.Dir + ")"
	}
	return pass("persona-pack", fmt.Sprintf("%s — %d personas", source, len(app.Pack.Names())))
}

// sandboxCheck reports the OS-isolation posture. With mode != "off", a missing
// backend is a blocker (the tool layer refuses to run commands unsandboxed);
// with the sandbox switched off it stays green and is called out as a warn so
// the operator has seen the posture.
func sandboxCheck(app *harness.App) Check {
	mode := strings.ToLower(strings.TrimSpace(app.Cfg.Sandbox.Mode))
	if mode == "" || mode == "off" {
		return warn("sandbox", "off — commands run without OS isolation; set [sandbox] mode = \"readonly\" or \"workspace\" to sandbox")
	}
	res := sandbox.Detect(sandbox.FromConfig(app.Cfg.Sandbox))
	if res.Available {
		return pass("sandbox", res.Reason)
	}
	return fail("sandbox", "missing: "+res.Reason)
}

// requiredTool reports a hard dependency by binary name.
func requiredTool(name, bin, remediation string) Check {
	if p, err := exec.LookPath(bin); err == nil && p != "" {
		return pass(name, p)
	}
	return fail(name, "missing: "+bin+" — "+remediation)
}

// optionalTool reports a tool that improves (but does not gate) work.
func optionalTool(name, bin, remediation string) Check {
	if p, err := exec.LookPath(bin); err == nil && p != "" {
		return pass(name, p)
	}
	return warn(name, "missing: "+bin+" ("+name+") — "+remediation)
}

// contextCheck reports the layer-5 workspace frame. CONTEXT.md is optional as a
// whole, but when present it must load and carry the authorization section — a
// frame without the operator-authored authorization loses its load-bearing
// section, so that is a warn rather than a pass.
func contextCheck(app *harness.App) Check {
	if !app.Cfg.Context.Enabled {
		return pass("workspace-context", "disabled (set [context] enabled = true to inject the layer-5 frame)")
	}
	ctxPath := app.Cfg.Context.Path
	if ctxPath == "" {
		ctxPath = "CONTEXT.md"
	}
	if _, err := os.Stat(filepath.Join(app.Workspace, ctxPath)); os.IsNotExist(err) {
		return warn("workspace-context", "missing: "+ctxPath+
			" — layer 5 empty; run `kintsugi context init` to add the operator frame, or set [context] enabled = false")
	}
	doc, _, err := app.LoadContext()
	if err != nil {
		return fail("workspace-context", "unreadable "+ctxPath+" — "+err.Error())
	}
	if _, ok := doc.Find(fctx.ClassAuthorization); !ok {
		return warn("workspace-context", "missing: Authorization & scope in "+ctxPath+
			" — run `kintsugi context init`, then record the review with `kintsugi context attest`")
	}
	return pass("workspace-context", ctxPath)
}

// skillsCheck reports discovered progressive-disclosure skills. None is a warn
// (the harness still works), and names the directory to add a SKILL.md to.
func skillsCheck(app *harness.App) Check {
	sk := app.Skills()
	if len(sk) == 0 {
		return warn("skills", "missing: none discovered under "+app.SkillsDir()+
			" — add <name>/SKILL.md (or <name>.md) with name/description frontmatter to teach procedures")
	}
	return pass("skills", fmt.Sprintf("%d discovered under %s", len(sk), app.SkillsDir()))
}

// doctorLedgerReadBytes bounds the ledger scan: doctor runs on every
// invocation and must stay fast, so a large ledger is counted as far as this
// and then reported as "N+ entries".
const doctorLedgerReadBytes = 256 << 10

// continuityCheck reports the local continuity layer (M8.5): whether it is on,
// where the ledger lives, and how much accepted work it holds.
//
// It deliberately does not open the ledger store: ledger.Open rebuilds the
// derived FTS index, which is wasted work for a readiness check. A bounded read
// of ledger.md (one "## <id> · <kind>" section per entry) answers the same
// question. Nothing here reads a refusal: the ledger cannot hold one.
func continuityCheck(app *harness.App) Check {
	cfg := app.Cfg.Continuity
	if !cfg.Enabled {
		return warn("continuity", "disabled (set [continuity] enabled = true to recall accepted work)")
	}
	path := filepath.Join(app.LedgerDir(), "ledger.md")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pass("continuity", "enabled · "+path+" · no accepted work yet")
		}
		return fail("continuity", path+": "+err.Error())
	}
	defer f.Close()

	n, truncated, err := countLedgerSections(f, doctorLedgerReadBytes)
	if err != nil {
		return fail("continuity", path+": "+err.Error())
	}
	detail := "enabled · " + path + " · "
	switch {
	case n == 0:
		detail += "no accepted work yet"
	case truncated:
		detail += strconv.Itoa(n) + "+ entries (bounded read)"
	default:
		detail += strconv.Itoa(n) + " entries"
	}
	return pass("continuity", detail)
}

// countLedgerSections counts entry headings in a ledger, reading at most max
// bytes. The bool reports whether the read was cut short.
func countLedgerSections(r io.Reader, max int64) (int, bool, error) {
	lr := io.LimitReader(r, max)
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	var read int64
	for sc.Scan() {
		read += int64(len(sc.Bytes())) + 1
		if strings.HasPrefix(sc.Text(), "## ") {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return n, false, err
	}
	return n, read >= max, nil
}

// attestationCheck reports the operator attestation (M8.5.2): missing is a
// normal state, a recorded review is shown with its reviewer and hash prefix,
// and a hash that no longer describes the current Authorization & scope text (or
// a review-by date that has passed) is reported as an issue — which is the whole
// reason a hash is recorded.
//
// Missing attestation is reclassified as a warn (not a fail): it names the exact
// command to run, and an environment with no attestation is still fully usable.
func attestationCheck(app *harness.App) Check {
	att, err := fctx.LoadAttestation(app.Workspace)
	if err != nil {
		return fail("attestation", err.Error())
	}
	if att == nil {
		return warn("attestation", "none (missing: .kintsugi/attestation.json — run `kintsugi context attest` to record an operator review)")
	}

	detail := fmt.Sprintf("%s by %s · hash %s", orDash(att.AttestedAt), orDash(att.Reviewer), hashPrefix(att.AuthorizationHash))
	if exp := strings.TrimSpace(att.ExpiresAt); exp != "" {
		detail += " · review by " + exp
	}

	doc, _, derr := app.LoadContext()
	switch {
	case derr != nil:
		return fail("attestation", detail+" · CONTEXT.md unreadable ("+derr.Error()+")")
	case !att.HashMatches(doc):
		return fail("attestation", detail+
			" · authorization hash no longer matches CONTEXT.md: re-review Authorization & scope, then `kintsugi context attest`")
	case att.Stale(time.Now()):
		return fail("attestation", detail+" · stale (review-by date has passed)")
	}
	return pass("attestation", detail)
}

// hashPrefix shortens an authorization hash for a one-line detail.
func hashPrefix(h string) string {
	h = strings.TrimPrefix(strings.TrimSpace(h), fctx.AttestationHashPrefix)
	if h == "" {
		return "unrecorded"
	}
	if len(h) > 12 {
		h = h[:12] + "…"
	}
	return fctx.AttestationHashPrefix + h
}

// orDash keeps a missing field visible instead of printing an empty column.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func ttyDetail(f *os.File) string {
	if isTTY(f) {
		return "yes"
	}
	return "no (headless use is supported)"
}
