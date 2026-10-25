package notify

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOSC9(t *testing.T) {
	// Capture stderr
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	n := New(Config{Backend: BackendOSC9})
	err := n.Notify(KindDone, "task completed")
	if err != nil {
		t.Fatalf("Notify error: %v", err)
	}

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Should contain OSC 9 sequence
	if !strings.Contains(output, "\x1b]9;4;") {
		t.Errorf("Expected OSC 9 sequence, got %q", output)
	}
	if !strings.Contains(output, "task completed") {
		t.Errorf("Expected message in output, got %q", output)
	}
	if !strings.HasSuffix(output, "\x1b\\") {
		t.Errorf("Expected ST terminator, got %q", output)
	}
}

func TestOSC777(t *testing.T) {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	n := New(Config{Backend: BackendOSC777})
	err := n.Notify(KindApproval, "approval needed")
	if err != nil {
		t.Fatalf("Notify error: %v", err)
	}

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Should contain OSC 777 sequence with kind as title
	if !strings.Contains(output, "\x1b]777;notify;") {
		t.Errorf("Expected OSC 777 sequence, got %q", output)
	}
	if !strings.Contains(output, "approval") {
		t.Errorf("Expected kind in output, got %q", output)
	}
	if !strings.Contains(output, "approval needed") {
		t.Errorf("Expected message in output, got %q", output)
	}
}

func TestBell(t *testing.T) {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	n := New(Config{Backend: BackendBell})
	err := n.Notify(KindDone, "task completed")
	if err != nil {
		t.Fatalf("Notify error: %v", err)
	}

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Should be just a bell character
	if output != "\x07" {
		t.Errorf("Expected bell character, got %q", output)
	}
}

func TestCommand(t *testing.T) {
	// Create a test script
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "notify.sh")
	outputPath := filepath.Join(tmpDir, "output.txt")

	script := `#!/bin/sh
echo "$1" > ` + outputPath
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	n := New(Config{
		Backend: BackendCommand,
		Command: scriptPath,
	})

	err := n.Notify(KindDone, "test message")
	if err != nil {
		t.Fatalf("Notify error: %v", err)
	}

	// Read the output file
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	if string(output) != "test message\n" {
		t.Errorf("Expected 'test message\\n', got %q", string(output))
	}
}

func TestCommandNoShell(t *testing.T) {
	// Verify that command backend does not use shell
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "notify.sh")
	outputPath := filepath.Join(tmpDir, "output.txt")

	// Script that would fail if interpreted by shell
	script := `#!/bin/sh
echo "$1" > ` + outputPath
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	n := New(Config{
		Backend: BackendCommand,
		Command: scriptPath,
	})

	// Message with shell metacharacters - should be passed as-is
	err := n.Notify(KindDone, "test; rm -rf /")
	if err != nil {
		t.Fatalf("Notify error: %v", err)
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	// Should contain the literal message, not execute rm
	if string(output) != "test; rm -rf /\n" {
		t.Errorf("Expected literal message, got %q", string(output))
	}
}

func TestCommandMissing(t *testing.T) {
	n := New(Config{
		Backend: BackendCommand,
		Command: "",
	})

	err := n.Notify(KindDone, "test")
	if err == nil {
		t.Error("Expected error for missing command")
	}
	if !strings.Contains(err.Error(), "command to be configured") {
		t.Errorf("Expected error about missing command, got %v", err)
	}
}

func TestOff(t *testing.T) {
	n := New(Config{Backend: BackendOff})

	// Should not error and should not output anything
	err := n.Notify(KindDone, "test")
	if err != nil {
		t.Errorf("Notify with off backend should not error, got %v", err)
	}
}

func TestEmptyBackend(t *testing.T) {
	n := New(Config{Backend: ""})

	// Empty backend should behave like off
	err := n.Notify(KindDone, "test")
	if err != nil {
		t.Errorf("Notify with empty backend should not error, got %v", err)
	}
}

func TestEscapeOSC(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with\x07bell", "withbell"},
		{"with\x1bescape", "withescape"},
		{"with\\backslash", "withbackslash"},
		{"all\x07\x1b\\together", "alltogether"},
	}

	for _, tt := range tests {
		result := escapeOSC(tt.input)
		if result != tt.expected {
			t.Errorf("escapeOSC(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestKinds(t *testing.T) {
	// Verify all kinds work
	kinds := []Kind{KindApproval, KindDone, KindQuota}
	for _, kind := range kinds {
		if kind == "" {
			t.Error("Kind should not be empty")
		}
	}
}

func TestBackends(t *testing.T) {
	// Verify all backends are defined
	backends := []Backend{BackendOSC9, BackendOSC777, BackendBell, BackendCommand, BackendOff}
	for _, backend := range backends {
		if backend == "" {
			t.Error("Backend should not be empty")
		}
	}
}
