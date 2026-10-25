// Package config loads AgentForge configuration.
//
// Precedence (later wins):
//  1. built-in defaults (this file)
//  2. ~/.config/kintsugi/config.toml
//  3. ./kintsugi.toml            (workspace)
//  4. env overrides (KINTSUGI_*)
//
// Secrets are never read from config: provider entries name the environment
// variable (api_key_env) and the key is read from the process env at request
// time.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type File struct {
	Core        Core                  `toml:"core"`
	PersonaPack PersonaPack           `toml:"persona_pack"`
	Context     Context               `toml:"context"`
	Harness     Harness               `toml:"harness"`
	Budget      Budget                `toml:"budget"`
	Behavior    Behavior              `toml:"behavior"`
	Permissions Permissions           `toml:"permissions"`
	Intercept   Intercept             `toml:"intercept"`
	UI          UI                    `toml:"ui"`
	Advisor     Advisor               `toml:"advisor"`
	Agents      Agents                `toml:"agents"`
	Gates       Gates                 `toml:"gates"`
	Roles       Roles                 `toml:"roles"`
	Quota       map[string]QuotaLimit `toml:"quota"`
	Providers   map[string]Provider   `toml:"providers"`
	Chains      map[string]Chain      `toml:"chains"`
	Workspace   Workspace             `toml:"workspace"`
	Skills      Skills                `toml:"skills"`
	MCP         MCP                   `toml:"mcp"`
	// Mods configures the constrained extension API (M12.5): external
	// processes that contribute tools and slash commands behind the
	// workspace trust marker.
	Mods      Mods      `toml:"mods"`
	Retention Retention `toml:"retention"`
	// Notify is the terminal notification posture (M10-D). Off by default.
	Notify Notify `toml:"notify"`
	// ACP is the external-agent adapter posture (M12-A). The zero value is the
	// recommended configuration, so an absent section changes nothing.
	ACP        ACP        `toml:"acp"`
	Continuity Continuity `toml:"continuity"`
	Hooks      Hooks      `toml:"hooks"`
	// Taste is the local-learning posture (M10-B). It is prompt text only and
	// can never widen a permission.
	Taste Taste `toml:"taste"`
	// Research configures the M8 research surface: the web-search backend
	// chain and the security-database lookups.
	Research Research `toml:"research"`
	// WorkspaceHooks are hooks that came from the workspace's kintsugi.toml.
	// That file is repository-controlled, so these only run behind the
	// project's trust marker (kintsugi hooks trust).
	WorkspaceHooks Hooks `toml:"-"`
	// Sandbox is the OS-level isolation posture for shell commands (M11.1).
	Sandbox Sandbox `toml:"sandbox"`
	// Scope is the operator-declared engagement scope (M11.2). It is
	// machine-readable, so the tool layer can enforce it instead of
	// trusting the model to remember it.
	Scope Scope `toml:"scope"`
	// Lab configures the disposable lab harness (M8 remainder, DESIGN §6.3):
	// the container runtime and the compose project the agent's commands run
	// in, never on the host.
	Lab Lab `toml:"lab"`
	// KBs binds named knowledge bases (DESIGN §3.1 layer 3) to roles. An empty
	// map means no knowledge is injected at compile time.
	KBs map[string]KB `toml:"kb"`
}

// Config is the loaded configuration document. It is an alias of File, so
// a caller can talk about "the config" without naming the file type.
type Config = File

// KB is one `[kb.<name>]` binding: where the base lives, how its content is
// injected, and which roles it is bound to.
type KB struct {
	Path        string   `toml:"path"`         // dir, relative to workspace or absolute
	Strategy    string   `toml:"strategy"`     // retrieval | always | on-demand | summary
	TopK        int      `toml:"top_k"`        // 0 = 8
	MustInclude []string `toml:"must_include"` // chunk ids forced ahead of retrieval hits
	Roles       []string `toml:"roles"`        // empty = all roles
}

type Core struct {
	Mode         string `toml:"mode"` // offline | normal
	DefaultModel string `toml:"default_model"`
	CatalogPath  string `toml:"catalog_path"` // empty = ~/.config/kintsugi/catalog.json
}

type PersonaPack struct {
	Dir        string   `toml:"dir"`
	Fallback   string   `toml:"fallback"`
	MatchOrder []string `toml:"match_order"`
}

