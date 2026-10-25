package context

import (
	"strings"
	"testing"
)

// TestAuthorizationTemplatesParse pins the contract that matters: every
// template renders into a CONTEXT.md that Parse accepts with an
// authorization section present and non-empty. A template whose example
// fails to parse would leave a newcomer worse off than the blank TODO.
func TestAuthorizationTemplatesParse(t *testing.T) {
	for _, k := range AuthorizationKinds {
		t.Run(string(k.Kind), func(t *testing.T) {
			body := DefaultTemplate("lab", string(k.Kind))
			doc, warns := Parse([]byte(body))
			if doc == nil {
				t.Fatalf("template does not parse: %v", warns)
			}
			auth := ""
			for _, s := range doc.Sections {
				if s.Class == ClassAuthorization {
					auth = s.Body
				}
			}
			if strings.TrimSpace(auth) == "" {
				t.Fatalf("the authorization section is empty for %s", k.Kind)
			}
			// The worked example says what to do with it: replace it with
			// your own words. A template that reads as final would be the
			// harness authorizing the operator.
			if !strings.Contains(auth, "Replace the example") {
				t.Fatalf("the template does not mark itself as an example to replace:\n%s", auth)
			}
		})
	}
}

// TestAuthorizationTemplateUnknownFallsBackToGeneral: an unknown kind is the
// general template, never a blank section.
func TestAuthorizationTemplateUnknownFallsBackToGeneral(t *testing.T) {
	if AuthorizationTemplate("nope") != AuthorizationTemplate("general") {
		t.Fatal("an unknown kind did not fall back to general")
	}
}
