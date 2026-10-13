//go:build windows

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/kolesnikovae/go-winjob"
	"golang.org/x/sys/windows"
)

// waitDelay bounds how long os/exec waits for a cancelled command on Windows.
const waitDelay = 5 * time.Second

// windowsProcessGroup manages a command's tree through a Windows Job Object.
//
// A Job Object is a durable kernel object: the kernel tracks membership
// independently of any process, so the command leader exiting does not reduce the
// runner's authority over the children it left behind. That is why this
// implementation has no notion of an unreaped leader, why Withdraw is a no-op —
// the handle keeps its authority for as long as it is open — and why Close is the
// last-resort backstop, because the job carries KillOnJobClose.
//
// Containment comes before execution: Setup creates the command suspended, and
// Track resumes it only after it is in the job. A process that spawns a child as
// its first action therefore cannot place that child outside the job.
//
// Handle lifetime: mu is held for the whole of every handle-dependent operation,
// not merely long enough to copy the pointer. Terminate and QueryCounters both
// dereference the handle after it is handed to them, and Close releases it, so a
// lock that only covered the read would still let Close free the handle out from
// under an in-flight call. Holding one lock across use and release is what makes
// those three safe against each other.
type windowsProcessGroup struct {
	mu sync.Mutex
	// job is nil once the group has never been created or has been closed.
	job *winjob.JobObject
	// createErr records that the job object could not be created at all.
	createErr error
	// tracked records that a command was actually assigned to the job. A job that
	// exists but was never assigned holds nothing, so terminating it would report
	// work that was never done.
	tracked bool
	// cancelled records that cancellation reached Kill before Track. The command
	// is still suspended then and has run no code, so Track contains it without
	// resuming it and the lifecycle's teardown terminates the job.
	cancelled bool
}

// resumeCommand resumes a command created suspended. It is a variable so tests
// can make the resume fail after the command has been assigned to the job.
var resumeCommand = resumeSuspendedProcess

func newProcessGroup() ProcessGroup {
	job, err := winjob.Create("", winjob.WithKillOnJobClose())
	return &windowsProcessGroup{job: job, createErr: err}
}

func (pg *windowsProcessGroup) Setup(cmd *exec.Cmd) {
	// Keep any attributes the caller set; only the creation flag is added.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	// cmd.Cancel must be set before cmd.Start; assigning it later is a silent
	// no-op and falls back to os.Process.Kill, bypassing the Job Object.
	cmd.Cancel = func() error {
		return pg.Kill()
	}
	cmd.WaitDelay = waitDelay
}

// Track adds the started, still suspended command to the job and then resumes
// it, so the command runs only once it is contained.
//
// The lock covers the Assign call as well as the reads, so a concurrent Close
// cannot release the handle while Assign is using it.
//
// A job that could not be created, or could not accept the command, is reported
// as an error. Returning nil there would let the command run with no containment
// while State() answered GroupUnavailable, which is a silent downgrade of the
// termination guarantee this type exists to provide.
func (pg *windowsProcessGroup) Track(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("track: process not started")
	}
	pg.mu.Lock()
	defer pg.mu.Unlock()

	if pg.job == nil {
		if pg.createErr != nil {
			return fmt.Errorf("track: job object unavailable: %w", pg.createErr)
		}
		return fmt.Errorf("track: job object is closed")
	}
	// The command is still suspended, so a failed Assign leaves a process that
	// has run no code and has no descendants; the caller kills it through its
	// own handle.
	if err := pg.job.Assign(cmd.Process); err != nil {
		return fmt.Errorf("track: assign to job object: %w", err)
	}
	pg.tracked = true
	if pg.cancelled {
		// Cancellation already arrived. Leave the command suspended inside the
		// job: it never runs, and teardown terminates the job.
		return nil
	}
	if err := resumeCommand(cmd.Process.Pid); err != nil {
		// The command is in the job but never ran. End it now, and wait for it,
		// so the seal that follows observes an empty job rather than a member
		// that is only on its way out.
		termErr := pg.job.Terminate()
		waitErr := waitProcessExit(cmd.Process.Pid, killSettle)
		return fmt.Errorf("track: resume suspended command: %w", errors.Join(err, termErr, waitErr))
	}
	return nil
}

// resumeSuspendedProcess resumes the threads of a process created with
// CREATE_SUSPENDED. os/exec closes the primary thread handle that CreateProcess
// returns, so the thread is found by its owning process in a thread snapshot.
// The caller still holds the process handle, so the PID cannot be reused, and a
// suspended process's threads cannot exit on their own.
func resumeSuspendedProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("thread snapshot: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open thread %d: %w", entry.ThreadID, openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if resumeErr != nil {
			return fmt.Errorf("resume thread %d: %w", entry.ThreadID, resumeErr)
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate threads: %w", err)
	}
	if resumed == 0 {
		return fmt.Errorf("no thread found for process %d", pid)
	}
	return nil
}

