package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFromConfig_Deterministic(t *testing.T) {
	a, err := FromConfig("/tmp/a.yml")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	b, err := FromConfig("/tmp/a.yml")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if a != b {
		t.Errorf("FromConfig not deterministic: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("hash length = %d, want 8", len(a))
	}
}

func TestFromConfig_DifferentPathsDifferentHashes(t *testing.T) {
	a, _ := FromConfig("/tmp/a.yml")
	b, _ := FromConfig("/tmp/b.yml")
	if a == b {
		t.Errorf("expected different hashes for different paths, both %q", a)
	}
}

func TestSocket_PathFormat(t *testing.T) {
	hash := "deadbeef"
	got := Socket(hash)
	if runtime.GOOS == "windows" {
		want := `\\.\pipe\pc-deadbeef`
		if got != want {
			t.Errorf("Socket = %q, want %q", got, want)
		}
		return
	}
	if !strings.HasSuffix(got, "pc-deadbeef.sock") {
		t.Errorf("Socket suffix mismatch: %q", got)
	}
}

func TestPID_PathFormat(t *testing.T) {
	got := PID("cafefeed")
	if !strings.HasSuffix(got, "pc-cafefeed.pid") {
		t.Errorf("PID suffix mismatch: %q", got)
	}
}

func TestLog_PathFormat(t *testing.T) {
	got := Log("c0ffee01")
	if !strings.HasSuffix(got, "pc-c0ffee01.log") {
		t.Errorf("Log suffix mismatch: %q", got)
	}
}

// TestCache_IsSharedAcrossProjects pins the property the update check relies
// on: the cache lives under the user cache directory, not inside whichever
// project directory the command happened to run from.
func TestCache_IsSharedAcrossProjects(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	first := Cache()
	if first == "" {
		t.Fatal("Cache() returned empty string")
	}
	if !strings.HasSuffix(first, filepath.Join("proc-compose")) {
		t.Errorf("Cache() = %q, want a path ending in proc-compose", first)
	}
	if second := Cache(); second != first {
		t.Errorf("Cache() is not stable across calls: %q vs %q", first, second)
	}

	// os.UserCacheDir already created the parent; Cache adds proc-compose.
	parent, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir: %v", err)
	}
	if want := filepath.Join(parent, "proc-compose"); first != want {
		t.Errorf("Cache() = %q, want %q", first, want)
	}
	if _, err := os.Stat(first); err != nil {
		t.Errorf("Cache() did not create %s: %v", first, err)
	}
}

func TestCacheFile_PathFormat(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	got := CacheFile("update-check.json")
	if want := filepath.Join(Cache(), "update-check.json"); got != want {
		t.Errorf("CacheFile() = %q, want %q", got, want)
	}
}

func TestCache_UnavailableDoesNotFallBackToSharedOrProjectPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("HOME", root)
	t.Setenv("LocalAppData", root)
	parent, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	// A regular file prevents creating the application cache directory.
	if err := os.WriteFile(filepath.Join(parent, "proc-compose"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := Cache(); got != "" {
		t.Fatalf("Cache() = %q, want unavailable", got)
	}
	if got := CacheFile("update-check.json"); got != "" {
		t.Fatalf("CacheFile() = %q, want unavailable", got)
	}
}
