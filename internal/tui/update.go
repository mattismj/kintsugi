package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/advisor"
	"kintsugi/internal/audit"
	"kintsugi/internal/commands"
	"kintsugi/internal/config"
	"kintsugi/internal/harness"
	"kintsugi/internal/kb"
	"kintsugi/internal/memory"
	"kintsugi/internal/notify"
	"kintsugi/internal/plan"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/taste"
	"kintsugi/internal/tools"
	"kintsugi/internal/ui"
)

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		// A resize changes how many lines fit, so re-clamp anything scrolled. A
		// width change re-wraps the transcript and renumbers every line, which
		// makes a stored anchor meaningless, so it goes with the re-clamp.
		m.scroll = clampInt(m.scroll, 0, m.maxScroll())
		m.anchor = scrollAnchor{}
		if m.wb != nil {
			m.wb.scroll = clampInt(m.wb.scroll, 0, m.wbMaxScroll())
		}
		if m.connect != nil {
			m.connect.scroll = clampInt(m.connect.scroll, 0, m.connect.maxScrollFor(m.connectRows()))
		}
		if m.picker != nil {
			m.picker.clampSel(len(m.picker.filtered()))
		}
		if m.refusal != nil {
			m.refusal.scroll = clampInt(m.refusal.scroll, 0, 1<<20)
		}
		return m, nil

	// Mouse is handled before the keyboard: wheel presses scroll, and every
	// other mouse event (clicks, drags, horizontal wheels) is ignored so a
	// stray click cannot move the transcript or fire a control. A modal panel
	// owns the wheel while it is open.
	case tea.MouseMsg:
		if m.connect != nil {
			m.handleConnectMouse(msg)
			return m, nil
		}
		if m.picker != nil {
			m.handleModelPickerMouse(msg)
			return m, nil
		}
		if m.refusal != nil {
			m.handleRefusalMouse(msg)
			return m, nil
		}
		if m.changes != nil {
			m.handleChangesMouse(msg)
			return m, nil
		}
		m.handleMouse(msg)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case connectResult:
		return m.handleConnectResult(msg)

	case modelsFetched:
		return m.handleModelsFetched(msg)

	case reviewResult:
		return m.handleReviewResult(msg)

	case thinkingDelta:
		if m.thinkIdx < 0 || m.thinkIdx >= len(m.blocks) || m.blocks[m.thinkIdx].kind != blockThinking {
			m.addBlock(block{kind: blockThinking})
			m.thinkIdx = len(m.blocks) - 1
		}
		m.blocks[m.thinkIdx].text += msg.s
		return m, listen(m.ch)

	case streamDelta:
		// The answer has started: the thinking block is closed and the
		// assistant block opens.
		m.thinkIdx = -1
		if m.stream < 0 || m.stream >= len(m.blocks) || m.blocks[m.stream].kind != blockAssistant {
			m.addBlock(block{kind: blockAssistant})
			m.stream = len(m.blocks) - 1
		}
		m.blocks[m.stream].text += msg.s
		// No per-token work here: the state is derived, never stored.
		// Ticks only happen on state changes (tool start/end, turn done).
		return m, listen(m.ch)

	case toolStreamNotice:
		m.appendToolStream(msg.ev)
		return m, listen(m.ch)

	case toolNotice:
		m.stream = -1
		m.thinkIdx = -1
		// The live block's replacement has arrived: the final card is the record.
		m.clearLiveTool()
		if strings.Contains(msg.res.Content, "harness note: repaired") {
			m.repairs++
		}
		m.addBlock(m.toolLine(msg.call, msg.res))
		// The session change set: /changes answers "what did the agent do"
		// from this fold, latest state per path.
		m.foldChanges(msg.res.Diff)
		return m, listen(m.ch)

	case usageNotice:
		if m.res != nil {
			m.app.RecordUsage(m.res.ProviderName, msg.prompt, msg.completion)
			before := m.quotaUsed
			m.quotaUsed += int64(msg.prompt + msg.completion)
			// A quota that crosses 80% is worth raising once, at the crossing —
			// not on every later turn, and not when it was already over.
			if m.quotaLimit > 0 && before < m.quotaLimit*80/100 && m.quotaUsed >= m.quotaLimit*80/100 {
				m.raise(notify.KindQuota, fmt.Sprintf("quota %d%% used on %s",
					m.quotaUsed*100/m.quotaLimit, m.quotaProvider))
			}
		}
		return m, listen(m.ch)

	case cacheNotice:
		m.cacheRead += int64(msg.read)
		m.cacheWritten += int64(msg.written)
		return m, listen(m.ch)

	case advisorNotice:
		m.stream = -1
		m.advisorNotes += len(msg.notes)
		for _, n := range msg.notes {
			tone := ui.PaperDim
			switch n.Kind {
			case advisor.KindBlocker:
				tone = ui.Danger
			case advisor.KindConcern:
				tone = ui.Warn
			}
			m.addCard("// advisor · "+string(n.Kind), n.Text, tone)
		}
		return m, listen(m.ch)

	case todoNotice:
		m.todos = msg.items
		return m, listen(m.ch)

	case editorDone:
		m.afterEditor(msg)
		return m, nil

	case interceptNotice:
		m.refusals++
		m.stream = -1
		if msg.outcome == "hard" {
			// A stonewall: retrying was stopped and the card recommends what
			// the telemetry says has worked on this work.
			body := fmt.Sprintf("class %s · %q\n%s\nretrying stopped — a reminder does not move this model.\n%s",
				msg.hit.Class, truncate(oneLine(msg.hit.Phrase), 64),
				m.refusalEvidence(), m.modelRecommendation())
			m.addCard("// refusal · hard ✸", body, ui.Danger)
			return m, listen(m.ch)
		}
		phrase := truncate(oneLine(msg.hit.Phrase), 64)
		// The refusal evidence card (A3): name the context that would have made
		// the work legible, and whether it was missing or truncated, so the card
		// is an action ("fix your CONTEXT.md") rather than a plain "we caught a
		// refusal".
		evidence := m.refusalEvidence()
		body := fmt.Sprintf("class %s · %q\n%s\nreminder %d · %s", msg.hit.Class, phrase, evidence, msg.level, msg.outcome)
		if msg.outcome == "handoff" {
			body = fmt.Sprintf("class %s · %q\n%s\nhanded back to operator · /frame re-injects Authorization & Domain", msg.hit.Class, phrase, evidence)
		}
		tone := ui.Warn
		if msg.outcome == "handoff" {
			tone = ui.Danger
		}
		m.addCard("// refusal · intercepted ✸", body, tone)
		return m, listen(m.ch)

	case driftNotice:
		// Metadata only. The ids are the prior accepted work the guard
		// re-injected; the class and outcome are telemetry labels. The drift row
		// itself is not recallable, so there is nothing here to remember.
		m.stream = -1
		ids := "none"
		if len(msg.matched) > 0 {
			ids = strings.Join(msg.matched, ", ")
		}
		m.addCard("// continuity · drift metadata",
			fmt.Sprintf("matched %s\nclass %s · outcome %s\nrecorded as metadata; never recalled",
				ids, orDash(msg.class), orDash(msg.outcome)), ui.Warn)
		return m, listen(m.ch)

	case approvalReq:
		m.approval = &msg
		m.raise(notify.KindApproval, "approval needed: "+msg.tool)
		// Keep draining: after the operator answers, the agent continues on the
		// same channel with tool results and the final turn.
		return m, listen(m.ch)

	case turnDone:
		m.busy = false
		m.stream = -1
		m.thinkIdx = -1
		m.clearLiveTool()
		m.hist = msg.hist
		if len(m.todos) == 0 {
			m.markTask("done")
		}
		m.addNote("· turn complete · " + m.log.ID)
		m.raise(notify.KindDone, "turn complete · "+m.log.ID)
		// The background review fork (A4) reviews a snapshot after the turn and
		// writes lessons/skills through the curator gate. It never touches the
		// live transcript or the cached prefix.
		harness.MaybeBackgroundCurate(m.app, m.log.ID)
		return m, nil

	case errMsg:
		m.busy = false
		m.stream = -1
		m.thinkIdx = -1
		m.clearLiveTool()
		m.markTask("error")
		m.addNote("✸ " + msg.err.Error())
		return m, nil
	}
	return m, nil
}

