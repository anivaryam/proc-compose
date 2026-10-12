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

// waitSiginfo is Linux's 64-bit siginfo_t, laid out so that si_pid can be read.
// Darwin's layout differs and is defined in exitwatch_darwin.go.
//
// unix.Siginfo exposes si_signo, si_errno and si_code but names neither si_pid
// nor the padding before it, so the field layout is spelled out here.
//
// si_pid is NOT at offset 12. Measured on linux/amd64 against a child that had
// exited but was not reaped, waitid(WEXITED|WNOWAIT|WNOHANG) returned:
//
//	offset  0: 17   si_signo  (SIGCHLD)
//	offset  4:  0   si_errno
//	offset  8:  2   si_code   (CLD_KILLED)
//	offset 12:  0   padding
//	offset 16: pid  si_pid
//	offset 20: uid  si_uid
//
// The same call against a still-running child returned all zeros, which is how
// setup distinguishes "no exit yet" from an observed exit without blocking.
type waitSiginfo struct {
	Signo int32
	Errno int32
	Code  int32
	_     int32
	Pid   int32
	Uid   int32
	_     [104]byte
}
