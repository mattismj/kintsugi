// The memory verbs (WP M10-C, DESIGN §16.17): the tool face of
// internal/memory's write surface.
//
// Six verbs sit next to memory_search:
//
//	retain       queue a fact            → curator/proposal gate for gated classes
//	recall       search the store        → read-only
//	reflect      synthesize over the bank → read-only, asks the curator model
//	memory_edit  update/forget/invalidate → curator/proposal gate for gated classes
//	learn        promote a lesson to a skill → curator/proposal gate for gated classes
//	manage_skill create/update/delete a skill file → writes the skills directory
//
// Two rules are enforced here and in internal/memory, and neither is new:
//
//   - Permission: every verb goes through the same matrix memory_search does
//     (Policy.Decide in execute). The read verbs stop there. The write verbs
//     additionally call authorize(), so a mutating memory call asks the
//     operator in default mode, is refused in dont-ask mode, and is refused in
//     plan mode by the floor below — the matrix's ordinary answers for a tool
//     that changes state.
//   - Gating: `authorization`, `scope` and corrections are operator-only. A
//     model write to one of those classes does not happen: internal/memory
//     files a proposal in the curator's own review queue instead, which
//     `kintsugi context apply` reviews. There is no verb-local notion of allowed.
//
// And the product invariant, restated because it is load-bearing: a refusal is
// telemetry, never memory. Every model-supplied text passes the curator's own
// refusal filter before it can be written, and every read path filters on the
// way out, so a refused turn cannot become a lesson, a skill, a proposal or a
// line of the session mental model.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"kintsugi/internal/audit"
	"kintsugi/internal/memory"
	"kintsugi/internal/provider"
)

// MemoryVerbConfig is the wiring the memory verbs need beyond the store itself.
// It is optional: a nil value means the workspace defaults, no curator model for
// `reflect`, and the hard-gated classes only.
type MemoryVerbConfig struct {
	// Gate is the write policy shared with the curator: the operator's
	// configured gated classes over the hard-gated ones, plus the session id
	// that labels a queued proposal.
	Gate memory.Gate
	// SkillsDir overrides the skills directory `learn` and `manage_skill`
	// write to. Empty means the workspace's own skills directory.
	SkillsDir string
	// Reflect asks the curator model to synthesize over the memory bank
	// (see NewReflector). nil means no curator model is attached, and
	// `reflect` then answers with the bank itself instead of inventing a
	// synthesis.
	Reflect memory.Reflector
}

// ---------- definitions ----------

func retainTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "retain",
			Description: "Record one durable fact about this project in its memory (a convention, a decision, a " +
				"gotcha you verified). The fact becomes a dated, searchable lesson. Operator-gated classes " +
				"(authorization, scope, corrections) are never written by you: they are queued as a proposal the " +
				"operator reviews. Never retain a refusal — a refusal is telemetry, not knowledge.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"text"},
				Properties: map[string]provider.ToolProp{
					"text":   {Type: "string", Description: "The fact, one line, self-contained and dated by the harness."},
					"class":  {Type: "string", Description: "Context class of the fact: lesson (default), convention, state, stack, domain, authorization, scope."},
					"models": {Type: "string", Description: "Optional comma-separated model ids the fact applies to."},
					"source": {Type: "string", Description: "Optional provenance note (defaults to this session id)."},
				},
			},
		},
	}
}

func recallTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "recall",
			Description: "Search the project's durable memory (recorded lessons and skills) and return ranked hits with " +
				"the stable id each one needs for memory_edit or learn. Read-only. Prefer it over re-deriving a " +
				"fact this workspace already recorded.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"query"},
				Properties: map[string]provider.ToolProp{
					"query": {Type: "string", Description: "Keywords describing what you need to recall."},
					"k":     {Type: "integer", Description: "Maximum hits (default 8, max 25)."},
				},
			},
		},
	}
}

func reflectTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "reflect",
			Description: "Ask the curator model to synthesize an answer over the whole memory bank (every recorded " +
				"lesson plus the skill index) instead of one keyword search. Read-only: it writes no memory. Use " +
				"it for cross-cutting questions — what this project has learned about a subsystem, which " +
				"conventions conflict — where recall's ranking is too narrow.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"question"},
				Properties: map[string]provider.ToolProp{
					"question":    {Type: "string", Description: "The question to answer from the memory bank."},
					"max_lessons": {Type: "integer", Description: "How many of the newest lessons to synthesize over (default 40)."},
					"max_tokens":  {Type: "integer", Description: "Token budget for the bank sent to the model (default 1600)."},
				},
			},
		},
	}
}

func memoryEditTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "memory_edit",
			Description: "Change one recorded memory entry by its id (the id recall prints): rewrite it, forget it, or " +
				"invalidate it (kept on disk for history, no longer recallable). Operator-gated classes are never " +
				"written by you: the change is queued as a proposal the operator reviews.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"op", "id"},
				Properties: map[string]provider.ToolProp{
					"op": {
						Type:        "string",
						Enum:        []any{"update", "forget", "invalidate"},
						Description: "update: replace the entry's text. forget: remove it. invalidate: retire it but keep it on disk.",
					},
					"id":     {Type: "string", Description: "Entry id from recall, e.g. \".kintsugi/learned.md:12\" or \".kintsugi/skills/lab-run.md\"."},
					"text":   {Type: "string", Description: "Replacement text (required for update)."},
					"reason": {Type: "string", Description: "Why the entry is being invalidated; recorded beside it."},
					"class":  {Type: "string", Description: "Optional class override; defaults to the entry's own (lesson, or convention for a skill)."},
				},
			},
		},
	}
}

func learnTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "learn",
			Description: "Promote a lesson to a reusable skill: the lesson becomes (or is appended to) a skill file " +
				"under the workspace skills directory, so its procedure is loaded on demand by the skill tool. " +
				"Operator-gated classes are never written by you: the promotion is queued as a proposal.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"lesson", "name"},
				Properties: map[string]provider.ToolProp{
					"lesson":      {Type: "string", Description: "The lesson to promote: an entry id from recall, or the lesson text itself."},
					"name":        {Type: "string", Description: "Skill name (letters, digits, '.', '_', '-'); it becomes <name>.md."},
					"description": {Type: "string", Description: "One-line description for the skills index (default: the lesson's first line)."},
					"class":       {Type: "string", Description: "Optional class; defaults to convention (a skill is a procedure)."},
				},
			},
		},
	}
}

func manageSkillTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "manage_skill",
			Description: "Create, update or delete one skill file under the workspace skills directory. The name is " +
				"validated and can never name a path, so a skill is always written where the operator's skills " +
				"live. Deleting keeps history when you give a reason (the file is moved aside, out of the index).",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"op", "name"},
				Properties: map[string]provider.ToolProp{
					"op": {
						Type:        "string",
						Enum:        []any{"create", "update", "delete"},
						Description: "create: a new skill file. update: replace an existing one's body. delete: remove it.",
					},
					"name":        {Type: "string", Description: "Skill name; the file is <skills dir>/<name>.md."},
					"description": {Type: "string", Description: "One-line description for the skills index."},
					"body":        {Type: "string", Description: "Markdown procedure (required for create and update)."},
					"reason":      {Type: "string", Description: "Why the skill is being deleted; with it the file is kept aside instead of destroyed."},
				},
			},
		},
	}
}

// ---------- shared plumbing ----------

// memoryVerbs reports whether the write verbs are usable: they need the
// workspace memory store, exactly like memory_search does.
func (e *Executor) memoryVerbs() (*memory.Store, error) {
	if e == nil || e.Memory == nil {
		return nil, fmt.Errorf("memory store not configured for this workspace")
	}
	return e.Memory, nil
}

// memoryGate is the write gate the verbs share with the curator: the operator's
// configured gated classes (when the harness wired them) over the hard-gated
// ones internal/context already enforces.
func (e *Executor) memoryGate() memory.Gate {
	g := memory.Gate{}
	if e.MemoryVerbs != nil {
		g = e.MemoryVerbs.Gate
	}
	if strings.TrimSpace(g.Session) == "" {
		g.Session = e.SessionID
	}
	return g
}

// memorySkillsDir is the directory manage_skill and learn write to: the one the
// harness configured, or the workspace default.
func (e *Executor) memorySkillsDir() string {
	if e.MemoryVerbs != nil && strings.TrimSpace(e.MemoryVerbs.SkillsDir) != "" {
		return e.MemoryVerbs.SkillsDir
	}
	if strings.TrimSpace(e.SkillsDir) != "" {
		return e.SkillsDir
	}
	return memory.SkillsDir(e.Workspace)
}

// memoryPlanFloor refuses memory writes in plan mode. The mode promises a
// read-only turn, and learned.md and the skills directory are workspace files:
// no rule and no approval lifts that floor, exactly as for edit/write.
func (e *Executor) memoryPlanFloor(tool string) error {
	mode := e.Policy.Mode
	if e.Policy.Yolo {
		mode = string(ModeYolo)
	}
	m, err := ParseMode(mode)
	if err != nil {
		m = ModeDefault
	}
	if m != ModePlan {
		return nil
	}
	err = fmt.Errorf("%s is refused in plan mode: the turn is read-only", tool)
	e.audit(tool, "", audit.DecisionDeny, err.Error(), "plan")
	return err
}

// memoryWriteGuard runs the two floors every mutating verb shares: the plan
// floor, then the permission matrix through authorize().
func (e *Executor) memoryWriteGuard(tool, detail string) error {
	if err := e.memoryPlanFloor(tool); err != nil {
		return err
	}
	return e.authorize(tool, "", detail)
}

