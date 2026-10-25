// Package redact scrubs secret-shaped strings before they are persisted to
// session logs or returned to a model from tool output (DESIGN §1.2, M3).
package redact

import (
	"regexp"
	"strings"
)

const Placeholder = "[REDACTED]"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`),                                                  // OpenAI-style
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),                                              // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),                                            // fine-grained PAT
	regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._\-]{12,}`),                                     // bearer headers
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                                      // AWS access key id
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),                                                      // AWS temp key id
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),                                                // Google API key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{5,}`),         // JWT
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), // PEM block
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*`),                                  // dangling PEM header + body
	regexp.MustCompile(`(?m)^\s*[A-Z][A-Z0-9_]{2,}\s*=\s*\S+$`),                                     // .env-style values
}

// Redact replaces secret-shaped substrings. It is deliberately conservative:
// false positives cost a log line, false negatives leak credentials.
func Redact(s string) string {
	for _, re := range patterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			low := strings.ToLower(m)
			switch {
			case strings.HasPrefix(low, "bearer "):
				return m[:7] + Placeholder
			case strings.HasPrefix(m, "-----"):
				return "-----BEGIN REDACTED PRIVATE KEY-----"
			case strings.Contains(m, "="):
				if i := strings.Index(m, "="); i > 0 {
					return m[:i+1] + Placeholder
				}
			}
			return Placeholder
		})
	}
	return s
}
