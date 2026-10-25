package context

// Attestation is the operator's recorded review of the static layer-5 frame
// (M8.5.2, DESIGN §16.23). It lives beside CONTEXT.md as
// `.kintsugi/attestation.json` and is written only by an explicit operator
// action — the harness never attests on its own, and nothing here grants,
// widens, or implies a permission.
//
// The attestation binds the operator's review to the *authorization and scope*
// sections it covered, via a content hash. When the operator edits those
// sections the hash stops matching; callers can then warn, which is the whole
// point of recording one.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// AttestationRel is the workspace-relative attestation file.
	AttestationRel = ".kintsugi/attestation.json"
	// AttestationHashPrefix marks an authorization hash value.
	AttestationHashPrefix = "sha256:"
	// AttestationNotPermission is the load-bearing line: an attestation is
	// context, never a grant. It is rendered on every attestation block.
	AttestationNotPermission = "This is context, not a permission grant."
)

// Attestation is the operator's review record for the current frame.
type Attestation struct {
	AttestedAt        string `json:"attested_at"`          // RFC3339 or YYYY-MM-DD, operator-supplied
	Reviewer          string `json:"reviewer"`             // who reviewed (free text, local)
	AuthorizationHash string `json:"authorization_hash"`   // sha256:<hex> of the authorization+scope render
	ExpiresAt         string `json:"expires_at,omitempty"` // optional review-by date
}

// AttestationPath is the absolute attestation path for a workspace.
func AttestationPath(workspace string) string {
	return filepath.Join(workspace, filepath.FromSlash(AttestationRel))
}

// LoadAttestation reads the workspace attestation. A missing file is not an
// error: it returns (nil, nil), meaning "never attested" — which is a normal
// state, not a failure.
func LoadAttestation(workspace string) (*Attestation, error) {
	path := AttestationPath(workspace)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("attestation: %w", err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, fmt.Errorf("attestation %s: empty file", path)
	}
	var a Attestation
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("attestation %s: %w", path, err)
	}
	return &a, nil
}

// SaveAttestation writes the attestation atomically, creating .kintsugi if
// needed. This is an operator action only; nothing in the
// compile path calls it.
func SaveAttestation(workspace string, a Attestation) error {
	path := AttestationPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("attestation: %w", err)
	}
	return nil
}

// AuthorizationHash hashes the rendered authorization and scope sections —
// the two operator-gated classes — and nothing else. Editing a stack line,
// re-curating the state section, or refreshing lessons leaves the hash alone;
// editing either gated section changes it. The canonical form is a single
// "sha256:" value so it can be printed, stored and compared verbatim.
func AuthorizationHash(d *Document) string {
	return AttestationHashPrefix + hex.EncodeToString(authorizationDigest(d))
}

// authorizationDigest is the exact bytes that are hashed: the gated sections in
// a fixed class order, titles included, bodies trimmed. Stable across
// re-parses of the same file (DESIGN §3.6: no timestamps, no counters).
func authorizationDigest(d *Document) []byte {
	var b strings.Builder
	if d != nil {
		for _, class := range []string{ClassAuthorization, ClassScope} {
			if s, ok := d.Find(class); ok {
				fmt.Fprintf(&b, "## %s\n\n%s\n", s.Title, strings.TrimSpace(s.Body))
			}
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return sum[:]
}

// HashMatches reports whether the recorded hash still describes the current
// document. A truncated hex prefix (>= 8 chars) of the current hash is accepted
// so a hand-written attestation stays usable; a hash from a different
// authorization text is not.
func (a *Attestation) HashMatches(d *Document) bool {
	if a == nil {
		return false
	}
	want := normalizeAttestationHash(a.AuthorizationHash)
	if want == "" {
		return false
	}
	have := normalizeAttestationHash(AuthorizationHash(d))
	if want == have {
		return true
	}
	return len(want) >= 8 && len(want) < len(have) && strings.HasPrefix(have, want)
}

// Stale reports whether the attestation should be re-reviewed: no attestation
// date at all, an expiry in the past (or an expiry that cannot be parsed, which
// cannot be trusted to be current), or now past a date-only review-by.
func (a *Attestation) Stale(now time.Time) bool {
	if a == nil {
		return true
	}
	if strings.TrimSpace(a.AttestedAt) == "" {
		return true
	}
	exp := strings.TrimSpace(a.ExpiresAt)
	if exp == "" {
		return false // no expiry: valid until the operator edits the frame
	}
	t, ok := parseAttestationTime(exp)
	if !ok {
		return true
	}
	return !now.Before(t)
}

// RenderAttestation renders the deterministic attestation block that belongs in
// the static layer-5 frame. It carries no timestamps of its own: the output is
// a pure function of the recorded fields, so the compiled prefix stays
// byte-identical turn to turn.
func RenderAttestation(a *Attestation) string {
	if a == nil {
		return ""
	}
	attested := attestationOneLine(a.AttestedAt)
	if attested == "" {
		attested = "unrecorded"
	}
	reviewer := attestationOneLine(a.Reviewer)
	if reviewer == "" {
		reviewer = "operator (unrecorded)"
	}
	hash := attestationOneLine(a.AuthorizationHash)
	if hash == "" {
		hash = "unrecorded"
	} else if !strings.HasPrefix(hash, AttestationHashPrefix) {
		hash = AttestationHashPrefix + hash
	}
	reviewBy := attestationOneLine(a.ExpiresAt)
	if reviewBy == "" {
		reviewBy = "not set"
	}

	var b strings.Builder
	b.WriteString("## Operator attestation\n\n")
	fmt.Fprintf(&b, "- attested: %s by %s\n", attested, reviewer)
	fmt.Fprintf(&b, "- authorization hash: %s\n", hash)
	fmt.Fprintf(&b, "- review by: %s\n", reviewBy)
	fmt.Fprintf(&b, "- %s\n", AttestationNotPermission)
	return b.String()
}

// singleLine collapses an operator-supplied value to one line so a stray
// newline cannot inject structure into the rendered block.
func attestationOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// attestationTimeLayouts are tried in order; the last one is a bare date and is
// treated as end-of-day so "review by 2026-03-01" stays valid through it.
var attestationTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseAttestationTime(s string) (time.Time, bool) {
	for i, layout := range attestationTimeLayouts {
		t, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		if i == len(attestationTimeLayouts)-1 {
			// date only: valid through the end of that day
			return t.AddDate(0, 0, 1).Add(-time.Second), true
		}
		return t, true
	}
	return time.Time{}, false
}

// normalizeAttestationHash lowercases hex and drops the optional prefix so a
// stored hash and a computed one compare cleanly.
func normalizeAttestationHash(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	return strings.TrimPrefix(h, AttestationHashPrefix)
}
