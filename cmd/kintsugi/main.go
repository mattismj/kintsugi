package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"kintsugi/internal/advisor"
	"kintsugi/internal/agent"
	"kintsugi/internal/audit"
	"kintsugi/internal/config"
	fctx "kintsugi/internal/context"
	"kintsugi/internal/continuity"
	"kintsugi/internal/credentials"
	"kintsugi/internal/doctor"
	"kintsugi/internal/harness"
	"kintsugi/internal/hooks"
	"kintsugi/internal/intercept"
	"kintsugi/internal/memory"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/skills"
	"kintsugi/internal/tools"
	"kintsugi/internal/tui"
)

// devVersion is the stamp every unstamped build reports: the milestone this
// tree is at, in the X.Y.Z-dev (Mxx) format the roadmap uses. It is the last
// resort of versionFrom, never the whole story.
const devVersion = "0.8.0-dev (M13c)"

// version is what `kintsugi version` prints. It is a var, not a const, because it
// is resolved at startup:
//
//  1. a release build stamps it at link time (.goreleaser.yaml passes
//     -X main.version={{ .Version }}), and the stamp wins untouched — the
//     linker only honours -X on a string with no dynamic initializer, so the
//     bare declaration below is load-bearing;
//  2. otherwise init() reads the build info the toolchain embedded in the
//     binary (module version, else the vcs revision);
//  3. and falls back to devVersion when neither says anything useful.
//
// internal/tui/model.go still carries its own `const version = "v0.8"` for the
// status-line stamp; it should read this shared value instead once a TUI-side
// hook is granted (M12-B deviation, reported).
var version string

func init() {
	if version != "" {
		return // stamped by -X main.version at link time
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		version = devVersion
		return
	}
	version = versionFrom(info)
}

// pseudoVersion matches the versions the go command invents for an untagged
// checkout (v0.0.0-20261021025354-df884f9f1270, optionally +dirty). Those are
// not releases: reporting one instead of the milestone stamp would hide which
// milestone a dev build came from.
var pseudoVersion = regexp.MustCompile(`-\d{14}-[0-9a-f]{12}($|\+)`)

// releaseVersion matches a tagged module version: vX.Y.Z with an optional
// pre-release suffix.
var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// versionFrom resolves the stamp from embedded build info. The three shapes it
// produces are documented in version_test.go, which pins them:
//
//	tagged release  1.4.0            (leading v trimmed; -X stamp equivalent)
//	dev + vcs       0.8.0-dev (M13a) df884f9f1270[-dirty]
//	dev, no vcs     0.8.0-dev (M13a)
func versionFrom(info *debug.BuildInfo) string {
	if info == nil {
		return devVersion
	}
	if v := info.Main.Version; releaseVersion.MatchString(v) && !pseudoVersion.MatchString(v) {
		return strings.TrimPrefix(v, "v")
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) < 12 {
		return devVersion
	}
	stamp := devVersion + " " + rev[:12]
	if dirty {
		stamp += "-dirty"
	}
	return stamp
}

