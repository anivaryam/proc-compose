//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func setDetachAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// IsAlive returns true if the process with the given PID is running.
func IsAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// StopProcess sends SIGTERM to the given process for graceful shutdown.
func StopProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// KillProcess force-kills the process (SIGKILL on Unix, taskkill /F on Windows).
func KillProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGKILL)
}

// procStartTime returns a stable, reuse-resistant identifier for a running
// process. On Linux it returns field 22 (start-time in jiffies) from
// /proc/<pid>/stat. On other Unixes (no /proc) it returns 0 with a nil error
// so callers fall back to PID-only liveness checks.
func procStartTime(pid int) (uint64, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// /proc/<pid>/stat: "<pid> (comm) state ppid ..." — the comm field can
	// contain spaces and parentheses, so split on the LAST ')' to skip it.
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 || close+2 > len(data) {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	rest := strings.Fields(string(data[close+2:]))
	// After the (comm) field, fields are 3..N; field 22 (1-indexed in proc(5))
	// is starttime, which is index 19 in this 0-based slice (3 + 19 = 22).
	if len(rest) < 20 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat field count", pid)
	}
	v, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}
