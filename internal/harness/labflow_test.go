package harness

// The blueprints → lab → build turn → gate → report flow, run as one test
// (WP M8-A).
//
// Everything here is synthetic and local, and the test is explicit about which
// parts are real:
//
//   - real: the blueprint render, the workspace config it writes, the compiled
//     prompt stack, the tool executor and its permission matrix, the lab
//     harness's lock, the gate's empty-diff path, the report renderer.
//   - faked: the container runtime (a shell script on PATH that records argv,
//     so no container is ever started) and the provider (a loopback SSE server,
//     the same shape scripts/mock_provider.py serves).
//
// No git command is run: the "empty diff" the gate reviews is measured, not
// assumed — the workspace tree is hashed before and after the turn.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/adapters"
	"kintsugi/internal/agent"
	"kintsugi/internal/blueprints"
	"kintsugi/internal/config"
	fctx "kintsugi/internal/context"
	"kintsugi/internal/gate"
	"kintsugi/internal/lab"
	"kintsugi/internal/persona"
	"kintsugi/internal/provider"
	"kintsugi/internal/report"
	"kintsugi/internal/tools"
	"kintsugi/pack"
)

// labFakeRuntime installs a fake container runtime on PATH and returns the path
// of the file it records its argv in.
//
// The script answers `compose version` (the probe lab.Detect makes) and exits
// zero for everything else, so what the flow exercises is the harness — the
// argv it builds, the lock it writes, the boundary it enforces — and never a
// container. It is prepended to PATH rather than replacing it: the turn's own
// shell tools still need a usable PATH.
func labFakeRuntime(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"log=\"${0%/*}/calls.log\"\n" +
		"printf '%s\\n' \"$*\" >> \"$log\"\n" +
		"if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"version\" ]; then\n" +
		"  echo \"Docker Compose version v2.29.0\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "calls.log")
}

