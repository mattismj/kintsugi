package tui

// In-TUI provider sign-in (`/connect`).
//
// The flow mirrors `kintsugi connect`, split into a browse → type → test → save
// sequence so the operator never leaves the transcript:
//
//	list     pick a configured provider (name · host · family · key status)
//	key      type the key into a masked field (bullets, never the raw text)
//	testing  a tea.Cmd pings <base>/models with the candidate key
//	done     the key was written 0600 and `/model <provider>/<model>` is offered
//	error    the endpoint refused it; retry, or save anyway (--no-verify)
//
// Privacy rules, same ones internal/credentials and `kintsugi connect` follow:
//   - the key is never rendered, never logged, never written to the session
//     file and never placed in an error message;
//   - an exported env var always outranks the stored key, and the panel says so;
//   - a failed probe writes nothing unless the operator asks for it.
//
// The probe client is built with Mode "normal": the operator explicitly asked to
// connect, and the session's own offline mode still governs real requests.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/config"
	"kintsugi/internal/credentials"
	"kintsugi/internal/harness"
	"kintsugi/internal/provider"
	"kintsugi/internal/ui"
)

// connectStage is where the view is in the sign-in sequence.
type connectStage int

const (
	connectList connectStage = iota
	connectKey
	connectTesting
	connectDone
	connectError
)

// connectStages names each stage for status text and tests.
func (s connectStage) String() string {
	switch s {
	case connectList:
		return "list"
	case connectKey:
		return "key"
	case connectTesting:
		return "testing"
	case connectDone:
		return "done"
	default:
		return "error"
	}
}

// connectProbeTimeout bounds the reachability check. It mirrors the CLI's
// connectVerifyTimeout: sign-in is an interactive step, not a task.
const connectProbeTimeout = 10 * time.Second

// connectProvider is one row of the picker. Status is credentials.Status
// ("env" | "stored" | "missing"), or "not required" when the entry has no
// api_key_env at all.
type connectProvider struct {
	Name    string
	BaseURL string
	Family  string
	Env     string
	Status  string
	Models  []string
}

// connectView is the whole modal. input holds the secret as runes: it is only
// ever counted, never rendered or logged.
type connectView struct {
	providers   []connectProvider
	sel, scroll int
	stage       connectStage
	provider    string
	input       []rune // secret; never rendered raw
	err         string
	note        string
	w, h        int
}

// connectResult is what the validation tea.Cmd sends back. It carries the
// provider name and the provider's own error text — never the key.
type connectResult struct {
	provider string
	err      error
}

// newConnectView lists the configured providers, sorted by name, with the key
// status each one would report right now.
func newConnectView(app *harness.App) *connectView {
	cv := &connectView{stage: connectList, sel: 0}
	if app == nil {
		return cv
	}
	names := make([]string, 0, len(app.Cfg.Providers))
	for name := range app.Cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := app.Cfg.Providers[name]
		models := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			if id := strings.TrimSpace(m.ID); id != "" {
				models = append(models, id)
			}
		}
		cv.providers = append(cv.providers, connectProvider{
			Name:    name,
			BaseURL: strings.TrimSpace(p.BaseURL),
			Family:  p.Family,
			Env:     strings.TrimSpace(p.APIKeyEnv),
			Status:  connectProviderStatus(name, p),
			Models:  models,
		})
	}
	return cv
}

// openConnect opens the view: on the key stage when a provider is named, on the
// list otherwise. An unknown name keeps the operator on the list, with a note,
// instead of opening a dead-end error stage.
func openConnect(app *harness.App, provider string) *connectView {
	cv := newConnectView(app)
	name := strings.TrimSpace(provider)
	if name == "" {
		return cv
	}
	for i, p := range cv.providers {
		if strings.EqualFold(p.Name, name) {
			cv.sel, cv.provider, cv.stage = i, p.Name, connectKey
			return cv
		}
	}
	cv.note = fmt.Sprintf("unknown provider %q · pick one below", name)
	return cv
}

