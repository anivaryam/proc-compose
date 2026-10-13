//go:build !windows

package daemon

import (
	"errors"
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
// process: on Linux the procfs start time from ProcStat. On other Unixes (no
// /proc) it returns 0 with a nil error so callers fall back to PID-only
// liveness checks.
func procStartTime(pid int) (uint64, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	info, err := ProcStat(pid)
	if err != nil {
		return 0, err
	}
	return info.StartTime, nil
}

// ProcStat reads a process's state, process group and start time from
// /proc/<pid>/stat. It is the single parser of that file. Off Linux it
// returns errors.ErrUnsupported.
func ProcStat(pid int) (ProcInfo, error) {
	if runtime.GOOS != "linux" {
		return ProcInfo{}, errors.ErrUnsupported
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcInfo{}, err
	}
	// /proc/<pid>/stat: "<pid> (comm) state ppid pgrp ..." — the comm field can
	// contain spaces and parentheses, so split on the LAST ')' to skip it.
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 || close+2 > len(data) {
		return ProcInfo{}, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	rest := strings.Fields(string(data[close+2:]))
	// After the (comm) field, fields are 3..N (1-indexed in proc(5)): state is
	// index 0 here, pgrp (field 5) index 2, and starttime (field 22) index 19.
	if len(rest) < 20 || len(rest[0]) == 0 {
		return ProcInfo{}, fmt.Errorf("unexpected /proc/%d/stat field count", pid)
	}
	pgid, err := strconv.Atoi(rest[2])
	if err != nil {
		return ProcInfo{}, fmt.Errorf("unexpected /proc/%d/stat pgrp: %w", pid, err)
	}
	start, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return ProcInfo{}, fmt.Errorf("unexpected /proc/%d/stat starttime: %w", pid, err)
	}
	return ProcInfo{State: rest[0][0], PGID: pgid, StartTime: start}, nil
}

// HostIdentity returns the current boot id and the pid and time namespaces of
// this process. Values that cannot be read, and every value off Linux, are
// empty.
func HostIdentity() Host {
	if runtime.GOOS != "linux" {
		return Host{}
	}
	var h Host
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		h.BootID = strings.TrimSpace(string(data))
	}
	h.PIDNS, _ = os.Readlink("/proc/self/ns/pid")
	h.TimeNS, _ = os.Readlink("/proc/self/ns/time")
	return h
}
