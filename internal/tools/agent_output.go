package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kintsugi/internal/hub"
	"kintsugi/internal/provider"
)

// agent_output (M10-D) is the collect side of a detached run: read its status,
// wait for its result, or kill it. It speaks only to a run id, and the hub
// carries no authority, so this tool grants none.
func agentOutputTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "agent_output",
			Description: "Collect a background task started with task(background=true). action=status reports one run, action=wait blocks for its result, action=kill cancels it.",
			Parameters: provider.ToolParameters{
				Type:     "object",
				Required: []string{"id"},
				Properties: map[string]provider.ToolProp{
					"id":      {Type: "string", Description: "The run id task(background=true) returned."},
					"action":  {Type: "string", Description: "status | wait | kill (default status)."},
					"timeout": {Type: "integer", Description: "For wait: seconds to block before giving up (default 300, max 3600)."},
				},
			},
		},
	}
}

const (
	agentOutputDefaultWaitS = 300
	agentOutputMaxWaitS     = 3600
)

func (e *Executor) agentOutput(ctx context.Context, args string) Result {
	var a struct {
		ID      string `json:"id"`
		Action  string `json:"action"`
		Timeout int    `json:"timeout"`
	}
	if err := decode(args, &a); err != nil {
		return failJSON(err)
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return failJSON(fmt.Errorf("agent_output: id is required"))
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		action = "status"
	}
	if e.Hub == nil {
		return failJSON(fmt.Errorf("agent_output: no background hub is configured in this session"))
	}

	switch action {
	case "status":
		run, ok := e.Hub.Status(id)
		if !ok {
			return failJSON(fmt.Errorf("agent_output: unknown run %q", id))
		}
		return jsonResult(runPayload(run, false))

	case "wait":
		run, ok := e.Hub.Status(id)
		if !ok {
			return failJSON(fmt.Errorf("agent_output: unknown run %q", id))
		}
		if isTerminated(run.State) {
			return jsonResult(runPayload(run, true))
		}
		secs := a.Timeout
		if secs <= 0 {
			secs = agentOutputDefaultWaitS
		}
		if secs > agentOutputMaxWaitS {
			secs = agentOutputMaxWaitS
		}
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
		_, _ = e.Hub.WaitContext(waitCtx, id)

		fresh, ok := e.Hub.Status(id)
		if !ok {
			return failJSON(fmt.Errorf("agent_output: run %q disappeared while waiting", id))
		}
		payload := runPayload(fresh, true)
		if !isTerminated(fresh.State) {
			// The wait expired, not the run. Report the live state rather than
			// failing, so the model can wait again or move on.
			payload["pending"] = true
			payload["note"] = fmt.Sprintf("still running after %ds; call agent_output again to keep waiting", secs)
		}
		return jsonResult(payload)

	case "kill":
		if err := e.Hub.Kill(id); err != nil {
			return failJSON(err)
		}
		run, ok := e.Hub.Status(id)
		if !ok {
			return jsonResult(map[string]any{"id": id, "state": string(hub.StateKilled)})
		}
		return jsonResult(runPayload(run, true))

	default:
		return failJSON(fmt.Errorf("agent_output: unknown action %q (want status, wait, or kill)", a.Action))
	}
}

// isTerminated reports whether a run has finished, however it finished.
func isTerminated(s hub.RunState) bool {
	return s == hub.StateDone || s == hub.StateFailed || s == hub.StateKilled
}

// runPayload renders one run for the model. With result it also carries the
// run's output text.
func runPayload(run *hub.Run, result bool) map[string]any {
	payload := map[string]any{
		"id":         run.ID,
		"title":      run.Title,
		"role":       run.Role,
		"model":      run.Model,
		"state":      string(run.State),
		"started":    run.StartedAt.UTC().Format(time.RFC3339),
		"elapsed_s":  int(run.Elapsed().Seconds()),
		"tokens_est": run.Tokens,
	}
	if isTerminated(run.State) {
		payload["ended"] = run.EndedAt.UTC().Format(time.RFC3339)
	}
	if run.Error != "" {
		payload["error"] = run.Error
	}
	if result && run.Result != nil && run.Result.Content != "" {
		payload["result"] = run.Result.Content
	}
	return payload
}

// jsonResult marshals a tool payload, failing the call if it cannot be encoded.
func jsonResult(v any) Result {
	b, err := json.Marshal(v)
	if err != nil {
		return failJSON(err)
	}
	return Result{Content: string(b)}
}
