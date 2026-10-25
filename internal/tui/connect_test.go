package tui

// Tests for the in-TUI provider sign-in (/connect). They are hermetic: the
// credential store is redirected with XDG_CONFIG_HOME and the only network
// traffic is to an httptest server on loopback.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"

	"kintsugi/internal/config"
	"kintsugi/internal/credentials"
	"kintsugi/internal/ui"
)

// ---------- helpers ----------

// connectModel is a transcript whose config carries the given providers.
func connectModel(w, h int, providers map[string]config.Provider) *Model {
	m := testModel(w, h)
	m.app.Cfg.Providers = providers
	return m
}

// keyRunes types s as one rune batch, the way a paste or fast typing arrives.
func keyRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func keyEnter() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }
func keyEsc() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyEsc} }

// monoTheme is the no-color theme the render assertions run against, so the
// panel text can be searched without stripping escape sequences.
func monoTheme() ui.Theme {
	return ui.Detect(func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	})
}

// pressKeys feeds a sequence of keys through Update, ignoring commands.
func pressKeys(m *Model, keys ...tea.KeyMsg) {
	for _, k := range keys {
		m.Update(k)
	}
}

// runCmd runs a returned tea.Cmd and delivers its message back to the model.
func runCmd(t *testing.T, m *Model, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("command produced no message")
	}
	m.Update(msg)
	return msg
}

// storedKey reads the credential store through the same path the TUI writes.
func storedKey(t *testing.T, provider string) (string, bool) {
	t.Helper()
	st, err := credentials.Load()
	if err != nil {
		t.Fatalf("credentials.Load: %v", err)
	}
	return st.Get(provider)
}

// manyProviders builds n providers named p00..pNN for scroll tests.
func manyProviders(n int) map[string]config.Provider {
	out := map[string]config.Provider{}
	for i := 0; i < n; i++ {
		out[fmt.Sprintf("p%02d", i)] = config.Provider{
			BaseURL: fmt.Sprintf("https://p%02d.example.test/v1", i),
			Family:  "test",
		}
	}
	return out
}

// ---------- list ----------

