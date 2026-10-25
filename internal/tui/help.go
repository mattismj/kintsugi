package tui

import (
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"kintsugi/internal/ui"
)

// helpGroup is one section of the command reference. The grouping exists because
// the flat list had grown past the point where it could be read: an operator
// looking for "how do I change the model" should not have to scan thirty lines
// of unrelated commands.
type helpGroup struct {
	name  string
	about string
	lines []string
}

// helpGroups is the whole built-in reference. Every command in the handleSlash
// switch belongs to exactly one group; a test derives the switch's cases and
// asserts that, so adding a command without documenting it fails the build
// rather than drifting — which is how `lab` and `mods` once shipped unreachable.
func helpGroups() []helpGroup {
	return []helpGroup{
		{name: "session", about: "the turn, the transcript", lines: []string{
			"/help [group]     this menu, or one group's commands",
			"/status           next action + session, budget and counters",
			"/prompt           prompt-stack inspector",
			"/agents           AGENTS.md, persona rule and markdown agents",
			"/modelctx         per-model AGENTS.md: view, edit, force one",
			"/scope            the declared boundary and the files that set it",
			"/clear            clear the transcript",
			"/quit             leave (ctrl+c, ctrl+d)",
		}},
		{name: "context", about: "the layers that make it useful", lines: []string{
			"/frame            inject Authorization & Domain once",
			"/context          open the layer-5 workbench",
			"/context edit     edit CONTEXT.md in $EDITOR",
			"/context diff     review curator proposals",
			"/context refresh  reload CONTEXT.md and recompile",
			"/persona          open the layer-2 pack workbench",
			"/persona edit     edit the matched persona",
			"/persona base     edit the shared _base persona",
			"/persona use <n>  force a persona; /persona auto to clear",
			"/skills           list workspace skills (bodies load on demand)",
			"/commands         list the custom commands (.kintsugi/commands)",
			"/kb               list knowledge bases; /kb <query> searches them",
			"/memory           recent workspace memory entries",
			"/panel <pillar>   expand one pillar: context|scope|refusal|privacy",
		}},
		{name: "models", about: "who answers, and how", lines: []string{
			"/model            pick a model (list, filter, switch)",
			"/refusal          view and teach the refusal lexicon",
			"/changes          every file the agent touched, with diffs",
			"/role <name>      switch to a configured model role",
			"/connect [p]      provider sign-in panel (stores a 0600 key)",
		}},
		{name: "permissions", about: "what the harness may do", lines: []string{
			"/mode [name]      show the permission mode, or switch it",
			"/guard [on|off]   continuity guard: drift re-injection this session",
		}},
		{name: "review", about: "plans, decisions, the record", lines: []string{
			"/plan             open the newest plan for review",
			"/plan list        list saved plans with their dates",
			"/plan <name>      open that plan for review",
			"/plan review      run a review round on the open plan",
			"/remember <text>  append a lesson to learned.md",
			"/recall [query]   recall prior accepted work (default: last task)",
			"/ledger [kind]    recent ledger entries (positive kinds + drift)",
			"/accept <text>    record an acceptance in the workspace ledger",
			"/rewind [id]      list checkpoints, or restore one's history",
		}},
	}
}

// helpMenu is /help with no argument: the group index plus the three keys an
// operator needs immediately. Deliberately short — it is a menu, not a manual.
func (m *Model) helpMenu() string {
	var b strings.Builder
	b.WriteString("commands are grouped · `/<group>` opens one\n\n")
	for _, g := range helpGroups() {
		cmd := "/help " + g.name
		b.WriteString(m.theme.Paint(ui.Paper, cmd))
		b.WriteString(strings.Repeat(" ", max(22-lipgloss.Width(cmd), 2)))
		b.WriteString(m.theme.Paint(ui.PaperSoft, g.about))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.theme.Paint(ui.PaperSoft, "enter send · ctrl+j newline · esc · ctrl+c abort · tab completes"))
	return b.String()
}

// helpGroupBody is /help <group>: the full list for one group in the existing
// aligned style. An unknown group says so and names the valid ones.
func (m *Model) helpGroupBody(name string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	// "/help /context" and "/help context" both work: an operator who copies a
	// command out of the list should not be told it does not exist.
	want = strings.TrimPrefix(want, "/")
	for _, g := range helpGroups() {
		if g.name == want {
			return strings.Join(g.lines, "\n"), true
		}
	}
	var names []string
	for _, g := range helpGroups() {
		names = append(names, g.name)
	}
	sort.Strings(names)
	return "no group " + strconv.Quote(want) + " · try " + strings.Join(names, ", "), false
}

// helpCommandNames is every documented command, for the drift test. The first
// field is the command with any argument placeholder stripped, so
// "/help [group]" contributes "/help".
func helpCommandNames() []string {
	var out []string
	for _, g := range helpGroups() {
		for _, line := range g.lines {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				out = append(out, fields[0])
			}
		}
	}
	return out
}
