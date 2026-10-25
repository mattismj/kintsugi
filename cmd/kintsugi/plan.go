package main

// `kintsugi plan` — the read-only planning turn (M9.2).
//
// The turn resolves a model, compiles the prompt and runs the agent loop on a
// read-only executor: every tool that could change the workspace (edit,
// write, apply_patch, bash, webfetch) asks the permission layer first, and
// the permission layer refuses them all. The plan lands in
// <workspace>/.kintsugi/plans/<slug>.md and the command prints where it
// went and what it says.
//
// Approving a plan is not a mode switch: it is asking the model to implement
// the plan in a normal turn.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"kintsugi/internal/agent"
	"kintsugi/internal/harness"
	"kintsugi/internal/intercept"
	"kintsugi/internal/plan"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
)

func cmdPlan(ws string, args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			cmdPlanList(ws, args[1:])
			return
		case "show":
			cmdPlanShow(ws, args[1:])
			return
		case "help", "-h", "--help":
			fmt.Print(planUsage())
			return
		}
	}

	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, planUsage()) }
	model := fs.String("model", "", "provider/model")
	persona := fs.String("persona", "", "persona name override")
	role := fs.String("role", "", "model role: default|coder|researcher|opsec")
	fs.Parse(args)

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		fmt.Fprint(os.Stderr, planUsage())
		os.Exit(2)
	}
	runPlanTurn(ws, task, *model, *persona, *role)
}

// planUsage is the command's help. It is a function so the text can be
// asserted in a test and printed from every exit path.
func planUsage() string {
	return `kintsugi plan — read-only planning

  kintsugi plan "<task>"       plan a task; the turn may only read
  kintsugi plan list           list the saved plans
  kintsugi plan show <name>    print one saved plan

  flags: --model provider/model · --persona name · --role default|coder|researcher|opsec

The planning turn refuses every mutating tool (edit, write, apply_patch, bash,
webfetch): the workspace cannot change while the plan is written. The plan is
saved under .kintsugi/plans/<slug>.md. Approving it means asking the model
to implement it in a normal turn.`
}

// planInstruction is the brief the planning turn runs under. It names the
// tools the turn may use and the shape of the document it must produce.
const planInstruction = `You are in plan mode. This turn is READ-ONLY.

Rules:
- Use only the read, list, glob and grep tools. Do not edit, write, patch or run commands.
- Do not change anything in the workspace.
- Investigate before you propose: read the files the task touches.

Write a Markdown plan and nothing else:
- open with a level-one heading: "# <short title>"
- then the steps, each one concrete and each naming the files it touches
- then a short "## Risks" section
- no preamble and no closing remarks.`

