package tui

// The model picker (`/model` with no argument).
//
// A typed `provider/model` string is muscle memory for nobody on their first
// day: the picker lists every configured provider and model with its key
// status, filters as you type, and switches on enter. The filter field
// doubles as the typed path — a filter that matches nothing is resolved as a
// raw ref on enter, so the operator who knows the exact id never fights the
// list.
//
// Rows carry the key status (env / stored / missing / local), so the picker
// answers "what can I actually use right now", not just "what exists". The
// session's current model is always listed, marked, and preselected — even
// when it is not in the static registry (a local runner's models are not).

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/catalog"
	"kintsugi/internal/config"
	"kintsugi/internal/credentials"
	"kintsugi/internal/harness"
	"kintsugi/internal/prompt"
	"kintsugi/internal/session"
	"kintsugi/internal/ui"
)

// modelEntry is one row of the picker: a resolvable provider/model ref plus
// the status that decides whether it can be used right now.
type modelEntry struct {
	Ref     string // provider/model
	Family  string
	Status  string // env | stored | missing | local
	Current bool
	// RefusalRate is the model's refusal percentage from the session
	// telemetry, when it has any turns. HasRate separates "0%" from "no
	// data" — the picker never invents a number.
	RefusalRate float64
	HasRate     bool
}

// modelPickerView is the whole modal: the filter, the list, the selection.
type modelPickerView struct {
	entries      []modelEntry
	filter       string
	sel, scroll  int
	note         string
	w, h         int
	fetching     map[string]bool // providers with a live /models fetch in flight
	fetchedTotal int             // completed fetches, for the status line
	centered     bool            // the window has been centred on the current model once
}

// modelPickerStatus is a model row's key column: same rule as the connect
// panel's, with "local" for loopback providers that need no key.
func modelPickerStatus(name string, p config.Provider) string {
	if strings.TrimSpace(p.APIKeyEnv) == "" || p.Offline {
		return "local"
	}
	switch credentials.Status(name, p.APIKeyEnv) {
	case "env":
		return "env"
	case "stored":
		return "stored"
	default:
		return "missing"
	}
}

// newModelPicker builds the entry list: every configured provider's curated
// models, plus the session's current model pinned to the top of its
// provider's group so it is always visible and selectable.
func newModelPicker(app *harness.App, current string) *modelPickerView {
	v := &modelPickerView{}
	if app == nil {
		return v
	}
	seen := map[string]bool{}
	add := func(ref, family, status string, isCurrent bool) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		v.entries = append(v.entries, modelEntry{Ref: ref, Family: family, Status: status, Current: isCurrent})
	}

	names := make([]string, 0, len(app.Cfg.Providers))
	for name := range app.Cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := app.Cfg.Providers[name]
		status := modelPickerStatus(name, p)
		models := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			if id := strings.TrimSpace(m.ID); id != "" {
				models = append(models, id)
			}
		}
		sort.Strings(models)
		for _, id := range models {
			add(name+"/"+id, p.Family, status, name+"/"+id == current)
		}
		if len(models) == 0 {
			// A passthrough provider (no curated list) still appears with a
			// "any id" row so it is discoverable; its entry resolves only when
			// the filter text names a real model id.
			if _, ok := app.Cfg.Providers[strings.Split(current, "/")[0]]; ok || !strings.Contains(current, "/") {
				continue
			}
		}
	}
	// The current model is always present, even off-registry (local runners).
	if current != "" && !seen[current] {
		prov, _, ok := strings.Cut(current, "/")
		fam, status := "", "local"
		if ok {
			if p, pok := app.Cfg.Providers[prov]; pok {
				fam, status = p.Family, modelPickerStatus(prov, p)
			}
		}
		add(current, fam, status, true)
	}
	// One alphabetical list: the picker opens centred on the current model,
	// so its provider's neighbours are the first thing the operator sees.
	sort.Slice(v.entries, func(i, j int) bool { return v.entries[i].Ref < v.entries[j].Ref })
	for i, e := range v.entries {
		if e.Current {
			v.sel = i
			break
		}
	}
	v.fetching = map[string]bool{}
	// The reliability column: one local aggregation, no network.
	if dir, err := session.Dir(); err == nil {
		if rows, err := session.ModelReliabilityStats(dir); err == nil {
			rate := map[string]float64{}
			for _, r := range rows {
				rate[r.Model] = r.RefusalRate
			}
			for i := range v.entries {
				if r, ok := rate[v.entries[i].Ref]; ok {
					v.entries[i].RefusalRate = r
					v.entries[i].HasRate = true
				}
			}
		}
	}
	return v
}

