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

// IsRunning is IsAlive without the exited processes it counts: a zombie still
// answers signal 0. Off Linux, where no process state can be read, it is
// IsAlive.
func IsRunning(pid int) bool {
	if !IsAlive(pid) {
		return false
	}
	info, err := ProcStat(pid)
	return err != nil || !ExitedState(info.State)
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

// procRunningStart is procStartTime for StartProven, which must also know
// whether the process still runs: a zombie keeps its pid and start time, and
// kill(pid, 0) still succeeds for it. One ProcStat read gives both. Off Linux
// there is no start time, so nothing is reported running.
func procRunningStart(pid int) (start uint64, running bool, err error) {
	if runtime.GOOS != "linux" {
		return 0, false, nil
	}
	info, err := ProcStat(pid)
	if err != nil {
		return 0, false, err
	}
	return info.StartTime, !ExitedState(info.State), nil
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

// syncDirectory flushes a directory's entries so the ones created inside it —
// such as the rename that installs an atomically written file — survive a
// machine-level failure. Not every filesystem supports flushing a directory;
// the caller discards the error, so an unsupported platform only loses the
// extra durability, never the write.
//
// A slow directory flush is possible too (NFS and FUSE mounts): swallowing
// errors does not bound that latency.
func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
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
