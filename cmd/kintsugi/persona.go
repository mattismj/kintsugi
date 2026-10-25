package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/persona"
	"kintsugi/internal/session"
)

// Trigger-map maintenance from telemetry (M8.5.4).
//
// This command closes the refusal-telemetry loop without ever storing a refusal:
//
//   - it reads the *matched filter term* recorded beside each interception (the
//     telemetry "phrase") and the class/outcome/ts — never the refused prose,
//     which is not even decoded here, because a refusal is telemetry, never
//     memory;
//   - it proposes operator-reviewable term → rephrase pairs against the persona
//     that the default model would compile;
//   - with --apply it appends those pairs — term, instead, class, added only — to
//     the persona file. Nothing else is written, and nothing about the refusal
//     reaches learned.md, the ledger or a skill.

func cmdPersona(ws string, args []string) {
	if len(args) == 0 {
		fail("usage: kintsugi persona triggers [--since YYYY-MM-DD] [--min N] [--apply]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "triggers":
		cmdPersonaTriggers(ws, rest)
	default:
		fail("usage: kintsugi persona triggers [--since YYYY-MM-DD] [--min N] [--apply]")
	}
}

func cmdPersonaTriggers(ws string, args []string) {
	fs := flag.NewFlagSet("persona triggers", flag.ExitOnError)
	since := fs.String("since", "", "only count refusal events on or after this date (YYYY-MM-DD)")
	minCount := fs.Int("min", 2, "minimum refusal events behind one proposal")
	apply := fs.Bool("apply", false, "append the proposals to the matched persona file")
	fs.Parse(args)

	sinceDate := strings.TrimSpace(*since)
	if sinceDate != "" {
		if _, err := time.Parse(persona.DateAddedLayout, sinceDate); err != nil {
			fail("--since must be YYYY-MM-DD (got %q)", sinceDate)
		}
	}

	app := newApp(ws)
	if app.Pack == nil {
		fail("persona pack not found at %s", config.ExpandHome(app.Cfg.PersonaPack.Dir))
	}

	// The persona that the default model would compile is the one whose trigger
	// map a proposal belongs to.
	res, err := app.Resolve("")
	if err != nil {
		fail("%v", err)
	}
	sel, err := app.Pack.Select("", res.Model.Family, res.Model.ID, res.Capabilities())
	if err != nil {
		fail("%v", err)
	}
	per := sel.Persona

	events, err := refusalEvents()
	if err != nil {
		fail("%v", err)
	}
	scoped := eventsForPersona(events, per.Name)
	proposals := persona.ProposeTriggers(scoped, per.Triggers.Known, sinceDate, *minCount)

	dir, _ := session.Dir()
	stats, _ := session.RefusalStats(dir)

	fmt.Printf("persona %s (rule: %s)\n  %s\n", per.Name, sel.Rule, per.Path)
	if line := telemetrySummary(stats); line != "" {
		fmt.Printf("telemetry %s\n", line)
	}
	fmt.Println("· refusals are telemetry, never memory: only the matched filter term, its class, the rephrase and a date are ever written")

	if len(proposals) == 0 {
		fmt.Printf("\nno proposals: %d refusal event(s) in scope%s did not cluster into a new trigger term\n",
			len(scoped), sinceSuffix(sinceDate))
		fmt.Println("· a proposal needs at least " + fmt.Sprint(*minCount) + " events matching the same phrase (--min)")
		return
	}

	fmt.Printf("\n%d proposal(s) from %d refusal event(s)%s:\n\n", len(proposals), len(scoped), sinceSuffix(sinceDate))
	for i, p := range proposals {
		fmt.Printf("%d. term:    %s\n   instead: %s\n   class %s · added %s\n   %s\n",
			i+1, p.Term, p.Instead, orDash(p.Class), p.ToTrigger().Added, p.Reason)
	}

	if !*apply {
		fmt.Println("\n· review, then re-run with --apply to append these to the persona file")
		return
	}
	if _, err := os.Stat(per.Path); err != nil {
		fail("persona file %s is not on disk (%v): run `kintsugi setup` to materialize the pack, then retry", per.Path, err)
	}

	triggers := make([]persona.Trigger, 0, len(proposals))
	for _, p := range proposals {
		triggers = append(triggers, p.ToTrigger())
	}
	n, err := persona.AppendTriggers(per.Path, triggers)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("\napplied %d (review with kintsugi pack show)\n", n)
}

// sinceSuffix renders the --since window for an operator-facing sentence.
func sinceSuffix(since string) string {
	if since == "" {
		return ""
	}
	return " since " + since
}

// telemetrySummary is a counter line: how much interception telemetry the report
// is built from, and across how many classes. The counts are integers — the
// phrase that produced one was dropped at the telemetry boundary.
func telemetrySummary(stats []session.RefusalStat) string {
	if len(stats) == 0 {
		return ""
	}
	total := 0
	classes := map[string]bool{}
	for _, s := range stats {
		total += s.Count
		if c := strings.TrimSpace(s.Class); c != "" {
			classes[c] = true
		}
	}
	return fmt.Sprintf("%d interception(s) across %d class(es) (counters only)", total, len(classes))
}

// refusalEvents reads the refusal telemetry event by event.
//
// Two things are taken from each record: the matched filter *term* (recorded as
// the telemetry "phrase") with its class/outcome/ts, and the session's model and
// persona. The refused prose is never decoded — refusalEventLine has no field
// for it — so nothing here can carry a transcript into a proposal.
//
// The event stream is used rather than the aggregate because the aggregate keeps
// counters only: clustering needs the term, and --since needs a timestamp.
func refusalEvents() ([]persona.RefusalEvent, error) {
	ids, err := session.List()
	if err != nil {
		return nil, err
	}
	var out []persona.RefusalEvent
	for _, id := range ids {
		var buf bytes.Buffer
		if err := session.ExportJSONL(id, &buf); err != nil {
			continue // one unreadable session must not sink the report
		}
		out = append(out, refusalEventsFrom(buf.Bytes())...)
	}
	return out, nil
}

// refusalEventLine is the subset of a session event this command is allowed to
// see. There is deliberately no field for the refused prose.
type refusalEventLine struct {
	TS   string `json:"ts"`
	Kind string `json:"kind"`
	V    struct {
		Model   string `json:"model"`
		Persona string `json:"persona"`
		Class   string `json:"class"`
		Phrase  string `json:"phrase"`
		Outcome string `json:"outcome"`
	} `json:"v"`
}

// refusalEventsFrom extracts the refusal telemetry from one session's event
// stream, carrying the session's model/persona forward from its meta event.
func refusalEventsFrom(data []byte) []persona.RefusalEvent {
	var model, personaName string
	var out []persona.RefusalEvent
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var ev refusalEventLine
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		switch ev.Kind {
		case "meta":
			if s := strings.TrimSpace(ev.V.Model); s != "" {
				model = s
			}
			if s := strings.TrimSpace(ev.V.Persona); s != "" {
				personaName = s
			}
		case "refusal":
			term := strings.TrimSpace(ev.V.Phrase)
			if term == "" {
				continue // an event without a usable term cannot propose one
			}
			out = append(out, persona.RefusalEvent{
				Model:   model,
				Persona: personaName,
				Class:   ev.V.Class,
				Phrase:  term,
				Outcome: ev.V.Outcome,
				TS:      ev.TS,
			})
		}
	}
	return out
}

// eventsForPersona keeps the telemetry that belongs to the selected persona. A
// record with no persona is kept: it is still refusal telemetry for this model,
// just from a session that never logged a persona.
func eventsForPersona(events []persona.RefusalEvent, name string) []persona.RefusalEvent {
	want := strings.ToLower(strings.TrimSpace(name))
	var out []persona.RefusalEvent
	for _, ev := range events {
		switch p := strings.ToLower(strings.TrimSpace(ev.Persona)); p {
		case "", "unknown":
			out = append(out, ev)
		default:
			if p == want {
				out = append(out, ev)
			}
		}
	}
	return out
}