// handleMouse implements wheel scrolling (M8.6.4) and cursor gaze (M13).
//
// Wheel up reveals earlier content in whichever surface owns the screen. Clicks,
// releases and drag are ignored: mouse capture must never steal a click or
// interfere with the operator selecting text (shift+drag still bypasses capture).
//
// Motion is recorded and nothing else. Bubble Tea renders after every message, so
// a handler that did work here would run it hundreds of times a second; the
// transcript memo keeps each of those renders cheap and the renderer's own
// framerate caps what reaches the terminal. The eyes read the position when they
// are drawn, so there is nothing to schedule.
func (m *Model) handleMouse(msg tea.MouseMsg) {
	if msg.Action != tea.MouseActionPress {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.wb != nil {
			m.wbScrollBy(-3)
			return
		}
		m.scrollBy(3)
	case tea.MouseButtonWheelDown:
		if m.wb != nil {
			m.wbScrollBy(3)
			return
		}
		m.scrollBy(-3)
	}
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The /connect panel owns the keyboard while it is open: not one key may
	// reach the input line, because everything typed there is a secret.
	if m.connect != nil {
		return m.handleConnectKey(msg)
	}

	// The model picker owns the keyboard while it is open: typing filters,
	// enter switches, esc closes.
	if m.picker != nil {
		return m.handleModelPickerKey(msg)
	}

	// The refusal workbench owns the keyboard while it is open.
	if m.refusal != nil {
		return m.handleRefusalKey(msg)
	}

	// The changes view owns the keyboard while it is open.
	if m.changes != nil {
		return m.handleChangesKey(msg)
	}

	// Approval modal owns the keyboard.
	if m.approval != nil {
		switch strings.ToLower(msg.String()) {
		case "y":
			m.resolveApproval(true)
		case "n", "esc":
			m.resolveApproval(false)
		case "ctrl+c":
			m.resolveApproval(false)
			return m, tea.Quit
		}
		return m, nil
	}
	if m.wb != nil {
		return m.handleWorkbenchKey(msg)
	}

	// The completion popup is not a modal: it owns exactly ↑ ↓ tab enter esc and
	// reports false for everything else, so a keystroke still reaches the input
	// line and the popup is rebuilt from the buffer afterwards. It sits after
	// every modal guard, so a surface that owns the keyboard keeps it.
	if m.suggest != nil && m.suggestKeys(msg) {
		return m, nil
	}

	// A focused roster owns its keys until the operator does something else.
	if m.rosterFocus && m.rosterFocusedKeys(msg) {
		return m, nil
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		if m.busy {
			if m.cancel != nil {
				m.cancel()
			}
			m.addNote("· aborted by operator")
			return m, nil
		}
		return m, tea.Quit
	case tea.KeyCtrlD:
		return m, tea.Quit
	case tea.KeyCtrlR:
		// Focus the background roster. The keys it owns are named in its own
		// footer line, so there is nothing to memorise.
		if !m.rosterVisible() {
			m.addNote("· no background runs to show")
			return m, nil
		}
		m.rosterFocus = !m.rosterFocus
		if m.rosterFocus {
			m.addNote("· roster focused: ↑↓ select, o open, k kill, esc leave")
		}
		return m, nil
	case tea.KeyCtrlT:
		m.toggleThinking()
	case tea.KeyCtrlU:
		m.setInput("")
	case tea.KeyCtrlP:
		m.togglePanel("inspector")
	case tea.KeyCtrlO:
		m.togglePanel("rail")
	case tea.KeyCtrlY:
		// Ctrl+Y copies the last assistant block; alt+y the last tool result.
		// Not ctrl+shift+y: bubbletea v1.3.10 only distinguishes ctrl+shift on
		// the arrow keys, so that binding would silently do nothing.
		//
		// OSC 52 is the only copy path that survives SSH and tmux, which is where
		// this harness actually runs — and while gaze tracking has the mouse
		// captured, the terminal's own drag-select is unavailable.
		if msg.Alt {
			m.copyLast(blockTool)
		} else {
			m.copyLast(blockAssistant)
		}
	case tea.KeyCtrlRight:
		if m.rightBias < 14 {
			m.rightBias += 2
		}
	case tea.KeyCtrlLeft:
		if m.rightBias > -8 {
			m.rightBias -= 2
		}
	case tea.KeyShiftRight:
		if m.leftBias < 14 {
			m.leftBias += 2
		}
	case tea.KeyShiftLeft:
		if m.leftBias > -4 {
			m.leftBias -= 2
		}
	case tea.KeyPgUp:
		m.scrollBy(5)
	case tea.KeyPgDown:
		m.scrollBy(-5)
	case tea.KeyEsc:
		m.scroll = 0
		m.clearAnchor()
	case tea.KeyUp:
		// Inside a multi-line draft the arrows move within the buffer; walking
		// history only starts at the edges, so a newline cannot silently
		// overwrite what the operator is writing.
		if m.caretRow() > 0 {
			m.caretVertical(-1)
			break
		}
		if m.histPos > 0 {
			m.histPos--
			m.setInput(m.history[m.histPos])
		}
	case tea.KeyDown:
		if m.caretRow() < strings.Count(m.input, "\n") {
			m.caretVertical(1)
			break
		}
		if m.histPos < len(m.history)-1 {
			m.histPos++
			m.setInput(m.history[m.histPos])
		} else {
			m.histPos = len(m.history)
			m.setInput("")
		}
	case tea.KeyLeft:
		m.moveCaret(-1)
	case tea.KeyRight:
		m.moveCaret(1)
	case tea.KeyCtrlA:
		m.inputCursor = m.lineStart()
	case tea.KeyCtrlE:
		m.inputCursor = m.lineEnd()
	case tea.KeyBackspace:
		m.backspace()
	case tea.KeyCtrlJ:
		// ctrl+j is the clean newline: it is a distinct key type, so it cannot
		// collide with enter. alt+enter is the discoverable spelling and lands
		// on the same code path.
		m.insertNewline()
	case tea.KeyEnter:
		// alt+enter opens a line instead of sending it.
		if msg.Alt {
			m.insertNewline()
			m.refreshSuggest()
			return m, nil
		}
		line := strings.TrimSpace(m.input)
		m.setInput("")
		m.refreshSuggest() // the draft is gone, so the popup goes with it
		if line == "" || m.busy {
			return m, nil
		}
		if strings.HasPrefix(line, "/") {
			return m.handleSlash(line)
		}
		if strings.HasPrefix(line, "@") {
			// A context reference (@file, @folder, @diff) is context, not an
			// agent mention. Anything else @-prefixed still names an agent.
			if commands.IsReferenceToken(line) {
				return m, m.submitExpanded(m.expandLine(line))
			}
			return m.handleMention(line)
		}
		// A plain task may still carry context references.
		return m, m.submitExpanded(m.expandLine(line))
	case tea.KeyTab:
		// Inside the open inspector, tab switches its tab (work/harness);
		// anywhere else it is the completion key.
		if m.panel == "inspector" {
			if m.inspectTab == "harness" {
				m.inspectTab = "work"
			} else {
				m.inspectTab = "harness"
			}
			return m, nil
		}
		// Tab is the completion key: accept the selected candidate, else open
		// the popup for the token under the caret, else indent two spaces in a
		// multi-line draft. On a single-line draft with nothing to complete it
		// does nothing, so tab can never surprise the operator with whitespace
		// they cannot see.
		switch {
		case m.suggest != nil:
			m.acceptSuggest()
		case m.openSuggest():
			// Just opened: the popup now owns the arrows.
		case strings.Contains(m.input, "\n"):
			m.insertRunes([]rune("  "))
		}
	case tea.KeyRunes:
		// alt+t copies the newest thinking block; a plain paste or a rune
		// batch falls through to the input line.
		if msg.Alt && string(msg.Runes) == "t" {
			m.copyThinking()
			return m, nil
		}
		// msg.Runes, never msg.String(): bubbletea wraps a bracketed paste in
		// "[" … "]" in String() so pasted text cannot trigger a key binding, and
		// copying a finding into the input would otherwise insert the brackets
		// literally. A paste arrives as one KeyMsg, so this is also how a
		// multi-line draft gets its text.
		//
		// A mouse-motion report that the reader split across a read boundary
		// arrives here as plain runes — its leading ESC was consumed as the Alt
		// flag or by the report it belonged to, so bubbletea can no longer see it
		// as a mouse event. Typing that into the draft is never what the operator
		// did, so it is dropped.
		_ = msg.Paste
		m.insertRunes(msg.Runes)
	case tea.KeySpace:
		m.insertRunes([]rune{' '})
	}
	// Every key that reaches here may have moved the buffer or the caret, so the
	// popup is rebuilt from what the draft now says — after the key, never
	// before. A key that changed neither is a no-op inside refreshSuggest.
	m.refreshSuggest()
	return m, nil
}