// mergeFetched folds one provider's live model list into the entries: new
// refs are added (the registry list is often stale — a token plan serves the
// whole current model line, not the three ids a curated list froze), the list
// is re-sorted, and the selection follows the same entry so the arriving rows
// cannot yank the operator's place in the list.
func (v *modelPickerView) mergeFetched(provider string, models []string, status, family string) {
	if v == nil {
		return
	}
	// The selected ref is captured before the append and resort: after them,
	// the old index points at a different entry in the new order.
	cur := ""
	if v.sel >= 0 && v.sel < len(v.entries) {
		cur = v.entries[v.sel].Ref
	}
	have := map[string]bool{}
	for _, e := range v.entries {
		have[e.Ref] = true
	}
	for _, id := range models {
		ref := provider + "/" + id
		if have[ref] {
			continue
		}
		have[ref] = true
		fam := family
		if fam == "" {
			fam = inferPickerFamily(id)
		}
		v.entries = append(v.entries, modelEntry{Ref: ref, Family: fam, Status: status})
	}
	sort.Slice(v.entries, func(i, j int) bool { return v.entries[i].Ref < v.entries[j].Ref })
	// Keep the selection on the same entry across the resort.
	for i, e := range v.entries {
		if e.Ref == cur {
			v.sel = i
			break
		}
	}
	v.clampSel(len(v.filtered()))
}

// inferPickerFamily guesses a family for a fetched model id, the same way the
// config resolver does: the id usually names its family.
func inferPickerFamily(id string) string {
	low := strings.ToLower(id)
	for _, fam := range []string{"deepseek", "qwen", "glm", "kimi", "minimax", "mimo", "llama", "mistral"} {
		if strings.Contains(low, fam) {
			return fam
		}
	}
	return ""
}

// filtered is the entry list narrowed by the filter: a case-insensitive
// substring match on provider/model and family. An empty filter shows all.
func (v *modelPickerView) filtered() []modelEntry {
	f := strings.ToLower(strings.TrimSpace(v.filter))
	if f == "" {
		return v.entries
	}
	var out []modelEntry
	for _, e := range v.entries {
		if strings.Contains(strings.ToLower(e.Ref), f) || strings.Contains(strings.ToLower(e.Family), f) {
			out = append(out, e)
		}
	}
	return out
}

// rows is how many list rows the panel can show, shared by the key handler
// and the renderer so they cannot disagree.
func modelPickerRows(screenH int) int {
	return max(screenH-1-2-3-2, 1) // header(1) + panel title/frame(3) + filter row + column header
}

func (m *Model) modelPickerRows() int { return modelPickerRows(max(m.h, 8)) }

// clampSel keeps the selection inside the filtered list.
func (v *modelPickerView) clampSel(n int) {
	if n == 0 {
		v.sel, v.scroll = 0, 0
		return
	}
	v.sel = clampInt(v.sel, 0, n-1)
}

