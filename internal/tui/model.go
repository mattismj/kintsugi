// Package tui is the terminal front-end. Visual language: the Kintsugi Terminal
// tokens (DESIGN §7.4) — ink ground, paper text, an amber heat ramp as the
// single brand family, sharp corners.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kintsugi/internal/advisor"
	"kintsugi/internal/agent"
	"kintsugi/internal/agents"
	"kintsugi/internal/audit"
	"kintsugi/internal/commands"
	"kintsugi/internal/continuity"
	"kintsugi/internal/harness"
	"kintsugi/internal/hub"
	"kintsugi/internal/intercept"
	"kintsugi/internal/keywords"
	"kintsugi/internal/ledger"
	"kintsugi/internal/memory"
	"kintsugi/internal/notify"
	"kintsugi/internal/prompt"
	"kintsugi/internal/provider"
	"kintsugi/internal/session"
	"kintsugi/internal/tools"
	"kintsugi/internal/ui"
)

const version = "v0.8"

type blockKind int

const (
	blockBanner blockKind = iota
	blockUser
	blockThinking
	blockAssistant
	blockTool
	blockNote
	blockCard
)

type block struct {
	kind   blockKind
	text   string
	title  string
	tone   ui.Token
	failed bool
	// expanded marks a thinking block the operator opened with ctrl+t. The
	// live stream shows a rolling window, a finished block collapses to a
	// few lines, and the full text is always one key away.
	expanded bool
	// diff carries the structured before/after for file-authoring tool calls
	// (edit, write, apply_patch). It is rendered as an inline unified diff
	// below the tool line, so an operator sees what changed instead of a
	// one-line "✓ edit path". nil for every other block kind.
	diff []tools.FileChange
}

type taskItem struct {
	label string
	state string
}

// Model is the Bubble Tea model. All rendering is derived from this state;
// view functions are pure so they can be snapshot-tested without a terminal.
type Model struct {
	app  *harness.App
	res  *harness.Resolved
	comp *prompt.Compiled
	loop *agent.Loop
	log  *session.Session
	mem  *memory.Store

	hist   []provider.Message
	blocks []block
	tasks  []taskItem
	todos  []tools.TodoItem
	stream int // index of the assistant block currently streaming, -1 when idle
	// thinkIdx is the thinking block's index while the model is reasoning,
	// -1 when idle. The thinking block is created before the answer starts
	// and stays in the transcript after the turn.
	thinkIdx int
	input    string
	// inputCursor is the caret's rune offset in input. The buffer holds newlines
	// (ctrl+j, alt+enter, and any bracketed paste), so this is the only thing
	// that says where typing lands.
	inputCursor int
	history     []string
	histPos     int
	busy        bool
	ch          chan tea.Msg
	ctx         context.Context
	cancel      context.CancelFunc

	theme               ui.Theme
	w, h                int
	leftBias, rightBias int
	scroll              int
	// anchor pins the viewport to an absolute transcript line while the
	// transcript grows underneath it. scroll is a distance from the bottom, so
	// appending lines moves the window; the anchor does not move. It is dropped
	// when the width changes, because a re-wrap renumbers every line.
	anchor scrollAnchor
	// copyHook observes clipboard payloads in tests; the real path needs a TTY.
	// It is nil in every run.
	copyHook func(text string)
	turn     int
	role     string
	start    time.Time
	status   string
	notice   string

	// transcriptMemo is the wrapped transcript for one width (M13). Wrapping is
	// the expensive part of View, and mouse tracking makes View hot: without
	// this, moving the cursor re-wraps the whole history per frame. The key is
	// a fingerprint of everything the wrap depends on, so it validates itself —
	// a mutation that forgets to invalidate still misses the cache, because the
	// fingerprint is recomputed from the model rather than remembered.
	transcriptMemo    []string
	transcriptMemoKey transcriptKey
	// revision is bumped by invalidateTranscript for changes a fingerprint
	// cannot see: a hot recompile that alters the banner behind an existing
	// block.
	revision int

	approval     *approvalReq
	refusals     int
	repairs      int
	advisor      *advisor.Advisor
	advisorNotes int

	// Layer 2/5 workbench state. wb is nil while the transcript owns the
	// screen; stamps detect on-disk edits between turns.
	wb              *workbenchView
	stamps          map[string]fileStamp
	personaOverride string

	// connect is the in-TUI provider sign-in panel (/connect). While it is
	// non-nil it owns the keyboard, the mouse and the screen.
	connect *connectView

	// picker is the model picker (/model with no argument). While it is
	// non-nil it owns the keyboard, the mouse and the screen.
	picker *modelPickerView

	// refusal is the refusal workbench (/refusal): the lexicon editor.
	// While it is non-nil it owns the keyboard, the mouse and the screen.
	refusal *refusalView

	// changes is the session change view (/changes): every file the agent
	// touched, latest state per path. While it is non-nil it owns the
	// keyboard, the mouse and the screen.
	changes   *changesView
	changeSet map[string]*changeRecord

	// liveTool is the running tool's streamed output (bash): the label and a
	// rolling window of output lines, rendered while the tool runs and
	// replaced by the final tool card when it returns. One tool runs at a
	// time in the loop, so one live block is the whole state.
	liveTool *liveToolBlock

	// suggest is the input line's completion popup (W3 item 3.4). It never owns
	// the keyboard: it consumes only ↑ ↓ tab enter esc and falls through for
	// everything else, so typing always reaches the buffer. suggestDismissed
	// remembers the token an esc (or an accept) closed the popup on, so a
	// refresh cannot reopen it on the same word; suggestVersion counts the
	// popup's own transitions (open, dismiss, accept), and suggestMemo is the
	// fingerprint — caret, buffer, version, dismissal — that keeps a key which
	// changes none of them from re-walking the workspace.
	suggest          *completeView
	suggestDismissed *suggestDismissal
	suggestVersion   int
	suggestMemo      string

	quotaProvider string
	quotaUsed     int64
	cacheRead     int64
	cacheWritten  int64
	quotaLimit    int64
	quotaReset    string
	closeTools    func()

	// notify (M10-D) raises approval-needed, turn-done and quota-warn events to
	// the operator's terminal. nil means off, which is the default: a harness
	// that pings the desktop is a decision the operator makes in config.
	notify *notify.Notifier

	// Checkpoint/rewind state (WP-2.2): how many snapshots the session holds
	// and which one the resume history was last restored from.
	checkpoints int
	rewoundTo   string

	// Continuity (M8.5): the workspace ledger stays open for the session so the
	// guard can recall prior accepted work without reopening it per turn, and
	// guard is nil whenever drift re-injection is off (/guard).
	ledger *ledger.Store
	guard  *continuity.Guard

	// policy is the session's live permission policy (M9). The mode inside it
	// is switched at runtime by /mode; the rules never change.
	policy tools.Policy

	// StartAgent starts one @mention turn (M9.3). It is a seam: nil means the
	// built-in runner (an agent turn on its own model, prompt and tool
	// allowlist), and a coordinator may replace it to route agent turns
	// through its own runner.
	StartAgent func(AgentMention) tea.Cmd

	// hub is the background subagent hub (M10-D). nil means no background runs.
	hub *hub.Hub
	// rosterCursor is the selected row in the roster panel.
	rosterCursor int
	// rosterFocus is set by ctrl+r: while it is true the roster owns the arrow
	// and action keys. Focus is transient by design — any unrelated key drops
	// it — so a roster can never swallow the operator's typing.
	rosterFocus bool

	// panel is the on-demand side panel: "" (none), "inspector" or "rail". The
	// default is "" — a centered transcript only. It switches full content in
	// and out, never two permanent columns.
	panel string
	// inspectTab is the inspector's tab: "work" (default) answers what the
	// session is doing; "harness" is the manifest/layer detail the operator
	// digs into deliberately.
	inspectTab string

	// startedFresh records whether this session began with no history, so the
	// mental-model digest (M10-C) is injected on the first turn of a fresh
	// session only. A resumed session already carries its own history and would
	// be re-told what it just said.
	startedFresh bool
	// mentalModelSent keeps the digest to once per session: it is orientation,
	// not per-turn context, and re-sending it would drift the cached prefix.
	mentalModelSent bool
}

