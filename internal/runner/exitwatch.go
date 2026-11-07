package runner

import (
	"errors"
	"time"
)

// exitObservation is what an exit observer concluded about the command leader.
//
// On Unix the distinction that matters is whether the runner may still signal the
// command's process group. It may, exactly while the leader is unreaped: the
// kernel keeps a zombie process-table slot reserved until a wait(2) collects it,
// and because the leader's PID equals the process-group ID, that identifier
// cannot be handed to an unrelated group while the slot exists.
//
// Windows never needs an observation; its Job Object holds its authority for as
// long as the handle is open.
type exitObservation int

const (
	// exitPending means no observation has completed yet.
	exitPending exitObservation = iota

	// exitObserved means the leader exited and has not been reaped. Its
	// process-table slot, and therefore the group identifier, is still ours.
	exitObserved

	// exitOwnershipLost means the leader's waitable state could not be
	// established, so its identifier may already have been released. No group
	// signal may be sent on the strength of this observation.
	exitOwnershipLost

	// exitUnsupported means this platform cannot observe the leader's exit
	// without reaping it.
	//
	// The value is unreachable on the supported platforms. Linux and macOS
	// observe the exit without reaping it, Windows needs no observation at
	// all, and on a platform that cannot observe it the launch fails with an
	// error: an observation failure is fatal, not a fallback to a
	// reap-based detection. The branch is kept so the contract stays
	// explicit.
	exitUnsupported
)

func (o exitObservation) String() string {
	switch o {
	case exitPending:
		return "not observed"
	case exitObserved:
		return "leader exited and was not reaped"
	case exitOwnershipLost:
		return "leader ownership could not be established"
	case exitUnsupported:
		return "exit observation is not supported on this platform"
	default:
		return "unknown"
	}
}

// exitObserver reports the command leader's exit without reaping it, so that
// signalling authority survives the leader's exit.
type exitObserver interface {
	// Observe blocks until the leader exits and reports what it concluded. It
	// returns when the leader exits on its own; stop is closed by the caller
	// when teardown has already delivered the signals, so a platform that
	// cannot wait for an exit it will never see can still return.
	Observe(stop <-chan struct{}) exitObservation
}

// observeLeaderExit sets up observation of the command leader's exit.
//
// A nil observer with a nil error means no observation was needed or is
// possible. Which of those it is depends on the platform, and the difference
// matters: durableContainment platforms need no observer because their
// containment survives the leader's exit, while elsewhere a nil observer means
// the exit can only be detected by reaping, which ends signalling authority.
//
// An error fails the launch. Whether the group may still be signalled first is
// leaderStillReserved's answer: a failed wait on the leader revokes signalling
// authority and must not lead to a group signal.
func observeLeaderExit(pid int) (exitObserver, error) { return observeLeaderExitPlatform(pid) }

// errObservationUnsupported reports that this platform has no way to observe
// the command leader's exit without reaping it. Only the platforms outside the
// support contract return it; see exitwatch_other.go.
var errObservationUnsupported = errors.New("exit observation without reaping is not supported on this platform")

// leaderStillReserved reports whether the leader is still provably unreaped
// after its exit could not be observed, so that its process-group ID is still
// ours to signal.
//
// That holds only when the platform merely lacks a reap-free observation. Such
// a failure says nothing about the leader, and nothing has collected it: the
// runner performs the only reap, and that reap has not happened yet. Any other
// failure is a failed wait on the leader itself — ECHILD means it is no longer
// a waitable child, so something else collected it — so its identifier may
// already have been released.
func leaderStillReserved(obsErr error) bool {
	return errors.Is(obsErr, errObservationUnsupported)
}

// groupPGID returns the group's numeric identifier, or 0 when it cannot be
// resolved. On Unix it is what an operator can act on when a shutdown reports a
// survivor.
func groupPGID(pg ProcessGroup) int {
	resolver, ok := pg.(interface{ numericID() int })
	if !ok {
		return 0
	}
	return resolver.numericID()
}

// memberPollInterval is how often liveness is re-checked while waiting out a
// teardown budget.
const memberPollInterval = 100 * time.Millisecond

// waitWhileLive polls until the group has no live members left or the budget is
// spent, reporting whether it drained.
//
// A group whose membership cannot be enumerated has not been proven empty, so
// it is polled until the budget is spent, exactly like a group that is still
// owned: treating "unknown" as "drained" would skip the escalation that
// follows this wait. Only an empty group ends the wait immediately.
func waitWhileLive(pg ProcessGroup, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if pg.State() == GroupEmpty {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(memberPollInterval)
	}
}
