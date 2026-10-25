package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Repair implements the ordered shape-repair catalogue (Command Code findings,
// DESIGN §6.4). Philosophy: validate first, repair the smallest thing that
// makes the call valid, and surface every repair as a note the model can learn
// from — never silently preprocess into a different request.
//
// Order matters: structural repairs run before type repairs, because a
// stringified array inside a null-heavy object needs the null dropped before
// the type check.

type Spec struct {
	Required []string
	Types    map[string]string // "string" | "integer" | "array" | "object" | "boolean"
	PathKey  string            // parameter that holds a filesystem path (autolink unwrap)
}

var specs = map[string]Spec{
	"read":          {Required: []string{"path"}, Types: map[string]string{"path": "string"}, PathKey: "path"},
	"list":          {Required: nil, Types: map[string]string{"path": "string"}, PathKey: "path"},
	"glob":          {Required: []string{"pattern"}, Types: map[string]string{"pattern": "string", "root": "string"}},
	"grep":          {Required: []string{"pattern"}, Types: map[string]string{"pattern": "string", "path": "string", "glob": "string", "max": "integer"}, PathKey: "path"},
	"edit":          {Required: []string{"path", "old", "new"}, Types: map[string]string{"path": "string", "old": "string", "new": "string"}},
	"write":         {Required: []string{"path", "content"}, Types: map[string]string{"path": "string", "content": "string", "append": "boolean"}},
	"apply_patch":   {Required: []string{"patch"}, Types: map[string]string{"patch": "string"}},
	"todo":          {Required: []string{"todos"}, Types: map[string]string{"todos": "array"}},
	"webfetch":      {Required: []string{"url"}, Types: map[string]string{"url": "string", "max_bytes": "integer"}},
	"websearch":     {Required: []string{"query"}, Types: map[string]string{"query": "string", "limit": "integer"}},
	"cve_lookup":    {Required: []string{"id"}, Types: map[string]string{"id": "string"}},
	"bash":          {Required: []string{"command"}, Types: map[string]string{"command": "string", "timeout_s": "integer"}},
	"memory_search": {Required: []string{"query"}, Types: map[string]string{"query": "string", "k": "integer"}},
	"kb_search":     {Required: []string{"query"}, Types: map[string]string{"query": "string", "kb": "string", "top_k": "integer"}},
}

// toolAliases maps a tool name a model emitted onto the name this harness
// exposes. Like the argument aliases below, the intent is unambiguous, so
// the call is renamed rather than rejected.
var toolAliases = map[string]string{
	"kb search":             "kb_search",
	"kb-search":             "kb_search",
	"kbsearch":              "kb_search",
	"search_kb":             "kb_search",
	"search kb":             "kb_search",
	"search-kb":             "kb_search",
	"search kb chunks":      "kb_search",
	"knowledge search":      "kb_search",
	"knowledge-search":      "kb_search",
	"knowledge_search":      "kb_search",
	"knowledge base search": "kb_search",
	"knowledge_base_search": "kb_search",
	"search knowledge base": "kb_search",
	"kb_lookup":             "kb_search",
	"lookup_kb":             "kb_search",
	// The research surface (M8).
	"search":      "websearch",
	"web_search":  "websearch",
	"websearch":   "websearch",
	"lookup_cve":  "cve_lookup",
	"cve":         "cve_lookup",
	"vuln":        "cve_lookup",
	"vuln_lookup": "cve_lookup",
	// The memory verbs (M10-C). Same rule as above: a model trained on
	// another harness emits one of these names for an unambiguous verb, so
	// the call is renamed rather than rejected. Renaming never widens
	// anything — the permission matrix evaluates the canonical name.
	"remember":       "retain",
	"retain_fact":    "retain",
	"record_fact":    "retain",
	"add_memory":     "retain",
	"memory_add":     "retain",
	"memorize":       "retain",
	"memory_recall":  "recall",
	"recall_memory":  "recall",
	"memory_reflect": "reflect",
	"reflect_memory": "reflect",
	"edit_memory":    "memory_edit",
	"memory_update":  "memory_edit",
	"forget":         "memory_edit",
	"invalidate":     "memory_edit",
	"promote_skill":  "learn",
	"skill_learn":    "learn",
	"skill_manage":   "manage_skill",
	"manage_skills":  "manage_skill",
	"create_skill":   "manage_skill",
	"delete_skill":   "manage_skill",
	"edit_skill":     "manage_skill",
}

// aliases is intent-defaulting (relational repair): models trained on other
// harnesses emit these names, and the intent is unambiguous.
var aliases = map[string]map[string]string{
	"read": {"file_path": "path", "file": "path", "filename": "path"},
	"list": {"dir": "path", "directory": "path", "dir_path": "path"},
	"edit": {
		"file_path": "path", "file": "path",
		"old_string": "old", "old_str": "old", "search": "old", "find": "old",
		"new_string": "new", "new_str": "new", "replace": "new", "replacement": "new",
	},
	"glob": {"file_pattern": "pattern", "path": "root"},
	"grep": {"query": "pattern", "regex": "pattern", "file_pattern": "glob"},
	"bash": {"cmd": "command", "shell": "command"},
	"write": {
		"file_path": "path", "file": "path", "filename": "path",
		"text": "content", "body": "content", "data": "content",
	},
	"apply_patch": {"input": "patch", "diff": "patch", "content": "patch"},
	"todo":        {"items": "todos", "tasks": "todos", "plan": "todos"},
	"webfetch":    {"href": "url", "link": "url", "target": "url"},
	"websearch": {
		"q": "query", "search": "query", "text": "query", "terms": "query",
		"n": "limit", "count": "limit", "max_results": "limit", "num_results": "limit",
	},
	"cve_lookup": {
		"cve": "id", "cve_id": "id", "vuln_id": "id", "vulnerability": "id",
		"advisory": "id", "ghsa": "id", "ghsa_id": "id",
	},
	"kb_search": {
		"q": "query", "search": "query", "text": "query", "terms": "query",
		"kb_id": "kb", "base": "kb", "kb_name": "kb", "knowledge_base": "kb",
		"k": "top_k", "limit": "top_k", "n": "top_k", "count": "top_k", "max": "top_k",
	},
	// The memory verbs (M10-C).
	"recall": {
		"q": "query", "search": "query", "text": "query", "terms": "query",
		"query_text": "query", "n": "k", "limit": "k", "top_k": "k", "count": "k", "max": "k",
	},
	"retain": {
		"fact": "text", "content": "text", "memory": "text", "note": "text",
		"value": "text", "lesson": "text", "body": "text", "message": "text",
		"kind": "class", "type": "class", "category": "class", "section": "class",
		"model": "models", "models_list": "models", "provenance": "source", "from": "source",
	},
	"reflect": {
		"ask": "question", "query": "question", "prompt": "question", "topic": "question",
		"text": "question", "max": "max_lessons", "lessons": "max_lessons",
		"budget": "max_tokens", "tokens": "max_tokens", "max_bank_tokens": "max_tokens",
	},
	"memory_edit": {
		"operation": "op", "action": "op", "mode": "op",
		"entry": "id", "entry_id": "id", "target": "id", "path": "id", "ref": "id",
		"new": "text", "new_text": "text", "replacement": "text", "content": "text", "value": "text",
		"note": "reason", "why": "reason", "explanation": "reason",
		"kind": "class", "type": "class", "category": "class", "section": "class",
	},
	"learn": {
		"lesson_id": "lesson", "id": "lesson", "entry": "lesson", "text": "lesson", "content": "lesson",
		"skill": "name", "skill_name": "name", "title": "name", "summary": "description",
		"kind": "class", "type": "class",
	},
	"manage_skill": {
		"operation": "op", "action": "op", "mode": "op",
		"skill": "name", "skill_name": "name", "title": "name",
		"content": "body", "text": "body", "markdown": "body", "procedure": "body",
		"summary": "description", "desc": "description", "why": "reason", "note": "reason",
	},
}

type RepairResult struct {
	Args    string   // repaired JSON object
	Classes []string // repair classes applied, in order
	Notes   []string // human-readable intent defaults worth surfacing
	// Tool is the canonical name of the tool the call should run under: the
	// name the model sent, or the alias it was renamed to.
	Tool string
}

var autolinkRe = regexp.MustCompile(`^\[([^\]]+)\]\([^)]*\)$`)

// Repair normalizes one raw tool-call argument string for the named tool.
func Repair(tool, raw string) (RepairResult, error) {
	name := canonicalTool(tool)
	spec, ok := specs[name]
	if !ok {
		return RepairResult{Args: raw, Tool: name}, nil
	}
	return repairWith(name, spec, raw)
}

// canonicalTool maps a tool name a model emitted onto the name this harness
// exposes. The intent is unambiguous, so the call is renamed rather than
// rejected. An unknown name is returned unchanged (MCP tools go this way).
func canonicalTool(tool string) string {
	name := strings.TrimSpace(tool)
	if canon, ok := toolAliases[strings.ToLower(name)]; ok {
		return canon
	}
	return name
}

func repairWith(tool string, spec Spec, raw string) (RepairResult, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	if tool == "apply_patch" && strings.HasPrefix(raw, "*** Begin Patch") {
		b, _ := json.Marshal(map[string]string{"patch": raw})
		return RepairResult{Args: string(b), Classes: []string{"freeform-envelope"}, Tool: tool}, nil
	}

	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		// A bare, non-JSON string: treat as the path for path-only tools.
		if spec.PathKey != "" && !strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[") {
			arg := strings.TrimSpace(raw)
			if m := autolinkRe.FindStringSubmatch(arg); m != nil {
				arg = m[1]
			}
			b, _ := json.Marshal(map[string]string{spec.PathKey: arg})
			return RepairResult{Args: string(b), Classes: []string{"bare-path"}, Tool: tool}, nil
		}
		return RepairResult{}, fmt.Errorf("tool arguments are not valid JSON: %w", err)
	}

	res := RepairResult{Tool: tool}

	// Root must be an object. A single-element array is a common model shape.
	if arr, isArr := v.([]any); isArr {
		if len(arr) == 1 {
			if obj, isObj := arr[0].(map[string]any); isObj {
				v = obj
				res.Classes = append(res.Classes, "root-array-unwrap")
			} else {
				return RepairResult{}, fmt.Errorf("tool arguments must be one JSON object")
			}
		} else {
			return RepairResult{}, fmt.Errorf("tool arguments must be one JSON object, got %d-element array", len(arr))
		}
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		// A bare string (e.g. `"src/main.go"`) for a path-taking tool.
		if s, isStr := v.(string); isStr && spec.PathKey != "" {
			s = unwrapAutolink(s)
			b, _ := json.Marshal(map[string]string{spec.PathKey: s})
			return RepairResult{Args: string(b), Classes: []string{"bare-path"}, Tool: tool}, nil
		}
		return RepairResult{}, fmt.Errorf("tool arguments must be one JSON object")
	}

	// 1. alias/intent defaults (before null handling, so alias fields survive).
	applyAliases := func() {
		al, ok := aliases[tool]
		if !ok {
			return
		}
		for from, to := range al {
			if _, exists := obj[to]; exists {
				continue
			}
			if val, exists := obj[from]; exists {
				obj[to] = val
				delete(obj, from)
				res.Classes = append(res.Classes, "alias-default")
				res.Notes = append(res.Notes, fmt.Sprintf("interpreted %q as %q", from, to))
			}
		}
	}
	applyAliases()

	// 2. null-for-optional: drop nulls on optional fields.
	for k, val := range obj {
		if val == nil && !contains(spec.Required, k) {
			delete(obj, k)
			res.Classes = append(res.Classes, "null-optional")
		}
	}

	// 3. single-argument wrapped in an object: {"args":{"path":...}}.
	if len(obj) == 1 {
		for _, val := range obj {
			if inner, ok := val.(map[string]any); ok && hasAnyKey(inner, spec) {
				obj = inner
				res.Classes = append(res.Classes, "nested-single-arg")
				applyAliases()
			}
		}
	}

	// 4. autolink paths and stringified containers.
	for k, val := range obj {
		want, hasType := spec.Types[k]
		switch tv := val.(type) {
		case string:
			if k == spec.PathKey {
				if unwrapped := unwrapAutolink(tv); unwrapped != tv {
					obj[k] = unwrapped
					res.Classes = append(res.Classes, "autolink-path")
				}
				continue
			}
			if hasType && want == "array" {
				var arr []any
				if err := json.Unmarshal([]byte(tv), &arr); err == nil {
					obj[k] = arr
					res.Classes = append(res.Classes, "stringified-array")
				} else {
					obj[k] = []any{tv}
					res.Classes = append(res.Classes, "scalar-array")
				}
			}
			if hasType && want == "integer" {
				if n, err := json.Number(tv).Int64(); err == nil {
					obj[k] = n
					res.Classes = append(res.Classes, "stringified-number")
				}
			}
		}
	}

	// 5. validate. If a required field is missing, do not invent it.
	for _, req := range spec.Required {
		if _, exists := obj[req]; !exists {
			return RepairResult{}, fmt.Errorf("missing required argument %q", req)
		}
	}
	for k, val := range obj {
		want, ok := spec.Types[k]
		if !ok {
			continue // unknown/extra fields are tolerated
		}
		if !typeOK(val, want) {
			return RepairResult{}, fmt.Errorf("argument %q has wrong type: want %s", k, want)
		}
	}

	b, err := json.Marshal(obj)
	if err != nil {
		return RepairResult{}, err
	}
	res.Args = string(b)
	return res, nil
}

func unwrapAutolink(s string) string {
	if m := autolinkRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1]
	}
	return s
}

func typeOK(v any, want string) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		switch v.(type) {
		case float64, int64, int:
			return true
		}
		return false
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	}
	return true
}

func hasAnyKey(obj map[string]any, spec Spec) bool {
	for k := range spec.Types {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
