//go:build !windows

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"golang.org/x/sys/unix"
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

func TestProcessGroup_SealClosesSignalAuthority(t *testing.T) {
	pg, cmd := startTrackedGroup(t, helperCmd("sleep", "120s"))
	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill before Seal = %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait() = nil after forced termination, want a killed-process result")
	}

	observation, err := pg.Seal()
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if observation.State != GroupEmpty {
		t.Fatalf("Seal() observed %v, want empty after termination", observation.State)
	}
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill after Seal = %v, want os.ErrProcessDone", err)
	}
}

func TestProcessGroup_SealPreservesSurvivorIdentity(t *testing.T) {
	// Replace the shell with the helper so test cleanup owns the group leader
	// itself; sealing withdraws group signaling, so a forked helper would escape
	// leader-only cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec "+helperCmd("sleep", "120s"))
	pg := newProcessGroup()
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	if err := pg.Track(cmd); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		cancel()
		t.Fatalf("track: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill() // direct owned handle; never use the withdrawn PGID
			_, _ = cmd.Process.Wait()
			if pidIsRunning(pid) {
				t.Errorf("seal fixture process %d still running after direct kill and wait", pid)
			}
		}
		cancel()
		_ = pg.Close()
	})
	wantIdentity := strconv.Itoa(cmd.Process.Pid)

	observation, err := pg.Seal()
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if observation.State != GroupOwned {
		t.Fatalf("Seal() state = %v, want an unverified owned group", observation.State)
	}
	if observation.Identity != wantIdentity {
		t.Fatalf("Seal() identity = %q, want preserved group ID %q", observation.Identity, wantIdentity)
	}
	if got := survivorsError(sealedGroupResult("survivor", observation)); got == nil || !strings.Contains(got.Error(), wantIdentity) {
		t.Fatalf("survivor result = %v, want preserved group ID %s", got, wantIdentity)
	}
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill after Seal = %v, want os.ErrProcessDone", err)
	}
}

// pidHasExited refines pidIsRunning where pidIsTerminatedUnreaped cannot see
// a zombie: on macOS there is no /proc, so an exited but unreaped process is
// detected through its kinfo state instead.
func pidHasExited(pid int) bool { return pidIsZombie(pid) }

// TestObserveLeaderExit_SetupDoesNotBlockOnARunningCommand pins correction 1:
// setup must return promptly for a long-running service, and must report that
// the leader has not exited yet rather than waiting for it.
func TestObserveLeaderExit_SetupDoesNotBlockOnARunningCommand(t *testing.T) {
	// Replace the shell with the helper so fixture cleanup owns and reaps the
	// exact process being observed rather than orphaning a shell child.
	_, cmd := startTrackedGroup(t, "exec "+helperCmd("sleep", "120s"))

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

// TestState_AbsenceIsDecidedByMembershipNotTheProbe pins how a group is judged
// gone.
//
// Absence is established by enumerating the group's members and finding nothing
// running. It is not established by kill(-pgid, 0), because that call's error
// does not mean "gone": on macOS it answers EPERM for a group the kernel does not
// know, which is why an ESRCH-only reading marked every macOS group unverifiable
// and an any-error reading would mark them all terminated instead.
//
// The probe is still meaningful, but only in one direction: a success proves
// something is there.
func TestState_AbsenceIsDecidedByMembershipNotTheProbe(t *testing.T) {
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	descendantPIDFile := filepath.Join(dir, "d.pid")
	pg, _ := startTrackedGroup(t, stay+" "+stubborn+" "+descendantPIDFile)

	if got := pg.State(); got != GroupOwned {
		t.Fatalf("State() with a live group = %v, want %v", got, GroupOwned)
	}

	// The probe now failing changes nothing: membership, not the probe, decides.
	for _, probeErr := range []error{syscall.EPERM, syscall.ESRCH, errors.New("probe could not answer")} {
		restore := groupKill
		groupKill = func(pid int, _ syscall.Signal) error { return probeErr }
		got := pg.State()
		groupKill = restore
		if got != GroupOwned {
			t.Fatalf("State() with a live member and probe error %v = %v, want %v; "+
				"a live member must be reported however the probe behaves", probeErr, got, GroupOwned)
		}
	}

	// With the group emptied, every probe outcome agrees it is gone.
	restore := groupKill
	groupKill = func(pid int, _ syscall.Signal) error { return syscall.EPERM }
	t.Cleanup(func() { groupKill = restore })
	if err := pg.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := pg.State()
		if got == GroupEmpty {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("State() = %v, want %v once the group is empty", got, GroupEmpty)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := survivorsError(groupResult("svc", pg, pg.State())); err != nil {
		t.Fatalf("survivorsError() for an empty group = %v, want nil", err)
	}
}

// TestSetupFailure_WithdrawsBeforeAnySignal pins the ordering the invariant
// depends on.
//
// When the wait on the leader fails, the leader's waitable state cannot be
// established and its identifier can no longer be proven to be ours. Sending a
// group signal at that point is exactly what the whole design exists to
// prevent, so authority has to be withdrawn first — before any signal,
// including a cleanup one.
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

// ── observation failure: the managed tree ───────────────────────────────────
//
// A launch whose exit cannot be observed fails. These tests drive that path
// through the observeExit seam and pin what the failure leaves behind, and
// whether the restart policy may launch another copy beside it.

// failedObservation is an observeExit seam that reports the same error for
// every launch.
type failedObservation struct {
	mu          sync.Mutex
	launches    int
	descendants []int
}

// failObservation installs a failedObservation on r. With a pidFile, each
// launch's failure is reported only once its descendant has recorded its PID,
// so the command's group has a member besides its leader; every descendant
// seen is killed by its exact PID at cleanup.
func failObservation(t *testing.T, r *Runner, pidFile string, err error) *failedObservation {
	t.Helper()
	f := &failedObservation{}
	t.Cleanup(func() {
		for _, pid := range f.seen() {
			if proc, findErr := os.FindProcess(pid); findErr == nil {
				_ = proc.Kill()
			}
		}
	})
	r.observeExit = func(int) (exitObserver, error) {
		descendant, ok := 0, false
		if pidFile != "" {
			descendant, ok = waitForPIDFile(t, pidFile, 20*time.Second)
			_ = os.Remove(pidFile)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.launches++
		if ok {
			f.descendants = append(f.descendants, descendant)
		}
		return nil, err
	}
	return f
}

func (f *failedObservation) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches
}

func (f *failedObservation) seen() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.descendants)
}

// TestObservationFailure_UnsupportedPlatformKillsTheGroup is the #48
// regression for a platform that cannot observe an exit without reaping it.
// The launch fails there, and it used to kill only the command itself, so its
// descendants were orphaned. That failure says nothing about the leader, which
// nothing has reaped yet, so its group ID is still the command's own and the
// whole group is killed before authority is withdrawn.
func TestObservationFailure_UnsupportedPlatformKillsTheGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	r := makeRunner(map[string]config.Process{
		// exec makes the script the leader, so its descendant is the only
		// other member of the group.
		"svc": {Cmd: "exec " + stay + " " + stubborn + " " + pidFile, Restart: "never"},
	})
	r.Output = io.Discard
	seam := failObservation(t, r, pidFile, errObservationUnsupported)

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "processes failed: svc") {
		t.Fatalf("Run = %v, want the launch to fail", err)
	}
	if got := snapshotOf(r, "svc"); got.State != "failed" || !strings.Contains(got.LastError, "observe leader exit") {
		t.Fatalf("svc = %+v, want failed with the observation failure", got)
	}
	descendants := seam.seen()
	if len(descendants) != 1 {
		t.Fatalf("descendants = %v, want the one the launch started", descendants)
	}
	assertTerminated(t, descendants[0], "SIGTERM-ignoring descendant of a launch whose exit could not be observed", 10*time.Second)
}

// TestObservationFailure_UnprovenLeaderIsNeitherSignalledNorRestarted pins
// the other failure: a failed wait on the leader itself, such as ECHILD when
// something else collected it. Its identifier may already belong to an
// unrelated group, so no group signal is sent, and the descendant is left
// running and reported rather than risk signalling that group. The restart
// policy used to launch a new copy beside it — one orphaned tree per restart
// — and does not apply now.
func TestObservationFailure_UnprovenLeaderIsNeitherSignalledNorRestarted(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	r := makeRunner(map[string]config.Process{
		// exec makes the script the leader, so once it is killed its
		// descendant is the only member left. MaxRestarts bounds the run if a
		// restart does happen.
		"svc": {Cmd: "exec " + stay + " " + stubborn + " " + pidFile, Restart: "always", MaxRestarts: 1},
	})
	r.Output = io.Discard
	seam := failObservation(t, r, pidFile, syscall.ECHILD)

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "so svc is not restarted") {
		t.Fatalf("Run = %v, want the failure reported with the reason it is not restarted", err)
	}
	if n := seam.count(); n != 1 {
		t.Fatalf("%d launches, want 1: a restart would run beside the tree that is still alive", n)
	}
	if got := snapshotOf(r, "svc"); got.State != "failed" || got.Restarts != 0 {
		t.Fatalf("svc = %+v, want failed without a restart", got)
	}
	descendants := seam.seen()
	if len(descendants) != 1 {
		t.Fatalf("descendants = %v, want the one the launch started", descendants)
	}
	if !pidIsRunning(descendants[0]) {
		t.Fatalf("descendant %d was terminated, want it left alone: its group could not be proven to be the command's", descendants[0])
	}
	if !slices.ContainsFunc(r.Unresolved(), func(u UnresolvedProcess) bool { return u.Name == "svc" }) {
		t.Fatalf("unresolved = %+v, want svc reported", r.Unresolved())
	}
}

// TestObservationFailure_ProvenEmptyGroupKeepsTheRestartPolicy pins that the
// rule is no wider than that: when nothing is left in the group once the
// leader is collected, a restart cannot duplicate anything, so the restart
// policy applies as it does to any failed launch.
func TestObservationFailure_ProvenEmptyGroupKeepsTheRestartPolicy(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"svc": {Cmd: serviceHelperCmd("sleep", "60s"), Restart: "on-failure", MaxRestarts: 1},
	})
	r.Output = io.Discard
	seam := failObservation(t, r, "", syscall.ECHILD)

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "processes failed: svc") {
		t.Fatalf("Run = %v, want the launches to fail", err)
	}
	if n := seam.count(); n != 2 {
		t.Fatalf("%d launches, want 2: the restart policy applies once the group is proven empty", n)
	}
	if got := r.Unresolved(); len(got) != 0 {
		t.Fatalf("unresolved = %+v, want none for a group proven empty", got)
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

// startTrackedGroup starts cmdStr in its own process group and returns the
// concrete group plus the Cmd, so a test can drive the group's own mutex. The
// Cmd's process is registered for cleanup immediately after creation, before any
// assertion, so no path leaks it.
//
// Cleanup kills the whole group, not only the leader. The leader is the shell,
// and a shell that forks the command instead of exec'ing it, as dash does, would
// otherwise leave the command running, reparented to init. The group is killed
// through groupForCleanup, not through pg, because a test may have withdrawn
// pg's signalling authority; registered after the leader's cleanup, it runs
// before the leader is reaped.
func startTrackedGroup(t *testing.T, cmdStr string) (ProcessGroup, *exec.Cmd) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	pg := newProcessGroup()
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	if err := pg.Track(cmd); err != nil {
		cancel()
		t.Fatalf("track: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			// Reap it. A zombie leader keeps its process-group ID allocated,
			// which both leaks the fixture and makes the group probe keep
			// succeeding for a group nothing is running in.
			_, _ = cmd.Process.Wait()
		}
		cancel()
		_ = pg.Close()
	})
	groupForCleanup(t, cmd.Process.Pid, "fixture group")
	return pg, cmd
}

