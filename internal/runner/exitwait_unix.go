//go:build !windows

package runner

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// waitSiginfo is Linux's and Darwin's siginfo_t, laid out so that si_pid can be
// read.
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

// waitid performs waitid(2) with a waitSiginfo.
//
// The syscall writes raw bytes at the given address, so the caller must pass a
// buffer whose layout matches the kernel's siginfo_t; that is why this takes a
// waitSiginfo rather than unix.Siginfo.
func waitid(idType, id, options int, info *waitSiginfo) error {
	_, _, errno := unix.Syscall6(unix.SYS_WAITID,
		uintptr(idType), uintptr(id), uintptr(unsafe.Pointer(info)),
		uintptr(options), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// reapFreeObserver observes a command leader's exit with waitid(WNOWAIT), which
// reports the exit without collecting it: the leader stays a zombie, and
// therefore keeps its process-table slot — and its PID, and the process-group ID
// equal to it — reserved.
//
// WNOWAIT is documented as leaving the child waitable: "Leave the child in a
// waitable state; a later wait call can be used to again retrieve the child
// status information." That is what lets this be the same mechanism on every Unix
// platform, and what lets cmd.Wait still return a real exit status afterwards.
type reapFreeObserver struct {
	pid int
	// sawExit records that the non-blocking setup probe already observed the
	// exit, so Observe does not wait for an event that has already happened.
	sawExit bool
}

// probe reap-free exit without blocking, so a long-running service cannot delay
// its own start.
//
// WNOHANG makes waitid return immediately whether or not the command has exited.
// The probe distinguishes "no exit yet" from an observed exit by si_pid, which the
// kernel leaves at zero when nothing in the requested state has happened.
func (o *reapFreeObserver) probe() error {
	var info waitSiginfo
	var err error
	for {
		err = waitid(pidIDType, o.pid, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, &info)
		if err != unix.EINTR {
			break
		}
		info = waitSiginfo{}
	}
	if err == nil {
		o.sawExit = info.Pid != 0
		return nil
	}
	// ECHILD means the leader is not a waitable child, so it has already been
	// collected elsewhere and its identifier can no longer be relied on.
	return err
}

// Observe blocks until the leader exits, leaving it unreaped.
func (o *reapFreeObserver) Observe(stop <-chan struct{}) exitObservation {
	if o.sawExit {
		return exitObserved
	}

	var info waitSiginfo
	for {
		err := waitid(pidIDType, o.pid, unix.WEXITED|unix.WNOWAIT, &info)
		switch {
		case err == nil:
			return exitObserved
		case err == unix.EINTR:
			info = waitSiginfo{}
			continue
		default:
			// ECHILD means the leader is no longer waitable, which is the only
			// way this can fail in normal operation. Treat any failure the same
			// way: an observation that does not establish the leader as an
			// unreaped zombie must never authorise a group signal.
			return exitOwnershipLost
		}
	}
}
