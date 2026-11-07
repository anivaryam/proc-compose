package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/logs"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
)

func fixture(t *testing.T) (*Store, verify.Options) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	return &Store{Root: t.TempDir()}, verify.Options{
		ConfigPath: filepath.Join(t.TempDir(), "proc-compose.yml"), Standalone: true,
		Argv: []string{os.Args[0], "-test.run=^$"}, StartupTimeout: 5 * time.Second,
		CheckTimeout: 5 * time.Second, CleanupTimeout: 30 * time.Second,
	}
}

func TestWork_RetainsVerdictAndLogsBeyondRuntime(t *testing.T) {
	s, opts := fixture(t)
	v, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if v.Terminal || v.State != "queued" || v.Result != nil || !strings.HasPrefix(v.Session, "job-") {
		t.Fatalf("submission claimed a verdict: %+v", v)
	}
	if err := s.Work(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(os.Getenv("XDG_RUNTIME_DIR")); err != nil {
		t.Fatal(err)
	}
	v, err = s.Get(v.ID)
	if err != nil || !v.Terminal || v.State != "passed" || v.Result == nil || v.Result.Cleanup.Status != "passed" {
		t.Fatalf("retained verdict: %+v %v", v, err)
	}
	if !strings.HasPrefix(*v.Result.ReceiptPath, v.Directory+string(os.PathSeparator)) || !v.Result.ReceiptPersisted {
		t.Fatalf("receipt is not retained with job: %+v", v.Result)
	}
	if _, err := verify.ReadReceipt(*v.Result.ReceiptPath); err != nil {
		t.Fatal(err)
	}
	page, err := s.Logs(v.ID, logs.Options{After: "start", Limit: 100})
	if err != nil || len(page.Events) == 0 || page.NextCursor == "" {
		t.Fatalf("retained logs: %+v %v", page, err)
	}
	if err := s.Work(context.Background(), v.ID); err == nil {
		t.Fatal("completed job replayed")
	}
}

func TestWork_CancelBeforeClaimStartsNothing(t *testing.T) {
	s, opts := fixture(t)
	v, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(v.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Work(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	v, err = s.Get(v.ID)
	if err != nil || v.State != "interrupted" || !v.Terminal || v.Result.Check.StartedAt != nil || v.Result.ExitCode != 130 {
		t.Fatalf("pre-claim cancellation: %+v %v", v, err)
	}
}

func TestGet_LeaseLossNeverReportsPass(t *testing.T) {
	s, opts := fixture(t)
	v, err := s.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := session.LeaseFile(filepath.Join(v.Directory, "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	rec, _, _ := s.read(v.ID)
	rec.State, rec.OwnerPID = "running", os.Getpid()
	if err := save(filepath.Join(v.Directory, "job.json"), rec); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(v.ID); err != nil || got.State != "running" || got.Terminal {
		t.Fatalf("held lease: %+v %v", got, err)
	}
	lease.Close()
	got, err := s.Get(v.ID)
	if err != nil || got.State != "abandoned" || got.Terminal {
		t.Fatalf("lost lease with live PID: %+v %v", got, err)
	}
	if _, err := s.Cancel(v.ID); err == nil {
		t.Fatal("abandoned cancellation claimed success")
	}
	if err := s.Work(context.Background(), v.ID); err == nil {
		t.Fatal("abandoned job replayed")
	}
}

func TestList_BoundedPagesAndCorruptVerdict(t *testing.T) {
	s, opts := fixture(t)
	for range 3 {
		if _, err := s.Prepare(opts); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(2, "")
	if err != nil || len(page.Jobs) != 2 || !page.HasMore {
		t.Fatalf("first page: %+v %v", page, err)
	}
	next, err := s.List(2, page.Before)
	if err != nil || len(next.Jobs) != 1 || next.HasMore || next.Jobs[0].ID >= page.Before {
		t.Fatalf("next page: %+v %v", next, err)
	}
	id := next.Jobs[0].ID
	rec, dir, _ := s.read(id)
	rec.State, rec.Result = "completed", &verify.Result{Status: "passed", ExitCode: 0}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "job.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); err == nil {
		t.Fatal("corrupt terminal result accepted")
	}
	if _, err := s.Get("../outside"); err == nil {
		t.Fatal("path traversal accepted as a job ID")
	}
}