// startGroupWithSurvivor is startTrackedGroup for a command that leaves a
// descendant running: it also returns the descendant's PID, and blocks until the
// descendant has recorded it, so the caller never observes the group mid-fork.
func startGroupWithSurvivor(t *testing.T, cmdStr, pidFile string) (ProcessGroup, *exec.Cmd, int) {
	t.Helper()

	pg, cmd := startTrackedGroup(t, cmdStr)
	descendant, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("descendant never recorded its PID; the fixture proves nothing")
	}
	return pg, cmd, descendant
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

// recordFixture writes a PID file for this test process, as `up` does before
// Run, and returns its path for Runner.RecordPath.
func recordFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pc-test.pid")
	if err := daemon.WritePID(path, os.Getpid(), filepath.Join(dir, "pc-test.sock")); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	return path
}

// recordedGroups returns the process-group record in the PID file; a missing
// record fails the test, because nil means "nothing was recorded at all".
func recordedGroups(t *testing.T, path string) []daemon.Group {
	t.Helper()
	rec, err := daemon.ReadRecord(path)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.Groups == nil {
		t.Fatalf("PID file %s has no process-group record", path)
	}
	return *rec.Groups
}

// leaderStartFor is what the record must hold for a live leader: its procfs
// start time where the platform has one, 0 elsewhere.
func leaderStartFor(pid int) uint64 {
	info, err := daemon.ProcStat(pid)
	if err != nil {
		return 0
	}
	return info.StartTime
}

// TestGroupRecord_PendingBeforeStartThenTrackedThenDropped follows one launch
// through the record: a pending entry exists before the command could have been
// tracked, the tracked entry names the leader and its start time, and a group
// proven empty at shutdown leaves the record.
func TestGroupRecord_PendingBeforeStartThenTrackedThenDropped(t *testing.T) {
	recordPath := recordFixture(t)
	r := makeRunner(map[string]config.Process{"api": {
		Cmd: serviceHelperCmd("sleep", "120s"), Restart: "never", ShutdownTimeout: shutdownGraceForTest,
	}})
	r.RecordPath = recordPath
	group := &admissionGateProcessGroup{
		ProcessGroup:     newProcessGroup(),
		trackStarted:     make(chan struct{}),
		trackRelease:     make(chan struct{}),
		terminateStarted: make(chan struct{}),
		terminateRelease: make(chan struct{}),
		closed:           make(chan struct{}),
	}
	close(group.terminateRelease)
	r.processGroupFactory = func() ProcessGroup { return group }

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = r.Run(ctx); close(runDone) }()
	release := sync.OnceFunc(func() { close(group.trackRelease) })
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	})

	select {
	case <-group.trackStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("launch never reached Track")
	}
	pid := group.pid
	trackProcessForCleanup(t, pid, "api")
	if got, want := recordedGroups(t, recordPath), []daemon.Group{{Name: "api"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("record after Start, before tracking = %+v, want pending %+v", got, want)
	}

	release()
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.State == "running" }, 10*time.Second) {
		t.Fatal("api never reached running")
	}
	want := []daemon.Group{{Name: "api", PGID: pid, LeaderStart: leaderStartFor(pid)}}
	if got := recordedGroups(t, recordPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("record while running = %+v, want %+v", got, want)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}
	if got := recordedGroups(t, recordPath); len(got) != 0 {
		t.Fatalf("record after a verified stop = %+v, want empty", got)
	}
	assertTerminated(t, pid, "api", 15*time.Second)
}

// TestGroupRecord_ProbeGroupRecorded pins that a quiet ready_when.cmd probe
// is still a managed process for crash recovery: while an attempt runs, its
// "<name>:probe" group is in the PID-file record with its leader, so a stop
// after a daemon crash can find it, and it leaves the record once proven
// empty at shutdown.
func TestGroupRecord_ProbeGroupRecorded(t *testing.T) {
	recordPath := recordFixture(t)
	pidFile := filepath.Join(t.TempDir(), "probe.pid")
	r := makeRunner(map[string]config.Process{"worker": {
		Cmd:             serviceHelperCmd("sleep", "120s"),
		ReadyTimeout:    -1,
		ReadyWhen:       &config.ReadyWhen{Cmd: serviceHelperCmd("pid-block", pidFile)},
		ShutdownTimeout: shutdownGraceForTest,
	}})
	r.RecordPath = recordPath
	r.Output = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		cancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("runner did not stop")
		}
	})

	pid, ok := waitForPIDFile(t, pidFile, 10*time.Second)
	if !ok {
		t.Fatal("probe never started")
	}
	trackProcessForCleanup(t, pid, "probe")
	want := daemon.Group{Name: "worker:probe", PGID: pid, LeaderStart: leaderStartFor(pid)}
	var got []daemon.Group
	found := false
	for deadline := time.Now().Add(5 * time.Second); !found && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		got = recordedGroups(t, recordPath)
		for _, g := range got {
			if g == want {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("record while the probe runs = %+v, want it to include %+v", got, want)
	}

	cancel()
	stopped = true
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}
	if got := recordedGroups(t, recordPath); len(got) != 0 {
		t.Fatalf("record after a verified stop = %+v, want empty", got)
	}
	assertTerminated(t, pid, "probe", 15*time.Second)
}

// stuckOwnedProcessGroup models a group whose member survives every signal:
// the real group is used for setup, tracking and signals, but it always reports
// a live member.
type stuckOwnedProcessGroup struct{ ProcessGroup }

func (g *stuckOwnedProcessGroup) State() GroupState { return GroupOwned }
func (g *stuckOwnedProcessGroup) Seal() (SealObservation, error) {
	observation, err := g.ProcessGroup.Seal()
	observation.State = GroupOwned
	return observation, err
}

// TestGroupRecord_SurvivorStaysRecorded: a group that could not be proven empty
// keeps its entry after the run, so a later stop still knows about it.
func TestGroupRecord_SurvivorStaysRecorded(t *testing.T) {
	recordPath := recordFixture(t)
	r := makeRunner(map[string]config.Process{"wedged": {
		Cmd: serviceHelperCmd("exit", "0"), Restart: "never", ShutdownTimeout: shutdownGraceForTest,
	}})
	r.RecordPath = recordPath
	r.processGroupFactory = func() ProcessGroup { return &stuckOwnedProcessGroup{ProcessGroup: newProcessGroup()} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		cancel()
		<-runDone
		t.Fatal("run with a survivor did not finish")
	}
	pid := snapshotOf(r, "wedged").PID
	if pid <= 0 {
		t.Fatal("wedged never started")
	}
	got := recordedGroups(t, recordPath)
	if len(got) != 1 || got[0].Name != "wedged" || got[0].PGID != pid {
		t.Fatalf("record after an unverified teardown = %+v, want the wedged group (pgid %d) kept", got, pid)
	}
}

// TestGroupRecord_PopulatedPreviousIncarnationKept: a restart drops the
// previous incarnation's entry only when that group is proven empty; a
// populated one stays, so two entries can share a name.
func TestGroupRecord_PopulatedPreviousIncarnationKept(t *testing.T) {
	recordPath := recordFixture(t)
	r := &Runner{RecordPath: recordPath}
	r.beginRecord()

	launch := func(pg ProcessGroup, pid int) {
		t.Helper()
		entry, err := r.recordPending("web", pg)
		if err != nil {
			t.Fatalf("recordPending: %v", err)
		}
		r.recordTracked(entry, pid)
		r.trackGroup("web", pg, 0)
	}
	populated := &unstoppableProcessGroup{}
	launch(populated, 41234)
	launch(newProcessGroup(), 41235)
	got := recordedGroups(t, recordPath)
	if len(got) != 2 || got[0].PGID != 41234 || got[1].PGID != 41235 || got[0].Name != "web" || got[1].Name != "web" {
		t.Fatalf("record after restarting over a populated group = %+v, want both web entries", got)
	}

	// The second incarnation's group is empty (never established), so the next
	// restart drops it and keeps the populated one.
	launch(newProcessGroup(), 41236)
	got = recordedGroups(t, recordPath)
	if len(got) != 2 || got[0].PGID != 41234 || got[1].PGID != 41236 {
		t.Fatalf("record after restarting over an empty group = %+v, want pgids 41234 and 41236", got)
	}
}

// TestGroupRecord_PendingWriteFailureRefusesLaunch: when the pending entry
// cannot be persisted, the command is never started.
func TestGroupRecord_PendingWriteFailureRefusesLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	recordPath := recordFixture(t)
	recordDir := filepath.Dir(recordPath)
	fixtures := t.TempDir()
	stubborn := writeScript(t, fixtures, "stubborn.sh", stubbornScript)
	commandPID := filepath.Join(fixtures, "command.pid")
	t.Cleanup(func() { _ = os.Chmod(recordDir, 0o700) })

	r := makeRunner(map[string]config.Process{"api": {Cmd: stubborn + " " + commandPID, Restart: "never"}})
	r.RecordPath = recordPath
	r.processGroupFactory = func() ProcessGroup {
		// The run has already written its empty record (W1); the directory
		// becomes unwritable before this launch's pending entry.
		if err := os.Chmod(recordDir, 0o500); err != nil {
			t.Errorf("chmod: %v", err)
		}
		return newProcessGroup()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- r.Run(ctx) }()
	var err error
	select {
	case err = <-runErr:
	case <-time.After(15 * time.Second):
	}
	// Checked first so a launch that wrongly happened is always cleaned up.
	if pid, ok := waitForPIDFile(t, commandPID, 200*time.Millisecond); ok {
		trackProcessForCleanup(t, pid, "unrecorded command")
		t.Fatalf("command started (PID %d) although its record could not be written", pid)
	}
	if err == nil {
		t.Fatal("Run did not fail although the launch could not be recorded")
	}
	state := snapshotOf(r, "api")
	if state.State != "failed" || !strings.Contains(state.LastError, "record") {
		t.Fatalf("api = %+v, want failed with a record error", state)
	}
	if got := recordedGroups(t, recordPath); len(got) != 0 {
		t.Fatalf("record = %+v, want the run's empty record unchanged", got)
	}
}