// Close releases MCP child processes and the continuity ledger owned by this
// session.
func (m *Model) Close() {
	if m.closeTools != nil {
		m.closeTools()
		m.closeTools = nil
	}
	if m.loop != nil && m.loop.Tools != nil && m.loop.Tools.Audit != nil {
		m.loop.Tools.Audit.Close()
	}
	if m.ledger != nil {
		m.ledger.Close()
		m.ledger = nil
	}
	m.guard = nil
}

type approvalReq struct {
	tool    string
	detail  string
	changes []tools.FileChange
	reply   chan bool
}

type streamDelta struct{ s string }
type turnDone struct{ hist []provider.Message }

type errMsg struct{ err error }
type toolNotice struct {
	call provider.ToolCall
	res  tools.Result
}
type advisorNotice struct{ notes []advisor.Note }
type usageNotice struct{ prompt, completion int }
type cacheNotice struct{ read, written int }
type todoNotice struct{ items []tools.TodoItem }

// toolStreamNotice is one line of a running tool's output, streamed before
// the tool returns.
type toolStreamNotice struct {
	ev tools.ToolOutputEvent
}

// thinkingDelta is one reasoning delta, streamed before the answer.
type thinkingDelta struct{ s string }

// liveToolBlock is the running tool's live output: the label and a rolling
// window of lines. dropped counts the lines that rolled out of the window,
// so the render can say so instead of silently forgetting.
type liveToolBlock struct {
	label   string
	lines   []string
	dropped int
}

// toolStreamMaxLines is the rolling window: the operator watching a long
// scan sees the tail; the full output arrives with the tool's result.
const toolStreamMaxLines = 30

// thinking windows: the live stream shows the tail of the reasoning, a
// finished block collapses to its last lines, and ctrl+t opens the full text.
const (
	thinkingLiveLines      = 30
	thinkingCollapsedLines = 3
)

// appendToolStream extends (or opens) the live block with one streamed line.
func (m *Model) appendToolStream(ev tools.ToolOutputEvent) {
	if m.liveTool == nil {
		m.liveTool = &liveToolBlock{label: ev.Label}
	}
	if m.liveTool.label == "" {
		m.liveTool.label = ev.Label
	}
	lt := m.liveTool
	lt.lines = append(lt.lines, ev.Line)
	if len(lt.lines) > toolStreamMaxLines {
		lt.dropped += len(lt.lines) - toolStreamMaxLines
		lt.lines = lt.lines[len(lt.lines)-toolStreamMaxLines:]
	}
}

// clearLiveTool drops the live block: the tool returned (or the turn ended)
// and the final card is the record.
func (m *Model) clearLiveTool() { m.liveTool = nil }

// newestThinking returns the index of the most recent thinking block, or -1.
func (m *Model) newestThinking() int {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockThinking {
			return i
		}
	}
	return -1
}

// toggleThinking opens or closes the newest thinking block.
func (m *Model) toggleThinking() {
	i := m.newestThinking()
	if i < 0 {
		m.addNote("· no thinking block yet — a model that exposes its reasoning leaves one here")
		return
	}
	m.blocks[i].expanded = !m.blocks[i].expanded
	if m.blocks[i].expanded {
		m.addNote("· thinking expanded · ctrl+t to close")
	}
}

// copyThinking sends the newest thinking block's full text to the clipboard.
func (m *Model) copyThinking() {
	i := m.newestThinking()
	if i < 0 {
		m.addNote("· no thinking to copy yet")
		return
	}
	text := strings.TrimSpace(m.blocks[i].text)
	if text == "" {
		m.addNote("· the newest thinking block is empty")
		return
	}
	m.sendToClipboard(text, "thinking")
}

type interceptNotice struct {
	hit     intercept.Hit
	level   int
	outcome string
}

// driftNotice reports that a turn drifted and its metadata was recorded. It
// carries ids, a class label and an outcome only: a drift event has no prose, so
// there is nothing here that could show a refusal.
type driftNotice struct {
	matched []string
	class   string
	outcome string
}

