//go:build windows

package runner

import "time"

// On Windows the runner does not need to observe the command leader's exit in
// order to keep authority over its tree.
//
// A Job Object is a durable kernel object: the kernel tracks membership
// independently of any process, so the leader exiting does not reduce the
// runner's authority over the children it left behind. There is no identifier
// to keep reserved and therefore nothing to observe, and no reap-ordering
// constraint to enforce. See processgroup_windows.go.

// durableContainment reports that this platform's containment keeps its
// authority across the command leader's exit.
//
// Windows is the only platform where that is true, and it is what tells the
// shared lifecycle code that a natural completion must run teardown rather than
// fall back to the Unix "exit observation unavailable" path.
const durableContainment = true

// observeLeaderExitPlatform reports that Windows needs no exit observation: a
// Job Object's membership is tracked by the kernel independently of any process,
// so the runner's authority does not depend on the leader's state.
//
// A nil observer on Windows means "no observation is needed", not "observation
// is unavailable". The shared code distinguishes the two via durableContainment.
func observeLeaderExitPlatform(pid int) (exitObserver, error) { return nil, nil }

// signallingEstablished reports whether the group's tree can still be
// signalled soundly.
//
// A job handle stays authoritative for as long as it is open, so the answer does
// not depend on the observation: even a leader that has exited and been reaped
// leaves live members tracked by the kernel. Durable containment is exactly what
// makes this unconditional.
func signallingEstablished(observation exitObservation, cancelled bool) bool { return true }

// terminateGroup terminates the job and waits for it to drain.
//
// Windows has no SIGTERM for console applications, so terminating the job is the
// whole teardown: it reaches every member at once, including members whose
// parent has already exited.
func terminateGroup(pg ProcessGroup, grace time.Duration) {
	_ = pg.Kill()
	waitWhileLive(pg, killSettle)
}
