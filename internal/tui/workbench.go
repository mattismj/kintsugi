package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbletea"

	fctx "kintsugi/internal/context"
	"kintsugi/internal/harness"
	"kintsugi/internal/persona"
	"kintsugi/internal/plan"
	"kintsugi/internal/prompt"
	"kintsugi/internal/ui"
	"kintsugi/internal/workbench"
)

// fileStamp is the cheap on-disk identity used for hot reload. ModTime alone
// misses same-second edits on some filesystems, so size participates too.
type fileStamp struct {
	Exists bool
	Mod    int64
	Size   int64
}

type workbenchView struct {
	kind    string // context | persona | plan | agents
	ctx     *workbench.ContextSnapshot
	per     *workbench.PersonaSnapshot
	plan    *planReview
	agents  *modelCtxSnapshot
	notices []string
	scroll  int
}

type editorDone struct {
	kind string
	path string
	err  error
}

// ---------- snapshots ----------

func (m *Model) contextSnapshot() (workbench.ContextSnapshot, error) {
	path := m.app.ContextPath()
	doc, warns, err := m.app.LoadContext()
	if err != nil {
		exists := false
		if _, statErr := os.Stat(path); statErr == nil {
			exists = true
		}
		return workbench.ContextSnapshot{Path: path, Exists: exists, Warnings: warns}, err
	}
	return workbench.SnapshotContext(path, doc, warns,
		m.app.Cfg.Context.InjectBudgetTokens, m.app.Cfg.Context.AutoAcceptClasses), nil
}

func (m *Model) personaSnapshot() (workbench.PersonaSnapshot, error) {
	if m.app.Pack == nil {
		return workbench.PersonaSnapshot{Model: m.viewModel()}, fmt.Errorf("persona pack not loaded")
	}
	var sel *persona.Selection
	if m.comp != nil && m.comp.Persona != nil {
		sel = m.comp.Persona
	} else if m.res != nil {
		var err error
		sel, err = m.app.Pack.Select(m.personaOverride, m.res.Model.Family, m.res.Model.ID, m.res.Capabilities())
		if err != nil {
			return workbench.PersonaSnapshot{Model: m.viewModel(), PackDir: m.app.PersonaPackDir()}, err
		}
	}
	return workbench.SnapshotPersonas(m.app.Pack, sel, m.comp, m.viewModel()), nil
}

// ---------- open / refresh ----------

func (m *Model) openContextWorkbench() {
	snap, err := m.contextSnapshot()
	wb := &workbenchView{kind: "context", ctx: &snap}
	switch {
	case err != nil && !snap.Exists:
		wb.notices = append(wb.notices, "✸ CONTEXT.md is missing — run `kintsugi context init`, or create it with your editor")
	case err != nil:
		wb.notices = append(wb.notices, "✸ "+err.Error())
	default:
		if snap.Truncated {
			wb.notices = append(wb.notices, "▪ inject budget reached: sections below the cut are not sent")
		}
		if len(snap.Warnings) > 0 {
			wb.notices = append(wb.notices, "▪ "+strings.Join(snap.Warnings, " · "))
		}
	}
	m.wb = wb
}

func (m *Model) openPersonaWorkbench() {
	snap, err := m.personaSnapshot()
	wb := &workbenchView{kind: "persona", per: &snap}
	if err != nil {
		wb.notices = append(wb.notices, "✸ "+err.Error())
	}
	m.wb = wb
}

