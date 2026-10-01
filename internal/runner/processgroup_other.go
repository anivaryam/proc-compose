//go:build !linux && !darwin && !windows

package runner

// groupMembers cannot enumerate process-group membership on this platform.
//
// proc-compose ships, tests and documents Linux, macOS and Windows only: the
// README platform matrix, the release build matrix and the CI test matrix all
// cover exactly those three. Reaching this file means the tree was cross
// compiled for a platform outside the support contract, so there is no released
// behaviour to preserve here and the honest answer is that membership is
// unknown.
//
// Returning false for the second return value makes liveMembers treat the group
// as populated, which makes escalation wait out the full budget rather than skip
// it. Shutdown still terminates the group; it just cannot shorten the wait, and
// the platform is not one proc-compose claims to support.
func groupMembers(pgid int) (int, bool) { return 0, false }