// Gaze tuning (M13). Motion events arrive far faster than a frame is worth
// drawing, so they are recorded and applied on the animation clock; the idle
// drift is how long the creature keeps looking at a cursor that has stopped
// moving before it eases back to centre and resumes its blink.
// New builds the transcript model. opts carries the per-invocation CLI
// overrides (alt screen, mouse, boot) so the terminal is configured exactly once.
func New(app *harness.App, res *harness.Resolved, comp *prompt.Compiled, log *session.Session, mem *memory.Store, history []provider.Message, opts Options) *Model {
	if app != nil && log != nil {
		app.SessionID = log.ID
	}
	th := ui.Detect(os.Getenv)
	th.Ornament = app.Cfg.UI.Ornament

	m := &Model{
		app: app, res: res, comp: comp, log: log, mem: mem,
		theme: th, w: 80, h: 24, stream: -1, role: "default",
		start: time.Now(),
	}
	// A session that opens with history is resumed: it already knows what was
	// said, so the orientation digest stays out of it.
	m.startedFresh = len(history) == 0
	m.hist = append(m.hist, history...)
	m.histPos = len(m.hist)

	// The session's live permission policy (M9). /mode switches the mode
	// inside it; the rules never change.
	m.policy = app.ToolPolicy()

	ex := &tools.Executor{
		Workspace: app.Workspace,
		Memory:    mem,
		Policy:    m.policy,
		Log:       log.Log,
		Offline:   app.Cfg.Core.Mode == "offline",
	}
	ex.ApplySecurity(app.Cfg)
	// The audit log (M11.3): local, append-only, and never part of a prompt.
	// Every permission decision this session makes lands in the workspace.
	sessionID := ""
	if log != nil {
		sessionID = log.ID
	}
	if err := ex.EnableAudit(app.Workspace, sessionID); err != nil {
		log.Log("meta", map[string]string{
			"event": "audit_unavailable", "error": err.Error(),
		})
	}
	ex.Approver = m.approver()
	m.closeTools = app.AttachTools(ex, log.Log)

	m.loop = &agent.Loop{
		Client: res.Client, Tools: ex,
		HardStop:     app.Cfg.Intercept.HardStop,
		HardThinking: app.Cfg.Intercept.HardThinking,
		// The hook gate lives in the loop, so the loop's logger is where a
		// hook denial is folded into the audit log.
		Log:      audit.WrapLogger(log, ex.Audit),
		MaxLoops: app.Cfg.Harness.MaxToolLoops,
	}
	if app.Cfg.Intercept.Enabled {
		if lx, err := app.RefusalLexicon(); err == nil {
			m.loop.Interceptor = lx
			m.loop.Reminder = app.ReminderBuilder(comp)
			m.loop.MaxRefusalRetries = app.Cfg.Intercept.MaxRetries
		}
		// Operator stream rules (I-70). A nil set is the default path; a file
		// that parsed with warnings still loads the rules that were valid.
		if rules, err := app.StreamRules(); err == nil && len(rules.Ordered()) > 0 {
			m.loop.StreamRules = rules
			m.loop.OnRule = m.noteRuleEvent
		}
	}
	// Continuity guard (M8.5.3): on a drift, re-inject the prior *accepted* work
	// for this task and record drift as metadata. loop.PriorWork stays nil: the
	// block is pre-injected per task in submit, so it is never sent twice.
	if app.Cfg.Continuity.Enabled && app.Cfg.Continuity.GuardOnRefusal {
		m.installContinuityGuard()
	}
	if app.Cfg.Agents.Subagents {
		if mgr, err := app.TaskManagerWithRoles(res, comp, ex.MCP, log.Log); err == nil {
			ex.Tasks = mgr
		} else {
			log.Log("meta", map[string]string{"subagents": "disabled: " + err.Error()})
		}
	}
	// The background hub (M10-D) is the one AttachTools put on the executor, so
	// a task(background=true) and the roster below see the same runs. It is nil
	// when subagents are off, which is also when nothing can detach.
	m.hub = ex.Hub
	// Notifications (M10-D) are off unless the operator configures a backend.
	m.notify = notifyFor(app.Cfg.Notify)
	if app.AdvisorEnabled() {
		if adv, err := app.AdvisorFor(res); err == nil {
			m.advisor = adv
			m.loop.Advisor = adv
			m.loop.MaxAdvisorRevisions = 1
		} else {
			log.Log("meta", map[string]string{"advisor": "unavailable: " + err.Error()})
		}
	}

	m.loadQuota()
	m.stamps = m.currentStamps()

	// Boot reveal: only a TTY with color and a nameplate gets the type-in. Mono
	// (§7.4.10), --no-boot and no-banner sessions open straight on content.
	m.loadCheckpointState()
	if m.rewoundTo != "" {
		m.addNote(fmt.Sprintf("· session was rewound to %s · %d checkpoints saved", m.rewoundTo, m.checkpoints))
	}
	return m
}

// revealStartup appends the first-run quickstart. It runs either at
// construction or at the end of the boot reveal — never twice, because the boot
// path only calls it through finishBoot. The nameplate block is gone: the header
// already carries `kintsugi · model`, and a second line of "session · model" at the
// top of the transcript is noise. The opening state is mostly empty space plus
// the input box, like every other harness.
func (m *Model) revealStartup() {
	if len(m.hist) == 0 {
		m.addCard("// getting started", m.quickstartText(), ui.Gold)
		return
	}
	m.addNote(fmt.Sprintf("resumed %d messages from session %s", len(m.hist), m.log.ID))
}

// loadCheckpointState refreshes the checkpoint counters the inspector shows.
// Best effort: a session that cannot be read simply reports no checkpoints.
func (m *Model) loadCheckpointState() {
	m.checkpoints, m.rewoundTo = 0, ""
	if m.log == nil || m.log.ID == "" || m.log.Path == "" {
		// No store bound (dry runs, fixtures): nothing to read.
		return
	}
	if list, err := session.ListCheckpoints(m.log.ID); err == nil {
		m.checkpoints = len(list)
	}
	if cp, ok, err := session.LatestRewind(m.log.ID); err == nil && ok {
		m.rewoundTo = cp.ID
	}
}

// approver routes approval requests through the update loop, so the agent
// goroutine blocks until the operator answers. A cancelled task denies. The
// proposed file changes ride the request, so the approval card can show the
// diff and the operator answers informed.
func (m *Model) approver() tools.Approver {
	return tools.ApproverFunc(func(ar tools.ApprovalRequest) bool {
		req := approvalReq{tool: ar.Tool, detail: ar.Detail, changes: ar.Changes, reply: make(chan bool, 1)}
		select {
		case m.ch <- req:
		case <-m.ctx.Done():
			return false
		}
		select {
		case ok := <-req.reply:
			return ok
		case <-m.ctx.Done():
			return false
		}
	})
}

// Init has nothing to schedule: no boot reveal, no animation clock. The
// identity is glyph state, derived at render.
func (m *Model) Init() tea.Cmd {
	return nil
}

// loadQuota refreshes the inspector's ledger view for the active provider.
func (m *Model) loadQuota() {
	m.quotaProvider, m.quotaUsed, m.quotaLimit, m.quotaReset = "", 0, 0, ""
	if m.app == nil || m.res == nil {
		return
	}
	st, err := m.app.QuotaStore()
	if err != nil {
		return
	}
	for _, q := range st.Snapshot() {
		if q.Provider == m.res.ProviderName {
			m.quotaProvider, m.quotaUsed, m.quotaLimit, m.quotaReset = q.Provider, q.Used, q.Limit, q.Reset
			return
		}
	}
	m.quotaProvider = m.res.ProviderName
}