func (m *Model) resolveApproval(ok bool) {
	req := m.approval
	m.approval = nil
	if req == nil {
		return
	}
	if ok {
		m.addNote("✓ approved " + req.tool)
	} else {
		m.addNote("× denied " + req.tool)
	}
	// Operator taste (M10-B): every grant/deny is a local signal. Best-effort:
	// a taste write failure never blocks the approval itself.
	if m.app != nil {
		_, _ = taste.RecordApproval(m.app.Workspace, req.tool, req.detail, ok)
	}
	select {
	case req.reply <- ok:
	default:
	}
}

func (m *Model) handleSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	cmd := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(line, cmd))

	switch cmd {
	case "/quit", "/q":
		return m, tea.Quit

	case "/help":
		if rest = strings.TrimSpace(rest); rest != "" {
			body, ok := m.helpGroupBody(rest)
			tone := ui.Gold
			if !ok {
				tone = ui.Warn
			}
			m.addCard("// help · "+strings.TrimPrefix(rest, "/"), body, tone)
			break
		}
		m.addCard("// help", m.helpMenu(), ui.Gold)

	case "/clear":
		m.blocks = nil
		m.scroll = 0
		m.clearAnchor()
		m.addNote("· transcript cleared")

	case "/status":
		m.addCard("// status", m.statusText(), ui.Gold)

	case "/prompt":
		m.addCard("// 03 inspector", m.inspectorText(), ui.Gold)

	case "/panel":
		// /panel expands one pillar into a transcript card. The inspector shows
		// the four as a summary; this is where an operator reads one in full —
		// the whole prompt stack, the declared scope, the refusal telemetry, or
		// the privacy posture.
		title, body, ok := m.pillarDetail(rest, m.w-4)
		if !ok {
			m.addNote("· usage: /panel context|scope|refusal|privacy")
			break
		}
		m.addCard(title, body, ui.Gold)

	case "/context":
		return m, m.handleContextCommand(rest)

	case "/connect":
		// One provider name only: a key is never accepted on the command line,
		// so a second field is refused rather than parsed as a secret.
		args := strings.Fields(rest)
		if len(args) > 1 {
			m.addNote("· /connect takes one provider name; a key is never read from the command line")
			break
		}
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		m.connect = openConnect(m.app, name)

	case "/persona":
		return m, m.handlePersonaCommand(rest)

	case "/agents":
		persona := m.comp.Manifest.Persona.Name
		if m.comp.Manifest.Persona.Rule != "" {
			persona += " (" + m.comp.Manifest.Persona.Rule + ")"
		}
		text, path := m.app.AgentsMD()
		lines := []string{"persona  " + persona}
		if path == "" {
			lines = append(lines, "agents   none",
				"", "add AGENTS.md at the workspace root (or ~/.config/kintsugi/AGENTS.md)",
				"it is injected unconditionally as operator instructions")
		} else {
			lines = append(lines, "agents   "+path, "")
			body := strings.TrimSpace(text)
			if len(body) > 1600 {
				body = body[:1600] + "…"
			}
			lines = append(lines, body)
		}
		lines = append(lines, m.agentsCard()...)
		// The three AGENTS.md tiers (I-80), so an operator sees which document
		// carries what without opening the surface first.
		tiers := m.app.AgentsTiers()
		if len(tiers) > 0 {
			lines = append(lines, "", "tiers (most specific last)")
			for _, t := range tiers {
				rule := ""
				if t.Rule != "" {
					rule = " · " + t.Rule
				}
				lines = append(lines, "  "+t.Tier+rule)
			}
		}
		lines = append(lines, "", "/modelctx  view, edit and force a per-model AGENTS.md")
		m.addCard("// agents", strings.Join(lines, "\n"), ui.Gold)

	case "/mode":
		m.handleMode(rest)

	case "/scope":
		// The boundary the tool layer enforces, with the files that set it. The
		// panel above is the same data; this makes it reachable by name so an
		// operator who sees "▲ scope undeclared" knows exactly what to type.
		title, body, _ := m.pillarDetail(pillarScope, max(m.transcriptWidth()-4, 24))
		m.addCard(title, body, ui.Gold)

	case "/modelctx", "/agents-md":
		// The per-model AGENTS.md surface (I-80): which document this model gets,
		// and the editor for it.
		m.openModelCtxWorkbench()

	case "/plan":
		// `/plan review` runs a round on the plan already open (or the newest
		// one). It lives under /plan rather than as a bare /review because a
		// built-in name shadows a custom command of the same name, and /review
		// is a common workspace command.
		if strings.TrimSpace(rest) == "review" {
			if p := m.planReview(); p == nil {
				m.handlePlan("")
			}
			if p := m.planReview(); p != nil {
				return m, m.runPlanReview()
			}
			return m, nil
		}
		m.handlePlan(rest)

	case "/skills":
		list := m.app.Skills()
		if len(list) == 0 {
			m.addNote("· no skills under " + m.app.SkillsDir())
			break
		}
		var lines []string
		for _, sk := range list {
			lines = append(lines, fmt.Sprintf("%-18s %s", truncate(sk.Name, 18), truncate(oneLine(sk.Description), 56)))
		}
		lines = append(lines, "", "bodies load on demand through the skill tool")
		m.addCard("// skills", strings.Join(lines, "\n"), ui.Gold)

	case "/commands":
		m.handleCommands()

	case "/kb":
		m.handleKB(rest)

	case "/memory":
		m.handleMemory(rest)

	case "/frame":
		frame := m.app.FrameText()
		if frame == "" {
			m.addNote("✗ no Authorization/Domain sections in CONTEXT.md · add both, then /frame again")
			break
		}
		m.hist = append(m.hist, provider.Message{Role: "system", Content: frame})
		secs := m.app.ContextSections("authorization", "domain")
		m.addCard("// frame · injected", fmt.Sprintf(
			"authorization %d tok · domain %d tok\none-shot frame queued for the next turn",
			(len(strings.TrimSpace(secs["authorization"]))+3)/4,
			(len(strings.TrimSpace(secs["domain"]))+3)/4), ui.Warn)

	case "/model":
		if m.busy {
			m.addNote("· wait for the current turn to finish")
			break
		}
		// No argument opens the picker: a typed provider/model string is
		// muscle memory for nobody on their first day. The typed form still
		// works for the operator who knows the ref.
		if rest == "" {
			return m, m.openModelPicker()
		}
		if err := m.applyModelSwitch(rest); err != nil {
			m.addNote("✗ " + err.Error())
		}

	case "/role":
		if m.busy {
			m.addNote("· wait for the current turn to finish")
			break
		}
		if rest == "" {
			m.addNote("usage: /role default|coder|researcher|opsec")
			break
		}
		res, err := m.app.ResolveRole(rest)
		if err != nil {
			m.addNote("✗ " + err.Error())
			break
		}
		comp, err := m.app.CompileRole(res, rest, harness.CompileOpts{Persona: m.personaOverride})
		if err != nil {
			m.addNote("✗ " + err.Error())
			break
		}
		for _, line := range prompt.Diff(m.comp.Manifest, comp.Manifest) {
			m.addNote("· " + line)
		}
		m.res, m.comp, m.role = res, comp, strings.ToLower(rest)
		m.loadQuota()
		m.loop.Client = res.Client
		m.loop.Reminder = m.app.ReminderBuilder(comp)
		m.log.Log("meta", map[string]string{
			"event": "role_switch", "role": m.role,
			"model":   res.ProviderName + "/" + res.Model.ID,
			"persona": comp.Manifest.Persona.Name, "stack": comp.Manifest.StackHash,
		})
		m.addCard("// role switched", fmt.Sprintf("%s · %s · persona %s", m.role,
			res.ProviderName+"/"+res.Model.ID, comp.Manifest.Persona.Name), ui.Gold)

	case "/remember":
		if rest == "" {
			m.addNote("usage: /remember <lesson>")
			break
		}
		if m.mem == nil {
			m.addNote("✗ memory store unavailable · check .kintsugi/ is writable, then /remember again")
			break
		}
		if _, err := m.mem.AddLesson(rest, m.res.Model.Family, "session "+m.log.ID); err != nil {
			m.addNote("✗ " + err.Error())
			break
		}
		m.addCard("// lesson recorded", truncate(rest, 120), ui.OK)

	case "/rewind":
		m.handleRewind(rest)

	case "/refusal":
		m.openRefusalWorkbench()

	case "/changes":
		m.openChanges()

	case "/recall":
		m.handleRecall(rest)

	case "/ledger":
		m.handleLedger(rest)

	case "/accept":
		m.handleAccept(rest)

	case "/guard":
		if m.app == nil {
			m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
			break
		}
		m.handleGuard(rest)

	default:
		// A custom command (M9.8): a Markdown file in the workspace tier or
		// in the user tier. The built-ins above win; a name that matches
		// neither is still an unknown command.
		exp, ok := m.expandCommand(line)
		if !ok {
			m.addNote("· unknown command " + cmd + " · try /help")
			break
		}
		return m, m.submitExpanded(exp)
	}
	return m, nil
}