// connectProviderStatus is the AUTH column: env beats stored, stored beats
// missing, and a provider with no api_key_env needs no key at all.
func connectProviderStatus(name string, p config.Provider) string {
	if strings.TrimSpace(p.APIKeyEnv) == "" {
		return "not required"
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

// connectModelID picks the model id for the probe: core.default_model when it
// names this provider, else the first curated model, else "model" (Ping only
// needs the endpoint, not a model).
func connectModelID(cfg config.File, name string, p config.Provider) string {
	if prov, id, ok := strings.Cut(cfg.Core.DefaultModel, "/"); ok && prov == name && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if len(p.Models) > 0 {
		if id := strings.TrimSpace(p.Models[0].ID); id != "" {
			return id
		}
	}
	return "model"
}

// ---------- state ----------

// current returns the selected provider entry.
func (v *connectView) current() (connectProvider, bool) {
	for _, p := range v.providers {
		if p.Name == v.provider {
			return p, true
		}
	}
	if v.provider != "" {
		return connectProvider{Name: v.provider, Status: "missing"}, false
	}
	return connectProvider{}, false
}

// maxScrollFor is the furthest the list can scroll with rows visible.
func (v *connectView) maxScrollFor(rows int) int {
	return max(len(v.providers)-max(rows, 1), 0)
}

// moveSel moves the selection by delta and keeps it inside the visible window.
func (v *connectView) moveSel(delta, rows int) {
	if len(v.providers) == 0 {
		v.sel, v.scroll = 0, 0
		return
	}
	v.sel = clampInt(v.sel+delta, 0, len(v.providers)-1)
	if v.sel < v.scroll {
		v.scroll = v.sel
	}
	if v.sel >= v.scroll+rows {
		v.scroll = v.sel - rows + 1
	}
	if v.scroll > v.maxScrollFor(rows) {
		v.scroll = v.maxScrollFor(rows)
	}
	if v.scroll < 0 {
		v.scroll = 0
	}
}

// scrollBy moves the list window by delta rows (positive = down). The selection
// is pulled into the window so the renderer can never disagree with the handler.
func (v *connectView) scrollBy(delta, rows int) {
	v.scroll = clampInt(v.scroll+delta, 0, v.maxScrollFor(rows))
	if len(v.providers) == 0 {
		return
	}
	if v.sel < v.scroll {
		v.sel = v.scroll
	}
	if v.sel >= v.scroll+max(rows, 1) {
		v.sel = v.scroll + max(rows, 1) - 1
	}
	v.sel = clampInt(v.sel, 0, len(v.providers)-1)
}

// openKey moves to the key stage for name with an empty field.
func (v *connectView) openKey(name string) {
	v.provider = name
	v.stage = connectKey
	v.input = nil
	v.err = ""
	v.note = ""
}

// back returns to the list and drops the secret held in memory.
func (v *connectView) back() {
	v.stage = connectList
	v.input = nil
	v.err = ""
}

// secret returns the typed key. The caller trims it; nothing logs the result.
func (v *connectView) secret() string { return strings.TrimSpace(string(v.input)) }

// ---------- validation / save ----------

// connectProbe pings the provider with the candidate key. The key lives only in
// this client; the returned error is the provider's own text.
func connectProbe(app *harness.App, name, key string) error {
	if app == nil {
		return fmt.Errorf("no workspace bound · start kintsugi from the workspace, or set KINTSUGI_WORKSPACE")
	}
	p, ok := app.Cfg.Providers[name]
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	if !strings.HasPrefix(p.BaseURL, "http") {
		return fmt.Errorf("provider %q has no base_url", name)
	}
	client, err := provider.New(p, connectModelID(app.Cfg, name, p), "normal")
	if err != nil {
		return err
	}
	// provider.New only reads the key from the environment; the candidate is
	// injected here so an exported variable cannot make a typo look valid.
	client.APIKey = key
	client.HTTP = &http.Client{Timeout: connectProbeTimeout}

	ctx, cancel := context.WithTimeout(context.Background(), connectProbeTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
}

// connectSubmitCmd validates then stores, off the Update goroutine. A failed
// probe writes nothing, so a typo can never end up in the store.
func (m *Model) connectSubmitCmd(name, key string) tea.Cmd {
	app := m.app
	return func() tea.Msg {
		if err := connectProbe(app, name, key); err != nil {
			return connectResult{provider: name, err: err}
		}
		if err := credentials.Set(name, key); err != nil {
			return connectResult{provider: name, err: err}
		}
		return connectResult{provider: name}
	}
}

// connectSaveCmd is the error stage's `s`: store the key without verifying, the
// same escape hatch `kintsugi connect --no-verify` offers.
func (m *Model) connectSaveCmd(name, key string) tea.Cmd {
	return func() tea.Msg {
		if key == "" {
			return connectResult{provider: name, err: fmt.Errorf("%s: no key entered", name)}
		}
		if err := credentials.Set(name, key); err != nil {
			return connectResult{provider: name, err: err}
		}
		return connectResult{provider: name}
	}
}

// ---------- input ----------

// connectListRows is how many provider rows the list can show. The screen is
// header(1) + panel + footer(2), and the panel body is panelH-3 (title + frame).
// It is shared by the key handler and the renderer so they cannot disagree.
func connectListRows(screenH int) int {
	return max(screenH-1-2-3-1, 1) // -1 for the column header row
}

// connectRows is connectListRows for the current terminal height.
func (m *Model) connectRows() int { return connectListRows(max(m.h, 8)) }

// handleConnectKey owns the keyboard while the panel is open.
func (m *Model) handleConnectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v := m.connect
	if v == nil {
		return m, nil
	}
	if msg.Type == tea.KeyCtrlC {
		m.connect = nil
		return m, tea.Quit
	}
	rows := m.connectRows()

	switch v.stage {
	case connectList:
		switch msg.String() {
		case "esc", "q":
			m.connect = nil
		case "up", "k":
			v.moveSel(-1, rows)
		case "down", "j":
			v.moveSel(1, rows)
		case "pgup":
			v.scrollBy(-rows, rows)
		case "pgdown":
			v.scrollBy(rows, rows)
		case "enter":
			if v.sel >= 0 && v.sel < len(v.providers) {
				v.openKey(v.providers[v.sel].Name)
			}
		}

	case connectKey:
		switch msg.Type {
		case tea.KeyEsc:
			v.back()
		case tea.KeyEnter:
			secret := v.secret()
			if secret == "" {
				v.err = "no key entered"
				return m, nil
			}
			v.stage, v.err = connectTesting, ""
			return m, m.connectSubmitCmd(v.provider, secret)
		case tea.KeyBackspace:
			if n := len(v.input); n > 0 {
				v.input = v.input[:n-1]
			}
		case tea.KeyCtrlU:
			v.input = nil
		case tea.KeyRunes:
			v.input = append(v.input, msg.Runes...)
		case tea.KeySpace:
			v.input = append(v.input, ' ')
		}

	case connectTesting:
		// The probe is in flight: nothing but closing the panel is accepted, so
		// a stray key cannot start a second ping or drop the candidate key.
		switch msg.Type {
		case tea.KeyEsc:
			m.connect = nil
		}

	case connectDone:
		switch msg.String() {
		case "esc", "enter", "q":
			m.connect = nil
		}

	case connectError:
		switch msg.String() {
		case "esc":
			v.back()
		case "enter":
			// Retry with the same secret: the candidate is still in memory, so
			// the operator never retypes a key for a transient 5xx.
			secret := v.secret()
			if secret == "" {
				v.stage = connectKey
				return m, nil
			}
			v.stage, v.err = connectTesting, ""
			return m, m.connectSubmitCmd(v.provider, secret)
		case "s":
			secret := v.secret()
			v.stage, v.err = connectTesting, ""
			return m, m.connectSaveCmd(v.provider, secret)
		}
	}
	return m, nil
}

// handleConnectMouse implements wheel scrolling over the list only.
func (m *Model) handleConnectMouse(msg tea.MouseMsg) {
	v := m.connect
	if v == nil || msg.Action != tea.MouseActionPress || v.stage != connectList {
		return
	}
	rows := m.connectRows()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scrollBy(-3, rows)
	case tea.MouseButtonWheelDown:
		v.scrollBy(3, rows)
	}
}

// handleConnectResult folds the probe outcome back into the view. A result for a
// provider the operator has navigated away from is dropped.
func (m *Model) handleConnectResult(res connectResult) (tea.Model, tea.Cmd) {
	v := m.connect
	if v == nil || res.provider != v.provider {
		return m, nil
	}
	if res.err != nil {
		// The candidate key stays in memory so `s` can save it anyway; nothing
		// of it is written to the transcript.
		v.stage, v.err = connectError, oneLine(res.err.Error())
		return m, nil
	}
	// Success: the key is on disk, the field is cleared, and the transcript gets
	// a permanent card so the follow-up survives closing the panel.
	v.stage, v.err, v.input = connectDone, "", nil
	model := "model"
	if m.app != nil {
		if p, ok := m.app.Cfg.Providers[v.provider]; ok {
			model = connectModelID(m.app.Cfg, v.provider, p)
		}
	} else if p, ok := v.current(); ok && len(p.Models) > 0 {
		model = p.Models[0]
	}
	v.note = fmt.Sprintf("/model %s/%s", v.provider, model)
	m.connectSavedStatus()
	env := v.currentEnv()
	hint := "no env var is set for this provider"
	if env != "" {
		hint = "an exported " + env + " still wins"
	}
	m.addCard("// connect · saved "+v.provider,
		fmt.Sprintf("key stored 0600 · %s\nuse it now: %s", hint, v.note), ui.OK)
	return m, nil
}

// currentEnv is the provider's env var for the transcript note.
func (v *connectView) currentEnv() string {
	if p, ok := v.current(); ok {
		return p.Env
	}
	return ""
}

// connectSavedStatus refreshes the stored provider's row so a return to the list
// shows the new source.
func (m *Model) connectSavedStatus() {
	v := m.connect
	if v == nil || m.app == nil {
		return
	}
	p, ok := m.app.Cfg.Providers[v.provider]
	if !ok {
		return
	}
	for i := range v.providers {
		if v.providers[i].Name == v.provider {
			v.providers[i].Status = connectProviderStatus(v.provider, p)
			return
		}
	}
}

// ---------- rendering ----------

// renderConnect draws the whole surface: the panel plus its two footer rows. The
// panel never exceeds height rows and no line is wider than width.
func (v *connectView) renderConnect(width, height int, theme ui.Theme) string {
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
	bodyH := panelH - 3 // one title row + two frame rows
	title, body := v.panelBody(max(width-2, 8), bodyH, theme)

	lines := strings.Split(theme.Panel(title, body, width, panelH), "\n")
	lines = append(lines, footer...)
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// footerLines is the key legend plus the stage's one-line reminder.
func (v *connectView) footerLines(width int, theme ui.Theme) []string {
	keys, hint := "[esc] close", "keys are stored 0600 · env vars always win"
	switch v.stage {
	case connectList:
		keys = "[enter] select · [↑/↓] move · [esc] close"
		hint = "env vars always win · keys are stored 0600 in " + credentials.Path()
	case connectKey:
		keys = "[enter] test & save · [esc] back"
		hint = "the key is never rendered or logged · env still wins"
	case connectTesting:
		keys = "[esc] close"
		hint = "probing /models · nothing is saved until the key verifies"
	case connectDone:
		keys = "[esc] close"
		hint = "an exported env var outranks the stored key"
	case connectError:
		keys = "[enter] retry · [s] save anyway · [esc] back"
		hint = "the key was not saved · env vars always win"
	}
	return []string{
		theme.Paint(ui.PaperSoft, clipShorten(keys, width)),
		theme.Paint(ui.PaperSoft, clipShorten(hint, width)),
	}
}

// panelBody dispatches to the stage renderer.
func (v *connectView) panelBody(width, height int, theme ui.Theme) (string, string) {
	switch v.stage {
	case connectKey:
		return v.keyBody(width, height, theme)
	case connectTesting:
		return v.testingBody(width, height, theme)
	case connectDone:
		return v.doneBody(width, height, theme)
	case connectError:
		return v.errorBody(width, height, theme)
	default:
		return v.listBody(width, height, theme)
	}
}

// listBody is the provider picker.
func (v *connectView) listBody(width, height int, theme ui.Theme) (string, string) {
	var lines []string
	if v.note != "" {
		lines = append(lines, theme.Paint(ui.Warn, clipShorten(v.note, width)))
	}
	v.clampSel()
	rows := max(height-len(lines)-1, 1) // -1 for the column header
	v.scroll = clampInt(v.scroll, 0, v.maxScrollFor(rows))
	if v.sel >= 0 {
		if v.sel < v.scroll {
			v.scroll = v.sel
		}
		if v.sel >= v.scroll+rows {
			v.scroll = v.sel - rows + 1
		}
	}
	nameW, hostW, famW := v.colWidths(width)
	lines = append(lines, theme.Paint(ui.PaperSoft, clipShorten(v.headerRow(nameW, hostW, famW), width)))
	if len(v.providers) == 0 {
		lines = append(lines, theme.Paint(ui.PaperSoft, "· no providers configured"))
	}
	end := minInt(v.scroll+rows, len(v.providers))
	for i := v.scroll; i < end; i++ {
		lines = append(lines, v.row(i, width, theme))
	}
	return "// connect · providers", padLines(lines, height)
}

// keyBody is the masked input. base and env are shown; the secret is not.
func (v *connectView) keyBody(width, height int, theme ui.Theme) (string, string) {
	p, _ := v.current()
	env := p.Env
	if env == "" {
		env = "not required"
	}
	var lines []string
	lines = append(lines, theme.Paint(ui.PaperSoft, "base      ")+clipShorten(orDash(p.BaseURL), max(width-10, 8)))
	lines = append(lines, theme.Paint(ui.PaperSoft, "env       ")+clipShorten(env, max(width-10, 8)))
	lines = append(lines, theme.Paint(ui.PaperSoft, "status    ")+p.Status)
	lines = append(lines, "")

	// Exactly one bullet per rune, clipped to the row; an empty field shows the
	// caret so it is obvious where typing lands.
	field := max(width-10, 4)
	mask := strings.Repeat("•", minInt(len(v.input), field))
	if mask == "" {
		mask = theme.Paint(ui.Gold, "▌")
	} else {
		mask = theme.Paint(ui.Paper, mask)
	}
	lines = append(lines, theme.Paint(ui.PaperSoft, "key       ")+mask)

	if v.err != "" {
		lines = append(lines, "", theme.Paint(ui.Danger, clipShorten("✗ "+v.err, width)))
	}
	return "// connect · " + v.provider, padLines(lines, height)
}

// testingBody carries no key and no URL: only the provider name.
func (v *connectView) testingBody(width, height int, theme ui.Theme) (string, string) {
	lines := []string{
		theme.Paint(ui.Gold, clipShorten("testing "+v.provider+"…", width)),
		"",
		theme.Paint(ui.PaperSoft, clipShorten("GET /models · 10s timeout · the key stays in memory", width)),
	}
	return "// connect · " + v.provider, padLines(lines, height)
}

// doneBody reports the save and the follow-up command. It never silently swaps
// the running client: /model is the operator's move.
func (v *connectView) doneBody(width, height int, theme ui.Theme) (string, string) {
	target := v.note
	if target == "" {
		target = "/model " + v.provider + "/model"
	}
	lines := []string{
		theme.Paint(ui.OK, clipShorten("saved "+v.provider+" · "+target, width)),
		"",
		theme.Paint(ui.Gold, clipShorten(target, width)),
		"",
		theme.Paint(ui.PaperSoft, clipShorten("an exported "+orDash(v.currentEnv())+" outranks the stored key", width)),
		theme.Paint(ui.PaperSoft, clipShorten("stored 0600 in "+credentials.Path(), width)),
	}
	return "// connect · " + v.provider, padLines(lines, height)
}

// errorBody shows the provider's message only — never the candidate key.
func (v *connectView) errorBody(width, height int, theme ui.Theme) (string, string) {
	msg := v.err
	if msg == "" {
		msg = "verification failed"
	}
	wrapped := wrap(msg, max(width-2, 8))
	lines := []string{theme.Paint(ui.Danger, clipShorten("✗ "+wrapped[0], width))}
	for _, l := range wrapped[1:] {
		lines = append(lines, theme.Paint(ui.Danger, clipShorten("  "+l, width)))
	}
	lines = append(lines,
		"",
		theme.Paint(ui.PaperSoft, clipShorten("nothing was saved · [s] stores it anyway", width)),
	)
	return "// connect · " + v.provider, padLines(lines, height)
}

// clampSel keeps sel inside the provider list (or at 0 when it is empty).
func (v *connectView) clampSel() {
	if len(v.providers) == 0 {
		v.sel, v.scroll = 0, 0
		return
	}
	v.sel = clampInt(v.sel, 0, len(v.providers)-1)
}

// colWidths sizes the three text columns from the data, then shrinks the host
// column (the least important) when the panel is narrow. The whole row is
// truncated as a final guard, so no line can exceed width.
func (v *connectView) colWidths(width int) (nameW, hostW, famW int) {
	nameW, hostW, famW = len("provider"), len("host"), len("family")
	for _, p := range v.providers {
		nameW = max(nameW, len(p.Name))
		hostW = max(hostW, len(connectHost(p.BaseURL)))
		famW = max(famW, len(p.Family))
	}
	nameW, famW = minInt(nameW, 22), minInt(famW, 10)
	// marker(2) + three 2-cell gaps(6) + the widest status("not required", 12)
	budget := width - 2 - 6 - 12
	if nameW+famW+hostW > budget {
		hostW = max(budget-nameW-famW, 8)
	}
	if nameW+hostW+famW > budget {
		nameW = max(budget-hostW-famW, 8)
	}
	if nameW+hostW+famW > budget {
		famW = max(budget-nameW-hostW, 4)
	}
	return nameW, hostW, famW
}

func (v *connectView) headerRow(nameW, hostW, famW int) string {
	return fmt.Sprintf("  %-*s  %-*s  %-*s  %s",
		nameW, "provider", hostW, "host", famW, "family", "key")
}

func (v *connectView) row(i, width int, theme ui.Theme) string {
	p := v.providers[i]
	nameW, hostW, famW := v.colWidths(width)
	mark, tone := " ", ui.PaperDim
	if i == v.sel {
		mark, tone = "▸", ui.Gold
	}
	row := fmt.Sprintf("%s %-*s  %-*s  %-*s  %s", mark,
		nameW, clipShorten(p.Name, nameW),
		hostW, clipShorten(connectHost(p.BaseURL), hostW),
		famW, clipShorten(orDash(p.Family), famW),
		p.Status)
	return theme.Paint(tone, clipShorten(row, width))
}

// connectHost strips the scheme (and any path) from a base URL for the host
// column. The full URL stays on the key stage; the row only needs a landmark.
func connectHost(baseURL string) string {
	s := strings.TrimSpace(baseURL)
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "—"
	}
	return s
}

// clipShorten keeps s within width cells, ellipsizing when it must cut. The
// package's truncate reserves the ellipsis outside the budget (w+1 cells), which
// would push a panel line one cell past the frame, so the connect renderer uses
// this instead.
func clipShorten(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// padLines makes lines exactly height rows long: it pads the tail and, when the
// list is too tall, keeps the head (the list renderer already scrolled).
func padLines(lines []string, height int) string {
	if height < 1 {
		height = 1
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
