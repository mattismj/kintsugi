// Package agent runs the turn loop: stream a turn, dispatch tool calls,
// continue until the model answers without one.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"kintsugi/internal/advisor"
	"kintsugi/internal/hooks"
	"kintsugi/internal/intercept"
	"kintsugi/internal/provider"
	"kintsugi/internal/tools"
)

type Loop struct {
	Client   *provider.Client
	Tools    *tools.Executor
	Log      Logger
	MaxLoops int
	OnDelta  func(string)
	// HardStop enables the hard/soft classification: a refusal that repeats
	// the same class, or whose thinking states an absolute position, stops
	// retrying. A reminder does not move a stonewall, and the retry budget
	// is better spent on refuses that can recover.
	HardStop bool
	// HardThinking enables the thinking-language signal within HardStop: an
	// absoluteness marker in the reasoning classifies hard before the second
	// retry. HardStop must be on for this to matter.
	HardThinking bool

	// OnThinking receives reasoning deltas as they arrive, before the answer.
	// The thinking is where a refusal forms first, and where the model's
	// false assumptions live — the operator reads it, the interceptor scans
	// it, and the TUI renders it. nil means the caller does not want the
	// stream; the thinking still lands on the assistant Message.
	OnThinking func(string)
	OnTool     func(call provider.ToolCall, res tools.Result)

	// Stream-level refusal interception (DESIGN §6.7). All optional.
	Interceptor       *intercept.Lexicon
	Reminder          func(level int, hit intercept.Hit) string
	OnIntercept       func(hit intercept.Hit, level int, outcome string)
	MaxRefusalRetries int

	// General stream rules (I-70). Optional: nil means the loop runs the refusal
	// lexicon alone, exactly as before. Abort and inject rules ride the same
	// retry path (and the same budget) as a refusal; redact rules never abort.
	//
	// OnRule receives the metadata of every rule application: rule, action,
	// reason and — for redact — a count. It never receives the matched text.
	StreamRules *intercept.Rules
	OnRule      func(ev RuleEvent)

	// During-the-work advisor (DESIGN §5). Optional; notes are shown to the
	// operator and blocker notes are fed back into the next request.
	Advisor             advisor.Reviewer
	OnAdvisor           func([]advisor.Note)
	MaxAdvisorRevisions int

	// Continuity guard hooks (M8.5). All optional and best-effort: a hook that
	// returns nothing leaves the loop byte-identical to a loop without it.
	//
	// PriorWork returns a rendered block of operator-accepted prior work for the
	// task; it is injected once, before the first request of a fresh session
	// (never on a resumed session that already has history).
	PriorWork func(task string) string
	// GuardRefusal returns a re-injected prior-work block and the ledger ids it
	// cites. It is called at most once per turn, on the first interception, and
	// only ever receives the task and the telemetry class label — never the
	// model's output, so a refusal cannot be echoed back into a prompt.
	GuardRefusal func(task, class string) (block string, matched []string)
	// OnDrift records that this turn drifted. It receives metadata only: the
	// task (the callee hashes it), the class, the outcome and the matched ids.
	OnDrift func(task, class, outcome string, matched []string)

	// OnUsage receives per-request token counts (exact when the provider
	// emits usage, approximate otherwise) for quota accounting.
	OnUsage func(promptTokens, completionTokens int)
	// OnCache receives prompt-cache counters when the provider reports them
	// (M10-A). nil is fine: the session log still carries the `cache` event.
	OnCache func(read, written int)

	// Mid-session compaction (M10.3). CompactAfter is the message count
	// above which the older turns are summarized before the next turn; zero
	// uses the default, a negative value disables compaction. CompactKeepRatio
	// is the share of the conversation kept verbatim. Summarize asks the model
	// for the summary; without it a long conversation is never compacted —
	// an unsummarized conversation is still a conversation.
	CompactAfter     int
	CompactKeepRatio float64
	Summarize        Summarizer
	// OnCompact reports a compaction (covered turns, kept turns).
	OnCompact func(covered, kept int)

	// Lifecycle hooks (M9.1). All optional: a nil runner means no hooks and
	// the loop behaves exactly as it did before hooks existed.
	//
	// Hooks may deny or constrain, never grant: a hook that answers "allow"
	// cannot widen what the permission layer allows, and an "ask" answer
	// leaves the tool to the existing approval flow.
	Hooks *hooks.Runner
	// SessionID and CWD are handed to the hooks as metadata.
	SessionID string
	CWD       string
	// Model and Persona are optional hook metadata.
	Model   string
	Persona string

	// sessionMu guards sessionStarted: the SessionStart hook fires once per
	// session (the first turn this loop runs), never once per turn.
	sessionMu      sync.Mutex
	sessionStarted bool
}

