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
// observer polls with WNOHANG, so it tells an exit from "nothing yet" by si_pid,
// which must therefore be read at Darwin's offset (see waitSiginfo). Darwin's
// waitid may also report a stopped child with only WEXITED requested; a spurious
// observation for a stopped child would make the later reap fail loudly rather
// than silently corrupt signalling authority.
func observeLeaderExitPlatform(pid int) (exitObserver, error) {
	obs := &reapFreeObserver{pid: pid}
	if err := obs.probe(); err != nil {
		return nil, err
	}
	return obs, nil
}

// waitSiginfo is Darwin's siginfo_t, laid out so that si_pid can be read.
//
// Unlike Linux, Darwin has no padding before si_pid: it is at offset 12, and
// offset 16 is si_uid. With Linux's layout, macOS CI (run 38306358474) never
// observed an exit, so every naturally exiting command ran until its deadline.
// The layout matches the Go
// runtime's own definition in runtime/defs_darwin_amd64.go and
// defs_darwin_arm64.go (Go 1.26.8). The buffer is 128 bytes, larger than
// Darwin's 104-byte siginfo_t.
type waitSiginfo struct {
	Signo  int32
	Errno  int32
	Code   int32
	Pid    int32
	Uid    uint32
	Status int32
	_      [104]byte
}
