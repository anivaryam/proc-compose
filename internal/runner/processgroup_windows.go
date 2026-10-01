//go:build windows

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kolesnikovae/go-winjob"
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
}

func newProcessGroup() ProcessGroup {
	job, err := winjob.Create("", winjob.WithKillOnJobClose())
	return &windowsProcessGroup{job: job, createErr: err}
}

func (pg *windowsProcessGroup) Setup(cmd *exec.Cmd) {
	// cmd.Cancel must be set before cmd.Start; assigning it later is a silent
	// no-op and falls back to os.Process.Kill, bypassing the Job Object.
	cmd.Cancel = func() error {
		return pg.Kill()
	}
	cmd.WaitDelay = waitDelay
}

// Track adds the started command to the job.
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
	if err := pg.job.Assign(cmd.Process); err != nil {
		return fmt.Errorf("track: assign to job object: %w", err)
	}
	pg.tracked = true
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
		// is the same outcome an untracked Unix group reports.
		return os.ErrProcessDone
	}
	return pg.job.Terminate()
}

// Withdraw records that the runner has finished signalling.
//
// Nothing is closed here. The job handle must stay open until the process has
// been verified and the group's verdict recorded, because the kernel keeps
// tracking membership through the handle regardless of whether the command
// leader has exited.
func (pg *windowsProcessGroup) Withdraw() {}

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
