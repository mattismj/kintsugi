package main

// `kintsugi session ab` — the model A/B measurement tool: fork a session at a
// checkpoint, replay the same turn on two models, and compare the results.
//
// The runs are read-only (plan-mode tool policy): a replay must not mutate the
// workspace, so it compares the models' reasoning and code in their answers
// and the tools they reach for, not applied diffs. The original session is
// never touched; each run gets its own throwaway session, flagged in its meta
// so the operator can tell what it was.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"kintsugi/internal/agent"
	"kintsugi/internal/harness"
	"kintsugi/internal/intercept"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
)

// abRun is one model's replay result.
type abRun struct {
	Model      string
	Content    string
	ToolCalls  []string
	Prompt     int
	Completion int
	Elapsed    time.Duration
	Intercepts []string // outcome labels, in order
	Err        error
}

func cmdSessionAB(ws string, args []string) {
	fs := flag.NewFlagSet("session ab", flag.ExitOnError)
	at := fs.String("at", "", "checkpoint id to fork from (default: the newest)")
	modelA := fs.String("model-a", "", "first model ref (provider/model)")
	modelB := fs.String("model-b", "", "second model ref (provider/model)")
	turn := fs.String("turn", "", "the user turn to replay (default: the last user message in the checkpoint)")
	timeout := fs.Duration("timeout", 0, "per-model run timeout (default: [agents] timeout_s, then 180s)")
	jsonOut := fs.Bool("json", false, "print the results as JSON")
	fs.Parse(args)
	rest := fs.Args()
	// Flags may follow the session id: flag.Parse stops at the first
	// positional, so re-parse whatever follows it.
	id := ""
	if len(rest) > 0 {
		id = strings.TrimSpace(rest[0])
		if len(rest) > 1 {
			fs.Parse(rest[1:])
		}
	}
	if id == "" || *modelA == "" || *modelB == "" {
		fail("usage: kintsugi session ab <session-id> --model-a <p/m> --model-b <p/m> [--at <checkpoint>] [--turn \"task\"]")
	}

	app := newApp(ws)
	limit := *timeout
	if limit <= 0 {
		if app.Cfg.Agents.TimeoutS > 0 {
			limit = time.Duration(app.Cfg.Agents.TimeoutS) * time.Second
		} else {
			limit = 180 * time.Second
		}
	}

	// The fork point: the named checkpoint, else the newest.
	list, err := session.ListCheckpoints(id)
	if err != nil {
		fail("%v", err)
	}
	if len(list) == 0 {
		fail("session %s has no checkpoints — save one with `kintsugi checkpoint %s <label>` first", id, id)
	}
	cpID := strings.TrimSpace(*at)
	if cpID == "" {
		cpID = list[len(list)-1].ID // ListCheckpoints is oldest first
	}
	cp, err := session.LoadCheckpoint(id, cpID)
	if err != nil {
		fail("%v", err)
	}

	// The turn: explicit, or the last user message in the checkpoint.
	task := strings.TrimSpace(*turn)
	if task == "" {
		for _, m := range cp.Messages {
			if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
				task = m.Content
			}
		}
		if task == "" {
			fail("the checkpoint has no user turn — pass --turn \"<task>\"")
		}
	}
	history := append([]provider.Message(nil), cp.Messages...)

	// Both runs get the session's compiled prompt for fairness.
	base, err := app.Resolve("")
	if err != nil {
		fail("%v", err)
	}
	comp, err := app.Compile(base, harness.CompileOpts{})
	if err != nil {
		fail("%v", err)
	}

	runs := make([]abRun, 0, 2)
	for _, ref := range []string{*modelA, *modelB} {
		runs = append(runs, runAB(app, comp, ref, history, task, limit))
	}

	if *jsonOut {
		b, err := json.MarshalIndent(abJSON(runs), "", "  ")
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(string(b))
		return
	}
	fmt.Print(renderAB(runs))
}

