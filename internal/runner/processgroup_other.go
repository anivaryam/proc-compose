//go:build !linux && !darwin && !windows

package runner

import "syscall"

// groupMembers cannot enumerate process-group membership on this platform.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// platform matrix in docs/platforms.md, the release build matrix and the CI test matrix all
// cover exactly those three. Reaching this file means the tree was cross
// compiled for a platform outside the support contract, so there is no released
// behaviour to preserve here and the honest answer is that membership is
// unknown.
//
// Returning false for the second return value makes liveMembers treat the group
// as populated, which makes escalation wait out the full budget rather than skip
// it. Shutdown still terminates the group; it just cannot shorten the wait, and
// the platform is not one proc-compose claims to support.
func groupMembers(pgid int) (int, bool) {
	pids, ok := enumerateGroupMembers(pgid)
	return len(pids), ok
}

// groupMemberPIDs cannot enumerate membership here either; see groupMembers.
func groupMemberPIDs(int) ([]int, bool) { return nil, false }

// probeEPERMProvesMember follows POSIX, where EPERM means a process exists
// that the caller may not signal. Membership is never known here, so it only
// keeps groupProvenEmpty consistent.
const probeEPERMProvesMember = true

// recoveryMemberPIDs cannot enumerate membership here either; see groupMembers.
func recoveryMemberPIDs(int) ([]int, bool) { return nil, false }

// recoveryGroupHolders cannot enumerate membership here either; see groupMembers.
func recoveryGroupHolders(int) (int, bool) { return 0, false }

// procfsIsOwn is true: nothing is enumerated here, so nothing can disagree.
func procfsIsOwn() bool { return true }

// openPidfd is unavailable outside Linux, so crash recovery never signals here.
func openPidfd(int) (int, error) { return -1, errSignalUnavailable }

// sendPidfd is unavailable outside Linux; see openPidfd.
func sendPidfd(int, syscall.Signal) error { return errSignalUnavailable }
