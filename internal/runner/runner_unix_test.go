//go:build !windows

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestProcessGroup_LeaderExitKeepsSignallingAuthority is the ownership
// regression the whole design turns on.
//
// A command that backgrounds work and then exits leaves a descendant running in
// the same process group, and that descendant is still ours to signal: the
// leader has not been reaped, so it occupies a process-table slot, and because
// its PID is the process-group ID the identifier cannot have been reassigned.
//
// The previous design withdrew authority as soon as the log stream ended and
// then reported these descendants instead of terminating them. Here the group
// stays signalable across the leader's exit and a SIGKILL reaches the descendant.
func TestProcessGroup_LeaderExitKeepsSignallingAuthority(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("Windows terminates through the Job Object; there is no reusable PGID")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	spawn := writeScript(t, dir, "spawn.sh", spawnScript)

	pg, cmd, descendant := startGroupWithSurvivor(t, spawn+" "+stubborn+" "+pidFile, pidFile)
	trackProcessForCleanup(t, descendant, "descendant")

	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() with an unreaped leader and a live descendant = %v, want %v", got, GroupOwned)
	}
	if !pidIsRunning(descendant) {
		t.Fatalf("descendant (PID %d) is not running; the fixture proves nothing", descendant)
	}

	// Observe the leader's exit the way exec does, without reaping it. This is
	// the state in which the old design sealed the group.
	obs, err := observeLeaderExit(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("observeLeaderExit: %v", err)
	}
	if obs == nil {
		t.Skip("this platform does not support exit observation without reaping")
	}
	stop := make(chan struct{})
	defer close(stop)
	if got := obs.Observe(stop); got != exitObserved {
		t.Fatalf("Observe() = %v, want %v", got, exitObserved)
	}

	// The leader is now a zombie and the group is still ours.
	if !signallingEstablished(exitObserved, false) {
		t.Fatal("signallingEstablished() = false after an observed exit, want the group to stay signalable")
	}
	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() after the leader exited = %v, want %v", got, GroupOwned)
	}
	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill() after the leader exited = %v, want the signal to be delivered", err)
	}
	assertTerminated(t, descendant, "same-group descendant of a naturally exited leader", 15*time.Second)

	_ = cmd.Wait()
}

// TestProcessGroup_WithdrawStopsSignalling pins the reap boundary: once
// authority is withdrawn, no signal source may aim at the identifier again.
// That is what keeps a forced shutdown or the stack sweep from racing the reap.
func TestProcessGroup_WithdrawStopsSignalling(t *testing.T) {
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	pg, cmd := startTrackedGroup(t, stay+" "+stubborn+" "+filepath.Join(dir, "d.pid"))

	if err := pg.Terminate(); err != nil {
		t.Fatalf("Terminate() before Withdraw = %v, want the signal to be delivered", err)
	}

	pg.Withdraw()
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill() after Withdraw = %v, want os.ErrProcessDone", err)
	}
	if err := pg.Terminate(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Terminate() after Withdraw = %v, want os.ErrProcessDone", err)
	}

	// Withdraw is one-way, and the reap may follow.
	_ = cmd.Wait()
	pg.Withdraw()
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill() after Withdraw and reap = %v, want os.ErrProcessDone", err)
	}
}