// runAB executes one model's replay against a throwaway session.
func runAB(app *harness.App, comp *prompt.Compiled, ref string, history []provider.Message, task string, timeout time.Duration) abRun {
	res := abRun{Model: ref}
	r, err := app.Resolve(ref)
	if err != nil {
		res.Err = err
		return res
	}
	res.Model = r.ProviderName + "/" + r.Model.ID

	log, err := session.New()
	if err != nil {
		res.Err = err
		return res
	}
	defer log.Close()
	log.Log("meta", map[string]string{"event": "session_ab", "model": res.Model})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ex := &tools.Executor{
		Workspace: app.Workspace,
		Policy:    tools.Policy{Mode: string(tools.ModePlan)}, // read-only replay
		Log:       log.Log,
		Offline:   app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(app.Cfg)

	loop := &agent.Loop{
		Client:            r.Client,
		Tools:             ex,
		Log:               log,
		MaxLoops:          app.Cfg.Harness.MaxToolLoops,
		MaxRefusalRetries: app.Cfg.Intercept.MaxRetries,
		HardStop:          app.Cfg.Intercept.HardStop,
		HardThinking:      app.Cfg.Intercept.HardThinking,
	}
	if app.Cfg.Intercept.Enabled {
		if lx, err := app.RefusalLexicon(); err == nil {
			loop.Interceptor = lx
			loop.Reminder = app.ReminderBuilder(comp)
		}
	}
	loop.OnDelta = func(s string) { res.Content += s }
	loop.OnTool = func(tc provider.ToolCall, _ tools.Result) {
		res.ToolCalls = append(res.ToolCalls, tc.Function.Name)
	}
	loop.OnUsage = func(p, c int) { res.Prompt += p; res.Completion += c }
	loop.OnIntercept = func(_ intercept.Hit, _ int, outcome string) {
		res.Intercepts = append(res.Intercepts, outcome)
	}

	start := time.Now()
	_, _, err = loop.RunConversation(ctx, comp.Text, history, task)
	res.Elapsed = time.Since(start)
	if err != nil {
		res.Err = err
	}
	return res
}

// abJSON is the scriptable shape.
func abJSON(runs []abRun) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		m := map[string]any{
			"model":      r.Model,
			"content":    r.Content,
			"tool_calls": r.ToolCalls,
			"prompt":     r.Prompt,
			"completion": r.Completion,
			"elapsed_ms": r.Elapsed.Milliseconds(),
			"refusals":   r.Intercepts,
		}
		if r.Err != nil {
			m["error"] = r.Err.Error()
		}
		out = append(out, m)
	}
	return out
}

// renderAB is the human report: a summary table, the two outputs, a verdict.
func renderAB(runs []abRun) string {
	var b strings.Builder
	b.WriteString("model                          elapsed  tokens in/out  refusals\n")
	for _, r := range runs {
		name := r.Model
		if r.Err != nil {
			name += " (ERROR)"
		}
		fmt.Fprintf(&b, "%-30s %7.1fs  %5d/%-5d  %s\n",
			clipAB(name, 30), r.Elapsed.Seconds(), r.Prompt, r.Completion,
			strings.Join(r.Intercepts, ","))
	}
	for _, r := range runs {
		b.WriteString("\n=== " + r.Model + " ===\n")
		if r.Err != nil {
			fmt.Fprintf(&b, "error: %v\n", r.Err)
			continue
		}
		content := strings.TrimSpace(r.Content)
		if content == "" {
			content = "(no answer)"
		}
		b.WriteString(content + "\n")
		if len(r.ToolCalls) > 0 {
			b.WriteString("tools: " + strings.Join(r.ToolCalls, ", ") + "\n")
		}
	}
	if len(runs) == 2 && runs[0].Err == nil && runs[1].Err == nil {
		if strings.TrimSpace(runs[0].Content) == strings.TrimSpace(runs[1].Content) {
			b.WriteString("\nverdict: identical outputs\n")
		} else {
			b.WriteString("\nverdict: outputs differ — read both above\n")
		}
	}
	return b.String()
}

func clipAB(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}