type Context struct {
	Enabled             bool     `toml:"enabled"`
	Path                string   `toml:"path"` // relative to workspace
	InjectBudgetTokens  int      `toml:"inject_budget_tokens"`
	CuratorModel        string   `toml:"curator_model"` // provider/model; empty disables curation
	CuratorOnSessionEnd bool     `toml:"curator_on_session_end"`
	AutoAcceptClasses   []string `toml:"auto_accept_classes"`
	GatedClasses        []string `toml:"gated_classes"`
}

type Harness struct {
	ToolCallRepair  bool `toml:"tool_call_repair"`
	RepairMax       int  `toml:"tool_call_repair_max"`
	OneToolPerTurn  bool `toml:"one_tool_per_turn"`
	MaxToolLoops    int  `toml:"max_tool_loops"`
	RequestTimeoutS int  `toml:"request_timeout_s"`
	// CompactAfter is the message count above which the older turns are
	// summarized before the next turn (M10.3). 0 = default, negative = off.
	CompactAfter int `toml:"compact_after"`
	// CompactKeepRatio is the share of the conversation kept verbatim.
	CompactKeepRatio float64 `toml:"compact_keep_ratio"`
}

type Budget struct {
	MaxPromptTokens int `toml:"max_prompt_tokens"`   // total budget for layers 0-5
	TaskReserve     int `toml:"task_reserve_tokens"` // reserved for the task + conversation
}

type Behavior struct {
	Dir            string `toml:"dir"`
	DefaultProfile string `toml:"default_profile"`
}

// Skills configures the progressive-disclosure skill directory.
type Skills struct {
	Dir string `toml:"dir"` // relative to workspace; default .kintsugi/skills
}

// Research configures the M8 research surface (IDEAS I-19): the web-search
// backend chain and the security-database lookups.
//
// No secret is stored here. As with the providers, a key-bearing backend
// names the environment variable that holds its key and the key is read from
// the process environment at request time, so a config file never carries a
// credential.
type Research struct {
	// SearchBackends is the ordered chain of web-search backends. The first
	// backend that answers wins; the rest are fallbacks. The chain chooses
	// which providers are reached, never where they live.
	SearchBackends []string `toml:"search_backends"`
	// BraveAPIKeyEnv names the environment variable holding the Brave Search
	// API key. Empty means "BRAVE_SEARCH_API_KEY".
	BraveAPIKeyEnv string `toml:"brave_api_key_env"`
	// TavilyAPIKeyEnv names the environment variable holding the Tavily API
	// key. Empty means "TAVILY_API_KEY".
	TavilyAPIKeyEnv string `toml:"tavily_api_key_env"`
	// TimeoutS bounds one research request in seconds. Empty means 15.
	TimeoutS int `toml:"timeout_s"`
}

// DefaultSearchBackends is the built-in search chain: a keyless backend
// first, then the two keyed ones. A keyless install therefore works out of
// the box, and an operator who has a key can move it to the front.
var DefaultSearchBackends = []string{"duckduckgo", "brave", "tavily"}

// Backends returns the configured backend chain in order. An empty
// configuration falls back to the built-in chain.
func (r Research) Backends() []string {
	if len(r.SearchBackends) == 0 {
		return append([]string(nil), DefaultSearchBackends...)
	}
	return append([]string(nil), r.SearchBackends...)
}

// BraveKeyEnv returns the environment variable holding the Brave key.
func (r Research) BraveKeyEnv() string {
	if v := strings.TrimSpace(r.BraveAPIKeyEnv); v != "" {
		return v
	}
	return "BRAVE_SEARCH_API_KEY"
}

// TavilyKeyEnv returns the environment variable holding the Tavily key.
func (r Research) TavilyKeyEnv() string {
	if v := strings.TrimSpace(r.TavilyAPIKeyEnv); v != "" {
		return v
	}
	return "TAVILY_API_KEY"
}

// Timeout returns the per-request timeout, bounded to a sane range.
func (r Research) Timeout() time.Duration {
	secs := r.TimeoutS
	if secs <= 0 {
		secs = 15
	}
	if secs > 120 {
		secs = 120
	}
	return time.Duration(secs) * time.Second
}

// MCP configures Model Context Protocol servers. Tool names are exposed as
// mcp__<server>__<tool> and pass through the normal permission gate.
//
// Two transports are supported. "stdio" (the default) runs a local process
// and speaks one JSON-RPC message per line. "http" speaks the streamable
// HTTP shape: one POST per message, answered with a JSON body or an SSE
// stream. Servers are isolated from each other: one that is down, slow or
// broken disables only itself.
type MCP struct {
	Enabled bool                 `toml:"enabled"`
	Servers map[string]MCPServer `toml:"servers"`
}