// memorySkillPath resolves a skill file path through the workspace floors
// (containment, declared scope, deny paths) when the skills directory is inside
// the workspace. The name is validated first, so it can never name a path.
func (e *Executor) memorySkillPath(tool, name string) (string, error) {
	if _, err := memory.NormalizeSkillName(name); err != nil {
		return "", err
	}
	dir := e.memorySkillsDir()
	abs := filepath.Join(dir, strings.TrimSuffix(name, ".md")+".md")
	if e.Workspace == "" {
		return abs, nil
	}
	root := filepath.Clean(e.Workspace)
	clean := filepath.Clean(abs)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		// A skills directory outside the workspace is the operator's own
		// configuration (a global skills dir); the model never names it.
		return abs, nil
	}
	relPath, _ := filepath.Rel(root, clean)
	return e.resolveFor(tool, filepath.ToSlash(relPath))
}

// memoryWriteOutput is the JSON body a mutating verb returns.
type memoryWriteOutput struct {
	Verb    string `json:"verb"`
	Op      string `json:"op,omitempty"`
	Class   string `json:"class,omitempty"`
	Written bool   `json:"written"`
	Queued  bool   `json:"queued"`
	ID      string `json:"id,omitempty"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail"`
}

func memoryWriteResult(r memory.WriteResult) Result {
	b, err := json.Marshal(memoryWriteOutput{
		Verb: r.Verb, Op: r.Op, Class: r.Class,
		Written: r.Written, Queued: r.Queued,
		ID: r.ID, Path: r.Path, Detail: r.Detail,
	})
	if err != nil {
		return failJSON(err)
	}
	// A queued proposal is a successful outcome, not a failure: the verb did
	// the only thing it is allowed to do with a gated class.
	return Result{Content: string(b)}
}

// flexInt accepts an integer the model sent as a string ("8"). The repair
// catalogue already coerces that for these verbs, so this is belt-and-braces
// for a caller that dispatches without repair.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch s {
	case "", "null":
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("want an integer, got %q", s)
	}
	*f = flexInt(n)
	return nil
}

// ---------- recall ----------

func (e *Executor) recall(args string) Result {
	store, err := e.memoryVerbs()
	if err != nil {
		return failJSON(err)
	}
	var a struct {
		Query string  `json:"query"`
		K     flexInt `json:"k"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Query) == "" {
		return Result{Content: `{"error":"recall: missing required property 'query'"}`, Failed: true}
	}
	entries, err := store.Recall(a.Query, int(a.K))
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: memory.FormatRecall(entries)}
}

// ---------- retain ----------

func (e *Executor) retain(args string) Result {
	store, err := e.memoryVerbs()
	if err != nil {
		return failJSON(err)
	}
	var a struct {
		Text   string `json:"text"`
		Class  string `json:"class"`
		Models string `json:"models"`
		Source string `json:"source"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return Result{Content: `{"error":"retain: missing required property 'text'"}`, Failed: true}
	}
	detail := "retain: " + clip(a.Text, 120)
	if err := e.memoryWriteGuard("retain", detail); err != nil {
		return failJSON(err)
	}
	source := strings.TrimSpace(a.Source)
	if source == "" && strings.TrimSpace(e.SessionID) != "" {
		source = "session " + e.SessionID
	}
	res, err := store.Retain(a.Text, a.Class, a.Models, source, e.memoryGate())
	if err != nil {
		return failJSON(err)
	}
	return memoryWriteResult(res)
}

// ---------- memory_edit ----------