func main() {
	ws, _ := os.Getwd()
	if v := os.Getenv("KINTSUGI_WORKSPACE"); v != "" {
		ws = v
	}

	cmd := "tui"
	rest := os.Args[1:]
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}

	switch cmd {
	case "tui":
		cmdTUI(ws, rest)
	case "run":
		cmdRun(ws, rest)
	case "plan":
		cmdPlan(ws, rest)
	case "hooks":
		cmdHooks(ws, rest)
	case "audit":
		cmdAudit(ws, rest)
	case "prompt":
		if len(rest) > 0 && rest[0] == "test" {
			cmdPromptTest(ws, rest[1:])
			return
		}
		cmdPrompt(ws, rest)
	case "models":
		cmdModels(ws, rest)
	case "pack":
		cmdPack(ws, rest)
	case "skills":
		cmdSkills(ws, rest)
	case "context":
		cmdContext(ws, rest)
	case "ledger":
		cmdLedger(ws, rest)
	case "taste":
		cmdTaste(ws, rest)
	case "recall":
		cmdRecall(ws, rest)
	case "persona":
		cmdPersona(ws, rest)
	case "memory":
		cmdMemory(ws, rest)
	case "session":
		cmdSession(ws, rest)
	case "doctor":
		cmdDoctor(ws, rest)
	case "repair":
		cmdRepair(rest)
	case "refusal":
		cmdRefusal(ws, rest)
	case "gate":
		cmdGate(ws, rest)
	case "dag":
		cmdDag(ws, rest)
	case "rules":
		cmdRules(ws, rest)
	case "quota":
		cmdQuota(ws, rest)
	case "report":
		cmdReport(ws, rest)
	case "init":
		cmdInit(ws, rest)
	case "connect":
		cmdConnect(ws, rest)
	case "setup":
		cmdSetup(ws, rest)
	case "new":
		cmdNew(ws, rest)
	case "validate":
		cmdValidate(ws, rest)
	case "checkpoint":
		cmdCheckpoint(ws, rest)
	case "rewind":
		cmdRewind(ws, rest)
	case "kb":
		cmdKB(ws, rest)
	case "lab":
		cmdLab(ws, rest)
	case "mods":
		cmdMods(ws, rest)
	case "rpc":
		cmdRPC(ws, rest)
	case "docs":
		cmdDocs(ws, rest)
	case "branding":
		cmdBranding(ws, rest)
	case "commit":
		cmdCommit(ws, rest)
	case "review":
		cmdReview(ws, rest)
	case "loop":
		cmdLoop(ws, rest)
	case "schedule":
		cmdSchedule(ws, rest)
	case "version", "-v", "--version":
		fmt.Println("kintsugi " + version)
	case "help", "-h", "--help":
		usage()
	default:
		if strings.HasPrefix(cmd, "-") {
			cmdTUI(ws, append([]string{cmd}, rest...))
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`kintsugi — privacy-first agent harness for open-weights models

  kintsugi                     interactive session
  kintsugi tui [flags]         interactive session flags: --no-alt-screen (keep scrollback),
                             --no-mouse (wheel scrolling off), --plain (line REPL), --resume <id>
  kintsugi run "<task>"        one task to completion (streams to stdout)
  kintsugi run --resume <id>   continue a session with a new task
  kintsugi run --architect     plan first on an architect model (read-only), then edit
                             with the coder model; the plan is saved under
                             .kintsugi/plans/ and injected into the edit turn
  kintsugi run --architect-model provider/model
                             model for the architect phase (default: the opsec role's model)
  kintsugi run --output-format json|stream-json
                             headless output: one JSON object at the end, or
                             one JSON object per line while the run happens
  kintsugi plan "<task>"       read-only planning turn; writes .kintsugi/plans/<slug>.md
  kintsugi plan list           list the saved plans
  kintsugi plan show <name>    print one saved plan
  kintsugi hooks trust        allow this workspace's project hooks to run
  kintsugi hooks list         operator and project hooks, with trust state
  kintsugi hooks status       whether this workspace is trusted
  kintsugi hooks untrust      revoke this workspace's trust marker
  kintsugi audit              permission decisions in this workspace (last 50)
  kintsugi audit --json       the same events as JSON
  kintsugi audit --clear      truncate the audit log (asks first)
  kintsugi lab up|down|status bring up / tear down / inspect the compose lab
  kintsugi mods list          mod manifests in this workspace, with trust state
  kintsugi mods trust         allow this workspace's project mods to run
  kintsugi mods untrust       revoke this workspace's trust marker
  kintsugi mods call <mod> <tool> <json>
                           start one mod and call one of its tools
  kintsugi commit              dry-run atomic commit groups from the working tree
                           --apply commits them after confirmation; never pushes
  kintsugi review [<ref>]      read-only code review of a diff (--json, --ship-check)
  kintsugi loop [interval] "<task>"
                            run once now, then every interval until a check
                            cannot help (--max N, --keep-going, --quiet)
  kintsugi schedule add "<cron>" "<task>"   store a scheduled job (5-field cron)
  kintsugi schedule list       jobs with state, next and last firing
  kintsugi schedule trigger <id>  fire one job now, cron untouched
  kintsugi schedule pause|resume|remove <id> / schedule run
                            control the schedule, or run the scheduler in the
                            foreground until ctrl-c
  kintsugi rpc [--mode stdio|tcp|http]
                           JSON-RPC loopback API (stdio|tcp|http; loopback only)
  kintsugi docs [<cmd>]        print a command's man page / reference doc
  kintsugi branding wordmark   the block FORGE wordmark, ramp-shaded
  kintsugi branding sigil      the compact anvil mark
  kintsugi branding palette    every design token: name, hex, 256/16 fallbacks, swatches
  kintsugi branding export <dir>  write wordmark.svg, sigil.svg, palette.svg into dir
  kintsugi prompt show         compile + print the prompt manifest
  kintsugi prompt diff         diff this compile against the last one
  kintsugi prompt show --text  print the compiled prompt itself
  kintsugi models             providers and models from config
  kintsugi models import <f>  merge a model catalog (--write to persist)
  kintsugi models fetch <p>   fetch a provider's /models list (--write to persist)
  kintsugi pack list           personas in the pack and their match rules
  kintsugi skills list         workspace skills (name, description, path)
  kintsugi skills show <name>  print a skill's full body
  kintsugi kb build <stix> <out>  build a KB from a MITRE CTI STIX bundle
  kintsugi kb reindex <dir>    rebuild the derived BM25 index
  kintsugi kb search <dir> "q" rank chunks with BM25 (--top 8)
  kintsugi kb list <dir>       base id/version/title + chunk count
  kintsugi context init        create CONTEXT.md (authorization template picked interactively)
  kintsugi context show        sections, hashes, gated markers
  kintsugi context why "q"     which injected context line says this
  kintsugi context diff        review curator proposals
  kintsugi context apply       apply auto-accepted proposals (--all, --gated)
  kintsugi context curate      run the curator over a session
  kintsugi context refresh     rebuild the Current state section from signals (--apply)
  kintsugi context attest      record an operator review of Authorization & scope
  kintsugi ledger list         accepted work, newest first (--kind, --json)
  kintsugi ledger add          append one accepted plan/artifact/decision entry
  kintsugi ledger export       write the ledger as JSONL (--file)
  kintsugi taste list          operator taste bullets, project + global (--json)
  kintsugi taste open          print the taste file path (and content)
  kintsugi taste lint          reject bullets without a date or kind
  kintsugi recall "<query>"    prior accepted work for a query (--top 5)
  kintsugi persona triggers    propose trigger-map entries from refusal telemetry (--apply)
  kintsugi memory add "..."    append a lesson to .kintsugi/learned.md
  kintsugi memory search "q"   search lessons + skills (FTS5)
  kintsugi memory reindex      rebuild the derived index
  kintsugi repair stats        show (model, tool, class) repair counters
  kintsugi gate review         read-only opsec review of a diff (--diff, --blocking)
  kintsugi dag run <plan.yaml> run a node plan with gates and revise cycles
  kintsugi rules scan          find existing AGENTS.md/.clinerules/.cursor rules
  kintsugi rules import        report only; --apply writes the inherited layer
  kintsugi quota               offline token usage vs configured limits
  kintsugi report [--out f]    plain-text findings report (findings, telemetry, quota)
  kintsugi init [--force]      draft CONTEXT.md + kintsugi.toml from the tree
  kintsugi connect --list      providers, endpoints and key status (env/stored/missing)
  kintsugi connect <provider>  verify a key against the endpoint, then store it (0600)
                            flags: --no-verify · --key-stdin · --delete
  kintsugi setup [--model ref] install the persona pack + global AGENTS.md/config
  kintsugi new [<blueprint>] <dir>  scaffold a project from a blueprint (--force)
  kintsugi validate            live battery per provider/model (--provider, --model, --timeout)
  kintsugi refusal stats       show refusal interception rates by model/persona
  kintsugi refusal trends      refusal + drift trends, fresh vs resumed sessions
  kintsugi session show <id>   replay a session log
  kintsugi session list        index sessions by title/model/persona/turns
  kintsugi session rename <id> "<title>"  set a session's title
  kintsugi session search <q>  search transcripts
  kintsugi session fork <id>   copy a session into a new branch
  kintsugi session export <id> write a session as legacy JSONL
  kintsugi checkpoint <id> [label]    snapshot a session's history as a rewind point
  kintsugi checkpoint list <id>       checkpoints: id, ts, label, message count
  kintsugi rewind <id> <ckpt-id>      mark a session rewound to a checkpoint
  kintsugi doctor [--net]      environment readiness

Environment:
  KINTSUGI_MODEL, KINTSUGI_MODE, KINTSUGI_CONFIG
  provider keys: named by api_key_env in the registry (run kintsugi models)
`)
}

func newApp(ws string) *harness.App {
	app, err := harness.New(ws)
	if err != nil {
		fail("%v", err)
	}
	return app
}

func cmdTUI(ws string, args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	noAlt := fs.Bool("no-alt-screen", false, "keep output in scrollback instead of the alternate screen")
	noMouse := fs.Bool("no-mouse", false, "disable mouse-wheel scrolling (mouse capture off)")
	plain := fs.Bool("plain", false, "force the line-based no-TTY stream")
	resume := fs.String("resume", "", "session id to continue")
	fs.Parse(args)

	app := newApp(ws)
	ref, persona := "", ""
	if *resume != "" {
		info, err := session.InfoOf(*resume)
		if err != nil {
			fail("%v", err)
		}
		ref, persona = info.Model, info.Persona
	}
	res, err := app.Resolve(ref)
	if err != nil {
		fail("%v", err)
	}
	// A missing key must not stop the TUI from opening: the operator needs to
	// see the interface (and `/connect`, `/model`, the ladder) to fix it. The
	// key is required at request time by the provider client's requireKey, so a
	// turn still fails cleanly with the env-var name if they try without one.
	comp, err := app.Compile(res, harness.CompileOpts{Persona: persona})
	if err != nil {
		fail("%v", err)
	}
	if err := prompt.SaveManifest(app.Workspace, comp.Manifest); err != nil {
		fmt.Fprintf(os.Stderr, "warn: manifest not saved: %v\n", err)
	}

	var log *session.Session
	var history []provider.Message
	if *resume != "" {
		log, err = session.Open(*resume)
		if err != nil {
			fail("%v", err)
		}
		history, err = session.History(*resume)
		if err != nil {
			fail("%v", err)
		}
	} else {
		log, err = session.New()
		if err != nil {
			fail("session: %v", err)
		}
		log.Log("meta", map[string]string{
			"model":   res.ProviderName + "/" + res.Model.ID,
			"persona": comp.Manifest.Persona.Name,
			"adapter": comp.Manifest.Adapter,
			"stack":   comp.Manifest.StackHash,
		})
	}
	defer log.Close()
	app.SessionID = log.ID

	mem, _ := app.MemoryStore()
	if mem != nil {
		defer mem.Close()
	}
	// ResolveMouse is the one place the mouse posture is decided: wheel
	// capture only, no all-motion tracking.
	capture := tui.ResolveMouse(app.Cfg.UI, *noMouse)
	opts := tui.Options{
		AltScreen:     app.Cfg.UI.AltScreen && !*noAlt,
		Mouse:         capture,
		Plain:         *plain || app.Cfg.UI.Plain || tui.ShouldRunPlain(false),
		ResumeHistory: history,
		ResumeID:      *resume,
	}
	if opts.Plain {
		if err := tui.RunPlain(app, res, comp, log, mem, history); err != nil {
			fail("tui: %v", err)
		}
		return
	}
	if err := tui.Run(app, res, comp, log, mem, opts); err != nil {
		fail("tui: %v", err)
	}
}

