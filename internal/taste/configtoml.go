package taste

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// This file reads the `[taste]` table out of kintsugi's own config files.
//
// Why it exists: internal/config/config.go has no Taste field yet, and that
// file is coordinator-owned this wave. Rather than ship a package that can
// only be switched off by an environment variable, the store reads the same
// TOML the rest of kintsugi reads — defaults, then ~/.config/kintsugi/config.toml,
// then <ws>/kintsugi.toml, later files winning per field. The moment
// config.File grows the Taste struct documented in config.go, the coordinator
// replaces ConfigFromFile's callers with a field-by-field mapping and this
// file can be deleted unchanged; nothing else in the package depends on it.
//
// It uses github.com/BurntSushi/toml, which go.mod already requires (it is
// internal/config's decoder), so no dependency is added.

// tomlTaste mirrors the `[taste]` table. Every field is optional and the
// zero value never means "off": Enabled and RecordEdits are pointers so an
// omitted key stays at its default (true), exactly as config.Advisor does.
type tomlTaste struct {
	Enabled      *bool  `toml:"enabled"`
	Dir          string `toml:"dir"`
	GlobalDir    string `toml:"global_dir"`
	BudgetTokens int    `toml:"budget_tokens"`
	MaxEntries   int    `toml:"max_entries"`
	RecordEdits  *bool  `toml:"record_edits"`
}

type tomlDoc struct {
	Taste tomlTaste `toml:"taste"`
}

// ConfigFromBytes decodes one TOML document's `[taste]` table. A document with
// no `[taste]` table yields the defaults, not an error.
func ConfigFromBytes(data []byte) (Config, error) {
	var doc tomlDoc
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return Config{}, fmt.Errorf("taste: config: %w", err)
	}
	return fromTOML(doc.Taste), nil
}

// ConfigFromFile decodes the `[taste]` table from the first of paths that can
// be read, and returns the loaded paths. Later paths win per field, matching
// config.Load's precedence. Unreadable paths are skipped: a taste
// configuration is never a reason for a command to fail.
func ConfigFromFile(paths ...string) (Config, []string, error) {
	cfg := DefaultConfig()
	var loaded []string
	for _, p := range paths {
		p = strings.TrimSpace(expandHome(p))
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue // absent or unreadable: the defaults stand
		}
		var doc tomlDoc
		if _, err := toml.Decode(string(data), &doc); err != nil {
			return cfg, loaded, fmt.Errorf("taste: config %s: %w", p, err)
		}
		cfg = cfg.override(fromTOML(doc.Taste))
		loaded = append(loaded, p)
	}
	return cfg, loaded, nil
}

func fromTOML(t tomlTaste) Config {
	return Config{
		Enabled:      t.Enabled,
		Dir:          strings.TrimSpace(t.Dir),
		GlobalDir:    strings.TrimSpace(t.GlobalDir),
		BudgetTokens: t.BudgetTokens,
		MaxEntries:   t.MaxEntries,
		RecordEdits:  t.RecordEdits,
	}
}

// ConfigForWorkspace resolves the effective `[taste]` block for a workspace:
// the global config file, then the workspace's kintsugi.toml, then
// $KINTSUGI_CONFIG — the same candidate order and precedence config.Load uses.
// It deliberately does not import internal/config, so the package stays usable
// from anywhere (and cycle-free once config.File grows a Taste field).
func ConfigForWorkspace(ws string) (Config, []string, error) {
	var paths []string
	if global := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); global != "" {
		paths = append(paths, filepath.Join(global, "kintsugi", "config.toml"))
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".config", "kintsugi", "config.toml"))
	}
	if ws = strings.TrimSpace(ws); ws != "" {
		paths = append(paths, filepath.Join(ws, "kintsugi.toml"))
	}
	if p := strings.TrimSpace(os.Getenv("KINTSUGI_CONFIG")); p != "" {
		paths = append(paths, p)
	}
	cfg, loaded, err := ConfigFromFile(paths...)
	if err != nil {
		return DefaultConfig(), loaded, err
	}
	return cfg.WithEnv(), loaded, nil
}

// StoreFor resolves the store a caller should use for a workspace: the config
// files' `[taste]` block plus the environment's overrides. It is the one-call
// entry point for cmd/kintsugi and for the coordinator's app.go hook.
func StoreFor(ws string) *Store {
	cfg, _, err := ConfigForWorkspace(ws)
	if err != nil {
		// A malformed [taste] table must not take a session down: fall back to
		// the defaults and let `kintsugi taste lint` / `kintsugi doctor` complain.
		cfg = DefaultConfig().WithEnv()
	}
	return Open(ws, cfg)
}

// override merges other into c field by field. Only fields other actually
// sets win, so a later config file that mentions `[taste]` for one key cannot
// silently re-enable or re-default the keys it does not mention.
func (c Config) override(other Config) Config {
	if other.Enabled != nil {
		c.Enabled = other.Enabled
	}
	if other.Dir != "" {
		c.Dir = other.Dir
	}
	if other.GlobalDir != "" {
		c.GlobalDir = other.GlobalDir
	}
	if other.BudgetTokens > 0 {
		c.BudgetTokens = other.BudgetTokens
	}
	if other.MaxEntries > 0 {
		c.MaxEntries = other.MaxEntries
	}
	if other.RecordEdits != nil {
		c.RecordEdits = other.RecordEdits
	}
	return c
}