func (m *Model) layout() (leftW, rightW, centerW, bodyH int, showLeft, showRight bool) {
	w, h := m.w, m.h
	// One side panel is allowed, on demand, never two. "rail" shows the ember +
	// activity; "inspector" shows the manifest/pillars. The default is none.
	showLeft = m.panel == "rail"
	showRight = m.panel == "inspector"
	leftW = 0
	rightW = 30
	if showLeft {
		leftW = 22
	}

	// The transcript column is capped so a wide terminal does not stretch a
	// conversation into a slab; the cap plus the on-demand panel is the
	// opencode/cline negative-space aesthetic.
	const maxCols = 100
	centerW = w
	if centerW > maxCols {
		centerW = maxCols
	}
	if showLeft {
		centerW -= leftW
		centerW -= 3 // gutter
	}
	if showRight {
		centerW -= rightW
		centerW -= 3 // gutter
	}
	if centerW < 20 {
		centerW = 20
	}
	if centerW < 1 {
		centerW = 1
	}

	bodyH = h - 1 - m.footerHeight()
	if bodyH < 5 {
		bodyH = 5
	}
	return
}

func (m *Model) addBlock(b block) { m.blocks = append(m.blocks, b) }

// ---------- permission mode (M9) ----------

// currentMode is the session's live permission mode.
//
// The mode is the baseline the permission matrix starts from: it decides
// whether a mutating call is allowed, asked about or refused. An empty
// policy mode is the default mode, and an unparsable one never happens —
// the mode is only ever set through tools.ParseMode.
func (m *Model) currentMode() tools.Mode {
	mode, err := tools.ParseMode(m.policy.Mode)
	if err != nil {
		return tools.ModeDefault
	}
	return mode
}

// modeLabel is the short word the header and the inspector show, so the
// operator always knows the posture the session is in.
func (m *Model) modeLabel() string { return string(m.currentMode()) }

// setMode switches the session's permission mode.
//
// Only the baseline moves: the rules never change, and an explicit deny
// rule still wins in every mode. The switch reaches the executor, so the
// next tool call is judged by the new mode — a call already in flight is
// not re-judged.
func (m *Model) setMode(mode tools.Mode) {
	m.policy.Mode = string(mode)
	// The yolo flag is the config's blunt version of the mode: it would
	// override every other mode, so it follows the mode instead.
	m.policy.Yolo = mode == tools.ModeYolo
	if m.loop != nil && m.loop.Tools != nil {
		m.loop.Tools.Policy = m.policy
	}
}

// togglePanel switches the on-demand side panel: same key toggles it off. Only
// one panel is ever shown, so switching to "inspector" while "rail" is open
// replaces it.
func (m *Model) togglePanel(name string) {
	if m.panel == name {
		m.panel = ""
		return
	}
	m.panel = name
}

// sessionID is the id of the session this transcript belongs to.
func (m *Model) sessionID() string {
	if m.log == nil {
		return ""
	}
	return m.log.ID
}

// ---------- scroll ----------

// transcriptHeight and transcriptWidth mirror the panel math in View so a
// scroll can be clamped without rendering. They share layout(), so a resize
// moves the clamp with the column.
func (m *Model) transcriptWidth() int {
	_, _, centerW, _, _, _ := m.layout()
	return max(centerW, 4)
}

func (m *Model) transcriptHeight() int { return max(m.bodyHeight()-2, 1) }

// bodyHeight is the panel body height the layout hands to every column.
func (m *Model) bodyHeight() int {
	_, _, _, bodyH, _, _ := m.layout()
	return bodyH
}

// maxScroll is the furthest the transcript can scroll back: one screen beyond
// the last line. m.scroll counts lines back from the bottom, so 0 is live.
func (m *Model) maxScroll() int {
	return max(len(m.transcriptLines(m.transcriptWidth()))-m.transcriptHeight(), 0)
}

// scrollBy moves the transcript by delta lines (positive = up) and clamps to
// [0, maxScroll]. Every scroll path — wheel, PgUp/PgDn — goes through here, so
// the view can never run past either end.
func (m *Model) scrollBy(delta int) {
	m.scroll = clampInt(m.scroll+delta, 0, m.maxScroll())
	m.pinAnchor()
}

// scrollAnchor is the viewport's fixed point: an absolute transcript line that
// the window is positioned against. While the model streams, lines are appended
// below it, and scroll — a distance from the bottom — would slide the window
// down with them. The anchor holds still instead.
type scrollAnchor struct {
	line  int // absolute index into transcriptLines
	width int // the width it was taken at; a re-wrap invalidates it
	valid bool
	// seen is the transcript length at the last render, so the footer can say
	// whether anything arrived below the pinned line.
	seen int
}

// pinAnchor records the current viewport as an anchor. At the live edge there is
// nothing to pin to — the operator wants the newest line — so the anchor is
// cleared rather than pointed at a moving bottom.
func (m *Model) pinAnchor() {
	if m.scroll <= 0 {
		m.anchor = scrollAnchor{}
		return
	}
	width := m.transcriptWidth()
	lines := m.transcriptLines(width)
	top := len(lines) - m.scroll - m.transcriptHeight()
	if top < 0 {
		top = 0
	}
	// An anchor inside the streaming block is meaningless: that block's line
	// count changes on every delta. Clamp it to the block's first line, which is
	// the last line index that stays put.
	if m.stream >= 0 && m.stream == len(m.blocks)-1 {
		top = min(top, m.firstLineOfLastBlock(width, len(lines)))
	}
	m.anchor = scrollAnchor{line: top, width: width, valid: true, seen: len(lines)}
}

// firstLineOfLastBlock is where the streaming block starts in the wrapped
// transcript, so an anchor can never point inside content that is still growing.
func (m *Model) firstLineOfLastBlock(width, total int) int {
	if len(m.blocks) == 0 {
		return 0
	}
	count := len(m.blockLines(len(m.blocks)-1, m.blocks[len(m.blocks)-1], width))
	start := total - count
	// buildTranscriptLines appends the live caret line below the last block
	// while a turn is running, so the block does not reach the slice's end.
	if m.busy && m.stream >= 0 && m.stream < len(m.blocks) {
		start--
	}
	return max(start, 0)
}

// anchorScroll converts the anchor back into a scroll offset. It returns
// (scroll, ok); ok is false when there is no usable anchor and the caller should
// keep whatever scroll it has.
func (m *Model) anchorScroll(width, height int) (int, bool) {
	if !m.anchor.valid || m.anchor.width != width {
		return 0, false
	}
	lines := m.transcriptLines(width)
	return clampInt(len(lines)-m.anchor.line-height, 0, m.maxScroll()), true
}

// newLinesBelow reports how many transcript lines arrived after the anchor was
// taken, which is what the footer turns into "new output".
func (m *Model) newLinesBelow() int {
	if !m.anchor.valid {
		return 0
	}
	return max(len(m.transcriptLines(m.transcriptWidth()))-m.anchor.seen, 0)
}

// clearAnchor drops the pin. Every site that sends the viewport back to the live
// edge goes through here, so a stale anchor can never drag the view away.
func (m *Model) clearAnchor() { m.anchor = scrollAnchor{} }

// ---------- input buffer ----------

// inputRunes is the input as runes, the unit inputCursor is measured in.
func (m *Model) inputRunes() []rune { return []rune(m.input) }

