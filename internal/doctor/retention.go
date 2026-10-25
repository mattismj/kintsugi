package doctor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session store file suffixes. A session id lives in <Dir()>/<id>.sqlite, with
// a legacy <Dir()>/<id>.jsonl kept as an import source (§10).
const (
	retentionSQLiteSuffix = ".sqlite"
	retentionJSONLSuffix  = ".jsonl"
)

// RetentionCandidate is one on-disk session file eligible for the sweep.
type RetentionCandidate struct {
	ID       string
	Path     string
	Modified time.Time
	Bytes    int64
}

// RetentionReport is the outcome of a dry-run sweep. Nothing has been deleted
// and nothing on disk has changed: the caller applies Delete.
type RetentionReport struct {
	Delete         []RetentionCandidate
	Keep           []RetentionCandidate
	BytesReclaimed int64
}

// Sweep computes which session files in dir the retention policy would drop.
//
// Policies compose: a file is a delete candidate when it is older than
// maxAgeDays (when > 0) or when it falls outside the newest maxSessions by
// mtime (when > 0). Both policies at 0 means "keep forever" and Delete stays
// empty. Only *.jsonl and *.sqlite files are considered; SQLite -wal/-shm
// sidecars and in-flight *.migrating scratch files are ignored. Candidates are
// ordered newest-first, and the sweep never removes anything itself.
//
// A legacy session that exists as both <id>.jsonl and <id>.sqlite appears as
// two candidates: they are distinct files and the caller deletes files.
func Sweep(dir string, maxSessions, maxAgeDays int, now time.Time) (RetentionReport, error) {
	rep := RetentionReport{}

	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No session store yet: an empty sweep, not a failure.
			return rep, nil
		}
		return rep, err
	}

	cands := make([]RetentionCandidate, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, retentionSQLiteSuffix):
		case strings.HasSuffix(name, retentionJSONLSuffix):
		default:
			// Sidecars (x.sqlite-wal, x.sqlite-shm), scratch files
			// (x.sqlite.migrating) and unrelated files are not sessions.
			continue
		}
		id := sessionIDFromName(name)
		if id == "" {
			continue
		}
		// Ignore sqlite sidecars that happen to carry a session-ish suffix.
		if strings.HasSuffix(id, "-wal") || strings.HasSuffix(id, "-shm") || strings.HasSuffix(id, ".migrating") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, RetentionCandidate{
			ID:       id,
			Path:     filepath.Join(dir, name),
			Modified: info.ModTime(),
			Bytes:    info.Size(),
		})
	}

	// Newest-first; ties break on id so the report is deterministic.
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].Modified.Equal(cands[j].Modified) {
			return cands[i].Modified.After(cands[j].Modified)
		}
		if cands[i].ID != cands[j].ID {
			return cands[i].ID < cands[j].ID
		}
		return cands[i].Path < cands[j].Path
	})

	drop := make([]bool, len(cands))
	if maxAgeDays > 0 {
		cutoff := now.Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
		for i, c := range cands {
			if c.Modified.Before(cutoff) {
				drop[i] = true
			}
		}
	}
	if maxSessions > 0 {
		// cands is newest-first, so everything past the head is the oldest
		// beyond the newest maxSessions.
		for i := maxSessions; i < len(cands); i++ {
			drop[i] = true
		}
	}

	for i, c := range cands {
		if drop[i] {
			rep.Delete = append(rep.Delete, c)
			rep.BytesReclaimed += c.Bytes
			continue
		}
		rep.Keep = append(rep.Keep, c)
	}
	return rep, nil
}

// sessionIDFromName strips the store suffix off a session file name.
func sessionIDFromName(name string) string {
	switch {
	case strings.HasSuffix(name, retentionSQLiteSuffix):
		return strings.TrimSuffix(name, retentionSQLiteSuffix)
	case strings.HasSuffix(name, retentionJSONLSuffix):
		return strings.TrimSuffix(name, retentionJSONLSuffix)
	default:
		return ""
	}
}
