//go:build darwin

package runner

import "golang.org/x/sys/unix"

// pidIsZombie reports whether pid has exited but has not been reaped yet. A
// zombie still answers signal 0, so liveness alone would count it as running.
func pidIsZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp.Proc.P_stat == szomb
}