func TestNewConnectViewListsProvidersWithStatus(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KINTSUGI_TUI_CONNECT_ENV", "exported-key")

	if err := credentials.Set("storedprov", "sk-stored-value"); err != nil {
		t.Fatalf("credentials.Set: %v", err)
	}

	app := testModel(100, 30).app
	app.Cfg.Providers = map[string]config.Provider{
		"zeta":       {BaseURL: "https://zeta.example.test/v1", Family: "zeta", APIKeyEnv: "KINTSUGI_TUI_CONNECT_ENV"},
		"alpha":      {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_CONNECT_MISSING"},
		"storedprov": {BaseURL: "https://stored.example.test/v1", Family: "stored", APIKeyEnv: "KINTSUGI_TUI_CONNECT_STORED"},
		"local":      {BaseURL: "http://127.0.0.1:11434/v1", Family: "llama"},
	}

	cv := newConnectView(app)
	if cv.stage != connectList {
		t.Fatalf("stage = %v, want list", cv.stage)
	}
	var names []string
	for _, p := range cv.providers {
		names = append(names, p.Name)
	}
	want := []string{"alpha", "local", "storedprov", "zeta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("provider order = %v, want %v", names, want)
	}

	status := map[string]string{}
	models := map[string][]string{}
	for _, p := range cv.providers {
		status[p.Name] = p.Status
		models[p.Name] = p.Models
	}
	if status["zeta"] != "env" {
		t.Fatalf("zeta status = %q, want env", status["zeta"])
	}
	if status["storedprov"] != "stored" {
		t.Fatalf("storedprov status = %q, want stored", status["storedprov"])
	}
	if status["alpha"] != "missing" {
		t.Fatalf("alpha status = %q, want missing", status["alpha"])
	}
	if status["local"] != "not required" {
		t.Fatalf("local status = %q, want not required", status["local"])
	}

	// The rendered picker names every provider and its host, and stays inside
	// the width it was given.
	out := cv.renderConnect(100, 28, monoTheme())
	for _, name := range want {
		if !strings.Contains(out, name) {
			t.Fatalf("picker missing provider %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "stored.example.test") {
		t.Fatalf("picker missing host column:\n%s", out)
	}
	if !strings.Contains(out, "// connect · providers") {
		t.Fatalf("picker missing title:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 100 {
			t.Fatalf("line wider than the panel (%d): %q", len([]rune(line)), line)
		}
	}
	if n := len(strings.Split(out, "\n")); n > 28 {
		t.Fatalf("render returned %d lines, want <= 28", n)
	}
}

// TestConnectRenderBounds renders every stage at awkward sizes: no line may be
// wider than the surface and no render may be taller than it.
func TestConnectRenderBounds(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	app := testModel(80, 24).app
	app.Cfg.Providers = map[string]config.Provider{
		"a-very-long-provider-name-that-will-not-fit": {
			BaseURL:   "https://very-long-host-name.example.test/v1/chat",
			Family:    "extremelylongfamily",
			APIKeyEnv: "KINTSUGI_TUI_BOUNDS_KEY",
		},
	}
	sizes := [][2]int{{24, 8}, {40, 12}, {80, 24}, {140, 44}}
	for _, size := range sizes {
		w, h := size[0], size[1]
		for _, stage := range []connectStage{connectList, connectKey, connectTesting, connectDone, connectError} {
			cv := openConnect(app, "a-very-long-provider-name-that-will-not-fit")
			cv.stage = stage
			cv.input = []rune("sk-bounds")
			cv.err = "provider 401: an error message long enough to need wrapping at any sensible width"
			cv.note = "/model a-very-long-provider-name-that-will-not-fit/model"
			out := cv.renderConnect(w, h, monoTheme())
			lines := strings.Split(out, "\n")
			if len(lines) > h {
				t.Fatalf("%v stage %v: %d lines > %d", size, stage, len(lines), h)
			}
			for _, line := range lines {
				if got := len([]rune(line)); got > w {
					t.Fatalf("%v stage %v: line of %d cells > %d: %q", size, stage, got, w, line)
				}
			}
			if strings.Contains(out, "sk-bounds") {
				t.Fatalf("%v stage %v leaked the key", size, stage)
			}
		}
	}
}

func TestConnectListNavigationAndScrollClamp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(80, 24, manyProviders(40))
	m.connect = openConnect(m.app, "")

	rows := m.connectRows()
	if rows <= 0 {
		t.Fatalf("rows = %d", rows)
	}
	limit := m.connect.maxScrollFor(rows)
	if limit <= 0 {
		t.Fatalf("test needs a scrollable list, limit = %d", limit)
	}

	// Down past the end: the selection stops on the last row and the window
	// follows it, never scrolling past the clamp.
	for i := 0; i < 60; i++ {
		pressKeys(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.connect.sel != 39 {
		t.Fatalf("sel = %d, want 39", m.connect.sel)
	}
	if m.connect.scroll != limit {
		t.Fatalf("scroll = %d, want clamp %d", m.connect.scroll, limit)
	}

	// Up to the top: both the selection and the window return to zero.
	for i := 0; i < 60; i++ {
		pressKeys(m, tea.KeyMsg{Type: tea.KeyUp})
	}
	if m.connect.sel != 0 || m.connect.scroll != 0 {
		t.Fatalf("sel/scroll = %d/%d, want 0/0", m.connect.sel, m.connect.scroll)
	}

	// PgDn scrolls the window without moving the selection off screen.
	pressKeys(m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.connect.scroll == 0 {
		t.Fatal("pgdown did not scroll the list")
	}
	if m.connect.scroll > limit {
		t.Fatalf("scroll = %d, want <= %d", m.connect.scroll, limit)
	}
	// The selected row is always visible.
	if m.connect.sel < m.connect.scroll || m.connect.sel >= m.connect.scroll+rows {
		t.Fatalf("selection %d outside window [%d,%d)", m.connect.sel, m.connect.scroll, m.connect.scroll+rows)
	}
}

func TestConnectMouseWheelScrollsList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(80, 24, manyProviders(40))
	m.connect = openConnect(m.app, "")

	m.Update(wheelMsg(tea.MouseButtonWheelDown))
	if m.connect.scroll != 3 {
		t.Fatalf("wheel down scroll = %d, want 3", m.connect.scroll)
	}
	m.Update(wheelMsg(tea.MouseButtonWheelDown))
	if m.connect.scroll != 6 {
		t.Fatalf("wheel down scroll = %d, want 6", m.connect.scroll)
	}
	m.Update(wheelMsg(tea.MouseButtonWheelUp))
	if m.connect.scroll != 3 {
		t.Fatalf("wheel up scroll = %d, want 3", m.connect.scroll)
	}
	for i := 0; i < 40; i++ {
		m.Update(wheelMsg(tea.MouseButtonWheelDown))
	}
	rows := m.connectRows()
	if m.connect.scroll != m.connect.maxScrollFor(rows) {
		t.Fatalf("wheel down clamped at %d, want %d", m.connect.scroll, m.connect.maxScrollFor(rows))
	}
	for i := 0; i < 40; i++ {
		m.Update(wheelMsg(tea.MouseButtonWheelUp))
	}
	if m.connect.scroll != 0 {
		t.Fatalf("wheel up clamped at %d, want 0", m.connect.scroll)
	}
}

// ---------- open / masked input ----------

func TestSlashConnectOpensPickerAndSelects(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY", Models: []config.Model{{ID: "alpha-1"}}},
	})

	pressKeys(m, keyRunes("/connect"), keyEnter())
	if m.connect == nil {
		t.Fatal("/connect did not open the connect view")
	}
	if m.connect.stage != connectList {
		t.Fatalf("stage = %v, want list", m.connect.stage)
	}

	pressKeys(m, keyEnter())
	if m.connect.stage != connectKey {
		t.Fatalf("stage = %v, want key", m.connect.stage)
	}
	if m.connect.provider != "alpha" {
		t.Fatalf("provider = %q, want alpha", m.connect.provider)
	}

	// The key stage shows the endpoint and the env var, masked input only.
	out := m.View()
	for _, want := range []string{"// connect · alpha", "https://alpha.example.test/v1", "KINTSUGI_TUI_ALPHA_KEY", "key"} {
		if !strings.Contains(out, want) {
			t.Fatalf("key stage missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "▌") {
		t.Fatalf("empty field should show the caret:\n%s", out)
	}
}

func TestConnectKeyIsMaskedAndNeverRendered(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	m.connect = openConnect(m.app, "alpha")
	if m.connect.stage != connectKey {
		t.Fatalf("stage = %v, want key", m.connect.stage)
	}

	const secret = "sk-tui-secret-9000"
	pressKeys(m, keyRunes(secret))
	if got := len(m.connect.input); got != len([]rune(secret)) {
		t.Fatalf("input length = %d, want %d", got, len([]rune(secret)))
	}

	out := m.View()
	if strings.Contains(out, secret) {
		t.Fatalf("rendered view leaked the key:\n%s", out)
	}
	if strings.Contains(out, "sk-tui") {
		t.Fatalf("rendered view leaked part of the key:\n%s", out)
	}
	if !strings.Contains(out, "••••") {
		t.Fatalf("masked field missing bullets:\n%s", out)
	}
	if strings.Count(out, "•") < len([]rune(secret)) {
		t.Fatalf("want one bullet per rune (%d), got %d:\n%s", len([]rune(secret)), strings.Count(out, "•"), out)
	}

	// Backspace removes a rune; ctrl+u clears the field.
	pressKeys(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if len(m.connect.input) != len([]rune(secret))-1 {
		t.Fatalf("backspace left %d runes", len(m.connect.input))
	}
	pressKeys(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if len(m.connect.input) != 0 {
		t.Fatalf("ctrl+u left %d runes", len(m.connect.input))
	}
}

func TestConnectEscGoesKeyToListToClosed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	m.connect = openConnect(m.app, "alpha")
	pressKeys(m, keyRunes("sk-discarded"))

	pressKeys(m, keyEsc())
	if m.connect == nil || m.connect.stage != connectList {
		t.Fatalf("first esc should return to the list, got %+v", m.connect)
	}
	if len(m.connect.input) != 0 {
		t.Fatal("going back must drop the secret held in memory")
	}
	pressKeys(m, keyEsc())
	if m.connect != nil {
		t.Fatal("second esc should close the view")
	}
}

func TestConnectRejectsKeyOnCommandLine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	pressKeys(m, keyRunes("/connect alpha sk-argv-secret"), keyEnter())
	if m.connect != nil {
		t.Fatal("/connect with two fields must not open the panel")
	}
	if out := m.View(); strings.Contains(out, "sk-argv-secret") {
		t.Fatalf("transcript echoed the argument as a key:\n%s", out)
	}
	if !strings.Contains(m.View(), "never read from the command line") {
		// The transcript wraps the note, so fall back to the raw block text.
		refused := false
		for _, b := range m.blocks {
			if b.kind == blockNote && strings.Contains(b.text, "never read from the command line") {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("expected a refusal note, got blocks %+v", m.blocks)
		}
	}
}

// ---------- validation ----------

// okProviderServer answers /models with 200 and records the bearer token it saw.
func okProviderServer(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if seen != nil {
			*seen = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"alpha-1"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConnectValidationSuccessSavesAndReachesDone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var auth string
	srv := okProviderServer(t, &auth)

	m := connectModel(100, 30, map[string]config.Provider{
		"local": {BaseURL: srv.URL, Family: "test", APIKeyEnv: "KINTSUGI_TUI_LOCAL_KEY", Models: []config.Model{{ID: "local-small"}}},
	})
	m.connect = openConnect(m.app, "local")
	if m.connect.stage != connectKey {
		t.Fatalf("stage = %v, want key", m.connect.stage)
	}

	const secret = "sk-live-ok-1234"
	pressKeys(m, keyRunes(secret))
	_, cmd := m.Update(keyEnter())
	if m.connect.stage != connectTesting {
		t.Fatalf("stage = %v, want testing", m.connect.stage)
	}
	// The testing stage carries the provider name only: no key, no URL.
	testing := m.View()
	if strings.Contains(testing, secret) || strings.Contains(testing, srv.URL) {
		t.Fatalf("testing stage leaked material:\n%s", testing)
	}
	if !strings.Contains(testing, "testing local…") {
		t.Fatalf("testing stage missing its label:\n%s", testing)
	}

	runCmd(t, m, cmd)
	if m.connect.stage != connectDone {
		t.Fatalf("stage = %v, want done (err %q)", m.connect.stage, m.connect.err)
	}
	if got, ok := storedKey(t, "local"); !ok || got != secret {
		t.Fatalf("stored key = %q, %v; want the typed key", got, ok)
	}
	if !strings.Contains(auth, secret) {
		t.Fatalf("probe did not send the candidate key, Authorization = %q", auth)
	}
	if len(m.connect.input) != 0 {
		t.Fatal("the key must be cleared from memory once saved")
	}

	out := m.View()
	if strings.Contains(out, secret) {
		t.Fatalf("done view leaked the key:\n%s", out)
	}
	if !strings.Contains(out, "saved local") {
		t.Fatalf("done stage missing the save line:\n%s", out)
	}
	if !strings.Contains(out, "/model local/local-small") {
		t.Fatalf("done stage missing the follow-up command:\n%s", out)
	}
	if !strings.Contains(out, "outranks the stored key") {
		t.Fatalf("done stage must say the env var still wins:\n%s", out)
	}

	// The transcript keeps a card so the follow-up survives closing the panel.
	found := false
	for _, b := range m.blocks {
		if b.kind == blockCard && strings.Contains(b.title, "connect · saved") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no connect card in the transcript: %+v", m.blocks)
	}
}

func TestConnectValidationFailureDoesNotSave(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "invalid api key")
	}))
	t.Cleanup(srv.Close)

	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: srv.URL, Family: "test", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY", Models: []config.Model{{ID: "alpha-1"}}},
	})
	m.connect = openConnect(m.app, "alpha")
	pressKeys(m, keyRunes("sk-bad-9999"))
	_, cmd := m.Update(keyEnter())
	runCmd(t, m, cmd)

	if m.connect.stage != connectError {
		t.Fatalf("stage = %v, want error", m.connect.stage)
	}
	if _, ok := storedKey(t, "alpha"); ok {
		t.Fatal("a failed probe must not save the key")
	}
	if _, err := os.Stat(credentials.Path()); err == nil {
		t.Fatal("a failed probe must not create the credential store")
	}

	out := m.View()
	if !strings.Contains(out, "401") {
		t.Fatalf("error stage missing the provider status:\n%s", out)
	}
	if strings.Contains(out, "sk-bad-9999") {
		t.Fatalf("error stage leaked the key:\n%s", out)
	}
	if !strings.Contains(out, "[s] save anyway") {
		t.Fatalf("error stage missing its keys:\n%s", out)
	}

	// `s` stores the key anyway and lands on done.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	runCmd(t, m, cmd)
	if m.connect.stage != connectDone {
		t.Fatalf("stage = %v, want done after save-anyway (err %q)", m.connect.stage, m.connect.err)
	}
	if got, ok := storedKey(t, "alpha"); !ok || got != "sk-bad-9999" {
		t.Fatalf("save-anyway stored %q, %v", got, ok)
	}

	// And the store really is mode 0600.
	info, err := os.Stat(credentials.Path())
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store mode = %o, want 600", perm)
	}
	if filepath.Dir(credentials.Path()) == "" {
		t.Fatal("store path has no directory")
	}
}

func TestConnectRetryAfterErrorReusesTheKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "upstream down")
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)

	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: srv.URL, Family: "test", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	m.connect = openConnect(m.app, "alpha")
	pressKeys(m, keyRunes("sk-retry-me"))
	_, cmd := m.Update(keyEnter())
	runCmd(t, m, cmd)
	if m.connect.stage != connectError {
		t.Fatalf("stage = %v, want error", m.connect.stage)
	}

	fail = false
	_, cmd = m.Update(keyEnter())
	if m.connect.stage != connectTesting {
		t.Fatalf("retry stage = %v, want testing", m.connect.stage)
	}
	runCmd(t, m, cmd)
	if m.connect.stage != connectDone {
		t.Fatalf("stage = %v, want done after a successful retry", m.connect.stage)
	}
	if got, ok := storedKey(t, "alpha"); !ok || got != "sk-retry-me" {
		t.Fatalf("stored key = %q, %v", got, ok)
	}
}

func TestConnectEmptyKeyStaysOnKeyStage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	m.connect = openConnect(m.app, "alpha")
	_, cmd := m.Update(keyEnter())
	if cmd != nil {
		t.Fatal("an empty field must not start a probe")
	}
	if m.connect.stage != connectKey {
		t.Fatalf("stage = %v, want key", m.connect.stage)
	}
	if !strings.Contains(m.View(), "no key entered") {
		t.Fatalf("expected an inline hint:\n%s", m.View())
	}
}

func TestConnectResultForOtherProviderIsIgnored(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	m.connect = openConnect(m.app, "alpha")
	m.Update(connectResult{provider: "beta", err: fmt.Errorf("stale")})
	if m.connect.stage != connectKey || m.connect.err != "" {
		t.Fatalf("stale result changed the view: %v %q", m.connect.stage, m.connect.err)
	}
}

func TestConnectViewUnknownProviderStaysOnList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := connectModel(100, 30, map[string]config.Provider{
		"alpha": {BaseURL: "https://alpha.example.test/v1", Family: "alpha", APIKeyEnv: "KINTSUGI_TUI_ALPHA_KEY"},
	})
	pressKeys(m, keyRunes("/connect nope"), keyEnter())
	if m.connect == nil {
		t.Fatal("expected the picker to open")
	}
	if m.connect.stage != connectList {
		t.Fatalf("stage = %v, want list", m.connect.stage)
	}
	if !strings.Contains(m.View(), "unknown provider") {
		t.Fatalf("expected an unknown-provider note:\n%s", m.View())
	}
}
