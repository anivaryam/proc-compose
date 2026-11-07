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
