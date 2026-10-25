package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Job is one scheduled task (I-56): a cron expression and the prose to run.
//
// Task is data. It is handed to the runner as a user turn and to nothing else —
// no system prompt is built from it, no permission rule is derived from it, and
// no scope is widened by it. See the package comment and guard.go.
type Job struct {
	// ID is a stable, human-typable identifier ("job-1", "job-2").
	ID string `json:"id"`
	// Cron is the expression as the operator wrote it, so a listing reads back
	// exactly what was typed.
	Cron string `json:"cron"`
	// Task is the prose the job runs. Data, never authority.
	Task string `json:"task"`
	// Created is when the job was added.
	Created time.Time `json:"created"`
	// NextAt is the next firing. It is computed when the job is written and
	// recomputed after every firing, so a job created at 10:00 for "0 3 * * *"
	// waits until tomorrow's 03:00 instead of firing the moment it is created.
	NextAt time.Time `json:"next_at"`
	// Paused suspends firing without deleting the job. A paused job keeps its
	// NextAt, so resuming does not silently skip a window: it fires (once) on
	// the next pass if that time has already gone by.
	Paused bool `json:"paused,omitempty"`
	// LastFired is the most recent firing (zero when it never fired).
	LastFired time.Time `json:"last_fired,omitzero"`
	// Fires counts recorded firings.
	Fires int `json:"fires,omitempty"`
	// LastError is the last firing's failure, for the listing. It carries the
	// runner's error text, which is harness prose, not payload prose.
	LastError string `json:"last_error,omitempty"`
}

// Schedule returns the job's parsed expression.
func (j Job) Schedule() (*Schedule, error) { return ParseCron(j.Cron) }

// Store is a workspace's job file. It is a small JSON document, written
// atomically: a truncated write must never lose every schedule at once.
type Store struct {
	path string
	jobs []Job
	now  func() time.Time
}

// storeDir is the workspace directory the harness owns.
const storeDir = ".kintsugi"

// storeName is the jobs file's name inside it.
const storeName = "schedules.json"

// StorePath is the job file's path for a workspace.
func StorePath(workspace string) string {
	return filepath.Join(workspace, storeDir, storeName)
}

// OpenStore loads (or creates in memory) a workspace's jobs. A missing file is
// an empty store, not an error: a workspace with no schedules is the normal
// state.
func OpenStore(workspace string) (*Store, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("schedule: no workspace")
	}
	s := &Store{path: StorePath(workspace), now: time.Now}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("schedule: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	// A malformed file is an error, never a file to be silently truncated: the
	// operator's schedules are the only copy.
	var doc struct {
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("schedule: %s is malformed: %w", s.path, err)
	}
	s.jobs = doc.Jobs
	return s, nil
}

// SetClock overrides the clock a store reads when it computes the next firing.
// It is the seam the tests use; the CLI leaves it alone.
func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Path is the file the store reads and writes.
func (s *Store) Path() string { return s.path }

// Jobs returns a copy of the stored jobs, in file order.
func (s *Store) Jobs() []Job {
	out := make([]Job, len(s.jobs))
	copy(out, s.jobs)
	return out
}

// Get returns one job by id.
func (s *Store) Get(id string) (Job, bool) {
	id = strings.TrimSpace(id)
	for _, j := range s.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// Add validates and stores a new job. The cron expression must parse and the
// payload must pass the injection guard — both checks happen here, at write
// time, so a job that could never be fired safely is never written.
func (s *Store) Add(cron, task string) (Job, error) {
	sch, err := ParseCron(cron)
	if err != nil {
		return Job{}, err
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return Job{}, fmt.Errorf("schedule: the task text is empty")
	}
	if ref, bad := GuardPayload(task); bad {
		return Job{}, ref
	}
	now := s.clock()()
	next, ok := sch.Next(now)
	if !ok {
		return Job{}, fmt.Errorf("schedule: %q never fires", strings.TrimSpace(cron))
	}
	job := Job{ID: s.nextID(), Cron: strings.TrimSpace(cron), Task: task, Created: now, NextAt: next}
	s.jobs = append(s.jobs, job)
	if err := s.Save(); err != nil {
		return Job{}, err
	}
	return job, nil
}

// nextID returns the first unused "job-N".
func (s *Store) nextID() string {
	used := map[string]bool{}
	for _, j := range s.jobs {
		used[j.ID] = true
	}
	for i := 1; ; i++ {
		id := fmt.Sprintf("job-%d", i)
		if !used[id] {
			return id
		}
	}
}

// Remove deletes a job.
func (s *Store) Remove(id string) error {
	id = strings.TrimSpace(id)
	for i, j := range s.jobs {
		if j.ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			return s.Save()
		}
	}
	return fmt.Errorf("schedule: unknown job %q (run `kintsugi schedule list`)", id)
}

