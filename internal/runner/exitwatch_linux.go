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

// is64bit is 1 on a 64-bit platform and 0 on a 32-bit one.
//
// Linux's siginfo_t places si_pid behind a pointer-sized amount of padding: the
// union that precedes it is 8 bytes wide on a 64-bit platform and 4 bytes wide
// on a 32-bit one. Go's own os/syscall uses the same rule.
const is64bit = ^uint(0) >> 63

// waitSiginfo is Linux's siginfo_t, laid out so that si_pid can be read.
// Darwin's layout differs and is defined in exitwatch_darwin.go.
//
// unix.Siginfo exposes si_signo, si_errno and si_code but names neither si_pid
// nor the padding before it, so the field layout is spelled out here.
//
// si_pid is NOT at a fixed offset. Measured on linux/amd64 against a child
// that had exited but was not reaped, waitid(WEXITED|WNOWAIT|WNOHANG)
// returned:
//
//	offset  0: 17   si_signo  (SIGCHLD)
//	offset  4:  0   si_errno
//	offset  8:  2   si_code   (CLD_KILLED)
//	offset 12:  0   padding
//	offset 16: pid  si_pid
//	offset 20: uid  si_uid
//
// On a 32-bit platform the same union is 4 bytes wide, so si_pid is at offset
// 12 and si_uid at offset 16 — the fields this struct reads would otherwise be
// swapped.
//
// The same call against a still-running child returned all zeros, which is how
// setup distinguishes "no exit yet" from an observed exit without blocking.
//
// The struct stays exactly 128 bytes on both widths: the kernel writes a full
// siginfo_t into the buffer, so a shorter struct would let it write past the
// end.
type waitSiginfo struct {
	Signo int32
	Errno int32
	Code  int32
	_     [is64bit]int32
	Pid   int32
	Uid   int32
	_     [128 - (5+is64bit)*4]byte
}