// waitProcessExit waits up to timeout for pid to exit without reaping it, so
// the lifecycle's own reap still collects the exit status.
func waitProcessExit(pid int, timeout time.Duration) error {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(process)
	event, err := windows.WaitForSingleObject(process, uint32(timeout.Milliseconds()))
	if err != nil {
		return fmt.Errorf("wait for process %d: %w", pid, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d did not exit within %s", pid, timeout)
	}
	return nil
}

// Terminate ends the job. Windows has no SIGTERM for console applications, so
// there is no graceful phase: terminating the job is the whole teardown, and it
// reaches every member at once, including members whose parent has exited.
func (pg *windowsProcessGroup) Terminate() error { return pg.Kill() }

func (pg *windowsProcessGroup) Kill() error {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.job == nil || !pg.tracked {
		// Nothing is assigned to this container. os/exec treats
		// os.ErrProcessDone as "nothing left to interrupt" and carries on, which
		// is the same outcome an untracked Unix group reports. A command started
		// but not yet tracked is still suspended; recording the request makes
		// Track contain it without letting it run.
		pg.cancelled = true
		return os.ErrProcessDone
	}
	return pg.job.Terminate()
}

// Withdraw is retained as the pre-reap lifecycle boundary. Windows containment
// remains authoritative until Seal/Close releases the Job Object.
//
// Nothing is closed here. The job handle must stay open until the process has
// been verified and the group's verdict recorded, because the kernel keeps
// tracking membership through the handle regardless of whether the command
// leader has exited.
func (pg *windowsProcessGroup) Withdraw() {}

// Seal observes membership and closes the Job Object while holding the same
// lock used by Terminate, State, Track, and Close. Close is kill-on-close, so
// the returned state is the last truthful observation before that action; a
// post-close query would only report that the handle is unavailable.
func (pg *windowsProcessGroup) Seal() (SealObservation, error) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	state := GroupUnavailable
	identity := "unknown"
	var stateErr error
	if pg.job != nil {
		identity = "job object"
		if !pg.tracked {
			state = GroupEmpty
		} else {
			var counters winjob.Counters
			if err := pg.job.QueryCounters(&counters); err != nil {
				stateErr = err
			} else if counters.ActiveProcesses == 0 {
				state = GroupEmpty
			} else {
				state = GroupOwned
			}
		}
	}
	if pg.job == nil {
		return SealObservation{State: state, Identity: identity}, pg.createErr
	}
	closeErr := pg.job.Close()
	pg.job = nil
	return SealObservation{State: state, Identity: identity}, errors.Join(stateErr, closeErr)
}

// State reports what the kernel knows about the job.
func (pg *windowsProcessGroup) State() GroupState {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.job == nil {
		return GroupUnavailable
	}
	var counters winjob.Counters
	if err := pg.job.QueryCounters(&counters); err != nil {
		// A handle that can no longer be queried proves nothing about the tree.
		return GroupUnavailable
	}
	if counters.ActiveProcesses == 0 {
		return GroupEmpty
	}
	return GroupOwned
}

func (pg *windowsProcessGroup) Identify() string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.job == nil {
		return "unknown"
	}
	return "job object"
}

func (pg *windowsProcessGroup) Close() error {
	// Closing the last handle terminates anything still in the job, which is the
	// backstop for a tree that outlived its own teardown. The lock is held across
	// the release, so a concurrent Terminate or State is either entirely before
	// it or entirely after it, and never mid-use of a freed handle.
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.job == nil {
		return nil
	}
	err := pg.job.Close()
	pg.job = nil
	return err
}

// lastProbeFailure returns why the job handle could not be queried, or nil.
//
// Windows answers from the kernel's own job counters rather than a probe, so
// there is no probe error to report; the accessor exists so the shared verdict
// reporter does not need to know which mechanism a platform uses.
func lastProbeFailure() error { return nil }

// lastTrackFailure returns why containment could not be established, or nil.
func lastTrackFailure() error { return nil }

// RecoverGroups has nothing to recover on Windows: every managed tree runs in a
// kill-on-close Job Object that ended with the daemon, and no process groups
// are recorded. Any group passed in is reported unknown and nothing is
// signalled.
func RecoverGroups(groups []daemon.Group, _ daemon.Host, _ RecoveryMode, _ time.Time) []RecoveredGroup {
	out := make([]RecoveredGroup, len(groups))
	for i, g := range groups {
		out[i] = RecoveredGroup{GroupResult: GroupResult{Name: g.Name, PGID: g.PGID, State: GroupUnavailable}, LeaderStart: g.LeaderStart, Outcome: OutcomeUnknown, Reason: "signal_unavailable"}
	}
	return out
}

// assessCrashedRun keeps the pre-record behaviour on Windows: a live daemon
// refuses the start, and any other PID file is stale, because its managed
// trees ended with the daemon's kill-on-close Job Objects.
func assessCrashedRun(pidPath string) (stale bool, err error) {
	if alive, pid := daemon.IsAliveFromPIDFile(pidPath); alive {
		return false, alreadyRunningError(pid)
	}
	_, statErr := os.Stat(pidPath)
	return statErr == nil, nil
}
