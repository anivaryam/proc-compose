//go:build linux

package runner

import (
	"os"
	"strconv"
	"syscall"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"golang.org/x/sys/unix"
)

// groupMembers counts the live members of a process group by walking procfs.
//
// It reports false for the second return value when the count could not be
// established, so that callers skip escalation rather than assume a group is
// empty on incomplete information.
func groupMembers(pgid int) (int, bool) {
	pids, ok := enumerateGroupMembers(pgid)
	return len(pids), ok
}

// groupMemberPIDs lists the live members of a process group by walking procfs;
// groupMembers counts over it.
//
// A member is live unless its /proc state is a termination state. cmd.Wait
// reaps the command leader only, so a killed descendant whose new parent has not
// collected it stays in procfs as a zombie; treating those as live would turn a
// successful shutdown into a reported failure.
func groupMemberPIDs(pgid int) ([]int, bool) {
	return walkGroupMembers(pgid, func(_ int, state byte) bool { return !isZombieState(state) })
}

// recoveryMemberPIDs is the membership walk crash recovery proves emptiness
// and pins members with. It differs from groupMemberPIDs in one way: a zombie
// state alone does not make a member dead, because a thread-group leader whose
// main thread exited reports Z while its other threads still run; see
// recoveryLive.
func recoveryMemberPIDs(pgid int) ([]int, bool) { return walkGroupMembers(pgid, recoveryLive) }

// recoveryGroupHolders counts every visible process in pgid, dead or alive.
func recoveryGroupHolders(pgid int) (int, bool) {
	pids, ok := walkGroupMembers(pgid, func(int, byte) bool { return true })
	return len(pids), ok
}

// probeEPERMProvesMember is true: Linux answers kill(-pgid, 0) with EPERM only
// when the group has a member and this process may signal none of them.
const probeEPERMProvesMember = true

// walkGroupMembers lists the members of pgid that live reports as live. A
// process whose procfs entry is hidden (hidepid) or unreadable is skipped, so
// an empty result alone does not prove another user's process absent.
func walkGroupMembers(pgid int, live func(pid int, state byte) bool) ([]int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var members []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		gid, state, ok := procStat(pid)
		if !ok || gid != pgid {
			continue
		}
		if live(pid, state) {
			members = append(members, pid)
		}
	}
	return members, true
}

// procStat returns a process's group ID and its single-letter state.
func procStat(pid int) (pgid int, state byte, ok bool) {
	info, err := daemon.ProcStat(pid)
	if err != nil {
		return 0, 0, false
	}
	return info.PGID, info.State, true
}

// procfsIsOwn reports whether the mounted /proc numbers processes as this
// process's pid namespace does. Crash recovery proves identity by reading
// /proc but signals through pidfd_open, which resolves a pid in the caller's
// namespace; under `unshare --pid` without a fresh /proc mount the two name
// different processes, so recovery must not act on what /proc says.
func procfsIsOwn() bool {
	self, err := os.Readlink("/proc/self")
	return err == nil && self == strconv.Itoa(os.Getpid())
}

// openPidfd returns a pidfd for pid: a handle that keeps naming that exact
// process, so a signal sent through it cannot reach a later owner of the pid.
func openPidfd(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

// sendPidfd signals the process a pidfd names. Signal 0 checks that the process
// has not been reaped, which is what lets a procfs read taken in between be
// attributed to it.
func sendPidfd(fd int, sig syscall.Signal) error { return unix.PidfdSendSignal(fd, sig, nil, 0) }
