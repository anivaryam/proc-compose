//go:build linux

package runner

import (
	"testing"
	"unsafe"
)

// TestWaitSiginfo_Layout pins the siginfo_t layout the exit observer reads.
//
// si_pid sits behind a pointer-sized amount of padding: the union that precedes
// it is 8 bytes wide on a 64-bit platform and 4 bytes wide on a 32-bit one, so
// si_pid is at offset 16 on 64-bit and at offset 12 on 32-bit. A layout that
// hardcodes the 64-bit offsets reads si_uid as si_pid on a 32-bit build.
//
// The struct must also stay exactly 128 bytes on both widths: waitid(2) is
// called with a pointer to it and the kernel writes a whole siginfo_t into the
// buffer, so a shorter struct would let the kernel write past its end.
func TestWaitSiginfo_Layout(t *testing.T) {
	if got := unsafe.Sizeof(waitSiginfo{}); got != 128 {
		t.Fatalf("sizeof(waitSiginfo) = %d, want 128", got)
	}
	wantPid := uintptr(12)
	if is64bit == 1 {
		wantPid = 16
	}
	if got := unsafe.Offsetof(waitSiginfo{}.Pid); got != wantPid {
		t.Fatalf("offsetof(waitSiginfo.Pid) = %d, want %d", got, wantPid)
	}
	if got := unsafe.Offsetof(waitSiginfo{}.Uid); got != wantPid+4 {
		t.Fatalf("offsetof(waitSiginfo.Uid) = %d, want %d", got, wantPid+4)
	}
}