// planExecutor builds the executor the planning turn runs on.
//
// The turn may only read. Every tool that would change the workspace — edit,
// write, apply_patch, bash, webfetch — asks the permission layer first, and
// the permission layer refuses them all: the approver denies every tool, and
// the command policy denies every command outright. The subagent manager and
// the MCP servers are left off, so the plan turn has no way to reach anything
// but the read tools.
func planExecutor(app *harness.App, log *session.Session) *tools.Executor {
	policy := app.ToolPolicy()
	policy.Yolo = false
	// Every command is refused by the policy itself, before the approver is
	// even asked.
	policy.DenyCommands = append(policy.DenyCommands, ".*")

	ex := &tools.Executor{
		Workspace: app.Workspace,
		Policy:    policy,
		Offline:   app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(app.Cfg)
	if log != nil {
		ex.Log = log.Log
	}
	ex.Approver = tools.ApproverFunc(func(tools.ApprovalRequest) bool { return false })
	return ex
}

// runPlanTurn runs the planning turn and saves the plan it produced.
func runPlanTurn(ws, task, modelRef, persona, role string) {
	app := newApp(ws)

	var res *harness.Resolved
	var err error
	if modelRef == "" && role != "" {
		res, err = app.ResolveRole(role)
	} else {
		res, err = app.Resolve(modelRef)
	}
	if err != nil {
		fail("plan: %v", err)
	}
	comp, err := app.CompileRole(res, role, harness.CompileOpts{Persona: persona, Task: task})
	if err != nil {
		fail("plan: %v", err)
	}
	_ = prompt.SaveManifest(app.Workspace, comp.Manifest)

	log, err := session.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: session: %v\n", err)
	}
	if log != nil {
		defer log.Close()
	}

	ex := planExecutor(app, log)
	loop := &agent.Loop{
		Client:   res.Client,
		Tools:    ex,
		Log:      log,
		MaxLoops: app.Cfg.Harness.MaxToolLoops,
		OnDelta:  func(s string) { fmt.Print(s) },
		OnUsage:  func(p, c int) { app.RecordUsage(res.ProviderName, p, c) },
		OnTool: func(call provider.ToolCall, r tools.Result) {
			state := "✓"
			if r.Failed {
				state = "✗"
			}
			fmt.Printf("\n  ⚙ %s %s", call.Function.Name, state)
		},
	}
	if app.Cfg.Intercept.Enabled {
		if lx, err := app.RefusalLexicon(); err == nil {
			loop.Interceptor = lx
			loop.Reminder = app.ReminderBuilder(comp)
			loop.MaxRefusalRetries = app.Cfg.Intercept.MaxRetries
			// Operator stream rules (I-70): the same machinery, steered by the
			// operator's own patterns.
			if rules, err := app.StreamRules(); err == nil && len(rules.Ordered()) > 0 {
				loop.StreamRules = rules
				loop.OnRule = func(ev agent.RuleEvent) {
					if ev.Action == intercept.ActionRedact {
						fmt.Printf("\n  ▪ redacted %d span(s) (%s)\n", ev.Count, ev.Rule)
						return
					}
					fmt.Printf("\n  ▪ stream rule %s (%s) · reminder %d · %s\n", ev.Rule, ev.Action, ev.Level, ev.Outcome)
				}
			}
			loop.OnIntercept = func(hit intercept.Hit, level int, outcome string) {
				switch outcome {
				case "handoff":
					fmt.Printf("\n  ⚠ refusal persisted (%s) · handing back to operator\n", hit.Class)
				default:
					fmt.Printf("\n  ⚠ refusal intercepted (%s) · reminder %d injected · resuming\n", hit.Class, level)
				}
			}
		}
	}

	final, _, err := loop.RunConversation(context.Background(), comp.Text, nil,
		planInstruction+"\n\nTask:\n"+task)
	if err != nil {
		fail("plan: %v", err)
	}

	body := strings.TrimSpace(final)
	art, err := plan.Save(ws, planTitle(task), body)
	if err != nil {
		fail("plan: %v", err)
	}
	if body == "" {
		fmt.Printf("\n\n· the model produced no plan — wrote an empty plan to %s\n", art.Path)
		return
	}
	fmt.Printf("\n\n· plan written to %s\n\n", art.Path)
	for _, line := range planHead(art.Content, 12) {
		fmt.Println(line)
	}
}

// cmdPlanList prints the saved plans, newest first.
func cmdPlanList(ws string, args []string) {
	fs := flag.NewFlagSet("plan list", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, planUsage()) }
	fs.Parse(args)

	list, err := plan.List(ws)
	if err != nil {
		fail("plan list: %v", err)
	}
	if len(list) == 0 {
		fmt.Printf("no plans under %s\n", plan.Dir(ws))
		return
	}
	for _, a := range list {
		title := plan.Title(a.Content)
		if title == "" {
			title = plan.Summary(a.Content)
		}
		fmt.Printf("%-28s %s  %s\n", a.Name, a.Created.Local().Format("2006-01-02 15:04"), oneLine(title))
	}
	fmt.Printf("\nnewest %s\n", list[0].Path)
}

// cmdPlanShow prints one saved plan.
func cmdPlanShow(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi plan show <name>")
	}
	art, err := plan.Load(ws, args[0])
	if err != nil {
		fail("plan show: %v", err)
	}
	fmt.Print(art.Content)
}

// planTitle is the name a plan is filed under: the task's first line.
func planTitle(task string) string {
	t := strings.TrimSpace(task)
	if i := strings.IndexAny(t, "\r\n"); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	if len(t) > 60 {
		t = strings.TrimSpace(t[:60])
	}
	if t == "" {
		t = "plan"
	}
	return t
}

// planHead returns the first lines of a plan document, without the marker:
// the marker is a file-format detail, not something the operator reads.
func planHead(content string, n int) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == plan.Marker {
			continue
		}
		out = append(out, line)
		if len(out) >= n {
			break
		}
	}
	return out
}
