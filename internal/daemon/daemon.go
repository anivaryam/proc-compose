// Package daemon handles daemonization, PID file management, and process supervision.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/anivaryam/proc-compose/internal/sharedfile"
)

// Record is the PID file's content: the daemon's own identity and, on Unix,
// the process groups it launched. Fields added after the first release are
// optional so older readers ignore them.
type Record struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt uint64 `json:"started_at,omitempty"` // platform-specific start-time for PID-reuse defence
	Host
	// Groups is nil when nothing was recorded (a legacy file, Windows, or
	// recording disabled for the run) and an empty list when recording ran and
	// no launched group is still unaccounted for.
	Groups *[]Group `json:"groups,omitempty"`
}

// Host identifies the boot and namespaces a record's pids belong to. Every
// field is empty off Linux; TimeNS is also empty on kernels without time
// namespaces.
type Host struct {
	BootID string `json:"boot_id,omitempty"` // /proc/sys/kernel/random/boot_id
	PIDNS  string `json:"pid_ns,omitempty"`  // readlink /proc/self/ns/pid
	TimeNS string `json:"time_ns,omitempty"` // readlink /proc/self/ns/time
}

// Group is one launched process group. Entries are kept as a superset of the
// groups that may still be running: one leaves the record only once its group
// is proven empty.
type Group struct {
	Name string `json:"name"`
	// PGID is 0 while the launch is pending: the entry is written before the
	// command starts, so a crash in between cannot lose it.
	PGID int `json:"pgid"`
	// LeaderStart is the leader's procfs start time in clock ticks, or 0 where
	// the platform has none.
	LeaderStart uint64 `json:"leader_start,omitempty"`
}

// ProcInfo is what ProcStat reads from /proc/<pid>/stat.
type ProcInfo struct {
	State     byte   // single-letter process state
	PGID      int    // process group
	StartTime uint64 // start time in clock ticks since boot
}

// ExitedState reports whether a ProcStat state letter means the process has
// terminated: Z is a zombie awaiting its reaper, X and x the dead and
// in-death-transit states of exit. Such a process keeps its pid and start
// time until it is reaped.
func ExitedState(state byte) bool {
	return state == 'Z' || state == 'X' || state == 'x'
}

// Reexec re-executes the current binary with args, fully detached from the
// terminal (new session, stdin closed, stdout+stderr → logPath).
// Returns as soon as the child is launched.
//
// The child is reaped in the background once it exits, so an exited daemon
// does not linger as a zombie for as long as the starter lives: a zombie
// keeps its pid and still answers signal 0, which would make a dead daemon
// look alive.
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
	// Wait for the child in the background: the wait reaps it when it exits.
	// The error is irrelevant here — the daemon's exit status is the child's
	// own business, and the starter is already gone by then. cmd.Wait closes
	// only the descriptors it created itself; stdout and stderr are the *os.File
	// lf, which exec uses directly and never closes, so lf.Close below stays
	// responsible for it.
	go func() {
		_ = cmd.Wait()
	}()
	lf.Close()
	return pid, nil
}

// WritePID atomically writes pid, addr, and start-time to pidPath as JSON,
// replacing any previous record.
// The start-time is captured from the live PID (best-effort) so ReadPID
// callers can verify the PID has not been reused after a daemon crash.
func WritePID(pidPath string, pid int, addr string) error {
	startedAt, _ := procStartTime(pid)
	return writeRecord(pidPath, Record{PID: pid, Addr: addr, StartedAt: startedAt})
}

// ErrNotOwner reports a PID file that now names another process, such as a
// second daemon that recreated it after the first one's file was deleted.
var ErrNotOwner = errors.New("PID file belongs to another process")

// syncDir flushes the directory a file was renamed into, so the directory
// entry survives a machine-level failure. It is a package-level var so the
// error path can be exercised; the platform implementation lives next to the
// other platform code.
var syncDir = syncDirectory

// WriteGroups replaces the process-group record in an existing PID file,
// keeping the daemon's pid, address and start time and stamping the current
// host identity. owner is the pid the file must name, normally os.Getpid().
// It fails, and changes nothing, when the PID file is missing or unreadable
// (an fs.ErrNotExist error for a missing one) or names a pid other than owner
// (ErrNotOwner): another daemon's record is never overwritten. A nil list is
// written as an empty record.
func WriteGroups(pidPath string, owner int, groups []Group) error {
	rec, err := ReadRecord(pidPath)
	if err != nil {
		return err
	}
	if rec.PID != owner {
		return fmt.Errorf("%w: %s names pid %d, not %d", ErrNotOwner, pidPath, rec.PID, owner)
	}
	if groups == nil {
		groups = []Group{}
	}
	rec.Host = HostIdentity()
	rec.Groups = &groups
	return writeRecord(pidPath, rec)
}

func writeRecord(pidPath string, rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("cannot marshal PID file: %w", err)
	}
	return WriteFileAtomic(pidPath, data, 0600)
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory that is synced and then renamed over path, so a reader never
// observes a partial file. The directory must already exist.
//
// The rename goes through sharedfile.Replace: on Windows a rename over a file
// that a reader has open fails unless the reader allows delete sharing and the
// rename uses POSIX semantics, so readers of these files read through
// sharedfile as well.
//
// After the rename the parent directory is flushed as well, so the directory
// entry the rename created survives a machine-level failure and not only the
// file's contents. That step is best-effort hardening: this helper promises
// reader-atomicity, not durability, and the artifacts it writes live in a
// runtime directory that is documented as ephemeral.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := sharedfile.Replace(tmp.Name(), path); err != nil {
		return err
	}
	// Flush the directory entry the rename created. The error is discarded on
	// purpose: a filesystem that cannot flush a directory (Windows, some
	// network and FUSE mounts) must not turn a successful rename into a
	// failed write.
	_ = syncDir(filepath.Dir(path))
	return nil
}

// ReadRecord reads the whole PID file. A file that does not parse is an error;
// it is never treated as a legacy file without groups. It reads through
// sharedfile, so a concurrent WriteFileAtomic still succeeds on Windows.
func ReadRecord(pidPath string) (Record, error) {
	data, err := sharedfile.ReadFile(pidPath)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("invalid PID file %s: %w", pidPath, err)
	}
	return rec, nil
}

// ReadPID reads pid, addr, and start-time from pidPath.
func ReadPID(pidPath string) (int, string, error) {
	pid, _, addr, err := readPIDFull(pidPath)
	return pid, addr, err
}

// readPIDFull returns pid, recorded start-time (or 0 if unset), and addr.
func readPIDFull(pidPath string) (int, uint64, string, error) {
	rec, err := ReadRecord(pidPath)
	if err != nil {
		return 0, 0, "", err
	}
	return rec.PID, rec.StartedAt, rec.Addr, nil
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

// StartProven reports whether rec's process is running now as the process
// that wrote rec: its pid names a process that is still running, not one
// that has exited and awaits its reaper, and its start time equals the
// recorded one. Unlike IsAliveFromPIDFile it never falls back to the pid
// alone, so a record without a start time (an older file, or macOS) or a
// process whose start time cannot be read is not proven; and it does not
// count an exited process that keeps its pid and start time (a zombie on
// Linux, or on Windows a process some handle still holds open). Use it where
// a stale record must never count as live.
func StartProven(rec Record) bool {
	if rec.PID <= 0 || rec.StartedAt == 0 || !IsAlive(rec.PID) {
		return false
	}
	cur, running, err := procRunningStart(rec.PID)
	return err == nil && running && cur == rec.StartedAt
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
