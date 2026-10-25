// Package credentials is the privacy-first local key store behind
// `kintsugi connect` and provider resolution (DESIGN §1.2).
//
// Design rules, in priority order:
//
//  1. Environment variables always win. A stored key is only a fallback; the
//     caller checks its provider's api_key_env first (see Status).
//  2. Keys are never written to config, prompts, logs, session files or error
//     messages. No function in this package returns a key inside an error, and
//     no argument is echoed back verbatim when it could be secret material.
//  3. The store is a single JSON file, 0600, inside a 0700 directory, replaced
//     atomically (temp file in the same directory + rename). It is never
//     rewritten when it cannot be parsed: a malformed file is an error the
//     operator must resolve, not a file to be silently truncated.
//
// Storage format:
//
//	{
//	  "version": 1,
//	  "keys": {
//	    "openrouter": "sk-or-..."
//	  }
//	}
//
// Provider names are trimmed but not lowercased: "OpenRouter" and
// "openrouter" are distinct entries. This matches provider names as they
// appear in config.toml, which are also case-sensitive.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Version is the on-disk schema version written by Save. Unknown future
// versions are refused rather than guessed at.
const Version = 1

// Store is the loaded key store. Keys maps provider name to API key.
type Store struct {
	Path string
	Keys map[string]string
}

// disk is the JSON shape of the store file.
type disk struct {
	Version int               `json:"version"`
	Keys    map[string]string `json:"keys"`
}

// Path is $XDG_CONFIG_HOME/kintsugi/credentials.json, or
// ~/.config/kintsugi/credentials.json when XDG_CONFIG_HOME is unset. It
// mirrors config.GlobalPath's directory logic without importing config (the
// config package must stay free of any secret-loading code path).
func Path() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "kintsugi", "credentials.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kintsugi", "credentials.json")
}

// Load reads the store. A missing file is an empty store, not an error. A file
// that exists but does not parse is an error: callers must not overwrite it,
// so no key is lost to a bad edit.
func Load() (*Store, error) {
	p := Path()
	s := &Store{Path: p, Keys: map[string]string{}}

	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, fmt.Errorf("credentials %s: cannot read store: %w", p, err)
	}

	var d disk
	if err := json.Unmarshal(b, &d); err != nil {
		// Deliberately no raw JSON text in the message: the file is expected to
		// contain secrets, and a malformed line is exactly where a partial key
		// would show up. Type/offset is enough to find the break.
		return s, malformed(p, err)
	}
	if d.Version > Version {
		return s, fmt.Errorf("credentials %s: unsupported store version %d (this build understands %d)",
			p, d.Version, Version)
	}
	for k, v := range d.Keys {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		s.Keys[k] = v
	}
	return s, nil
}

// malformed builds the parse error without echoing any of the file's content.
func malformed(path string, err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Errorf("credentials %s: malformed JSON at byte %d (file left untouched)", path, syn.Offset)
	}
	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		return fmt.Errorf("credentials %s: malformed JSON: field %q must be %s (file left untouched)",
			path, typ.Field, typ.Type)
	}
	return fmt.Errorf("credentials %s: malformed JSON (file left untouched)", path)
}

// Get returns the stored key for provider. It never consults the environment;
// callers check their provider's api_key_env first so the env always wins. An
// empty stored value counts as absent.
func (s *Store) Get(provider string) (string, bool) {
	if s == nil {
		return "", false
	}
	v, ok := s.Keys[strings.TrimSpace(provider)]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// Set stores key for provider using load-modify-write: existing entries are
// preserved, the file is written atomically with mode 0600 in a 0700
// directory, and a malformed store is an error rather than a rewrite target.
// The key is trimmed of surrounding whitespace; an empty key is an error.
func Set(provider, key string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return errors.New("credentials: provider name is required")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("credentials: empty key for provider %q (nothing written)", provider)
	}

	s, err := Load()
	if err != nil {
		return err
	}
	s.Keys[provider] = key
	return s.save()
}

// Delete removes provider's key. Deleting an absent provider is a no-op and
// does not create a file. When the last key goes, the store stays a valid
// empty document ({"version":1,"keys":{}}) rather than disappearing.
func Delete(provider string) error {
	provider = strings.TrimSpace(provider)
	s, err := Load()
	if err != nil {
		return err
	}
	if _, ok := s.Keys[provider]; !ok {
		return nil
	}
	delete(s.Keys, provider)
	return s.save()
}

// Status reports where a provider's key comes from for display: "env" when
// envName is set and non-empty in the environment, otherwise "stored" when the
// store holds a key, otherwise "missing". An empty envName means the provider
// has no environment option. An unreadable store reports "missing"; the error
// surfaces on the next Set or Load.
func Status(provider, envName string) string {
	if envName != "" && os.Getenv(envName) != "" {
		return "env"
	}
	s, err := Load()
	if err != nil {
		return "missing"
	}
	if _, ok := s.Get(provider); ok {
		return "stored"
	}
	return "missing"
}

// save writes the store atomically. The temp file is created in the target
// directory so the rename cannot cross filesystems.
func (s *Store) save() error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credentials %s: cannot create directory: %w", dir, err)
	}

	d := disk{Version: Version, Keys: s.Keys}
	if d.Keys == nil {
		d.Keys = map[string]string{}
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials %s: cannot encode store: %w", s.Path, err)
	}
	b = append(b, '\n')

	f, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("credentials %s: cannot create temp file: %w", s.Path, err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("credentials %s: cannot set mode 0600: %w", s.Path, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("credentials %s: cannot write: %w", s.Path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("credentials %s: cannot flush: %w", s.Path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("credentials %s: cannot close: %w", s.Path, err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("credentials %s: cannot replace store: %w", s.Path, err)
	}
	tmp = ""
	return nil
}
