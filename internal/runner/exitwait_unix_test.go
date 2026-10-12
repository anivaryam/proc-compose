//go:build linux || darwin

package runner

import (
	"runtime"
	"testing"
	"unsafe"
)

// TestWaitSiginfo_PidMatchesKernelLayout pins where si_pid is read. Linux pads
// before it and Darwin does not, so the Linux offset read si_uid on macOS, and
// CI run 38306358474 showed no exit ever being observed there.
func TestWaitSiginfo_PidMatchesKernelLayout(t *testing.T) {
	want := map[string]uintptr{"linux": 16, "darwin": 12}[runtime.GOOS]
	if got := unsafe.Offsetof(waitSiginfo{}.Pid); got != want {
		t.Fatalf("si_pid offset = %d, want %d on %s", got, want, runtime.GOOS)
	}
}
