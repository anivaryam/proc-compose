//go:build linux

package daemon

import (
	"os"
	"strings"
	"syscall"
	"testing"
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