// TestGroupRecord_UnrecordableHealthCheckIsNotAFailure: a health_when.cmd
// check that the supervisor refuses to launch, because its pending entry
// cannot be persisted, is not a health verdict. With health_action: restart
// it must not mark the service unhealthy and kill it through the restart
// policy; the refusal is logged once as a skipped check instead, and the
// verdict reads unknown until checks run again.
func TestGroupRecord_UnrecordableHealthCheckIsNotAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	recordPath := recordFixture(t)
	recordDir := filepath.Dir(recordPath)
	t.Cleanup(func() { _ = os.Chmod(recordDir, 0o700) })
	r := makeRunner(map[string]config.Process{"api": {
		Cmd:             serviceHelperCmd("sleep", "120s"),
		HealthWhen:      &config.ReadyWhen{Cmd: "exit 0"},
		HealthInterval:  1,
		HealthAction:    config.HealthActionRestart,
		Restart:         "on-failure",
		MaxRestarts:     1,
		ShutdownTimeout: shutdownGraceForTest,
	}})
	r.RecordPath = recordPath
	var journal bytes.Buffer
	r.Output, r.Journal = io.Discard, &journal
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-runDone:
			case <-time.After(15 * time.Second):
				t.Error("runner did not stop")
			}
		})
	}
	t.Cleanup(stop)

	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Health == "healthy" && s.PID > 0 }, 10*time.Second) {
		t.Fatalf("api never became healthy: %+v", snapshotOf(r, "api"))
	}
	before := snapshotOf(r, "api")
	trackProcessForCleanup(t, before.PID, "api")
	if err := os.Chmod(recordDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Well past healthThreshold refused checks at a 1s interval.
	time.Sleep(time.Duration(healthThreshold+2) * time.Second)
	after := snapshotOf(r, "api")
	if err := os.Chmod(recordDir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// No check backs the earlier verdict any more, so it is unknown, with the
	// launch error as health_error; the incarnation itself is untouched.
	if after.State != "running" || after.PID != before.PID || after.Restarts != 0 || after.Health != "unknown" || !strings.Contains(after.HealthError, "health check not run") {
		t.Fatalf("api after refused health checks = %+v, want the same incarnation with an unknown verdict (was %+v)", after, before)
	}
	// The verdict comes back once checks run again.
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Health == "healthy" && s.HealthError == "" }, 10*time.Second) {
		t.Fatalf("api never became healthy again: %+v", snapshotOf(r, "api"))
	}
	stop()

	skipped := 0
	for _, event := range journalEvents(t, &journal) {
		switch event.Code {
		case "health.check_failed", "health.unhealthy":
			t.Fatalf("a refused health check was counted as a failure: %+v", event)
		case "health.check_skipped":
			skipped++
			if event.Level != "warn" || !strings.Contains(event.Line, "record process group") {
				t.Fatalf("health.check_skipped = %+v, want a warn naming the record error", event)
			}
		}
	}
	if skipped != 1 {
		t.Fatalf("health.check_skipped events = %d, want one for the repeated refusal", skipped)
	}
}

// TestGroupRecord_UnrecordableRunStillLaunches: when the run's initial record
// cannot be written (no PID file), recording is disabled for the run and
// launches proceed; nothing is created in its place.
func TestGroupRecord_UnrecordableRunStillLaunches(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "missing.pid")
	r := makeRunner(map[string]config.Process{"api": {
		Cmd: serviceHelperCmd("sleep", "120s"), Restart: "never", ShutdownTimeout: shutdownGraceForTest,
	}})
	r.RecordPath = recordPath
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	})
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.State == "running" && s.PID > 0 }, 10*time.Second) {
		t.Fatalf("api did not launch with recording disabled: %+v", snapshotOf(r, "api"))
	}
	trackProcessForCleanup(t, snapshotOf(r, "api").PID, "api")
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("a disabled record created %s: %v", recordPath, err)
	}
}

// TestGroupRecord_PIDFileRemovedMidRunStillLaunches: a PID file that
// disappears after the run's initial record (a removed runtime or temp
// directory) disables recording instead of refusing every later launch, and
// nothing is recreated in its place. A PID file that another daemon has
// recreated in the meantime (after `$XDG_RUNTIME_DIR` was removed at logout)
// disables recording the same way and is left exactly as that daemon wrote it.
func TestGroupRecord_PIDFileRemovedMidRunStillLaunches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replaced bool
	}{
		{name: "removed"},
		{name: "replaced by another daemon", replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordPath := recordFixture(t)
			otherPID := os.Getppid() // a live pid that is not this process
			r := makeRunner(map[string]config.Process{"api": {
				Cmd: serviceHelperCmd("sleep", "120s"), Restart: "never", ShutdownTimeout: shutdownGraceForTest,
			}})
			r.RecordPath = recordPath
			var mutate sync.Once
			r.processGroupFactory = func() ProcessGroup {
				// The run has already written its empty record (W1); the PID
				// file is lost before this launch's pending entry.
				mutate.Do(func() {
					if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
						t.Errorf("remove PID file: %v", err)
					}
					if tc.replaced {
						if err := daemon.WritePID(recordPath, otherPID, recordPath+".other.sock"); err != nil {
							t.Errorf("recreate PID file: %v", err)
						}
					}
				})
				return newProcessGroup()
			}
			ctx, cancel := context.WithCancel(context.Background())
			runDone := make(chan error, 1)
			go func() { runDone <- r.Run(ctx) }()
			var stopOnce sync.Once
			stop := func() {
				stopOnce.Do(func() {
					cancel()
					select {
					case <-runDone:
					case <-time.After(15 * time.Second):
						t.Error("runner did not stop")
					}
				})
			}
			t.Cleanup(stop)
			if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.State == "running" && s.PID > 0 }, 10*time.Second) {
				t.Fatalf("api launch was refused after the PID file was lost: %+v", snapshotOf(r, "api"))
			}
			trackProcessForCleanup(t, snapshotOf(r, "api").PID, "api")
			r.recordMu.Lock()
			on := r.recordOn
			r.recordMu.Unlock()
			if on {
				t.Fatal("recording still enabled after the PID file was lost")
			}
			if !tc.replaced {
				if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
					t.Fatalf("a lost record recreated %s: %v", recordPath, err)
				}
				return
			}
			rec, err := daemon.ReadRecord(recordPath)
			if err != nil {
				t.Fatalf("ReadRecord: %v", err)
			}
			if rec.PID != otherPID || rec.Groups != nil {
				t.Fatalf("other daemon's PID file = %+v, want pid %d and no groups written over it", rec, otherPID)
			}
			other, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			// Shutdown drops the api entry (W7/W9); that write must not land either.
			stop()
			after, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(other) {
				t.Fatalf("shutdown rewrote the other daemon's PID file:\nbefore %s\nafter  %s", other, after)
			}
		})
	}
}

// ownedAfterFailedTrack fails Track and then, like a group with a member that
// outlived the leader's kill, reports itself still owned when sealed.
type ownedAfterFailedTrack struct{ *failedTrackProcessGroup }

func (g ownedAfterFailedTrack) Seal() (SealObservation, error) {
	observation, err := g.failedTrackProcessGroup.Seal()
	observation.State = GroupOwned
	return observation, err
}

// TestGroupRecord_FailedTrackDropsOnlyProvenEmpty (W4): when Track fails after
// Start, the pending entry leaves the record only if Seal proves the group
// empty; an owned group keeps it.
func TestGroupRecord_FailedTrackDropsOnlyProvenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owned bool
		want  []daemon.Group
	}{
		{name: "empty group dropped", owned: false, want: []daemon.Group{}},
		{name: "owned group kept", owned: true, want: []daemon.Group{{Name: "api"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordPath := recordFixture(t)
			r := makeRunner(map[string]config.Process{"api": {Cmd: serviceHelperCmd("sleep", "120s"), Restart: "never"}})
			r.RecordPath = recordPath
			group := &failedTrackProcessGroup{ProcessGroup: newProcessGroup(), sealed: make(chan struct{})}
			if tc.owned {
				r.processGroupFactory = func() ProcessGroup { return ownedAfterFailedTrack{group} }
			} else {
				r.processGroupFactory = func() ProcessGroup { return group }
			}
			runDone := make(chan error, 1)
			go func() { runDone <- r.Run(context.Background()) }()
			var runErr error
			select {
			case runErr = <-runDone:
			case <-time.After(15 * time.Second):
				if group.cmd != nil && group.cmd.Process != nil {
					_ = group.cmd.Process.Kill()
				}
				<-runDone
				t.Fatal("Run did not finish after Track failure")
			}
			if group.cmd == nil || group.cmd.Process == nil {
				t.Fatal("Track failure did not observe the started process")
			}
			// exec kills and reaps the started process before Run returns.
			if pidIsRunning(group.cmd.Process.Pid) {
				_ = group.cmd.Process.Kill()
				t.Fatalf("failed-Track process %d survived Run", group.cmd.Process.Pid)
			}
			if runErr == nil {
				t.Fatal("Run succeeded although Track failed")
			}
			if got := recordedGroups(t, recordPath); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("record after failed Track = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGroupRecord_SweepKeepsUnprovenGroups (W9): the shutdown sweep drops the
// entry of a group it proves empty and keeps the entry of one it cannot.
func TestGroupRecord_SweepKeepsUnprovenGroups(t *testing.T) {
	recordPath := recordFixture(t)
	r := &Runner{RecordPath: recordPath}
	r.beginRecord()
	launch := func(name string, pg ProcessGroup, pid int) {
		t.Helper()
		entry, err := r.recordPending(name, pg)
		if err != nil {
			t.Fatalf("recordPending: %v", err)
		}
		r.recordTracked(entry, pid)
		r.trackGroup(name, pg, 0)
	}
	launch("wedged", &unstoppableProcessGroup{}, 41234)
	launch("done", newProcessGroup(), 41235)
	if got := recordedGroups(t, recordPath); len(got) != 2 {
		t.Fatalf("record before the sweep = %+v, want both groups", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.sweepGroups(ctx)
	got := recordedGroups(t, recordPath)
	if len(got) != 1 || got[0].Name != "wedged" || got[0].PGID != 41234 {
		t.Fatalf("record after the sweep = %+v, want only the unproven wedged group", got)
	}
}

// ── crash recovery ───────────────────────────────────────────────────────────

// requireLinuxRecovery skips tests of signalling recovery, which only Linux
// performs: it needs procfs start times and pidfds.
func requireLinuxRecovery(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("crash recovery signals only on Linux; elsewhere it reports empty or unknown")
	}
}

// startRecoveryGroup starts argv as the leader of its own process group, the
// way the runner launches a command, and returns it with the group record a
// crashed daemon would have left: the leader's pid as pgid and its start time.
// Cleanup kills that exact leader and reaps it; members are the caller's to
// register.
func startRecoveryGroup(t *testing.T, name string, argv ...string) (*exec.Cmd, daemon.Group) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd, daemon.Group{Name: name, PGID: cmd.Process.Pid, LeaderStart: leaderStartFor(cmd.Process.Pid)}
}

// recordSignals replaces the pidfd signal seam with one that delivers only the
// null signal and records every other signal instead of sending it, so a test
// can prove a refusal without risking a real signal reaching the wrong process.
func recordSignals(t *testing.T) *[]syscall.Signal {
	t.Helper()
	var (
		mu   sync.Mutex
		sent []syscall.Signal
	)
	restore := pidfdSend
	pidfdSend = func(fd int, sig syscall.Signal) error {
		if sig == 0 {
			return restore(fd, sig)
		}
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, sig)
		return nil
	}
	t.Cleanup(func() { pidfdSend = restore })
	return &sent
}

// recordProbes wraps the group existence probe and records every pid it is
// asked about, so a test can prove no probe ever named pid 0 or -1 (this
// process's group and every process it may signal).
func recordProbes(t *testing.T) *[]int {
	t.Helper()
	var (
		mu   sync.Mutex
		pids []int
	)
	restore := groupKill
	groupKill = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		pids = append(pids, pid)
		mu.Unlock()
		return restore(pid, sig)
	}
	t.Cleanup(func() { groupKill = restore })
	return &pids
}

func assertSingleRecovery(t *testing.T, got []RecoveredGroup, outcome, reason string) RecoveredGroup {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("RecoverGroups returned %d results, want 1: %+v", len(got), got)
	}
	if got[0].Outcome != outcome || got[0].Reason != reason {
		t.Fatalf("RecoverGroups = outcome %q reason %q, want %q %q (%+v)",
			got[0].Outcome, got[0].Reason, outcome, reason, got[0])
	}
	return got[0]
}

func assertStillRunning(t *testing.T, pid int, what string) {
	t.Helper()
	if !pidIsRunning(pid) {
		t.Fatalf("%s (PID %d) is no longer running; recovery signalled a process it had not proven", what, pid)
	}
}

// TestRecoverGroups_TerminatesProvenGroup is the S3 headline: a group whose
// leader is alive with the recorded start time is proven ours, and a graceful
// recovery ends every member with SIGTERM alone.
func TestRecoverGroups_TerminatesProvenGroup(t *testing.T) {
	requireLinuxRecovery(t)
	memberFile := filepath.Join(t.TempDir(), "member.pid")
	_, g := startRecoveryGroup(t, "web", "sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", memberFile)
	member, ok := waitForPIDFile(t, memberFile, 10*time.Second)
	if !ok {
		t.Fatal("member never recorded its PID")
	}
	trackProcessForCleanup(t, member, "member")

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverGraceful, time.Now().Add(10*time.Second))
	r := assertSingleRecovery(t, got, OutcomeTerminated, "leader_proven")
	if !reflect.DeepEqual(r.Signals, []string{"SIGTERM"}) {
		t.Fatalf("Signals = %v, want [SIGTERM]", r.Signals)
	}
	if r.Name != "web" || r.PGID != g.PGID || r.LeaderStart != g.LeaderStart {
		t.Fatalf("result identity = %+v, want the record %+v", r, g)
	}
	if !containsPID(r.PIDs, g.PGID) || !containsPID(r.PIDs, member) {
		t.Fatalf("PIDs = %v, want leader %d and member %d", r.PIDs, g.PGID, member)
	}
	assertTerminated(t, member, "proven member", 5*time.Second)
	assertTerminated(t, g.PGID, "proven leader", 5*time.Second)
}

// TestRecoverGroups_ForceKillsWithoutTERM pins the forced mode: proven members
// get SIGKILL at once, so even a SIGTERM-ignoring group ends without a wait.
func TestRecoverGroups_ForceKillsWithoutTERM(t *testing.T) {
	requireLinuxRecovery(t)
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	leaderFile := filepath.Join(dir, "leader.pid")
	_, g := startRecoveryGroup(t, "stubborn", "sh", stubborn, leaderFile)
	if _, ok := waitForPIDFile(t, leaderFile, 10*time.Second); !ok {
		t.Fatal("stubborn leader never recorded its PID")
	}

	start := time.Now()
	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(30*time.Second))
	r := assertSingleRecovery(t, got, OutcomeTerminated, "leader_proven")
	if !reflect.DeepEqual(r.Signals, []string{"SIGKILL"}) {
		t.Fatalf("Signals = %v, want [SIGKILL]", r.Signals)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("forced recovery took %s; it waited out the graceful deadline", elapsed)
	}
}