func cmdRun(ws string, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	model := fs.String("model", "", "provider/model")
	persona := fs.String("persona", "", "persona name override")
	profile := fs.String("profile", "", "behavior profile")
	yolo := fs.Bool("yolo", false, "skip tool approvals for this session (dangerous)")
	frame := fs.Bool("frame", false, "inject the Authorization & Domain context once before the task")
	role := fs.String("role", "", "model role: default|coder|researcher|opsec")
	resume := fs.String("resume", "", "session id to continue")
	outputFormat := fs.String("output-format", "text", "output format: text|json|stream-json")
	architect := fs.Bool("architect", false, "plan first on an architect model, then edit with the coder model")
	architectModel := fs.String("architect-model", "", "provider/model for the architect phase (default: the opsec role's model)")
	fs.Parse(args)

	switch *outputFormat {
	case session.ModeText, session.ModeJSON, session.ModeStreamJSON:
	default:
		fmt.Fprintf(os.Stderr, "unknown --output-format %q (want text|json|stream-json)\n", *outputFormat)
		os.Exit(2)
	}

	// The headless emitter (M9.5). In text mode it writes nothing and the
	// plain stream below is printed exactly as before; in the JSON modes it
	// owns stdout, so every other line goes to stderr instead.
	em := session.NewEmitter(os.Stdout, *outputFormat)
	defer em.Close()

	// abort ends the run with a failure. In a JSON mode the failure travels
	// inside the JSON document and stdout carries no prose at all.
	abort := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if em.Enabled() {
			_ = em.Emit(session.Event{Type: session.EventError, Error: msg, Ended: stampNow()})
			_ = em.Close()
			os.Exit(1)
		}
		fail("%s", msg)
	}

	// note prints an informational line. A JSON mode keeps stdout for JSON
	// only, so the line goes to stderr instead.
	note := func(format string, a ...any) {
		if em.Enabled() {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
			return
		}
		fmt.Printf(format+"\n", a...)
	}

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		fmt.Fprintln(os.Stderr, "usage: kintsugi run [--model provider/model] [--resume <id>] \"<task>\"")
		os.Exit(2)
	}

	var resumed session.Info
	if *resume != "" {
		info, err := session.InfoOf(*resume)
		if err != nil {
			abort("%v", err)
		}
		resumed = info
		if *model == "" {
			*model = info.Model
		}
		if *persona == "" {
			*persona = info.Persona
		}
	}

	app := newApp(ws)
	var res *harness.Resolved
	var err error
	if *model == "" && *role != "" {
		res, err = app.ResolveRole(*role)
	} else {
		res, err = app.Resolve(*model)
	}
	if err != nil {
		abort("%v", err)
	}
	comp, err := app.CompileRole(res, *role, harness.CompileOpts{Persona: *persona, Profile: *profile})
	if err != nil {
		abort("%v", err)
	}
	_ = prompt.SaveManifest(app.Workspace, comp.Manifest)

	var log *session.Session
	var history []provider.Message
	if *resume != "" {
		log, err = session.Open(*resume)
		if err != nil {
			abort("%v", err)
		}
		history, err = session.History(*resume)
		if err != nil {
			abort("%v", err)
		}
	} else {
		log, err = session.New()
		if err != nil {
			abort("session: %v", err)
		}
		log.Log("meta", map[string]string{
			"model":   res.ProviderName + "/" + res.Model.ID,
			"persona": comp.Manifest.Persona.Name,
			"role":    *role,
			"stack":   comp.Manifest.StackHash,
		})
	}
	defer log.Close()

	// The session title (M9.7) is derived from the operator's own first task,
	// locally: a heuristic, never a model call. It is set before the frame is
	// injected so the authorization text never ends up in the title, and a
	// session that already has one keeps it.
	log.SetTitle(session.DeriveTitle(task))

	// The session header is the first event of the JSON stream.
	started := stampNow()
	if !resumed.Started.IsZero() {
		started = resumed.Started.UTC().Format(time.RFC3339Nano)
	}
	_ = em.Emit(session.Event{
		Type:      session.EventSession,
		SessionID: log.ID,
		Model:     res.ProviderName + "/" + res.Model.ID,
		Persona:   comp.Manifest.Persona.Name,
		Role:      *role,
		Stack:     comp.Manifest.StackHash,
		Started:   started,
		Mode:      app.Cfg.Core.Mode,
	})

	// A rewind marker makes the checkpoint the resume history.
	if *resume != "" {
		if cp, ok, err := session.LatestRewind(*resume); err == nil && ok {
			history = cp.Messages
			note("rewound to %s (%d messages)", cp.ID, len(history))
		}
	}
	// Compact an over-long history before the first turn.
	hookRunner := app.HooksRunner(log.Log)
	if hookRunner != nil {
		_, _ = hookRunner.Fire(context.Background(), hooks.Event{Name: hooks.PreCompact, SessionID: log.ID, CWD: app.Workspace})
	}
	if compacted, ok, err := app.CompactHistory(context.Background(), history); err != nil {
		fmt.Fprintf(os.Stderr, "warn: compaction: %v\n", err)
	} else if ok {
		history = compacted
		note("history compacted to %d messages", len(history))
		if hookRunner != nil {
			_, _ = hookRunner.Fire(context.Background(), hooks.Event{Name: hooks.PostCompact, SessionID: log.ID, CWD: app.Workspace})
		}
	}

	mem, _ := app.MemoryStore()
	if mem != nil {
		defer mem.Close()
	}

	note("%s session %s · %s/%s · persona %s\n", "operator@kintsugi:~$", log.ID, res.ProviderName, res.Model.ID, comp.Manifest.Persona.Name)

	// Continuity (M8.5), before the first turn: a local preflight decides whether
	// the operator frame is worth injecting, and prior *accepted* work is recalled
	// from the workspace ledger. Both blocks are prepended as system messages and
	// the loop's own PriorWork hook stays nil, so nothing is injected twice.
	// A refusal is never memory: neither block can carry one — the preflight sees
	// trigger terms and class counters, the recall sees accepted entries only.
	if app.Cfg.Continuity.Enabled {
		var prefix []provider.Message
		// Orientation before recall (M10-C): the mental-model digest is what the
		// workspace already holds, the prior-work block is what matches this ask.
		// Both are first-turn-only — a resumed session already carries its own
		// history and would be re-told what it just said.
		if len(history) == 0 {
			if digest := strings.TrimSpace(app.MentalModel()); digest != "" {
				prefix = append(prefix, provider.Message{Role: "system", Content: digest})
				note("mental model · accepted work + newest lessons (%d bytes)", len(digest))
			}
		}
		if pre, risk := app.PreflightFor(res, task); strings.TrimSpace(pre) != "" {
			if block := continuity.Block(pre); block != "" {
				prefix = append(prefix, provider.Message{Role: "system", Content: block})
			}
			note("preflight · frame injected (class %s)", classLabel(risk.Classes))
		}
		if prior, ids, err := app.RecallPriorWork(task); err != nil {
			fmt.Fprintf(os.Stderr, "warn: continuity recall: %v\n", err)
		} else if strings.TrimSpace(prior) != "" {
			if block := continuity.Block(prior); block != "" {
				prefix = append(prefix, provider.Message{Role: "system", Content: block})
			}
			note("continuity · %d prior accepted work items (%s)", len(ids), strings.Join(ids, ", "))
		}
		if len(prefix) > 0 {
			history = append(prefix, history...)
		}
	}

	policy := app.ToolPolicy()
	if *yolo {
		policy.Mode = string(tools.ModeYolo)
	}
	ex := &tools.Executor{
		Workspace: app.Workspace,
		Memory:    mem,
		Policy:    policy,
		Log:       runLog(log, em),
		Offline:   app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(app.Cfg)
	if err := ex.EnableAudit(app.Workspace, log.ID); err != nil {
		fmt.Fprintf(os.Stderr, "warn: audit log: %v\n", err)
	} else {
		defer ex.Audit.Close()
	}
	closeTools := app.AttachTools(ex, log.Log)
	defer closeTools()
	ex.OnTodo = func(items []tools.TodoItem) {
		if em.Enabled() {
			return
		}
		fmt.Print("\n  plan:")
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
			fmt.Printf(" %s %s", mark, t.Content)
		}
		fmt.Println()
	}
	if app.Cfg.Agents.Subagents {
		if mgr, err := app.TaskManagerWithRoles(res, comp, ex.MCP, log.Log); err == nil {
			ex.Tasks = mgr
		} else {
			fmt.Fprintf(os.Stderr, "warn: subagents: %v\n", err)
		}
	}
	if !policy.Yolo {
		ex.Approver = promptApprover()
	}

	loop := &agent.Loop{
		Client:   res.Client,
		Tools:    ex,
		Log:      audit.WrapLogger(runLogger{inner: log, emit: loopEvents(em)}, ex.Audit),
		MaxLoops: app.Cfg.Harness.MaxToolLoops,
		OnDelta: func(s string) {
			// The JSON modes carry the assistant text in the assistant
			// event; the plain stream still streams the deltas.
			if em.Enabled() {
				return
			}
			fmt.Print(s)
		},
		OnUsage: func(p, c int) {
			app.RecordUsage(res.ProviderName, p, c)
			_ = em.Emit(session.Event{Type: session.EventUsage, PromptTokens: p, CompletionTokens: c})
		},
		OnTool: func(call provider.ToolCall, r tools.Result) {
			if em.Enabled() {
				_ = em.Emit(session.Event{Type: session.EventToolCall, Name: call.Function.Name, Args: call.Function.Arguments})
				_ = em.Emit(session.Event{Type: session.EventToolResult, Name: call.Function.Name, Result: r.Content, Failed: r.Failed})
				return
			}
			args := call.Function.Arguments
			if len(args) > 120 {
				args = args[:120] + "…"
			}
			fmt.Printf("\n  ⚙ %s %s", call.Function.Name, args)
			if r.Failed {
				fmt.Print("  ✗")
			}
		},
	}
	loop.CompactAfter = app.Cfg.Harness.CompactAfter
	loop.CompactKeepRatio = app.Cfg.Harness.CompactKeepRatio
	loop.Summarize = app.Summarizer()
	if hr := app.HooksRunner(log.Log); hr != nil {
		loop.Hooks = hr
		loop.SessionID = log.ID
		loop.CWD = app.Workspace
		loop.Model = res.ProviderName + "/" + res.Model.ID
		loop.Persona = comp.Manifest.Persona.Name
	}
	if app.Cfg.Intercept.Enabled {
		if lx, err := app.RefusalLexicon(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: refusal lexicon: %v\n", err)
		} else {
			loop.Interceptor = lx
			loop.Reminder = app.ReminderBuilder(comp)
			loop.MaxRefusalRetries = app.Cfg.Intercept.MaxRetries
			// Operator stream rules (I-70). The headless path emits one JSON event
			// per rule application so a CI consumer sees scrubs and interrupts
			// without parsing prose.
			if rules, err := app.StreamRules(); err == nil && len(rules.Ordered()) > 0 {
				loop.StreamRules = rules
				loop.OnRule = func(ev agent.RuleEvent) {
					_ = em.Emit(session.Event{Type: session.EventStreamRule, Rule: ev.Rule, Action: ev.Action, Reason: ev.Reason, Count: ev.Count})
				}
			}
			loop.OnIntercept = func(hit intercept.Hit, level int, outcome string) {
				_ = em.Emit(session.Event{Type: session.EventRefusal, Class: hit.Class, Level: level, Outcome: outcome})
				if em.Enabled() {
					return
				}
				switch outcome {
				case "handoff":
					fmt.Printf("\n  ⚠ refusal persisted (%s) · handing back to operator\n", hit.Class)
				default:
					fmt.Printf("\n  ⚠ refusal intercepted (%s) · reminder %d injected · resuming\n", hit.Class, level)
				}
			}
		}
	}

	if app.AdvisorEnabled() {
		if adv, err := app.AdvisorFor(res); err == nil {
			loop.Advisor = adv
			loop.MaxAdvisorRevisions = 1
			loop.OnAdvisor = func(notes []advisor.Note) {
				for _, n := range notes {
					note("\n  ◇ advisor (%s): %s", n.Kind, n.Text)
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "warn: advisor: %v\n", err)
		}
	}

	// Continuity guard (M8.5.3): on a drift, re-inject the accepted work the
	// ledger already holds for this task and record the drift as metadata. The
	// guard receives the task and the telemetry class label only — never the
	// model's output — so a refusal cannot be echoed back into a prompt, and the
	// drift row carries a task hash, matched ids, a class and an outcome.
	// loop.PriorWork stays nil: the block was already pre-injected above.
	if app.Cfg.Continuity.Enabled && app.Cfg.Continuity.GuardOnRefusal {
		if store, err := app.LedgerStore(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: continuity guard: %v\n", err)
		} else {
			defer store.Close()
			guard := &continuity.Guard{
				Store:  store,
				TopK:   app.Cfg.Continuity.PriorWorkTopK,
				Budget: app.Cfg.Continuity.PriorWorkBudgetTokens,
				Log:    log.Log,
			}
			loop.GuardRefusal = guard.OnRefusal
			loop.OnDrift = func(task, class, outcome string, matched []string) {
				// Drift means a refusal contradicted prior accepted work. A
				// refusal with no match is ordinary telemetry, not drift: do not
				// record it as memory-adjacent metadata.
				if len(matched) == 0 {
					return
				}
				_ = em.Emit(session.Event{Type: session.EventDrift, Class: class, Outcome: outcome, IDs: matched})
				if err := app.RecordDrift(task, class, outcome, matched); err != nil {
					fmt.Fprintf(os.Stderr, "warn: drift: %v\n", err)
					return
				}
				// Telemetry only: class + outcome, never the model's prose.
				log.Log("drift", map[string]string{"class": class, "outcome": outcome})
				note("\n  continuity · drift metadata recorded (%s)", idsLabel(matched))
			}
		}
	}

	if *frame {
		if frame := app.FrameText(); frame != "" {
			task = frame + "\n\n" + task
		} else {
			fmt.Fprintln(os.Stderr, "warn: no Authorization/Domain sections found in CONTEXT.md")
		}
	}

	// Mid-session compaction (M10.3): when the conversation grows past the
	// configured threshold, the older turns are summarized and the recent
	// ones are kept verbatim. The summary replaces the oldest turns, so the
	// compiled system prompt — the cached prefix — is never rewritten, and a
	// failed summary leaves the history untouched.
	compactOpts := app.CompactSettings()
	turn := &agent.CompactLoop{
		Loop:             loop,
		CompactAfter:     compactOpts.After,
		CompactKeepRatio: compactOpts.KeepRatio,
		Summarize:        agent.NewSummarizer(res.Client),
		OnCompact: func(covered, kept int) {
			note("· compacted: %d older turns summarized, %d kept", covered, kept)
		},
	}

	var final string
	if *architect {
		// The architect/editor split (M10.2): a read-only plan phase on the
		// architect model, then the edit phase on the coder model with the
		// plan injected as context.
		out, err := app.RunArchitect(context.Background(), harness.ArchitectOptions{
			Task:           task,
			ArchitectModel: *architectModel,
			Coder:          res,
			CoderLoop:      turn,
			Wiring:         turn.Loop,
			Compiled:       comp,
			History:        history,
			Persona:        *persona,
			Profile:        *profile,
			Note:           note,
			OnTool:         loop.OnTool,
		})
		if err != nil {
			if em.Enabled() {
				abort("%v", err)
			}
			fail("\n✗ %v", err)
		}
		final = out.Final
	} else {
		final, _, err = turn.RunConversation(context.Background(), comp.Text, history, task)
		if err != nil {
			if em.Enabled() {
				abort("%v", err)
			}
			fail("\n✗ %v", err)
		}
	}
	_ = em.Emit(session.Event{Type: session.EventFinal, Text: final, Ended: stampNow()})
	note("\n\n· replay: kintsugi session show %s · stack %s", log.ID, comp.Manifest.StackHash)
	if err := em.Close(); err != nil {
		fail("%v", err)
	}

	harness.MaybeCurate(app, log.ID)
}

