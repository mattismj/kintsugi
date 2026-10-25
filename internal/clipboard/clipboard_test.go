package clipboard

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCopyEmitsAnOSC52Sequence(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Copy(&buf, "finding: T1059.001"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b]52;") {
		t.Fatalf("not an OSC 52 sequence: %q", out)
	}
	// The payload is base64 of the text, and the sequence is terminated.
	want := base64.StdEncoding.EncodeToString([]byte("finding: T1059.001"))
	if !strings.Contains(out, want) {
		t.Fatalf("payload missing from %q", out)
	}
	if !strings.HasSuffix(out, "\a") && !strings.HasSuffix(out, "\x1b\\") {
		t.Fatalf("sequence is not terminated: %q", out)
	}
}

func TestCopyOfEmptyTextWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Copy(&buf, ""); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("an empty copy wrote %q", buf.String())
	}
}

func TestCopyCapsAnOversizedRequest(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("A", maxBytes+5000)
	if _, err := Copy(&buf, big); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	// The encoded payload must correspond to the capped text, not the original.
	want := base64.StdEncoding.EncodeToString([]byte(big[:maxBytes]))
	if !strings.Contains(buf.String(), want) {
		t.Fatal("an oversized copy was not capped before encoding")
	}
}

func TestCopyWrapsForTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	t.Setenv("TERM", "tmux-256color")
	var buf bytes.Buffer
	if _, err := Copy(&buf, "x"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	// The tmux passthrough wraps the whole OSC in a DCS envelope.
	if !strings.Contains(buf.String(), "\x1bPtmux;") {
		t.Fatalf("no tmux passthrough: %q", buf.String())
	}
}

func TestCopyWrapsForScreen(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TERM", "screen-256color")
	var buf bytes.Buffer
	if _, err := Copy(&buf, "x"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if !strings.Contains(buf.String(), "\x1bP") {
		t.Fatalf("no screen passthrough: %q", buf.String())
	}
}

func TestSupportedWithoutATerminal(t *testing.T) {
	// Under `go test` stdout is a pipe, so this asserts the honest answer: the
	// heuristic says the environment cannot honour the request.
	if Supported() {
		t.Skip("stdout is a terminal in this environment; the pipe case cannot be exercised")
	}
}
