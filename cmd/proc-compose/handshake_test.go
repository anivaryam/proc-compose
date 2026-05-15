package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testUnixSocketPath(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\` + strings.ReplaceAll(t.Name()+"-"+name, "/", "-")
	}
	dir, err := os.MkdirTemp("/tmp", "pc-test-*")
	if err != nil {
		t.Fatalf("temp socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func sleepCommand(t *testing.T, seconds int) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows lacks GNU sleep; use timeout in /T mode for parity.
		return exec.Command("cmd", "/c", "timeout", "/t", "30", "/nobreak")
	}
	_ = seconds
	return exec.Command("sleep", "30")
}

// TestWaitForDaemonReady_DetectsDeadChild ensures the parent surfaces an
// error when the child process exits during startup instead of silently
// reporting "daemon started".
func TestWaitForDaemonReady_DetectsDeadChild(t *testing.T) {
	// Spawn a quick-exit child.
	cmd := exec.Command("sh", "-c", "exit 7")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "7")
	}
	logPath := filepath.Join(t.TempDir(), "child.log")
	if err := os.WriteFile(logPath, []byte("preflight failed: port 8080 busy\n"), 0644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Wait for the child to actually exit so IsAlive returns false.
	_ = cmd.Wait()

	socketPath := testUnixSocketPath(t, "ipc.sock")
	err := waitForDaemonReady(pid, socketPath, logPath, 2*time.Second)
	if err == nil {
		t.Fatal("expected error when child has died, got nil")
	}
	if !strings.Contains(err.Error(), "died during startup") {
		t.Fatalf("expected 'died during startup' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("expected log tail in error, got: %v", err)
	}
}

// TestWaitForDaemonReady_TimesOutWithoutSocket ensures the parent fails
// when the child stays alive but never opens the IPC socket.
func TestWaitForDaemonReady_TimesOutWithoutSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipe semantics differ enough to need a separate test")
	}
	cmd := sleepCommand(t, 30)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	logPath := filepath.Join(t.TempDir(), "child.log")
	os.WriteFile(logPath, []byte("starting...\n"), 0644)

	socketPath := testUnixSocketPath(t, "ipc.sock")
	start := time.Now()
	err := waitForDaemonReady(cmd.Process.Pid, socketPath, logPath, 500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected 'did not become ready' in error, got: %v", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("waitForDaemonReady ran %s, expected ~500ms", elapsed)
	}
}

// TestWaitForDaemonReady_SucceedsWhenSocketReady ensures the parent
// returns nil once the IPC socket is accepting connections.
func TestWaitForDaemonReady_SucceedsWhenSocketReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipe Dial path uses go-winio; covered by ipc tests")
	}
	cmd := sleepCommand(t, 30)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	socketPath := testUnixSocketPath(t, "ipc.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	logPath := filepath.Join(t.TempDir(), "child.log")
	os.WriteFile(logPath, []byte(""), 0644)
	if err := waitForDaemonReady(cmd.Process.Pid, socketPath, logPath, 2*time.Second); err != nil {
		t.Fatalf("expected nil when socket is ready, got: %v", err)
	}
}
