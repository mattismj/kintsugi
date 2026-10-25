package taste

import (
	"os"
	"strings"
)

// Config is the resolved `[taste]` block.
//
// The zero value is the default and is deliberately enabled: taste is local
// learning from the operator's own decisions, and an install that has not
// configured it still gets it. To turn it off an operator writes
// `[taste] enabled = false` in kintsugi.toml or in
// ~/.config/kintsugi/config.toml.
//
// Coordinator note (reported, not wired here — internal/config/config.go is
// not owned by this wave): the config package has no Taste field yet. The
// field this package expects is
//
//	// Taste configures local taste learning (M10-B, DESIGN §16.15). It is
//	// prompt text only: nothing here can widen a permission.
//	type Taste struct {
//	    Enabled      *bool  `toml:"enabled"`       // nil = true (the default)
//	    Dir          string `toml:"dir"`           // workspace-relative; default .kintsugi/taste
//	    GlobalDir    string `toml:"global_dir"`    // default ~/.config/kintsugi/taste
//	    BudgetTokens int    `toml:"budget_tokens"` // injected block cap; default 300
//	    MaxEntries   int    `toml:"max_entries"`   // bullets per block; default 12
//	    RecordEdits  *bool  `toml:"record_edits"`  // the edit heuristic; nil = true
//	}
//
// with `File` gaining `Taste Taste `toml:"taste"“, and `Defaults()` setting
// `Taste: Taste{BudgetTokens: 300, MaxEntries: 12}` — both pointers left nil,
// which means true. app.go then converts it field by field into a
// taste.Config (this package does not import internal/config, so there is no
// cycle either way).
//
// `Enabled` is a *bool rather than a bool because a TOML table that omits the
// key must not silently disable taste: a bool's zero value is false, so every
// operator who wrote `[taste] dir = "…"` for any other reason would lose the
// signals without asking for it. config.Advisor.Enabled already uses that
// shape for the same reason.
type Config struct {
	// Enabled nil means the default: on.
	Enabled *bool
	// Dir is the project taste directory. Empty means DefaultDir, resolved
	// against the workspace (absolute and "~/…" are honoured).
	Dir string
	// GlobalDir is the operator-global taste directory. Empty means
	// ~/.config/kintsugi/taste.
	GlobalDir string
	// BudgetTokens bounds the injected block. <= 0 means DefaultBudgetTokens.
	BudgetTokens int
	// MaxEntries bounds how many bullets one block carries. <= 0 means
	// DefaultMaxEntries.
	MaxEntries int
	// RecordEdits nil means the default: the edit heuristic is on.
	RecordEdits *bool
}

const (
	// DefaultBudgetTokens is the ~300-token cap WP M10-B names for the
	// injected block.
	DefaultBudgetTokens = 300
	// DefaultMaxEntries is how many bullets one block carries at most. The
	// newest win, so an overflowing block is a block about recent decisions.
	DefaultMaxEntries = 12
	// bytesPerToken is the same rough estimate internal/prompt and
	// internal/continuity use, which keeps a block's token count stable and
	// diffable.
	bytesPerToken = 4
)

// On reports whether taste is enabled. An unset Enabled means yes.
func (c Config) On() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// EditsOn reports whether the `edit` heuristic should fire. An unset
// RecordEdits means yes, matching Enabled's shape.
func (c Config) EditsOn() bool {
	if c.RecordEdits == nil {
		return true
	}
	return *c.RecordEdits
}

// Budget is the effective block budget in tokens.
func (c Config) Budget() int {
	if c.BudgetTokens <= 0 {
		return DefaultBudgetTokens
	}
	return c.BudgetTokens
}

// Entries is the effective per-block bullet cap.
func (c Config) Entries() int {
	if c.MaxEntries <= 0 {
		return DefaultMaxEntries
	}
	return c.MaxEntries
}

// WithEnabled returns a copy with Enabled set, for callers that would
// otherwise build a *bool by hand.
func (c Config) WithEnabled(on bool) Config {
	c.Enabled = &on
	return c
}

// WithEdits returns a copy with the edit heuristic explicitly on or off.
func (c Config) WithEdits(on bool) Config {
	c.RecordEdits = &on
	return c
}

// DefaultConfig is the built-in `[taste]` block: enabled, default paths, a
// 300-token block, edit heuristic on.
func DefaultConfig() Config {
	return Config{BudgetTokens: DefaultBudgetTokens, MaxEntries: DefaultMaxEntries}
}

// ---------- environment overrides ----------
//
// Two variables exist so taste can be relocated and switched off from a shell
// before the config field above is wired by the coordinator:
//
//	KINTSUGI_TASTE=off             disable every write (never enables)
//	KINTSUGI_TASTE_DIR=<dir>       the project taste directory
//	KINTSUGI_TASTE_GLOBAL_DIR=<d>  the global taste directory
//
// They only fill gaps: an explicit config value always wins, and
// KINTSUGI_TASTE can only turn taste off, so `enabled = false` in a config file
// can never be overridden back on by the environment.

// WithEnv returns a copy with the environment's overrides applied. It is what
// every Store consults, so the write path, the read path and the renderer
// always agree about where the store lives and whether it is live.
func (c Config) WithEnv() Config {
	if strings.TrimSpace(c.Dir) == "" {
		if v := strings.TrimSpace(os.Getenv("KINTSUGI_TASTE_DIR")); v != "" {
			c.Dir = v
		}
	}
	if strings.TrimSpace(c.GlobalDir) == "" {
		if v := strings.TrimSpace(os.Getenv("KINTSUGI_TASTE_GLOBAL_DIR")); v != "" {
			c.GlobalDir = v
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KINTSUGI_TASTE"))) {
	case "off", "false", "no", "0":
		off := false
		c.Enabled = &off
	}
	return c
}
