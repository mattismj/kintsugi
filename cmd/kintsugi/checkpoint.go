package main

import (
	"fmt"
	"strings"

	"kintsugi/internal/session"
)

// cmdCheckpoint saves or lists session checkpoints (WP-2.2).
//
//	kintsugi checkpoint <session-id> [label]   save the current history
//	kintsugi checkpoint list <session-id>      list saved checkpoints
//
// Checkpoints are session state, not workspace state, so ws is accepted for
// signature symmetry with the other subcommands and otherwise unused.
func cmdCheckpoint(ws string, args []string) {
	_ = ws
	if len(args) == 0 {
		fail("usage: kintsugi checkpoint <session-id> [label] | kintsugi checkpoint list <session-id>")
	}
	if args[0] == "list" {
		if len(args) < 2 {
			fail("usage: kintsugi checkpoint list <session-id>")
		}
		checkpointList(args[1])
		return
	}

	id := args[0]
	label := strings.TrimSpace(strings.Join(args[1:], " "))
	msgs, err := session.History(id)
	if err != nil {
		fail("%v", err)
	}
	cp, err := session.SaveCheckpoint(id, label, msgs)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("checkpoint %s · %d messages · session %s\n", cp.ID, len(cp.Messages), id)
	if cp.Label != "" {
		fmt.Printf("label      %s\n", cp.Label)
	}
	fmt.Printf("rewind with: kintsugi rewind %s %s\n", id, cp.ID)
}

// checkpointList prints one line per checkpoint, oldest first.
func checkpointList(id string) {
	list, err := session.ListCheckpoints(id)
	if err != nil {
		fail("%v", err)
	}
	if len(list) == 0 {
		fmt.Printf("no checkpoints in session %s (save one with `kintsugi checkpoint %s <label>`)\n", id, id)
		return
	}
	fmt.Printf("%-26s %-30s %8s  %s\n", "ID", "SAVED", "MESSAGES", "LABEL")
	for _, cp := range list {
		label := cp.Label
		if label == "" {
			label = "—"
		}
		fmt.Printf("%-26s %-30s %8d  %s\n", cp.ID, cp.TS, len(cp.Messages), label)
	}
}

// cmdRewind records a rewind marker on a session (WP-2.2). It validates the
// checkpoint before writing anything: a resumed session must never point at a
// checkpoint that does not exist.
func cmdRewind(ws string, args []string) {
	_ = ws
	if len(args) < 2 {
		fail("usage: kintsugi rewind <session-id> <checkpoint-id>")
	}
	sid, cid := args[0], args[1]
	cp, err := session.LoadCheckpoint(sid, cid)
	if err != nil {
		fail("%v", err)
	}
	if err := session.MarkRewind(sid, cp.ID); err != nil {
		fail("%v", err)
	}
	fmt.Printf("rewound %s to %s · %d messages restored\n", sid, cp.ID, len(cp.Messages))
	if cp.Label != "" {
		fmt.Printf("label   %s\n", cp.Label)
	}
	fmt.Printf("resume with: kintsugi run --resume %s\n", sid)
}