// moveSel moves the selection by delta and keeps it inside the window.
func (v *modelPickerView) moveSel(delta, rows, n int) {
	if n == 0 {
		v.sel, v.scroll = 0, 0
		return
	}
	v.sel = clampInt(v.sel+delta, 0, n-1)
	if v.sel < v.scroll {
		v.scroll = v.sel
	}
	if v.sel >= v.scroll+rows {
		v.scroll = v.sel - rows + 1
	}
	if v.scroll > max(n-rows, 0) {
		v.scroll = max(n-rows, 0)
	}
	if v.scroll < 0 {
		v.scroll = 0
	}
}

// ---------- input ----------

// handleModelPickerKey owns the keyboard while the picker is open. Typing
// filters, arrows move, enter switches (or resolves the filter as a raw ref),
// esc closes.
func (m *Model) handleModelPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v := m.picker
	if v == nil {
		return m, nil
	}
	if msg.Type == tea.KeyCtrlC {
		m.picker = nil
		return m, tea.Quit
	}
	rows := m.modelPickerRows()
	list := v.filtered()
	n := len(list)

	switch msg.Type {
	case tea.KeyEsc:
		m.picker = nil
	case tea.KeyUp:
		v.moveSel(-1, rows, n)
	case tea.KeyDown:
		v.moveSel(1, rows, n)
	case tea.KeyPgUp:
		v.scroll = clampInt(v.scroll-rows, 0, max(n-rows, 0))
		if n > 0 {
			if v.sel >= v.scroll+rows {
				v.sel = v.scroll + rows - 1
			}
			if v.sel < v.scroll {
				v.sel = v.scroll
			}
		}
	case tea.KeyPgDown:
		v.scroll = clampInt(v.scroll+rows, 0, max(n-rows, 0))
		if n > 0 {
			if v.sel < v.scroll {
				v.sel = v.scroll
			}
			if v.sel >= v.scroll+rows {
				v.sel = v.scroll + rows - 1
			}
		}
	case tea.KeyBackspace:
		if len(v.filter) > 0 {
			r := []rune(v.filter)
			v.filter = string(r[:len(r)-1])
		}
		v.clampSel(len(v.filtered()))
	case tea.KeyCtrlU:
		v.filter = ""
	case tea.KeyEnter:
		return m.modelPickerSubmit(list)
	case tea.KeyRunes:
		v.filter += string(msg.Runes)
		v.clampSel(len(v.filtered()))
	case tea.KeySpace:
		v.filter += " "
		v.clampSel(len(v.filtered()))
	}
	return m, nil
}

// modelPickerSubmit is enter: switch to the selected entry, or — when the
// filter matches nothing — try the filter itself as a ref, which keeps the
// typed path alive inside the picker.
func (m *Model) modelPickerSubmit(list []modelEntry) (tea.Model, tea.Cmd) {
	v := m.picker
	if v == nil {
		return m, nil
	}
	ref := ""
	if v.sel >= 0 && v.sel < len(list) {
		ref = list[v.sel].Ref
	} else if f := strings.TrimSpace(v.filter); f != "" {
		ref = f
	}
	if ref == "" {
		return m, nil
	}
	if m.busy {
		v.note = "wait for the current turn to finish — the model switches between turns"
		return m, nil
	}
	if err := m.applyModelSwitch(ref); err != nil {
		// Stay open with the error: a bad ref should not close the picker and
		// dump the operator back into the transcript to retype everything.
		v.note = "✗ " + oneLine(err.Error())
		return m, nil
	}
	m.picker = nil
	return m, nil
}

// handleModelPickerMouse implements wheel scrolling over the list.
func (m *Model) handleModelPickerMouse(msg tea.MouseMsg) {
	v := m.picker
	if v == nil || msg.Action != tea.MouseActionPress {
		return
	}
	rows := m.modelPickerRows()
	n := len(v.filtered())
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scroll = clampInt(v.scroll-3, 0, max(n-rows, 0))
	case tea.MouseButtonWheelDown:
		v.scroll = clampInt(v.scroll+3, 0, max(n-rows, 0))
	}
	if n > 0 {
		if v.sel < v.scroll {
			v.sel = v.scroll
		}
		if v.sel >= v.scroll+rows {
			v.sel = minInt(v.scroll+rows-1, n-1)
		}
	}
}