// TestRecoverGroups_ReusedPIDIsEmptyNeverSignalled: a live process at the
// recorded pgid whose start time differs is a new owner of the number. The
// kernel only reallocates a pid nothing uses as a pgid, so the recorded group
// is proven empty, and the new owner is never signalled.
func TestRecoverGroups_ReusedPIDIsEmptyNeverSignalled(t *testing.T) {
	requireLinuxRecovery(t)
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	g.LeaderStart++
	sent := recordSignals(t)

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	r := assertSingleRecovery(t, got, OutcomeEmpty, "pgid_reused")
	if len(r.Signals) != 0 || len(*sent) != 0 {
		t.Fatalf("signals %v / sent %v for a reused pgid, want none", r.Signals, *sent)
	}
	assertStillRunning(t, g.PGID, "new owner of the pid")
}

// TestRecoverGroups_BootMismatchIsEmptyWithoutSignal: a record from another
// boot names processes that cannot exist any more.
func TestRecoverGroups_BootMismatchIsEmptyWithoutSignal(t *testing.T) {
	requireLinuxRecovery(t)
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	host := daemon.HostIdentity()
	if host.BootID == "" {
		t.Skip("boot id unreadable")
	}
	host.BootID = "00000000-0000-0000-0000-000000000000"
	probes := recordProbes(t)

	got := RecoverGroups([]daemon.Group{g}, host, RecoverForce, time.Now().Add(5*time.Second))
	r := assertSingleRecovery(t, got, OutcomeEmpty, "rebooted")
	if len(r.Signals) != 0 || len(*probes) != 0 {
		t.Fatalf("signals %v / probes %v after a reboot, want nothing enumerated or signalled", r.Signals, *probes)
	}
	assertStillRunning(t, g.PGID, "process from this boot")
}

