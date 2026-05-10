// Package daemon handles daemonization, PID file management, and process supervision.
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type pidFile struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt uint64 `json:"started_at,omitempty"` // platform-specific start-time for PID-reuse defence
}

// Reexec re-executes the current binary with args, fully detached from the
// terminal (new session, stdin closed, stdout+stderr → logPath).
// Returns as soon as the child is launched; does NOT wait.
func Reexec(args []string, logPath string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("cannot find executable: %w", err)
	}

	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, fmt.Errorf("cannot open log file %s: %w", logPath, err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	setDetachAttr(cmd)

	if err := cmd.Start(); err != nil {
		lf.Close()
		return 0, fmt.Errorf("failed to start daemon: %w", err)
	}

	pid := cmd.Process.Pid
	cmd.Process.Release()
	lf.Close()
	return pid, nil
}

// WritePID writes pid, addr, and start-time to pidPath as JSON.
// The start-time is captured from the live PID (best-effort) so ReadPID
// callers can verify the PID has not been reused after a daemon crash.
func WritePID(pidPath string, pid int, addr string) error {
	startedAt, _ := procStartTime(pid)
	pf := pidFile{PID: pid, Addr: addr, StartedAt: startedAt}
	data, err := json.Marshal(pf)
	if err != nil {
		return fmt.Errorf("cannot marshal PID file: %w", err)
	}
	return os.WriteFile(pidPath, data, 0600)
}

// ReadPID reads pid, addr, and start-time from pidPath.
func ReadPID(pidPath string) (int, string, error) {
	pid, _, addr, err := readPIDFull(pidPath)
	return pid, addr, err
}

// readPIDFull returns pid, recorded start-time (or 0 if unset), and addr.
func readPIDFull(pidPath string) (int, uint64, string, error) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, 0, "", err
	}
	var pf pidFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return 0, 0, "", fmt.Errorf("invalid PID file %s: %w", pidPath, err)
	}
	return pf.PID, pf.StartedAt, pf.Addr, nil
}

// IsAliveFromPIDFile reads the pidfile and verifies the recorded process is
// still the same one (PID + start-time). Use this instead of IsAlive(pid)
// alone wherever the PID came from disk, to defend against PID reuse.
func IsAliveFromPIDFile(pidPath string) (bool, int) {
	pid, startedAt, _, err := readPIDFull(pidPath)
	if err != nil || pid <= 0 {
		return false, 0
	}
	if !IsAlive(pid) {
		return false, pid
	}
	if startedAt == 0 {
		// pidfile predates start-time tracking; fall back to liveness only.
		return true, pid
	}
	cur, err := procStartTime(pid)
	if err != nil || cur == 0 {
		return true, pid // can't read → assume our PID
	}
	return cur == startedAt, pid
}

// Cleanup removes the PID and socket files (best-effort, ignores errors).
func Cleanup(pidPath, socketPath string) {
	os.Remove(pidPath)
	os.Remove(socketPath)
}

// StripFlag removes all occurrences of flag (and its value if hasValue=true)
// from args. Used to build child arg lists for re-exec.
func StripFlag(args []string, flag string, hasValue bool) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == flag {
			if hasValue {
				skip = true
			}
			continue
		}
		out = append(out, a)
	}
	return out
}
