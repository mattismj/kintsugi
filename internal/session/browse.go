package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"kintsugi/internal/provider"
)

// Info is the cheap session index the CLI and TUI show without replaying a
// transcript. It is derived from the first meta entry and the event stream.
type Info struct {
	ID      string
	Path    string
	Model   string
	Persona string
	Role    string
	Stack   string
	Title   string
	Started time.Time
	LastAt  time.Time
	Turns   int
	Bytes   int64
}

// DisplayTitle returns the title for display. A session that predates titles
// has none: it shows an em dash rather than an empty column.
func (i Info) DisplayTitle() string { return dash(i.Title) }

// Open reopens an existing session for append, importing a legacy JSONL file
// into the store on first touch. History is not replayed here; callers use
// History when they need the model-visible transcript.
func Open(id string) (*Session, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return openSessionAt(dir, id)
}

// Fork copies a session's event stream into a new session and records the
// origin. The original is never modified, so a resumed branch cannot rewrite
// history the operator may still want to audit.
func Fork(id string) (*Session, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	src, err := openSessionAt(dir, id)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := New()
	if err != nil {
		return nil, err
	}
	if err := copyEvents(src, dst); err != nil {
		dst.Close()
		return nil, err
	}
	dst.Log("meta", map[string]string{"forked_from": id})
	return dst, nil
}

// History reconstructs the provider-visible conversation from the event log.
// The loop logs an assistant event before each batch of tool calls and a tool
// result after each call, so tool calls are reattached to their assistant
// message and results are paired with the pending call in order.
func History(id string) ([]provider.Message, error) {
	entries, err := readEntries(id)
	if err != nil {
		return nil, err
	}
	var out []provider.Message
	var pending []provider.ToolCall
	for _, e := range entries {
		switch e.Kind {
		case "user":
			s, _ := e.V.(string)
			if s != "" {
				out = append(out, provider.Message{Role: "user", Content: s})
			}
		case "assistant":
			s, _ := e.V.(string)
			out = append(out, provider.Message{Role: "assistant", Content: s})
		case "system":
			s, _ := e.V.(string)
			if s != "" {
				out = append(out, provider.Message{Role: "system", Content: s})
			}
		case "tool_call":
			var tc provider.ToolCall
			if !remarshal(e.V, &tc) || tc.Function.Name == "" {
				continue
			}
			pending = append(pending, tc)
			if n := len(out); n > 0 && out[n-1].Role == "assistant" {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, tc)
			} else {
				out = append(out, provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{tc}})
			}
		case "tool_result":
			s, _ := e.V.(string)
			m := provider.Message{Role: "tool", Content: s}
			if len(pending) > 0 {
				m.ToolCallID = pending[0].ID
				m.Name = pending[0].Function.Name
				pending = pending[1:]
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// ListInfo returns sessions newest-first.
func ListInfo() ([]Info, error) {
	ids, err := List()
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(ids))
	for _, id := range ids {
		info, err := InfoOf(id)
		if err != nil {
			continue
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID > infos[j].ID })
	return infos, nil
}

// InfoOf reads one session's index fields. Opening a legacy JSONL session here
// imports it (see migrateJSONL); the JSONL file itself is left alone.
func InfoOf(id string) (Info, error) {
	dir, err := Dir()
	if err != nil {
		return Info{}, err
	}
	s, err := openSessionAt(dir, id)
	if err != nil {
		return Info{}, err
	}
	defer s.Close()
	entries, err := s.entries()
	if err != nil {
		return Info{}, err
	}
	info := Info{ID: id, Path: s.Path, Bytes: storeSize(s.Path)}
	if len(entries) > 0 {
		info.Started, _ = time.Parse(time.RFC3339Nano, entries[0].TS)
		info.LastAt, _ = time.Parse(time.RFC3339Nano, entries[len(entries)-1].TS)
	}
	for _, e := range entries {
		switch e.Kind {
		case "meta":
			if info.Model == "" {
				info.Model = metaString(e.V, "model")
			}
			if info.Persona == "" {
				info.Persona = metaString(e.V, "persona")
			}
			if info.Role == "" {
				info.Role = metaString(e.V, "role")
			}
			if info.Stack == "" {
				info.Stack = metaString(e.V, "stack")
			}
			// The last title wins: an explicit rename replaces the derived one.
			if t := strings.TrimSpace(metaString(e.V, "title")); t != "" {
				info.Title = t
			}
		case "user":
			info.Turns++
		}
	}
	return info, nil
}

// FormatInfoTable renders the session index as a table. The title sits next
// to the id — it is what an operator scans for — and is cut with an ellipsis
// so the row stays readable. A session without a title shows an em dash.
func FormatInfoTable(infos []Info) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s %-40s %-28s %-18s %5s %8s  %s\n",
		"ID", "TITLE", "MODEL", "PERSONA", "TURNS", "SIZE", "STARTED")
	for _, s := range infos {
		started := ""
		if !s.Started.IsZero() {
			started = s.Started.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "%-24s %-40s %-28s %-18s %5d %8d  %s\n",
			s.ID, fitText(s.DisplayTitle(), 40), fitText(s.Model, 28), fitText(s.Persona, 18),
			s.Turns, s.Bytes, started)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Hit is one search match inside a session.
type Hit struct {
	ID      string
	Kind    string
	TS      time.Time
	Snippet string
}

// Search scans user/assistant/tool text for a case-insensitive substring.
// Results are newest session first, then in transcript order.
func Search(query string, limit int) ([]Hit, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, fmt.Errorf("search query is empty")
	}
	infos, err := ListInfo()
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, info := range infos {
		entries, err := readEntries(info.ID)
		if err != nil {
			continue
		}
		for _, e := range entries {
			s, ok := e.V.(string)
			if !ok || s == "" {
				continue
			}
			lower := strings.ToLower(s)
			idx := strings.Index(lower, q)
			if idx < 0 {
				continue
			}
			ts, _ := time.Parse(time.RFC3339Nano, e.TS)
			hits = append(hits, Hit{ID: info.ID, Kind: e.Kind, TS: ts, Snippet: snippet(s, idx, len(q), 90)})
			if limit > 0 && len(hits) >= limit {
				return hits, nil
			}
		}
	}
	return hits, nil
}

func snippet(s string, idx, qlen, width int) string {
	start := idx - width/2
	if start < 0 {
		start = 0
	}
	end := idx + qlen + width/2
	if end > len(s) {
		end = len(s)
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(s) {
		suffix = "…"
	}
	return prefix + strings.ReplaceAll(s[start:end], "\n", " ") + suffix
}

// readEntries returns a session's events from its store, importing a legacy
// JSONL file on first touch.
func readEntries(id string) ([]entry, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return readEntriesAt(dir, id)
}

func remarshal(v any, dst any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}
