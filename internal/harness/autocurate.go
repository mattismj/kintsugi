package harness

import (
	"fmt"
	"os"
	"strings"

	"kintsugi/internal/session"
)

// MaybeBackgroundCurate runs the taste/skill review in a detached goroutine when
// `[taste] background = true` and a curator model is configured (A4, from
// Hermes' background review fork). It operates on a snapshot of the session
// transcript — never the live transcript or the cached prompt — and writes only
// through the existing curator gate, so it can never widen a permission. It
// returns immediately and swallows failure: a background reviewer must never
// fail a run.
func MaybeBackgroundCurate(a *App, sessionID string) {
	if a == nil || !a.Cfg.Taste.Background {
		return
	}
	if strings.TrimSpace(a.Cfg.Context.CuratorModel) == "" {
		return
	}
	go func() {
		transcript, err := session.TranscriptText(sessionID, curatorTranscriptLines)
		if err != nil {
			return
		}
		if _, err := a.Curate("", sessionID, transcript); err != nil {
			fmt.Fprintf(os.Stderr, "background review: %v\n", err)
		}
	}()
}

// curatorTranscriptLines bounds how much of the session the end-of-session
// curator reads. The curator is a summariser, not an auditor.
const curatorTranscriptLines = 400

// MaybeCurate runs one curator pass when a session ends, if the operator asked
// for it and a curator model is configured. It returns nil — silently — when
// curation is disabled, unconfigured, or fails: a curator must never fail a
// run or change its exit status.
func MaybeCurate(a *App, sessionID string) *CurateResult {
	if a == nil || !a.Cfg.Context.CuratorOnSessionEnd {
		return nil
	}
	if strings.TrimSpace(a.Cfg.Context.CuratorModel) == "" {
		return nil
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}

	transcript, err := session.TranscriptText(sessionID, curatorTranscriptLines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "curator: transcript: %v\n", err)
		return nil
	}
	res, err := a.Curate("", sessionID, transcript)
	if err != nil {
		fmt.Fprintf(os.Stderr, "curator: %v\n", err)
		return nil
	}

	applied := make([]string, 0, len(res.Apply.Applied))
	for _, p := range res.Apply.Applied {
		name := p.Section
		if p.Class != "" {
			name = p.Class
		}
		applied = append(applied, name)
	}
	list := "none"
	if len(applied) > 0 {
		list = strings.Join(applied, ", ")
	}
	fmt.Fprintf(os.Stderr, "curator: %d proposals -> %s (applied: %s)\n", res.Proposals, res.ProposalPath, list)
	return res
}
