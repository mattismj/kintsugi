package tools

import (
	"encoding/json"
	"strings"
)

// ClassifierMode selects when the two-stage classifier runs (A1, from the
// qwen-code survey). It asserts refusal-adjacent tool calls against
// deterministic buckets before the operator is even asked, so a model that
// would otherwise refuse to *propose* a call gets a harness answer instead of
// a lecture. The classifier is opt-in on purpose: "off" means the exact
// behaviour today, byte-identical, and no extra model call.
type ClassifierMode string

const (
	ClassifierOff  ClassifierMode = "off"
	ClassifierSoft ClassifierMode = "soft"
	ClassifierHard ClassifierMode = "hard"
)

// ClassifierVerdict is what a model-side classifier returned. It mirrors the
// stage-1 shape qwen-code uses — a single boolean — so the classifier prompt
// can stay a 1-token answer instead of prose.
type ClassifierVerdict struct {
	ShouldBlock bool   `json:"shouldBlock"`
	Reason      string `json:"reason,omitempty"`
}

// classifyHard refuses the request outright: an irreversible/data-exfiltration
// shape. A hard block is never rescued by a later stage.
func classifyHard(tool, spec string) (block bool, reason string) {
	t := strings.ToLower(strings.TrimSpace(tool))
	s := strings.ToLower(strings.TrimSpace(spec))
	switch t {
	case "bash":
		// Data exfiltration to a remote, or irreversible destruction.
		for _, pattern := range []string{
			"curl ", "wget ", "nc ", "ncat ", "socat ", "scp ", "rsync ",
			"shred ", "mkfs", "dd if=", "rm -rf /", "> /dev/",
			"git push", "git reset --hard", "git clean -",
		} {
			if strings.Contains(s, pattern) {
				return true, "irreversible or exfiltrating shell command"
			}
		}
	case "webfetch", "websearch":
		// webfetch/websearch carry their own scope guard; the classifier only
		// hard-blocks a clearly external push (none of these tools push), so it
		// hard-blocks nothing here. The scope guard remains authoritative.
		return false, ""
	case "write", "edit", "apply_patch":
		if strings.Contains(s, ".env") || strings.Contains(s, "credentials") ||
			strings.Contains(s, "id_rsa") || strings.Contains(s, "id_ed25519") {
			return true, "credential file write/read target"
		}
	}
	return false, ""
}

// evalClassifier runs the model classifier. When nil or off it never blocks;
// the caller decides what to do with a block based on ClassifierMode.
func (e *Executor) evalClassifier(tool, spec string) ClassifierVerdict {
	if e.Classifier == nil {
		return ClassifierVerdict{}
	}
	v, err := e.Classifier.Classify(tool, spec)
	if err != nil {
		return ClassifierVerdict{} // a broken classifier never blocks (it is advisory)
	}
	return v
}

// classify decides whether a tool call should be blocked before the permission
// matrix, using the two-stage classifier. It returns the verdict and a reason;
// the permission matrix still runs after a non-block, so the classifier only
// ever tightens, never widens, the decision.
//
// The zero value of ClassifierMode is "" — unset. Unset and "off" are the same
// and mean the classifier is completely disabled: byte-identical to the
// pre-classifier behaviour, and neither the hard buckets nor the model stage
// run. Only an explicit "soft" or "hard" enables anything.
func (e *Executor) classify(tool, spec string, mode ClassifierMode) (block bool, reason string) {
	m := normalizeClassifierMode(mode)
	if m == ClassifierOff {
		return false, ""
	}
	// Deterministic hard buckets: a narrow additional floor for exfiltration
	// and irreversible destruction. They are an extra floor, not a replacement
	// for the policy/scope floors, and they only activate when the operator
	// opted the classifier in.
	if block, reason := classifyHard(tool, spec); block {
		return true, reason
	}
	// The model-backed stage is advisory and only in soft/hard mode.
	if e.Classifier == nil {
		return false, ""
	}
	v := e.evalClassifier(tool, spec)
	if !v.ShouldBlock {
		return false, ""
	}
	return true, firstNonEmpty(v.Reason, "classifier flagged the call")
}

// normalizeClassifierMode maps the zero/unset value to off, so a config that
// never mentions classifier_mode behaves exactly as before the feature existed.
func normalizeClassifierMode(m ClassifierMode) ClassifierMode {
	switch ClassifierMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case ClassifierSoft:
		return ClassifierSoft
	case ClassifierHard:
		return ClassifierHard
	default:
		return ClassifierOff
	}
}

// Classify is the seam the model classifier implements: one tool + spec in, a
// single boolean out. Implementations must be cheap and must never write; a
// classifier that errors is advisory and the caller ignores it.
type Classifier interface {
	Classify(tool, spec string) (ClassifierVerdict, error)
}

// classifierJSON is the on-the-wire result shape the prompt asks for, kept so
// a future model-backed Classifier can parse stage-1 output with a single
// Unmarshal.
type classifierJSON struct {
	ShouldBlock bool   `json:"shouldBlock"`
	Reason      string `json:"reason"`
}

func parseClassifierVerdict(raw []byte) (ClassifierVerdict, bool) {
	var v classifierJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return ClassifierVerdict{}, false
	}
	return ClassifierVerdict{ShouldBlock: v.ShouldBlock, Reason: v.Reason}, true
}
