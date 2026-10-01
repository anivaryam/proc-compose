//go:build windows

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"

	"golang.org/x/sys/windows"
)

// These tests exercise windowsProcessGroup itself, on Windows. A transcription of
// it built elsewhere would not be the implementation under test, and the Windows
// Job Object is the only thing that gives the runner authority over a tree once
// the command leader has exited — so the properties below have to be verified
// against the real thing.

// startGroupForJobTest starts cmdStr in its own Job Object and returns the group
// plus the Cmd.
//
// Cleanup releases the job before killing the command: closing the last job
// handle terminates everything still assigned to it, which is what makes a failing
// assertion unable to leak a process.
func startGroupForJobTest(t *testing.T, cmdStr string) (*windowsProcessGroup, *exec.Cmd) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "cmd", "/c", cmdStr)
	pg := newProcessGroup().(*windowsProcessGroup)
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	if err := pg.Track(cmd); err != nil {
		cancel()
		_ = pg.Close()
		t.Fatalf("track: %v", err)
	}
	t.Cleanup(func() {
		// Release the job first: KillOnJobClose terminates any remaining member.
		_ = pg.Close()
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return pg, cmd
}

// TestWindowsJob_HandleOutlivesLeader covers the handle half of successful
// completion: the Job Object must stay usable for verification once the leader has
// been collected.
//
// This is handle-level coverage only. The defect this was originally written
// against — a run that never finished because the exit status was received twice —
// lived in Runner.exec, not here, and is covered by
// TestWindowsRun_SuccessfulTaskCompletes through the runner.
func TestWindowsJob_HandleOutlivesLeader(t *testing.T) {
	pg, cmd := startGroupForJobTest(t, helperCmd("exit", "0"))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() = %v, want a clean exit", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Wait never returned for a successful task")
	}

	// The job must still be usable for verification after the leader is gone,
	// and must report an empty group.
	if got := pg.State(); got != GroupEmpty {
		t.Fatalf("State() after the leader exited = %v, want %v", got, GroupEmpty)
	}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWindowsJob_AuthoritySurvivesLeaderExit pins the property that makes Windows
// different from Unix: the job handle keeps its authority after the leader exits,
// so descendants can still be terminated and the result verified.
func TestWindowsJob_AuthoritySurvivesLeaderExit(t *testing.T) {
	if !durableContainment {
		t.Fatal("durableContainment is false on Windows; the Windows path would be the Unix fallback")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// The leader starts a long-lived child and exits at once. Terminating the job
	// must reach that child even though the leader is already gone.
	pg, cmd := startGroupForJobTest(t, helperCmd("spawn-job-child", pidFile))

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(60 * time.Second):
		t.Fatal("leader never exited")
	}

	child := waitForJobChildPID(t, pidFile)
	// The recorded pid is the child's, and it must be alive before anything is
	// asserted about terminating it. Asserting on a pid that was never running
	// would make the termination check below vacuous.
	if child == cmd.Process.Pid {
		t.Fatalf("pid file recorded the leader (%d), not a surviving child; the fixture "+
			"would prove nothing about the job", child)
	}
	if !windowsProcessRunning(child) {
		t.Fatalf("child (PID %d) is not running before the job is terminated; the fixture "+
			"proves nothing", child)
	}
	defer func() {
		// Best effort: the assertion below should have terminated it already.
		_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(child)).Run()
	}()

	// The leader has been collected and its job membership would be unprovable on
	// Unix. Here it must still be reportable and terminable.
	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() with the leader already exited = %v, want %v", got, GroupOwned)
	}

	// Withdraw is a no-op on Windows by design: authority lives in the handle.
	pg.Withdraw()
	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill() after the leader exited = %v, want the job to be terminated", err)
	}
	waitWhileLive(pg, 30*time.Second)

	if got := pg.State(); got != GroupEmpty {
		t.Fatalf("State() after terminating the job = %v, want %v", got, GroupEmpty)
	}
	if windowsProcessRunning(child) {
		t.Fatalf("child (PID %d) survived a job terminate; the Job Object must reach "+
			"members whose parent has already exited", child)
	}
}

// TestWindowsJob_UnavailableContainmentIsReported pins the containment-failure
// contract.
//
// A Job Object that could not be created must not leave a command running with no
// containment while Track reports success. It has to surface as an error, and
// State() must stay unverifiable rather than claiming an empty group.
func TestWindowsJob_UnavailableContainmentIsReported(t *testing.T) {
	// A group that was never created models a failed job object creation.
	pg := &windowsProcessGroup{createErr: errors.New("job object could not be created")}

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find self: %v", err)
	}
	if err := pg.Track(&exec.Cmd{Process: self}); err == nil {
		t.Fatal("Track() with no job object = nil, want an error")
	}
	if got := pg.State(); got != GroupUnavailable {
		t.Fatalf("State() with no containment = %v, want %v", got, GroupUnavailable)
	}
	if pg.Identify() != "unknown" {
		t.Fatalf("Identify() with no containment = %q, want %q", pg.Identify(), "unknown")
	}
	// Signalling must report that there is nothing to terminate rather than acting
	// on a handle that was never created.
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill() with no containment = %v, want os.ErrProcessDone", err)
	}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close() with no containment = %v, want nil", err)
	}
}