// runLogger tees the agent loop's session events into the JSON emitter. The
// session log stays the source of truth; the emitter is only a second reader.
type runLogger struct {
	inner agent.Logger
	emit  func(kind string, v any)
}

func (l runLogger) Log(kind string, v any) {
	if l.inner != nil {
		l.inner.Log(kind, v)
	}
	if l.emit != nil {
		l.emit(kind, v)
	}
}

// loopEvents maps the agent loop's session events onto the headless schema.
// Only the events that carry run content are mapped; telemetry the loop writes
// for its own bookkeeping stays in the session log.
func loopEvents(em *session.Emitter) func(kind string, v any) {
	return func(kind string, v any) {
		switch kind {
		case "user":
			if s, ok := v.(string); ok {
				_ = em.Emit(session.Event{Type: session.EventUser, Text: s})
			}
		case "thinking":
			if s, ok := v.(string); ok {
				_ = em.Emit(session.Event{Type: session.EventThinking, Text: s})
			}
		case "assistant":
			if s, ok := v.(string); ok {
				_ = em.Emit(session.Event{Type: session.EventAssistant, Text: s})
			}
		}
	}
}

// runLog wraps the session logger so the tool layer's repair telemetry also
// reaches the headless stream. The session log is written first, exactly as
// before.
func runLog(log *session.Session, em *session.Emitter) func(kind string, v any) {
	return func(kind string, v any) {
		log.Log(kind, v)
		switch kind {
		case "tool_repair", "tool_repair_failed":
			if ev, ok := repairEvent(kind, v); ok {
				_ = em.Emit(ev)
			}
		}
	}
}

