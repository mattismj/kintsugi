package persona

import (
	"fmt"
	"strings"
)

// Lint is one actionable finding about a resolved persona stack. Errors mean
// the effective stack is broken or self-defeating; warnings are worth fixing
// before the next cloud session.
type Lint struct {
	Level   string // error | warning | info
	Persona string
	Message string
}

// LintSelection checks the effective chain rather than one file in isolation:
// duplicate trigger terms, missing classes, and inheritance gaps only matter
// for the stack that actually compiles.
func (p *Pack) LintSelection(sel *Selection) []Lint {
	if sel == nil {
		return nil
	}
	var out []Lint

	rewrites := map[string]string{}
	for _, per := range sel.Chain {
		for i, t := range per.Triggers.Known {
			term := strings.TrimSpace(t.Term)
			instead := strings.TrimSpace(t.Instead)
			label := per.Name
			if label == "" {
				label = fmt.Sprintf("chain[%d]", i)
			}
			if term == "" {
				out = append(out, Lint{Level: "error", Persona: label,
					Message: fmt.Sprintf("triggers.known[%d] has an empty term", i)})
				continue
			}
			if instead == "" {
				out = append(out, Lint{Level: "error", Persona: label,
					Message: fmt.Sprintf("trigger %q has no honest rephrase", term)})
			} else if strings.Contains(strings.ToLower(instead), strings.ToLower(term)) {
				out = append(out, Lint{Level: "error", Persona: label,
					Message: fmt.Sprintf("trigger %q rephrase still contains the trigger term", term)})
			}
			if strings.TrimSpace(t.Class) == "" {
				out = append(out, Lint{Level: "warning", Persona: label,
					Message: fmt.Sprintf("trigger %q has no class; it will be offered for every interception", term)})
			}
			key := strings.ToLower(term)
			if prev, ok := rewrites[key]; ok && prev != instead {
				out = append(out, Lint{Level: "warning", Persona: label,
					Message: fmt.Sprintf("trigger %q is defined twice with different rephrases", term)})
			} else {
				rewrites[key] = instead
			}
		}
	}

	if sel.Persona != nil && sel.Persona.Name != p.Fallbak && !sel.Persona.Standalone {
		found := false
		for _, per := range sel.Chain {
			if per.Name == p.Fallbak {
				found = true
				break
			}
		}
		if !found {
			out = append(out, Lint{Level: "warning", Persona: sel.Persona.Name,
				Message: fmt.Sprintf("does not inherit %q; shared doctrine is absent from the compiled stack", p.Fallbak)})
		}
	}

	for _, w := range sel.Warnings {
		out = append(out, Lint{Level: "warning", Persona: sel.Persona.Name, Message: w})
	}
	return out
}