// handleCommands implements /commands: the custom commands this session has,
// with their description and argument hint. A workspace command shadows a
// user command of the same name, so the list is what /name will run.
func (m *Model) handleCommands() {
	list := m.loadCommands()
	if len(list) == 0 {
		m.addNote("· no custom commands — add .kintsugi/commands/<name>.md to the workspace")
		return
	}
	lines := make([]string, 0, len(list)+2)
	for _, c := range list {
		lines = append(lines, fmt.Sprintf("%-20s %-20s %s",
			"/"+c.Name, truncate(c.ArgumentHint, 20), truncate(oneLine(c.Description), 60)))
	}
	lines = append(lines, "", "/name <args> expands a command into a task")
	m.addCard("// commands", strings.Join(lines, "\n"), ui.Gold)
}

// modeList is the set of modes a session can be in, in the order they widen.
// The aliases tools.ParseMode accepts ("bypass", "auto-accept", ...) work
// too, but the list is the canonical set.
const modeList = "default  accept-edits  plan  yolo  dont-ask"

// handleMode implements /mode (M9): it reports the session's permission mode
// and switches it.
//
// The mode is the baseline the permission matrix starts from: it decides
// whether a mutating call runs, is asked about, or is refused. Switching it
// changes what the *next* tool calls are judged by — a call already in
// flight is not re-judged, so the switch waits for the turn to end.
//
// yolo is reachable only by typing it. No keypress switches a session into
// a posture that approves everything.
func (m *Model) handleMode(rest string) {
	if m.busy {
		// The executor's policy is read by the running turn, so the switch
		// waits for the turn to end: the next turn runs in the new mode.
		m.addNote("· wait for the current turn to finish — the mode switches between turns")
		return
	}
	arg := strings.TrimSpace(rest)
	if arg == "" {
		m.addCard("// permission mode", m.modeText(), ui.Gold)
		return
	}
	mode, err := tools.ParseMode(arg)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	from := m.currentMode()
	m.setMode(mode)
	m.addNote(fmt.Sprintf("· permission mode %s → %s", from, mode))
	m.log.Log("meta", map[string]string{
		"event": "mode_switch", "from": string(from), "mode": string(mode),
	})
	m.recordModeSwitch(from, mode)
}