// TestObserveLeaderExit_SetupDoesNotBlockOnARunningCommand pins correction 1:
// setup must return promptly for a long-running service, and must report that
// the leader has not exited yet rather than waiting for it.
func TestObserveLeaderExit_SetupDoesNotBlockOnARunningCommand(t *testing.T) {
	_, cmd := startTrackedGroup(t, helperCmd("sleep", "120s"))

	done := make(chan struct{})
	var obs exitObserver
	var err error
	go func() {
		defer close(done)
		obs, err = observeLeaderExit(cmd.Process.Pid)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("observeLeaderExit blocked on a command that is still running; a healthy service " +
			"would never be able to start")
	}
	if err != nil {
		t.Fatalf("observeLeaderExit: %v", err)
	}
	if obs == nil {
		t.Skip("this platform does not support exit observation without reaping")
	}

	// Setup reported no exit yet. Waiting for it must still not be a deadline:
	// the service is expected to keep running.
	observed := make(chan exitObservation, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() { observed <- obs.Observe(stop) }()

	select {
	case got := <-observed:
		t.Fatalf("Observe() returned %v after 2s while the command was still running", got)
	case <-time.After(2 * time.Second):
	}
}

// TestLiveMembers_IgnoresUnreapedZombie pins correction 2.
//
// cmd.Wait reaps the command leader only. A descendant that outlived it and was
// then killed is reparented and may still be an unreaped zombie for a while, and
// counting it as live would turn a successful shutdown into a reported failure.
func TestLiveMembers_IgnoresUnreapedZombie(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	spawn := writeScript(t, dir, "spawn.sh", spawnScript)

	pg, cmd, descendant := startGroupWithSurvivor(t, spawn+" "+stubborn+" "+pidFile, pidFile)
	trackProcessForCleanup(t, descendant, "descendant")

	if !liveMembers(groupPGID(pg)) {
		t.Fatal("liveMembers() = false while a descendant is running")
	}

	_ = pg.Kill()
	_ = cmd.Wait()

	// The descendant is dead but its new parent may not have reaped it yet, so
	// either it is gone or it is a zombie. Both must read as "not live", or a
	// container whose PID 1 does not collect orphans would fail every shutdown.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if !liveMembers(groupPGID(pg)) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("liveMembers() still reports members after the group was killed "+
				"(descendant PID %d)", descendant)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// uncontainedProcessGroup models a platform where no containment handle could be
// obtained — a Job Object that could not be created or assigned.

// TestObserveLeaderExit_AlreadyExitedChildIsDetectedAtSetup pins the fast-exit
// half of the observer contract.
//
// Setup has to answer two things at once: it must not block on a running command,
// and it must report an exit that has already happened. Those pull in opposite
// directions, so both are asserted here. It also pins where si_pid actually
// lands in Linux's siginfo_t: an offset-12 read would silently report "no exit
// yet" for a child that had already been reaped-able, and the regression would
// pass while the observer blocked forever.
func TestObserveLeaderExit_AlreadyExitedChildIsDetectedAtSetup(t *testing.T) {
	// A command that exits immediately and leaves a same-group descendant
	// running, so the group stays populated and the leader's exit is the only
	// thing that has happened.
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	spawn := writeScript(t, dir, "spawn.sh", spawnScript)
	pidFile := filepath.Join(dir, "d.pid")
	pg, cmd, descendant := startGroupWithSurvivor(t, spawn+" "+stubborn+" "+pidFile, pidFile)
	// The stubborn descendant ignores signals, so the fixture's own cleanup only
	// reaps the leader. Track it explicitly so this test cannot leak it.
	trackProcessForCleanup(t, descendant, "already-exited-leader descendant")

	// Wait for the leader to have exited but not been reaped. Nothing else can
	// reap it: this test owns the only reap, which has not happened yet.
	//
	// Liveness is not the signal here: a zombie still answers a liveness probe,
	// and the shared pidIsRunning helper can only tell a zombie from a live
	// process on Linux, where it reads /proc. So wait on the observation itself.
	var obs exitObserver
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		obs, err = observeLeaderExit(cmd.Process.Pid)
		if err == nil && obs != nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("leader never exited (last observeLeaderExit err=%v)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err != nil {
		t.Fatalf("observeLeaderExit on an already-exited leader: %v", err)
	}
	if obs == nil {
		t.Skip("this platform does not support exit observation without reaping")
	}

	stop := make(chan struct{})
	defer close(stop)
	done := make(chan exitObservation, 1)
	go func() { done <- obs.Observe(stop) }()

	select {
	case got := <-done:
		if got != exitObserved {
			t.Fatalf("Observe() on an already-exited leader = %v, want %v", got, exitObserved)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Observe() blocked on a leader that had already exited; the setup probe did not " +
			"detect the exit, so si_pid is very likely being read from the wrong offset")
	}

	// Observation must not have reaped it: the zombie is still ours, so the
	// identifier is still reserved.
	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() after observing the leader's exit = %v, want %v", got, GroupOwned)
	}
	if !signallingEstablished(exitObserved, false) {
		t.Fatal("signallingEstablished(exitObserved, ...) = false after detecting the exit")
	}
	_ = cmd.Wait()
}