// labReadCalls returns the argv lines the fake runtime recorded.
func labReadCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// labTreeSnapshot hashes every file in the workspace except the harness's own
// directory, so "the turn changed nothing" is a measurement rather than a
// claim. .kintsugi is skipped because the harness writes into it by design
// (the lab lock, the audit log).
func labTreeSnapshot(t *testing.T, ws string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(ws, p)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if d.Name() == ".kintsugi" {
				return filepath.SkipDir
			}
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// labDiff renders the two snapshots as a unified-diff-shaped summary. An
// unchanged tree yields the empty string, which is exactly what the gate
// reviews: no change, nothing to flag.
func labDiff(before, after map[string]string) string {
	var b strings.Builder
	for rel, hash := range before {
		if after[rel] != hash {
			b.WriteString("diff --git a/" + rel + " b/" + rel + "\n")
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			b.WriteString("diff --git a/" + rel + " b/" + rel + "\n")
		}
	}
	return b.String()
}

// labFlowApp renders a blueprint into a fresh workspace and builds an App on
// it: the workspace's own kintsugi.toml is really loaded, and only the
// provider and the default model are pointed at the loopback mock (the same
// substitution scripts/demo.sh makes). The XDG directories are redirected so
// the flow never reads or writes the operator's own state.
func labFlowApp(t *testing.T, blueprint, ws string, mock *mockModel) *App {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("KINTSUGI_CONFIG", "")
	t.Setenv("KINTSUGI_MODE", "")
	t.Setenv("KINTSUGI_MODEL", "")

	cfg, paths, err := config.Load(ws)
	if err != nil {
		t.Fatalf("load the blueprint's config: %v", err)
	}
	if cfg.Workspace.Profile != "lab" || cfg.Core.Mode != "offline" {
		t.Fatalf("the blueprint config is not the lab/offline profile: %+v", cfg.Workspace)
	}
	// The operator's [lab] block names the one service the blueprint's compose
	// file declares; everything else stays at the harness default.
	cfg.Lab.Services = []string{"fixture"}
	cfg.Providers["mock"] = config.Provider{
		BaseURL: mock.URL,
		Wire:    "openai-completions",
		Family:  "deepseek",
		Offline: true,
		Models:  []config.Model{{ID: "mock-model", Family: "deepseek", Context: 32000}},
	}
	cfg.Core.DefaultModel = "mock/mock-model"
	cfg.Advisor.Enabled = boolPtr(false)
	cfg.Agents.Subagents = false

	reg := appAdapters(t)
	pk, err := persona.LoadFS(pack.FS, "agents/global", cfg.PersonaPack.Fallback)
	if err != nil {
		t.Fatalf("load the embedded persona pack: %v", err)
	}
	return &App{Cfg: cfg, ConfigPaths: paths, Adapters: reg, Workspace: ws, Pack: pk}
}

// TestLabFlowBlueprintToReport is the M8-A integration flow: a rendered
// blueprint becomes a workspace `kintsugi lab up` works in, an agent turn runs
// inside that boundary on read-only tools, the gate reviews the (empty) diff,
// the report renders, and the teardown lifts the boundary again.
func TestLabFlowBlueprintToReport(t *testing.T) {
	ctx := context.Background()

	for _, blueprint := range []string{"c2", "recon"} {
		t.Run(blueprint, func(t *testing.T) {
			log := labFakeRuntime(t, "docker")
			ws := t.TempDir()

			// ---- 1. blueprint -------------------------------------------------
			written, skipped, err := blueprints.Render(blueprint, ws, "lab-flow", false)
			if err != nil {
				t.Fatalf("render %s: %v", blueprint, err)
			}
			if len(skipped) != 0 {
				t.Fatalf("a fresh workspace skipped files: %v", skipped)
			}
			if !containsSlice(written, lab.DefaultComposeFile) {
				t.Fatalf("the blueprint shipped no compose file: %v", written)
			}

			// ---- 2. lab up (fake runtime) -------------------------------------
			// The provider is started before the app is built so the config can
			// name its loopback URL.
			mock := newMockModel(t, labFlowResponder())
			mock.label = "mock"
			app := labFlowApp(t, blueprint, ws, mock)
			labCfg := lab.FromConfig(app.Cfg.Lab)
			if labCfg.ComposeFile != lab.DefaultComposeFile {
				t.Fatalf("compose file = %q, want the blueprint default %q", labCfg.ComposeFile, lab.DefaultComposeFile)
			}

			state, err := lab.Up(ctx, ws, labCfg)
			if err != nil {
				t.Fatalf("lab up on a rendered %s workspace: %v", blueprint, err)
			}
			if !state.Up || !state.Locked {
				t.Fatalf("lab up reported %+v, want an up, locked lab", state)
			}
			lockPath := lab.LockPath(ws)
			if _, err := os.Stat(lockPath); err != nil {
				t.Fatalf("the lock was not written at %s: %v", lockPath, err)
			}
			lock, err := lab.ReadLock(ws)
			if err != nil || lock == nil {
				t.Fatalf("read the lock: %v (%+v)", err, lock)
			}
			if !lab.SameWorkspace(lock.Workspace, ws) {
				t.Fatalf("the lock names workspace %q, want %q", lock.Workspace, ws)
			}
			upCalls := labReadCalls(t, log)
			wantUp := "compose -p " + lab.DefaultProject + " -f " +
				filepath.Join(ws, filepath.FromSlash(lab.DefaultComposeFile)) + " up -d fixture"
			if !containsSlice(upCalls, wantUp) {
				t.Fatalf("the compose argv is not the documented one:\n%s", strings.Join(upCalls, "\n"))
			}

			// ---- 3. one agent turn, read-only tools ---------------------------
			resolved, err := app.Resolve("mock/mock-model")
			if err != nil {
				t.Fatalf("resolve the mock model: %v", err)
			}
			comp, err := app.Compile(resolved, CompileOpts{Task: labFlowTask})
			if err != nil {
				t.Fatalf("compile the prompt stack: %v", err)
			}
			// The blueprint's own CONTEXT.md is what the model reads, including
			// the authorization placeholder the harness never fills in.
			if !strings.Contains(comp.Text, blueprints.OperatorPlaceholder) {
				t.Fatal("the compiled stack does not carry the blueprint's authorization placeholder")
			}

			ex := &tools.Executor{Workspace: ws, Policy: app.ToolPolicy()}
			closeTools := app.AttachTools(ex, nil)
			defer closeTools()
			if ex.Lab == nil || ex.Lab.ComposeFile != lab.DefaultComposeFile {
				t.Fatalf("the executor carries no lab posture: %+v", ex.Lab)
			}

			before := labTreeSnapshot(t, ws)
			var toolResults []string
			lockSeenDuring := true
			loop := &agent.Loop{
				Client:   resolved.Client,
				Tools:    ex,
				MaxLoops: 4,
				OnTool: func(call provider.ToolCall, res tools.Result) {
					// The boundary holds for the whole turn: every tool call
					// runs with the lock on disk.
					if _, err := os.Stat(lockPath); err != nil {
						lockSeenDuring = false
					}
					toolResults = append(toolResults, call.Function.Name+" → "+res.Content)
				},
			}
			final, err := loop.Run(ctx, comp.Text, labFlowTask)
			if err != nil {
				t.Fatalf("the turn failed: %v", err)
			}
			if !lockSeenDuring {
				t.Fatal("the lab lock disappeared during the turn")
			}
			if strings.TrimSpace(final) == "" {
				t.Fatal("the turn produced no answer")
			}
			joined := strings.Join(toolResults, "\n")
			for _, tool := range []string{"list", "read"} {
				if !strings.Contains(joined, tool+" → ") {
					t.Fatalf("the turn never ran %q:\n%s", tool, joined)
				}
			}
			// The read-only turn really read the shipped compose file.
			if !strings.Contains(joined, "network_mode: none") {
				t.Fatalf("the turn did not read the lab compose file:\n%s", joined)
			}

			// The model's attempt to run a command on the host was refused by
			// the lab boundary, and nothing landed on disk.
			var bashRefused bool
			for _, r := range toolResults {
				if strings.HasPrefix(r, "bash → ") && strings.Contains(r, "a lab is up") {
					bashRefused = true
				}
			}
			if !bashRefused {
				t.Fatalf("host bash was not refused by the lab boundary during the turn:\n%s", joined)
			}
			if _, err := os.Stat(filepath.Join(ws, "host-artifact.txt")); !os.IsNotExist(err) {
				t.Fatal("the refused host command still wrote a file")
			}

			// ---- 4. the executor refuses host bash while the lock exists ------
			// The same guard, asked directly: this is the boundary the turn ran
			// inside, checked from the outside.
			direct := ex.Dispatch(provider.ToolCall{
				ID:   "direct-bash",
				Type: "function",
				Function: provider.Function{
					Name:      "bash",
					Arguments: `{"command":"touch host-artifact.txt"}`,
				},
			})
			if !direct.Failed {
				t.Fatalf("host bash ran while the lab was up: %+v", direct)
			}
			for _, want := range []string{"a lab is up", lockPath, "run_in_lab", "kintsugi lab down"} {
				if !strings.Contains(direct.Content, want) {
					t.Fatalf("the refusal does not name %q: %+v", want, direct)
				}
			}
			if _, err := os.Stat(filepath.Join(ws, "host-artifact.txt")); !os.IsNotExist(err) {
				t.Fatal("the refused host command still wrote a file")
			}

			// The refusal has a working alternative: the command runs through
			// the compose client, and the recorded argv shows it is the
			// argument of `sh -lc` inside the service, never of a host shell.
			//
			// The blueprint ships no yolo, so the matrix asks for the lab
			// command and the executor has no approver attached. An approver is
			// attached here — this is the operator saying yes to the in-lab
			// command. It is attached *after* the host-bash assertions above on
			// purpose: labHostGuard runs before the permission gate, so no
			// approver could have answered the refused host command anyway.
			var approvals []string
			ex.Approver = tools.ApproverFunc(func(ar tools.ApprovalRequest) bool {
				approvals = append(approvals, ar.Tool)
				return true
			})
			inLab := ex.Dispatch(provider.ToolCall{
				ID:   "lab-exec",
				Type: "function",
				Function: provider.Function{
					Name:      "run_in_lab",
					Arguments: `{"command":"cat /etc/hostname"}`,
				},
			})
			if inLab.Failed {
				t.Fatalf("run_in_lab failed inside the lab boundary: %+v", inLab)
			}
			if !containsSlice(approvals, "run_in_lab") {
				t.Fatalf("the lab command was not put to the operator: %v", approvals)
			}
			// No host shell was asked about, and nothing was approved for the
			// host shell either.
			if containsSlice(approvals, "bash") {
				t.Fatalf("the host shell was approved while the lab was up: %v", approvals)
			}
			execCalls := labReadCalls(t, log)
			wantExec := "compose -p " + lab.DefaultProject + " exec -T fixture sh -lc cat /etc/hostname"
			if !containsSlice(execCalls, wantExec) {
				t.Fatalf("the lab command did not reach the compose client as %q:\n%s",
					wantExec, strings.Join(execCalls, "\n"))
			}

			// ---- 5. gate review on the (measured) empty diff ------------------
			diff := labDiff(before, labTreeSnapshot(t, ws))
			if diff != "" {
				t.Fatalf("a read-only turn changed the workspace:\n%s", diff)
			}
			scope := app.ContextSections(fctx.ClassAuthorization)[fctx.ClassAuthorization]
			if !strings.Contains(scope, "TODO") {
				t.Fatalf("the scope handed to the gate is not the operator placeholder: %q", scope)
			}
			callsBeforeGate := mock.calls()
			res := gate.Run(ctx, gate.Spec{Kind: "opsec-review", Blocking: false},
				&gate.Reviewer{Client: resolved.Client}, ws, scope, diff)
			if res.Blocking {
				t.Fatal("the gate was blocking; this flow runs it as advisory")
			}
			if !res.Passed || res.Verdict != "pass" {
				t.Fatalf("the gate on an empty diff = %+v, want an advisory pass", res)
			}
			if mock.calls() != callsBeforeGate {
				t.Fatalf("the gate called the provider for an empty diff (%d → %d requests)",
					callsBeforeGate, mock.calls())
			}

			// ---- 6. report -----------------------------------------------------
			// The findings file is this flow's own artifact: it records what the
			// run established, so the report has something honest to render.
			findings := "# Findings — lab-flow (" + blueprint + ")\n\n" +
				"Recorded by the M8-A lab flow test; `bash scripts/gauntlet.sh` runs it.\n\n" +
				"- lab: one `fixture` service, network_mode none, no published ports\n" +
				"- gate: opsec-review on an empty diff → " + res.Verdict + " (advisory)\n" +
				"- turn: read-only; the workspace tree is unchanged\n"
			write(t, filepath.Join(ws, ".kintsugi", "findings.md"), findings)

			in, err := report.Load(ws)
			if err != nil {
				t.Fatalf("report.Load: %v", err)
			}
			if in.Project != filepath.Base(ws) {
				t.Fatalf("report project = %q, want the workspace name %q", in.Project, filepath.Base(ws))
			}
			if !strings.Contains(in.Findings, "opsec-review on an empty diff") {
				t.Fatalf("the report dropped the findings file: %q", in.Findings)
			}
			body := report.Render(in)
			for _, section := range []string{
				"# kintsugi report · ",
				"// 01 summary",
				"// 02 findings",
				"// 03 refusal telemetry",
				"// 04 repair telemetry",
				"// 05 quota",
			} {
				if !strings.Contains(body, section) {
					t.Fatalf("the report has no %q section:\n%s", section, body)
				}
			}
			if !strings.Contains(body, "network_mode none") {
				t.Fatalf("the report does not carry the run's findings:\n%s", body)
			}

			// ---- 7. lab down: the boundary lifts ------------------------------
			rep, err := lab.DownReport(ctx, ws, labCfg)
			if err != nil {
				t.Fatalf("lab down: %v", err)
			}
			if !rep.LockFound || !rep.LockRemoved || !rep.Teardown {
				t.Fatalf("the teardown report is %+v, want the lock found, removed and the project torn down", rep)
			}
			if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
				t.Fatalf("the lock survived the teardown: %v", err)
			}
			if down, err := lab.Status(ctx, ws, labCfg); err != nil || down.Up || down.Locked {
				t.Fatalf("the lab is still up after the teardown: %+v (%v)", down, err)
			}
			if !containsSlice(labReadCalls(t, log), "compose -p "+lab.DefaultProject+" -f "+
				filepath.Join(ws, filepath.FromSlash(lab.DefaultComposeFile))+" down -v") {
				t.Fatalf("the teardown did not run compose down -v:\n%s",
					strings.Join(labReadCalls(t, log), "\n"))
			}

			// The same call is no longer refused by the lab boundary. The
			// approver is detached first so what is left is plainly the
			// ordinary permission gate asking — that is the point of the
			// distinction: the lab lock and the permission matrix are two
			// different refusals, and only one of them lifted.
			ex.Approver = nil
			after := ex.Dispatch(provider.ToolCall{
				ID:   "after-down",
				Type: "function",
				Function: provider.Function{
					Name:      "bash",
					Arguments: `{"command":"echo hi"}`,
				},
			})
			if strings.Contains(after.Content, "a lab is up") {
				t.Fatalf("the lab boundary outlived the lock: %+v", after)
			}
			if !after.Failed || !strings.Contains(after.Content, "approval") {
				t.Fatalf("after the teardown the refusal is not the permission gate: %+v", after)
			}
			// With the boundary lifted a permissive policy runs the command.
			loose := &tools.Executor{Workspace: ws, Policy: tools.Policy{Yolo: true}}
			if res := loose.Dispatch(provider.ToolCall{
				ID:       "loose",
				Type:     "function",
				Function: provider.Function{Name: "bash", Arguments: `{"command":"echo lab-is-down"}`},
			}); res.Failed || !strings.Contains(res.Content, "lab-is-down") {
				t.Fatalf("host bash did not run after the teardown: %+v", res)
			}
		})
	}
}