// TestRecoverGroups_BootUnknownIsUnknown: on Linux a record whose boot id is
// missing, or a current boot id that cannot be read, cannot be told apart from
// a record of a previous boot. The namespace check does not catch a reboot
// (the initial namespaces keep their inode numbers), so every group is unknown
// and nothing is enumerated or signalled.
func TestRecoverGroups_BootUnknownIsUnknown(t *testing.T) {
	requireLinuxRecovery(t)
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	for name, tc := range map[string]struct{ recorded, current string }{
		"recorded": {recorded: "", current: "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
		"current":  {recorded: "3f2504e0-4f89-11d3-9a0c-0305e82c3301", current: ""},
	} {
		t.Run(name, func(t *testing.T) {
			restore := currentHost
			currentHost = func() daemon.Host {
				h := restore()
				h.BootID = tc.current
				return h
			}
			t.Cleanup(func() { currentHost = restore })
			host := daemon.HostIdentity()
			host.BootID = tc.recorded
			sent := recordSignals(t)
			probes := recordProbes(t)

			got := RecoverGroups([]daemon.Group{g}, host, RecoverForce, time.Now().Add(5*time.Second))
			r := assertSingleRecovery(t, got, OutcomeUnknown, "boot_unknown")
			if len(r.Signals) != 0 || len(*sent) != 0 || len(*probes) != 0 {
				t.Fatalf("signals %v / sent %v / probes %v without both boot ids, want nothing enumerated or signalled", r.Signals, *sent, *probes)
			}
			assertStillRunning(t, g.PGID, "leader of a record with an unknown boot")
		})
	}
}

// TestRecoverGroups_NamespaceMismatchIsUnknown: pids and start times recorded
// in another pid or time namespace mean nothing here.
func TestRecoverGroups_NamespaceMismatchIsUnknown(t *testing.T) {
	requireLinuxRecovery(t)
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	for name, mutate := range map[string]func(*daemon.Host){
		"pid":  func(h *daemon.Host) { h.PIDNS = "pid:[1]" },
		"time": func(h *daemon.Host) { h.TimeNS = "time:[1]" },
	} {
		host := daemon.HostIdentity()
		mutate(&host)
		got := RecoverGroups([]daemon.Group{g}, host, RecoverForce, time.Now().Add(5*time.Second))
		r := assertSingleRecovery(t, got, OutcomeUnknown, "namespace_mismatch")
		if len(r.Signals) != 0 {
			t.Fatalf("%s namespace mismatch sent %v", name, r.Signals)
		}
		assertStillRunning(t, g.PGID, name+" namespace mismatch leader")
	}
}

// TestRecoverGroups_ForeignProcfsIsUnknown: under `unshare --pid` without a
// fresh /proc mount the recorded namespaces match, but /proc numbers the host's
// processes while a pidfd opens the pid in ours. A proof read from that /proc
// is about a different process than the one a signal would reach, so every
// group is unknown and nothing is enumerated or signalled.
func TestRecoverGroups_ForeignProcfsIsUnknown(t *testing.T) {
	requireLinuxRecovery(t)
	if !procfsIsOwn() {
		t.Fatal("procfsIsOwn() = false in the test's own namespace; the guard would refuse every recovery")
	}
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	restore := procOwn
	procOwn = func() bool { return false }
	t.Cleanup(func() { procOwn = restore })
	sent := recordSignals(t)
	probes := recordProbes(t)

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	r := assertSingleRecovery(t, got, OutcomeUnknown, "namespace_mismatch")
	if len(r.Signals) != 0 || len(*sent) != 0 || len(*probes) != 0 {
		t.Fatalf("signals %v / sent %v / probes %v with a foreign /proc, want nothing enumerated or signalled", r.Signals, *sent, *probes)
	}
	assertStillRunning(t, g.PGID, "leader seen through a foreign /proc")
}

// TestRecoverGroups_LeaderGoneMembersAliveIsUnknown is the roleDetached shape
// after a crash: the leader was reaped, so nothing pins the pgid to our group
// and its members cannot be proven ours. They are reported, never signalled.
func TestRecoverGroups_LeaderGoneMembersAliveIsUnknown(t *testing.T) {
	requireLinuxRecovery(t)
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	spawn := writeScript(t, dir, "spawn.sh", spawnScript)
	descFile := filepath.Join(dir, "descendant.pid")
	cmd := exec.Command("sh", spawn, stubborn, descFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := daemon.Group{Name: "detached-child", PGID: cmd.Process.Pid, LeaderStart: leaderStartFor(cmd.Process.Pid)}
	descendant, ok := waitForPIDFile(t, descFile, 20*time.Second)
	trackProcessForCleanup(t, descendant, "descendant")
	if !ok {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("descendant never recorded its PID")
	}
	_ = cmd.Wait() // reap the leader, as init does once the daemon is gone
	sent := recordSignals(t)

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	r := assertSingleRecovery(t, got, OutcomeUnknown, "leader_gone")
	if len(r.Signals) != 0 || len(*sent) != 0 {
		t.Fatalf("signals %v / sent %v with the leader gone, want none", r.Signals, *sent)
	}
	if r.LiveMembers < 1 || !containsPID(r.PIDs, descendant) {
		t.Fatalf("result %+v does not report the live descendant %d", r, descendant)
	}
	assertStillRunning(t, descendant, "unproven descendant")
}

// TestRecoverGroups_WrapperLeaderExitsOnTERMChildStillKilled: a wrapper leader
// exits on SIGTERM and is reaped while its SIGTERM-ignoring child keeps
// running. The pidfd taken before SIGTERM still pins that child, so the KILL
// phase ends it although the leader can no longer be proven.
//
// The child is one long-lived process that already ignores SIGTERM when it
// records its PID and never forks, so the group at classification is exactly
// the wrapper and the child; nothing has to exit on its own within killSettle
// for the group to be proven empty.
func TestRecoverGroups_WrapperLeaderExitsOnTERMChildStillKilled(t *testing.T) {
	requireLinuxRecovery(t)
	dir := t.TempDir()
	child := writeScript(t, dir, "stubborn.sh", "#!/bin/sh\ntrap '' TERM INT HUP\necho $$ > \"$1\"\nexec sleep 90\n")
	childFile := filepath.Join(dir, "child.pid")
	// startRecoveryGroup reaps the leader as soon as it exits, as init does.
	_, g := startRecoveryGroup(t, "wrapper", "sh", "-c", `sh "$1" "$2" & wait`, "sh", child, childFile)
	childPID, ok := waitForPIDFile(t, childFile, 10*time.Second)
	if !ok {
		t.Fatal("child never recorded its PID")
	}
	trackProcessForCleanup(t, childPID, "TERM-ignoring child")
	waitForComm(t, childPID, "sleep")

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverGraceful, time.Now().Add(2*time.Second))
	r := assertSingleRecovery(t, got, OutcomeTerminated, "leader_proven")
	if !reflect.DeepEqual(r.Signals, []string{"SIGTERM", "SIGKILL"}) {
		t.Fatalf("Signals = %v, want [SIGTERM SIGKILL]", r.Signals)
	}
	if !containsPID(r.PIDs, childPID) {
		t.Fatalf("PIDs = %v, want the pinned child %d", r.PIDs, childPID)
	}
	assertTerminated(t, childPID, "TERM-ignoring child of an exited wrapper", 5*time.Second)
}

// waitForComm waits until pid runs the program named comm, so a fixture that
// execs into its final program has finished forking and setting up.
func waitForComm(t *testing.T, pid int, comm string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if err == nil && strings.TrimSpace(string(data)) == comm {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("PID %d never became %q (last read %q, %v)", pid, comm, data, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRecoverGroups_RepinKillsMemberForkedAfterTERM: while the leader is
// still proven after SIGTERM, a member it forks after the first scan is pinned
// before SIGKILL and ends with the group. The leader ignores SIGTERM and forks
// that member in its TERM trap, so the member did not exist at classification.
func TestRecoverGroups_RepinKillsMemberForkedAfterTERM(t *testing.T) {
	requireLinuxRecovery(t)
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready.pid")
	freshFile := filepath.Join(dir, "fresh.pid")
	leader := writeScript(t, dir, "spawner.sh", `#!/bin/sh
trap 'sleep 90 & echo $! > "$2"' TERM
sleep 90 &
echo $$ > "$1"
while :; do wait; done
`)
	_, g := startRecoveryGroup(t, "spawner", "sh", leader, readyFile, freshFile)
	if _, ok := waitForPIDFile(t, readyFile, 10*time.Second); !ok {
		t.Fatal("leader never became ready")
	}
	if _, err := os.Stat(freshFile); err == nil {
		t.Fatal("the late member exists before SIGTERM; the fixture cannot test repin")
	}

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverGraceful, time.Now().Add(time.Second))
	fresh, ok := waitForPIDFile(t, freshFile, time.Second)
	if !ok {
		t.Fatalf("leader never forked the late member on SIGTERM; results %+v", got)
	}
	trackProcessForCleanup(t, fresh, "member forked after SIGTERM")
	r := assertSingleRecovery(t, got, OutcomeTerminated, "leader_proven")
	if !reflect.DeepEqual(r.Signals, []string{"SIGTERM", "SIGKILL"}) {
		t.Fatalf("Signals = %v, want [SIGTERM SIGKILL]", r.Signals)
	}
	if !containsPID(r.PIDs, fresh) {
		t.Fatalf("PIDs = %v, want the member %d forked after SIGTERM", r.PIDs, fresh)
	}
	assertTerminated(t, fresh, "member forked after SIGTERM", 5*time.Second)
}

// TestRecoverGroups_SignalUnavailableIsUnknown: without pidfds (an old kernel,
// or a seccomp filter) there is no race-free way to signal a proven process,
// and recovery never falls back to a bare pid.
func TestRecoverGroups_SignalUnavailableIsUnknown(t *testing.T) {
	requireLinuxRecovery(t)
	for _, errno := range []syscall.Errno{syscall.ENOSYS, syscall.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			_, g := startRecoveryGroup(t, "web", "sleep", "60")
			restore := pidfdOpen
			pidfdOpen = func(int) (int, error) { return -1, errno }
			t.Cleanup(func() { pidfdOpen = restore })

			got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
			r := assertSingleRecovery(t, got, OutcomeUnknown, "signal_unavailable")
			if len(r.Signals) != 0 {
				t.Fatalf("Signals = %v without pidfds, want none", r.Signals)
			}
			assertStillRunning(t, g.PGID, "leader without pidfds")
		})
	}
}

// TestRecoverGroups_OwnPgrpAndPgidOneRefused pins the signal guards. Signals
// are recorded rather than sent, so a broken guard fails the test instead of
// killing the test binary's own process group.
//
// The own-group guard sits inside rule C, after rules E and R: a recorded pgid
// whose number now belongs to a different leader is proven empty as
// pgid_reused even when that leader heads the caller's own group, and a group
// whose leader is still the recorded one is refused as own_group.
func TestRecoverGroups_OwnPgrpAndPgidOneRefused(t *testing.T) {
	sent := recordSignals(t)
	probes := recordProbes(t)
	own := syscall.Getpgrp()
	ownStart := leaderStartFor(own)
	groups := []daemon.Group{
		{Name: "own", PGID: own, LeaderStart: ownStart},
		{Name: "init", PGID: 1, LeaderStart: leaderStartFor(1)},
		{Name: "negative", PGID: -5, LeaderStart: 1},
	}
	type verdict struct{ outcome, reason string }
	want := map[string]verdict{
		"own":      {OutcomeUnknown, "own_group"},
		"init":     {OutcomeUnknown, "invalid_pgid"},
		"negative": {OutcomeUnknown, "invalid_pgid"},
	}
	if ownStart == 0 {
		// No live leader with a readable start time (macOS, or a group whose
		// leader exited): the group cannot be proven at all, so it is unknown
		// for that reason; it is still never signalled.
		want["own"] = verdict{OutcomeUnknown, ""}
	} else if runtime.GOOS == "linux" {
		groups = append(groups, daemon.Group{Name: "own-reused", PGID: own, LeaderStart: ownStart + 1})
		want["own-reused"] = verdict{OutcomeEmpty, "pgid_reused"}
	}
	got := RecoverGroups(groups, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	if len(got) != len(groups) {
		t.Fatalf("RecoverGroups returned %+v, want %d results", got, len(groups))
	}
	for _, r := range got {
		w := want[r.Name]
		if r.Outcome != w.outcome || (w.reason != "" && r.Reason != w.reason) || len(r.Signals) != 0 {
			t.Errorf("%s: %+v, want %s %s with no signal", r.Name, r, w.outcome, w.reason)
		}
	}
	if len(*sent) != 0 {
		t.Fatalf("signals %v were sent for refused groups", *sent)
	}
	for _, pid := range *probes {
		if pid >= -1 {
			t.Fatalf("existence probe named pid %d, which is never a single foreign group", pid)
		}
	}
}

// TestRecoverGroups_PendingIsUnknown: an entry written before cmd.Start names
// no group yet; pgid 0 must never reach kill(2), where it means our own group.
func TestRecoverGroups_PendingIsUnknown(t *testing.T) {
	probes := recordProbes(t)
	got := RecoverGroups([]daemon.Group{{Name: "api"}}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	r := assertSingleRecovery(t, got, OutcomeUnknown, "launch_pending")
	if len(r.Signals) != 0 || len(*probes) != 0 {
		t.Fatalf("signals %v / probes %v for a pending launch, want none", r.Signals, *probes)
	}
}

// TestRecoverGroups_UnknownGroupDoesNotSpareProvenOnes: one group left unknown
// must not stop recovery from ending the groups it can prove. Each record is
// resolved on its own, so a mixed crash still terminates every provable
// survivor and reports only the rest as unknown.
func TestRecoverGroups_UnknownGroupDoesNotSpareProvenOnes(t *testing.T) {
	requireLinuxRecovery(t)
	_, g := startRecoveryGroup(t, "web", "sleep", "60")

	got := RecoverGroups([]daemon.Group{{Name: "api"}, g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(10*time.Second))
	if len(got) != 2 {
		t.Fatalf("RecoverGroups returned %d results, want 2: %+v", len(got), got)
	}
	byName := map[string]RecoveredGroup{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if r := byName["api"]; r.Outcome != OutcomeUnknown || r.Reason != "launch_pending" {
		t.Fatalf("pending group = outcome %q reason %q, want unknown launch_pending (%+v)", r.Outcome, r.Reason, r)
	}
	r := byName["web"]
	if r.Outcome != OutcomeTerminated || r.Reason != "leader_proven" {
		t.Fatalf("proven group = outcome %q reason %q, want terminated leader_proven (%+v)", r.Outcome, r.Reason, r)
	}
	if !reflect.DeepEqual(r.Signals, []string{"SIGKILL"}) {
		t.Fatalf("proven group Signals = %v, want [SIGKILL]", r.Signals)
	}
	assertTerminated(t, g.PGID, "proven leader beside an unknown group", 5*time.Second)
}

// TestRecoverGroups_ReapedGroupIsEmpty: a group with no live holder of its pgid
// ended, whether or not its identity can be proven. It runs on every Unix.
func TestRecoverGroups_ReapedGroupIsEmpty(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := daemon.Group{Name: "done", PGID: cmd.Process.Pid, LeaderStart: leaderStartFor(cmd.Process.Pid)}
	_ = cmd.Wait()
	sent := recordSignals(t)

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	r := assertSingleRecovery(t, got, OutcomeEmpty, "no_members")
	if len(r.Signals) != 0 || len(*sent) != 0 {
		t.Fatalf("signals %v / sent %v for an empty group, want none", r.Signals, *sent)
	}
}

// TestRecoverGroups_UnsignallableMemberIsNeverProvenEmpty: on Linux
// kill(-P, 0) answers EPERM only while P has a member this process may not
// signal. Under a procfs mounted hidepid=2 that member, another user's process
// for example, is invisible to the membership scan as well, so an EPERM probe
// with scans that find nothing must leave the group unknown, never empty. An
// unprivileged test cannot build such a group, so a reaped group (which the
// scan cannot see either) stands in for it and the probe is stubbed.
func TestRecoverGroups_UnsignallableMemberIsNeverProvenEmpty(t *testing.T) {
	requireLinuxRecovery(t)
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := daemon.Group{Name: "hidden", PGID: cmd.Process.Pid, LeaderStart: leaderStartFor(cmd.Process.Pid)}
	_ = cmd.Wait()
	restore := groupKill
	groupKill = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			t.Errorf("groupKill(%d, %v): recovery must signal only through pidfds", pid, sig)
		}
		return syscall.EPERM
	}
	t.Cleanup(func() { groupKill = restore })
	sent := recordSignals(t)

	for _, mode := range []RecoveryMode{RecoverAssessOnly, RecoverForce} {
		got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), mode, time.Now().Add(5*time.Second))
		r := assertSingleRecovery(t, got, OutcomeUnknown, "enumeration_failed")
		if len(r.Signals) != 0 || len(*sent) != 0 {
			t.Fatalf("mode %v: signals %v / sent %v for an unprovable group, want none", mode, r.Signals, *sent)
		}
	}

	// Members the scan can see are still reported: the EPERM rule only refuses
	// to read an empty scan as proof of absence.
	_, live := startRecoveryGroup(t, "visible", "sleep", "60")
	empty, members, known := groupProvenEmpty(live.PGID)
	if empty || !known || !containsPID(members, live.PGID) {
		t.Fatalf("groupProvenEmpty(visible group) with an EPERM probe = %v %v %v, want members including %d",
			empty, members, known, live.PGID)
	}
}

// TestRecoverGroups_SignallableHiddenHolderIsNeverProvenEmpty: a probe that
// succeeds proves a holder this process may signal. A setuid process that keeps
// this user's real uid answers the probe yet is hidden from the scan under
// hidepid, so an empty scan is proof only when a visible dead holder explains
// the probe. A reaped group stands in for the hidden holder, as above.
func TestRecoverGroups_SignallableHiddenHolderIsNeverProvenEmpty(t *testing.T) {
	requireLinuxRecovery(t)
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := daemon.Group{Name: "hidden", PGID: cmd.Process.Pid, LeaderStart: leaderStartFor(cmd.Process.Pid)}
	_ = cmd.Wait()
	restore := groupKill
	groupKill = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			t.Errorf("groupKill(%d, %v): recovery must signal only through pidfds", pid, sig)
		}
		return nil
	}
	sent := recordSignals(t)
	for _, mode := range []RecoveryMode{RecoverAssessOnly, RecoverForce} {
		got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), mode, time.Now().Add(5*time.Second))
		r := assertSingleRecovery(t, got, OutcomeUnknown, "enumeration_failed")
		if len(r.Signals) != 0 || len(*sent) != 0 {
			t.Fatalf("mode %v: signals %v / sent %v for an unprovable group, want none", mode, r.Signals, *sent)
		}
	}
	groupKill = restore

	// A visible zombie explains a successful probe: an exited leader that has
	// not been reaped yet leaves the group provably empty of live members.
	zombie := exec.Command("true")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zombie.Wait() })
	pgid := zombie.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, err := daemon.ProcStat(pgid); err == nil && isZombieState(info.State) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leader %d never became a zombie", pgid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := groupKill(-pgid, 0); err != nil {
		t.Fatalf("kill(-%d, 0) on a zombie-only group = %v, want success; the test proves nothing", pgid, err)
	}
	if empty, members, known := groupProvenEmpty(pgid); !empty || !known || len(members) != 0 {
		t.Fatalf("groupProvenEmpty(zombie-only group) = %v %v %v, want empty and known", empty, members, known)
	}
}