// TestState_GroupExistenceIsDecidedByTheProbe pins how a group's existence is
// judged, using the only signal that behaves the same way on Linux and macOS.
//
// A failed kill(-pgid, 0) is the kernel saying it does not know of such a group,
// and that is the same evidence on both systems. It must not be reinterpreted as a
// permission failure: on macOS the error is not ESRCH, so keying on ESRCH alone
// reported every group as unverifiable and turned every clean shutdown into a
// failure.
func TestState_GroupExistenceIsDecidedByTheProbe(t *testing.T) {
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	descendantPIDFile := filepath.Join(dir, "d.pid")
	pg, _ := startTrackedGroup(t, stay+" "+stubborn+" "+descendantPIDFile)
	// The probe below is made to fail, so cleanup must not depend on the group
	// responding. Track the group directly and let it be torn down by PID.
	if pgid := groupPGID(pg); pgid > 1 {
		groupForCleanup(t, pgid, "probe-controlled group")
	}

	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() with a live group = %v, want %v", got, GroupOwned)
	}

	// A probe failure means the kernel does not know the group. macOS answers
	// with something other than ESRCH, so this must not key on ESRCH.
	restore := groupKill
	groupKill = func(pid int, _ syscall.Signal) error { return syscall.EPERM }
	t.Cleanup(func() { groupKill = restore })

	if got := pg.State(); got != GroupEmpty {
		t.Fatalf("State() when the probe reports no such group = %v, want %v", got, GroupEmpty)
	}
	if err := survivorsError(groupResult("svc", pg, pg.State())); err != nil {
		t.Fatalf("survivorsError() for a group the kernel does not know = %v, want nil", err)
	}
}

// TestSetupFailure_WithdrawsBeforeAnySignal pins the ordering the invariant
// depends on.
//
// When exit observation cannot be established, the leader's identifier can no
// longer be proven to be ours. Sending a group signal at that point is exactly
// what the whole design exists to prevent, so authority has to be withdrawn
// first — before any signal, including a cleanup one.
//
// This is exercised through the group rather than by forcing observeLeaderExit
// to fail, because the ordering lives in the group: once Withdraw returns,
// Terminate and Kill must refuse no matter what the caller asked for first.
func TestSetupFailure_WithdrawsBeforeAnySignal(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("uses a Unix process group")
	}
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	pg, _ := startTrackedGroup(t, stay+" "+stubborn+" "+filepath.Join(dir, "d.pid"))

	pg.Withdraw()

	// Every signal source refuses after withdrawal, in the order a setup failure
	// would otherwise have used them. The checks are immediate assertions rather
	// than waits: the stubborn fixture is still running and deliberately not
	// cleaned up here, because the test is about refusal, not termination.
	if err := pg.Terminate(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Terminate() after Withdraw = %v, want os.ErrProcessDone", err)
	}
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill() after Withdraw = %v, want os.ErrProcessDone", err)
	}
}

// waitForFilePresent blocks until path exists, so a fixture is only used once it
// is genuinely ready.

