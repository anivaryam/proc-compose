//go:build windows

package daemon

import (
	"errors"
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
	return creationTime(h)
}

// procRunningStart is procStartTime for StartProven, which must also know
// whether the process still runs: an exited process stays openable, with the
// same creation time, while any handle to it is open. Only a wait that times
// out at once proves it still runs; the exit code cannot, because a process
// may exit with STILL_ACTIVE's value.
func procRunningStart(pid int) (start uint64, running bool, err error) {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION|syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return 0, false, err
	}
	defer syscall.CloseHandle(h)
	if start, err = creationTime(h); err != nil {
		return 0, false, err
	}
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return 0, false, err
	}
	return start, event == syscall.WAIT_TIMEOUT, nil
}

// creationTime is the creation time of the process h, from GetProcessTimes.
func creationTime(h syscall.Handle) (uint64, error) {
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}

// ProcStat is Linux-only.
func ProcStat(int) (ProcInfo, error) { return ProcInfo{}, errors.ErrUnsupported }

// syncDirectory is a no-op on Windows, where a directory handle cannot be
// flushed the way a Unix directory can. The write itself is unaffected: the
// durability step is best-effort.
func syncDirectory(string) error { return nil }

// HostIdentity has nothing to report on Windows, which records no process
// groups.
func HostIdentity() Host { return Host{} }