// TestRecoveryLive_ZombieStateWithOtherThreadsIsLive pins the recovery
// liveness rule: a Z or X state is dead only for a single-task process. A
// thread-group leader whose main thread exited reports Z while its other
// threads run, and a task list that cannot be read proves nothing.
func TestRecoveryLive_ZombieStateWithOtherThreadsIsLive(t *testing.T) {
	restore := procTasks
	t.Cleanup(func() { procTasks = restore })
	for _, tc := range []struct {
		state byte
		tasks int
		err   error
		want  bool
	}{
		{state: 'S', tasks: 1, want: true},
		{state: 'R', tasks: 0, err: os.ErrNotExist, want: true},
		{state: 'Z', tasks: 1, want: false},
		{state: 'X', tasks: 1, want: false},
		{state: 'Z', tasks: 2, want: true},
		{state: 'X', tasks: 3, want: true},
		{state: 'Z', tasks: 0, err: os.ErrPermission, want: true},
	} {
		procTasks = func(int) (int, error) { return tc.tasks, tc.err }
		if got := recoveryLive(4242, tc.state); got != tc.want {
			t.Errorf("recoveryLive(state %q, %d tasks, err %v) = %v, want %v", tc.state, tc.tasks, tc.err, got, tc.want)
		}
	}
}

// zombieLeaderScript exits its main thread while another thread keeps
// running, so procfs reports the thread-group leader as Z although the
// process is alive.
const zombieLeaderScript = `import ctypes, threading, time
threading.Thread(target=time.sleep, args=(60,)).start()
ctypes.CDLL(None).pthread_exit(None)
`

// TestRecoverGroups_ZombieLeaderWithLiveThreadsIsNotEmpty: a group whose only
// process is a running thread group with an exited main thread must not be
// proven empty. It is proven ours like any other live leader and ends on
// SIGKILL.
func TestRecoverGroups_ZombieLeaderWithLiveThreadsIsNotEmpty(t *testing.T) {
	requireLinuxRecovery(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to exit a leader's main thread while other threads run")
	}
	script := writeScript(t, t.TempDir(), "zombie_leader.py", zombieLeaderScript)
	_, g := startRecoveryGroup(t, "threads", python, script)
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, statErr := daemon.ProcStat(g.PGID)
		n, taskErr := countProcTasks(g.PGID)
		if statErr == nil && info.State == 'Z' && taskErr == nil && n > 1 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Skipf("could not build a Z leader with live threads (stat %+v %v, tasks %d %v)", info, statErr, n, taskErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverAssessOnly, time.Now())
	r := assertSingleRecovery(t, got, OutcomeLive, "leader_proven")
	if !containsPID(r.PIDs, g.PGID) {
		t.Fatalf("assessment = %+v, want the running Z leader %d reported live", r, g.PGID)
	}

	got = RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	r = assertSingleRecovery(t, got, OutcomeTerminated, "leader_proven")
	if !reflect.DeepEqual(r.Signals, []string{"SIGKILL"}) || !containsPID(r.PIDs, g.PGID) {
		t.Fatalf("forced recovery = %+v, want SIGKILL to the Z leader %d", r, g.PGID)
	}
}

// TestRecoverGroups_LiveGroupWithoutStartTimeIsNeverSignalled is the macOS
// contract, checked on every Unix: with no leader start time nothing proves a
// live group ours, so it is unknown and never signalled.
func TestRecoverGroups_LiveGroupWithoutStartTimeIsNeverSignalled(t *testing.T) {
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	g.LeaderStart = 0
	sent := recordSignals(t)

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverForce, time.Now().Add(5*time.Second))
	if len(got) != 1 || got[0].Outcome != OutcomeUnknown || len(got[0].Signals) != 0 || len(*sent) != 0 {
		t.Fatalf("RecoverGroups = %+v (sent %v), want unknown with no signal", got, *sent)
	}
	assertStillRunning(t, g.PGID, "leader with no recorded start time")
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// TestRecoverGroups_AssessOnlyReportsLiveWithoutSignal is what `up` relies on:
// assessing a crashed run proves a running group ours and reports it live, but
// opens no pidfd and sends nothing.
func TestRecoverGroups_AssessOnlyReportsLiveWithoutSignal(t *testing.T) {
	requireLinuxRecovery(t)
	_, g := startRecoveryGroup(t, "web", "sleep", "60")
	restore := pidfdOpen
	pidfdOpen = func(int) (int, error) { t.Error("AssessOnly opened a pidfd"); return -1, syscall.ENOSYS }
	t.Cleanup(func() { pidfdOpen = restore })

	got := RecoverGroups([]daemon.Group{g}, daemon.HostIdentity(), RecoverAssessOnly, time.Now())
	r := assertSingleRecovery(t, got, OutcomeLive, "leader_proven")
	if len(r.Signals) != 0 || r.LiveMembers != 1 || !containsPID(r.PIDs, g.PGID) {
		t.Fatalf("assessment = %+v, want the live leader reported and no signal", r)
	}
	assertStillRunning(t, g.PGID, "assessed leader")
}

// TestRecordedGroupOf_ProvesMembershipLikeAssessOnly pins the holder half of
// session placement: a running process belongs to a recorded group only when
// the record passes the host rules and the group's leader is proven by its
// start time, as RecoverAssessOnly proves it. It opens no pidfd and sends
// nothing.
func TestRecordedGroupOf_ProvesMembershipLikeAssessOnly(t *testing.T) {
	requireLinuxRecovery(t)
	memberFile := filepath.Join(t.TempDir(), "member.pid")
	_, g := startRecoveryGroup(t, "api", "sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", memberFile)
	member, ok := waitForPIDFile(t, memberFile, 10*time.Second)
	if !ok {
		t.Fatal("member never recorded its PID")
	}
	trackProcessForCleanup(t, member, "member")
	restore := pidfdOpen
	pidfdOpen = func(int) (int, error) { t.Error("recordedGroupOf opened a pidfd"); return -1, syscall.ENOSYS }
	t.Cleanup(func() { pidfdOpen = restore })
	sent := recordSignals(t)
	other := daemon.Group{Name: "web", PGID: 99999, LeaderStart: 1}
	host := daemon.HostIdentity()

	for _, pid := range []int{g.PGID, member} {
		if got, ok := recordedGroupOf(pid, []daemon.Group{other, g}, host); !ok || got != g {
			t.Fatalf("recordedGroupOf(%d) = %+v, %v; want %+v", pid, got, ok, g)
		}
	}
	reused := g
	reused.LeaderStart++
	noStart := g
	noStart.LeaderStart = 0
	rebooted := host
	rebooted.BootID = "00000000-0000-0000-0000-000000000000"
	for name, tc := range map[string]struct {
		pid    int
		groups []daemon.Group
		host   daemon.Host
	}{
		"leader start reused":  {member, []daemon.Group{reused}, host},
		"no leader start":      {member, []daemon.Group{noStart}, host},
		"another boot's group": {member, []daemon.Group{g}, rebooted},
		"not a member":         {os.Getpid(), []daemon.Group{g}, host},
		"no groups":            {member, nil, host},
		"no process":           {0, []daemon.Group{g}, host},
	} {
		if got, ok := recordedGroupOf(tc.pid, tc.groups, tc.host); ok {
			t.Errorf("%s: recordedGroupOf = %+v, want no group", name, got)
		}
	}
	if len(*sent) != 0 {
		t.Fatalf("recordedGroupOf sent %v", *sent)
	}
	assertStillRunning(t, member, "member")
}

// TestRun_ReadyTimeoutGracefulExitCountsAsFailure is the regression for the
// probe-cancel verdict. A readiness timeout cancels the incarnation; a service
// that handles SIGTERM by exiting 0 used to land in the clean-exit branch, so it
// was reported "exited", never restarted under on-failure, never counted toward
// max_restarts, and under restart: never the run did not fail. Both cancel
// sites are covered: the tcp/http probe result and the log-probe timer.
//
// The verdict must not depend on how the reap reports the cancel. os/exec turns
// a clean exit into ctx.Err() only when its context watcher's cmd.Cancel
// succeeds; when that watcher is scheduled after the group was withdrawn,
// Cancel returns os.ErrProcessDone and Wait reports the raw exit status, 0. The
// waitProcess seam forces that outcome so the test does not rely on scheduling.
func TestRun_ReadyTimeoutGracefulExitCountsAsFailure(t *testing.T) {
	tests := []struct {
		name  string
		probe config.ReadyWhen
		want  string
	}{
		// 127.0.0.1:1 is a reserved port that nothing listens on.
		{"tcp", config.ReadyWhen{TCP: "127.0.0.1:1"}, "readiness probe (tcp) timed out"},
		{"log", config.ReadyWhen{Log: "^NEVER$"}, "readiness probe (log) timed out"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probe := tc.probe
			r := makeRunner(map[string]config.Process{
				"server": {
					// exec keeps the helper as the group leader, so the group
					// SIGTERM reaches only the process that exits 0 on it.
					Cmd:          serviceHelperCmd("term-exit0"),
					Restart:      "never",
					ReadyTimeout: 1,
					ReadyWhen:    &probe,
				},
			})
			r.waitProcess = func(cmd *exec.Cmd) error {
				err := cmd.Wait()
				if errors.Is(err, context.Canceled) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
					return nil // the watcher lost the race: Wait reports the clean exit
				}
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := r.Run(ctx)
			if ctx.Err() != nil {
				t.Fatalf("Run did not finish on its own: %v", err)
			}
			got := snapshotOf(r, "server")
			if got.ExitCode == nil || *got.ExitCode != 0 {
				t.Fatalf("exit code = %s, want 0: the fixture must exit gracefully on SIGTERM to exercise the clean-exit path", fmtExitCode(got.ExitCode))
			}
			if got.State != "failed" {
				t.Errorf("state = %q, want failed after a readiness timeout", got.State)
			}
			if !strings.Contains(got.LastError, tc.want) {
				t.Errorf("last_error = %q, want it to contain %q", got.LastError, tc.want)
			}
			if err == nil || !strings.Contains(err.Error(), "processes failed") {
				t.Fatalf("Run error = %v, want processes failed", err)
			}
		})
	}
}