// repairEvent turns a tool repair log entry into a headless event. The
// payload carries the tool name and the repair classes — never the model's
// prose.
func repairEvent(kind string, v any) (session.Event, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return session.Event{}, false
	}
	name, _ := m["tool"].(string)
	ev := session.Event{Type: session.EventRepair, Name: name}
	switch kind {
	case "tool_repair":
		switch cs := m["classes"].(type) {
		case []string:
			ev.Classes = cs
		case []any:
			for _, c := range cs {
				if s, ok := c.(string); ok {
					ev.Classes = append(ev.Classes, s)
				}
			}
		}
	case "tool_repair_failed":
		ev.Failed = true
		ev.Error, _ = m["error"].(string)
	}
	return ev, true
}

// stampNow is the timestamp the headless header uses: RFC 3339 with
// nanoseconds, in UTC.
func stampNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func cmdPrompt(ws string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kintsugi prompt show|diff [flags]")
		os.Exit(2)
	}
	mode := args[0]
	fs := flag.NewFlagSet("prompt "+mode, flag.ExitOnError)
	model := fs.String("model", "", "provider/model")
	persona := fs.String("persona", "", "persona override")
	profile := fs.String("profile", "", "behavior profile")
	task := fs.String("task", "", "include a task layer")
	asText := fs.Bool("text", false, "print the compiled prompt")
	asJSON := fs.Bool("json", false, "print the manifest as JSON")
	fs.Parse(args[1:])

	app := newApp(ws)
	res, err := app.Resolve(*model)
	if err != nil {
		fail("%v", err)
	}
	comp, err := app.Compile(res, harness.CompileOpts{Persona: *persona, Profile: *profile, Task: *task})
	if err != nil {
		fail("%v", err)
	}

	var prev *prompt.Manifest
	if mode == "diff" {
		prev, _ = prompt.LoadManifest(app.Workspace)
	}

	if *asText {
		fmt.Print(comp.Text)
		fmt.Println()
		return
	}
	if *asJSON {
		b, _ := json.MarshalIndent(comp.Manifest, "", "  ")
		fmt.Println(string(b))
		return
	}

	if mode == "diff" {
		if prev == nil {
			fmt.Println("no previous manifest — this compile becomes the baseline")
		} else {
			for _, line := range prompt.Diff(*prev, comp.Manifest) {
				fmt.Println(line)
			}
			fmt.Println()
		}
	}

	printManifest(comp.Manifest, app.Workspace)
	if err := prompt.SaveManifest(app.Workspace, comp.Manifest); err != nil {
		fail("save manifest: %v", err)
	}
}

