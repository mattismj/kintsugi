package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempConfig points the store at a throwaway XDG config root and returns it.
func useTempConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	return root
}

func readFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	return b
}

func TestSetGetRoundTripAndModes(t *testing.T) {
	root := useTempConfig(t)

	if err := Set("openrouter", "sk-or-v1-abc123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := s.Get("openrouter")
	if !ok || got != "sk-or-v1-abc123" {
		t.Fatalf("Get = %q, %v; want stored key", got, ok)
	}
	if _, ok := s.Get("never-stored"); ok {
		t.Fatal("Get reported a key that was never stored")
	}

	if want := filepath.Join(root, "kintsugi", "credentials.json"); Path() != want {
		t.Fatalf("Path = %q, want %q", Path(), want)
	}

	fi, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("store mode = %o, want 600", mode)
	}
	di, err := os.Stat(filepath.Dir(Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 700", mode)
	}

	// The on-disk shape is part of the contract.
	var raw struct {
		Version int               `json:"version"`
		Keys    map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(readFile(t), &raw); err != nil {
		t.Fatalf("store is not valid JSON: %v", err)
	}
	if raw.Version != 1 {
		t.Errorf("version = %d, want 1", raw.Version)
	}
	if raw.Keys["openrouter"] != "sk-or-v1-abc123" {
		t.Errorf("on-disk keys = %v", raw.Keys)
	}
}

func TestStatusEnvWins(t *testing.T) {
	useTempConfig(t)
	t.Setenv("KINTSUGI_TEST_KEY", "from-env")

	if got := Status("openrouter", "KINTSUGI_TEST_KEY"); got != "env" {
		t.Errorf("no store, env set: Status = %q, want env", got)
	}
	if got := Status("openrouter", ""); got != "missing" {
		t.Errorf("no env name, nothing stored: Status = %q, want missing", got)
	}

	if err := Set("openrouter", "stored-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := Status("openrouter", "KINTSUGI_TEST_KEY"); got != "env" {
		t.Errorf("env and stored: Status = %q, want env", got)
	}
	if got := Status("openrouter", "KINTSUGI_ABSENT_KEY"); got != "stored" {
		t.Errorf("stored only: Status = %q, want stored", got)
	}
	if got := Status("openrouter", ""); got != "stored" {
		t.Errorf("stored, no env option: Status = %q, want stored", got)
	}
	// Env var set to empty string is not a key.
	t.Setenv("KINTSUGI_EMPTY_KEY", "")
	if err := Delete("openrouter"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := Status("openrouter", "KINTSUGI_EMPTY_KEY"); got != "missing" {
		t.Errorf("empty env var, nothing stored: Status = %q, want missing", got)
	}
}

func TestDelete(t *testing.T) {
	useTempConfig(t)

	if err := Delete("ghost"); err != nil {
		t.Errorf("Delete absent on missing store: %v", err)
	}
	if _, err := os.Stat(Path()); !os.IsNotExist(err) {
		t.Errorf("Delete of absent provider created a file: %v", err)
	}

	if err := Set("openrouter", "k1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Set("deepseek", "k2"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Delete("openrouter"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := Delete("openrouter"); err != nil {
		t.Errorf("Delete twice: %v", err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if _, ok := s.Get("openrouter"); ok {
		t.Error("deleted key is still readable")
	}
	if v, ok := s.Get("deepseek"); !ok || v != "k2" {
		t.Errorf("unrelated key lost: %q, %v", v, ok)
	}

	// Emptying the store leaves a valid document rather than removing it.
	if err := Delete("deepseek"); err != nil {
		t.Fatalf("Delete last: %v", err)
	}
	b := readFile(t)
	var raw struct {
		Version int               `json:"version"`
		Keys    map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("empty store is not valid JSON: %v", err)
	}
	if raw.Version != 1 || len(raw.Keys) != 0 {
		t.Errorf("empty store = %s", b)
	}
}

func TestSetOverwriteKeepsOtherProviders(t *testing.T) {
	useTempConfig(t)

	if err := Set("openrouter", "first"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Set("deepseek", "second"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Overwrite one: load-modify-write must not drop the other.
	if err := Set("openrouter", "first-v2"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v, _ := s.Get("openrouter"); v != "first-v2" {
		t.Errorf("openrouter = %q, want first-v2", v)
	}
	if v, _ := s.Get("deepseek"); v != "second" {
		t.Errorf("deepseek = %q, want second", v)
	}

	// A temp file from the atomic write must not be left behind.
	ents, err := os.ReadDir(filepath.Dir(Path()))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "credentials.json" {
		names := make([]string, len(ents))
		for i, e := range ents {
			names[i] = e.Name()
		}
		t.Errorf("dir contains %v, want only credentials.json", names)
	}
}

func TestMalformedStoreIsNotOverwritten(t *testing.T) {
	useTempConfig(t)
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("{\"version\": 1, \"keys\": {\"openrouter\": ")
	if err := os.WriteFile(Path(), garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted malformed JSON")
	} else if !strings.Contains(err.Error(), "malformed JSON") {
		t.Errorf("Load error = %v, want a malformed JSON message", err)
	}

	if err := Set("deepseek", "newkey"); err == nil {
		t.Fatal("Set overwrote a malformed store")
	}
	if err := Delete("openrouter"); err == nil {
		t.Fatal("Delete overwrote a malformed store")
	}

	if got := readFile(t); string(got) != string(garbage) {
		t.Errorf("malformed file changed:\n got %q\nwant %q", got, garbage)
	}
}

func TestErrorsNeverContainKey(t *testing.T) {
	useTempConfig(t)
	const sentinel = "sk-SENTINEL-4f9d2b7c1a8e-DO-NOT-LEAK"

	if err := Set("  ", sentinel); err == nil {
		t.Fatal("Set accepted an empty provider name")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Errorf("Set provider error leaked the key: %v", err)
	}

	if err := Set("openrouter", sentinel); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Set("openrouter", " \t\n"+sentinel+"\n"); err != nil {
		t.Fatalf("Set padded: %v", err)
	}
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v, _ := s.Get("openrouter"); v != sentinel {
		t.Errorf("key not trimmed: %q", v)
	}

	// A malformed file that embeds the sentinel must not echo it back.
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(b), `"openrouter": "`+sentinel+`"`, `"openrouter": `+sentinel, 1)
	if broken == string(b) {
		t.Fatalf("test setup: sentinel not found in store: %s", b)
	}
	if err := os.WriteFile(Path(), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted malformed JSON")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Errorf("Load error leaked the key: %v", err)
	}

	if err := Set("deepseek", sentinel); err == nil {
		t.Fatal("Set accepted a malformed store")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Errorf("Set error leaked the key: %v", err)
	}
}

func TestSetRejectsEmptyKey(t *testing.T) {
	useTempConfig(t)

	for _, k := range []string{"", " ", "\t", "  \n\t "} {
		if err := Set("openrouter", k); err == nil {
			t.Errorf("Set accepted key %q", k)
		}
	}
	if _, err := os.Stat(Path()); !os.IsNotExist(err) {
		t.Errorf("rejected keys still wrote the store: %v", err)
	}
}