type MCPServer struct {
	Command  string            `toml:"command"`
	Args     []string          `toml:"args"`
	Env      map[string]string `toml:"env"`
	CWD      string            `toml:"cwd"`
	TimeoutS int               `toml:"timeout_s"` // default 60
	// AllowTools restricts which of the server's discovered tools reach the
	// model. Patterns are glob-ish: "web_search" matches that tool, "mcp__x__*"
	// matches every tool of server x. An empty list allows every tool the
	// server advertises, which is the default behaviour.
	AllowTools []string `toml:"allow_tools"`
	// Transport selects the wire: "stdio" (the default) or "http". An HTTP
	// server needs no command; it needs a url.
	Transport string `toml:"transport"`
	// URL is the endpoint of an HTTP server (streamable HTTP). It is used
	// only when transport = "http".
	URL string `toml:"url"`
	// BearerEnv names the environment variable holding the bearer token.
	// The token itself never appears in the config; an unset variable is an
	// error, not a silent anonymous request.
	BearerEnv string `toml:"bearer_env"`
	// HeadersEnv maps a header name to the environment variable holding its
	// value (for example {"X-Api-Key" = "DOCS_API_KEY"}). As with the bearer
	// token, the config carries names only, never values.
	HeadersEnv map[string]string `toml:"headers_env"`
}

// Mods configures the constrained extension API (M12.5).
//
//   - dir: where the mod manifests live, relative to the workspace. The
//     default is .kintsugi/mods, inside the repository, so the mods are
//     repository-controlled and only run behind the workspace trust marker
//     (`kintsugi mods trust`, the same marker as the project hooks).
//   - enabled: whether mods are looked for at all. It defaults to false:
//     a mod is code from outside the harness, so it is opt-in.
//
// A mod is an external process that speaks newline-delimited JSON-RPC on
// stdin/stdout. There is no in-process script runtime and no dynamic
// library loading: the process boundary is the trust boundary.
type Mods struct {
	Dir     string `toml:"dir"`     // workspace-relative; default .kintsugi/mods
	Enabled bool   `toml:"enabled"` // default false
}

type Workspace struct {
	Profile string `toml:"profile"`
	Network string `toml:"network"`
}

// Permissions is the per-profile declarative tool policy (DESIGN §6.5).
type Permissions struct {
	AllowPaths    []string `toml:"allow_paths"`
	DenyPaths     []string `toml:"deny_paths"`
	AllowCommands []string `toml:"allow_commands"`
	DenyCommands  []string `toml:"deny_commands"`
	Yolo          bool     `toml:"yolo"`
	// Mode is the session permission mode: default | accept-edits | plan |
	// yolo | dont-ask. Rules are evaluated after the mode baseline.
	Mode string `toml:"mode"`
	// Rules are evaluated in order, last match wins: deny -> ask -> allow.
	Rules []PermRule `toml:"rules"`
	// ClassifierMode gates the two-stage permission classifier (A1): off | soft
	// | hard. off (the default) is byte-identical to the pre-classifier
	// behaviour; soft lets a model-backed classifier block refusal-adjacent
	// calls; hard runs the full stage-1/stage-2 rescue. The classifier only
	// ever tightens a decision, never widens it.
	ClassifierMode string `toml:"classifier_mode"`
}

// PermRule is one entry of the permission matrix. Tool matches a tool name
// (case-insensitive, `*` wildcards allowed); Spec narrows a shell command or a
// file path; Action is allow | ask | deny.
type PermRule struct {
	Tool   string `toml:"tool"`
	Spec   string `toml:"spec"`
	Action string `toml:"action"`
}

// Taste configures local taste learning (M10-B, DESIGN §16.15). It is prompt
// text only: nothing here can widen a permission.
type Taste struct {
	// Enabled nil means the default: on.
	Enabled *bool `toml:"enabled"`
	// Dir is the project taste directory. Empty means .kintsugi/taste.
	Dir string `toml:"dir"`
	// GlobalDir is the operator-global taste directory. Empty means
	// ~/.config/kintsugi/taste.
	GlobalDir string `toml:"global_dir"`
	// BudgetTokens bounds the injected block. 0 means the default.
	BudgetTokens int `toml:"budget_tokens"`
	// MaxEntries bounds how many bullets one block carries. 0 means the default.
	MaxEntries int `toml:"max_entries"`
	// RecordEdits nil means the edit heuristic is on.
	RecordEdits *bool `toml:"record_edits"`
	// Background enables the background review fork (A4): after a turn a
	// detached run reviews a snapshot and writes lessons/skills through the
	// same gate, never touching the live transcript or cached prefix. Off by
	// default — it costs one extra model call per turn.
	Background bool `toml:"background"`
}

