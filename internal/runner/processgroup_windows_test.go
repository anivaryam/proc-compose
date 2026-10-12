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
func startGroupForJobTest(t *testing.T, cmdStr string) (*windowsProcessGroup, *exec.Cmd) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pg, cmd := startSuspendedForJobTest(t, exec.CommandContext(ctx, "cmd", "/c", cmdStr))
	if err := pg.Track(cmd); err != nil {
		t.Fatalf("track: %v", err)
	}
	return pg, cmd
}

// startSuspendedForJobTest prepares cmd through a new group and starts it, but
// does not track it, so the command is created suspended and outside the job.
//
// Cleanup releases the job before killing the command through its own handle:
// closing the last job handle terminates everything still assigned to it, and the
// handle kill covers a command that was never assigned, which is what makes a
// failing assertion unable to leak a process.
func startSuspendedForJobTest(t *testing.T, cmd *exec.Cmd) (*windowsProcessGroup, *exec.Cmd) {
	t.Helper()
	pg := newProcessGroup().(*windowsProcessGroup)
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		_ = pg.Close()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = pg.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return pg, cmd
}

// directHelper builds a command that runs the test helper directly, with no
// shell in between, so the leader is the helper itself.
func directHelper(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunnerHelperProcess$", "--", helperSentinel, encodeHelperArgs(args...))
}