// SetPaused pauses or resumes a job.
func (s *Store) SetPaused(id string, paused bool) (Job, error) {
	id = strings.TrimSpace(id)
	for i := range s.jobs {
		if s.jobs[i].ID != id {
			continue
		}
		s.jobs[i].Paused = paused
		if err := s.Save(); err != nil {
			return Job{}, err
		}
		return s.jobs[i], nil
	}
	return Job{}, fmt.Errorf("schedule: unknown job %q (run `kintsugi schedule list`)", id)
}

// RecordFiring notes one firing's outcome and schedules the following one.
// at is the instant the firing was for; the next firing is computed strictly
// after it, so a job cannot fire twice for the same window.
func (s *Store) RecordFiring(id string, at time.Time, err error) error {
	for i := range s.jobs {
		if s.jobs[i].ID != id {
			continue
		}
		s.jobs[i].LastFired = at
		s.jobs[i].Fires++
		s.jobs[i].LastError = ""
		if err != nil {
			s.jobs[i].LastError = err.Error()
		}
		if sch, serr := s.jobs[i].Schedule(); serr == nil {
			if next, ok := sch.Next(at); ok {
				s.jobs[i].NextAt = next
			}
		}
		return s.Save()
	}
	return fmt.Errorf("schedule: unknown job %q", id)
}

// Due returns the jobs whose next firing is at or before now, earliest first.
// A paused job is never due. A job whose expression no longer parses is
// skipped rather than fatal: one bad edit must not stop every other job.
func (s *Store) Due(now time.Time) []Job {
	var out []Job
	for _, j := range s.jobs {
		if j.Paused || j.NextAt.IsZero() {
			continue
		}
		if j.NextAt.After(now) {
			continue
		}
		out = append(out, j)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].NextAt.Before(out[j].NextAt) })
	return out
}

// SetNextAt moves one job's next firing. It exists for the operator's explicit
// "run this now" path: a trigger sets the window to the immediate past so the
// job is due, and the firing then schedules the next one from its cron as
// usual. Nothing else in the harness moves a window by hand.
func (s *Store) SetNextAt(id string, at time.Time) error {
	id = strings.TrimSpace(id)
	for i := range s.jobs {
		if s.jobs[i].ID != id {
			continue
		}
		s.jobs[i].NextAt = at
		return s.Save()
	}
	return fmt.Errorf("schedule: unknown job %q (run `kintsugi schedule list`)", id)
}

// clock returns the store's clock, defaulting to the wall clock.
func (s *Store) clock() func() time.Time {
	if s.now != nil {
		return s.now
	}
	return time.Now
}

// Save writes the job file atomically, 0600 in a 0755 directory. The file holds
// the operator's scheduled prose and is a control surface for automation, so it
// is not world-readable.
func (s *Store) Save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	doc := struct {
		Jobs []Job `json:"jobs"`
	}{Jobs: s.jobs}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("schedule: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("schedule: %w", err)
	}
	return nil
}

// SortedIDs returns the job ids in display order (job-2 before job-10).
func (s *Store) SortedIDs() []string {
	ids := make([]string, 0, len(s.jobs))
	for _, j := range s.jobs {
		ids = append(ids, j.ID)
	}
	sort.SliceStable(ids, func(i, j int) bool { return idLess(ids[i], ids[j]) })
	return ids
}

// idLess orders "job-N" ids numerically, falling back to lexical order.
func idLess(a, b string) bool {
	ia, na := numericSuffix(a)
	ib, nb := numericSuffix(b)
	if na && nb && ia != ib {
		return ia < ib
	}
	return a < b
}

// numericSuffix parses a trailing run of digits.
func numericSuffix(s string) (int, bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return 0, false
	}
	n := 0
	for _, c := range s[i:] {
		n = n*10 + int(c-'0')
	}
	return n, true
}
