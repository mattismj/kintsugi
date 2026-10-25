package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"kintsugi/internal/provider"
)

// TodoItem is one operator-visible plan entry. The list is session state, not
// project state: a resumed session starts with an empty plan unless the model
// sends it again.
type TodoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

func todoTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name: "todo",
			Description: "Replace the session plan with the complete task list. Send every item each time, including completed ones. " +
				"Status: pending, in_progress, completed, or cancelled. Use this for multi-step work so the operator can see what is done and what is next.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"todos"},
				Properties: map[string]provider.ToolProp{
					"todos": {
						Type:        "array",
						Description: "Complete ordered plan.",
						Items: &provider.ToolProp{
							Type: "object",
							Properties: map[string]provider.ToolProp{
								"content":  {Type: "string", Description: "One concrete step."},
								"status":   {Type: "string", Enum: []any{"pending", "in_progress", "completed", "cancelled"}},
								"priority": {Type: "string", Enum: []any{"high", "medium", "low"}},
							},
							Required: []string{"content", "status"},
						},
					},
				},
			},
		},
	}
}

func (e *Executor) todo(args string) Result {
	var a struct {
		Todos []struct {
			Content  string `json:"content"`
			Title    string `json:"title"`
			Text     string `json:"text"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
		} `json:"todos"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	if len(a.Todos) > 50 {
		return failJSON(fmt.Errorf("todo: at most 50 items are supported, got %d", len(a.Todos)))
	}
	items := make([]TodoItem, 0, len(a.Todos))
	counts := map[string]int{}
	for i, raw := range a.Todos {
		content := strings.TrimSpace(raw.Content)
		if content == "" {
			content = strings.TrimSpace(raw.Title)
		}
		if content == "" {
			content = strings.TrimSpace(raw.Text)
		}
		if content == "" {
			return failJSON(fmt.Errorf("todo: item %d has empty content", i+1))
		}
		status, ok := normalizeTodoStatus(raw.Status)
		if !ok {
			return failJSON(fmt.Errorf("todo: item %d has unknown status %q (use pending, in_progress, completed, cancelled)", i+1, raw.Status))
		}
		priority, ok := normalizeTodoPriority(raw.Priority)
		if !ok {
			return failJSON(fmt.Errorf("todo: item %d has unknown priority %q (use high, medium, low)", i+1, raw.Priority))
		}
		items = append(items, TodoItem{Content: content, Status: status, Priority: priority})
		counts[status]++
	}
	if e.OnTodo != nil {
		e.OnTodo(append([]TodoItem(nil), items...))
	}
	b, _ := json.Marshal(map[string]any{
		"ok":     true,
		"todos":  items,
		"counts": counts,
	})
	return Result{Content: string(b)}
}

func normalizeTodoStatus(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	switch s {
	case "", "pending", "todo", "open":
		return "pending", true
	case "in-progress", "doing", "active", "running":
		return "in_progress", true
	case "completed", "complete", "done", "finished":
		return "completed", true
	case "cancelled", "canceled", "dropped", "skipped":
		return "cancelled", true
	}
	return "", false
}

func normalizeTodoPriority(p string) (string, bool) {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "", "medium", "normal":
		return "medium", true
	case "high", "urgent":
		return "high", true
	case "low":
		return "low", true
	}
	return "", false
}
