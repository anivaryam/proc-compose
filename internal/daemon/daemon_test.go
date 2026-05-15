package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestStripFlag_NoValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		flag string
		want []string
	}{
		{"single hit", []string{"a", "--silent", "b"}, "--silent", []string{"a", "b"}},
		{"start", []string{"--silent", "a"}, "--silent", []string{"a"}},
		{"end", []string{"a", "--silent"}, "--silent", []string{"a"}},
		{"multiple", []string{"--silent", "a", "--silent"}, "--silent", []string{"a"}},
		{"absent", []string{"a", "b"}, "--silent", []string{"a", "b"}},
		{"empty", []string{}, "--silent", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripFlag(tc.in, tc.flag, false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripFlag(%v, %q, false) = %v, want %v", tc.in, tc.flag, got, tc.want)
			}
		})
	}
}

func TestStripFlag_WithValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		flag string
		want []string
	}{
		{"single", []string{"-f", "config.yml", "up"}, "-f", []string{"up"}},
		{"middle", []string{"up", "--file", "x.yml", "--name", "n"}, "--file", []string{"up", "--name", "n"}},
		{"end", []string{"up", "--file", "x.yml"}, "--file", []string{"up"}},
		{"absent", []string{"up", "--silent"}, "--file", []string{"up", "--silent"}},
		{"two occurrences", []string{"--file", "a", "--file", "b"}, "--file", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripFlag(tc.in, tc.flag, true)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripFlag(%v, %q, true) = %v, want %v", tc.in, tc.flag, got, tc.want)
			}
		})
	}
}

func TestPIDRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if err := WritePID(path, 4242, "/tmp/test.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	pid, addr, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 4242 {
		t.Errorf("pid = %d, want 4242", pid)
	}
	if addr != "/tmp/test.sock" {
		t.Errorf("addr = %q, want %q", addr, "/tmp/test.sock")
	}
}

func TestReadPID_Missing(t *testing.T) {
	_, _, err := ReadPID(filepath.Join(t.TempDir(), "missing.pid"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestReadPID_Corrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pid")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadPID(path); err == nil {
		t.Fatal("expected error for corrupted PID file, got nil")
	}
}

func TestCleanup_RemovesFiles(t *testing.T) {
	dir := t.TempDir()
	pid := filepath.Join(dir, "x.pid")
	sock := filepath.Join(dir, "x.sock")
	os.WriteFile(pid, []byte("{}"), 0600)
	os.WriteFile(sock, []byte(""), 0600)

	Cleanup(pid, sock)

	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Error("pid file should be removed")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Error("socket file should be removed")
	}
}

func TestCleanup_MissingFilesNoError(t *testing.T) {
	// Cleanup is best-effort; should not panic on missing files.
	Cleanup("/nonexistent.pid", "/nonexistent.sock")
}

func TestPIDFile_Mode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if err := WritePID(path, os.Getpid(), "/tmp/x.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if mode != 0600 {
		t.Errorf("PID file mode = %o, want 0600 (world-readable PID file leaks runtime path)", mode)
	}
}

// TestIsAliveFromPIDFile_RoundtripSelf verifies that a pidfile written for
// the current process reports alive (the start-time check matches because
// the OS hasn't reused our PID).
func TestIsAliveFromPIDFile_RoundtripSelf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self.pid")
	if err := WritePID(path, os.Getpid(), "/tmp/x.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	alive, pid := IsAliveFromPIDFile(path)
	if !alive {
		t.Errorf("expected current process to be alive, got false (pid=%d)", pid)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

// TestIsAliveFromPIDFile_MissingFile returns alive=false, pid=0 cleanly.
func TestIsAliveFromPIDFile_MissingFile(t *testing.T) {
	alive, pid := IsAliveFromPIDFile(filepath.Join(t.TempDir(), "nope.pid"))
	if alive {
		t.Error("expected alive=false for missing pidfile")
	}
	if pid != 0 {
		t.Errorf("expected pid=0 for missing pidfile, got %d", pid)
	}
}

// TestIsAliveFromPIDFile_StartTimeMismatch ensures that when the recorded
// start-time disagrees with the live process's start-time we treat the
// pidfile as stale (defending against PID reuse).
func TestIsAliveFromPIDFile_StartTimeMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.pid")

	// Synthesize a pidfile that points at *our* PID but with a wrong
	// start-time. On Linux procStartTime returns a real value; on other
	// Unixes it returns 0 and we fall back to liveness-only — skip there.
	startedAt, err := procStartTime(os.Getpid())
	if err != nil {
		t.Skipf("procStartTime unavailable: %v", err)
	}
	if startedAt == 0 {
		t.Skip("procStartTime unavailable on this platform")
	}
	stale := pidFile{PID: os.Getpid(), Addr: "/tmp/x.sock", StartedAt: 1}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	alive, pid := IsAliveFromPIDFile(path)
	if alive {
		t.Errorf("expected stale=true to surface as alive=false, got alive=true (pid=%d)", pid)
	}
}
