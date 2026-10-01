//go:build darwin

package runner

// pidIDType is waitid's P_PID.
//
// x/sys/unix exports P_PID for Linux but not for Darwin, and the value is fixed
// by the kernel ABI: P_ALL is 0 and P_PID is 1. waitid(2) on Darwin is reachable
// through SYS_WAITID, which x/sys does export here.
const pidIDType = 1

// observeLeaderExitPlatform observes the command leader's exit with
// waitid(WNOWAIT), the same primitive Linux uses.
//
// This replaced a kqueue EVFILT_PROC implementation. Two macOS CI runs showed that
// approach cannot be made reliable without an assumption that does not hold:
//
//  1. XNU's filt_procattach documents EVFILT_PROC as edge-triggered at attach, so
//     a knote registered against an already-exited process never fires.
//  2. Covering that needs a process-table lookup, and both obvious forms of it
//     failed: kern.proc.pid answers ESRCH for a zombie, and a kern.proc.all scan
//     did not match a running leader either.
//
// waitid(WNOWAIT) needs no such lookup: it observes the exit directly and leaves
// the leader waitable, so a later cmd.Wait still reports a real exit status. The
// only Darwin-specific concern left is that waitid may report a stopped child even
// with only WEXITED requested; Observe ignores the payload and returns on any
// successful return, and a spurious return for a stopped child would make the
// later reap fail loudly rather than silently corrupt signalling authority.
func observeLeaderExitPlatform(pid int) (exitObserver, error) {
	obs := &reapFreeObserver{pid: pid}
	if err := obs.probe(); err != nil {
		return nil, err
	}
	return obs, nil
}