// Sandbox is the OS-level isolation posture for shell commands (M11.1,
// DESIGN §16.9). The permission layer answers "may the agent ask for this?";
// this block decides what the process can actually do.
//
//   - mode: off | readonly | workspace. "off" (the default) runs commands
//     without OS isolation; "readonly" binds the workspace read-only;
//     "workspace" binds it read-write and leaves the rest of the host
//     read-only.
//   - network: none | loopback | allowlist. "none" (the default when the
//     sandbox is on) removes the network namespace; "loopback" keeps only
//     the loopback interface; "allowlist" keeps the host network and relies
//     on the fetch layer's host guard (bubblewrap cannot filter by host).
//   - binary: the sandbox executable (default "bwrap", Linux only).
//   - extra_args: extra backend arguments, inserted before the command.
//
// An unavailable backend is never a silent fallback: with mode != "off" the
// tool layer refuses the command and names the reason.
type Sandbox struct {
	Mode      string   `toml:"mode"`       // off | readonly | workspace
	Network   string   `toml:"network"`    // none | loopback | allowlist
	Allowlist []string `toml:"allowlist"`  // hosts reachable when network = allowlist
	Binary    string   `toml:"binary"`     // backend executable (default: bwrap)
	ExtraArgs []string `toml:"extra_args"` // extra backend arguments
}

// Scope is the operator-declared engagement scope (DESIGN §6.1, M11.2).
// The scope is injected into the prompt as context and read here as a rule
// set the tool layer enforces.
//
//   - paths: workspace-relative globs in scope ("src/**", "*.md").
//   - commands: commands in scope ("go test", "git*").
//   - hosts: hosts in scope ("example.com", "*.lab.example").
//   - out_of_scope: explicit deny list; it always wins.
//
// An empty scope declares nothing, so nothing is narrowed: a path, command
// or host outside a *declared* rule set is denied.
type Scope struct {
	Paths      []string `toml:"paths"`
	Commands   []string `toml:"commands"`
	Hosts      []string `toml:"hosts"`
	OutOfScope []string `toml:"out_of_scope"`
}

// Lab is the lab-harness block (M8 remainder, DESIGN §6.3): the disposable
// fixture environment the agent's commands run in.
//
//   - runtime: auto | docker | podman. "auto" (the default) detects docker
//     first, then podman.
//   - compose_file: the compose file, relative to the workspace. The default
//     is "lab/docker-compose.yml".
//   - project: the compose project name. The default is "kintsugi-lab".
//   - services: the services to bring up. Empty means every service the
//     compose file declares.
//   - network: the lab network name, recorded in the lab lock.
//
// These values are the operator's: the harness never takes a compose file
// path or a project name from a tool argument.
type Lab struct {
	Runtime     string   `toml:"runtime"`      // auto | docker | podman
	ComposeFile string   `toml:"compose_file"` // relative to the workspace
	Project     string   `toml:"project"`      // compose project name
	Services    []string `toml:"services"`     // services to bring up
	Network     string   `toml:"network"`      // lab network name
}

// Hooks configures lifecycle hooks (M9.1). Each event holds command hooks; a
// hook reads the event JSON on stdin and answers on stdout.
type Hooks struct {
	SessionStart     []HookRule `toml:"SessionStart"`
	UserPromptSubmit []HookRule `toml:"UserPromptSubmit"`
	PreToolUse       []HookRule `toml:"PreToolUse"`
	PostToolUse      []HookRule `toml:"PostToolUse"`
	Stop             []HookRule `toml:"Stop"`
	PreCompact       []HookRule `toml:"PreCompact"`
	PostCompact      []HookRule `toml:"PostCompact"`
	SubagentStart    []HookRule `toml:"SubagentStart"`
	SubagentStop     []HookRule `toml:"SubagentStop"`
	Notification     []HookRule `toml:"Notification"`
	ModelSwitch      []HookRule `toml:"ModelSwitch"`
}

// HookRule is one command hook. Matcher narrows the event (a tool name for
// tool events); Command is the executable; Args is passed as the argv (no
// shell); Timeout bounds the run; Async detaches it; OnFailure is
// continue | block.
type HookRule struct {
	Matcher   string   `toml:"matcher"`
	Command   string   `toml:"command"`
	Args      []string `toml:"args"`
	Timeout   int      `toml:"timeout"`
	Async     bool     `toml:"async"`
	OnFailure string   `toml:"on_failure"`
}

