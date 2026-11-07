package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
)

type PruneOptions struct {
	OlderThan time.Duration
	Keep      int
	Limit     int
	DryRun    bool
}

type PruneResult struct {
	Version   int          `json:"schema_version"`
	Kind      string       `json:"kind"`
	DryRun    bool         `json:"dry_run"`
	Cutoff    time.Time    `json:"finished_before"`
	Keep      int          `json:"keep"`
	Eligible  int          `json:"eligible"`
	Protected int          `json:"protected"`
	HasMore   bool         `json:"has_more"`
	Entries   []PruneEntry `json:"entries"`
}

type PruneEntry struct {
	ID      string `json:"job_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// prunableFinish requires positive cleanup evidence, including pre-acquisition
// refusals that never owned processes. Lost workers/compact failures don't have
// that evidence and remain available for investigation.
func prunableFinish(rec *Record) (time.Time, bool) {
	r := rec.Result
	if rec.State != "completed" || !verify.ValidTerminalResult(r) {
		return time.Time{}, false
	}
	verified := r.Cleanup.Status == verify.StatusPassed && r.Cleanup.SignalingAuthority == "withdrawn" && r.Cleanup.TerminationVerified != nil && *r.Cleanup.TerminationVerified
	neverOwned := r.Cleanup.Status == verify.StatusNotNeeded && r.Cleanup.SignalingAuthority == "none" && r.AttemptID == nil && r.RunID == nil && r.Check.StartedAt == nil && r.Owner == nil
	if !verified && !neverOwned {
		return time.Time{}, false
	}
	finished, err := time.Parse(time.RFC3339Nano, *r.FinishedAt)
	return finished, err == nil && !finished.IsZero()
}

// pruneRecord refuses redirected/non-regular metadata before opening it. In
// particular a FIFO cannot turn retention into an unbounded read.
func (s *Store) pruneRecord(id string) (*Record, string, error) {
	dir, err := s.directory(id)
	if err != nil {
		return nil, dir, err
	}
	for _, path := range []string{dir, filepath.Join(dir, "job.json"), filepath.Join(dir, "worker.lock")} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, dir, err
		}
		if (path == dir && !info.IsDir()) || (path != dir && !info.Mode().IsRegular()) {
			return nil, dir, fmt.Errorf("job path is not a regular directory/file: %s", path)
		}
	}
	return s.read(id)
}

// Prune deletes retained evidence, not identities. The worker lease inode and
// a minimal tombstone stay permanently: no lock file is ever unlinked while a
// process could hold it, including on Windows. The pruning state is durable
// before deletion; interrupted deletions are resumable on a later invocation.
func (s *Store) Prune(opts PruneOptions) (*PruneResult, error) {
	if !filepath.IsAbs(s.Root) || opts.OlderThan < 0 || opts.Keep < 0 || opts.Keep > 10000 || opts.Limit < 1 || opts.Limit > 100 {
		return nil, errors.New("prune requires an absolute store, nonnegative age, keep 0..10000, and limit 1..100")
	}
	result := &PruneResult{Version: 1, Kind: "jobs.prune", DryRun: opts.DryRun, Cutoff: time.Now().UTC().Add(-opts.OlderThan), Keep: opts.Keep, Entries: []PruneEntry{}}
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id       string
		finished time.Time
	}
	var completed, pending []candidate
	for _, entry := range entries {
		if !verify.ValidAttemptID(entry.Name()) {
			continue
		}
		rec, _, err := s.pruneRecord(entry.Name())
		if err != nil {
			result.Protected++
			continue
		}
		if rec.State == "pruned" {
			continue
		}
		if rec.State == "pruning" && rec.PrunedAt != nil {
			pending = append(pending, candidate{rec.ID, *rec.PrunedAt})
			continue
		}
		finished, ok := prunableFinish(rec)
		if !ok {
			result.Protected++
			continue
		}
		completed = append(completed, candidate{rec.ID, finished})
	}
	sort.Slice(completed, func(i, j int) bool {
		if completed[i].finished.Equal(completed[j].finished) {
			return completed[i].id > completed[j].id
		}
		return completed[i].finished.After(completed[j].finished)
	})
	candidates := pending
	for i, c := range completed {
		if i < opts.Keep || !c.finished.Before(result.Cutoff) {
			result.Protected++
			continue
		}
		candidates = append(candidates, c)
	}
	result.Eligible, result.HasMore = len(candidates), len(candidates) > opts.Limit
	failed := false
	for _, c := range candidates[:min(len(candidates), opts.Limit)] {
		entry := s.pruneOne(c.id, result.Cutoff, opts.DryRun)
		if entry.Status == "protected" {
			result.Protected++
		}
		failed = failed || entry.Status == "error"
		result.Entries = append(result.Entries, entry)
	}
	if failed {
		return result, errors.New("some job evidence could not be pruned; inspect entries and retry")
	}
	return result, nil
}

func (s *Store) pruneOne(id string, cutoff time.Time, dryRun bool) PruneEntry {
	entry := PruneEntry{ID: id, Status: "protected"}
	_, dir, err := s.pruneRecord(id)
	if err != nil {
		entry.Message = "job metadata cannot be safely read"
		return entry
	}
	lease, err := session.LeaseFile(filepath.Join(dir, "worker.lock"))
	if err != nil {
		entry.Message = "worker lease is held or unavailable"
		return entry
	}
	defer lease.Close()
	rec, _, err := s.pruneRecord(id)
	if err != nil {
		entry.Message = "job metadata changed or became unreadable"
		return entry
	}
	if rec.State == "pruned" {
		entry.Status = "already_pruned"
		return entry
	}
	if rec.State != "pruning" {
		finished, ok := prunableFinish(rec)
		if !ok || !finished.Before(cutoff) {
			return entry
		}
	} else if rec.PrunedAt == nil {
		return entry
	}
	if dryRun {
		entry.Status = "would_prune"
		return entry
	}
	if rec.State != "pruning" {
		now := time.Now().UTC()
		rec = &Record{Version: 1, ID: id, State: "pruning", CreatedAt: rec.CreatedAt, PrunedAt: &now,
			Request: Request{ConfigPath: rec.Request.ConfigPath, Session: rec.Request.Session, TaskName: rec.Request.TaskName}}
		if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
			entry.Status, entry.Message = "error", "cannot persist pruning tombstone"
			return entry
		}
	}
	files, err := os.ReadDir(dir)
	if err == nil {
		for _, file := range files {
			if file.Name() == "worker.lock" || file.Name() == "job.json" {
				continue
			}
			// RemoveAll unlinks symlinks instead of following their targets.
			if err = os.RemoveAll(filepath.Join(dir, file.Name())); err != nil {
				break
			}
		}
	}
	if err != nil {
		entry.Status, entry.Message = "error", "evidence removal incomplete; pruning will resume next time"
		return entry
	}
	rec.State = "pruned"
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		entry.Status, entry.Message = "error", "cannot finalize pruning tombstone; retry is safe"
		return entry
	}
	entry.Status = "pruned"
	return entry
}