// insertRunes inserts text at the caret and leaves the caret after it. It is the
// only path that grows the buffer, so the cursor can never desynchronise.
func (m *Model) insertRunes(rs []rune) {
	if len(rs) == 0 {
		return
	}
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	out := make([]rune, 0, len(buf)+len(rs))
	out = append(out, buf[:at]...)
	out = append(out, rs...)
	out = append(out, buf[at:]...)
	m.input = string(out)
	m.inputCursor = at + len(rs)
}

// setInput replaces the buffer and puts the caret at the end. History replay and
// completion both use it.
func (m *Model) setInput(s string) {
	m.input = s
	m.inputCursor = len([]rune(s))
}

// backspace deletes the rune before the caret. At the start of a line it joins
// the previous line, which is what makes a multi-line buffer feel like one.
func (m *Model) backspace() {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	if at == 0 {
		return
	}
	out := append(append([]rune{}, buf[:at-1]...), buf[at:]...)
	m.input = string(out)
	m.inputCursor = at - 1
}

// insertNewline opens a new line at the caret.
func (m *Model) insertNewline() { m.insertRunes([]rune{'\n'}) }

// moveCaret moves the caret by delta runes, travelled through the buffer.
func (m *Model) moveCaret(delta int) {
	m.inputCursor = clampInt(m.inputCursor+delta, 0, len(m.inputRunes()))
}

// lineStart is the caret index of the beginning of its current line.
func (m *Model) lineStart() int {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	for at > 0 && buf[at-1] != '\n' {
		at--
	}
	return at
}

// lineEnd is the caret index of the end of its current line.
func (m *Model) lineEnd() int {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	for at < len(buf) && buf[at] != '\n' {
		at++
	}
	return at
}

// caretRow is the zero-based buffer row the caret sits on.
func (m *Model) caretRow() int {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	return strings.Count(string(buf[:at]), "\n")
}

// caretVertical moves the caret one buffer row up or down, keeping its column
// where the target row is long enough and clamping it where it is not.
func (m *Model) caretVertical(delta int) {
	buf := m.inputRunes()
	at := clampInt(m.inputCursor, 0, len(buf))
	col := at - strings.LastIndex(string(buf[:at]), "\n") - 1
	row := m.caretRow()

	start := 0
	for i := 0; i < row+delta; i++ {
		n := strings.IndexByte(string(buf[start:]), '\n')
		if n < 0 {
			return
		}
		start += n + 1
	}
	end := len(buf)
	if n := strings.IndexByte(string(buf[start:]), '\n'); n >= 0 {
		end = start + n
	}
	m.inputCursor = clampInt(start+col, start, end)
}

// inputRows is how many rows the input box needs: one per buffer line, capped so
// a large paste can never eat the transcript. It is the single source of truth
// for the footer's height.
func (m *Model) inputRows() int {
	rows := strings.Count(m.input, "\n") + 1
	return clampInt(rows, 1, inputRowsMax)
}

// inputRowsMax caps the input box. Beyond this the buffer scrolls inside the box
// instead of the transcript shrinking to nothing.
const inputRowsMax = 5

// footerHeight is the rows footer() draws: the input box, the hint row and the
// stamp. layout() subtracts it, so a taller box and the scroll math can never
// disagree. A modal draws its own footer and hides the input box, so it keeps
// the fixed three rows regardless of what is drafted behind it.
func (m *Model) footerHeight() int {
	if m.wb != nil || m.connect != nil || m.picker != nil || m.refusal != nil || m.changes != nil {
		return 3
	}
	return m.inputRows() + 2
}

// wbMaxScroll mirrors the clamp workbenchBody applies at render time, so the
// wheel reaches the same bound the renderer would impose a frame later.
func (m *Model) wbMaxScroll() int {
	if m.wb == nil {
		return 0
	}
	return max(len(m.workbenchLines(m.w-2))-max(m.bodyHeight()-3, 1), 0)
}

// workbenchLines is the unpadded workbench content, matching what
// workbenchBody renders before its own clamp and padding.
func (m *Model) workbenchLines(width int) []string {
	if m.wb == nil {
		return nil
	}
	var lines []string
	if m.wb.kind == "context" {
		lines = m.contextLines(width)
	} else if m.wb.kind == "plan" {
		lines = m.planLines(width)
	} else if m.wb.kind == "agents" {
		lines = m.modelCtxLines(width)
	} else {
		lines = m.personaLines(width)
	}
	for _, n := range m.wb.notices {
		lines = append(lines, m.theme.Paint(ui.Warn, n))
	}
	if len(lines) == 0 {
		lines = []string{m.theme.Paint(ui.PaperSoft, "· no data")}
	}
	return lines
}

// wbScrollBy scrolls the workbench and clamps it: positive delta = down, the
// same sign convention handleWorkbenchKey uses for j/↓.
func (m *Model) wbScrollBy(delta int) {
	if m.wb == nil {
		return
	}
	m.wb.scroll = clampInt(m.wb.scroll+delta, 0, m.wbMaxScroll())
}

func (m *Model) addNote(text string) {
	m.addBlock(block{kind: blockNote, text: text})
}

func (m *Model) addCard(title, body string, tone ui.Token) {
	// Every card carries the identity glyph for the event it reports (or the
	// session's current state), so a glance down the transcript shows what the
	// harness is doing without reading the text (§7.4.3).
	state := m.cardState(title)
	m.addBlock(block{kind: blockCard, title: m.cardTitle(state, tone, title), text: body, tone: tone})
}

func (m *Model) markTask(state string) {
	if len(m.tasks) == 0 {
		return
	}
	m.tasks[len(m.tasks)-1].state = state
}

func (m *Model) submit(task string) tea.Cmd {
	return m.submitExpanded(commands.Expansion{Original: task, Prompt: task})
}

// submitExpanded sends an expanded prompt (M9.8): a custom slash command, or
// a line whose context references were expanded. The transcript shows the
// operator's original line, what was injected and the prompt that was sent —
// a rewrite is never hidden.
func (m *Model) submitExpanded(exp commands.Expansion) tea.Cmd {
	// Pick up external edits to CONTEXT.md or the persona pack before the turn
	// reads comp.Text. The seam is shown in the transcript, never hidden.
	m.hotReload()
	m.history = append(m.history, exp.Original)
	m.histPos = len(m.history)
	m.tasks = append(m.tasks, taskItem{label: exp.Original, state: "running"})
	m.addBlock(block{kind: blockUser, text: exp.Original})
	m.addExpansionNote(exp.Summary())
	// Prose-only turn modifiers (I-79). The framing note is appended to the task
	// sent to the model; the operator's own words reach it unchanged, and no
	// keyword can alter permissions, scope or the tool set.
	framed, honoured := keywords.Apply(exp.Prompt)
	if len(honoured) > 0 {
		m.addNote("· " + strings.Join(honoured, " · ") + " · turn framing applied")
		exp.Prompt = framed
	}
	if exp.Prompt != exp.Original {
		m.addCard("// prompt · sent", displayPrompt(exp.Prompt), ui.Gold)
	}
	m.injectMentalModel()
	m.injectContinuity(exp.Prompt)
	m.busy = true
	m.scroll = 0
	m.clearAnchor()
	m.turn++
	m.ctx, m.cancel = context.WithCancel(context.Background())
	// The cadence rises from idle to active; re-arm alongside the turn so the
	// ember speeds up immediately (the generation counter retires the old tick).
	return m.startTask(exp.Prompt)
}

