// Checkpoints and rewind (DESIGN §10, WP-2.2).
//
// A checkpoint is a snapshot of the model-visible message list, stored as one
// more event in the session's append-only log. Rewinding never rewrites the
// events that came before it: it appends a `rewind` event naming the checkpoint
// the session was resumed from, so an operator can still audit what the model
// actually saw on every branch.
package session

import (
	"fmt"
	"time"

	"kintsugi/internal/provider"
)

// Event kinds owned by this file. History ignores both: a checkpoint is not a
// turn, and a rewind marker is not a message.
const (
	KindCheckpoint = "checkpoint"
	KindRewind     = "rewind"
)

// Checkpoint is one saved transcript, replayable with a rewind. Messages is the
// provider-visible conversation exactly as the loop would send it (system
// prompt excluded — that is recompiled by the caller).
type Checkpoint struct {
	ID       string             `json:"id"`
	Label    string             `json:"label"`
	TS       string             `json:"ts"`
	Messages []provider.Message `json:"messages"`
}

// rewindRef is the payload of a rewind event.
type rewindRef struct {
	Checkpoint string `json:"checkpoint"`
}

// SaveCheckpoint appends msgs to the session log as a new checkpoint and
// returns it. The id is a UTC timestamp plus a short random suffix, so two
// checkpoints saved in the same second stay distinguishable and ids sort in
// save order.
//
// Payloads go through the session redactor like every other event, so
// secret-shaped strings are scrubbed before they reach disk.
func SaveCheckpoint(sessionID, label string, msgs []provider.Message) (Checkpoint, error) {
	if sessionID == "" {
		return Checkpoint{}, fmt.Errorf("checkpoint: empty session id")
	}
	s, err := Open(sessionID)
	if err != nil {
		return Checkpoint{}, err
	}
	defer s.Close()

	cp := Checkpoint{
		ID:       time.Now().UTC().Format("20060102-150405") + "-" + shortID(),
		Label:    label,
		TS:       nowTS(),
		Messages: append([]provider.Message(nil), msgs...),
	}
	if s.handle() == nil {
		return Checkpoint{}, fmt.Errorf("checkpoint: session %q is closed", sessionID)
	}
	s.Log(KindCheckpoint, cp)
	return cp, nil
}

// ListCheckpoints returns every checkpoint in the session, oldest first (log
// order). Undecodable events are skipped: a damaged row must not hide the rest.
func ListCheckpoints(sessionID string) ([]Checkpoint, error) {
	entries, err := readEntries(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]Checkpoint, 0, 4)
	for _, e := range entries {
		if e.Kind != KindCheckpoint {
			continue
		}
		var cp Checkpoint
		if !remarshal(e.V, &cp) || cp.ID == "" {
			continue
		}
		out = append(out, cp)
	}
	return out, nil
}

// LoadCheckpoint returns one checkpoint by id.
func LoadCheckpoint(sessionID, id string) (Checkpoint, error) {
	if id == "" {
		return Checkpoint{}, fmt.Errorf("checkpoint: empty id")
	}
	list, err := ListCheckpoints(sessionID)
	if err != nil {
		return Checkpoint{}, err
	}
	for _, cp := range list {
		if cp.ID == id {
			return cp, nil
		}
	}
	return Checkpoint{}, fmt.Errorf("session %q has no checkpoint %q", sessionID, id)
}

// LatestRewind returns the checkpoint the session was last rewound to. The bool
// is false when the session has never been rewound; an error means a rewind
// event points at a checkpoint that is no longer in the log.
func LatestRewind(sessionID string) (Checkpoint, bool, error) {
	entries, err := readEntries(sessionID)
	if err != nil {
		return Checkpoint{}, false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Kind != KindRewind {
			continue
		}
		var ref rewindRef
		if !remarshal(e.V, &ref) || ref.Checkpoint == "" {
			continue
		}
		cp, err := LoadCheckpoint(sessionID, ref.Checkpoint)
		if err != nil {
			return Checkpoint{}, false, err
		}
		return cp, true, nil
	}
	return Checkpoint{}, false, nil
}

// MarkRewind records that the session was resumed from checkpointID. The
// checkpoint is validated first, so the marker can never dangle.
func MarkRewind(sessionID, checkpointID string) error {
	if _, err := LoadCheckpoint(sessionID, checkpointID); err != nil {
		return err
	}
	s, err := Open(sessionID)
	if err != nil {
		return err
	}
	defer s.Close()
	if s.handle() == nil {
		return fmt.Errorf("checkpoint: session %q is closed", sessionID)
	}
	s.Log(KindRewind, rewindRef{Checkpoint: checkpointID})
	return nil
}
