package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kintsugi/internal/advisor"
	"kintsugi/internal/agent"
	"kintsugi/internal/config"
	fctx "kintsugi/internal/context"
	"kintsugi/internal/dag"
	"kintsugi/internal/gate"
	"kintsugi/internal/harness"
	"kintsugi/internal/memory"
	planstore "kintsugi/internal/plan"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
)

func cmdGate(ws string, args []string) {
	if len(args) == 0 || args[0] != "review" {
		fail("usage: kintsugi gate review --diff <file|-> [--blocking] [--json]")
	}
	fs := flag.NewFlagSet("gate review", flag.ExitOnError)
	diffPath := fs.String("diff", "", "diff file to review, or - for stdin")
	blocking := fs.Bool("blocking", false, "exit non-zero on a non-pass verdict")
	asJSON := fs.Bool("json", false, "print the typed review as JSON")
	fs.Parse(args[1:])
	if *diffPath == "" {
		fail("gate review: --diff is required")
	}

	var raw []byte
	var err error
	if *diffPath == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*diffPath)
	}
	if err != nil {
		fail("gate review: %v", err)
	}

	app := newApp(ws)
	reviewer, err := app.GateReviewer()
	if err != nil {
		fail("gate review: %v", err)
	}
	result := gate.Run(context.Background(), gate.Spec{Kind: "opsec-review", Blocking: *blocking},
		reviewer, app.Workspace, appScope(app), string(raw))

	if *asJSON {
		b, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("verdict: %s (%s)\n", result.Verdict, ternary(result.Passed, "pass", "fail"))
		if result.Output != "" {
			fmt.Println(result.Output)
		}
		for _, f := range result.Findings {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Printf("  [%s] %s — %s\n", f.Severity, loc, f.Consequence)
		}
		if len(result.NotAFinding) > 0 {
			fmt.Println("not a finding:")
			for _, n := range result.NotAFinding {
				fmt.Println("  - " + n)
			}
		}
	}
	if *blocking && !result.Passed {
		os.Exit(1)
	}
}

func cmdDag(ws string, args []string) {
	if len(args) == 0 || args[0] != "run" {
		fail("usage: kintsugi dag run <plan.yaml> [--json] [--max-revisions N]")
	}
	fs := flag.NewFlagSet("dag run", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	maxRev := fs.Int("max-revisions", 1, "blocking-gate revise cycles per node")
	fs.Parse(args[1:])
	rest := fs.Args()
	if len(rest) == 0 {
		fail("dag run: exactly one plan file is required")
	}
	planPath := rest[0]
	if len(rest) > 1 {
		fs.Parse(rest[1:])
	}

	raw, err := os.ReadFile(planPath)
	if err != nil {
		fail("dag run: %v", err)
	}
	var plan dag.Plan
	if err := yaml.Unmarshal(raw, &plan); err != nil {
		fail("dag run: %v", err)
	}

	app := newApp(ws)
	res, err := app.Resolve("")
	if err != nil {
		fail("dag run: %v", err)
	}
	comp, err := app.Compile(res, harness.CompileOpts{})
	if err != nil {
		fail("dag run: %v", err)
	}
	log, err := session.New()
	if err != nil {
		fail("dag run: session: %v", err)
	}
	defer log.Close()
	mem, _ := app.MemoryStore()
	if mem != nil {
		defer mem.Close()
	}
	reviewer, err := app.GateReviewer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: gate reviewer: %v\n", err)
	}

	scope := appScope(app)
	// A plan-review gate judges a plan, so the newest saved plan is handed over.
	// Without it the gate would review the diff and answer a question nobody
	// asked; a workspace with no plan leaves it empty, and the gate falls back.
	planText := ""
	if plans, perr := planstore.List(app.Workspace); perr == nil && len(plans) > 0 {
		planText = plans[0].Content
	}
	runner := &cliNodeRunner{app: app, res: res, comp: comp, log: log, mem: mem, policy: app.ToolPolicy()}
	engine := &dag.Engine{
		Runner:       runner,
		Diff:         func(ctx context.Context) (string, error) { return workspaceDiff(ctx, app.Workspace) },
		Reviewer:     reviewer,
		Workspace:    app.Workspace,
		Scope:        scope,
		PlanArtifact: planText,
		ACP:          acpSettings(app.Cfg.ACP),
		MaxRevisions: *maxRev,
		Log:          log,
	}
	report, err := engine.Run(context.Background(), plan)
	if err != nil {
		fail("dag run: %v", err)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("plan %s\n", report.Plan)
		for _, n := range report.Nodes {
			fmt.Printf("  %-5s %-20s revisions=%d\n", n.State, n.ID, n.Revisions)
			if n.State != "done" && n.Output != "" {
				fmt.Println("        " + oneLine(n.Output))
			}
			if n.State != "done" && n.Feedback != "" {
				for _, line := range strings.Split(n.Feedback, "\n") {
					fmt.Println("        " + line)
				}
			}
		}
	}
	if !report.Done() {
		os.Exit(1)
	}
}

type cliNodeRunner struct {
	app    *harness.App
	res    *harness.Resolved
	comp   *prompt.Compiled
	log    *session.Session
	mem    *memory.Store
	policy tools.Policy
}

