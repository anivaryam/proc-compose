//go:build !windows

package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
)

func TestRun_CheckDescendantsAreCleanedUp(t *testing.T) {
	f := newFixture(t, serviceConfig)
	childPID := filepath.Join(t.TempDir(), "child.pid")
	// The check leader exits at once and leaves a background child behind.
	result := Run(context.Background(), f.options(t, "", "/bin/sh", "-c", `sleep 60 & echo $! > "$1"`, "sh", childPID))
	if result.Check.Status != StatusPassed || result.Cleanup.Status != StatusPassed || result.Status != StatusPassed {
		t.Fatalf("check %s cleanup %+v errors %v", result.Check.Status, result.Cleanup, result.Errors)
	}
	data, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	assertGone(t, pid)
}

func TestRun_EscapedCheckDescendantIsUnverified(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		t.Skip("setsid not available")
	}
	f := newFixture(t, serviceConfig)
	childPID := filepath.Join(t.TempDir(), "child.pid")
	opts := f.options(t, "", "/bin/sh", "-c", `setsid sh -c 'echo $$ > "$1"; exec sleep 30' sh "$1" & sleep 0.5`, "sh", childPID)
	result := Run(context.Background(), opts)
	data, _ := os.ReadFile(childPID)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		t.Cleanup(func() {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		})
	}
	if result.Status == StatusPassed || result.Cleanup.Status != StatusUnverified || result.Cleanup.TerminationVerified == nil || *result.Cleanup.TerminationVerified {
		t.Fatalf("escaped descendant reported as clean: status %s cleanup %+v", result.Status, result.Cleanup)
	}
}

// TestVerify_RefusesCrashedRunRecord: verify never runs `up`'s preflight, so it
// gates on a crashed run itself, under the lease it just took. A record whose
// group is still running is not overwritten: the attempt fails as
// session_occupied, the group is untouched and the lease is released.
func TestVerify_RefusesCrashedRunRecord(t *testing.T) {
	f := newFixture(t, serviceConfig)
	opts := f.options(t, "", "true")
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	leader := exec.Command("sleep", "60")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = leader.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = leader.Process.Kill()
		<-reaped
	})
	info, _ := daemon.ProcStat(leader.Process.Pid)
	pidPath := paths.PID(opts.Hash)
	if err := daemon.WritePID(pidPath, gone.Process.Pid, paths.Socket(opts.Hash)); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteGroups(pidPath, gone.Process.Pid, []daemon.Group{{Name: "api", PGID: leader.Process.Pid, LeaderStart: info.StartTime}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(pidPath)

	result := Run(context.Background(), opts)
	assertComplete(t, result)
	if result.ExitCode != ExitUsage || result.Phase != PhaseAcquiring || len(result.Errors) == 0 || result.Errors[0].Code != CodeSessionOccupied {
		t.Fatalf("verify over a crashed run = exit %d phase %s errors %v, want session_occupied", result.ExitCode, result.Phase, result.Errors)
	}
	if after, _ := os.ReadFile(pidPath); string(after) != string(before) {
		t.Fatalf("verify overwrote the crashed run's record:\nbefore %s\nafter  %s", before, after)
	}
	if err := syscall.Kill(leader.Process.Pid, 0); err != nil {
		t.Fatalf("verify signalled the recorded leader: %v", err)
	}
	if held, _ := session.InstanceHeld(opts.Hash); held {
		t.Fatal("refusal kept the instance lease")
	}
}

// TestReadEnvironmentReport_RefusesNonRegularFiles: a FIFO or a symlink the
// check leaves at the report path is invalid evidence, and reading it must not
// block verify.
func TestReadEnvironmentReport_RefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(eligibleReport), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		done := make(chan error, 1)
		go func() { _, err := readEnvironmentReport(path); done <- err }()
		select {
		case err := <-done:
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: err = %v, want an invalid report", filepath.Base(path), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: reading the report blocked", filepath.Base(path))
		}
	}
}