// ---------- rendering ----------

// renderModelPicker draws the whole surface: the panel plus its footer rows.
func (v *modelPickerView) renderModelPicker(width, height int, theme ui.Theme) string {
	if width < 24 {
		width = 24
	}
	if height < 8 {
		height = 8
	}
	v.w, v.h = width, height

	footer := v.footerLines(width, theme)
	panelH := height - len(footer)
	if panelH < 5 {
		panelH = 5
	}
	bodyH := panelH - 3 // title row + two frame rows
	title, body := v.panelBody(max(width-2, 8), bodyH, theme)

	lines := strings.Split(theme.Panel(title, body, width, panelH), "\n")
	lines = append(lines, footer...)
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// footerLines is the key legend plus the list state: the count, the live
// fetches in flight, or the note.
func (v *modelPickerView) footerLines(width int, theme ui.Theme) []string {
	keys := "[enter] switch · [↑/↓] move · type to filter · [esc] close"
	hint := fmt.Sprintf("%d model(s)", len(v.filtered()))
	if n := len(v.fetching); n > 0 {
		hint = fmt.Sprintf("%d model(s) · fetching live models from %d provider(s)…", len(v.filtered()), n)
	}
	if v.note != "" {
		hint = v.note
	}
	return []string{
		theme.Paint(ui.PaperSoft, clipShorten(keys, width)),
		theme.Paint(ui.PaperSoft, clipShorten(hint, width)),
	}
}

// panelBody is the filter field, the column header, and the list.
func (v *modelPickerView) panelBody(width, height int, theme ui.Theme) (string, string) {
	var lines []string
	// The filter field: the typed text with a caret, gold when active.
	field := max(width-8, 4)
	fl := clipShorten(v.filter, field)
	caret := theme.Paint(ui.Gold, "▌")
	lines = append(lines, theme.Paint(ui.PaperSoft, "filter  ")+theme.Paint(ui.Paper, fl)+caret)

	if v.note != "" {
		lines = append(lines, theme.Paint(ui.Warn, clipShorten(v.note, width)))
	}
	list := v.filtered()
	n := len(list)
	rows := max(height-len(lines)-1, 1) // -1 for the column header
	v.clampSel(n)
	if !v.centered {
		// Open the window centred on the current model: the operator starts
		// oriented — their model in the middle of the screen, its neighbours
		// above and below — instead of at the alphabetical tail.
		v.centered = true
		v.scroll = max(v.sel-rows/2, 0)
		if lim := max(n-rows, 0); v.scroll > lim {
			v.scroll = lim
		}
	}
	v.scroll = clampInt(v.scroll, 0, max(n-rows, 0))
	if v.sel >= 0 {
		if v.sel < v.scroll {
			v.scroll = v.sel
		}
		if v.sel >= v.scroll+rows {
			v.scroll = v.sel - rows + 1
		}
	}
	lines = append(lines, theme.Paint(ui.PaperSoft, clipShorten("  model                          family    key", width)))
	if n == 0 {
		lines = append(lines, theme.Paint(ui.PaperSoft, "· no match — enter resolves the filter as a model ref"))
	}
	end := minInt(v.scroll+rows, n)
	for i := v.scroll; i < end; i++ {
		lines = append(lines, v.row(list[i], i == v.sel, width, theme))
	}
	return "// model · pick", padLines(lines, height)
}

// row renders one entry: the selection mark, the ref, family and key status.
// The current model carries a gold star beside its mark so it reads at a
// glance without hunting.
func (v *modelPickerView) row(e modelEntry, selected bool, width int, theme ui.Theme) string {
	mark, tone, cur := " ", ui.PaperDim, ""
	if e.Current {
		cur = theme.Paint(ui.Hikari, " ✦")
	}
	if selected {
		mark, tone = "▸", ui.Gold
	}
	ref := clipShorten(e.Ref, 32)
	row := fmt.Sprintf("%s %s%s", mark, ref, cur)
	if fam := clipShorten(orDash(e.Family), 10); fam != "—" {
		row += "  " + fam
	} else {
		row += "  —"
	}
	row += "  " + e.Status
	if e.HasRate {
		row += fmt.Sprintf("  %.0f%% ref", e.RefusalRate)
	}
	return theme.Paint(tone, clipShorten(row, width))
}

// ---------- switching ----------

// openModelPicker opens the picker with the session's current model
// preselected, and kicks off live model fetches for every provider that can
// answer one: a key resolves (env or the 0600 store) or the endpoint is
// loopback. Fetched lists merge into the entries as they arrive, because a
// curated registry is always stale — a token plan serves the whole current
// model line, not the three ids the list froze.
//
// Egress follows the /connect precedent: opening the picker is an explicit
// operator action, so reaching a keyed provider's /models is sanctioned even
// while the session itself stays in offline mode. A provider with no key is
// never probed.
func (m *Model) openModelPicker() tea.Cmd {
	current := ""
	if m.res != nil {
		current = m.res.ProviderName + "/" + m.res.Model.ID
	}
	m.picker = newModelPicker(m.app, current)
	if m.app == nil {
		return nil
	}
	var cmds []tea.Cmd
	names := make([]string, 0, len(m.app.Cfg.Providers))
	for name := range m.app.Cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := m.app.Cfg.Providers[name]
		if !m.providerCanListModels(name, p) {
			continue
		}
		m.picker.fetching[name] = true
		cmds = append(cmds, m.fetchModelListCmd(name, p))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// providerCanListModels reports whether a live fetch is worth starting: a key
// resolves (env, then the store), or the provider is loopback and needs none.
func (m *Model) providerCanListModels(name string, p config.Provider) bool {
	if strings.TrimSpace(p.APIKeyEnv) == "" {
		return p.Offline || config.IsLoopback(p.BaseURL)
	}
	switch credentials.Status(name, p.APIKeyEnv) {
	case "env", "stored":
		return true
	}
	return false
}

// modelsFetched is one live fetch result. Errors are counters, not cards: a
// provider that cannot be listed is simply absent from the merged list.
type modelsFetched struct {
	provider string
	models   []string
	err      error
}

// fetchModelListCmd fetches one provider's live model list off the Update
// goroutine. The key resolves env-first, then the 0600 store — the same
// precedence everything uses — and never reaches the transcript.
func (m *Model) fetchModelListCmd(name string, p config.Provider) tea.Cmd {
	return func() tea.Msg {
		key := ""
		if p.APIKeyEnv != "" {
			key = strings.TrimSpace(os.Getenv(p.APIKeyEnv))
			if key == "" {
				if s, err := credentials.Load(); err == nil {
					if k, ok := s.Get(name); ok {
						key = strings.TrimSpace(k)
					}
				}
			}
		}
		models, err := catalog.FetchWithKey(context.Background(), p, key, 15*time.Second)
		if err != nil {
			return modelsFetched{provider: name, err: err}
		}
		ids := make([]string, 0, len(models))
		for _, mm := range models {
			if id := strings.TrimSpace(mm.ID); id != "" {
				ids = append(ids, id)
			}
		}
		return modelsFetched{provider: name, models: ids}
	}
}

// handleModelsFetched folds a live list into the picker and into the session's
// provider config, so the fetched ids are resolvable from both. A result for a
// picker that has closed is dropped.
func (m *Model) handleModelsFetched(msg modelsFetched) (tea.Model, tea.Cmd) {
	v := m.picker
	if v == nil {
		return m, nil
	}
	delete(v.fetching, msg.provider)
	v.fetchedTotal++
	if msg.err != nil {
		// A failed fetch is quiet: the registry entries for that provider are
		// still there, and the footer's count says how many lists landed.
		m.persistCatalogWhenBatchDone()
		return m, nil
	}
	family, status := "", "missing"
	if p, ok := m.app.Cfg.Providers[msg.provider]; ok {
		family = p.Family
		status = modelPickerStatus(msg.provider, p)
		// Merge into the live config so Resolve accepts the fetched ids for
		// both the picker and the typed /model form, this session.
		have := map[string]bool{}
		for _, mm := range p.Models {
			have[mm.ID] = true
		}
		for _, id := range msg.models {
			if have[id] {
				continue
			}
			fam := family
			if fam == "" {
				fam = inferPickerFamily(id)
			}
			p.Models = append(p.Models, config.Model{ID: id, Family: fam})
			have[id] = true
		}
		m.app.Cfg.Providers[msg.provider] = p
	}
	v.mergeFetched(msg.provider, msg.models, status, family)
	m.persistCatalogWhenBatchDone()
	return m, nil
}

// persistCatalogWhenBatchDone writes the session's learned models into the
// cached catalog once, when a fetch batch completes: the next session's
// static list is fresh before any fetch runs. Loopback providers are
// skipped — an ollama model set is per-machine and changes with pulls.
// Best-effort: a write failure is a transcript note, never an error.
func (m *Model) persistCatalogWhenBatchDone() {
	v := m.picker
	if v == nil || len(v.fetching) > 0 || v.fetchedTotal == 0 || m.app == nil {
		return // a batch is still in flight, or nothing was fetched
	}
	cat := &catalog.Catalog{Version: catalog.Version, Providers: map[string]catalog.Provider{}}
	for name, p := range m.app.Cfg.Providers {
		if p.Offline || config.IsLoopback(p.BaseURL) {
			continue // per-machine lists are not worth caching
		}
		if len(p.Models) == 0 {
			continue
		}
		models := make([]catalog.Model, 0, len(p.Models))
		for _, mm := range p.Models {
			models = append(models, catalog.Model{ID: mm.ID, Family: mm.Family})
		}
		cat.Providers[name] = catalog.Provider{Models: models}
	}
	if len(cat.Providers) == 0 {
		return
	}
	path := m.app.Cfg.Core.CatalogPath
	if strings.TrimSpace(path) == "" {
		path = config.ExpandHome("~/.config/kintsugi/catalog.json")
	}
	if err := catalog.Save(path, cat); err != nil {
		m.addNote("· catalog not saved: " + oneLine(err.Error()))
	}
}

// applyModelSwitch resolves ref, recompiles, and moves the session onto the
// new model: the shared path for the typed /model form and the picker's
// enter. It reports the failure either path should surface.
func (m *Model) applyModelSwitch(ref string) error {
	res, err := m.app.Resolve(ref)
	if err != nil {
		return err
	}
	comp, err := m.app.Compile(res, harness.CompileOpts{Persona: m.personaOverride})
	if err != nil {
		return err
	}
	for _, line := range prompt.Diff(m.comp.Manifest, comp.Manifest) {
		m.addNote("· " + line)
	}
	m.res, m.comp, m.role = res, comp, "custom"
	m.loadQuota()
	if m.loop != nil {
		m.loop.Client = res.Client
		m.loop.Reminder = m.app.ReminderBuilder(comp)
	}
	m.log.Log("meta", map[string]string{
		"event": "model_switch", "model": res.ProviderName + "/" + res.Model.ID,
		"persona": comp.Manifest.Persona.Name, "stack": comp.Manifest.StackHash,
	})
	m.addCard("// model switched", fmt.Sprintf("%s · persona %s · %d tok",
		res.ProviderName+"/"+res.Model.ID, comp.Manifest.Persona.Name, comp.Manifest.Totals.Tokens), ui.Gold)
	return nil
}
