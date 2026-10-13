//go:build !darwin && !windows

package runner

// pidIsZombie is darwin's zombie check. Elsewhere on Unix,
// pidIsTerminatedUnreaped reads the zombie state from /proc.
func pidIsZombie(int) bool { return false }
