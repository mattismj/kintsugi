package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kintsugi/internal/provider"
)

type fakeTasks struct {
	got  []TaskRequest
	out  string
	err  error
	seen context.Context
}

func (f *fakeTasks) RunTasks(ctx context.Context, tasks []TaskRequest) (string, error) {
	f.seen = ctx
	f.got = append([]TaskRequest{}, tasks...)
	return f.out, f.err
}

func TestTaskToolRegisteredOnlyWhenEnabled(t *testing.T) {
	e := &Executor{Workspace: t.TempDir()}
	if hasTool(e.Definitions(), "task") {
		t.Fatal("task registered without a runner")
	}
	e.Tasks = &fakeTasks{}
	if !hasTool(e.Definitions(), "task") {
		t.Fatal("task not registered with a runner")
	}
}

func TestTaskDispatch(t *testing.T) {
	f := &fakeTasks{out: `{"results":[]}`}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name:      "task",
		Arguments: `{"tasks":[{"title":"a","prompt":"do a"},{"title":"b","prompt":"do b","agent":"opsec"}]}`,
	}})
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	if len(f.got) != 2 || f.got[1].Agent != "opsec" || f.got[0].Title != "a" {
		t.Fatalf("tasks = %+v", f.got)
	}
	if f.seen == nil {
		t.Fatal("context not propagated")
	}
}

func TestTaskDispatchRoleParsed(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name:      "task",
		Arguments: `{"tasks":[{"title":"r","prompt":"research it","role":"researcher"},{"title":"c","prompt":"code it","role":"Coder"}]}`,
	}})
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	if len(f.got) != 2 {
		t.Fatalf("tasks = %+v", f.got)
	}
	if f.got[0].Role != "researcher" {
		t.Fatalf("role = %q, want researcher", f.got[0].Role)
	}
	// Roles are normalized to the schema's lowercase enum.
	if f.got[1].Role != "coder" {
		t.Fatalf("role = %q, want coder", f.got[1].Role)
	}
}

func TestTaskDispatchRoleDefaultsToCoder(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name:      "task",
		Arguments: `{"tasks":[{"title":"a","prompt":"do a"}]}`,
	}})
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	if len(f.got) != 1 || f.got[0].Role != "coder" {
		t.Fatalf("tasks = %+v, want role coder", f.got)
	}
	if got := (TaskRequest{}).ModelRole(); got != "coder" {
		t.Fatalf("ModelRole() = %q", got)
	}
	for _, role := range TaskRoles {
		if !ValidModelRole(role) {
			t.Fatalf("%q not accepted by ValidModelRole", role)
		}
	}
	if ValidModelRole("reviewer") {
		t.Fatal("unknown role accepted")
	}
}

func TestTaskToolRoleSchema(t *testing.T) {
	defs := (&Executor{Workspace: t.TempDir(), Tasks: &fakeTasks{}}).Definitions()
	var schema *provider.ToolProp
	for _, d := range defs {
		if d.Function.Name != "task" {
			continue
		}
		items := d.Function.Parameters.Properties["tasks"].Items
		if items == nil {
			t.Fatal("task schema has no item schema")
		}
		p, ok := items.Properties["role"]
		if !ok {
			t.Fatal("task item schema has no role property")
		}
		schema = &p
	}
	if schema == nil {
		t.Fatal("task tool not registered")
	}
	if schema.Type != "string" || len(schema.Enum) != 3 {
		t.Fatalf("role schema = %+v", schema)
	}
	want := map[string]bool{"coder": false, "researcher": false, "opsec": false}
	for _, v := range schema.Enum {
		s, _ := v.(string)
		if _, ok := want[s]; !ok {
			t.Fatalf("unexpected enum value %v", v)
		}
		want[s] = true
	}
	for role, seen := range want {
		if !seen {
			t.Fatalf("enum missing %q: %+v", role, schema.Enum)
		}
	}
}

func TestTaskDispatchAcceptsStringifiedArray(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name:      "task",
		Arguments: `{"tasks":"[{\"title\":\"a\",\"prompt\":\"do a\"}]"}`,
	}})
	if res.Failed || len(f.got) != 1 {
		t.Fatalf("res=%+v tasks=%+v", res, f.got)
	}
}