func printManifest(m prompt.Manifest, ws string) {
	fmt.Printf("model   %s/%s  family=%s  adapter=%s\n", m.Model.Provider, m.Model.ID, m.Model.Family, m.Adapter)
	fmt.Printf("persona %s  (rule: %s)\n", m.Persona.Name, m.Persona.Rule)
	fmt.Printf("stack   %s\n\n", m.StackHash)
	fmt.Printf("%-12s %-9s %-10s %6s  %s\n", "layer", "kind", "hash", "tokens", "source")
	for _, l := range m.Layers {
		src := l.Path
		if src == "" {
			src = "—"
		}
		mark := ""
		if l.Dropped {
			mark = "  [dropped]"
		}
		if len(l.DroppedSections) > 0 && !l.Dropped {
			mark = "  [sections: " + strings.Join(l.DroppedSections, ",") + "]"
		}
		fmt.Printf("%-12s %-9s %-10s %6d  %s%s\n", l.ID, l.Kind, shortHash(l.Hash), l.Tokens, src, mark)
	}
	fmt.Printf("\ntotal %d tokens (budget %d)", m.Totals.Tokens, m.Totals.Budget)
	if m.Totals.WindowUsedPct > 0 {
		fmt.Printf(" · %.1f%% of window", m.Totals.WindowUsedPct*100)
	}
	fmt.Println()
	for _, w := range m.Warnings {
		fmt.Println("warn: " + w)
	}
}

func cmdModels(ws string, args []string) {
	if len(args) > 0 && args[0] == "import" {
		cmdModelsImport(ws, args[1:])
		return
	}
	if len(args) > 0 && args[0] == "fetch" {
		cmdModelsFetch(ws, args[1:])
		return
	}
	if len(args) > 0 && args[0] == "report" {
		cmdModelsReport(args[1:])
		return
	}
	app := newApp(ws)
	cfg := app.Cfg
	names := cfg.ProviderNames()
	sort.Strings(names)
	for _, name := range names {
		p := cfg.Providers[name]
		key := "—"
		if p.APIKeyEnv != "" {
			// Where the key lives: an exported variable wins over the stored
			// copy (see `kintsugi connect`), so the live source is what matters.
			switch credentials.Status(name, p.APIKeyEnv) {
			case "env":
				key = p.APIKeyEnv + " ENV (set)"
			case "stored":
				key = p.APIKeyEnv + " stored"
			default:
				key = p.APIKeyEnv + " missing"
			}
		}
		fmt.Printf("%-12s %-42s %s\n", name, p.BaseURL, key)
		for _, m := range p.Models {
			marker := " "
			if name+"/"+m.ID == cfg.Core.DefaultModel {
				marker = "*"
			}
			fam := m.Family
			if fam == "" {
				fam = p.Family
			}
			fmt.Printf("  %s %-30s family=%s", marker, m.ID, orDash(fam))
			if m.Context > 0 {
				fmt.Printf(" ctx=%d", m.Context)
			}
			if m.Reasoning {
				fmt.Printf(" reasoning")
			}
			fmt.Println()
		}
		if len(p.Models) == 0 {
			fmt.Println("    (no model list — any model id is accepted)")
		}
	}
}

func cmdSkills(ws string, args []string) {
	app := newApp(ws)
	list := app.Skills()
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "":
		if len(list) == 0 {
			fmt.Printf("no skills under %s\n", app.SkillsDir())
			return
		}
		for _, sk := range list {
			fmt.Printf("%-20s %s\n  %s\n", sk.Name, sk.Description, sk.Rel)
		}
	case "show":
		if len(args) < 2 {
			fail("usage: kintsugi skills show <name>")
		}
		sk, ok := skills.Find(list, args[1])
		if !ok {
			fail("unknown skill %q (run `kintsugi skills list`)", args[1])
		}
		fmt.Printf("# Skill: %s\n\nSource: %s\n\n%s\n", sk.Name, sk.Rel, sk.Body)
	default:
		fail("usage: kintsugi skills [list|show <name>]")
	}
}

func cmdPack(ws string, args []string) {
	app := newApp(ws)
	if app.Pack == nil {
		fail("persona pack not found at %s", config.ExpandHome(app.Cfg.PersonaPack.Dir))
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		for _, p := range app.Personas() {
			rule := "fallback"
			switch {
			case len(p.IDGlob) > 0:
				rule = "id_glob:" + strings.Join(p.IDGlob, ",")
			case len(p.Family) > 0:
				rule = "family:" + strings.Join(p.Family, ",")
			case len(p.Caps) > 0:
				rule = "capability:" + strings.Join(p.Caps, ",")
			}
			trig := ""
			if p.Triggers > 0 {
				trig = fmt.Sprintf(" · %d known triggers", p.Triggers)
			}
			fmt.Printf("%-16s %-34s inherits=%s%s\n", p.Name, rule,
				orDash(strings.Join(p.Inherits, ",")), trig)
		}
	case "show":
		if len(args) < 2 {
			fail("usage: kintsugi pack show <persona>")
		}
		per, ok := app.Pack.Get(args[1])
		if !ok {
			fail("no persona %q", args[1])
		}
		fmt.Printf("# %s\n%s\n\nfamily=%v id_glob=%v inherits=%v\n\n%s\n",
			per.Name, per.Path, per.Match.Family, per.Match.IDGlob, per.Inherits,
			strings.TrimSpace(per.Body))
		for _, t := range per.Triggers.Known {
			fmt.Printf("trigger: %q → %s\n", t.Term, t.Instead)
		}
	default:
		fail("usage: kintsugi pack list|show <persona>")
	}
}

func cmdSession(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi session list|show <id>|rename <id> <title>|search <q>|fork <id>|export <id>|ab <id>")
	}
	switch args[0] {
	case "ab":
		cmdSessionAB(ws, args[1:])
	case "list":
		infos, err := session.ListInfo()
		if err != nil {
			fail("%v", err)
		}
		if len(infos) == 0 {
			fmt.Println("no sessions")
			return
		}
		fmt.Println(session.FormatInfoTable(infos))
	case "show":
		if len(args) < 2 {
			fail("usage: kintsugi session show <id>")
		}
		if err := session.Show(args[1]); err != nil {
			fail("%v", err)
		}
	case "rename":
		if len(args) < 3 {
			fail("usage: kintsugi session rename <id> \"<title>\"")
		}
		title := strings.TrimSpace(strings.Join(args[2:], " "))
		if err := session.Rename(args[1], title); err != nil {
			fail("%v", err)
		}
		fmt.Printf("renamed %s → %s\n", args[1], title)
	case "export":
		if len(args) < 2 {
			fail("usage: kintsugi session export <id> [--file <path>]")
		}
		out := os.Stdout
		if len(args) >= 4 && args[2] == "--file" {
			f, err := os.Create(args[3])
			if err != nil {
				fail("%v", err)
			}
			defer f.Close()
			out = f
		}
		if err := session.ExportJSONL(args[1], out); err != nil {
			fail("%v", err)
		}
		if out != os.Stdout {
			fmt.Printf("exported %s\n", args[3])
		}
	case "search":
		if len(args) < 2 {
			fail("usage: kintsugi session search <query>")
		}
		hits, err := session.Search(strings.Join(args[1:], " "), 50)
		if err != nil {
			fail("%v", err)
		}
		if len(hits) == 0 {
			fmt.Println("no matches")
			return
		}
		for _, h := range hits {
			when := ""
			if !h.TS.IsZero() {
				when = h.TS.Local().Format("2006-01-02 15:04")
			}
			fmt.Printf("%-24s %-10s %-16s %s\n", h.ID, h.Kind, when, h.Snippet)
		}
	case "fork":
		if len(args) < 2 {
			fail("usage: kintsugi session fork <id>")
		}
		fork, err := session.Fork(args[1])
		if err != nil {
			fail("%v", err)
		}
		id := fork.ID
		fork.Close()
		fmt.Printf("forked %s → %s\n", args[1], id)
	default:
		fail("usage: kintsugi session list|show <id>|rename <id> <title>|search <q>|fork <id>|export <id>|ab <id>")
	}
}

