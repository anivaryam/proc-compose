package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
)

// Completed fixtures use the real runner so pruning tests depend on genuine
// terminal/cleanup evidence, not a second hand-written success contract.
func completedJob(t *testing.T, s *Store, opts verify.Options, age time.Duration) *View {
	t.Helper()
	v, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Work(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	rec, dir, err := s.read(v.ID)
	if err != nil || rec.Result == nil || rec.Result.ExitCode != 0 {
		t.Fatalf("fixture: %+v %v", rec, err)
	}
	finished := time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
	rec.Result.FinishedAt = &finished
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPrune_PreviewAgeCountBatchAndImmutableIdentity(t *testing.T) {
	s, opts := fixture(t)
	oldest := completedJob(t, s, opts, 10*24*time.Hour)
	old := completedJob(t, s, opts, 9*24*time.Hour)
	newest := completedJob(t, s, opts, time.Hour)
	policy := PruneOptions{OlderThan: 7 * 24 * time.Hour, Keep: 1, Limit: 1, DryRun: true}
	preview, err := s.Prune(policy)
	if err != nil || preview.Eligible != 2 || !preview.HasMore || len(preview.Entries) != 1 || preview.Entries[0].Status != "would_prune" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	for _, v := range []*View{oldest, old, newest} {
		if got, err := s.Get(v.ID); err != nil || got.State != "passed" {
			t.Fatalf("preview changed job: %+v %v", got, err)
		}
	}
	policy.DryRun = false
	for range 2 {
		result, err := s.Prune(policy)
		if err != nil || len(result.Entries) != 1 || result.Entries[0].Status != "pruned" {
			t.Fatalf("prune: %+v %v", result, err)
		}
	}
	for _, v := range []*View{oldest, old} {
		got, err := s.Get(v.ID)
		if err != nil || got.State != "pruned" || got.Result != nil || got.PrunedAt == nil {
			t.Fatalf("tombstone: %+v %v", got, err)
		}
		files, err := os.ReadDir(v.Directory)
		if err != nil || len(files) != 2 || files[0].Name() != "job.json" || files[1].Name() != "worker.lock" {
			t.Fatalf("evidence remains: %v %v", files, err)
		}
		if err := s.Work(context.Background(), v.ID); err == nil {
			t.Fatal("pruned job replayed")
		}
	}
	page, err := s.List(20, "")
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].ID != newest.ID {
		t.Fatalf("list includes tombstones: %+v %v", page, err)
	}
	policy.Keep, policy.OlderThan = 0, 2*time.Hour
	if result, err := s.Prune(policy); err != nil || result.Eligible != 0 {
		t.Fatalf("age ignored: %+v %v", result, err)
	}
	policy.Keep, policy.OlderThan = 1, 0
	if result, err := s.Prune(policy); err != nil || result.Eligible != 0 {
		t.Fatalf("keep ignored: %+v %v", result, err)
	}
}

func TestPrune_ProtectsWorkerLeaseAndUnverifiedStates(t *testing.T) {
	s, opts := fixture(t)
	verified := completedJob(t, s, opts, time.Hour)
	unverified := completedJob(t, s, opts, time.Hour)
	rec, dir, _ := s.read(unverified.ID)
	rec.Result.Cleanup.TerminationVerified = nil
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	corruptLease, err := session.LeaseFile(filepath.Join(corrupt.Directory, "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	corruptLease.Close()
	if err := os.WriteFile(filepath.Join(corrupt.Directory, "job.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	abandoned, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	rec, dir, _ = s.read(abandoned.ID)
	rec.State = "running"
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	lease, err := session.LeaseFile(filepath.Join(verified.Directory, "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	policy := PruneOptions{Keep: 0, Limit: 100}
	result, err := s.Prune(policy)
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Status != "protected" {
		t.Fatalf("held lease: %+v %v", result, err)
	}
	for _, v := range []*View{verified, unverified, queued, abandoned, corrupt} {
		if _, err := os.Stat(filepath.Join(v.Directory, "job.json")); err != nil {
			t.Fatalf("protected job removed: %v", err)
		}
	}
	lease.Close()
	result, err = s.Prune(policy)
	if err != nil || len(result.Entries) != 1 || result.Entries[0].ID != verified.ID || result.Entries[0].Status != "pruned" {
		t.Fatalf("lease release: %+v %v", result, err)
	}
}

func TestPrune_PreAcquisitionRefusalHasNoOwnedProcesses(t *testing.T) {
	s, opts := fixture(t)
	opts.Dir = "missing-directory"
	v, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Work(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(v.ID)
	if err != nil || got.Result == nil || got.Result.ExitCode != verify.ExitUsage || got.Result.AttemptID != nil {
		t.Fatalf("fixture did not refuse: %+v %v", got, err)
	}
	result, err := s.Prune(PruneOptions{Limit: 20})
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Status != "pruned" {
		t.Fatalf("never-owned refusal: %+v %v", result, err)
	}
}

func TestPrune_ResumeAndConcurrentCalls(t *testing.T) {
	s, opts := fixture(t)
	v := completedJob(t, s, opts, time.Hour)
	rec, dir, _ := s.read(v.ID)
	now := time.Now().UTC()
	rec.State, rec.Result, rec.OwnerPID, rec.PrunedAt = "pruning", nil, 0, &now
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	// Resume a previously committed deletion even if the new policy keeps all
	// normal completions. Competing pruning calls never replace the lock inode.
	lockBefore, err := os.Stat(filepath.Join(dir, "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Prune(PruneOptions{Keep: 100, OlderThan: 24 * time.Hour, Limit: 20})
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(v.ID)
	if err != nil || got.State != "pruned" {
		t.Fatalf("resume: %+v %v", got, err)
	}
	lockAfter, err := os.Stat(filepath.Join(dir, "worker.lock"))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("pruning replaced the permanent worker lock")
	}
}

func TestPrune_RefusesRedirectedOrCorruptMetadata(t *testing.T) {
	s, opts := fixture(t)
	v := completedJob(t, s, opts, time.Hour)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(v.Directory, "external")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result, err := s.Prune(PruneOptions{Limit: 20})
	if err != nil || result.Entries[0].Status != "pruned" {
		t.Fatalf("prune with leaf symlink: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("followed an artifact symlink")
	}
	redirect, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(redirect.Directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, redirect.Directory); err != nil {
		t.Fatal(err)
	}
	result, err = s.Prune(PruneOptions{Limit: 20})
	if err != nil || result.Protected != 1 || len(result.Entries) != 0 {
		t.Fatalf("redirected job: %+v %v", result, err)
	}
}
