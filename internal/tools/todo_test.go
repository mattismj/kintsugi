package tools

import (
	"strings"
	"testing"
)

func TestTodoNormalizesAndEmits(t *testing.T) {
	var got []TodoItem
	ex := &Executor{OnTodo: func(items []TodoItem) { got = items }}
	res := dispatch(t, ex, "todo", `{"todos":[
		{"content":"read the repo","status":"done"},
		{"title":"patch the loop","status":"in progress","priority":"high"},
		{"text":"run tests","status":"todo","priority":"normal"}
	]}`)
	if res.Failed {
		t.Fatalf("todo failed: %+v", res)
	}
	if len(got) != 3 {
		t.Fatalf("callback items = %+v", got)
	}
	if got[0].Status != "completed" || got[0].Priority != "medium" {
		t.Fatalf("item 0 = %+v", got[0])
	}
	if got[1].Content != "patch the loop" || got[1].Status != "in_progress" || got[1].Priority != "high" {
		t.Fatalf("item 1 = %+v", got[1])
	}
	if got[2].Content != "run tests" || got[2].Status != "pending" {
		t.Fatalf("item 2 = %+v", got[2])
	}
	if !strings.Contains(res.Content, `"counts"`) || !strings.Contains(res.Content, `"completed":1`) {
		t.Fatalf("result = %s", res.Content)
	}
}

func TestTodoRejectsBadItems(t *testing.T) {
	ex := &Executor{}
	for _, args := range []string{
		`{"todos":[{"content":"","status":"pending"}]}`,
		`{"todos":[{"content":"x","status":"someday"}]}`,
		`{"todos":[{"content":"x","status":"pending","priority":"critical"}]}`,
	} {
		res := dispatch(t, ex, "todo", args)
		if !res.Failed {
			t.Fatalf("accepted invalid todo: %s", args)
		}
	}
}

func TestTodoRepairAliases(t *testing.T) {
	rep, err := Repair("todo", `{"items":"[{\"content\":\"x\",\"status\":\"pending\"}]"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Args, `"todos"`) || !contains(rep.Classes, "stringified-array") {
		t.Fatalf("repair = %+v", rep)
	}
	res := dispatch(t, &Executor{}, "todo", rep.Args)
	if res.Failed {
		t.Fatalf("repaired todo failed: %+v", res)
	}
}