// modeText is the /mode card: the mode the session is in and the modes it
// can be switched to.
func (m *Model) modeText() string {
	return fmt.Sprintf("current   %s\n\n%s\n\nthe mode decides what the next tool calls are judged by",
		m.modeLabel(), modeList)
}

// recordModeSwitch writes the mode change to the workspace audit log
// (M11.3). A posture change is part of the security record: the operator
// must be able to see afterwards which posture was in force when a call was
// allowed or refused.
func (m *Model) recordModeSwitch(from, to tools.Mode) {
	if m.app == nil || strings.TrimSpace(m.app.Workspace) == "" {
		return
	}
	_ = audit.Record(m.app.Workspace, audit.Event{
		SessionID: m.sessionID(),
		Tool:      "mode",
		Detail:    string(to),
		Decision:  audit.DecisionSwitch,
		Reason:    string(from) + " → " + string(to),
	})
}

// handlePlan implements /plan (M9.2). A named plan opens in the review
// workbench; with no argument the newest plan opens there, because reviewing is
// what the surface is for. `/plan list` keeps the listing.
func (m *Model) handlePlan(rest string) {
	if m.app == nil {
		m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
		return
	}
	name := strings.TrimSpace(rest)
	if name != "" && name != "list" {
		m.openPlanWorkbench(name)
		return
	}
	list, err := plan.List(m.app.Workspace)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	if len(list) == 0 {
		m.addNote("· no plans yet — write one with `kintsugi plan \"<task>\"`")
		return
	}
	if name != "list" {
		m.openPlanWorkbench(list[0].Name)
		return
	}
	lines := make([]string, 0, len(list)+3)
	for _, a := range list {
		title := plan.Title(a.Content)
		if title == "" {
			title = plan.Summary(a.Content)
		}
		lines = append(lines, fmt.Sprintf("%-24s %s  %s",
			a.Name, a.Created.Local().Format("2006-01-02 15:04"), truncate(oneLine(title), 44)))
	}
	lines = append(lines, "", "newest  "+list[0].Path, "",
		"/plan <name> opens a plan for review",
		"/plan with no argument opens the newest one")
	m.addCard("// plans", strings.Join(lines, "\n"), ui.Gold)
}