func fit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func cmdDoctor(ws string, args []string) {
	for _, a := range args {
		if a == "--retention" || a == "-retention" {
			cmdDoctorRetention(args)
			return
		}
	}
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	netProbe := fs.Bool("net", false, "probe the default provider endpoint")
	fs.Parse(args)

	app := newApp(ws)
	checks := doctor.Run(app, *netProbe)
	failed := false
	for _, c := range checks {
		state := c.EffectiveState()
		mark := "✓ " + string(state)
		switch state {
		case doctor.StateFail:
			mark = "✗ " + string(state)
			failed = true
		case doctor.StateWarn:
			mark = "! " + string(state)
		}
		fmt.Printf("%s %-16s %s\n", mark, c.Name, c.Detail)
	}
	if failed {
		fmt.Println("\nkintsugi doctor: issues found")
		os.Exit(1)
	}
	fmt.Println("\nkintsugi doctor: clean")
}

func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func cmdContext(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi context init|show|why|diff|apply|curate|refresh|attest")
	}
	sub, rest := args[0], args[1:]
	app := newApp(ws)
	path := app.ContextPath()

	switch sub {
	case "refresh":
		cmdContextRefresh(app.Workspace, rest)
	case "attest":
		cmdContextAttest(app, rest)
	case "init":
		fs := flag.NewFlagSet("context init", flag.ExitOnError)
		force := fs.Bool("force", false, "overwrite an existing file")
		name := fs.String("name", filepath.Base(app.Workspace), "project name")
		kind := fs.String("template", "", "authorization template: "+contextKindList()+" (interactive pick when omitted on a TTY)")
		fs.Parse(rest)
		if _, err := os.Stat(path); err == nil && !*force {
			fail("%s exists (use --force to overwrite)", path)
		}
		chosen := strings.TrimSpace(*kind)
		if chosen == "" {
			chosen = pickContextTemplate()
		}
		if chosen == "" {
			chosen = "general"
		}
		if !contextKindKnown(chosen) {
			fail("unknown template %q — pick one of: %s", chosen, contextKindList())
		}
		if err := os.WriteFile(path, []byte(fctx.DefaultTemplate(*name, chosen)), 0o644); err != nil {
			fail("%v", err)
		}
		fmt.Printf("wrote %s (template: %s)\n", path, chosen)
		fmt.Println("Edit the Authorization & scope section: replace the example with your own words before relying on it.")
		fmt.Println("It is operator-authored, gated against the curator, and injected on every turn.")

	case "show":
		doc, warns, err := app.LoadContext()
		if err != nil {
			fail("%v", err)
		}
		total := 0
		for _, s := range doc.Sections {
			tokens := (len(s.Body) + 3) / 4
			total += tokens
			gate := ""
			if fctx.HardGated(s.Class) {
				gate = "  [OPERATOR-GATED]"
			}
			fmt.Printf("%-16s hash=%-8s %5d tok%s\n", s.Class, doc.Hash(s.Class), tokens, gate)
		}
		fmt.Printf("\ndocument hash %s · %d tokens\n", doc.SectionsHash(), total)
		for _, w := range warns {
			fmt.Println("warn: " + w)
		}

	case "why":
		query := strings.Join(rest, " ")
		if strings.TrimSpace(query) == "" {
			fail("usage: kintsugi context why \"<question or phrase>\"")
		}
		doc, _, err := app.LoadContext()
		if err != nil {
			fail("%v", err)
		}
		hits := doc.Why(query)
		if len(hits) == 0 {
			fmt.Printf("no context entry matches %q\n", query)
			return
		}
		for _, h := range hits {
			fmt.Printf("%s · %s (line %d)\n  %s\n", h.Section, h.Class, h.LineNo, h.Line)
		}
		fmt.Println("\n(all listed entries are injected at layer 5 on every turn)")

	case "diff", "apply":
		fs := flag.NewFlagSet("context "+sub, flag.ExitOnError)
		file := fs.String("file", "", "proposal file (default: latest)")
		all := fs.Bool("all", false, "apply every non-gated proposal")
		gated := fs.Bool("gated", false, "operator approval: include authorization/scope proposals")
		fs.Parse(rest)

		doc, _, err := app.LoadContext()
		if err != nil {
			fail("%v", err)
		}
		pf := *file
		if pf == "" {
			pf, err = fctx.LatestProposal(app.Workspace)
			if err != nil {
				fail("%v", err)
			}
		}
		pfSet, err := fctx.LoadProposalFile(pf)
		if err != nil {
			fail("%v", err)
		}
		mode := fctx.ApplyMode{
			AutoClasses:  app.Cfg.Context.AutoAcceptClasses,
			IncludeAll:   *all,
			IncludeGated: *gated,
			SkillsDir:    app.SkillsDir(),
		}
		if sub == "diff" {
			fmt.Printf("proposal file %s (session %s, model %s)\n\n", pf, pfSet.Session, pfSet.Model)
			fmt.Print(fctx.Diff(doc, pfSet.Proposals, mode))
			return
		}
		res, err := fctx.Apply(doc, pfSet.Proposals, mode)
		if err != nil {
			fail("%v", err)
		}
		// Skill proposals write their own SKILL.md, so the document is only
		// rewritten when a section actually moved.
		if contextDocumentTouched(res.Applied) {
			if err := fctx.WriteDocument(path, doc); err != nil {
				fail("%v", err)
			}
		}
		for _, w := range res.Skills {
			verb := "wrote"
			if w.Replaced {
				verb = "replaced"
			}
			fmt.Printf("%s skill %s → %s\n", verb, w.Name, w.Path)
		}
		fmt.Printf("applied %d · pending %d · gated %d\n", len(res.Applied), len(res.Pending), len(res.Gated))
		if len(res.Pending) > 0 {
			fmt.Println("pending proposals need --all to apply")
		}
		if len(res.Gated) > 0 {
			fmt.Println("gated proposals were not applied: authorization/scope is operator-authored (edit the file, or pass --gated to approve explicitly)")
		}

	case "curate":
		fs := flag.NewFlagSet("context curate", flag.ExitOnError)
		model := fs.String("model", "", "provider/model (default: context.curator_model)")
		sessionID := fs.String("session", "", "session id (default: latest)")
		lines := fs.Int("lines", 400, "max transcript lines")
		fs.Parse(rest)
		sid := *sessionID
		if sid == "" {
			sid, _ = session.LatestID()
			if sid == "" {
				fail("no sessions found")
			}
		}
		transcript, err := session.TranscriptText(sid, *lines)
		if err != nil {
			fail("%v", err)
		}
		res, err := app.Curate(*model, sid, transcript)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("curator: %d proposals → %d auto-applied, %d pending, %d gated\n  %s\n",
			res.Proposals, len(res.Apply.Applied), len(res.Apply.Pending), len(res.Apply.Gated), res.ProposalPath)
		fmt.Println("review with: kintsugi context diff")

	default:
		fail("usage: kintsugi context init|show|why|diff|apply|curate|refresh|attest")
	}
}