// TestWindowsJob_TrackRefusesAClosedHandle covers the other containment failure:
// the job was created but is no longer usable.
func TestWindowsJob_TrackRefusesAClosedHandle(t *testing.T) {
	pg, cmd := startGroupForJobTest(t, helperCmd("sleep", "30s"))
	if err := pg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := pg.Track(cmd); err == nil {
		t.Fatal("Track() on a closed job = nil, want an error")
	}
	if got := pg.State(); got != GroupUnavailable {
		t.Fatalf("State() after Close = %v, want %v", got, GroupUnavailable)
	}
	// A closed group must stay closed rather than being closed twice.
	if err := pg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestWindowsJob_ConcurrentUseAndClose is the handle-lifetime regression.
//
// Kill, State and Close all dereference the job handle, and Close releases it. If
// the mutex only covered reading the pointer, Close could free the handle while a
// Terminate or QueryCounters was still using it. This asserts that use and
// release cannot overlap, under -race as well as directly.
func TestWindowsJob_ConcurrentUseAndClose(t *testing.T) {
	pg, _ := startGroupForJobTest(t, helperCmd("sleep", "120s"))

	var wg sync.WaitGroup
	const rounds = 16
	for i := 0; i < rounds; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = pg.Kill()
		}()
		go func() {
			defer wg.Done()
			_ = pg.State()
		}()
		go func() {
			defer wg.Done()
			_ = pg.Close()
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()

	select {
	case <-finished:
	case <-time.After(120 * time.Second):
		t.Fatal("concurrent use and close deadlocked on the job handle")
	}

	// Once closed the group must report unverifiable rather than dereference the
	// released handle again.
	if got := pg.State(); got != GroupUnavailable {
		t.Fatalf("State() after Close = %v, want %v", got, GroupUnavailable)
	}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close() after concurrent use = %v, want nil", err)
	}
}

// TestWindowsJob_WithdrawDoesNotReleaseAuthority pins that the sequencing step
// cannot strand live members: withdrawing records intent, and the handle stays
// usable until Close.
func TestWindowsJob_WithdrawDoesNotReleaseAuthority(t *testing.T) {
	pg, _ := startGroupForJobTest(t, helperCmd("sleep", "120s"))

	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() before Withdraw = %v, want %v", got, GroupOwned)
	}
	pg.Withdraw()
	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() after Withdraw = %v, want %v; the handle must stay authoritative "+
			"until Close", got, GroupOwned)
	}
	if pg.Identify() == "unknown" {
		t.Fatal("Identify() after Withdraw = unknown, want the job to still be identified")
	}
}

// waitForJobChildPID blocks until the backgrounded child records its PID.
func waitForJobChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(trimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatal("backgrounded child never recorded its PID")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// TestWindowsRun_SuccessfulTaskCompletes is the runner-level regression for a run
// that could never finish.
//
// A durable-containment platform detects natural completion by reaping, and the
// lifecycle needs the exit status a second time at the end, where the reap is
// sequenced after signalling. Receiving it twice from a single-send channel blocked
// forever, so a successfully completing task never returned.
//
// This goes through Runner.Run because the defect lived there. A test that calls
// cmd.Wait directly cannot see it.
func TestWindowsRun_SuccessfulTaskCompletes(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		// Mode matters: without it the process is a service, and a task takes
		// the dependency and stack-completion path rather than the service path.
		"task": {Cmd: helperCmd("exit", "0"), Mode: config.ProcessModeTask, Restart: "never", ShutdownTimeout: 5},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want a clean completion", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("Run never returned for a successful task; the exit status was probably " +
			"received twice from a single-send channel")
	}

	if err := r.ShutdownResult(); err != nil {
		t.Fatalf("successful task produced a shutdown verdict: %v", err)
	}
}

// TestWindowsRun_SuccessfulServiceExitsNaturally covers the same completion path
// for a service-shaped process rather than a one-shot task.
//
// The distinction matters: a service registers with the runner, reports state
// transitions, and is torn down on cancellation, so it exercises more of exec than a
// task does.
func TestWindowsRun_SuccessfulServiceExitsNaturally(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"svc": {Cmd: helperCmd("exit", "0"), Restart: "never", ShutdownTimeout: 5},
	})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want a clean completion", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("Run never returned for a naturally exiting service")
	}

	if err := r.ShutdownResult(); err != nil {
		t.Fatalf("natural service exit produced a shutdown verdict: %v", err)
	}
}

// TestWindowsRun_StopAfterNaturalExitIsClean is the stack-level counterpart: a
// managed process that has already finished must not turn the subsequent stop into
// a reported failure.
func TestWindowsRun_StopAfterNaturalExitIsClean(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "svc.pid")

	r := makeRunner(map[string]config.Process{
		"svc":    {Cmd: helperCmd("record-and-exit", pidFile), Restart: "never", ShutdownTimeout: 5},
		"keeper": {Cmd: helperCmd("sleep", "120s"), Restart: "never", ShutdownTimeout: 5},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	pid, ok := waitForPIDFile(t, pidFile, 60*time.Second)
	if !ok {
		cancel()
		t.Fatal("service never recorded its PID")
	}
	trackProcessForCleanup(t, pid, "natural-exit service")

	// The pid file only proves the command started. What this test needs is the
	// command's completed lifecycle: cancelling while it is still running would
	// exercise the shutdown path, not a stop after a natural exit.
	if !waitForProcState(t, r, "svc", func(st ipc.ProcState) bool {
		return st.State == "exited"
	}, 60*time.Second) {
		cancel()
		t.Fatal("service never reached the exited state; the stop would not be " +
			"covering a natural completion")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after a natural exit = %v, want a clean stop", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("Run did not finish shutting down after a natural exit")
	}

	if err := r.ShutdownResult(); err != nil {
		t.Fatalf("stop after a natural exit reported %v, want a clean verdict", err)
	}
}

// windowsProcessRunning reports whether pid is still executing.
//
// The shared pidIsRunning helper is not usable here: it can only tell a terminated
// process from a live one by reading /proc, which does not exist on Windows, so a
// terminated-but-unhandled process still reads as running. This asks the kernel for
// the exit code instead, which is authoritative on every Windows version.
func windowsProcessRunning(pid int) bool {
	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259
	)
	handle, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}