// groupForCleanup registers a whole process group for teardown at the end of a
// test.
//
// Some tests deliberately withdraw signalling authority or make the probe
// unanswerable, so cleanup cannot go through the group's own methods. Killing the
// numeric group directly is the only route left, and it is confined to test
// cleanup where the group is known to have been created by this test.
//
// It signals and returns rather than waiting for the group to disappear: the
// leader is reaped by the start helper's own cleanup, which cleanups run before,
// so waiting here would deadlock against that reap.
func groupForCleanup(t *testing.T, pgid int, what string) {
	t.Helper()
	t.Cleanup(func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("cleanup: cannot kill %s (pgid %d): %v", what, pgid, err)
		}
	})
}

// TestSingleReap_ResultIsReceivedExactlyOnce pins the fix for a run that hangs
// after a natural completion.
//
// The lifecycle legitimately needs the command's exit status twice: once when a
// natural completion is detected, and once at the end where the reap is sequenced
// after signalling. A single-send channel drains on a single receive, so the
// second read blocks forever. This is what made every naturally completing run on
// a durable-containment platform unable to finish.
func TestSingleReap_ResultIsReceivedExactlyOnce(t *testing.T) {
	var r singleReap
	if r.taken() {
		t.Fatal("a zero singleReap reports its result as already taken")
	}
	if err := r.recv(); err != nil {
		t.Fatalf("recv() on a never-started singleReap = %v, want nil", err)
	}

	want := errors.New("exit status 42")
	r.start(func() error { return want })

	// taken must flip once the result is published, so the caller can skip the
	// second wait rather than block on it.
	deadline := time.Now().Add(10 * time.Second)
	for !r.taken() {
		if !time.Now().Before(deadline) {
			t.Fatal("taken() never became true after the reap completed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Receiving repeatedly must return the same result rather than block, which
	// is what a second receive in the lifecycle used to do.
	for i := 0; i < 3; i++ {
		got := r.recv()
		if got != want {
			t.Fatalf("recv() #%d = %v, want %v", i+1, got, want)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.recv(); err != want {
			t.Errorf("recv() after further waits = %v, want %v", err, want)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recv() blocked on a second read of an already-published result")
	}
}

// TestSingleReap_ConcurrentReceiversDoNotBlock proves the result is safe to share
// across goroutines, which is how the run iteration and the sweep can both observe
// an outcome.
func TestSingleReap_ConcurrentReceiversDoNotBlock(t *testing.T) {
	var r singleReap
	want := errors.New("boom")
	r.start(func() error { return want })

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.recv(); err != want {
				t.Errorf("recv() = %v, want %v", err, want)
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent receivers deadlocked on a single published result")
	}
}

// TestSignallingEstablished_OwnershipLostRevokesAuthority pins that a failed
// observation revokes authority on Unix.
//
// It lives here because the rule is Unix-specific. Where containment is durable the
// handle keeps its authority no matter what the leader's state is, so a failed
// observation revokes nothing and asserting otherwise would be wrong.
func TestSignallingEstablished_OwnershipLostRevokesAuthority(t *testing.T) {
	if durableContainment {
		t.Skip("durable containment keeps its authority regardless of the observation")
	}
	if signallingEstablished(exitOwnershipLost, true) {
		t.Fatal("signallingEstablished(exitOwnershipLost, cancelled) = true; a failed observation " +
			"must revoke authority even when the caller is shutting down")
	}
	if signallingEstablished(exitOwnershipLost, false) {
		t.Fatal("signallingEstablished(exitOwnershipLost, running) = true, want false")
	}

	// A running unreaped leader and an observed exit both keep authority.
	if !signallingEstablished(exitPending, true) {
		t.Fatal("signallingEstablished(exitPending, cancelled) = false; a cancelled command's " +
			"running, unreaped group must still be signalled")
	}
	if !signallingEstablished(exitObserved, false) {
		t.Fatal("signallingEstablished(exitObserved, running) = false; a zombie leader still " +
			"reserves its process-group ID")
	}
}