// promptDisplayCap bounds how much of an expanded prompt the transcript
// shows. The whole prompt is always what is sent; the transcript shows enough
// of it to see what happened.
const promptDisplayCap = 4000

// addExpansionNote shows what an expansion injected: one line per reference
// and one per note, so a glance down the transcript says what was added to
// the prompt and how big it was.
func (m *Model) addExpansionNote(summary string) {
	for _, line := range strings.Split(summary, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m.addNote("· " + line)
	}
}

// displayPrompt is an expanded prompt as the transcript shows it: the head of
// the prompt, and a marker when the rest was left out.
func displayPrompt(p string) string {
	if len(p) <= promptDisplayCap {
		return p
	}
	head := strings.ToValidUTF8(p[:promptDisplayCap], "")
	return head + fmt.Sprintf("\n…[the transcript shows %d of %d bytes; the whole prompt was sent]",
		promptDisplayCap, len(p))
}

// injectMentalModel queues the session's orientation digest (M10-C) as a system
// message, once, on the first turn of a fresh session only. It is distinct from
// the continuity prior-work block — that one is task-matched recall, this is the
// standing picture of what the workspace already holds — and it is labelled as
// such by its renderer.
//
// It reads only positive ledger kinds and lessons, so a refusal cannot reach it.
// A digest that fails to render is dropped silently: orientation must never fail
// a turn.
func (m *Model) injectMentalModel() {
	if m.app == nil || !m.startedFresh || m.mentalModelSent {
		return
	}
	if !m.app.Cfg.Continuity.Enabled {
		return
	}
	block := strings.TrimSpace(m.app.MentalModel())
	if block == "" {
		return
	}
	m.mentalModelSent = true
	m.hist = append(m.hist, provider.Message{Role: "system", Content: block})
	if m.log != nil {
		m.log.Log("mental_model", map[string]any{"bytes": len(block)})
	}
	m.addCard("// "+memory.MentalModelLabel,
		fmt.Sprintf("%d token(s) · accepted work + newest lessons\nqueued as a system message for this turn",
			memory.Tokens(block)), ui.Gold)
}

// injectContinuity queues the local continuity material for task as system
// messages before the turn starts: the preflight frame when the local scorer
// says it is worth injecting, then the prior *accepted* work the ledger recalls.
// Both are visible in the transcript — the seam is never hidden — and both are
// positive material only: a refusal is never memory, so neither block can carry
// one.
func (m *Model) injectContinuity(task string) {
	if m.app == nil || strings.TrimSpace(task) == "" {
		return
	}
	inj := continuityForTask(m.app, m.res, task)
	if len(inj.Messages) == 0 {
		return
	}
	m.hist = append(m.hist, inj.Messages...)
	if inj.Preflight {
		m.addCard("// preflight · frame injected",
			fmt.Sprintf("score %d · class %s\noperator frame queued as a system message for this turn",
				inj.Score, inj.Classes), ui.Warn)
	}
	if len(inj.Prior) > 0 {
		m.addCard("// continuity · prior accepted work",
			fmt.Sprintf("%d item(s) · ids %s\nqueued as a system message for this turn",
				len(inj.Prior), strings.Join(inj.Prior, ", ")), ui.Gold)
	}
}

func (m *Model) startTask(task string) tea.Cmd {
	ch := make(chan tea.Msg, 1024)
	m.ch = ch
	m.stream = -1
	hist := m.hist
	system := m.comp.Text

	m.wireCallbacks(m.loop, ch)
	m.wireToolStream(ch)

	go func() {
		defer close(ch)
		final, updated, err := m.loop.RunConversation(m.ctx, system, hist, task)
		if err != nil {
			ch <- errMsg{err}
			_ = final
			return
		}
		ch <- turnDone{hist: updated}
	}()
	return listen(ch)
}

// wireCallbacks attaches the transcript callbacks to a loop. The session turn
// and an agent turn run on different loops but share one set of callbacks, so
// the wiring lives here and nowhere else.
func (m *Model) wireCallbacks(loop *agent.Loop, ch chan tea.Msg) {
	if loop == nil {
		return
	}
	loop.OnDelta = func(s string) {
		select {
		case ch <- streamDelta{s}:
		default: // drop under back-pressure; the final content is authoritative
		}
	}
	loop.OnIntercept = func(hit intercept.Hit, level int, outcome string) {
		select {
		case ch <- interceptNotice{hit: hit, level: level, outcome: outcome}:
		default:
		}
	}
	loop.OnTool = func(call provider.ToolCall, res tools.Result) {
		select {
		case ch <- toolNotice{call: call, res: res}:
		default:
		}
	}
	loop.OnUsage = func(p, c int) {
		select {
		case ch <- usageNotice{prompt: p, completion: c}:
		default:
		}
	}
	loop.OnCache = func(r, w int) {
		select {
		case ch <- cacheNotice{read: r, written: w}:
		default:
		}
	}
	loop.OnAdvisor = func(notes []advisor.Note) {
		select {
		case ch <- advisorNotice{notes: notes}:
		default:
		}
	}
	if loop.Tools != nil {
		loop.Tools.OnTodo = func(items []tools.TodoItem) {
			select {
			case ch <- todoNotice{items: items}:
			default:
			}
		}
	}
}

// wireToolStream attaches the executor's output stream to this turn's
// channel: the live block rides the same non-blocking drop the assistant
// deltas use, so a tool flooding output can never stall the turn.
func (m *Model) wireToolStream(ch chan tea.Msg) {
	if m.loop == nil || m.loop.Tools == nil {
		return
	}
	// The loop's tool runner is the executor this model built; the type is
	// concrete, so the wiring is a direct field set.
	ex := m.loop.Tools
	ex.OnOutput = func(ev tools.ToolOutputEvent) {
		select {
		case ch <- toolStreamNotice{ev: ev}:
		default: // drop under back-pressure; the final content is authoritative
		}
	}
}

func listen(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *Model) viewModel() string {
	if m.res == nil {
		return ""
	}
	return m.res.ProviderName + "/" + m.res.Model.ID
}

func (m *Model) personaName() string {
	if m.comp != nil && m.comp.Manifest.Persona.Name != "" {
		return m.comp.Manifest.Persona.Name
	}
	return "?"
}