// cmdContextAttest records the operator's review of the two gated context
// classes (M8.5.2). It is an operator action from end to end: nothing here calls
// a model, and nothing else in the harness writes an attestation. The recorded
// authorization hash binds the review to the current Authorization & scope text,
// so editing either section makes `kintsugi doctor` report the attestation stale.
func cmdContextAttest(app *harness.App, args []string) {
	fs := flag.NewFlagSet("context attest", flag.ExitOnError)
	reviewer := fs.String("reviewer", "", "who reviewed (default: $USER)")
	expires := fs.String("expires", "", "review-by date (YYYY-MM-DD)")
	fs.Parse(args)

	path := app.ContextPath()
	if _, err := os.Stat(path); err != nil {
		fail("no CONTEXT.md at %s (run `kintsugi context init`)", path)
	}
	doc, warns, err := app.LoadContext()
	if err != nil {
		fail("%v", err)
	}
	// Nothing to attest when the operator-gated section the hash binds is absent:
	// an attestation over an empty authorization text would be a promise about
	// nothing.
	if _, ok := doc.Find(fctx.ClassAuthorization); !ok {
		fail("%s has no %q section: add the operator-gated section, then attest", path, fctx.ClassAuthorization)
	}

	exp := strings.TrimSpace(*expires)
	if exp != "" {
		if _, err := time.Parse("2006-01-02", exp); err != nil {
			fail("--expires must be YYYY-MM-DD (got %q)", exp)
		}
	}
	who := strings.TrimSpace(*reviewer)
	if who == "" {
		who = os.Getenv("USER")
	}
	if who == "" {
		who = "operator"
	}

	att := fctx.Attestation{
		AttestedAt:        time.Now().UTC().Format(time.RFC3339),
		Reviewer:          who,
		AuthorizationHash: fctx.AuthorizationHash(doc),
		ExpiresAt:         exp,
	}
	if err := fctx.SaveAttestation(app.Workspace, att); err != nil {
		fail("%v", err)
	}

	fmt.Printf("attested %s\n", fctx.AttestationPath(app.Workspace))
	fmt.Printf("  reviewer %s · at %s\n", att.Reviewer, att.AttestedAt)
	fmt.Printf("  authorization hash %s\n", att.AuthorizationHash)
	fmt.Printf("  review by %s\n", orDash(exp))
	fmt.Println("· " + fctx.AttestationNotPermission + " Editing Authorization & scope makes this stale.")
	for _, w := range warns {
		fmt.Println("warn: " + w)
	}
}

func cmdMemory(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi memory add|search|reindex|list")
	}
	sub, rest := args[0], args[1:]
	app := newApp(ws)
	store, err := app.MemoryStore()
	if err != nil {
		fail("%v", err)
	}
	defer store.Close()

	switch sub {
	case "add":
		// Flags may appear before or after the lesson text.
		var models, source string
		var words []string
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			switch {
			case a == "--models" && i+1 < len(rest):
				i++
				models = rest[i]
			case strings.HasPrefix(a, "--models="):
				models = strings.TrimPrefix(a, "--models=")
			case a == "--source" && i+1 < len(rest):
				i++
				source = rest[i]
			case strings.HasPrefix(a, "--source="):
				source = strings.TrimPrefix(a, "--source=")
			default:
				words = append(words, a)
			}
		}
		text := strings.TrimSpace(strings.Join(words, " "))
		if text == "" {
			fail("usage: kintsugi memory add \"<lesson>\" [--models a,b] [--source ...]")
		}
		line, err := store.AddLesson(text, models, source)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("added %s\n", line)

	case "search":
		k := 8
		var words []string
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			switch {
			case a == "-k" || a == "--k":
				if i+1 < len(rest) {
					i++
					fmt.Sscanf(rest[i], "%d", &k)
				}
			case strings.HasPrefix(a, "--k="):
				fmt.Sscanf(strings.TrimPrefix(a, "--k="), "%d", &k)
			default:
				words = append(words, a)
			}
		}
		q := strings.TrimSpace(strings.Join(words, " "))
		if q == "" {
			fail("usage: kintsugi memory search \"<query>\"")
		}
		entries, err := store.Search(q, k)
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(memory.Format(entries))

	case "reindex":
		n, err := store.Reindex()
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("indexed %d entries → %s\n", n, memory.IndexPath(app.Workspace))

	case "list":
		b, err := os.ReadFile(memory.LearnedPath(app.Workspace))
		if err != nil {
			fmt.Println("(no learned lessons yet)")
			return
		}
		fmt.Print(strings.TrimSpace(string(b)) + "\n")

	default:
		fail("usage: kintsugi memory add|search|reindex|list")
	}
}

// promptApprover asks once per tool on the CLI. Non-interactive stdin denies:
// a piped task must be explicit with --yolo rather than silently approved.
// File-authoring approvals print the proposed changes first — a CLI approval
// is answered with the change in front of the operator, not a one-line summary.
func promptApprover() tools.ApproverFunc {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return func(tools.ApprovalRequest) bool { return false }
	}
	reader := bufio.NewReader(os.Stdin)
	return func(ar tools.ApprovalRequest) bool {
		detail := ar.Detail
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		fmt.Fprintf(os.Stderr, "\n  ⚠ %s: %s\n", ar.Tool, detail)
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
			fmt.Fprintln(os.Stderr, "  approved.")
			return true
		}
		fmt.Fprintln(os.Stderr, "  denied.")
		return false
	}
}

func cmdRepair(args []string) {
	if len(args) == 0 || args[0] != "stats" {
		fail("usage: kintsugi repair stats")
	}
	dir, err := session.Dir()
	if err != nil {
		fail("%v", err)
	}
	stats, err := session.RepairStats(dir)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(session.FormatRepairStats(stats))
}

// contextKindList names the authorization templates, comma-separated, for
// flags and errors.
func contextKindList() string {
	var kinds []string
	for _, k := range fctx.AuthorizationKinds {
		kinds = append(kinds, string(k.Kind))
	}
	return strings.Join(kinds, "|")
}

// contextKindKnown reports whether kind names a template.
func contextKindKnown(kind string) bool {
	for _, k := range fctx.AuthorizationKinds {
		if string(k.Kind) == strings.TrimSpace(kind) {
			return true
		}
	}
	return false
}

// pickContextTemplate offers the kinds on a TTY and returns the choice; an
// empty return (non-TTY, EOF, or an unparsable answer) means "general".
// The picker writes to stderr so stdout stays clean for piping.
func pickContextTemplate() string {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return ""
	}
	fmt.Fprintln(os.Stderr, "authorization template — what kind of work is this workspace for?")
	for i, k := range fctx.AuthorizationKinds {
		fmt.Fprintf(os.Stderr, "  %d. %-10s %s\n", i+1, k.Kind, k.Description)
	}
	fmt.Fprint(os.Stderr, "pick [1]: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "general"
	}
	n := 0
	if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= len(fctx.AuthorizationKinds) {
		return string(fctx.AuthorizationKinds[n-1].Kind)
	}
	// A typed kind name is accepted too: the numbers are a convenience, not
	// a constraint.
	if contextKindKnown(line) {
		return line
	}
	return ""
}