// For returns the hooks registered for an event.
func (h Hooks) For(event string) []HookRule {
	switch event {
	case "SessionStart":
		return h.SessionStart
	case "UserPromptSubmit":
		return h.UserPromptSubmit
	case "PreToolUse":
		return h.PreToolUse
	case "PostToolUse":
		return h.PostToolUse
	case "Stop":
		return h.Stop
	case "PreCompact":
		return h.PreCompact
	case "PostCompact":
		return h.PostCompact
	case "SubagentStart":
		return h.SubagentStart
	case "SubagentStop":
		return h.SubagentStop
	case "Notification":
		return h.Notification
	case "ModelSwitch":
		return h.ModelSwitch
	}
	return nil
}

// UI holds TUI behavior switches (DESIGN §7.4).
type UI struct {
	Banner    bool `toml:"banner"`     // static nameplate on session start
	AltScreen bool `toml:"alt_screen"` // false keeps output in scrollback
	Ornament  bool `toml:"ornament"`   // JP footer ornament
	Plain     bool `toml:"plain"`      // force the no-TTY stream REPL
	Mouse     bool `toml:"mouse"`      // mouse-wheel scrolling; --no-mouse disables capture
}

// ACP configures the external-agent adapter (M12-A) that a DAG's `acp:` nodes
// drive. Every field is optional and the zero value is the recommended posture:
// the adapter is on, timeouts are the package defaults, and the observer is
// read-only. The adapter runs a separate agent process and observes its tool
// calls — it never executes on the agent's behalf, and nothing here reads the
// harness's own credentials.
type ACP struct {
	// Off disables the adapter entirely. An `acp:` node then goes to the normal
	// runner, which fails it as an unknown role rather than spawning anything —
	// a clear failure rather than a silent no-op.
	Off bool `toml:"off"`
	// Timeout bounds the handshake and each control request, as a Go duration
	// ("30s"). Empty means the package default.
	Timeout string `toml:"timeout"`
	// PromptTimeout bounds one prompt turn. Empty means the package default.
	PromptTimeout string `toml:"prompt_timeout"`
	// CancelGrace is how long a cancelled node waits after session/cancel before
	// the agent process is killed. Empty means the package default.
	CancelGrace string `toml:"cancel_grace"`
}

// Notify configures the terminal bell/notification backend (M10-D). It is off by
// default: a harness that pings the operator's desktop is a decision, not a
// default. Nothing here leaves the machine — osc/bell write to the operator's
// own terminal, and the command backend runs one named executable with the
// message as argv, never through a shell.
type Notify struct {
	Backend string `toml:"backend"` // off | osc9 | osc777 | bell | command
	Command string `toml:"command"` // for backend=command: one executable
}

// Advisor is the during-the-work secondary reviewer (DESIGN §5, I-17).
// Enabled nil means "profile default": on for lab, off otherwise.
type Advisor struct {
	Enabled   *bool  `toml:"enabled"`
	Model     string `toml:"model"`      // provider/model; empty = same as worker
	MaxTokens int    `toml:"max_tokens"` // approximate session cap
}

// Active resolves the profile default when enabled is unset.
func (a Advisor) Active(profile string) bool {
	if a.Enabled != nil {
		return *a.Enabled
	}
	return profile == "lab"
}

// Agents configures subagent fan-out (DESIGN §5.2).
type Agents struct {
	Subagents    bool   `toml:"subagents"`
	MaxSubagents int    `toml:"max_subagents"`
	WorktreeDir  string `toml:"worktree_dir"` // empty = os temp
	TimeoutS     int    `toml:"timeout_s"`
}

// Gates configures the opsec-review gate (DESIGN §5.1).
type Gates struct {
	Model        string `toml:"model"` // empty = default model
	Blocking     bool   `toml:"blocking"`
	MaxRevisions int    `toml:"max_revisions"`
}

// Roles binds model roles to (model, persona) pairs (DESIGN §8.4, I-23).
// An empty model falls back to core.default_model; an empty persona uses the
// pack's normal matching.
type Roles struct {
	Default    string            `toml:"default"`
	Coder      string            `toml:"coder"`
	Researcher string            `toml:"researcher"`
	Opsec      string            `toml:"opsec"`
	Personas   map[string]string `toml:"personas"`
}

func (r Roles) Model(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "coder":
		return r.Coder
	case "researcher":
		return r.Researcher
	case "opsec", "reviewer":
		return r.Opsec
	default:
		return r.Default
	}
}