// TestRun_ReadyWhen_CmdProbeCutAtDeadlineDoesNotPass is the cmd-probe
// counterpart of TestRun_ReadyTimeoutGracefulExitCountsAsFailure. A probe
// attempt that hangs until ready_timeout and then exits 0 on SIGTERM never
// passed, so the service must fail at ready_timeout rather than latch ready.
// The waitProcess seam forces the reap os/exec reports when its context
// watcher loses the race with the group's withdrawal: the raw exit status 0.
func TestRun_ReadyWhen_CmdProbeCutAtDeadlineDoesNotPass(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			Restart:      "never",
			ReadyTimeout: 2,
			ReadyWhen:    &config.ReadyWhen{Cmd: serviceHelperCmd("term-exit0")},
			// The probe inherits the process env; see
			// TestRun_HealthReportMarksUnhealthyAndRecovers.
			Env: map[string]string{"GORACE": "atexit_sleep_ms=0"},
		},
	})
	var cutExit0 atomic.Bool
	r.waitProcess = func(cmd *exec.Cmd) error {
		err := cmd.Wait()
		// Only probe attempts run under a deadline.
		if errors.Is(err, context.DeadlineExceeded) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			cutExit0.Store(true)
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if ctx.Err() != nil {
		t.Fatalf("Run did not finish on its own (%v): worker = %+v", err, snapshotOf(r, "worker"))
	}
	if !cutExit0.Load() {
		t.Fatal("no probe attempt was cut at its deadline and exited 0: the fixture did not exercise the race")
	}
	const want = "readiness probe (cmd) timed out (last attempt: context deadline exceeded)"
	if got := snapshotOf(r, "worker"); got.State != "failed" || got.LastError != want {
		t.Fatalf("worker = %+v, want failed with LastError %q", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "processes failed") {
		t.Fatalf("Run error = %v, want processes failed", err)
	}
}

// TestRun_HealthRestartRespectsMaxRestarts pins health_action: restart. An
// unhealthy incarnation is cancelled and counted as a failure even when it
// exits 0 on SIGTERM, so it restarts through the ordinary policy path and
// max_restarts ends it as failed. The waitProcess seam forces the clean-exit
// reap, as in TestRun_ReadyTimeoutGracefulExitCountsAsFailure.
func TestRun_HealthRestartRespectsMaxRestarts(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"server": {
			Cmd:            serviceHelperCmd("term-exit0"),
			Restart:        "on-failure",
			MaxRestarts:    1,
			HealthWhen:     &config.ReadyWhen{TCP: "127.0.0.1:1"},
			HealthInterval: 1,
			HealthAction:   config.HealthActionRestart,
		},
	})
	var journal bytes.Buffer
	r.Output, r.Journal = io.Discard, &journal
	r.waitProcess = func(cmd *exec.Cmd) error {
		err := cmd.Wait()
		if errors.Is(err, context.Canceled) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if ctx.Err() != nil {
		t.Fatalf("Run did not finish on its own: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "processes failed") {
		t.Fatalf("Run error = %v, want processes failed", err)
	}
	got := snapshotOf(r, "server")
	if got.State != "failed" || got.Restarts != 1 || got.Generation != 2 || !strings.Contains(got.LastError, "max restarts (1)") {
		t.Fatalf("final state = %+v, want failed after one health restart", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exit code = %s, want 0: the fixture must exit gracefully on SIGTERM", fmtExitCode(got.ExitCode))
	}
	failed := 0
	for _, event := range journalEvents(t, &journal) {
		if event.Code == "process.failed" {
			failed++
			if !strings.Contains(event.Line, "health check failed (3 consecutive)") {
				t.Fatalf("process.failed = %q, want the health verdict", event.Line)
			}
		}
	}
	if failed != 2 {
		t.Fatalf("process.failed events = %d, want one per unhealthy incarnation", failed)
	}
}

func fmtExitCode(code *int) string {
	if code == nil {
		return "<nil>"
	}
	return strconv.Itoa(*code)
}

// ── termination signalling: membership versus authority ──────────────────────

// fixedStateGroup is a ProcessGroup double that reports a fixed state and
// records the signals it received.
//
// When emptyAfterKill is set it reports an empty group once it has been killed,
// which models a group that really did drain; that keeps the tests that drive
// the whole escalation short, because the settle window after a kill is two
// seconds long.
type fixedStateGroup struct {
	state          GroupState
	emptyAfterKill bool

	mu         sync.Mutex
	terminated int
	killed     int
}

func (g *fixedStateGroup) Setup(*exec.Cmd)       {}
func (g *fixedStateGroup) Track(*exec.Cmd) error { return nil }

func (g *fixedStateGroup) Terminate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.terminated++
	return nil
}

func (g *fixedStateGroup) Kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.killed++
	return nil
}

func (g *fixedStateGroup) State() GroupState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.emptyAfterKill && g.killed > 0 {
		return GroupEmpty
	}
	return g.state
}

func (g *fixedStateGroup) Identify() string { return "fixed" }
func (g *fixedStateGroup) Withdraw()        {}

func (g *fixedStateGroup) Seal() (SealObservation, error) {
	return SealObservation{State: g.State(), Identity: "fixed"}, nil
}

func (g *fixedStateGroup) Close() error { return nil }

func (g *fixedStateGroup) counts() (terminated, killed int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.terminated, g.killed
}

// TestTerminateGroup_UnknownGroupIsSignalled pins that a group whose
// membership cannot be enumerated is still signalled.
//
// The runner has already established signalling authority when it gets here,
// so declining to signal because the membership is unknown would leave the
// tree running in exactly the situation where the runner cannot prove it is
// gone.
func TestTerminateGroup_UnknownGroupIsSignalled(t *testing.T) {
	pg := &fixedStateGroup{state: GroupUnavailable, emptyAfterKill: true}
	terminateGroup(pg, 50*time.Millisecond)
	terminated, killed := pg.counts()
	if terminated != 1 {
		t.Fatalf("Terminate calls on an unknown group = %d, want 1", terminated)
	}
	if killed != 1 {
		t.Fatalf("Kill calls on an unknown group = %d, want the escalation to run", killed)
	}
}

// TestTerminateGroup_EmptyGroupIsNotSignalled pins the other half of the rule:
// an empty group has nothing left to stop, so it is neither signalled nor
// waited for.
func TestTerminateGroup_EmptyGroupIsNotSignalled(t *testing.T) {
	pg := &fixedStateGroup{state: GroupEmpty}
	start := time.Now()
	terminateGroup(pg, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("an empty group spent %s of the grace window", elapsed)
	}
	terminated, killed := pg.counts()
	if terminated != 0 || killed != 0 {
		t.Fatalf("signals sent to an empty group: terminate=%d kill=%d, want none", terminated, killed)
	}
}

// TestWaitWhileLive_UnknownGroupIsNotDrained pins that "unknown" is not
// "drained": a group whose membership cannot be enumerated is polled until the
// budget is spent, so the escalation that follows the wait still runs.
func TestWaitWhileLive_UnknownGroupIsNotDrained(t *testing.T) {
	pg := &fixedStateGroup{state: GroupUnavailable}
	start := time.Now()
	drained := waitWhileLive(pg, 50*time.Millisecond)
	elapsed := time.Since(start)
	if drained {
		t.Fatal("waitWhileLive reported an unknown group as drained")
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("waitWhileLive returned after %s, want it to spend the budget", elapsed)
	}
}

// TestWaitWhileLive_EmptyGroupDrainsImmediately pins that an empty group ends
// the wait at once, so a successful command with no descendants pays no
// teardown budget.
func TestWaitWhileLive_EmptyGroupDrainsImmediately(t *testing.T) {
	pg := &fixedStateGroup{state: GroupEmpty}
	start := time.Now()
	if !waitWhileLive(pg, 5*time.Second) {
		t.Fatal("waitWhileLive did not report an empty group as drained")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("an empty group spent %s of the budget", elapsed)
	}
}

// TestSignal_DeadGroupReportsProcessDone pins the ProcessGroup contract: a
// group that is gone reports os.ErrProcessDone, not the raw ESRCH.
//
// The contract is in processgroup.go: Terminate and Kill "return
// os.ErrProcessDone when the group can no longer be signalled". A caller
// cannot tell "nothing left to signal" from "the signal failed" unless the
// two are the same value.
func TestSignal_DeadGroupReportsProcessDone(t *testing.T) {
	pg := &unixProcessGroup{pgid: deadProcessGroupID(t)}
	err := pg.Terminate()
	if !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Terminate on a dead group = %v, want os.ErrProcessDone", err)
	}
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill on a dead group = %v, want os.ErrProcessDone", err)
	}
}

// deadProcessGroupID returns a process-group ID that provably does not exist,
// so a signal to it is answered with ESRCH rather than reaching a process.
//
// It starts a short-lived child, waits for it to be collected, and then uses
// its identifier: the identifier is free again, and a group led by it does not
// exist.
func deadProcessGroupID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for probe: %v", err)
	}
	return pid
}

// ── Setup: the caller's attributes ───────────────────────────────────────────

// TestSetup_KeepsCallerSysProcAttr pins that Setup adds the process-group flag
// to the caller's attributes instead of replacing them.
//
// The Windows implementation already preserves them, and the contract is the
// same on both platforms: a caller that sets credentials, a chroot or a
// parent-death signal must not silently lose them.
func TestSetup_KeepsCallerSysProcAttr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: "/tmp"}
	pg := newProcessGroup()
	pg.Setup(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("Setup dropped the caller's SysProcAttr")
	}
	if cmd.SysProcAttr.Chroot != "/tmp" {
		t.Fatalf("caller's SysProcAttr.Chroot = %q, want it preserved", cmd.SysProcAttr.Chroot)
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setup did not set Setpgid")
	}
}

// TestSetup_SetsSetpgidWithoutCallerStruct pins the other half: a command
// that brought no attributes of its own still ends up in its own group.
func TestSetup_SetsSetpgidWithoutCallerStruct(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	pg := newProcessGroup()
	pg.Setup(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("Setup left SysProcAttr nil")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setup did not set Setpgid")
	}
}

// TestSetup_CommandLeadsItsOwnGroupWhateverTheCallerSet pins the fields Setup
// does not leave to the caller. Track records the command's PID as its group
// ID, so the command must lead a new group whose ID is its own PID:
//
//   - a caller's Pgid used to survive and put the command into an existing
//     group, so every signal went to a group the command was not in and its
//     ESRCH read as "nothing left to signal";
//   - a caller's Setsid used to be combined with Setpgid, and setpgid(2) on the
//     new session leader fails, so the command could not start at all.
//
// Each case starts a real command and checks the kernel's answer, and the
// group is then killed through Setup's own group.
func TestSetup_CommandLeadsItsOwnGroupWhateverTheCallerSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attr   func() *syscall.SysProcAttr
		setsid bool
	}{
		{"caller pgid", func() *syscall.SysProcAttr {
			return &syscall.SysProcAttr{Setpgid: true, Pgid: syscall.Getpgrp()}
		}, false},
		{"caller setsid", func() *syscall.SysProcAttr {
			return &syscall.SysProcAttr{Setsid: true}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			argv := helperArgv("sleep", "60s")
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.SysProcAttr = tc.attr()
			pg := newProcessGroup()
			pg.Setup(cmd)
			if cmd.SysProcAttr.Pgid != 0 {
				t.Fatalf("Pgid = %d after Setup, want 0", cmd.SysProcAttr.Pgid)
			}
			if cmd.SysProcAttr.Setsid != tc.setsid {
				t.Fatalf("Setsid = %v after Setup, want the caller's %v", cmd.SysProcAttr.Setsid, tc.setsid)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start with the caller's attributes after Setup: %v", err)
			}
			pid := cmd.Process.Pid
			t.Cleanup(func() {
				_ = cmd.Process.Kill() // the owned handle, whatever group it is in
				_, _ = cmd.Process.Wait()
			})
			if err := pg.Track(cmd); err != nil {
				t.Fatalf("track: %v", err)
			}

			pgid, err := syscall.Getpgid(pid)
			if err != nil {
				t.Fatalf("getpgid(%d): %v", pid, err)
			}
			if pgid != pid {
				t.Fatalf("the command's group = %d, want a new group led by the command (%d)", pgid, pid)
			}
			if tc.setsid {
				if sid, err := unix.Getsid(pid); err != nil || sid != pid {
					t.Fatalf("the command's session = %d (%v), want the caller's setsid to lead a new session (%d)", sid, err, pid)
				}
			}

			if err := pg.Kill(); err != nil {
				t.Fatalf("Kill() = %v, want the group signal to reach the command", err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("Wait() = nil after the group kill, want the command killed")
			}
		})
	}
}

