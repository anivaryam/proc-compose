package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/jobs"
	"github.com/anivaryam/proc-compose/internal/verify"
)

func decodeJob(t *testing.T, stdout string) jobs.View {
	t.Helper()
	var v jobs.View
	if err := json.Unmarshal([]byte(stdout), &v); err != nil || v.ID == "" {
		t.Fatalf("invalid job response: %s (%v)", stdout, err)
	}
	return v
}

func cleanupJob(t *testing.T, store *jobs.Store, id string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = store.Cancel(id)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		view, err := store.Wait(ctx, id)
		if err != nil || !view.Terminal {
			t.Errorf("job %s did not finish cleanup: %+v %v", id, view, err)
		}
	})
}

func TestJobArtifactHelper(t *testing.T) {
	path := os.Getenv("PROC_COMPOSE_JUNIT_REPORT")
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(`<testsuite><testcase name="regression"><failure message="expected 2, got 1"/></testcase></testsuite>`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("PROC_COMPOSE_ARTIFACT_DIR"), "trace.txt"), []byte("raw trace"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestJobsCLI_RetainedArtifactsAndJUnitFailure(t *testing.T) {
	bin := buildTestBinary(t)
	dir, runtimeDir := t.TempDir(), t.TempDir()
	t.Setenv("PROC_COMPOSE_JOB_DIR", t.TempDir())
	store, err := jobs.Open()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runVerifyCLIIn(t, bin, dir, runtimeDir, "run", "--background", "--json", "--", os.Args[0], "-test.run=^TestJobArtifactHelper$")
	if code != 0 {
		t.Fatalf("launch: %d %s %s", code, stdout, stderr)
	}
	job := decodeJob(t, stdout)
	cleanupJob(t, store, job.ID)
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "wait", job.ID, "--json", "--timeout", "10s")
	job = decodeJob(t, stdout)
	if code != 1 || !job.Terminal || job.Result == nil || job.Result.Artifacts == nil || job.Result.Artifacts.JUnit.Failures != 1 || *job.Result.Check.ExitCode != 0 || job.Result.Cleanup.Status != "passed" {
		t.Fatalf("retained report failure: %d %s %s", code, stdout, stderr)
	}
	if err := os.RemoveAll(runtimeDir); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, t.TempDir(), "jobs", "show", job.ID, "--json")
	job = decodeJob(t, stdout)
	if code != 0 || len(job.Result.Artifacts.Files) != 2 || job.Result.Artifacts.JUnit.Examples[0].Name != "regression" {
		t.Fatalf("reconnect: %d %s %s", code, stdout, stderr)
	}
	if data, err := os.ReadFile(filepath.Join(job.Result.Artifacts.Directory, "trace.txt")); err != nil || string(data) != "raw trace" {
		t.Fatalf("retained raw file: %q %v", data, err)
	}
}