// agentsCard renders the Markdown-defined agents (M9.3): the ones an
// @mention can reach, with the model and the tools each one may use.
func (m *Model) agentsCard() []string {
	list := m.loadAgents()
	if len(list) == 0 {
		return []string{"", "markdown agents  none — add .kintsugi/agents/<name>.md to the workspace"}
	}
	lines := []string{"", "markdown agents"}
	for _, a := range list {
		model := a.Model
		if model == "" {
			model = "session"
		}
		lines = append(lines, fmt.Sprintf("%-16s %-22s %-14s %s",
			a.Name, truncate(oneLine(a.Description), 22), truncate(model, 14),
			strings.Join(a.ToolList(), ",")))
	}
	lines = append(lines, "", "@<name> <task> hands a task to an agent")
	return lines
}

// handleMention implements the @mention dispatch (M9.3). The line is only
// consumed when the agent exists and the operator gave it a task: an unknown
// name is a note, never a turn.
func (m *Model) handleMention(line string) (tea.Model, tea.Cmd) {
	am, err := m.resolveMention(line)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return m, nil
	}
	// The context references in the agent's task expand too: the agent gets
	// the context the operator asked for, not the token.
	exp := m.expandLine(am.Task)
	am.Task = exp.Prompt
	m.scroll = 0
	m.clearAnchor()
	m.history = append(m.history, line)
	m.histPos = len(m.history)
	m.tasks = append(m.tasks, taskItem{label: "@" + am.Agent.Name + " " + exp.Original, state: "running"})
	m.addBlock(block{kind: blockUser, text: line})
	m.addExpansionNote(exp.Summary())
	m.busy = true
	m.turn++
	m.ctx, m.cancel = context.WithCancel(context.Background())
	cmd := m.startAgentTurn(am)
	if cmd == nil {
		// The turn could not start: the session must not stay busy.
		m.busy = false
		m.markTask("error")
		return m, nil
	}
	return m, cmd
}

// startAgentTurn starts one resolved mention: the seam when one is set, the
// built-in runner otherwise.
func (m *Model) startAgentTurn(am AgentMention) tea.Cmd {
	if m.StartAgent != nil {
		return m.StartAgent(am)
	}
	return m.runAgentTurn(am)
}

// kbDefaultTopK is the result count used when a base configures none. It
// mirrors the compiler's default, so /kb and the prompt see a base at the
// same depth.
const kbDefaultTopK = 8

