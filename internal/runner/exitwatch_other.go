//go:build !linux && !darwin && !windows

package runner

// observeLeaderExitPlatform reports that this platform cannot observe the
// command leader's exit without reaping it.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// platform matrix in docs/platforms.md, the release build matrix and the CI test matrix cover
// exactly those three.
//
// The error is fatal for the launch, not a fallback to a reap-based detection
// of the exit. Such a detection would race the reap against a cancellation's
// group signal, and a group signalled after its leader was reaped may reach an
// unrelated group that reused the identifier. Every launch therefore fails on
// these platforms.
//
// The failure leaves nothing running, though. It says nothing about the
// leader, which nothing has reaped yet, so the caller kills the whole process
// group while its identifier is still reserved, then withdraws authority and
// reaps the leader. The group's membership cannot be enumerated here, so it is
// never proven empty, and the process is reported as unverified and not
// restarted. docs/platforms.md records these platforms as unsupported.
func observeLeaderExitPlatform(pid int) (exitObserver, error) {
	return nil, errObservationUnsupported
}
