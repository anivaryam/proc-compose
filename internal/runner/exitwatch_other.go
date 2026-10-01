//go:build !linux && !darwin && !windows

package runner

import "errors"

// errObservationUnsupported reports that this platform has no way to observe the
// command leader's exit without reaping it.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// README platform matrix, the release build matrix and the CI test matrix cover
// exactly those three. Nothing here is a released behaviour for another platform,
// so the caller falls back to detecting exit by reaping — which is what
// proc-compose has always done — rather than terminating process groups it cannot
// prove it owns.
var errObservationUnsupported = errors.New("exit observation without reaping is not supported on this platform")

// newPlatformExitObserver reports that this platform cannot observe the command
// leader's exit without reaping it.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// README platform matrix, the release build matrix and the CI test matrix cover
// exactly those three. Nothing here is a released behaviour for another
// platform, so the caller falls back to detecting exit by reaping — which is
// what proc-compose has always done — rather than terminating process groups it
// cannot prove it owns.
func observeLeaderExitPlatform(pid int) (exitObserver, error) {
	return nil, errObservationUnsupported
}