// handleKB implements /kb. With no argument it lists the configured
// knowledge bases — path, strategy and top_k. With a query it runs the local
// BM25 search over them: the index is derived from the Markdown on disk, so
// this costs no network and no model call.
func (m *Model) handleKB(rest string) {
	if m.app == nil {
		m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
		return
	}
	names := make([]string, 0, len(m.app.Cfg.KBs))
	for name := range m.app.Cfg.KBs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		m.addNote("· no knowledge bases configured")
		return
	}
	var lines []string
	for _, name := range names {
		cfg := m.app.Cfg.KBs[name]
		strategy, ok := kb.Strategy(cfg.Strategy)
		if !ok {
			strategy = cfg.Strategy + " (unknown)"
		}
		lines = append(lines, fmt.Sprintf("%-12s %-10s top_k %d", name, strategy, kbTopK(cfg)))
		lines = append(lines, "  path        "+m.kbPath(cfg.Path))
	}
	query := strings.TrimSpace(rest)
	if query != "" {
		lines = append(lines, "", "search  "+query)
		for _, name := range names {
			lines = append(lines, m.kbSearch(name, query)...)
		}
	}
	m.addCard("// knowledge bases", strings.Join(lines, "\n"), ui.Gold)
}

// kbTopK is the retrieval depth of one base: the configured one, or the
// default when the base does not set it.
func kbTopK(cfg config.KB) int {
	if cfg.TopK <= 0 {
		return kbDefaultTopK
	}
	return cfg.TopK
}

// kbPath resolves a configured KB path the way the compiler does: ~ expanded,
// absolute as-is, anything else relative to the workspace.
func (m *Model) kbPath(p string) string {
	p = strings.TrimSpace(config.ExpandHome(p))
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(m.app.Workspace, p)
}

// kbSearch runs the local BM25 search for one base and returns the rows to
// print. Everything is best-effort: a base that cannot be loaded, indexed or
// searched becomes one line saying so, never a failed command.
func (m *Model) kbSearch(name, query string) []string {
	cfg := m.app.Cfg.KBs[name]
	dir := m.kbPath(cfg.Path)
	if dir == "" {
		return []string{name + ": path is empty"}
	}
	k, err := kb.Load(dir)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	ix, err := kb.OpenIndex(k)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	defer ix.Close()
	// The index is derived and disposable: build it on first use.
	if err := kb.EnsureIndex(ix); err != nil {
		return []string{name + ": " + err.Error()}
	}
	hits, err := ix.Search(query, kbTopK(cfg), nil)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	if len(hits) == 0 {
		return []string{name + ": no hits"}
	}
	out := []string{name + ":"}
	for _, h := range hits {
		out = append(out, fmt.Sprintf("  %-18s %-40s %7.2f",
			truncate(h.Chunk.ID, 18), truncate(oneLine(h.Chunk.Title), 40), h.Score))
	}
	out = append(out, "  bm25 score: lower is better")
	return out
}

// memoryListLimit is how many entries /memory prints.
const memoryListLimit = 10

// memEntry is one row of the memory list: what it is, when it was written and
// what it says.
type memEntry struct {
	kind    string
	date    string
	summary string
}

// handleMemory implements /memory: the recent entries of the workspace
// memory store, newest first. The Markdown is the source of truth, so the
// list is read from learned.md and the skill files next to it — never from a
// stale index.
func (m *Model) handleMemory(rest string) {
	if m.mem == nil {
		m.addNote("· memory store unavailable")
		return
	}
	entries := m.memoryEntries()
	if len(entries) == 0 {
		m.addNote("· no memory entries yet — /remember <lesson> records one")
		return
	}
	query := strings.ToLower(strings.TrimSpace(rest))
	var lines []string
	for _, e := range entries {
		if len(lines) >= memoryListLimit {
			break
		}
		if query != "" && !strings.Contains(strings.ToLower(e.summary), query) {
			continue
		}
		date := e.date
		if date == "" {
			date = "—"
		}
		lines = append(lines, fmt.Sprintf("%-7s %-10s %s", e.kind, date, truncate(oneLine(e.summary), 60)))
	}
	if len(lines) == 0 {
		m.addNote("· no memory entries matched")
		return
	}
	m.addCard("// memory", strings.Join(lines, "\n"), ui.Gold)
}

// memoryEntries reads the workspace memory store: the lessons in learned.md
// and the skill files beside it. The list is in display order — the newest
// lesson first, the skills after them. An unreadable store is simply empty —
// the caller prints the one-line note.
func (m *Model) memoryEntries() []memEntry {
	workspace := ""
	if m.mem != nil {
		workspace = m.mem.Workspace
	}
	if workspace == "" && m.app != nil {
		workspace = m.app.Workspace
	}
	if workspace == "" {
		return nil
	}
	var lessons []memEntry
	if raw, err := os.ReadFile(memory.LearnedPath(workspace)); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			e, ok := memory.ParseLesson(line)
			if !ok {
				continue
			}
			lessons = append(lessons, memEntry{kind: "lesson", date: e.Date, summary: e.Text})
		}
	}
	// learned.md is append-only, so the last line is the newest lesson.
	for i, j := 0, len(lessons)-1; i < j; i, j = i+1, j-1 {
		lessons[i], lessons[j] = lessons[j], lessons[i]
	}
	files, _ := filepath.Glob(filepath.Join(memory.SkillsDir(workspace), "*.md"))
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lessons = append(lessons, memEntry{
			kind:    "skill",
			summary: filepath.Base(f) + " — " + oneLine(string(raw)),
		})
	}
	return lessons
}