func (e *Executor) memoryEdit(args string) Result {
	store, err := e.memoryVerbs()
	if err != nil {
		return failJSON(err)
	}
	var a struct {
		Op     string `json:"op"`
		ID     string `json:"id"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
		Class  string `json:"class"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if strings.TrimSpace(a.Op) == "" {
		return Result{Content: `{"error":"memory_edit: missing required property 'op' (update|forget|invalidate)"}`, Failed: true}
	}
	if strings.TrimSpace(a.ID) == "" {
		return Result{Content: `{"error":"memory_edit: missing required property 'id' (recall prints it)"}`, Failed: true}
	}
	detail := fmt.Sprintf("memory_edit %s %s", strings.TrimSpace(a.Op), clip(a.ID, 80))
	if err := e.memoryWriteGuard("memory_edit", detail); err != nil {
		return failJSON(err)
	}
	res, err := store.Edit(memory.EditRequest{
		Op:        a.Op,
		ID:        a.ID,
		Text:      a.Text,
		Reason:    a.Reason,
		Class:     a.Class,
		SkillsDir: e.memorySkillsDir(),
	}, e.memoryGate())
	if err != nil {
		return failJSON(err)
	}
	return memoryWriteResult(res)
}

// ---------- learn ----------

func (e *Executor) learn(args string) Result {
	store, err := e.memoryVerbs()
	if err != nil {
		return failJSON(err)
	}
	var a struct {
		Lesson      string `json:"lesson"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Class       string `json:"class"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if strings.TrimSpace(a.Lesson) == "" {
		return Result{Content: `{"error":"learn: missing required property 'lesson' (an id from recall, or the lesson text)"}`, Failed: true}
	}
	if strings.TrimSpace(a.Name) == "" {
		return Result{Content: `{"error":"learn: missing required property 'name'"}`, Failed: true}
	}
	if _, err := memory.NormalizeSkillName(a.Name); err != nil {
		return failJSON(err)
	}
	detail := "learn: promote a lesson to the skill " + clip(a.Name, 60)
	if err := e.memoryWriteGuard("learn", detail); err != nil {
		return failJSON(err)
	}
	if _, err := e.memorySkillPath("learn", a.Name); err != nil {
		return failJSON(err)
	}
	res, err := store.Promote(memory.PromoteRequest{
		Lesson:      a.Lesson,
		Name:        a.Name,
		Description: a.Description,
		Class:       a.Class,
		SkillsDir:   e.memorySkillsDir(),
	}, e.memoryGate())
	if err != nil {
		return failJSON(err)
	}
	return memoryWriteResult(res)
}

// ---------- manage_skill ----------

func (e *Executor) manageSkill(args string) Result {
	store, err := e.memoryVerbs()
	if err != nil {
		return failJSON(err)
	}
	var a struct {
		Op          string `json:"op"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
		Reason      string `json:"reason"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if strings.TrimSpace(a.Op) == "" {
		return Result{Content: `{"error":"manage_skill: missing required property 'op' (create|update|delete)"}`, Failed: true}
	}
	if strings.TrimSpace(a.Name) == "" {
		return Result{Content: `{"error":"manage_skill: missing required property 'name'"}`, Failed: true}
	}
	// The skills directory is operator-configured; the declared scope and the
	// deny paths still apply to a workspace-local one.
	if _, err := e.memorySkillPath("manage_skill", a.Name); err != nil {
		return failJSON(err)
	}
	detail := fmt.Sprintf("manage_skill %s %s", strings.TrimSpace(a.Op), clip(a.Name, 60))
	if err := e.memoryWriteGuard("manage_skill", detail); err != nil {
		return failJSON(err)
	}
	res, err := store.ManageSkill(memory.SkillRequest{
		Op:          a.Op,
		Name:        a.Name,
		Description: a.Description,
		Body:        a.Body,
		Reason:      a.Reason,
		SkillsDir:   e.memorySkillsDir(),
	}, e.memoryGate())
	if err != nil {
		return failJSON(err)
	}
	return memoryWriteResult(res)
}

// ---------- reflect ----------

// NewReflector builds the synthesis seam `reflect` uses from a provider client:
// one plain completion, no tools, so a reflection can never call a tool or
// change the workspace. The harness wires the curator's model here
// (context.curator_model, falling back to the default model).
func NewReflector(client *provider.Client) memory.Reflector {
	return func(ctx context.Context, system, user string) (string, error) {
		if client == nil {
			return "", fmt.Errorf("reflect: no model is attached")
		}
		resp, err := client.Stream(ctx, []provider.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		}, nil, provider.StreamCallbacks{})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

func (e *Executor) reflect(ctx context.Context, args string) Result {
	store, err := e.memoryVerbs()
	if err != nil {
		return failJSON(err)
	}
	var a struct {
		Question   string  `json:"question"`
		MaxLessons flexInt `json:"max_lessons"`
		MaxTokens  flexInt `json:"max_tokens"`
	}
	if err := decode(args, &a); err != nil || strings.TrimSpace(a.Question) == "" {
		return Result{Content: `{"error":"reflect: missing required property 'question'"}`, Failed: true}
	}
	question := strings.TrimSpace(a.Question)
	bank := store.Bank(int(a.MaxLessons), int(a.MaxTokens))

	var reflector memory.Reflector
	if e.MemoryVerbs != nil {
		reflector = e.MemoryVerbs.Reflect
	}
	if reflector == nil {
		// No curator model is attached to this session. The honest answer is
		// the bank itself, labelled as such: never an invented synthesis.
		return Result{Content: bank + "\n\n(note: no curator model is attached to this session, so this is the " +
			"memory bank itself, not a synthesis over it)"}
	}
	system, user := memory.ReflectPrompt(bank, question)
	answer, err := reflector(ctx, system, user)
	if err != nil {
		return failJSON(err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return Result{Content: "the curator model returned no synthesis; the bank it read was:\n\n" + bank}
	}
	return Result{Content: answer}
}
