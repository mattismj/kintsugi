package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"kintsugi/internal/advisor"
	"kintsugi/internal/agent"
	"kintsugi/internal/audit"
	"kintsugi/internal/commands"
	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/intercept"
	"kintsugi/internal/memory"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
)

// plainMode is the mode a policy is in: the default mode when the policy
// names none.
func plainMode(p tools.Policy) tools.Mode {
	mode, err := tools.ParseMode(p.Mode)
	if err != nil {
		return tools.ModeDefault
	}
	return mode
}

type Options struct {
	AltScreen bool
	Plain     bool
	// Mouse enables mouse-wheel scrolling (tea cell-motion capture). Motion
	// tracking is gone with the gaze experiment: capture is wheel-only, so the
	// terminal's own drag-to-select keeps working beside the program.
	Mouse         bool
	ResumeHistory []provider.Message
	ResumeID      string
}

// ResolveMouse is the single place the mouse posture is decided, so the CLI, the
// model and the tests cannot each carry their own copy of the rule and drift
// apart. It is wheel-capture only: cell motion, no all-motion tracking.
func ResolveMouse(cfg config.UI, noMouse bool) (capture bool) {
	return cfg.Mouse && !noMouse
}

// Run starts the Bubble Tea program and returns when it exits.
func Run(app *harness.App, res *harness.Resolved, comp *prompt.Compiled, log *session.Session, mem *memory.Store, opts Options) error {
	fmt.Printf("operator@kintsugi:~$ session %s · model %s/%s\n", log.ID, res.ProviderName, res.Model.ID)
	model := New(app, res, comp, log, mem, opts.ResumeHistory, opts)
	defer model.Close()

	// Mouse capture is opt-out: it is added to both the alt-screen and the
	// inline program, so wheel scrolling works whether output stays in
	// scrollback or not. RunPlain never enables it.
	progOpts := []tea.ProgramOption{}
	if opts.AltScreen {
		progOpts = append(progOpts, tea.WithAltScreen())
	}
	// Cell-motion capture: the wheel comes through, and the terminal's own
	// drag-to-select (shift or not, depending on the terminal) keeps working.
	if opts.Mouse {
		progOpts = append(progOpts, tea.WithMouseCellMotion())
	}
	program := tea.NewProgram(model, progOpts...)

	_, err := program.Run()
	fmt.Printf("\n--- kintsugi %s ---\n", version)
	fmt.Printf("session %s → %s  (replay: kintsugi session show %s)\n", log.ID, log.Path, log.ID)
	if err == nil {
		// End-of-session curation: advisory only, never fails the run.
		harness.MaybeCurate(app, log.ID)
	}
	return err
}

// ShouldRunPlain reports whether the terminal cannot support the panel TUI.
func ShouldRunPlain(cfgPlain bool) bool {
	if cfgPlain || os.Getenv("KINTSUGI_PLAIN") == "1" {
		return true
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return true
	}
	return !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd())
}