func (r *cliNodeRunner) RunNode(ctx context.Context, node dag.Node, feedback string) (string, error) {
	res, comp := r.res, r.comp
	if role := strings.TrimSpace(node.AssignedTo); role != "" {
		if rr, err := r.app.ResolveRole(role); err == nil {
			if cc, err := r.app.CompileRole(rr, role, harness.CompileOpts{}); err == nil {
				res, comp = rr, cc
			}
		}
	}
	if store, err := r.app.QuotaStore(); err == nil {
		estimate := comp.Manifest.Totals.Tokens + (len(node.Task)+3)/4 + 2000
		if ok, msg := store.CanFit(res.ProviderName, estimate); !ok {
			return "", fmt.Errorf("quota: %s", msg)
		}
	}
	task := node.Task
	if len(node.Acceptance) > 0 {
		task += "\n\nAcceptance criteria:\n- " + strings.Join(node.Acceptance, "\n- ")
	}
	if feedback != "" {
		task += "\n\nGate feedback from the previous attempt (revise, then finish):\n" + feedback
	}
	ex := &tools.Executor{Workspace: r.app.Workspace, Memory: r.mem, SkillsDir: r.app.SkillsDir(), Policy: r.policy, Log: r.log.Log, Offline: r.app.Cfg.Core.Mode == "offline"}
	ex.ApplySecurity(r.app.Cfg)
	if r.app.Cfg.Agents.Subagents {
		if mgr, err := r.app.TaskManagerWithRoles(res, comp, nil, r.log.Log); err == nil {
			ex.Tasks = mgr
		}
	}
	loop := &agent.Loop{Client: res.Client, Tools: ex, Log: r.log, MaxLoops: r.app.Cfg.Harness.MaxToolLoops}
	loop.OnUsage = func(p, c int) { r.app.RecordUsage(res.ProviderName, p, c) }
	loop.OnDelta = func(s string) { fmt.Print(s) }
	loop.OnTool = func(call provider.ToolCall, res tools.Result) {
		state := "✓"
		if res.Failed {
			state = "✗"
		}
		fmt.Printf("\n  %s %s\n", state, call.Function.Name)
	}
	if r.app.AdvisorEnabled() {
		if adv, err := r.app.AdvisorFor(res); err == nil {
			loop.Advisor = adv
			loop.MaxAdvisorRevisions = 1
			loop.OnAdvisor = func(notes []advisor.Note) {
				for _, n := range notes {
					fmt.Printf("\n  ◇ advisor (%s): %s\n", n.Kind, n.Text)
				}
			}
		}
	}
	fmt.Printf("\n[%s] %s\n", node.ID, oneLine(node.Task))
	final, _, err := loop.RunConversation(ctx, comp.Text, nil, task)
	fmt.Println()
	return final, err
}

func workspaceDiff(ctx context.Context, ws string) (string, error) {
	var b strings.Builder
	cmd := exec.CommandContext(ctx, "git", "-C", ws, "diff", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff: %v: %s", err, strings.TrimSpace(string(out)))
	}
	b.Write(out)

	files, _ := exec.CommandContext(ctx, "git", "-C", ws, "ls-files", "--others", "--exclude-standard").Output()
	budget := 60000 - b.Len()
	for _, name := range strings.Split(strings.TrimSpace(string(files)), "\n") {
		if budget <= 0 || strings.TrimSpace(name) == "" {
			break
		}
		data, err := os.ReadFile(filepath.Join(ws, name))
		if err != nil || len(data) == 0 || strings.ContainsRune(string(data[:min(len(data), 512)]), 0) {
			continue
		}
		chunk := string(data)
		if len(chunk) > budget {
			chunk = chunk[:budget] + "\n…[truncated]"
		}
		fmt.Fprintf(&b, "\n--- new file: %s\n%s", name, chunk)
		budget -= len(chunk)
	}
	return b.String(), nil
}

// acpSettings translates the config section into the adapter's settings. An
// unparsable or empty duration keeps the package default rather than collapsing
// to zero — a zero timeout would mean "no timeout", which is the opposite of
// what a missing value should do.
func acpSettings(cfg config.ACP) dag.ACPSettings {
	out := dag.ACPSettings{Off: cfg.Off}
	if d, err := time.ParseDuration(strings.TrimSpace(cfg.Timeout)); err == nil {
		out.Timeout = d
	}
	if d, err := time.ParseDuration(strings.TrimSpace(cfg.PromptTimeout)); err == nil {
		out.PromptTimeout = d
	}
	if d, err := time.ParseDuration(strings.TrimSpace(cfg.CancelGrace)); err == nil {
		out.CancelGrace = d
	}
	return out
}

func appScope(app *harness.App) string {
	secs := app.ContextSections("authorization", "domain")
	var b strings.Builder
	for _, class := range []string{"authorization", "domain"} {
		if body := strings.TrimSpace(secs[class]); body != "" {
			b.WriteString(body + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// contextDocumentTouched reports whether an applied proposal set changed
// CONTEXT.md. A class-"skill" proposal writes its own SKILL.md (WP-3.7) and
// leaves the document untouched, so an apply that did nothing but promote skills
// must not rewrite it.
func contextDocumentTouched(applied []fctx.Proposal) bool {
	for _, p := range applied {
		if p.Class != fctx.ClassSkill {
			return true
		}
	}
	return false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