// ── setsid(2) and the managed process group ─────────────────────────────────

// setsidHelperSentinel marks the helper invocation of TestRunnerSetsidHelper.
const setsidHelperSentinel = "__PROC_COMPOSE_SETSID_HELPER__"

// setsidHelperArgs builds the argument list that re-invokes this test binary
// in setsid-helper mode.
//
// The helper calls setsid(2) itself. The setsid(1) utility cannot stand in for
// it: it forks when its caller is already a process-group leader, so it would
// never report the restriction this test is about.
func setsidHelperArgs(args ...string) []string {
	return []string{
		"-test.run=^TestRunnerSetsidHelper$",
		"--",
		setsidHelperSentinel,
		encodeHelperArgs(args...),
	}
}

// setsidHelperPayload returns the decoded helper payload, and whether this
// process is the helper at all.
func setsidHelperPayload() (string, bool) {
	for i, a := range os.Args {
		if a != setsidHelperSentinel {
			continue
		}
		if i+1 >= len(os.Args) {
			return "", false
		}
		decoded, err := base64.RawURLEncoding.DecodeString(os.Args[i+1])
		if err != nil {
			return "", false
		}
		return string(decoded), true
	}
	return "", false
}

// TestRunnerSetsidHelper is the child program of the setsid tests. It exists
// as a test so the test binary can re-invoke itself.
func TestRunnerSetsidHelper(t *testing.T) {
	payload, ok := setsidHelperPayload()
	if !ok {
		return
	}
	mode := "setsid"
	if parts := strings.Split(payload, "\x00"); len(parts) > 0 && parts[0] != "" {
		mode = parts[0]
	}
	switch mode {
	case "setsid":
		os.Exit(runSetsidHelper())
	case "setsid-descendant":
		os.Exit(runSetsidDescendantHelper())
	}
	os.Exit(2)
}

// runSetsidHelper calls setsid(2) in this process and reports the outcome.
func runSetsidHelper() int {
	if _, err := unix.Setsid(); err != nil {
		name := "unknown"
		if errors.Is(err, syscall.EPERM) {
			name = "EPERM"
		}
		fmt.Fprintf(os.Stderr, "setsid failed: %s: %v\n", name, err)
		return 3
	}
	fmt.Println("setsid ok")
	return 0
}

// runSetsidDescendantHelper starts a child that calls setsid(2) and reports
// the child's exit code.
//
// The child is a descendant, not the group leader, so its setsid(2) call is
// allowed — that is the case the restriction does not cover.
func runSetsidDescendantHelper() int {
	child := exec.Command(os.Args[0], setsidHelperArgs("setsid")...)
	out, err := child.CombinedOutput()
	fmt.Printf("%s", out)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			fmt.Printf("descendant exit: %d\n", exitErr.ExitCode())
			return 0
		}
		fmt.Printf("descendant failed: %v\n", err)
		return 4
	}
	fmt.Println("descendant exit: 0")
	return 0
}

// TestSetsid_LeaderSideCallIsRefusedUnderProcessGroupSetup pins the
// documented restriction: a managed command is the leader of its own process
// group, and POSIX forbids a process-group leader from calling setsid(2).
//
// It pins the two cases the restriction does not cover as well: the same child
// succeeds when it is not put in a group of its own, and a descendant — which
// is not the leader — may call setsid(2) freely.
func TestSetsid_LeaderSideCallIsRefusedUnderProcessGroupSetup(t *testing.T) {
	out, code := runSetsidChild(t, false)
	if code != 0 || !strings.Contains(out, "setsid ok") {
		t.Fatalf("setsid without a process group = %q (exit %d), want it to succeed", out, code)
	}

	out, code = runSetsidChild(t, true)
	if code != 3 || !strings.Contains(out, "EPERM") {
		t.Fatalf("setsid as a group leader = %q (exit %d), want EPERM", out, code)
	}

	out, code = runSetsidChild(t, true, "setsid-descendant")
	if code != 0 || !strings.Contains(out, "descendant exit: 0") {
		t.Fatalf("setsid in a descendant = %q (exit %d), want it to succeed", out, code)
	}

	// The setsid(1) utility is not affected by the restriction: it forks when
	// its caller is already a group leader, so `cmd: setsid server` still
	// works.
	if path, err := exec.LookPath("setsid"); err == nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd := exec.CommandContext(ctx, path, "true")
		newProcessGroup().Setup(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setsid(1) as a group leader = %q (%v), want it to succeed", out, err)
		}
	}
}

// runSetsidChild runs the setsid helper and reports its combined output and
// exit code. When grouped is set the child leads its own process group, the
// way the runner launches a managed command.
func runSetsidChild(t *testing.T, grouped bool, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], setsidHelperArgs(args...)...)
	if grouped {
		newProcessGroup().Setup(cmd)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run setsid helper: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// ── group members and group-specific reasons ────────────────────────────────
//
// The shutdown verdict is the operator's only account of what could not be
// terminated, so it has to name the members it found and explain a failure
// with the reason of the group it is reporting on.

// stubGroupEnumeration replaces the platform's membership enumeration and
// returns the function that restores it.
func stubGroupEnumeration(t *testing.T, fn func(pgid int) ([]int, bool)) func() {
	t.Helper()
	previous := enumerateGroupMembers
	enumerateGroupMembers = fn
	return func() { enumerateGroupMembers = previous }
}

// stubGroupKill replaces the existence probe and returns the function that
// restores it.
func stubGroupKill(t *testing.T, fn func(pid int, sig syscall.Signal) error) func() {
	t.Helper()
	previous := groupKill
	groupKill = fn
	return func() { groupKill = previous }
}

// TestSurvivorsError_NamesTheEnumeratedMembers pins that the verdict names the
// members the runner enumerated: a group whose members could be read must not
// be reported as "could not be enumerated".
//
// The fixture has a descendant that stays in the group, so the member list
// holds a PID that is not the group's identifier: only an enumerated member
// list can name it.
func TestSurvivorsError_NamesTheEnumeratedMembers(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	pg, _, descendant := startGroupWithSurvivor(t, stay+" "+stubborn+" "+pidFile, pidFile)

	state := pg.State()
	if state != GroupOwned {
		t.Fatalf("State() = %v, want %v for a group with live members", state, GroupOwned)
	}
	err := survivorsError(groupResult("svc", pg, state))
	if err == nil {
		t.Fatal("survivorsError() on an owned group = nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Itoa(descendant)) {
		t.Fatalf("verdict = %q, want it to name the live member %d", msg, descendant)
	}
	if strings.Contains(msg, "could not be enumerated") {
		t.Fatalf("verdict = %q, want the enumerated member named instead", msg)
	}
}

// TestSurvivorsError_UnenumerableMembersKeepTheWording pins the other case:
// when the enumeration fails but the kill probe succeeds, the members really
// could not be enumerated, so the wording says so and no member list is
// rendered.
func TestSurvivorsError_UnenumerableMembersKeepTheWording(t *testing.T) {
	pg, _ := startTrackedGroup(t, helperCmd("sleep", "120s"))
	restoreEnumeration := stubGroupEnumeration(t, func(int) ([]int, bool) { return nil, false })
	defer restoreEnumeration()
	restoreKill := stubGroupKill(t, func(int, syscall.Signal) error { return nil })
	defer restoreKill()

	state := pg.State()
	if state != GroupOwned {
		t.Fatalf("State() = %v, want %v when only the kill probe succeeded", state, GroupOwned)
	}
	err := survivorsError(groupResult("svc", pg, state))
	if err == nil {
		t.Fatal("survivorsError() on an owned group = nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not be enumerated") {
		t.Fatalf("verdict = %q, want it to say the members could not be enumerated", msg)
	}
	if strings.Contains(msg, "still running") {
		t.Fatalf("verdict = %q, want no member list when the members are unknown", msg)
	}
}

// TestSealObservation_NamesTheEnumeratedMembers pins that the sealed path
// carries the members too: the sweep's verdict names the PIDs it found.
func TestSealObservation_NamesTheEnumeratedMembers(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)
	pg, _, descendant := startGroupWithSurvivor(t, stay+" "+stubborn+" "+pidFile, pidFile)

	observation, err := pg.Seal()
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	found := false
	for _, member := range observation.Members {
		if member == strconv.Itoa(descendant) {
			found = true
		}
	}
	if !found {
		t.Fatalf("seal observation members = %v, want the live member %d", observation.Members, descendant)
	}
	verdict := survivorsError(sealedGroupResult("svc", observation))
	if verdict == nil {
		t.Fatal("survivorsError() on a sealed group with a live member = nil, want an error")
	}
	if !strings.Contains(verdict.Error(), strconv.Itoa(descendant)) {
		t.Fatalf("verdict = %q, want it to name the live member %d", verdict, descendant)
	}
}

// TestSurvivorsError_ReasonBelongsToTheGroupItCameFrom pins that a reason
// belongs to the group that produced it: a group that has no reason of its own
// reports none, even when another group's membership could not be enumerated.
func TestSurvivorsError_ReasonBelongsToTheGroupItCameFrom(t *testing.T) {
	pgA, _ := startTrackedGroup(t, helperCmd("sleep", "120s"))
	restoreEnumeration := stubGroupEnumeration(t, func(int) ([]int, bool) { return nil, false })
	restoreKill := stubGroupKill(t, func(int, syscall.Signal) error { return syscall.ESRCH })
	if state := pgA.State(); state != GroupUnavailable {
		restoreEnumeration()
		restoreKill()
		t.Fatalf("State() = %v, want %v when neither the enumeration nor the probe answered", state, GroupUnavailable)
	}
	reason := unavailableReason(pgA)
	if reason == "" {
		restoreEnumeration()
		restoreKill()
		t.Fatal("group A has no reason of its own, want the enumeration failure")
	}
	restoreEnumeration()
	restoreKill()

	// Group B is a group with no reason of its own: it never failed a
	// probe, so its verdict must not explain itself with A's reason.
	pgB := &uncontainedProcessGroup{}
	err := survivorsError(groupResult("b", pgB, pgB.State()))
	if err == nil {
		t.Fatal("survivorsError() on an unavailable group = nil, want an error")
	}
	if strings.Contains(err.Error(), reason) {
		t.Fatalf("group B's verdict carries group A's reason: %v", err)
	}
}

// TestSurvivorsError_EstablishedGroupReportsNoReason pins that a group whose
// state was established successfully reports no reason: the failure of
// another group's enumeration must not survive on a healthy group.
func TestSurvivorsError_EstablishedGroupReportsNoReason(t *testing.T) {
	restoreEnumeration := stubGroupEnumeration(t, func(int) ([]int, bool) { return nil, false })
	restoreKill := stubGroupKill(t, func(int, syscall.Signal) error { return syscall.ESRCH })
	failing, _ := startTrackedGroup(t, helperCmd("sleep", "120s"))
	_ = failing.State()
	restoreEnumeration()
	restoreKill()

	healthy, _ := startTrackedGroup(t, helperCmd("sleep", "120s"))
	if state := healthy.State(); state != GroupOwned {
		t.Fatalf("State() = %v, want %v for a group with a live member", state, GroupOwned)
	}
	if reason := unavailableReason(healthy); reason != "" {
		t.Fatalf("a group whose state was established reports the reason %q, want none", reason)
	}
	if reason := unavailableReason(&uncontainedProcessGroup{}); reason != "" {
		t.Fatalf("a group with no reason of its own reports %q, want none", reason)
	}
}
