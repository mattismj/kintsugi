// Package quota is the offline quota ledger (DESIGN §8.4). Providers are not
// required to expose a quota API: the harness records approximate usage per
// period and refuses plan nodes that cannot fit the configured limit.
package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Limit struct {
	LimitTokens int64  `json:"limit_tokens"`
	Reset       string `json:"reset,omitempty"` // daily | weekly | monthly | none
	Note        string `json:"note,omitempty"`
}

type Entry struct {
	Period     string `json:"period"`
	UsedTokens int64  `json:"used_tokens"`
	Requests   int    `json:"requests"`
}

type State struct {
	Providers map[string]Entry `json:"providers"`
}

type ProviderState struct {
	Provider  string `json:"provider"`
	Used      int64  `json:"used_tokens"`
	Limit     int64  `json:"limit_tokens,omitempty"`
	Remaining int64  `json:"remaining_tokens"`
	Period    string `json:"period"`
	Reset     string `json:"reset,omitempty"`
	Requests  int    `json:"requests"`
}

type Store struct {
	Path   string
	Limits map[string]Limit
	Now    func() time.Time

	state State
}

// DefaultPath is the global cache ledger (quota is provider-wide, not per
// workspace).
func DefaultPath() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return filepath.Join(os.TempDir(), "kintsugi-quota.json")
	}
	return filepath.Join(dir, "kintsugi", "quota.json")
}

func Open(path string, limits map[string]Limit) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	s := &Store{Path: path, Limits: limits, Now: time.Now, state: State{Providers: map[string]Entry{}}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("quota ledger %s: %w", path, err)
		}
		if s.state.Providers == nil {
			s.state.Providers = map[string]Entry{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) period(provider string) string {
	return Period(s.Now(), s.Limits[provider].Reset)
}

// Period returns the accounting bucket for a reset policy.
func Period(now time.Time, reset string) string {
	switch reset {
	case "daily":
		return now.Format("2006-01-02")
	case "weekly":
		y, w := now.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case "monthly":
		return now.Format("2006-01")
	default:
		return "all"
	}
}

// current returns (and lazily resets) the entry for a provider.
func (s *Store) current(provider string) Entry {
	e := s.state.Providers[provider]
	p := s.period(provider)
	if e.Period != p {
		e = Entry{Period: p}
	}
	return e
}

func (s *Store) Remaining(provider string) (int64, bool) {
	lim := s.Limits[provider]
	if lim.LimitTokens <= 0 {
		return 0, false
	}
	used := s.current(provider).UsedTokens
	rem := lim.LimitTokens - used
	if rem < 0 {
		rem = 0
	}
	return rem, true
}

// CanFit refuses an operation that cannot fit in the remaining quota.
func (s *Store) CanFit(provider string, tokens int) (bool, string) {
	rem, limited := s.Remaining(provider)
	if !limited {
		return true, ""
	}
	if int64(tokens) > rem {
		return false, fmt.Sprintf("%s quota: need ~%d tokens, %d remaining this %s", provider, tokens, rem, resetName(s.Limits[provider].Reset))
	}
	return true, ""
}

func (s *Store) Add(provider string, tokens int) error {
	if tokens <= 0 {
		return nil
	}
	e := s.current(provider)
	e.UsedTokens += int64(tokens)
	e.Requests++
	s.state.Providers[provider] = e
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s *Store) Snapshot() []ProviderState {
	names := map[string]bool{}
	for name := range s.Limits {
		names[name] = true
	}
	for name := range s.state.Providers {
		names[name] = true
	}
	out := make([]ProviderState, 0, len(names))
	for name := range names {
		e := s.current(name)
		lim := s.Limits[name]
		rem, _ := s.Remaining(name)
		out = append(out, ProviderState{
			Provider: name, Used: e.UsedTokens, Limit: lim.LimitTokens,
			Remaining: rem, Period: e.Period, Reset: lim.Reset, Requests: e.Requests,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func resetName(reset string) string {
	if reset == "" {
		return "period"
	}
	return reset + " period"
}
