//go:build !linux && !darwin && !windows

package runner

import "errors"

// errObservationUnsupported reports that this platform has no way to observe the
// command leader's exit without reaping it.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// platform matrix in docs/platforms.md, the release build matrix and the CI test matrix cover
// exactly those three.
//
// An observation failure is fatal here, not a fallback. The caller withdraws
// signalling authority, kills the leader it owns by its own handle, and
// returns an error: there is no reap-based detection of the exit to fall back
// to, and terminating a process group the runner cannot prove it owns would
// be unsound. Every launch therefore fails on these platforms.
var errObservationUnsupported = errors.New("exit observation without reaping is not supported on this platform")

// newPlatformExitObserver reports that this platform cannot observe the command
// leader's exit without reaping it.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// platform matrix in docs/platforms.md, the release build matrix and the CI test matrix cover
// exactly those three.
//
// The error is fatal for the launch: the caller has no reap-based fallback, so
// every command fails here. docs/platforms.md records these platforms as
// unsupported.
func observeLeaderExitPlatform(pid int) (exitObserver, error) {
	return nil, errObservationUnsupported
}