// assertNotExecuted fails if marker appears within the observation window. A
// suspended command cannot write it, however long it is held.
func assertNotExecuted(t *testing.T, marker string, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s ran before it was contained", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitProcessGone fails unless pid stops executing within timeout.
func waitProcessGone(t *testing.T, pid int, what string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for windowsProcessRunning(pid) {
		if !time.Now().Before(deadline) {
			t.Fatalf("%s (PID %d) is still running after %s", what, pid, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWindowsJob_ImmediateDescendantIsContained is the containment-before-
// execution regression. The leader spawns a long-lived child as its first action
// and exits. Track is held for a second after Start, the window in which the
// child used to be created outside the job; terminating the job must still reach
// it.
func TestWindowsJob_ImmediateDescendantIsContained(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	pg, cmd := startSuspendedForJobTest(t, directHelper(context.Background(), "spawn-job-child", pidFile))
	assertNotExecuted(t, pidFile, "leader")
	if err := pg.Track(cmd); err != nil {
		t.Fatalf("track: %v", err)
	}
	child := waitForJobChildPID(t, pidFile)
	defer func() { _ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(child)).Run() }()
	if !windowsProcessRunning(child) {
		t.Fatalf("child (PID %d) is not running before the job is terminated; the fixture proves nothing", child)
	}
	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill() = %v", err)
	}
	waitProcessGone(t, child, "immediate descendant", 30*time.Second)
	waitWhileLive(pg, 30*time.Second)
	if got := pg.State(); got != GroupEmpty {
		t.Fatalf("State() after terminating the job = %v, want %v", got, GroupEmpty)
	}
}

// TestWindowsJob_CancelBeforeTrackNeverRuns covers cancellation that lands
// between creation and containment. The command must be contained without ever
// running, and the owned job handle must end it, through Kill or through the
// kill-on-close of Seal.
func TestWindowsJob_CancelBeforeTrackNeverRuns(t *testing.T) {
	for _, end := range []string{"kill", "seal"} {
		t.Run(end, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pg, cmd := startSuspendedForJobTest(t, directHelper(ctx, "touch-block", marker))
			cancel()
			deadline := time.Now().Add(10 * time.Second)
			for {
				pg.mu.Lock()
				cancelled := pg.cancelled
				pg.mu.Unlock()
				if cancelled {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatal("context cancellation never reached the group's Cancel")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := pg.Track(cmd); err != nil {
				t.Fatalf("Track() after cancellation = %v, want the command contained", err)
			}
			if got := pg.State(); got != GroupOwned {
				t.Fatalf("State() of the contained command = %v, want %v", got, GroupOwned)
			}
			assertNotExecuted(t, marker, "cancelled command")
			switch end {
			case "kill":
				if err := pg.Kill(); err != nil {
					t.Fatalf("Kill() = %v", err)
				}
				waitWhileLive(pg, 30*time.Second)
				if got := pg.State(); got != GroupEmpty {
					t.Fatalf("State() after Kill = %v, want %v", got, GroupEmpty)
				}
			case "seal":
				observation, err := pg.Seal()
				if err != nil || observation.State != GroupOwned {
					t.Fatalf("Seal() = %+v, %v; want the suspended member observed before release", observation, err)
				}
			}
			waitProcessGone(t, cmd.Process.Pid, "cancelled command", 30*time.Second)
		})
	}
}

// TestWindowsJob_ResumeFailureEndsTheContainedCommand covers a command that was
// assigned but could not be resumed. Track must fail, the command must never
// have run, and the job must already be empty for the seal that follows.
func TestWindowsJob_ResumeFailureEndsTheContainedCommand(t *testing.T) {
	resumeErr := errors.New("controlled resume failure")
	defer func(previous func(int) error) { resumeCommand = previous }(resumeCommand)
	resumeCommand = func(int) error { return resumeErr }

	marker := filepath.Join(t.TempDir(), "ran")
	pg, cmd := startSuspendedForJobTest(t, directHelper(context.Background(), "touch-block", marker))
	if err := pg.Track(cmd); !errors.Is(err, resumeErr) {
		t.Fatalf("Track() = %v, want the resume failure", err)
	}
	if windowsProcessRunning(cmd.Process.Pid) {
		t.Fatalf("command (PID %d) still running after Track reported the resume failure", cmd.Process.Pid)
	}
	observation, err := pg.Seal()
	if err != nil || observation.State != GroupEmpty {
		t.Fatalf("Seal() = %+v, %v; want an empty job", observation, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("command ran although it was never resumed")
	}
}

// TestWindowsJob_TrackRaceWithKillAndClose races containment against signalling
// and release on a suspended command. Whatever the order, the handle is never
// used after release, and the command ends: through the job, through
// kill-on-close, or through the caller's own handle when Track fails.
func TestWindowsJob_TrackRaceWithKillAndClose(t *testing.T) {
	for i := 0; i < 8; i++ {
		pg, cmd := startSuspendedForJobTest(t, directHelper(context.Background(), "sleep", "120s"))
		var wg sync.WaitGroup
		var trackErr error
		wg.Add(3)
		go func() { defer wg.Done(); trackErr = pg.Track(cmd) }()
		go func() { defer wg.Done(); _ = pg.Kill() }()
		go func() { defer wg.Done(); _ = pg.Close() }()
		wg.Wait()
		if trackErr != nil {
			// The lifecycle kills a command whose Track failed through its own
			// handle; do the same here.
			_ = cmd.Process.Kill()
		}
		if got := pg.State(); got != GroupUnavailable {
			t.Fatalf("State() after Close = %v, want %v", got, GroupUnavailable)
		}
		waitProcessGone(t, cmd.Process.Pid, "raced command", 30*time.Second)
	}
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
	createErr := errors.New("job object could not be created")
	pg := &windowsProcessGroup{createErr: createErr}

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
	observation, err := pg.Seal()
	if !errors.Is(err, createErr) {
		t.Fatalf("Seal() error = %v, want job creation error", err)
	}
	if observation.State != GroupUnavailable || observation.Identity != "unknown" {
		t.Fatalf("Seal() observation = %+v, want unavailable state and unknown identity", observation)
	}
}

// TestWindowsJob_TrackRefusesAClosedHandle covers the other containment failure:
// the job was created but is no longer usable. The command is still suspended
// then, so it has run no code, and the caller's own handle ends it.
func TestWindowsJob_TrackRefusesAClosedHandle(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	pg, cmd := startSuspendedForJobTest(t, directHelper(context.Background(), "touch-block", marker))
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
	assertNotExecuted(t, marker, "uncontained command")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill through the owned handle: %v", err)
	}
	waitProcessGone(t, cmd.Process.Pid, "uncontained command", 30*time.Second)
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

func TestWindowsJob_SealPreservesPreCloseObservation(t *testing.T) {
	pg, _ := startGroupForJobTest(t, "ping -n 30 127.0.0.1 >NUL")
	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() before Seal = %v, want owned", got)
	}
	observation, err := pg.Seal()
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if observation.State != GroupOwned {
		t.Fatalf("Seal() observation = %v, want pre-close owned state", observation.State)
	}
	if observation.Identity != "job object" {
		t.Fatalf("Seal() identity = %q, want pre-close job identity", observation.Identity)
	}
	if got := pg.State(); got != GroupUnavailable {
		t.Fatalf("State() after Seal = %v, want unavailable rather than false emptiness", got)
	}
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill after Seal = %v, want os.ErrProcessDone", err)
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

// pidHasExited lets the shared pidIsRunning helper tell an exited process from a
// running one on Windows, where an open handle keeps the process object, and
// therefore OpenProcess, alive after exit.
func pidHasExited(pid int) bool {
	return !windowsProcessRunning(pid)
}
