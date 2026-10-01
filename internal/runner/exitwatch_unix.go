//go:build !windows

package runner

import "time"

// durableContainment reports whether this platform's containment keeps its
// authority across the command leader's exit.
//
// It is false everywhere on Unix: authority depends on the leader being
// unreaped, because an unreaped leader keeps its process-table slot and
// therefore its PID — and the process-group ID equal to it — reserved. Windows
// reports true, because a Job Object is tracked independently of any process.
//
// The shared lifecycle code needs this to tell "this platform needs no exit
// observation" apart from "exit observation is unavailable here"; without it a
// Windows natural completion would be misread as an unavailable observation and
// would skip teardown even though its Job Object is authoritative.
const durableContainment = false

// signallingEstablished reports whether the group's tree can still be signalled
// soundly.
//
// On Unix, authority is held while the leader is unreaped: it occupies a
// process-table slot, so its PID — and the process-group ID that equals it —
// cannot be reassigned to an unrelated group. The runner performs the only
// reap, so on cancellation that holds whether or not the leader has exited,
// including while it is still running. After an observed exit it holds as well,
// because a zombie keeps its slot until a wait collects it.
//
// It is lost only when observation failed, because then the leader's waitable
// state is unknown and its identifier may already have been released. An exit
// observed only by reaping is exactly that case.
func signallingEstablished(observation exitObservation, cancelled bool) bool {
	if observation == exitOwnershipLost {
		return false
	}
	if observation == exitUnsupported {
		// The exit was seen only by reaping, so a natural completion has already
		// released the leader's process-table slot and the identifier is no
		// longer reserved. A cancellation reaches this line before any reap, so
		// the leader is still unreaped and the group is still ours to signal.
		return cancelled
	}
	return cancelled || observation == exitObserved
}

// terminateGroup asks the group to stop and escalates only while members remain.
//
// It sends graceful termination first even when the leader exited on its own,
// because the members left behind are the command's own descendants and are
// entitled to a graceful shutdown rather than an immediate kill. It returns
// without waiting when the group already has no live members, so a successful
// task with no descendants does not pay the grace and settle budgets.
//
// Membership inspection only decides how long to wait. It never decides whether
// signalling is permitted: that comes from signallingEstablished, because only
// the reserved leader proves the identifier is ours.
//
// The caller must hold signalling authority.
func terminateGroup(pg ProcessGroup, grace time.Duration) {
	if pg.State() != GroupOwned {
		return
	}
	_ = pg.Terminate()
	if waitWhileLive(pg, grace) {
		return
	}
	// SIGKILL cannot be caught, so anything still live after the settle window is
	// unkillable or untracked, and is reported by the caller.
	_ = pg.Kill()
	waitWhileLive(pg, killSettle)
}