// toolLine is the compact transcript card for a tool call (DESIGN §7.4.8).
func (m *Model) toolLine(tc provider.ToolCall, res tools.Result) block {
	glyph := "✓"
	tone := ui.OK
	if res.Failed {
		glyph = "✗"
		tone = ui.Danger
	}
	arg := firstArg(tc.Function.Arguments)
	label := tc.Function.Name
	if arg != "" {
		label += " " + arg
	}
	body := ""
	if res.Failed {
		body = truncate(cleanJSON(res.Content), 160)
	}
	return block{
		kind:   blockTool,
		text:   glyph + " " + label,
		title:  body,
		tone:   tone,
		failed: res.Failed,
		// The diff is rendered inline below the tool line on success. A tool
		// that carries no FileChanges (read, bash, grep, every non-authoring
		// tool) leaves this nil and the renderer shows nothing — the line
		// stays the one-liner it always was.
		diff: res.Diff,
	}
}

func firstArg(args string) string {
	for _, key := range []string{"path", "pattern", "command", "query"} {
		if v := jsonField(args, key); v != "" {
			return truncate(v, 60)
		}
	}
	return ""
}

func jsonField(args, key string) string {
	needle := `"` + key + `"`
	i := strings.Index(args, needle)
	if i < 0 {
		return ""
	}
	rest := args[i+len(needle):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func cleanJSON(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	}
	return s
}

// truncate keeps at most w cells of s and marks a cut with an ellipsis. The
// ellipsis sits outside the budget, so a cut result is w+1 cells — that is the
// contract the callers budget for (they pass width-1 where a frame is adjacent),
// and clipShorten exists for the one renderer that needs the mark inside.
//
// It counts terminal cells, not runes: a CJK or double-width glyph is one rune
// and two cells, so a rune count let a wide path overflow its panel by up to
// twice its budget, and fitScreen only clamps height.
func truncate(s string, w int) string {
	if w <= 0 {
		return "…"
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	// Walk cells, not runes: an ANSI escape is a single styling instruction whose
	// bytes would otherwise be cut in half, leaking the tail ("[32m") into the
	// transcript as literal text. Escapes are copied whole and cost no cells; a
	// wide glyph costs two.
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if n := ansiSequenceLen(s[i:]); n > 0 {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
			i++ // a stray escape byte: drop it rather than emit it
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := lipgloss.Width(string(r))
		if used+rw > w {
			break
		}
		b.WriteString(s[i : i+size])
		used += rw
		i += size
	}
	// A reset only when the kept text actually carried styling: a plain string
	// must stay byte-plain, or mono output would grow escapes it promised not to
	// emit, and a test comparing plain bytes would fail for the wrong reason.
	if strings.ContainsRune(b.String(), 0x1b) {
		return b.String() + "\x1b[0m…"
	}
	return b.String() + "…"
}

// ansiSequenceLen returns the byte length of the ANSI escape sequence at the
// start of s, or 0 when it is not one this package recognises.
func ansiSequenceLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 0
	}
	switch s[1] {
	case '[': // CSI: ESC [ params final
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return 0
	case ']': // OSC: ESC ] ... BEL or ST
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return 0
	default:
		return 2 // a two-byte escape
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------- markdown-defined agents (M9.3) ----------

// AgentMention is a resolved @mention: everything one agent turn needs. The
// operator typed "@name task"; the harness resolved the name to an agent, the
// agent to a model and a prompt, and the agent to a tool allowlist.
type AgentMention struct {
	Agent      agents.Agent
	Task       string   // what the operator asked the agent to do
	Model      string   // the model ref to run on; empty means the session's model
	Role       string   // the role to run on; empty means no role
	System     string   // the system prompt: the agent's body
	Tools      []string // the effective tool allowlist
	MaxTurns   int
	Background bool
}

// loadAgents reads the agent tiers for this workspace: the workspace tier
// first, then the user tier. The tiers are read on demand, so an agent file
// the operator just edited is visible on the next mention.
func (m *Model) loadAgents() []agents.Agent {
	if m.app == nil {
		return nil
	}
	list, _ := agents.Load(agents.Dirs(m.app.Workspace)...)
	return list
}

// resolveMention resolves "@name task" against the loaded agents. An unknown
// name is an error, never a turn: the line is not consumed.
func (m *Model) resolveMention(line string) (AgentMention, error) {
	name, task := splitMention(line)
	if name == "" {
		return AgentMention{}, fmt.Errorf("a mention needs an agent name: @<name> <task>")
	}
	if task == "" {
		return AgentMention{}, fmt.Errorf("@%s needs a task: @%s <what to do>", name, name)
	}
	list := m.loadAgents()
	a, ok := agents.Select(list, name)
	if !ok {
		// The name is matched exactly first; a case difference is the one
		// forgiveness, and the agent's own spelling is what is reported.
		for _, cand := range list {
			if strings.EqualFold(cand.Name, name) {
				a, ok = cand, true
				break
			}
		}
	}
	if !ok {
		return AgentMention{}, fmt.Errorf("unknown agent %q — %s", name, agentNames(list))
	}
	ref, role, err := m.resolveAgentModel(a)
	if err != nil {
		return AgentMention{}, err
	}
	system := strings.TrimSpace(a.Body)
	if system == "" && m.comp != nil {
		system = m.comp.Text
	}
	allow := a.ToolList()
	if a.ReadOnly() {
		allow = intersectTools(allow, agents.ReadOnlyTools)
	}
	return AgentMention{
		Agent:      a,
		Task:       task,
		Model:      ref,
		Role:       role,
		System:     system,
		Tools:      allow,
		MaxTurns:   a.MaxTurns,
		Background: a.Background,
	}, nil
}

// resolveAgentModel maps the model an agent declares onto what the session
// can run: a "provider/model" ref is used as-is, a bare word is a role, and
// an agent that names no model runs on the session's own model.
func (m *Model) resolveAgentModel(a agents.Agent) (ref, role string, err error) {
	decl := strings.TrimSpace(a.Model)
	if decl == "" {
		if m.res != nil {
			return m.res.ProviderName + "/" + m.res.Model.ID, "", nil
		}
		return "", "", fmt.Errorf("agent %q names no model and the session has none", a.Name)
	}
	if strings.Contains(decl, "/") {
		if m.app != nil {
			if _, _, _, err := m.app.Cfg.ResolveModel(decl); err != nil {
				return "", "", fmt.Errorf("agent %q: %w", a.Name, err)
			}
		}
		return decl, "", nil
	}
	switch strings.ToLower(decl) {
	case "coder", "researcher", "opsec", "reviewer", "default":
		return "", strings.ToLower(decl), nil
	}
	return "", "", fmt.Errorf(
		"agent %q names %q, which is neither a model ref (provider/model) nor a role (coder|researcher|opsec)",
		a.Name, decl)
}

// agentModel resolves the model one agent turn runs on.
func (m *Model) agentModel(am AgentMention) (*harness.Resolved, error) {
	switch {
	case am.Role != "":
		return m.app.ResolveRole(am.Role)
	case am.Model != "":
		return m.app.Resolve(am.Model)
	}
	if m.res != nil {
		return m.res, nil
	}
	return nil, fmt.Errorf("no model to run the agent on")
}

// agentExecutor builds the executor one agent turn runs on.
//
// The agent's allowlist is the permission: the approver refuses every tool
// the allowlist does not name. The subagent manager and the MCP servers are
// deliberately left off — delegation stays one level deep, and an agent
// cannot reach a tool the operator did not name it.
func (m *Model) agentExecutor(am AgentMention) *tools.Executor {
	ex := &tools.Executor{}
	if m.app != nil {
		ex.Workspace = m.app.Workspace
		ex.Policy = m.app.ToolPolicy()
		ex.Offline = m.app.Cfg.Core.Mode == "offline"
	}
	if m.log != nil {
		ex.Log = m.log.Log
	}
	ex.Approver = m.agentApprover(am)
	return ex
}

// agentApprover is the permission layer of one agent turn. A tool the
// allowlist names is allowed; every other tool is refused. A mutating tool
// the allowlist does name still asks the operator first, unless the agent
// declares the yolo permission mode.
func (m *Model) agentApprover(am AgentMention) tools.Approver {
	allow := map[string]bool{}
	for _, t := range am.Tools {
		allow[t] = true
	}
	mode := strings.ToLower(strings.TrimSpace(am.Agent.PermissionMode))
	return tools.ApproverFunc(func(ar tools.ApprovalRequest) bool {
		if !allow[ar.Tool] {
			return false
		}
		switch mode {
		case "yolo", "accept-edits":
			return true
		}
		if mutatingTools[ar.Tool] {
			// The allowlist names it, but the operator still answers.
			return m.approver().Approve(ar)
		}
		return true
	})
}

// mutatingTools are the tools that change the workspace or leave the machine.
// An agent that names one of them still waits for the operator.
var mutatingTools = map[string]bool{
	"edit": true, "write": true, "apply_patch": true, "bash": true, "webfetch": true,
}

// agentLoop builds the loop for one agent turn. The session's loop settings
// (refusal interception, advisor, continuity) carry over; the agent's own
// max_turns caps the tool loops.
func (m *Model) agentLoop(res *harness.Resolved, ex *tools.Executor, am AgentMention) *agent.Loop {
	loop := &agent.Loop{Client: res.Client, Tools: ex, MaxLoops: 8}
	if m.loop != nil {
		loop.Log = m.loop.Log
		loop.MaxLoops = m.loop.MaxLoops
		loop.Interceptor = m.loop.Interceptor
		loop.Reminder = m.loop.Reminder
		loop.MaxRefusalRetries = m.loop.MaxRefusalRetries
		loop.Advisor = m.loop.Advisor
		loop.MaxAdvisorRevisions = m.loop.MaxAdvisorRevisions
		loop.Hooks = m.loop.Hooks
		loop.SessionID = m.loop.SessionID
		loop.CWD = m.loop.CWD
		loop.Model = m.loop.Model
		loop.Persona = m.loop.Persona
	}
	if am.MaxTurns > 0 {
		loop.MaxLoops = am.MaxTurns
	}
	return loop
}

// runAgentTurn runs one @mention turn: the agent's model, the agent's prompt
// and the agent's tools. It is a side conversation — the session history is
// left alone, so an agent's answer never rewrites what the operator and the
// session have said.
func (m *Model) runAgentTurn(am AgentMention) tea.Cmd {
	if m.app == nil {
		m.addNote("✗ no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
		return nil
	}
	res, err := m.agentModel(am)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return nil
	}
	ex := m.agentExecutor(am)
	ch := make(chan tea.Msg, 1024)
	m.ch = ch
	m.stream = -1
	hist := m.hist
	loop := m.agentLoop(res, ex, am)
	m.wireCallbacks(loop, ch)

	go func() {
		defer close(ch)
		if _, _, err := loop.RunConversation(m.ctx, am.System, nil, am.Task); err != nil {
			ch <- errMsg{err}
			return
		}
		ch <- turnDone{hist: hist}
	}()
	return listen(ch)
}

// splitMention splits "@name the task" into the agent name and the task.
func splitMention(line string) (name, task string) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "@"))
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		task = strings.TrimSpace(s[i+1:])
		s = s[:i]
	}
	return strings.TrimSpace(s), task
}