// handleRewind implements /rewind (WP-2.2). With no argument it lists the
// session's checkpoints; with an id it restores that snapshot as the loop's
// resume history. Prior events are never rewritten: the rewind is recorded as a
// marker event, so `kintsugi run --resume` follows the same branch.
func (m *Model) handleRewind(rest string) {
	if m.log == nil || m.log.ID == "" || m.log.Path == "" {
		m.addNote("✗ no session store bound to this transcript · start a new session, then /rewind")
		return
	}
	id := strings.TrimSpace(rest)
	if id == "" {
		list, err := session.ListCheckpoints(m.log.ID)
		if err != nil {
			m.addNote("✗ " + err.Error())
			return
		}
		m.checkpoints = len(list)
		if len(list) == 0 {
			m.addNote("· no checkpoints · save one with `kintsugi checkpoint " + m.log.ID + " <label>`")
			return
		}
		lines := make([]string, 0, len(list)+2)
		for _, cp := range list {
			label := cp.Label
			if label == "" {
				label = "—"
			}
			mark := " "
			if cp.ID == m.rewoundTo {
				mark = "▸"
			}
			lines = append(lines, fmt.Sprintf("%s %-24s %3d msgs  %s",
				mark, cp.ID, len(cp.Messages), truncate(oneLine(label), 40)))
		}
		lines = append(lines, "", "/rewind <id> restores that checkpoint's history")
		m.addCard("// checkpoints", strings.Join(lines, "\n"), ui.Gold)
		return
	}
	if m.busy {
		m.addNote("· wait for the current turn to finish")
		return
	}
	cp, err := session.LoadCheckpoint(m.log.ID, id)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	if err := session.MarkRewind(m.log.ID, cp.ID); err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	m.hist = append([]provider.Message(nil), cp.Messages...)
	m.loadCheckpointState()
	m.addNote(fmt.Sprintf("rewound to %s (%d messages)", cp.ID, len(cp.Messages)))
}

// quickstartText is the startup card's body. It leads with the getting-started
// ladder — three steps that read the workspace and name the exact command for
// whatever is missing — and then carries the session facts. A wall of settings
// tells an operator nothing about whether they can start; a checklist does.
func (m *Model) quickstartText() string {
	// The opening card is short: what is missing, and the command that fixes it.
	// The session facts (persona/agents/kb) are reachable via /status and
	// /agents; printing them here makes the first screen a wall.
	var lines []string
	lines = append(lines, m.gettingStarted()...)
	return strings.Join(lines, "\n")
}

// sessionFacts is the reference block: what the session is built from. It
// deliberately omits the model and the context readiness — the ladder above
// states both, and stating them twice invited the two from disagreeing.
func (m *Model) sessionFacts() string {
	persona := m.comp.Manifest.Persona.Name
	if persona == "" {
		persona = "—"
	}
	if m.comp.Manifest.Persona.Rule != "" {
		persona += " (" + m.comp.Manifest.Persona.Rule + ")"
	}
	agents := "none"
	if _, p := m.app.AgentsMD(); p != "" {
		agents = p
	}
	kb := "none"
	var names []string
	for name := range m.app.Cfg.KBs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		kb = strings.Join(names, ", ")
	}
	return fmt.Sprintf("persona   %s\nagents    %s\nkb        %s",
		persona, truncate(agents, 78), kb)
}

// statusDetail is the stat block the /status card shows under the derived
// next-action line (see statusText in setup.go).
func (m *Model) statusDetail() string {
	elapsed := time.Since(m.start).Round(time.Second)
	total, budget := 0, 0
	if m.comp != nil {
		total = m.comp.Manifest.Totals.Tokens
		budget = m.comp.Manifest.Totals.Budget
	}
	cache := "✓ prefix stable"
	if stale := m.stalePaths(); len(stale) > 0 {
		cache = "✸ recompile on send · " + fileList(stale)
	}
	persona := m.personaName()
	if m.personaOverride != "" {
		persona += " (forced)"
	}
	agents := m.app.AgentsMDPath()
	if agents == "" {
		agents = "none"
	}
	return fmt.Sprintf("session %s\nmodel   %s\nrole    %s\nmode    %s\npersona %s\nagents  %s\nturns   %d · %s\ntokens  %d / %d\nrefusal hits %d · repairs %d\ncache   %s\nstack   %s",
		m.log.ID, m.viewModel(), m.role, m.modeLabel(), persona, truncate(agents, 64), m.turn, elapsed,
		total, budget, m.refusals, m.repairs, cache, shortHash(m.comp.Manifest.StackHash))
}

func (m *Model) inspectorText() string {
	var b strings.Builder
	total := 0
	for _, l := range m.comp.Manifest.Layers {
		if l.Dropped {
			continue
		}
		total += l.Tokens
		fmt.Fprintf(&b, "%-10s %5d  %s\n", l.ID, l.Tokens, shortHash(l.Hash))
	}
	fmt.Fprintf(&b, "%-10s %5d\n", "total", total)
	fmt.Fprintf(&b, "budget     %5d\n", m.comp.Manifest.Totals.Budget)
	fmt.Fprintf(&b, "stack      %s\n", shortHash(m.comp.Manifest.StackHash))
	fmt.Fprintf(&b, "repairs    %d · refusals %d", m.repairs, m.refusals)
	if m.advisor != nil {
		fmt.Fprintf(&b, "\nadvisor    %d notes · %d tok", m.advisorNotes, m.advisor.Used())
	}
	return b.String()
}

func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
