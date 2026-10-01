//go:build !windows

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
)

type unixProcessGroup struct {
	mu sync.Mutex
	// pgid is the command's process-group ID, or 0 once the group is closed.
	// Setpgid makes the leader's PID equal the group ID, which is what makes
	// the identifier ours to signal.
	pgid int
	// withdrawn ends signalling authority. It is set before the leader is
	// reaped and is never cleared.
	withdrawn bool
	// trackErr records a containment failure reported by Track.
	trackErr error
}

func newProcessGroup() ProcessGroup { return &unixProcessGroup{} }

func (pg *unixProcessGroup) Setup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return pg.Terminate() }
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = defaultShutdownGrace
	}
}

func (pg *unixProcessGroup) Track(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("track: process not started")
	}
	pg.mu.Lock()
	defer pg.mu.Unlock()
	pg.pgid = cmd.Process.Pid
	return nil
}

// signal sends sig to the whole process group.
//
// The ownership check and the kill(2) run under one lock, and Withdraw takes the
// same lock, so a signal either completes entirely before authority is withdrawn
// — while the leader is still unreaped and the identifier cannot have been
// reassigned — or is refused afterwards. pid 0 and -1 would mean "my process
// group" and "every process we may signal", so they are never sent.
func (pg *unixProcessGroup) signal(sig syscall.Signal) error {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.withdrawn || pg.pgid <= 1 {
		return os.ErrProcessDone
	}
	return syscall.Kill(-pg.pgid, sig)
}

func (pg *unixProcessGroup) Terminate() error { return pg.signal(syscall.SIGTERM) }
func (pg *unixProcessGroup) Kill() error      { return pg.signal(syscall.SIGKILL) }

// Withdraw ends signalling authority. One-way, and called before the reap.
func (pg *unixProcessGroup) Withdraw() {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	pg.withdrawn = true
}

func (pg *unixProcessGroup) State() GroupState {
	pg.mu.Lock()
	pgid := pg.pgid
	pg.mu.Unlock()

	if pgid <= 1 {
		return GroupEmpty
	}
	if pg.trackErr != nil {
		return GroupUnavailable
	}
	// Absence is established by enumerating the group and finding nothing running,
	// not by asking kill(2) whether the group still exists.
	//
	// That distinction is measured, not stylistic. kill(-pgid, 0) returns EPERM on
	// macOS for a group the kernel does not know — the errno is reported in the
	// verdict as "operation not permitted" — so keying absence on ESRCH marked
	// every macOS group unverifiable, and keying it on any probe failure at all
	// would read EPERM as proof of termination. The probe is therefore only ever
	// allowed to ADD a conclusion: a success proves something is there, and a
	// failure contributes nothing.
	members, known := processGroupMembers(pgid)
	if !known {
		// Membership could not be read, so nothing can be established either way.
		// Fall back to the probe: a live group is still evidence of one.
		if groupKill(-pgid, 0) == nil {
			return GroupOwned
		}
		probeErr := fmt.Errorf("process group membership could not be enumerated")
		groupProbeFailure.Store(&probeErr)
		return GroupUnavailable
	}
	if members > 0 {
		return GroupOwned
	}
	// No member is running. A group whose only remaining members are terminated
	// but unreaped is not a survivor, which is why this is decided by state rather
	// than by the leader's presence.
	return GroupEmpty
}

// groupProbeFailure records why the last existence probe could not be answered,
// so State can explain an unverifiable verdict. It is written before State returns
// and read immediately after, under no lock, because a group is only probed by the
// goroutine that owns its teardown.
var groupProbeFailure atomic.Pointer[error]

// lastProbeFailure returns the probe error that produced the most recent
// GroupUnavailable, or nil.
func lastProbeFailure() error {
	if p := groupProbeFailure.Load(); p != nil {
		return *p
	}
	return nil
}

// Identify returns the group's process-group ID for operator-facing messages.
func (pg *unixProcessGroup) Identify() string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.pgid <= 1 {
		return "unknown"
	}
	return identifyPID(pg.pgid)
}

// Close releases the group's identity.
func (pg *unixProcessGroup) Close() error {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	pg.pgid = 0
	return nil
}

// groupTrackFailure records why Track could not establish containment.
var groupTrackFailure atomic.Pointer[error]

// lastTrackFailure returns the most recent Track error, or nil.
func lastTrackFailure() error {
	if p := groupTrackFailure.Load(); p != nil {
		return *p
	}
	return nil
}

// groupKill is the existence probe a group's state is derived from. It is
// indirected so the probe can be exercised without arranging a real unsignalable
// group, which an unprivileged test cannot do.
var groupKill = syscall.Kill

// liveMembers reports whether the process group still has a member that is
// running rather than terminated.
//
// A terminated-but-unreaped descendant does not count. cmd.Wait reaps the
// command leader only, so a descendant that outlived it and was then killed is
// reparented to init and may still be an unreaped zombie for a while; counting
// it would turn a successful shutdown into a reported failure.
func liveMembers(pgid int) bool {
	members, known := processGroupMembers(pgid)
	if !known {
		// Membership could not be enumerated. Assume members are present so
		// escalation is never skipped on incomplete information.
		return true
	}
	return members > 0
}

// processGroupMembers returns the number of live members of a process group and
// whether that count could be established at all.
func processGroupMembers(pgid int) (int, bool) { return groupMembers(pgid) }

// numericID returns the group's process-group ID, or 0 when there is none.
func (pg *unixProcessGroup) numericID() int {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.pgid <= 1 {
		return 0
	}
	return pg.pgid
}
