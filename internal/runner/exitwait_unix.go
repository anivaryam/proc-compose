//go:build linux || darwin

package runner

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// waitid performs waitid(2) with a waitSiginfo.
//
// The syscall writes raw bytes at the given address, so the caller must pass a
// buffer whose layout matches the kernel's siginfo_t; that is why this takes a
// waitSiginfo, defined per platform, rather than unix.Siginfo.
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

// Observe polls for the leader's exit, leaving it unreaped. Polling makes stop
// effective so a sealed cleanup can join the observer before returning.
func (o *reapFreeObserver) Observe(stop <-chan struct{}) exitObservation {
	if o.sawExit {
		return exitObserved
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return exitPending
		case <-ticker.C:
			var info waitSiginfo
			for {
				err := waitid(pidIDType, o.pid, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, &info)
				if err == unix.EINTR {
					info = waitSiginfo{}
					continue
				}
				if err != nil {
					// ECHILD means the leader is no longer waitable. Any failure
					// cannot authorise group signalling.
					return exitOwnershipLost
				}
				if info.Pid != 0 {
					return exitObserved
				}
				break
			}
		}
	}
}
