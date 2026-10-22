//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"

	"github.com/anivaryam/proc-compose/internal/daemon"
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
	// probeErr records why the group's membership could not be read. It is
	// the group's own reason: it is set when an enumeration fails and cleared
	// when one succeeds, so a group whose state was established reports no
	// reason.
	probeErr error
	// members records the live members of the group's last enumeration. It
	// describes the observation that produced the current state: a failed
	// enumeration clears it, so the verdict never names a stale enumeration.
	members []string
}

func newProcessGroup() ProcessGroup { return &unixProcessGroup{} }

// Setup keeps any attributes the caller set and forces only the ones the group
// model depends on: the command must lead a new process group whose ID is its
// own PID, because Track records the PID as the group ID and every signal is
// sent to it.
//
//   - Pgid is forced to 0. Any other value joins an existing group, whose ID is
//     not the command's PID: signals would go to a group the command is not in
//     and be answered with ESRCH, which reads as "nothing left to signal".
//   - Setsid is kept. setsid(2) already makes the command the leader of a new
//     group whose ID is its own PID, and setpgid(2) on a session leader fails
//     with EPERM, so the group flag is set only without it. A caller's Setctty
//     needs Setsid, so it keeps working.
func (pg *unixProcessGroup) Setup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pgid = 0
	cmd.SysProcAttr.Setpgid = !cmd.SysProcAttr.Setsid
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
//
// ESRCH is reported as os.ErrProcessDone: the group is gone, so there is
// nothing left to signal. EPERM is a different condition — members exist but
// are not ours to signal — and stays distinct.
func (pg *unixProcessGroup) signal(sig syscall.Signal) error {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.withdrawn || pg.pgid <= 1 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pg.pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func (pg *unixProcessGroup) Terminate() error { return pg.signal(syscall.SIGTERM) }
func (pg *unixProcessGroup) Kill() error      { return pg.signal(syscall.SIGKILL) }

// Withdraw ends signalling authority. One-way, and called before the reap.
func (pg *unixProcessGroup) Withdraw() {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	pg.withdrawn = true
}

// Seal withdraws signal authority before observing and releasing the group.
// Withdraw and every signal share mu, so the observation cannot race a signal
// that was admitted before the seal.
func (pg *unixProcessGroup) Seal() (SealObservation, error) {
	pg.Withdraw()
	state := pg.State()
	observation := SealObservation{
		State:    state,
		Identity: pg.Identify(),
		Members:  pg.memberPIDs(),
		Reason:   pg.failureReason(),
	}
	return observation, pg.Close()
}

func (pg *unixProcessGroup) State() GroupState {
	pg.mu.Lock()
	pgid := pg.pgid
	trackErr := pg.trackErr
	pg.mu.Unlock()

	if pgid <= 1 {
		return GroupEmpty
	}
	if trackErr != nil {
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
	pids, known := enumerateGroupMembers(pgid)
	if !known {
		// Membership could not be read, so nothing can be established either way.
		// Fall back to the probe: a live group is still evidence of one.
		pg.recordObservation(nil, fmt.Errorf("process group membership could not be enumerated"))
		if groupKill(-pgid, 0) == nil {
			return GroupOwned
		}
		return GroupUnavailable
	}
	pg.recordObservation(pids, nil)
	if len(pids) > 0 {
		return GroupOwned
	}
	// No member is running. A group whose only remaining members are terminated
	// but unreaped is not a survivor, which is why this is decided by state rather
	// than by the leader's presence.
	return GroupEmpty
}

// recordObservation stores the members of one enumeration and the reason it
// could not be made.
//
// The record describes the observation that produced the current state: a
// failed enumeration clears the members, so the verdict reports that the
// members could not be enumerated instead of naming a stale list, and a
// successful enumeration clears the reason, so a group whose state was
// established reports no reason at all.
func (pg *unixProcessGroup) recordObservation(pids []int, err error) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	pg.probeErr = err
	if err != nil {
		pg.members = nil
		return
	}
	pg.members = make([]string, 0, len(pids))
	for _, pid := range pids {
		pg.members = append(pg.members, identifyPID(pid))
	}
}

// memberPIDs returns the live members of the group's last enumeration, or nil
// when that enumeration failed.
func (pg *unixProcessGroup) memberPIDs() []string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return slices.Clone(pg.members)
}

// failureReason returns why the group's containment could not be established,
// or "" when the group has no reason of its own.
func (pg *unixProcessGroup) failureReason() string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.probeErr == nil {
		return ""
	}
	return pg.probeErr.Error()
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

// enumerateGroupMembers lists the live members of a process group.
//
// It is indirected so a test can force an enumeration failure and observe
// what the runner then reports. The platform implementations provide the real
// walk.
var enumerateGroupMembers = groupMemberPIDs

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

// isZombieState reports whether a procfs state letter means the process has
// terminated. It reads daemon.ProcStat states, which only Linux provides. Z is the ordinary zombie state; X and x are the dead and
// in-death-transit states used during exit. daemon.ExitedState is the one
// definition, shared with daemon.StartProven.
func isZombieState(state byte) bool {
	return daemon.ExitedState(state)
}