func TestTaskDispatchValidation(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	cases := []string{
		`{}`,
		`{"tasks":[]}`,
		`{"tasks":[{"title":"","prompt":"x"}]}`,
		`{"tasks":[{"title":"a","prompt":"x"},{"title":"b","prompt":"x"},{"title":"c","prompt":"x"},{"title":"d","prompt":"x"},{"title":"e","prompt":"x"}]}`,
		`{"tasks":"not json"}`,
		`{"tasks":[{"title":"a","prompt":"x","role":"wizard"}]}`,
	}
	for _, args := range cases {
		res := e.Dispatch(provider.ToolCall{Function: provider.Function{Name: "task", Arguments: args}})
		if !res.Failed {
			t.Errorf("args %s accepted", args)
		}
	}
	f.err = errors.New("git missing")
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name: "task", Arguments: `{"tasks":[{"title":"a","prompt":"x"}]}`,
	}})
	if !res.Failed || !strings.Contains(res.Content, "git missing") {
		t.Fatalf("runner error = %+v", res)
	}
}

func hasTool(defs []provider.Tool, name string) bool {
	for _, d := range defs {
		if d.Function.Name == name {
			return true
		}
	}
	return false
}

// ---------- M8.5.5 typed handoff ----------

func TestTaskDispatchContextHandoff(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name: "task",
		Arguments: `{"tasks":[
			{"title":"a","prompt":"do a","context":"  accepted plan L-7: bundle types are additive  "},
			{"title":"b","prompt":"do b"}
		]}`,
	}})
	if res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	if len(f.got) != 2 {
		t.Fatalf("tasks = %+v", f.got)
	}
	if got, want := f.got[0].Context, "accepted plan L-7: bundle types are additive"; got != want {
		t.Fatalf("context = %q, want %q", got, want)
	}
	// No handoff is an empty handoff, not an error and not a refusal.
	if f.got[1].Context != "" {
		t.Fatalf("context = %q, want empty", f.got[1].Context)
	}
}

func TestTaskDispatchContextIsBounded(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	huge := strings.Repeat("handoff detail ", 1000)
	args := `{"tasks":[{"title":"a","prompt":"do a","context":"` + huge + `"}]}`
	if res := e.Dispatch(provider.ToolCall{Function: provider.Function{Name: "task", Arguments: args}}); res.Failed {
		t.Fatalf("dispatch failed: %s", res.Content)
	}
	got := f.got[0].Context
	if len(got) > taskContextMaxBytes+len("\n…[handoff truncated]") {
		t.Fatalf("context length = %d, want <= %d", len(got), taskContextMaxBytes)
	}
	if !strings.HasSuffix(got, "[handoff truncated]") {
		t.Fatalf("context = %q…", got[len(got)-60:])
	}
	if got != normalizeTaskContext(huge) {
		t.Fatal("dispatch did not apply the same normalization")
	}
}

func TestTaskToolContextSchema(t *testing.T) {
	defs := (&Executor{Workspace: t.TempDir(), Tasks: &fakeTasks{}}).Definitions()
	for _, d := range defs {
		if d.Function.Name != "task" {
			continue
		}
		items := d.Function.Parameters.Properties["tasks"].Items
		if items == nil {
			t.Fatal("task schema has no item schema")
		}
		ctx, ok := items.Properties["context"]
		if !ok {
			t.Fatal("task item schema has no context property")
		}
		if ctx.Type != "string" {
			t.Fatalf("context schema = %+v", ctx)
		}
		if !strings.Contains(ctx.Description, "handoff") {
			t.Fatalf("context description = %q", ctx.Description)
		}
		// The handoff is optional: title + prompt stay the only required fields.
		for _, req := range items.Required {
			if req == "context" {
				t.Fatal("context must not be required")
			}
		}
		return
	}
	t.Fatal("task tool not registered")
}

func TestTaskContextSurvivesStringifiedArray(t *testing.T) {
	f := &fakeTasks{out: "ok"}
	e := &Executor{Workspace: t.TempDir(), Tasks: f, Policy: Policy{Yolo: true}}
	res := e.Dispatch(provider.ToolCall{Function: provider.Function{
		Name:      "task",
		Arguments: `{"tasks":"[{\"title\":\"a\",\"prompt\":\"do a\",\"context\":\"handoff\"}]"}`,
	}})
	if res.Failed || len(f.got) != 1 || f.got[0].Context != "handoff" {
		t.Fatalf("res=%+v tasks=%+v", res, f.got)
	}
}