func (m *Model) refreshWorkbench() {
	if m.wb == nil {
		return
	}
	kind := m.wb.kind
	// A plan or the model-context list re-reads itself from disk. Neither is a
	// prompt layer, so reloading them has nothing to do with the compiled prompt.
	if kind == "agents" {
		m.openModelCtxWorkbench()
		return
	}
	if kind == "plan" {
		p := m.wb.plan
		if p == nil {
			return
		}
		art, err := plan.Load(m.app.Workspace, p.art.Name)
		if err != nil {
			m.addNote("✗ " + err.Error())
			return
		}
		rev, err := plan.LoadReview(m.app.Workspace, art.Name)
		if err != nil {
			m.addNote("✗ " + err.Error())
			return
		}
		cursor := p.cursor
		p.art, p.rev = art, rev
		p.cursor = clampInt(cursor, 0, p.lineCount()-1)
		return
	}
	if kind == "context" {
		old := m.wb.ctx
		res, err := m.recompile("workbench refresh")
		if err != nil {
			m.addNote("✗ " + err.Error())
		} else {
			m.reportRecompile("// prompt recompiled", "workbench refresh", res)
		}
		m.openContextWorkbench()
		if m.wb.ctx != nil && old != nil {
			m.wb.notices = append(workbench.DiffContext(*old, *m.wb.ctx), m.wb.notices...)
		}
		return
	}
	if err := m.app.ReloadPersona(); err != nil {
		m.addNote("✗ persona reload: " + err.Error())
	} else {
		res, err := m.recompile("persona reload")
		if err != nil {
			m.addNote("✗ " + err.Error())
		} else {
			m.reportRecompile("// prompt recompiled", "persona reload", res)
		}
	}
	m.openPersonaWorkbench()
}

// ---------- hot reload ----------

func (m *Model) currentStamps() map[string]fileStamp {
	out := map[string]fileStamp{}
	for _, p := range m.watchPaths() {
		out[p] = stampPath(p)
	}
	return out
}