func TestJobsCLI_BackgroundReconnectFailureAndCancel(t *testing.T) {
	bin := buildTestBinary(t)
	dir, runtimeDir := t.TempDir(), t.TempDir()
	t.Setenv("PROC_COMPOSE_JOB_DIR", t.TempDir())
	store, err := jobs.Open()
	if err != nil {
		t.Fatal(err)
	}
	launch := func(argv ...string) jobs.View {
		t.Helper()
		args := append([]string{"run", "--background", "--json", "--"}, argv...)
		stdout, stderr, code := runVerifyCLIIn(t, bin, dir, runtimeDir, args...)
		if code != 0 {
			t.Fatalf("launch: exit %d stdout %s stderr %s", code, stdout, stderr)
		}
		v := decodeJob(t, stdout)
		cleanupJob(t, store, v.ID)
		return v
	}

	passed := launch(bin, "--version")
	// A new CLI in another directory and runtime namespace retrieves the job.
	stdout, stderr, code := runVerifyCLI(t, bin, t.TempDir(), "jobs", "wait", passed.ID, "--json", "--timeout", "10s")
	if v := decodeJob(t, stdout); code != 0 || !v.Terminal || v.State != "passed" || v.Result.Cleanup.Status != "passed" {
		t.Fatalf("reconnect: code %d view %+v stderr %s", code, v, stderr)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "logs", passed.ID, "--json", "--after", "start")
	page := decodeOne(t, stdout)
	if code != 0 || len(page["events"].([]any)) == 0 || page["next_cursor"] == "" {
		t.Fatalf("logs: code %d page %v stderr %s", code, page, stderr)
	}
	failed := launch(bin, "unknown-command")
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "wait", failed.ID, "--json", "--timeout", "10s")
	if v := decodeJob(t, stdout); code != 1 || v.State != "failed" || v.Result.Check.ExitCode == nil || *v.Result.Check.ExitCode != 1 {
		t.Fatalf("failure: code %d view %+v stderr %s", code, v, stderr)
	}

	marker, ready := filepath.Join(dir, "signal"), filepath.Join(dir, "ready")
	active := launch(os.Args[0], "-test.run=^TestSignalWitnessHelper$", "--", signalWitnessSentinel, marker, ready)
	waitForFile(t, ready, 10*time.Second)
	otherReady := filepath.Join(dir, "other-ready")
	other := launch(os.Args[0], "-test.run=^TestSignalWitnessHelper$", "--", signalWitnessSentinel, filepath.Join(dir, "other-signal"), otherReady)
	waitForFile(t, otherReady, 10*time.Second)
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "prune", "--keep", "0", "--older-than", "0s", "--json")
	if result := decodeOne(t, stdout); code != 0 || result["kind"] != "jobs.prune" {
		t.Fatalf("prune during live jobs: %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "wait", active.ID, "--json", "--timeout", "1ms")
	if v := decodeJob(t, stdout); code != 3 || v.Terminal || !v.WaitTimedOut {
		t.Fatalf("finite wait: code %d view %+v stderr %s", code, v, stderr)
	}
	// A wait timeout must not stop the worker or its check.
	stdout, _, code = runVerifyCLI(t, bin, dir, "jobs", "show", active.ID, "--json")
	if v := decodeJob(t, stdout); code != 0 || v.State != "running" || v.CancelRequested {
		t.Fatalf("wait changed the job: %+v code %d", v, code)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "cancel", active.ID, "--json", "--timeout", "40s")
	if v := decodeJob(t, stdout); code != 130 || !v.Terminal || v.State != "interrupted" || v.Result.Cleanup.Status != "passed" {
		t.Fatalf("cancel: code %d view %+v stderr %s", code, v, stderr)
	}
	stdout, _, code = runVerifyCLI(t, bin, dir, "jobs", "show", other.ID, "--json")
	if v := decodeJob(t, stdout); code != 0 || v.State != "running" || v.CancelRequested {
		t.Fatalf("cancellation touched another job: %+v code %d", v, code)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "show", "../../other", "--json")
	if code == 0 || !strings.Contains(stdout, `"status": "error"`) {
		t.Fatalf("invalid ID: code %d stdout %s stderr %s", code, stdout, stderr)
	}
}

func TestJobsCLI_PrunePreviewAndRetainedTombstone(t *testing.T) {
	bin := buildTestBinary(t)
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PROC_COMPOSE_JOB_DIR", t.TempDir())
	store, err := jobs.Open()
	if err != nil {
		t.Fatal(err)
	}
	v, err := store.Prepare(verify.Options{ConfigPath: filepath.Join(dir, "proc-compose.yml"), Standalone: true, Argv: []string{bin, "--version"}, StartupTimeout: verify.DefaultStartupTimeout, CheckTimeout: verify.DefaultCheckTimeout, CleanupTimeout: verify.DefaultCleanupTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Work(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runVerifyCLI(t, bin, dir, "jobs", "prune", "--keep", "0", "--older-than", "0s", "--dry-run", "--json")
	result := decodeOne(t, stdout)
	if code != 0 || result["dry_run"] != true || result["entries"].([]any)[0].(map[string]any)["status"] != "would_prune" {
		t.Fatalf("preview: %d %s %s", code, stdout, stderr)
	}
	if got, err := store.Get(v.ID); err != nil || got.State != "passed" {
		t.Fatalf("preview changed result: %+v %v", got, err)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "prune", "--keep", "0", "--older-than", "0s", "--json")
	result = decodeOne(t, stdout)
	if code != 0 || result["entries"].([]any)[0].(map[string]any)["status"] != "pruned" {
		t.Fatalf("prune: %d %s %s", code, stdout, stderr)
	}
	stdout, _, code = runVerifyCLI(t, bin, dir, "jobs", "show", v.ID, "--json")
	if got := decodeJob(t, stdout); code != 0 || got.State != "pruned" || got.Result != nil {
		t.Fatalf("tombstone: %+v exit%d", got, code)
	}
	stdout, _, code = runVerifyCLI(t, bin, dir, "jobs", "wait", v.ID, "--json")
	if got := decodeJob(t, stdout); code != 1 || got.State != "pruned" {
		t.Fatalf("pruned wait: %+v exit%d", got, code)
	}
	stdout, _, code = runVerifyCLI(t, bin, dir, "jobs", "logs", v.ID, "--json")
	if result := decodeOne(t, stdout); code != 1 || result["status"] != "error" {
		t.Fatalf("pruned logs: %d %s", code, stdout)
	}
	stdout, _, code = runVerifyCLI(t, bin, dir, "jobs", "prune", "--keep", "-1", "--json")
	if result := decodeOne(t, stdout); code == 0 || result["status"] != "error" {
		t.Fatalf("invalid policy: %d %s", code, stdout)
	}
}

func TestJobsCLI_BackgroundVerifyRefusesOccupiedSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("service fixture uses POSIX sleep; standalone worker coverage is portable")
	}
	bin := buildTestBinary(t)
	dir, runtimeDir := t.TempDir(), t.TempDir()
	t.Setenv("PROC_COMPOSE_JOB_DIR", t.TempDir())
	store, err := jobs.Open()
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(dir, "proc-compose.yml"), "processes:\n  api:\n    cmd: exec sleep 60\n")
	ready := filepath.Join(dir, "ready")
	stdout, stderr, code := runVerifyCLIIn(t, bin, dir, runtimeDir,
		"--session", "occupied", "verify", "--background", "--json", "--",
		os.Args[0], "-test.run=^TestSignalWitnessHelper$", "--", signalWitnessSentinel, filepath.Join(dir, "signal"), ready)
	if code != 0 {
		t.Fatalf("verify launch: %d %s %s", code, stdout, stderr)
	}
	first := decodeJob(t, stdout)
	cleanupJob(t, store, first.ID)
	waitForFile(t, ready, 10*time.Second)
	stdout, stderr, code = runVerifyCLIIn(t, bin, dir, runtimeDir,
		"--session", "occupied", "verify", "--background", "--json", "--", bin, "--version")
	if code != 0 {
		t.Fatalf("second launch acknowledgement: %d %s %s", code, stdout, stderr)
	}
	second := decodeJob(t, stdout)
	cleanupJob(t, store, second.ID)
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "wait", second.ID, "--timeout", "10s", "--json")
	v := decodeJob(t, stdout)
	if code != 2 || v.State != "failed" || v.Result.Check.StartedAt != nil || !strings.Contains(stdout, "session_occupied") {
		t.Fatalf("occupied refusal: %d %+v %s", code, v, stderr)
	}
	if v, err := store.Get(first.ID); err != nil || v.State != "running" || v.CancelRequested {
		t.Fatalf("refusal changed the first job: %+v %v", v, err)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "jobs", "cancel", first.ID, "--timeout", "40s", "--json")
	v = decodeJob(t, stdout)
	if code != 130 || v.Result.Cleanup.Status != "passed" || len(v.Result.SelectedProcesses) != 1 || v.Result.Standalone {
		t.Fatalf("verify cleanup: %d %+v %s", code, v, stderr)
	}
}
