//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProcStat_MatchesProcStartTimeAndPgrp is the parity test for the single
// /proc/<pid>/stat parser: the start time it reports is the one the PID file
// records, and the group is the kernel's own answer.
func TestProcStat_MatchesProcStartTimeAndPgrp(t *testing.T) {
	info, err := ProcStat(os.Getpid())
	if err != nil {
		t.Fatalf("ProcStat(self): %v", err)
	}
	start, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("procStartTime(self): %v", err)
	}
	if info.StartTime == 0 || info.StartTime != start {
		t.Fatalf("ProcStat start = %d, procStartTime = %d", info.StartTime, start)
	}
	if info.PGID != syscall.Getpgrp() {
		t.Fatalf("ProcStat pgid = %d, Getpgrp = %d", info.PGID, syscall.Getpgrp())
	}
	if info.State != 'R' && info.State != 'S' {
		t.Fatalf("ProcStat state of the running test = %q, want R or S", info.State)
	}
	if _, err := ProcStat(-1); err == nil {
		t.Fatal("ProcStat(-1) succeeded")
	}
}

// TestHostIdentity_LinuxNonEmptyStable: boot id and pid namespace are always
// readable on Linux and do not change within a process.
func TestHostIdentity_LinuxNonEmptyStable(t *testing.T) {
	h := HostIdentity()
	if h.BootID == "" || strings.ContainsAny(h.BootID, " \n") {
		t.Fatalf("BootID = %q, want a trimmed non-empty id", h.BootID)
	}
	if !strings.HasPrefix(h.PIDNS, "pid:[") {
		t.Fatalf("PIDNS = %q, want pid:[...]", h.PIDNS)
	}
	if h.TimeNS != "" && !strings.HasPrefix(h.TimeNS, "time:[") {
		t.Fatalf("TimeNS = %q, want empty or time:[...]", h.TimeNS)
	}
	if again := HostIdentity(); again != h {
		t.Fatalf("HostIdentity changed: %+v then %+v", h, again)
	}
}

// TestStartProven_ZombieIsNotProven pins that a supervisor that has exited
// but not been reaped is not live: a zombie keeps its pid and start time, and
// kill(pid, 0) still succeeds for it. IsAliveFromPIDFile, which `sessions`
// uses, keeps its own semantics and still reports it.
func TestStartProven_ZombieIsNotProven(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	path := filepath.Join(t.TempDir(), "child.pid")
	if err := WritePID(path, cmd.Process.Pid, ""); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if !StartProven(rec) {
		t.Fatalf("StartProven(%+v) = false for a running child", rec)
	}
	if !IsRunning(cmd.Process.Pid) {
		t.Fatal("IsRunning = false for a running child")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		info, err := ProcStat(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("ProcStat(killed child): %v", err)
		}
		if info.State == 'Z' {
			if info.StartTime != rec.StartedAt {
				t.Fatalf("zombie start time %d, recorded %d", info.StartTime, rec.StartedAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed child never became a zombie (state %q)", info.State)
		}
	}
	if StartProven(rec) {
		t.Fatal("StartProven = true for an exited, unreaped child (zombie)")
	}
	if alive, _ := IsAliveFromPIDFile(path); !alive {
		t.Fatal("IsAliveFromPIDFile changed: it no longer reports the zombie")
	}
	if IsRunning(cmd.Process.Pid) {
		t.Fatal("IsRunning = true for an exited, unreaped child (zombie)")
	}
	_ = cmd.Wait()
	reaped = true
	if StartProven(rec) {
		t.Fatal("StartProven = true for a reaped child")
	}
}

// TestReexec_ReapsTheChildItStarts pins that the daemon the starter launches
// is reaped when it exits. Without a wait(2) the child stays a zombie for as
// long as the starter lives: it keeps its pid, /proc/<pid> stays readable, and
// kill(pid, 0) still succeeds — so IsAlive reports a dead daemon as alive.
//
// The assertion is on /proc, so this test is Linux-only: it must not also be
// required on windows-latest.
func TestReexec_ReapsTheChildItStarts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	// Reexec re-executes this very binary, so the args must make it exit at
	// once: "-test.run=^$" matches no test.
	pid, err := Reexec([]string{"-test.run=^$"}, logPath)
	if err != nil {
		t.Fatalf("Reexec: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("Reexec returned pid %d, want a live pid", pid)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, statErr := os.Stat(fmt.Sprintf("/proc/%d", pid))
		if os.IsNotExist(statErr) {
			return // reaped: the pid is gone
		}
		if time.Now().After(deadline) {
			info, procErr := ProcStat(pid)
			t.Fatalf("child %d was not reaped within 10s (state %q, procfs error %v)", pid, info.State, procErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExitedState names the terminated procfs states.
func TestExitedState(t *testing.T) {
	for _, state := range []byte("ZXx") {
		if !ExitedState(state) {
			t.Errorf("ExitedState(%q) = false", state)
		}
	}
	for _, state := range []byte("RSDTtIWP") {
		if ExitedState(state) {
			t.Errorf("ExitedState(%q) = true", state)
		}
	}
}