func (r Roles) Persona(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = "default"
	}
	return r.Personas[role]
}

// QuotaLimit configures the offline quota ledger for one provider.
type QuotaLimit struct {
	LimitTokens int64  `toml:"limit_tokens"`
	Reset       string `toml:"reset"` // daily | weekly | monthly | none
	Note        string `toml:"note"`
	// Live asks for a best-effort live quota probe (provider /usage or
	// /user/balance) in addition to the local ledger. The ledger stays
	// authoritative: a failed probe never invalidates it.
	Live bool `toml:"live"`
}

// Chain is an ordered fallback list for one model ref (§ WP-4.2). Slot 0 is
// the primary; the rest are tried in order when a slot fails retryably.
// Each slot is a normal `provider/model` ref resolved by ResolveModel.
type Chain struct {
	Providers []string `toml:"providers"`
}

// Retention bounds the on-disk session store. The zero value keeps every
// session forever: both policies are opt-in, and the sweep is report-only
// unless the operator passes --apply.
type Retention struct {
	MaxSessions int `toml:"max_sessions"` // keep the newest N; 0 = unlimited
	MaxAgeDays  int `toml:"max_age_days"` // delete sessions older than N days; 0 = keep forever
}

// Continuity configures the local Context Ledger and prior-work recall
// (M8.5). A refusal is never memory: these settings only govern positive,
// operator-accepted prior work and the preflight frame.
type Continuity struct {
	Enabled               bool   `toml:"enabled"`
	PriorWorkTopK         int    `toml:"prior_work_top_k"`
	PriorWorkBudgetTokens int    `toml:"prior_work_budget_tokens"`
	Preflight             bool   `toml:"preflight"`
	GuardOnRefusal        bool   `toml:"guard_on_refusal"`
	LedgerDir             string `toml:"ledger"` // relative to workspace; default .kintsugi/ledger
}

// Intercept is stream-level refusal interception (DESIGN §6.7).
type Intercept struct {
	Enabled     bool   `toml:"enabled"`
	MaxRetries  int    `toml:"max_retries"`
	LexiconPath string `toml:"lexicon_path"` // TOML overrides; empty = built-ins
	// HardStop classifies a refusal that will not move (the same class again
	// in one turn) and stops retrying: a reminder does not shift a stonewall.
	HardStop bool `toml:"hard_stop"`
	// HardThinking classifies hard from absolute language in the reasoning,
	// before the second retry. HardStop must be on for this to matter.
	HardThinking bool `toml:"hard_thinking"`
	// RulesPath is the operator stream-rule file (I-70): regex rules with an
	// abort, redact or inject action. Empty means the refusal lexicon alone,
	// which is the behaviour every session had before rules existed.
	RulesPath string `toml:"rules_path"`
}

type Provider struct {
	BaseURL         string  `toml:"base_url" json:"base_url"`
	APIKeyEnv       string  `toml:"api_key_env" json:"api_key_env"`
	Family          string  `toml:"family" json:"family"`
	Wire            string  `toml:"wire" json:"wire"` // openai-completions | anthropic-messages
	Offline         bool    `toml:"offline" json:"offline"`
	ReasoningEffort string  `toml:"reasoning_effort" json:"reasoning_effort"` // e.g. "none" to disable thinking on local models
	Models          []Model `toml:"models" json:"models"`
}

type Model struct {
	ID           string   `toml:"id" json:"id"`
	Family       string   `toml:"family" json:"family"`
	Wire         string   `toml:"wire" json:"wire"`
	Context      int      `toml:"context" json:"context"`
	Reasoning    bool     `toml:"reasoning" json:"reasoning"`
	Capabilities []string `toml:"capabilities" json:"capabilities"`
}

