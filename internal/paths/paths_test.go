package paths

import (
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
