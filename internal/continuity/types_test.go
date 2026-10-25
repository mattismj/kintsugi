package continuity

import (
	"reflect"
	"strings"
	"testing"
)

// TestFrameTypesCarryOnlyPositiveFields pins the invariant "a refusal is never
// memory" at the type level: the frame API has no field that could carry
// refusal text, a rejection outcome, a transcript, or a model message. If
// someone adds one, this test fails before the data can be rendered.
func TestFrameTypesCarryOnlyPositiveFields(t *testing.T) {
	check := func(name string, v any, want ...string) {
		typ := reflect.TypeOf(v)
		got := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			got = append(got, f.Name)

			// Every field must be a positive scalar or a list of them — no
			// nested message/transcript carriers.
			switch f.Type.Kind() {
			case reflect.String, reflect.Int:
			case reflect.Slice:
				elem := f.Type.Elem()
				if elem.Kind() != reflect.String && elem != reflect.TypeOf(PriorWork{}) {
					t.Errorf("%s.%s is %s; only string lists or prior-work lists are allowed", name, f.Name, f.Type)
				}
			default:
				t.Errorf("%s.%s has kind %s; only string/int fields are allowed", name, f.Name, f.Type.Kind())
			}

			// No negative vocabulary in any field name.
			low := strings.ToLower(f.Name)
			for _, bad := range []string{"refus", "declin", "reject", "denied", "outcome", "transcript", "message", "body", "text", "error", "raw", "content"} {
				if strings.Contains(low, bad) {
					t.Errorf("%s.%s looks like a negative/raw carrier (matched %q)", name, f.Name, bad)
				}
			}
			// JSON tags keep the same shape (snake_case of the field name).
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag != "" && strings.ReplaceAll(tag, "_", "") != low {
				t.Errorf("%s.%s json tag %q does not mirror the field name", name, f.Name, tag)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fields = %v, want %v", name, got, want)
		}
	}

	check("PriorWork", PriorWork{}, "ID", "Date", "Kind", "Summary", "Evidence")
	check("Frame", Frame{}, "Attestation", "PriorWork", "BudgetTokens")
}

// TestRenderAPIHasNoNegativeInputs documents, in prose the compiler cannot
// check, that the rendering API takes only the positive surface: an attestation
// block string and accepted work. Nothing in the signature accepts a model
// message, a turn record, or a refusal.
func TestRenderAPIHasNoNegativeInputs(t *testing.T) {
	// These are compile-time assertions about the shapes the API accepts: a
	// refusal-shaped value has no type to be passed as.
	var _ func(string, []PriorWork, int) string = RenderFrame
	var _ func([]PriorWork, int) string = RenderPriorWork
	var _ func(string) string = Block

	// The only strings RenderFrame renders are the ones handed in; the render
	// itself never adds prose about refusals except the sentence that says its
	// absence proves nothing.
	out := RenderFrame("", []PriorWork{{ID: "L0001", Summary: "fuzz the parser"}}, 600)
	for _, bad := range []string{"I can't", "I cannot", "I'm unable", "I must decline", "As an AI"} {
		if strings.Contains(out, bad) {
			t.Fatalf("render produced refusal-shaped text %q", bad)
		}
	}
	if !strings.Contains(out, "its absence is not evidence of refusal") {
		t.Fatal("the frame must state that absence is not evidence of refusal")
	}
}
