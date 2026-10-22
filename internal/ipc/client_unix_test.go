//go:build !windows

package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDialTimeout_PreservesCauseWithoutLeakingPath pins the dial-error
// contract: the error names the operating system's cause, errors.Is finds
// it, and the message carries the sanitized socket path, never the raw one.
func TestDialTimeout_PreservesCauseWithoutLeakingPath(t *testing.T) {
	home := usableHome(t)
	missing := filepath.Join(home, ".proc-compose-dial-test", "missing.sock")

	_, err := DialTimeout(missing, 500*time.Millisecond)
	if err == nil {
		t.Fatal("DialTimeout on a missing socket = nil, want an error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "no proc-compose daemon running") {
		t.Fatalf("dial error = %q, want it to start with %q", msg, "no proc-compose daemon running")
	}
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("dial error = %v, want it to wrap syscall.ENOENT", err)
	}
	if !strings.Contains(msg, "~") {
		t.Fatalf("dial error = %q, want the sanitized socket path", msg)
	}
	if strings.Contains(msg, home) {
		t.Fatalf("dial error = %q leaks the unsanitized home path %q", msg, home)
	}
}

// TestDialTimeout_OverLongSocketPathReportsEINVAL pins the cause for a
// permanently unusable socket address: the operating system's reason
// survives, and the unsanitized path does not.
func TestDialTimeout_OverLongSocketPathReportsEINVAL(t *testing.T) {
	home := usableHome(t)
	// Longer than the sun_path limit.
	long := filepath.Join(home, strings.Repeat("a", 200))

	_, err := DialTimeout(long, 500*time.Millisecond)
	if err == nil {
		t.Fatal("DialTimeout on an over-long socket path = nil, want an error")
	}
	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("dial error = %v, want it to wrap syscall.EINVAL", err)
	}
	if strings.Contains(err.Error(), home) {
		t.Fatalf("dial error = %q leaks the unsanitized home path %q", err, home)
	}
}

// TestDialTimeout_RefusedSocketReportsECONNREFUSED pins the cause for a
// socket whose daemon is gone: the path exists but nothing listens.
func TestDialTimeout_RefusedSocketReportsECONNREFUSED(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Leave the socket file behind, so that connecting to it is refused
	// rather than reported as missing.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	_, err = DialTimeout(path, 500*time.Millisecond)
	if err == nil {
		t.Fatal("DialTimeout on a refused socket = nil, want an error")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial error = %v, want it to wrap syscall.ECONNREFUSED", err)
	}
}

// usableHome returns the home directory to build socket paths under, or
// skips the test when there is none that can be sanitized.
func usableHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if home == "" || home == string(os.PathSeparator) {
		t.Skip("no usable home directory to build a socket path under")
	}
	return home
}