// agentNames renders the loaded agent names for an error message.
func agentNames(list []agents.Agent) string {
	if len(list) == 0 {
		return "no agents are loaded (add .kintsugi/agents/<name>.md to the workspace)"
	}
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, a.Name)
	}
	return "loaded agents: " + strings.Join(names, ", ")
}

// intersectTools keeps the tools of a that are also in b, in a's order.
func intersectTools(a, b []string) []string {
	keep := map[string]bool{}
	for _, t := range b {
		keep[t] = true
	}
	var out []string
	for _, t := range a {
		if keep[t] {
			out = append(out, t)
		}
	}
	return out
}

// ---------- custom commands and context references (M9.8) ----------

// loadCommands reads the custom command tiers for this workspace: the
// workspace tier first, then the user tier. They are read on demand, so a
// command file the operator just edited is visible on the next line.
func (m *Model) loadCommands() []commands.Command {
	if m.app == nil {
		return nil
	}
	return commands.Load(commands.Dirs(m.app.Workspace)...)
}

// commandOptions is the expansion context for one line: the workspace, the
// discovered commands and the session's permission policy. The policy is the
// one the bash tool uses, so a `!` line in a command file is gated exactly
// like a shell command the model asked for.
func (m *Model) commandOptions() commands.Options {
	opts := commands.Options{Commands: m.loadCommands()}
	if m.app != nil {
		opts.Workspace = m.app.Workspace
		opts.Policy = m.app.ToolPolicy()
	}
	return opts
}

// expandLine expands the context references in the operator's line. A path
// that cannot be read stays literal: the turn never fails over a reference.
func (m *Model) expandLine(line string) commands.Expansion {
	return commands.ExpandReferences(line, m.commandOptions())
}

// expandCommand expands a custom slash command and the context references
// inside its body. The second result is false when the line names no custom
// command.
func (m *Model) expandCommand(line string) (commands.Expansion, bool) {
	opts := m.commandOptions()
	exp, ok := commands.ExpandCommand(line, opts)
	if !ok {
		return commands.Expansion{}, false
	}
	refs := commands.ExpandReferences(exp.Prompt, opts)
	exp.Prompt = refs.Prompt
	exp.Refs = refs.Refs
	return exp, true
}
