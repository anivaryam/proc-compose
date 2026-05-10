//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func setDetachAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // CREATE_NO_WINDOW
	}
}

// IsAlive checks if a process is still running by attempting to open it.
func IsAlive(pid int) bool {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(h)
	return true
}

// StopProcess terminates the process tree using taskkill on Windows.
// Windows lacks SIGTERM for console apps started with CREATE_NEW_PROCESS_GROUP;
// taskkill /T /F is the closest reliable equivalent.
func StopProcess(proc *os.Process) error {
	return KillProcess(proc)
}

// KillProcess force-kills the process tree.
func KillProcess(proc *os.Process) error {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", proc.Pid))
	return kill.Run()
}

// procStartTime returns a process creation-time identifier from
// GetProcessTimes. The 64-bit creation time is unique per process invocation
// on the running system and survives across pidfile reads.
func procStartTime(pid int) (uint64, error) {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}