func (m *Model) watchPaths() []string {
	if m.app == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(m.app.ContextPath())
	for _, sk := range m.app.Skills() {
		add(sk.Path)
	}
	if dir := m.app.PersonaPackDir(); dir != "" {
		ents, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range ents {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
					add(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func stampPath(path string) fileStamp {
	st, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{Exists: true, Mod: st.ModTime().UnixNano(), Size: st.Size()}
}

// stalePaths compares the watched files against the compile-time stamps.
func (m *Model) stalePaths() []string {
	if m.app == nil {
		return nil
	}
	cur := m.currentStamps()
	seen := map[string]bool{}
	var out []string
	for p, c := range cur {
		seen[p] = true
		if m.stamps[p] != c {
			out = append(out, p)
		}
	}
	for p := range m.stamps {
		if !seen[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// hotReload recompiles only when a watched file changed. Called before a turn
// so an external editor or curator pass is picked up without restarting.
func (m *Model) hotReload() {
	if m.busy {
		return
	}
	stale := m.stalePaths()
	if len(stale) == 0 {
		return
	}
	packChanged := false
	packDir := m.app.PersonaPackDir()
	for _, p := range stale {
		if packDir != "" && filepath.Dir(p) == packDir && strings.HasSuffix(p, ".md") {
			packChanged = true
			break
		}
	}
	if packChanged && m.app.Pack != nil {
		if err := m.app.ReloadPersona(); err != nil {
			m.addNote("✗ persona reload: " + err.Error())
			m.stamps = m.currentStamps()
			return
		}
	}
	res, err := m.recompile("files changed on disk")
	if err != nil {
		m.addNote("✗ recompile: " + err.Error())
		m.stamps = m.currentStamps()
		return
	}
	m.addCard("// hot reload · "+fileList(stale),
		strings.Join(res.diff, "\n")+"\n"+cacheLine(res), ui.Gold)
}

func fileList(paths []string) string {
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	return strings.Join(names, ", ")
}

// ---------- compile / report ----------

type recompileResult struct {
	diff     []string
	prefix   []string
	warnings []string
}

func (m *Model) compileCurrent() (*prompt.Compiled, error) {
	if m.app == nil || m.res == nil {
		return nil, fmt.Errorf("no active model")
	}
	opts := harness.CompileOpts{Persona: m.personaOverride}
	if m.role != "" && m.role != "default" && m.role != "custom" {
		return m.app.CompileRole(m.res, m.role, opts)
	}
	return m.app.Compile(m.res, opts)
}

func (m *Model) recompile(reason string) (*recompileResult, error) {
	comp, err := m.compileCurrent()
	if err != nil {
		return nil, err
	}
	old := m.comp
	m.comp = comp
	// The banner and the quickstart card render model/persona and file-backed
	// context live, not from a stored block, so the transcript memo must not
	// survive a recompile. This is the one choke point all three reload paths
	// (hot reload, CONTEXT.md saved, persona saved) pass through.
	m.invalidateTranscript()
	if m.loop != nil {
		m.loop.Reminder = m.app.ReminderBuilder(comp)
	}
	m.stamps = m.currentStamps()
	if m.app.Workspace != "" {
		if err := prompt.SaveManifest(m.app.Workspace, comp.Manifest); err != nil {
			m.addNote("· manifest not saved: " + err.Error())
		}
	}
	if m.log != nil {
		m.log.Log("meta", map[string]string{
			"event": "recompile", "reason": reason,
			"persona": comp.Manifest.Persona.Name, "stack": comp.Manifest.StackHash,
		})
	}
	res := &recompileResult{warnings: comp.Manifest.Warnings}
	if old != nil {
		res.diff = prompt.Diff(old.Manifest, comp.Manifest)
		res.prefix = prompt.PrefixChanged(old.Manifest, comp.Manifest)
	}
	return res, nil
}

func cacheLine(res *recompileResult) string {
	if res == nil {
		return ""
	}
	if len(res.prefix) > 0 {
		return "✸ cache prefix invalidated: " + strings.Join(res.prefix, ", ") + " · next request is cold"
	}
	if len(res.diff) > 0 {
		return "· cache prefix intact · task layers changed"
	}
	return "· no prompt changes"
}

func (m *Model) reportRecompile(title, reason string, res *recompileResult) {
	if res == nil {
		return
	}
	lines := append([]string{}, res.diff...)
	if len(res.warnings) > 0 {
		lines = append(lines, "warnings: "+strings.Join(res.warnings, " · "))
	}
	lines = append(lines, cacheLine(res))
	tone := ui.Gold
	if len(res.prefix) > 0 {
		tone = ui.Warn
	}
	m.addCard(title, reason+"\n"+strings.Join(lines, "\n"), tone)
}

// ---------- editing ----------

func editorCommand(path string) *exec.Cmd {
	ed := strings.TrimSpace(os.Getenv("VISUAL"))
	if ed == "" {
		ed = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if ed == "" {
		ed = "vi"
	}
	fields := strings.Fields(ed)
	if len(fields) == 0 {
		fields = []string{"vi"}
	}
	args := append(append([]string{}, fields[1:]...), path)
	return exec.Command(fields[0], args...)
}

func (m *Model) editFile(kind, path string) tea.Cmd {
	if path == "" {
		m.addNote("✗ no file to edit · press r to reload the pack, then e again")
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		m.addNote("✗ " + path + " does not exist · " + createHint(kind))
		return nil
	}
	m.addNote("· opening " + path + " in $EDITOR")
	return tea.ExecProcess(editorCommand(path), func(err error) tea.Msg {
		return editorDone{kind: kind, path: path, err: err}
	})
}

// createHint names the command that produces a missing file for each surface.
// "does not exist" without it is a dead end: the operator knows what broke and
// not what to type.
func createHint(kind string) string {
	switch kind {
	case "context":
		return "create it with `kintsugi context init`; the harness never drafts Authorization itself"
	case "persona":
		return "press r in /persona to reload the pack, or check persona_pack.dir"
	case "plan":
		return "open another with /plan <name>, or write one with `kintsugi plan \"<task>\"`"
	case "agents":
		return "press n in /agents to create one keyed to this model"
	case "refusal":
		return "press e in /refusal again — the file is created on first edit"
	}
	return "check the path, then retry"
}

func (m *Model) afterEditor(msg editorDone) {
	if msg.err != nil {
		m.addNote("✗ editor: " + msg.err.Error())
		return
	}
	if msg.kind == "context" {
		m.afterContextEdit()
		return
	}
	m.afterPersonaEdit()
}

func (m *Model) afterContextEdit() {
	var before *workbench.ContextSnapshot
	if m.wb != nil && m.wb.kind == "context" {
		if m.wb.ctx != nil {
			c := *m.wb.ctx
			before = &c
		}
	}
	snap, err := m.contextSnapshot()
	if err != nil {
		m.addNote("✗ context: " + err.Error())
		return
	}
	var lines []string
	if before != nil {
		lines = workbench.DiffContext(*before, snap)
	}
	if !snap.Exists {
		m.addNote("✗ CONTEXT.md disappeared · recreate it with `kintsugi context init`, then r to recompile")
		return
	}
	if len(lines) == 0 {
		lines = []string{"no section changes"}
	}
	gate := ""
	if len(snap.Warnings) > 0 {
		gate = "\n▪ " + strings.Join(snap.Warnings, " · ")
	}
	body := strings.Join(lines, "\n") + gate
	tone := ui.Gold
	res, rerr := m.recompile("CONTEXT.md saved")
	if rerr != nil {
		body += "\n✗ recompile: " + rerr.Error()
	} else {
		body += "\n" + cacheLine(res)
		if len(res.warnings) > 0 {
			body += "\nwarnings: " + strings.Join(res.warnings, " · ")
		}
		if len(res.prefix) > 0 {
			tone = ui.Warn
		}
	}
	m.addCard("// context saved · recompiled", body, tone)
	m.addNote("· Authorization & scope remains operator-authored; the harness did not edit it")
	if m.wb != nil && m.wb.kind == "context" {
		m.wb.ctx = &snap
		m.wb.notices = append(lines, m.wb.notices...)
		m.wb.scroll = 0
	}
}

func (m *Model) afterPersonaEdit() {
	if err := m.app.ReloadPersona(); err != nil {
		m.addNote("✗ persona not reloaded: " + err.Error())
		m.addNote("· the previous pack remains active; fix the file and press r in /persona")
		return
	}
	res, err := m.recompile("persona saved")
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	tone := ui.Gold
	if len(res.prefix) > 0 {
		tone = ui.Warn
	}
	m.addCard("// persona saved · recompiled", strings.Join(res.diff, "\n")+"\n"+cacheLine(res), tone)
	snap, serr := m.personaSnapshot()
	if m.wb != nil && m.wb.kind == "persona" && serr == nil {
		m.wb.per = &snap
		m.wb.scroll = 0
	}
}

// ---------- rendering ----------

func (m *Model) workbenchTitle() string {
	if m.wb == nil {
		return "// workbench"
	}
	if m.wb.kind == "context" {
		return "// 04 context · layer 5 · CONTEXT.md"
	}
	if m.wb.kind == "plan" {
		name := "plan"
		round := 0
		if m.wb.plan != nil {
			name = m.wb.plan.art.Name
			round = m.wb.plan.roundNumber()
		}
		if round > 0 {
			return fmt.Sprintf("// 04 plan · %s · round %d", name, round)
		}
		return "// 04 plan · " + name + " · unreviewed"
	}
	if m.wb.kind == "agents" {
		model := m.viewModel()
		if sel := m.wb.agents; sel != nil && sel.selected.Doc != nil {
			return "// 04 model context · " + model + " · " + sel.selected.Doc.Name
		}
		return "// 04 model context · " + model
	}
	name := "persona"
	if m.wb.per != nil && m.wb.per.Selected != "" {
		name = m.wb.per.Selected
	}
	return "// 04 persona · layer 2 · " + name
}

func (m *Model) workbenchFooter() string {
	keys := "[esc] close  [↑/↓] scroll"
	if m.wb != nil {
		switch m.wb.kind {
		case "context":
			keys += "  [e] edit  [r] recompile  [d] proposals"
		case "persona":
			keys += "  [e] edit persona"
			if m.wb.per != nil && m.wb.per.BasePath != "" && m.wb.per.BasePath != m.wb.per.SelectedPath {
				keys += "  [b] edit base"
			}
			keys += "  [r] reload pack"
		case "plan":
			if m.wb.plan != nil && m.wb.plan.compose {
				keys = "[enter] save comment  [esc] cancel  [!] risky  [?] why  [-] cut"
				break
			}
			keys += "  [^n/^p] select  [c] comment  [a] approve  [x] reject  [r] review  [d] diff  [y] copy"
		case "agents":
			keys += "  [^n/^p] select  [e] edit  [n] new  [u] use this  [r] back to rules"
		}
	}
	lines := []string{m.theme.Paint(ui.PaperSoft, keys)}
	// The second footer row is the staleness warning for the prompt layers. It
	// is meaningless on a plan, which is not a layer — and worse, it would tell
	// the operator to press r, which on this surface starts a review round.
	if m.wb != nil && m.wb.kind == "plan" {
		lines = append(lines, m.theme.Paint(ui.PaperSoft, "· a round snapshots the plan; comments stay anchored to the line they were written on"))
	} else if m.wb != nil && m.wb.kind == "agents" {
		// r means "back to rule matching" on this surface, so the layer-staleness
		// hint would be pointing at the wrong key.
		lines = append(lines, m.theme.Paint(ui.PaperSoft, "· edit a document and the next send recompiles the prompt"))
	} else if stale := m.stalePaths(); len(stale) > 0 {
		lines = append(lines, m.theme.Paint(ui.Warn, "✸ changed on disk: "+fileList(stale)+" · r recompiles"))
	} else {
		lines = append(lines, m.theme.Paint(ui.PaperSoft, "· layers 0-2 byte-stable until a persona/adapter change"))
	}
	stamp := "kintsugi · " + version + " · session " + m.log.ID
	lines = append(lines, m.theme.Paint(ui.PaperSoft, stamp))
	// The footer sits outside the panel, so nothing else clips it. A long key
	// list or a long session stamp would otherwise push the whole frame past the
	// terminal width — which fitScreen does not catch, because it only clamps
	// height.
	for i, l := range lines {
		lines[i] = clipCells(l, m.w)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) workbenchBody(width, height int) string {
	var lines []string
	if m.wb == nil {
		return ""
	}
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
	maxScroll := len(lines) - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.wb.scroll > maxScroll {
		m.wb.scroll = maxScroll
	}
	if m.wb.scroll < 0 {
		m.wb.scroll = 0
	}
	end := m.wb.scroll + height
	if end > len(lines) {
		end = len(lines)
	}
	visible := lines[m.wb.scroll:end]
	for len(visible) < height {
		visible = append(visible, "")
	}
	return strings.Join(visible, "\n")
}

func (m *Model) contextLines(width int) []string {
	s := m.wb.ctx
	if s == nil {
		return nil
	}
	var out []string
	out = append(out, m.theme.Paint(ui.PaperSoft, "path      ")+truncate(s.Path, max(width-10, 8)))
	if !s.Exists {
		out = append(out, m.theme.Paint(ui.Danger, "status    missing"))
		return out
	}
	out = append(out, m.theme.Paint(ui.PaperSoft, "project   ")+orDash(s.Project))
	budget := "none"
	if s.Budget > 0 {
		budget = fmt.Sprintf("%d tok", s.Budget)
	}
	out = append(out, m.theme.Paint(ui.PaperSoft, "document  ")+fmt.Sprintf("%s · %d tok · budget %s", shortHash(s.Hash), s.Tokens, budget))
	status := "✓ every parsed section is injected at layer 5"
	if s.Truncated {
		status = "✸ truncated at inject budget · raise [context] inject_budget_tokens to send the rest"
	}
	out = append(out, m.theme.Paint(ui.PaperSoft, "inject    ")+status)
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft, fmt.Sprintf("SECTION               CLASS          TOK  GATE    STATE")))
	for _, sec := range s.Sections {
		gateTone := ui.PaperDim
		switch sec.Gate {
		case "gated":
			gateTone = ui.Warn
		case "auto":
			gateTone = ui.OK
		}
		state := m.theme.Paint(ui.OK, "✓ injected")
		if !sec.Injected {
			state = m.theme.Paint(ui.Danger, "— dropped")
		}
		classLabel := sec.Class
		if sec.Index > 0 {
			classLabel = fmt.Sprintf("%s[%d]", sec.Class, sec.Index+1)
		}
		line := fmt.Sprintf("%-20s %-13s %4d  ", truncate(sec.Title, 20), classLabel, sec.Tokens) +
			m.theme.Paint(gateTone, fmt.Sprintf("%-6s", sec.Gate)) + "  " + state
		out = append(out, line)
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft, "gated = operator-only (authorization/scope) · review = needs --all · auto = curator may apply"))
	return out
}

func (m *Model) personaLines(width int) []string {
	s := m.wb.per
	if s == nil {
		return nil
	}
	var out []string
	out = append(out, m.theme.Paint(ui.PaperSoft, "pack      ")+truncate(orDash(s.PackDir), max(width-10, 8)))
	out = append(out, m.theme.Paint(ui.PaperSoft, "model     ")+orDash(s.Model))
	out = append(out, m.theme.Paint(ui.PaperSoft, "match     ")+orDash(s.Rule)+" · "+s.Summary())
	tokenState := fmt.Sprintf("%d tok", s.Tokens)
	if s.Dropped {
		tokenState += " (dropped by budget)"
	}
	out = append(out, m.theme.Paint(ui.PaperSoft, "layer 2   ")+tokenState)
	out = append(out, m.theme.Paint(ui.PaperSoft, "selected  ")+truncate(orDash(s.SelectedPath), max(width-10, 8)))
	if s.BasePath != "" && s.BasePath != s.SelectedPath {
		out = append(out, m.theme.Paint(ui.PaperSoft, "base      ")+truncate(s.BasePath, max(width-10, 8)))
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft, "PACK"))
	for _, card := range s.Available {
		mark := "·"
		tone := ui.PaperDim
		if card.Effective {
			mark = "▸"
			tone = ui.Gold
		}
		if card.Matched {
			mark = "★"
		}
		desc := truncate(oneLine(card.Description), max(width-32, 12))
		row := fmt.Sprintf("%s %-22s %2d trig  %s", mark, truncate(card.Name, 22), card.Triggers, desc)
		out = append(out, m.theme.Paint(tone, row))
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft, fmt.Sprintf("TRIGGERS (%d)", len(s.Triggers))))
	if len(s.Triggers) == 0 {
		out = append(out, m.theme.Paint(ui.PaperSoft, "· none yet — add entries from refusal telemetry"))
	}
	for _, t := range s.Triggers {
		row := fmt.Sprintf("  %-18s %-16s %s → %s", truncate(t.Persona, 18), truncate(orDash(t.Class), 16), t.Term, t.Instead)
		out = append(out, truncate(row, max(width, 16)))
	}
	out = append(out, "")
	out = append(out, m.theme.Paint(ui.PaperSoft, fmt.Sprintf("LINT (%d)", len(s.Lint))))
	if len(s.Lint) == 0 {
		out = append(out, m.theme.Paint(ui.OK, "✓ no findings for the effective stack"))
	}
	for _, l := range s.Lint {
		tone := ui.Warn
		if l.Level == "error" {
			tone = ui.Danger
		}
		out = append(out, m.theme.Paint(tone, fmt.Sprintf("  %-7s %s: %s", l.Level, l.Persona, l.Message)))
	}
	return out
}

// ---------- slash commands ----------

func (m *Model) handleContextCommand(rest string) tea.Cmd {
	fields := strings.Fields(rest)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch sub {
	case "":
		m.openContextWorkbench()
	case "edit":
		m.openContextWorkbench()
		return m.editFile("context", m.app.ContextPath())
	case "refresh":
		res, err := m.recompile("context refresh")
		if err != nil {
			m.addNote("✗ " + err.Error())
			return nil
		}
		m.reportRecompile("// context refreshed", "CONTEXT.md re-read from disk", res)
		if m.wb != nil && m.wb.kind == "context" {
			m.openContextWorkbench()
		}
	case "diff":
		m.contextProposalDiff()
	case "init":
		m.addNote("· run `kintsugi context init`; the harness does not draft Authorization itself")
	default:
		m.addNote("usage: /context [edit|diff|refresh]")
	}
	return nil
}

func (m *Model) handlePersonaCommand(rest string) tea.Cmd {
	fields := strings.Fields(rest)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch sub {
	case "", "list":
		m.openPersonaWorkbench()
	case "edit":
		m.openPersonaWorkbench()
		if m.wb != nil && m.wb.per != nil {
			return m.editFile("persona", m.wb.per.SelectedPath)
		}
	case "base":
		m.openPersonaWorkbench()
		if m.wb != nil && m.wb.per != nil && m.wb.per.BasePath != "" {
			return m.editFile("persona", m.wb.per.BasePath)
		}
		m.addNote("✗ no base persona in this pack · edit the selected one with /persona edit")
	case "refresh":
		if err := m.app.ReloadPersona(); err != nil {
			m.addNote("✗ persona reload: " + err.Error())
			return nil
		}
		res, err := m.recompile("persona refresh")
		if err != nil {
			m.addNote("✗ " + err.Error())
			return nil
		}
		m.reportRecompile("// persona refreshed", "pack re-read from disk", res)
		if m.wb != nil && m.wb.kind == "persona" {
			m.openPersonaWorkbench()
		}
	case "use":
		if len(fields) < 2 {
			m.addNote("usage: /persona use <name>|auto")
			return nil
		}
		m.usePersona(fields[1])
	case "auto":
		m.usePersona("")
	default:
		m.addNote("usage: /persona [edit|base|refresh|use <name>|auto]")
	}
	return nil
}

func (m *Model) usePersona(name string) {
	if name != "" {
		if m.app.Pack == nil {
			m.addNote("✗ persona pack not loaded · run `kintsugi setup`, then /persona refresh")
			return
		}
		if _, ok := m.app.Pack.Get(name); !ok {
			m.addNote(fmt.Sprintf("✗ persona %q not in %s · /persona lists what is there",
				name, m.app.PersonaPackDir()))
			return
		}
	}
	prev := m.personaOverride
	m.personaOverride = name
	res, err := m.recompile("persona override")
	if err != nil {
		m.personaOverride = prev
		m.addNote("✗ " + err.Error())
		return
	}
	label := "auto matching restored"
	if name != "" {
		label = "persona forced to " + name
	}
	m.reportRecompile("// persona selected", label, res)
	if m.wb != nil && m.wb.kind == "persona" {
		m.openPersonaWorkbench()
	}
}

// ---------- input ----------

func (m *Model) handleWorkbenchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The comment composer owns the keyboard while it is open, the way the
	// connect panel does: every printable key is comment text, not a command.
	if p := m.planReview(); p != nil && p.compose {
		return m.handleComposerKey(msg)
	}
	// The model-context surface owns its own keys and falls through for the
	// shared ones (esc, scroll), so it behaves like every other workbench kind.
	if m.wb != nil && m.wb.kind == "agents" {
		if mm, cmd, ok := m.handleModelCtxKey(msg); ok {
			return mm, cmd
		}
	}
	switch strings.ToLower(msg.String()) {
	case "esc", "q", "ctrl+c":
		m.wb = nil
		return m, nil
	case "up", "k":
		if m.wb != nil && m.wb.scroll > 0 {
			m.wb.scroll--
		}
		return m, nil
	case "down", "j":
		if m.wb != nil {
			m.wb.scroll++
		}
		return m, nil
	case "pgup":
		if m.wb != nil {
			m.wb.scroll -= 8
		}
		return m, nil
	case "pgdown":
		if m.wb != nil {
			m.wb.scroll += 8
		}
		return m, nil
	case "e":
		if m.planReview() != nil {
			return m, m.editFile("plan", m.wb.plan.art.Path)
		}
		kind, path := "context", m.app.ContextPath()
		if m.wb != nil && m.wb.kind == "persona" && m.wb.per != nil {
			kind, path = "persona", m.wb.per.SelectedPath
		}
		return m, m.editFile(kind, path)
	case "b":
		if m.wb != nil && m.wb.kind == "persona" && m.wb.per != nil {
			return m, m.editFile("persona", m.wb.per.BasePath)
		}
	case "r":
		// On a plan, r is the review round: it is the verb the surface exists
		// for, and reloading is the fallback, not the point.
		if m.planReview() != nil {
			return m, m.runPlanReview()
		}
		m.refreshWorkbench()
		return m, nil
	case "d":
		if p := m.planReview(); p != nil {
			if p.diffFrom > 0 {
				p.diffFrom = 0
				return m, nil
			}
			if p.roundNumber() > 1 {
				p.diffFrom = p.roundNumber() - 1
			} else {
				m.addNote("· no earlier round to diff against")
			}
			return m, nil
		}
		if m.wb != nil && m.wb.kind == "context" {
			m.contextProposalDiff()
		}
		return m, nil
	// ctrl+n/ctrl+p move the selection rather than j/k, which stay bound to
	// scrolling: a plan is a document to read, not a list to walk.
	case "ctrl+n":
		if m.planReview() != nil {
			m.reviewMoveCursor(1)
		}
		return m, nil
	case "ctrl+p":
		if m.planReview() != nil {
			m.reviewMoveCursor(-1)
		}
		return m, nil
	case "c", "enter":
		if p := m.planReview(); p != nil {
			p.compose, p.draft = true, ""
			m.reviewRevealCursor()
		}
		return m, nil
	case "a":
		if m.planReview() != nil {
			return m, m.setPlanVerdict(plan.VerdictApprove)
		}
		return m, nil
	case "x":
		if m.planReview() != nil {
			return m, m.setPlanVerdict(plan.VerdictReject)
		}
		return m, nil
	case "y":
		// Copy the plan. A plan is the thing an operator pastes into a ticket,
		// and OSC 52 is the only copy that works over SSH where this runs.
		if m.planReview() != nil {
			m.copyPlan()
		}
		return m, nil
	}
	return m, nil
}

// handleComposerKey is the comment composer's key path. It accepts text and the
// three quick-comment prefixes, and nothing it sees can reach the transcript
// input line underneath.
func (m *Model) handleComposerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.planReview()
	if p == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		p.compose, p.draft = false, ""
		return m, nil
	case tea.KeyEnter:
		m.saveDraftComment()
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(p.draft); len(r) > 0 {
			p.draft = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeySpace:
		p.draft += " "
		return m, nil
	case tea.KeyRunes:
		p.draft += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (m *Model) contextProposalDiff() {
	path, err := fctx.LatestProposal(m.app.Workspace)
	if err != nil {
		m.addNote("· no pending context proposals")
		return
	}
	pf, err := fctx.LoadProposalFile(path)
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	doc, _, err := m.app.LoadContext()
	if err != nil {
		m.addNote("✗ " + err.Error())
		return
	}
	mode := fctx.ApplyMode{AutoClasses: m.app.Cfg.Context.AutoAcceptClasses}
	m.addCard("// context proposals", "file "+path+"\n\n"+fctx.Diff(doc, pf.Proposals, mode), ui.Gold)
}
