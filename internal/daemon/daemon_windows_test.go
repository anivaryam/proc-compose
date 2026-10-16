//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestStartProven_ExitedProcessWithOpenHandleIsNotProven pins the Windows
// side of a crashed, unreaped supervisor: once a process has exited, any
// handle still open to it (here this test's own, and the one exec.Cmd keeps
// until Wait) keeps it openable with the same creation time. It must not be
// proven live. IsAliveFromPIDFile, which `sessions` uses, keeps its own
// semantics and still reports it.
func TestStartProven_ExitedProcessWithOpenHandleIsNotProven(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	path := filepath.Join(t.TempDir(), "child.pid")
	if err := WritePID(path, cmd.Process.Pid, ""); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.StartedAt == 0 {
		t.Fatal("no creation time recorded for the child")
	}
	if event, err := syscall.WaitForSingleObject(h, 30000); err != nil || event != syscall.WAIT_OBJECT_0 {
		t.Fatalf("child did not exit: event %d, %v", event, err)
	}
	if StartProven(rec) {
		t.Fatal("StartProven = true for an exited process that a handle keeps open")
	}
	if alive, _ := IsAliveFromPIDFile(path); !alive {
		t.Fatal("IsAliveFromPIDFile changed: it no longer reports the exited, still-open process")
	}
	self := filepath.Join(t.TempDir(), "self.pid")
	if err := WritePID(self, os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	if live, err := ReadRecord(self); err != nil || !StartProven(live) {
		t.Fatalf("StartProven(this running process) = false (%v)", err)
	}
}
