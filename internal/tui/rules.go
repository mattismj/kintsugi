package tui

import (
	"fmt"

	"kintsugi/internal/agent"
	"kintsugi/internal/intercept"
)

// noteRuleEvent records one stream-rule application in the transcript.
//
// It shows the rule name, the action and the reason — never the matched text.
// That is the whole point of the feature's shape: an operator rule can scrub or
// interrupt the model, and doing so must not quietly become a record of what the
// model said. A redactHappened note names the rule and the count; the span is
// gone by the time this is called.
func (m *Model) noteRuleEvent(ev agent.RuleEvent) {
	switch ev.Action {
	case intercept.ActionRedact:
		m.addNote(fmt.Sprintf("· redacted %d %s (%s)",
			ev.Count, plural(ev.Count, "span"), orDash(ev.Rule)))
	default:
		detail := ev.Action
		if ev.Reason != "" {
			detail += " · " + ev.Reason
		}
		m.addNote(fmt.Sprintf("✦ stream rule %s · %s", orDash(ev.Rule), detail))
	}
}