// Logger is intentionally tiny so the loop is testable without sessions.
type Logger interface {
	Log(kind string, v any)
}

type nopLogger struct{}

func (nopLogger) Log(string, any) {}

// Run executes one task from scratch.
func (l *Loop) Run(ctx context.Context, systemPrompt, task string) (string, error) {
	final, _, err := l.RunConversation(ctx, systemPrompt, nil, task)
	return final, err
}

// RunConversation executes one user turn against prior history. It returns the
// final assistant content and the full updated history (user turn + everything
// the loop produced), so a caller can keep a multi-turn session without
// re-compiling the prompt stack.
func (l *Loop) RunConversation(ctx context.Context, systemPrompt string, history []provider.Message, task string) (string, []provider.Message, error) {
	log := l.Log
	if log == nil {
		log = nopLogger{}
	}
	maxLoops := l.MaxLoops
	if maxLoops <= 0 {
		maxLoops = 8
	}

	msgs := make([]provider.Message, 0, len(history)+2)
	msgs = append(msgs, provider.Message{Role: "system", Content: systemPrompt})
	// Mid-session compaction (M10.3): summarize the older turns before the
	// turn when the conversation outgrew the threshold. The system prompt is
	// never rewritten, so the cached prefix stays byte-identical.
	if l.Summarize != nil && l.CompactAfter >= 0 && len(history) > l.CompactAfter {
		l.fireCompactHooks(ctx, hooks.PreCompact)
		res, err := Compact(ctx, history, CompactOptions{
			After:     l.CompactAfter,
			KeepRatio: l.CompactKeepRatio,
		}, l.Summarize)
		switch {
		case err != nil:
			// A failed compaction is not a failed turn.
			log.Log("compact_error", err.Error())
		case res.Changed:
			history = res.Messages
			log.Log("compact", map[string]any{"covered": res.Covered, "kept": res.Kept})
			if l.OnCompact != nil {
				l.OnCompact(res.Covered, res.Kept)
			}
			l.fireCompactHooks(ctx, hooks.PostCompact)
		}
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, provider.Message{Role: "user", Content: task})
	log.Log("user", task)

	// Prior accepted work is a fresh-session affordance: a resumed session
	// already carries its own history, so injecting it again would duplicate
	// context the operator has already seen (and would drift the prefix).
	if len(history) == 0 && l.PriorWork != nil {
		if block := strings.TrimSpace(l.PriorWork(task)); block != "" {
			msgs = append(msgs, provider.Message{Role: "system", Content: block})
			log.Log("prior_work", map[string]any{"bytes": len(block)})
		}
	}

	failedCalls := map[string]int{} // loop-break detector (M0 spike finding)
	advisorRevisions := 0
	maxAdvisorRevisions := l.MaxAdvisorRevisions
	if maxAdvisorRevisions <= 0 {
		maxAdvisorRevisions = 1
	}
	// stopReminderSent keeps the Stop hook to one continuation per turn.
	stopReminderSent := false

	// Lifecycle hooks (M9.1). SessionStart fires once per session — the first
	// turn this loop runs — and UserPromptSubmit fires before every user
	// turn. Both are no-ops when no runner is attached.
	if l.Hooks != nil {
		if l.claimSessionStart() {
			dec := l.fireHooks(ctx, log, hooks.Event{
				Name:  hooks.SessionStart,
				Extra: map[string]any{"resumed": len(history) > 0},
			})
			if c := strings.TrimSpace(dec.AdditionalContext); c != "" {
				msgs = append(msgs, provider.Message{Role: "system", Content: c})
			}
		}
		dec := l.fireHooks(ctx, log, hooks.Event{
			Name:      hooks.UserPromptSubmit,
			ToolInput: task,
		})
		if dec.Decision == "deny" {
			return "", history, fmt.Errorf("prompt blocked by hook: %s", hookReason(dec))
		}
		if c := strings.TrimSpace(dec.AdditionalContext); c != "" {
			msgs = append(msgs, provider.Message{Role: "system", Content: c})
		}
	}

	for i := 0; i <= maxLoops; i++ {
		a, err := l.streamTurn(ctx, &msgs, task, log)
		if err != nil {
			log.Log("error", err.Error())
			return "", history, err
		}
		// The thinking is persisted before the answer: it is where the
		// refusal forms and where the operator reads the model's assumptions.
		// Log redacts; the report and export consume it from the event stream.
		if strings.TrimSpace(a.Thinking) != "" {
			log.Log("thinking", a.Thinking)
		}
		log.Log("assistant", a.Content)
		if a.Usage != nil && (a.Usage.CacheReadTokens > 0 || a.Usage.CacheWriteTokens > 0) {
			// Prompt-cache telemetry (M10-A). The session's meta event carries
			// the model, so CacheStats can attribute this without it.
			log.Log("cache", map[string]any{
				"read":    a.Usage.CacheReadTokens,
				"written": a.Usage.CacheWriteTokens,
			})
			if l.OnCache != nil {
				l.OnCache(a.Usage.CacheReadTokens, a.Usage.CacheWriteTokens)
			}
		}
		if l.OnUsage != nil {
			p, c := approxTokens(msgs), approxTokensText(a.Content)
			if a.Usage != nil && a.Usage.PromptTokens > 0 {
				p, c = a.Usage.PromptTokens, a.Usage.CompletionTokens
			}
			l.OnUsage(p, c)
		}

		notes := l.consultAdvisor(ctx, msgs, log)
		blockers := advisor.Blockers(notes)

		if len(a.ToolCalls) == 0 {
			if len(blockers) > 0 && advisorRevisions < maxAdvisorRevisions {
				msgs = append(msgs, a)
				msgs = append(msgs, provider.Message{Role: "system", Content: advisor.Feedback(blockers)})
				advisorRevisions++
				log.Log("advisor_revise", map[string]any{"notes": blockers})
				continue
			}
			// Stop (M9.1): the turn is complete. A deny asks the model to
			// continue — once per turn, never in a loop.
			if l.Hooks != nil {
				dec := l.fireHooks(ctx, log, hooks.Event{
					Name:      hooks.Stop,
					ToolInput: task,
				})
				if dec.Decision == "deny" && !stopReminderSent {
					stopReminderSent = true
					msgs = append(msgs, a)
					msgs = append(msgs, provider.Message{Role: "system", Content: stopReminder(dec.Reason)})
					log.Log("hook_stop", map[string]any{"reason": dec.Reason})
					continue
				}
			}
			return a.Content, msgs[1:], nil
		}

		msgs = append(msgs, a)
		// Tool calls are executed one at a time, in the order the model emitted
		// them. This is the harness's contract, not an oversight: the prompt
		// says so (internal/adapters renders "Tool-call execution: serial" for
		// every model) and must keep saying so until this loop really is
		// concurrent.
		//
		// Why serial, and what a concurrent loop would have to preserve:
		//   - The approval gate is interactive: Executor.authorize blocks on the
		//     operator, and its session "approved" cache is not a scheduling
		//     primitive. Firing two prompts at once would let a mutating call
		//     race the answer that was meant for a different call.
		//   - Two calls must never mutate the same path at once. Serial
		//     execution gives that for free; a pool would need a per-path lock.
		//   - failedCalls below is a read-then-increment guard. Serial ordering
		//     makes "the identical call after a failure is not re-run" exact;
		//     concurrently it would be a race.
		//   - The tool results are appended in call order, never completion
		//     order, because models index them positionally.
		// Everything in this body — the duplicate-failure guard, PreToolUse, the
		// permission gate, PostToolUse, the log and OnTool callbacks — runs in
		// that one order, exactly once per call.
		for _, tc := range a.ToolCalls {
			key := tc.Function.Name + "\x00" + tc.Function.Arguments
			var res tools.Result
			executed := false
			if failedCalls[key] > 0 {
				// Identical call after failure: do not execute again. Inject a
				// course-correction note instead of another identical error.
				res = tools.Result{
					Failed:  true,
					Content: `{"error":"this exact call already failed once; it was not executed again","hint":"change the argument or the strategy — e.g. resolve the path, list a directory, or read a different file"}`,
				}
			} else {
				// PreToolUse (M9.1): the hook runs before the permission gate.
				// A deny stops the call — the model gets a failed result and
				// the loop continues. An "ask" leaves the call to the normal
				// approval flow; an "allow" widens nothing.
				denied, reason := l.preToolUse(ctx, log, tc)
				if denied {
					res = tools.Result{Failed: true, Content: blockedToolResult(reason)}
				} else {
					res = l.Tools.Dispatch(tc)
					executed = true
				}
			}
			if res.Failed {
				failedCalls[key]++
			}

			// PostToolUse (M9.1): the hook observes the result. Its additional
			// context is appended to the result text so the model sees it.
			if executed {
				dec := l.fireHooks(ctx, log, hooks.Event{
					Name:      hooks.PostToolUse,
					ToolName:  tc.Function.Name,
					ToolInput: tc.Function.Arguments,
					Extra: map[string]any{
						"tool_use_id":   tc.ID,
						"tool_response": res.Content,
					},
				})
				if c := strings.TrimSpace(dec.AdditionalContext); c != "" {
					res.Content = strings.TrimRight(res.Content, "\n") + "\n\n[hook] " + c
				}
			}

			log.Log("tool_call", tc)
			log.Log("tool_result", res.Content)
			if l.OnTool != nil {
				l.OnTool(tc, res)
			}
			msgs = append(msgs, provider.Message{
				Role:       "tool",
				Content:    res.Content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
		if len(blockers) > 0 && advisorRevisions < maxAdvisorRevisions {
			msgs = append(msgs, provider.Message{Role: "system", Content: advisor.Feedback(blockers)})
			advisorRevisions++
			log.Log("advisor_revise", map[string]any{"notes": blockers})
		}
	}
	return "", history, fmt.Errorf("tool loop exceeded %d iterations", maxLoops)
}

// ---- lifecycle hooks (M9.1) ------------------------------------------------

// claimSessionStart reports whether this is the first turn of the session.
// The SessionStart hook fires once per session, never once per turn.
func (l *Loop) claimSessionStart() bool {
	l.sessionMu.Lock()
	defer l.sessionMu.Unlock()
	if l.sessionStarted {
		return false
	}
	l.sessionStarted = true
	return true
}

// fireHooks runs the hooks for one event and logs the outcome. It never
// fails a turn: a hook error is telemetry, the decision is the policy.
func (l *Loop) fireHooks(ctx context.Context, log Logger, ev hooks.Event) hooks.Decision {
	if l.Hooks == nil {
		return hooks.Decision{}
	}
	ev.SessionID = l.SessionID
	ev.CWD = l.CWD
	if ev.Model == "" {
		ev.Model = l.Model
	}
	if ev.Persona == "" {
		ev.Persona = l.Persona
	}
	dec, err := l.Hooks.Fire(ctx, ev)
	if err != nil {
		log.Log("hook_error", map[string]any{
			"event": ev.Name, "tool": ev.ToolName, "error": err.Error(),
		})
	}
	return dec
}

// preToolUse fires the PreToolUse hook for one tool call. It reports whether
// the call is denied and why. An "ask" answer is deliberately not acted on:
// the call stays with the normal approval flow, and an "allow" answer grants
// nothing the permission layer would not grant anyway.
func (l *Loop) preToolUse(ctx context.Context, log Logger, tc provider.ToolCall) (bool, string) {
	if l.Hooks == nil {
		return false, ""
	}
	dec := l.fireHooks(ctx, log, hooks.Event{
		Name:      hooks.PreToolUse,
		ToolName:  tc.Function.Name,
		ToolInput: tc.Function.Arguments,
		Extra:     map[string]any{"tool_use_id": tc.ID},
	})
	if dec.Decision != "deny" {
		return false, ""
	}
	log.Log("hook_block", map[string]any{
		"event": hooks.PreToolUse, "tool": tc.Function.Name, "reason": dec.Reason,
	})
	return true, dec.Reason
}

// blockedToolResult is the failed tool result the model sees when a hook
// denies a call.
func blockedToolResult(reason string) string {
	msg := "blocked by hook"
	if r := strings.TrimSpace(reason); r != "" {
		msg += ": " + r
	}
	b, err := json.Marshal(map[string]any{"error": msg, "hook": hooks.PreToolUse})
	if err != nil {
		return `{"error":"blocked by hook"}`
	}
	return string(b)
}

// stopReminder is the single system reminder a denied Stop injects.
func stopReminder(reason string) string {
	b := &strings.Builder{}
	b.WriteString("## Harness reminder (stop denied by hook)\n\n")
	if r := strings.TrimSpace(reason); r != "" {
		b.WriteString("The Stop hook denied the end of this turn: " + r + "\n\n")
	}
	b.WriteString("Continue the task: address the reason above and keep working.")
	return b.String()
}

// hookReason is the reason of a decision, with a fallback for a deny that
// carries no reason at all.
func hookReason(dec hooks.Decision) string {
	if r := strings.TrimSpace(dec.Reason); r != "" {
		return r
	}
	return "no reason given"
}

// consultAdvisor never fails a turn: provider or parse errors are logged and
// the workspace agent continues. Notes are surfaced before they are used.
func approxTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) + len(m.ToolCallID) + len(m.Name)
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return (total + 3) / 4
}

func approxTokensText(s string) int { return (len(s) + 3) / 4 }

func (l *Loop) consultAdvisor(ctx context.Context, msgs []provider.Message, log Logger) []advisor.Note {
	if l.Advisor == nil || ctx.Err() != nil {
		return nil
	}
	notes, err := l.Advisor.Review(ctx, msgs)
	if err != nil {
		log.Log("advisor_error", err.Error())
		return nil
	}
	if len(notes) == 0 {
		return nil
	}
	log.Log("advisor_notes", map[string]any{"notes": notes})
	if l.OnAdvisor != nil {
		l.OnAdvisor(notes)
	}
	return notes
}

// RuleEvent is the metadata of one stream-rule application. It carries a rule
// name, an action, a reason and — for redact — a count. It has no field for the
// matched span, so a rule can never write model output anywhere: a refusal is
// still never memory, and redaction is still only a name and a count.
type RuleEvent struct {
	Rule    string `json:"rule"`
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	Level   int    `json:"level,omitempty"`   // 1-based; 0 for redact (never aborts)
	Outcome string `json:"outcome,omitempty"` // resumed | handoff | redacted
	Count   int    `json:"count,omitempty"`   // redact matches, 0 otherwise
}

// streamTurn streams one assistant turn and performs refusal interception.
// A hit aborts the stream, injects a reminder, and retries: 1st hit soft,
// 2nd hit strong, 3rd hands the turn back to the operator. Aborted partial
// output never enters the message history.
//
// General stream rules (I-70) reuse that loop rather than adding a second one:
// an abort or inject rule hit takes the same path, the same budget and the same
// handoff. Only the reminder differs — an inject rule supplies its own text, an
// abort rule gets a fixed harness note naming the rule. A redact rule never
// aborts: its spans are scrubbed out of the text before the transcript and the
// log see it, and only a rule name and a count are recorded.
//
// task is the operator's current turn. It is passed to the continuity hooks as
// the recall key and hashed into drift metadata; it is never combined with the
// model's output, so a refusal cannot be recorded or replayed.
func (l *Loop) streamTurn(ctx context.Context, msgs *[]provider.Message, task string, log Logger) (provider.Message, error) {
	retries := l.MaxRefusalRetries
	if retries <= 0 {
		retries = 2
	}

	lastClass := ""
	for attempt := 0; ; attempt++ {
		turnCtx, cancel := context.WithCancel(ctx)
		var acc strings.Builder // raw text: what the model said, used for matching
		var hit intercept.Hit
		aborted := false

		// Redaction is per attempt: a retried turn starts from nothing.
		var scrubber *intercept.Scrubber
		if l.StreamRules != nil {
			scrubber = l.StreamRules.Scrubber(l.Persona)
		}

		// The thinking accumulator: the interceptor scans it as it arrives,
		// because a refusal forms in the thinking before the answer commits
		// to it. The same abort path fires — cancel the context, let the
		// retry logic below handle recovery.
		var thinkingAcc strings.Builder
		hardFromThinking := false
		onThinking := func(s string) {
			thinkingAcc.WriteString(s)
			if l.OnThinking != nil {
				l.OnThinking(s)
			}
			// Scan the thinking with the same lexicon as the answer: the
			// language is more direct here ("I will not help with this"
			// in the thinking vs. a polished "I'm sorry" in the answer),
			// so the patterns catch earlier and the abort saves the rest
			// of the stonewall from being generated.
			if l.Interceptor != nil && !aborted {
				if h, ok := l.Interceptor.Scan(thinkingAcc.String()); ok {
					hit = h
					aborted = true
					// The early-warning hardness signal: absolute language in
					// the reasoning means the model has decided.
					if l.HardStop && l.HardThinking && intercept.Hardness(thinkingAcc.String()) {
						hardFromThinking = true
					}
					cancel()
				}
			}
		}

		onDelta := func(s string) {
			acc.WriteString(s)
			// Prefix match: the refusal is starting — abort before the rest
			// of the stonewall is generated. This fires earlier than the
			// full-pattern Scan, which only completes once the refusal is
			// already written. The boundary rule (sentence start) prevents
			// false positives on mid-sentence "I can't believe".
			if l.Interceptor != nil && !aborted {
				if h, ok := l.Interceptor.PrefixMatch(acc.String()); ok {
					hit = h
					aborted = true
					if l.HardStop && l.HardThinking && intercept.Hardness(thinkingAcc.String()) {
						hardFromThinking = true
					}
					// A prefix match carries the class; if the prefix is a
					// hard marker, classify hard immediately.
					for _, px := range l.Interceptor.Prefixes() {
						if strings.EqualFold(px.Text, h.Phrase) && px.Hard {
							hardFromThinking = true
						}
					}
					cancel()
				}
			}
			// Operator rules take precedence over the refusal lexicon: they are
			// the more specific statement of what this workspace wants. A redact
			// rule never stops the stream, so only aborting rules are checked.
			if l.StreamRules != nil && !aborted {
				if h, ok := l.StreamRules.MatchAbort(acc.String(), l.Persona); ok {
					hit = *h
					aborted = true
					cancel()
				}
			}
			if l.Interceptor != nil && !aborted {
				if h, ok := l.Interceptor.Scan(acc.String()); ok {
					hit = h
					aborted = true
					cancel()
				}
			}
			shown := s
			if scrubber != nil {
				shown = scrubber.Write(s)
			}
			if shown != "" && l.OnDelta != nil {
				l.OnDelta(shown)
			}
		}

		cb := provider.StreamCallbacks{OnDelta: onDelta, OnThinking: onThinking}
		a, err := l.Client.Stream(turnCtx, *msgs, l.Tools.Definitions(), cb)
		cancel()

		// Classifier refusal walk (A2). A refusal finish reason is not the
		// intercept lexicon's case — the model answered, it answered "no". On the
		// first attempt, before any same-model reminder, walk the fallback chain
		// so a sibling open-weights model can answer what this one refused. Each
		// model trips at most once; the walk is a provider-client move, so the
		// retry budget below still governs same-model reminders.
		if err == nil && attempt == 0 && provider.RefusalFinish(a.FinishReason) && len(l.Client.Fallbacks) > 0 {
			a, err = l.Client.StreamRefusal(turnCtx, *msgs, l.Tools.Definitions(), cb)
		}

		outcome := "resumed"
		lastClassAtHit := hit.Class
		if aborted && attempt+1 > retries {
			outcome = "handoff"
		}
		// The hard/soft classification. A stonewall is a refusal that will
		// not move: the same class hit again in this turn, or absolute
		// language in the thinking on the first hit. Retrying those burns a
		// full turn for nothing, so the loop stops and says so.
		if aborted && l.HardStop && hit.Rule == "" {
			if hardFromThinking || (attempt > 0 && lastClass != "" && lastClass == lastClassAtHit) {
				outcome = "hard"
			}
		}
		// An aborted-and-retried attempt's text is discarded, so its held-back
		// tail and its redaction telemetry are discarded with it. What an
		// accepted or handed-off attempt shows is flushed and accounted for
		// here: the log line is a rule name and a count, and the spans
		// themselves are nowhere.
		partial := acc.String()
		if scrubber != nil {
			// Whatever is logged or shown is scrubbed, even when the attempt is
			// then thrown away: a redact span must never reach a log line.
			partial = scrubber.Seen()
			if !aborted || outcome == "handoff" {
				if tail := scrubber.Flush(); tail != "" && l.OnDelta != nil {
					l.OnDelta(tail)
				}
				partial = scrubber.Seen()
				for _, red := range scrubber.Redactions() {
					log.Log("stream_rule", map[string]any{
						"rule": red.Rule, "action": red.Action, "reason": red.Reason,
						"count": red.Count,
					})
					if l.OnRule != nil {
						l.OnRule(RuleEvent{
							Rule: red.Rule, Action: red.Action, Reason: red.Reason,
							Outcome: "redacted", Count: red.Count,
						})
					}
				}
			}
		}

		if !aborted {
			// Only a redacted stream is rewritten; a stream with no redact rule
			// returns the provider's message untouched.
			if scrubber != nil {
				a.Content = partial
			}
			return a, err
		}

		if hit.Rule != "" {
			// A rule hit is stream_rule telemetry, never a refusal: the refusal
			// event keeps its exact shape so refusal stats/trends still count
			// only the refusal lexicon.
			log.Log("stream_rule", map[string]any{
				"rule": hit.Rule, "action": hit.Action, "reason": hit.Reason,
				"level": attempt + 1, "outcome": outcome,
			})
			if l.OnRule != nil {
				l.OnRule(RuleEvent{
					Rule: hit.Rule, Action: hit.Action, Reason: hit.Reason,
					Level: attempt + 1, Outcome: outcome,
				})
			}
		} else {
			log.Log("refusal", map[string]any{
				"class": hit.Class, "phrase": hit.Phrase,
				"level": attempt + 1, "outcome": outcome,
				// partial is the scrubbed text: a redact rule must never write
				// its span here, and with no redact rule it is the raw bytes.
				"partial": partial,
			})
			if l.OnIntercept != nil {
				l.OnIntercept(hit, attempt+1, outcome)
			}
		}

		// The continuity guard runs at most once per turn, on the first
		// interception: the block it returns is prior *accepted* work, which
		// does not change from retry to retry. Its ids are the drift metadata.
		var guardBlock string
		var matched []string
		if attempt == 0 && l.GuardRefusal != nil {
			guardBlock, matched = l.GuardRefusal(task, hit.Class)
		}
		// Drift is metadata: task, class, outcome, matched ids. The model's
		// output is not part of it and never becomes memory.
		if l.OnDrift != nil {
			l.OnDrift(task, hit.Class, outcome, matched)
		}

		if outcome == "handoff" || outcome == "hard" {
			return provider.Message{Content: partial + handoffNote(hit, attempt+1, outcome)}, nil
		}
		lastClass = lastClassAtHit

		reminder := ""
		if hit.Rule != "" {
			// The operator's own rule text, or a fixed harness note naming the
			// rule. Never the model's phrase: a rule hit has none.
			reminder = intercept.RuleReminder(attempt+1, hit)
		} else {
			if l.Reminder != nil {
				reminder = l.Reminder(attempt+1, hit)
			}
			if reminder == "" {
				reminder = intercept.BuildReminder(attempt+1, hit, "", "", nil)
			}
		}
		// A no-match guard contributes nothing, so the injected reminder stays
		// byte-identical to the pre-continuity behavior.
		if guardBlock != "" {
			reminder = reminder + "\n\n" + guardBlock
		}
		*msgs = append(*msgs, provider.Message{Role: "system", Content: reminder})
	}
}

// handoffNote is the one visible seam left when the retry budget runs out. The
// refusal wording is unchanged; a stream rule names itself instead.
func handoffNote(hit intercept.Hit, level int, outcome string) string {
	if outcome == "hard" {
		return fmt.Sprintf("\n\n[harness: hard refusal — this model is stonewalling on this task class after %d hit(s); re-injecting authorization will not move it. Switch models with /model, or rephrase the task at a different angle.]", level)
	}
	if hit.Rule == "" {
		return fmt.Sprintf("\n\n[harness: refusal persisted after %d reminders — handing back to the operator. Use /frame to re-inject the authorization sections.]", level)
	}
	return fmt.Sprintf("\n\n[harness: stream rule %q still matched after %d reminders — handing back to the operator. Use /frame to re-inject the authorization sections.]", hit.Rule, level)
}
