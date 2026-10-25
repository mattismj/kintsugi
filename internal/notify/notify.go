// Package notify emits terminal notifications for approval-needed, turn-done,
// and quota-warn events. It supports OSC 9, OSC 777, bell, and command backends.
package notify

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Kind is the notification kind.
type Kind string

const (
	KindApproval Kind = "approval"
	KindDone     Kind = "done"
	KindQuota    Kind = "quota"
)

// Backend is the notification backend.
type Backend string

const (
	BackendOSC9    Backend = "osc9"
	BackendOSC777  Backend = "osc777"
	BackendBell    Backend = "bell"
	BackendCommand Backend = "command"
	BackendOff     Backend = "off"
)

// Config is the notification configuration.
type Config struct {
	Backend Backend
	Command string // for command backend: the executable to run
}

// Notifier emits notifications.
type Notifier struct {
	cfg Config
}

// New creates a notifier with the given configuration.
func New(cfg Config) *Notifier {
	return &Notifier{cfg: cfg}
}

// Notify emits a notification of the given kind with the given message.
func (n *Notifier) Notify(kind Kind, message string) error {
	if n.cfg.Backend == BackendOff || n.cfg.Backend == "" {
		return nil
	}

	switch n.cfg.Backend {
	case BackendOSC9:
		return n.osc9(message)
	case BackendOSC777:
		return n.osc777(kind, message)
	case BackendBell:
		return n.bell()
	case BackendCommand:
		return n.command(message)
	default:
		return fmt.Errorf("unknown backend %q", n.cfg.Backend)
	}
}

// osc9 emits an OSC 9 notification (iTerm2).
func (n *Notifier) osc9(message string) error {
	// OSC 9;4;message ST
	_, err := fmt.Fprintf(os.Stderr, "\x1b]9;4;%s\x1b\\", escapeOSC(message))
	return err
}

// osc777 emits an OSC 777 notification (terminal.app, some Linux terminals).
func (n *Notifier) osc777(kind Kind, message string) error {
	// OSC 777;notify;title;body ST
	title := string(kind)
	_, err := fmt.Fprintf(os.Stderr, "\x1b]777;notify;%s;%s\x1b\\", escapeOSC(title), escapeOSC(message))
	return err
}

// bell emits a bell character.
func (n *Notifier) bell() error {
	_, err := fmt.Fprint(os.Stderr, "\x07")
	return err
}

// command runs the configured command with the message as argv.
func (n *Notifier) command(message string) error {
	if n.cfg.Command == "" {
		return fmt.Errorf("command backend requires a command to be configured")
	}

	// No shell: run the executable directly with the message as argv.
	cmd := exec.Command(n.cfg.Command, message)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// escapeOSC escapes special characters in OSC strings.
func escapeOSC(s string) string {
	// OSC strings are terminated by ST (ESC \) or BEL.
	// Remove any embedded ST or BEL to prevent premature termination.
	s = strings.ReplaceAll(s, "\x1b", "")
	s = strings.ReplaceAll(s, "\x07", "")
	s = strings.ReplaceAll(s, "\\", "")
	return s
}