// labFlowTask is the turn's task. It asks for reading and reporting only, which
// is what a lab-scoped first pass should be. The host command it asks the model
// to attempt is deliberately in the instructions: the flow wants the boundary
// refused inside a real turn, not only in a hand-built dispatch below.
const labFlowTask = "Inspect this lab workspace and report whether the fixture is egress-free. " +
	"Steps: list the workspace root; try `touch host-artifact.txt` with the bash tool so the " +
	"harness records what it does with a host command while a lab is up; read " +
	"lab/docker-compose.yml; then answer in two sentences."

// labFlowResponder drives the mock provider: one read-only tool, one host
// command the boundary must refuse, one read of the shipped compose file, then
// the answer.
func labFlowResponder() func(int) []string {
	return func(n int) []string {
		switch n {
		case 1:
			return toolCallReply("call-list", "list", `{"path":"."}`)
		case 2:
			return toolCallReply("call-bash", "bash", `{"command":"touch host-artifact.txt"}`)
		case 3:
			return toolCallReply("call-read", "read", `{"path":"lab/docker-compose.yml"}`)
		default:
			return textReply("The fixture runs with network_mode: none and publishes no ports, " +
				"so it is egress-free. Host commands are refused while the lab is up.")
		}
	}
}

// appAdapters loads the adapter registry the App resolves models through.
func appAdapters(t *testing.T) *adapters.Registry {
	t.Helper()
	reg, err := adapters.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// containsSlice reports whether list holds want.
func containsSlice(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// boolPtr is a helper for the tri-state config booleans.
func boolPtr(b bool) *bool { return &b }
