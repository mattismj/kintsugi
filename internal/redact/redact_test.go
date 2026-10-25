package redact

import (
	"strings"
	"testing"
)

func TestRedactSecretShapes(t *testing.T) {
	cases := []string{
		`export OPENAI_KEY=sk-abcdefghijklmnopqrstuvwx`,
		`token ghp_012345678901234567890123456789012345`,
		`github_pat_11ABCDEFG0123456789_abcdefghijklmnop`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefg`,
		`aws AKIAIOSFODNN7EXAMPLE here`,
		`google AIzaSyA1234567890abcdefghijklmnopqrstuv`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		"API_KEY=supersecretvalue",
	}
	for _, in := range cases {
		out := Redact(in)
		if out == in {
			t.Errorf("not redacted: %q", in)
		}
		for _, leak := range []string{"sk-abcdef", "ghp_0123", "AKIAIOSFODNN7EXAMPLE", "AIzaSyA", "eyJhbGci", "supersecretvalue", "MIIE"} {
			if strings.Contains(out, leak) {
				t.Errorf("leak %q remains in %q", leak, out)
			}
		}
	}
}

func TestRedactLeavesNormalText(t *testing.T) {
	in := "go test ./... passes; see README.md and src/main.go"
	if out := Redact(in); out != in {
		t.Fatalf("normal text changed: %q", out)
	}
}
