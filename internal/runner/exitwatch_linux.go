//go:build linux

package runner

import "golang.org/x/sys/unix"

// pidIDType is waitid's P_PID: wait only for the process with the given pid.
const pidIDType = unix.P_PID

// observeLeaderExitPlatform observes the command leader's exit with
// waitid(WNOWAIT), which reports the exit while leaving the leader a zombie and
// therefore still holding its process-table slot.
//
// golang.org/x/sys/unix exposes waitid directly on Linux, and the standard
// library's own os.Process uses this same call in os/wait_waitid.go, so the
// primitive is the same one the runtime already relies on.
func observeLeaderExitPlatform(pid int) (exitObserver, error) {
	obs := &reapFreeObserver{pid: pid}
	if err := obs.probe(); err != nil {
		return nil, err
	}
	return obs, nil
}