// UnmarshalTOML accepts both `models = ["a", "b"]` and table arrays.
func (m *Model) UnmarshalTOML(v any) error {
	switch t := v.(type) {
	case string:
		m.ID = t
		return nil
	case map[string]any:
		if s, ok := t["id"].(string); ok {
			m.ID = s
		}
		if s, ok := t["family"].(string); ok {
			m.Family = s
		}
		if s, ok := t["wire"].(string); ok {
			m.Wire = s
		}
		if n, ok := t["context"].(int64); ok {
			m.Context = int(n)
		}
		if b, ok := t["reasoning"].(bool); ok {
			m.Reasoning = b
		}
		if arr, ok := t["capabilities"].([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok {
					m.Capabilities = append(m.Capabilities, s)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("model entry must be a string or a table, got %T", v)
	}
}

func Defaults() File {
	return File{
		Core: Core{Mode: "offline", DefaultModel: "deepseek/deepseek-chat"},
		PersonaPack: PersonaPack{
			Dir:      "~/.config/kintsugi/agents/global",
			Fallback: "_base",
			MatchOrder: []string{
				"explicit", "id_glob", "family", "capability", "fallback",
			},
		},
		Context: Context{
			Enabled: true, Path: "CONTEXT.md", InjectBudgetTokens: 1200,
			AutoAcceptClasses: []string{"state", "convention", "lesson"},
			GatedClasses:      []string{"authorization", "scope"},
		},
		Harness: Harness{
			ToolCallRepair:   true,
			RepairMax:        2,
			OneToolPerTurn:   true,
			MaxToolLoops:     8,
			RequestTimeoutS:  300,
			CompactAfter:     24,
			CompactKeepRatio: 0.4,
		},
		Budget: Budget{MaxPromptTokens: 24000, TaskReserve: 4000},
		Behavior: Behavior{
			Dir:            "~/.config/kintsugi/agents/behavior",
			DefaultProfile: "",
		},
		Intercept: Intercept{Enabled: true, MaxRetries: 2, HardStop: true, HardThinking: true},
		Continuity: Continuity{
			Enabled:               true,
			PriorWorkTopK:         5,
			PriorWorkBudgetTokens: 600,
			Preflight:             true,
			GuardOnRefusal:        true,
			LedgerDir:             ".kintsugi/ledger",
		},
		UI: UI{
			Banner: true, AltScreen: true, Ornament: true, Mouse: true,
		},
		Advisor: Advisor{MaxTokens: 20000},
		Agents:  Agents{Subagents: true, MaxSubagents: 3, TimeoutS: 300},
		Gates:   Gates{MaxRevisions: 1},
		Skills:  Skills{Dir: ".kintsugi/skills"},
		MCP:     MCP{Enabled: true},
		// Mods are off until the operator asks for them: a mod is code from
		// outside the harness. The directory is inside the workspace, so the
		// mods are repository-controlled and run only behind the trust
		// marker.
		Mods: Mods{Dir: ".kintsugi/mods", Enabled: false},
		// The sandbox is opt-in: with mode "off" commands run unsandboxed,
		// exactly as before M11.1. When it is switched on, the network
		// posture defaults to "none" (no egress).
		Sandbox: Sandbox{Mode: "off", Network: "none"},
		// An empty scope declares no scope, so nothing is narrowed.
		Scope: Scope{},
		// The lab harness: a container runtime and a compose project. The
		// runtime is detected unless the operator names one.
		Lab: Lab{
			Runtime:     "auto",
			ComposeFile: "lab/docker-compose.yml",
			Project:     "kintsugi-lab",
		},
		Research: Research{
			SearchBackends:  append([]string(nil), DefaultSearchBackends...),
			BraveAPIKeyEnv:  "BRAVE_SEARCH_API_KEY",
			TavilyAPIKeyEnv: "TAVILY_API_KEY",
			TimeoutS:        15,
		},
		Permissions: Permissions{
			DenyPaths: []string{"**/.env", "**/.env.*", "**/id_rsa", "**/id_ed25519", "**/*.pem", "**/.git/**"},
			DenyCommands: []string{
				`rm\s+-[a-zA-Z]*[rf][a-zA-Z]*\s+/(\s|$)`,
				`\bmkfs(\.|\s)`,
				`\bdd\s+if=`,
				`:\(\)\s*\{`,
			},
		},
		Providers: defaultProviders(),
	}
}

// Load merges defaults, global config, workspace config and env overrides.
func Load(workspaceDir string) (File, []string, error) {
	cfg := Defaults()
	var loaded []string
	// operatorHooks are the hooks from the operator's own config files. Hooks
	// that come from a workspace config are repository-controlled and are
	// tracked separately (cfg.WorkspaceHooks) behind the trust marker.
	operatorHooks := cfg.Hooks

	candidates := []string{GlobalPath()}
	if workspaceDir != "" {
		candidates = append(candidates, filepath.Join(workspaceDir, "kintsugi.toml"))
	}
	if p := os.Getenv("KINTSUGI_CONFIG"); p != "" {
		candidates = append(candidates, p)
	}

	for _, p := range candidates {
		p = expandHome(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// A workspace config is repository-controlled: its [hooks] section
		// runs commands the repo chooses, so it is kept apart from the
		// operator's hooks and only runs behind an explicit trust marker.
		if isWorkspaceConfig(p, workspaceDir) {
			var ws File
			if _, err := toml.DecodeFile(p, &ws); err != nil {
				return cfg, loaded, fmt.Errorf("config %s: %w", p, err)
			}
			wsHooks := ws.Hooks
			if _, err := toml.DecodeFile(p, &cfg); err != nil {
				return cfg, loaded, fmt.Errorf("config %s: %w", p, err)
			}
			cfg.Hooks = operatorHooks
			cfg.WorkspaceHooks = wsHooks
		} else {
			if _, err := toml.DecodeFile(p, &cfg); err != nil {
				return cfg, loaded, fmt.Errorf("config %s: %w", p, err)
			}
			operatorHooks = cfg.Hooks
		}
		loaded = append(loaded, p)
	}

	if v := os.Getenv("KINTSUGI_MODE"); v != "" {
		cfg.Core.Mode = v
	}
	if v := os.Getenv("KINTSUGI_MODEL"); v != "" {
		cfg.Core.DefaultModel = v
	}
	if cfg.Core.Mode != "offline" && cfg.Core.Mode != "normal" {
		return cfg, loaded, fmt.Errorf("core.mode must be offline|normal, got %q", cfg.Core.Mode)
	}
	return cfg, loaded, nil
}

// isWorkspaceConfig reports whether a loaded config path is the workspace's own
// kintsugi.toml (repository-controlled) rather than an operator-level file.
func isWorkspaceConfig(path, workspaceDir string) bool {
	if workspaceDir == "" {
		return false
	}
	return filepath.Clean(path) == filepath.Clean(filepath.Join(workspaceDir, "kintsugi.toml"))
}

func GlobalPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "kintsugi", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kintsugi", "config.toml")
}

// ResolveModel parses `provider/model` and returns the provider entry plus the
// effective model description. Unknown models on passthrough providers (no
// model list) are allowed; on curated providers they are an error so typos
// fail loudly instead of silently switching families.
func (f File) ResolveModel(ref string) (Provider, string, Model, error) {
	if strings.TrimSpace(ref) == "" {
		return Provider{}, "", Model{}, fmt.Errorf("no default model configured: set core.default_model, or run `kintsugi models fetch <provider> --write`")
	}
	provName, modelID, ok := strings.Cut(ref, "/")
	if !ok || provName == "" || modelID == "" {
		return Provider{}, "", Model{}, fmt.Errorf("model ref must be provider/model, got %q", ref)
	}
	p, ok := f.Providers[provName]
	if !ok {
		return Provider{}, "", Model{}, fmt.Errorf("unknown provider %q (configured: %s)", provName, strings.Join(f.ProviderNames(), ", "))
	}

	var m Model
	found := false
	for _, cand := range p.Models {
		if cand.ID == modelID {
			m, found = cand, true
			break
		}
	}
	if !found && len(p.Models) > 0 {
		return Provider{}, "", Model{}, fmt.Errorf("provider %q has no model %q (configured: %s)", provName, modelID, modelIDs(p.Models))
	}
	if !found {
		m = Model{ID: modelID}
	}

	// Fill from provider defaults.
	if m.Family == "" {
		m.Family = p.Family
	}
	if m.Family == "" {
		m.Family = inferFamily(modelID)
	}
	if m.Wire == "" {
		m.Wire = p.Wire
	}
	if m.Wire == "" {
		m.Wire = "openai-completions"
	}
	return p, provName, m, nil
}

func (f File) ProviderNames() []string {
	names := make([]string, 0, len(f.Providers))
	for k := range f.Providers {
		names = append(names, k)
	}
	return names
}

func inferFamily(id string) string {
	low := strings.ToLower(id)
	for _, fam := range []string{"deepseek", "qwen", "glm", "kimi", "minimax", "mimo", "llama", "mistral"} {
		if strings.Contains(low, fam) {
			return fam
		}
	}
	return ""
}

func modelIDs(ms []Model) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return strings.Join(ids, ", ")
}

func IsLoopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// APIKey returns the key for a provider from its named env var.
func (p Provider) APIKey() (string, error) {
	if p.APIKeyEnv == "" {
		return "", nil
	}
	v := os.Getenv(p.APIKeyEnv)
	if v == "" {
		return "", fmt.Errorf("provider needs %s (set it in the environment)", p.APIKeyEnv)
	}
	return v, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func ExpandHome(p string) string { return expandHome(p) }