// RunPlain is the no-TTY stream: one line in, one turn out, all state visible
// in scrollback. Same tools, permissions and interception as the panel TUI.
func RunPlain(app *harness.App, res *harness.Resolved, comp *prompt.Compiled, log *session.Session, mem *memory.Store, history []provider.Message) error {
	if app != nil && log != nil {
		app.SessionID = log.ID
	}
	// The session's live permission policy (M9). The mode inside it is
	// switched at runtime by /mode; the rules never change.
	policy := app.ToolPolicy()
	ex := &tools.Executor{
		Workspace: app.Workspace,
		Memory:    mem,
		Policy:    policy,
		Log:       log.Log,
		Offline:   app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(app.Cfg)
	// The audit log (M11.3): local, append-only, and never part of a prompt.
	sessionID := ""
	if log != nil {
		sessionID = log.ID
	}
	if err := ex.EnableAudit(app.Workspace, sessionID); err != nil {
		fmt.Fprintln(os.Stderr, "warn: audit log unavailable:", err)
	}
	defer app.AttachTools(ex, log.Log)()
	ex.OnTodo = func(items []tools.TodoItem) {
		fmt.Println("\n  plan:")
		for _, t := range items {
			mark := "[ ]"
			switch t.Status {
			case "in_progress":
				mark = "[>]"
			case "completed":
				mark = "[x]"
			case "cancelled":
				mark = "[-]"
			}
			fmt.Printf("  %s %s\n", mark, t.Content)
		}
	}
	reader := bufio.NewReader(os.Stdin)
	ex.Approver = tools.ApproverFunc(func(ar tools.ApprovalRequest) bool {
		fmt.Fprintf(os.Stderr, "\n  ⚠ %s: %s\n", ar.Tool, truncate(ar.Detail, 300))
		// The proposed change prints before the prompt: a plain-mode approval
		// is answered with the diff in front of the operator too.
		for _, fc := range ar.Changes {
			fmt.Fprintf(os.Stderr, "  %s %s (%d → %d lines)\n", fc.Kind, fc.Path,
				strings.Count(fc.Before, "\n")+1, strings.Count(fc.After, "\n")+1)
		}
		fmt.Fprintf(os.Stderr, "  approve for this session? [y/N] ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		}
		return false
	})

	loop := &agent.Loop{
		Client: res.Client, Tools: ex,
		// The hook gate lives in the loop, so the loop's logger is where a
		// hook denial is folded into the audit log.
		Log:      audit.WrapLogger(log, ex.Audit),
		MaxLoops: app.Cfg.Harness.MaxToolLoops,
	}
	if hr := app.HooksRunner(log.Log); hr != nil {
		loop.Hooks = hr
		loop.SessionID = log.ID
		loop.CWD = app.Workspace
		loop.Model = res.ProviderName + "/" + res.Model.ID
		loop.Persona = comp.Manifest.Persona.Name
	}
	if app.Cfg.Intercept.Enabled {
		if lx, err := app.RefusalLexicon(); err == nil {
			loop.Interceptor = lx
			loop.Reminder = app.ReminderBuilder(comp)
			loop.MaxRefusalRetries = app.Cfg.Intercept.MaxRetries
		}
	}
	if app.Cfg.Agents.Subagents {
		if mgr, err := app.TaskManagerWithRoles(res, comp, ex.MCP, log.Log); err == nil {
			ex.Tasks = mgr
		} else {
			fmt.Println("subagents unavailable:", err)
		}
	}
	if app.AdvisorEnabled() {
		if adv, err := app.AdvisorFor(res); err == nil {
			loop.Advisor = adv
			loop.MaxAdvisorRevisions = 1
			loop.OnAdvisor = func(notes []advisor.Note) {
				for _, n := range notes {
					fmt.Printf("\n  ◇ advisor (%s): %s\n", n.Kind, n.Text)
				}
			}
		} else {
			fmt.Println("advisor unavailable:", err)
		}
	}
	loop.OnUsage = func(p, c int) { app.RecordUsage(res.ProviderName, p, c) }
	loop.OnDelta = func(s string) { fmt.Print(s) }
	loop.OnTool = func(call provider.ToolCall, r tools.Result) {
		state := "✓"
		if r.Failed {
			state = "✗"
		}
		fmt.Printf("\n  %s %s %s\n", state, call.Function.Name, truncate(firstArg(call.Function.Arguments), 80))
	}
	loop.OnIntercept = func(hit intercept.Hit, level int, outcome string) {
		if outcome == "handoff" {
			fmt.Printf("\n  ⚠ refusal persisted (%s) · handing back to operator\n", hit.Class)
			return
		}
		fmt.Printf("\n  ⚠ refusal intercepted (%s) · reminder %d injected · resuming\n", hit.Class, level)
	}

	var hist []provider.Message
	hist = append(hist, history...)
	// Orientation (M10-C): a fresh session opens with the digest of what the
	// workspace already accepted plus its newest lessons. A resumed session
	// already carries its own history, so it is not re-told what it just said.
	// It reads positive ledger kinds only, so a refusal cannot be in it.
	startedFresh := len(hist) == 0
	if startedFresh && app.Cfg.Continuity.Enabled {
		if digest := strings.TrimSpace(app.MentalModel()); digest != "" {
			hist = append(hist, provider.Message{Role: "system", Content: digest})
			fmt.Printf("// %s · accepted work + newest lessons\n", memory.MentalModelLabel)
		}
	}
	var lastTask string
	// The expansion context for the custom commands and the context
	// references (M9.8). The policy is the one the bash tool uses, so a
	// `!` line in a command file is gated exactly like a shell command.
	opts := commands.Options{
		Workspace: app.Workspace,
		Policy:    policy,
	}
	fmt.Printf("--- kintsugi %s ---\n", version)
	fmt.Printf("session %s · %s/%s · persona %s\n", log.ID, res.ProviderName, res.Model.ID, comp.Manifest.Persona.Name)
	if len(hist) > 0 {
		fmt.Printf("resumed %d messages from session %s\n", len(hist), log.ID)
	}
	fmt.Println("type a task · /frame · /status · /mode · /recall · /ledger · /accept · /rewind · /commands · /help · /quit")

	for {
		fmt.Print("\noperator@kintsugi:~$ ")
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "/quit" || line == "/q":
			harness.MaybeCurate(app, log.ID)
			return nil
		case line == "/help":
			fmt.Println("/agents  /frame  /status  /mode [name]  /prompt  /model <ref>  /role <name>  /connect <provider>  /remember <text>  /recall [q]  /ledger [kind]  /accept <text>  /rewind [id]  /commands  /quit")
			continue
		case line == "/agents":
			persona := comp.Manifest.Persona.Name
			if comp.Manifest.Persona.Rule != "" {
				persona += " (" + comp.Manifest.Persona.Rule + ")"
			}
			fmt.Println("persona", persona)
			text, path := app.AgentsMD()
			if path == "" {
				fmt.Println("agents  none — add AGENTS.md at the workspace root or ~/.config/kintsugi/AGENTS.md")
			} else {
				fmt.Println("agents ", path)
				fmt.Println(strings.TrimSpace(text))
			}
			continue
		case line == "/status":
			fmt.Printf("session %s · %s\nstack %s · %d tok · refusals: see kintsugi refusal stats\n",
				log.ID, comp.Manifest.Persona.Name, shortHash(comp.Manifest.StackHash), comp.Manifest.Totals.Tokens)
			continue
		case line == "/mode" || strings.HasPrefix(line, "/mode "):
			// The mode is the baseline the permission matrix starts from: it
			// decides whether a mutating call runs, is asked about, or is
			// refused. yolo is reachable only by typing it — no keypress
			// switches the session into a posture that approves everything.
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/mode"))
			if arg == "" {
				fmt.Printf("mode    %s\nmodes   %s\n", plainMode(policy), modeList)
				continue
			}
			mode, err := tools.ParseMode(arg)
			if err != nil {
				fmt.Println("mode:", err)
				continue
			}
			from := plainMode(policy)
			policy.Mode = string(mode)
			// The yolo flag is the config's blunt version of the mode: it
			// would override every other mode, so it follows the mode.
			policy.Yolo = mode == tools.ModeYolo
			ex.Policy = policy
			opts.Policy = policy
			fmt.Printf("mode %s → %s\n", from, mode)
			_ = audit.Record(app.Workspace, audit.Event{
				SessionID: sessionID,
				Tool:      "mode",
				Detail:    string(mode),
				Decision:  audit.DecisionSwitch,
				Reason:    string(from) + " → " + string(mode),
			})
			continue
		case line == "/prompt":
			for _, l := range comp.Manifest.Layers {
				fmt.Printf("%-10s %5d  %s\n", l.ID, l.Tokens, shortHash(l.Hash))
			}
			continue
		case line == "/frame":
			frame := app.FrameText()
			if frame == "" {
				fmt.Println("no Authorization/Domain sections in CONTEXT.md")
				continue
			}
			hist = append(hist, provider.Message{Role: "system", Content: frame})
			fmt.Println("// frame · injected")
			continue
		case strings.HasPrefix(line, "/remember "):
			if mem == nil {
				fmt.Println("memory store unavailable")
				continue
			}
			if _, err := mem.AddLesson(strings.TrimPrefix(line, "/remember "), res.Model.Family, "session "+log.ID); err != nil {
				fmt.Println("remember:", err)
			} else {
				fmt.Println("lesson recorded")
			}
			continue
		case line == "/rewind" || strings.HasPrefix(line, "/rewind "):
			// Checkpoints (WP-2.2): list, or restore a saved history. The
			// rewind is a marker event; earlier turns are never rewritten.
			id := strings.TrimSpace(strings.TrimPrefix(line, "/rewind"))
			if id == "" {
				list, err := session.ListCheckpoints(log.ID)
				if err != nil {
					fmt.Println("rewind:", err)
					continue
				}
				if len(list) == 0 {
					fmt.Printf("no checkpoints · save one with `kintsugi checkpoint %s <label>`\n", log.ID)
					continue
				}
				for _, cp := range list {
					label := cp.Label
					if label == "" {
						label = "—"
					}
					fmt.Printf("%-26s %3d msgs  %s\n", cp.ID, len(cp.Messages), label)
				}
				fmt.Println("/rewind <id> restores that checkpoint's history")
				continue
			}
			cp, err := session.LoadCheckpoint(log.ID, id)
			if err != nil {
				fmt.Println("rewind:", err)
				continue
			}
			if err := session.MarkRewind(log.ID, cp.ID); err != nil {
				fmt.Println("rewind:", err)
				continue
			}
			hist = append([]provider.Message(nil), cp.Messages...)
			fmt.Printf("rewound to %s (%d messages)\n", cp.ID, len(cp.Messages))
			continue
		case strings.HasPrefix(line, "/role "):
			role := strings.TrimSpace(strings.TrimPrefix(line, "/role "))
			next, err := app.ResolveRole(role)
			if err != nil {
				fmt.Println("role:", err)
				continue
			}
			nextComp, err := app.CompileRole(next, role, harness.CompileOpts{})
			if err != nil {
				fmt.Println("role:", err)
				continue
			}
			for _, line := range prompt.Diff(comp.Manifest, nextComp.Manifest) {
				fmt.Println("  ·", line)
			}
			res, comp = next, nextComp
			loop.Client = next.Client
			loop.Reminder = app.ReminderBuilder(nextComp)
			fmt.Printf("role %s → %s · persona %s\n", role, next.ProviderName+"/"+next.Model.ID, nextComp.Manifest.Persona.Name)
			continue
		case strings.HasPrefix(line, "/model "):
			ref := strings.TrimSpace(strings.TrimPrefix(line, "/model "))
			next, err := app.Resolve(ref)
			if err != nil {
				fmt.Println("model:", err)
				continue
			}
			nextComp, err := app.Compile(next, harness.CompileOpts{})
			if err != nil {
				fmt.Println("model:", err)
				continue
			}
			for _, line := range prompt.Diff(comp.Manifest, nextComp.Manifest) {
				fmt.Println("  ·", line)
			}
			res, comp = next, nextComp
			loop.Client = next.Client
			loop.Reminder = app.ReminderBuilder(nextComp)
			fmt.Printf("switched to %s · persona %s\n", next.ProviderName+"/"+next.Model.ID, nextComp.Manifest.Persona.Name)
			continue
		case line == "/recall" || strings.HasPrefix(line, "/recall "):
			// Prior accepted work, on demand. Same recall as the panel TUI; the
			// ledger never holds a refusal, so this can only show accepted work.
			q := strings.TrimSpace(strings.TrimPrefix(line, "/recall"))
			if q == "" {
				q = lastTask
			}
			fmt.Println(strings.Join(plainRecall(app, q), "\n"))
			continue
		case line == "/ledger" || strings.HasPrefix(line, "/ledger "):
			kind := strings.TrimSpace(strings.TrimPrefix(line, "/ledger"))
			fmt.Println(strings.Join(plainLedger(app, kind), "\n"))
			continue
		case line == "/accept" || strings.HasPrefix(line, "/accept "):
			summary := strings.TrimSpace(strings.TrimPrefix(line, "/accept"))
			if summary == "" {
				fmt.Println("usage: /accept <what was accepted>")
				continue
			}
			entry, err := recordAcceptance(app, summary, log.ID, res.ProviderName+"/"+res.Model.ID, comp.Manifest.Persona.Name, lastTask)
			if err != nil {
				fmt.Println("accept:", err)
				continue
			}
			fmt.Printf("// continuity · acceptance recorded %s\n", entry.ID)
			continue
		case line == "/connect" || strings.HasPrefix(line, "/connect "):
			// Sign-in is a masked prompt with wheel-scrolled provider rows: it
			// needs the panel TUI. Plain mode hands the operator the CLI, which
			// reads the key through a hidden prompt — never from argv, so a key
			// typed after the provider name is refused rather than echoed.
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/connect"))
			if arg == "" {
				fmt.Println("interactive connect is TUI-only; run: kintsugi connect <provider>")
				continue
			}
			if strings.ContainsAny(arg, " \t") {
				fmt.Println("connect: a key is never read from the command line; run: kintsugi connect <provider>")
				continue
			}
			fmt.Printf("interactive connect is TUI-only; run: kintsugi connect %s\n", arg)
			continue
		case line == "/commands":
			// The custom commands of this workspace (M9.8).
			list := commands.Load(commands.Dirs(app.Workspace)...)
			if len(list) == 0 {
				fmt.Println("no custom commands — add .kintsugi/commands/<name>.md to the workspace")
				continue
			}
			for _, c := range list {
				hint := c.ArgumentHint
				if hint != "" {
					hint = " " + hint
				}
				fmt.Printf("%-24s %s\n", "/"+c.Name+hint, c.Description)
			}
			continue
		case strings.HasPrefix(line, "/"):
			// A custom command (M9.8) is resolved below, with the context
			// references: the plain REPL and the panel TUI behave the same.
		}

		// A custom slash command expands into the prompt; every line then has
		// its context references expanded, exactly once, on the final prompt.
		original := line
		prompt := line
		var notes []string
		if strings.HasPrefix(line, "/") {
			opts.Commands = commands.Load(commands.Dirs(app.Workspace)...)
			exp, ok := commands.ExpandCommand(line, opts)
			if !ok {
				fmt.Println("unknown command; try /help")
				continue
			}
			prompt, notes = exp.Prompt, exp.Notes
		}
		expanded := commands.ExpandReferences(prompt, opts)
		if prompt != original || len(expanded.Refs) > 0 || len(notes) > 0 {
			fmt.Println("operator ▸ " + original)
			for _, n := range notes {
				fmt.Println("· " + n)
			}
			for _, l := range strings.Split(expanded.Summary(), "\n") {
				if strings.TrimSpace(l) == "" {
					continue
				}
				fmt.Println("· " + strings.TrimSpace(l))
			}
			fmt.Println(expanded.Prompt)
		}
		line = expanded.Prompt

		started := time.Now()
		// Continuity, before the turn: the same local preflight and prior-work
		// recall the panel TUI performs, queued as system messages. loop.PriorWork
		// stays nil so the block is never injected twice.
		if inj := continuityForTask(app, res, line); len(inj.Messages) > 0 {
			hist = append(hist, inj.Messages...)
			if inj.Preflight {
				fmt.Printf("// preflight · frame injected (class %s)\n", inj.Classes)
			}
			if len(inj.Prior) > 0 {
				fmt.Printf("// continuity · %d prior accepted work items (%s)\n",
					len(inj.Prior), strings.Join(inj.Prior, ", "))
			}
		}
		lastTask = original
		_, updated, err := loop.RunConversation(context.Background(), comp.Text, hist, line)
		if err != nil {
			fmt.Printf("\n✗ %v\n", err)
			continue
		}
		hist = updated
		log.Log("turn", map[string]any{"seconds": time.Since(started).Seconds()})
	}
	// stdin closed: a clean end of the stream, same as /quit.
	harness.MaybeCurate(app, log.ID)
	return nil
}
