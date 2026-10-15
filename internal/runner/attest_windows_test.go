//go:build windows

package runner

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	gnet "github.com/shirou/gopsutil/v3/net"
	"golang.org/x/sys/windows"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// startServeGroup runs the serve-http helper in a fresh Job Object, as exec
// does, and returns the group once the helper answers on addr.
func startServeGroup(t testing.TB, addr string) ProcessGroup {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestRunnerHelperProcess$", "--", helperSentinel, encodeHelperArgs("serve-http", addr))
	pg := newProcessGroup()
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := pg.Track(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pg.Kill()
		_ = cmd.Wait()
		_ = pg.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get("http://" + addr + "/"); err == nil {
			resp.Body.Close()
			return pg
		}
		if time.Now().After(deadline) {
			t.Fatal("serve-http never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func dialConn(t testing.TB, addr string) probeConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c, _ := probeConnOf(conn)
	return c
}

// TestAttribute_DualStackListenerInTheJobIsOwned pins the Windows table
// forms: a [::] listener reached through 127.0.0.1, held by a process in the
// Job Object, is owned, and the same socket judged against another job is
// foreign.
func TestAttribute_DualStackListenerInTheJobIsOwned(t *testing.T) {
	port := freeAddr(t)
	_, p, _ := net.SplitHostPort(port)
	pg := startServeGroup(t, "[::]:"+p)
	c := dialConn(t, "127.0.0.1:"+p)
	if got := attributeSocket(c, pg); got.Result != ipc.OwnerOwned || got.Command == "" {
		t.Fatalf("attribution = %+v, want owned with the server's image name", got)
	}
	other := newProcessGroup()
	defer other.Close()
	if got := attributeSocket(c, other); got.Result != ipc.OwnerForeign {
		t.Fatalf("against another job: attribution = %+v, want foreign", got)
	}
}

// TestAttribute_ProtectedHolderIsUnattributable uses a listener the System
// process (PID 4) holds, such as SMB on port 445, which cannot be opened:
// its membership is unknown, which must never read as foreign.
func TestAttribute_ProtectedHolderIsUnattributable(t *testing.T) {
	conns, err := gnet.Connections("tcp")
	if err != nil {
		t.Fatal(err)
	}
	target := ""
	for _, c := range conns {
		if c.Status == "LISTEN" && c.Pid == 4 && (c.Laddr.IP == "0.0.0.0" || c.Laddr.IP == "127.0.0.1") {
			target = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(c.Laddr.Port)))
			break
		}
	}
	if target == "" {
		t.Skip("no loopback-reachable listener held by the System process")
	}
	conn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Skipf("dial %s: %v", target, err)
	}
	defer conn.Close()
	c, _ := probeConnOf(conn)
	pg := newProcessGroup()
	defer pg.Close()
	got := attributeSocket(c, pg)
	// Unattributable, and the holder is not identified in any field: an
	// uninspectable process is not known to be this user's.
	if got.Result != ipc.OwnerUnattributable || got.PID != 0 || got.Command != "" || strings.Contains(got.Reason, "PID") || strings.Contains(got.Reason, " 4") {
		t.Fatalf("attribution of %s = %+v, want unattributable with no holder identity", target, got)
	}
	t.Logf("%s: %s", target, got.Reason)
}

// TestAttribute_OtherUsersHolderIsNotNamed pins the same-user rule on
// Windows: a listener held by a process of another user (a service such as
// the RPC endpoint mapper's svchost) is unattributable and not named, even
// when this process may open it.
func TestAttribute_OtherUsersHolderIsNotNamed(t *testing.T) {
	conns, err := gnet.Connections("tcp")
	if err != nil {
		t.Fatal(err)
	}
	target, holder := "", int32(0)
	for _, c := range conns {
		if c.Status != "LISTEN" || c.Pid <= 4 || (c.Laddr.IP != "0.0.0.0" && c.Laddr.IP != "127.0.0.1") {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(c.Pid))
		if err != nil {
			continue
		}
		other := !processSameUser(h)
		windows.CloseHandle(h)
		if other {
			target, holder = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(c.Laddr.Port))), c.Pid
			break
		}
	}
	if target == "" {
		t.Skip("no loopback-reachable listener of another user that this process can open")
	}
	conn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Skipf("dial %s: %v", target, err)
	}
	defer conn.Close()
	c, _ := probeConnOf(conn)
	pg := newProcessGroup()
	defer pg.Close()
	got := attributeSocket(c, pg)
	if got.Result != ipc.OwnerUnattributable || got.PID != 0 || got.Command != "" || !strings.Contains(got.Reason, "another user") ||
		strings.Contains(got.Reason, "PID") || strings.Contains(got.Reason, strconv.Itoa(int(holder))) {
		t.Fatalf("attribution of %s = %+v, want unattributable and unnamed as another user's", target, got)
	}
	t.Logf("%s (held by PID %d, another user's, openable by this process): %+v", target, holder, got)
}

// TestProcessSameUser compares this process's token user with its own.
func TestProcessSameUser(t *testing.T) {
	if !processSameUser(windows.CurrentProcess()) {
		t.Fatal("processSameUser(self) = false")
	}
}

// TestIsProcessInJob_ResultIsAFourByteBOOL pins why isProcessInJob receives
// its result in an int32: the kernel writes a four-byte BOOL. A canary right
// after the result must survive, and the answer must match membership.
// go-winjob's wrapper passes a one-byte bool, so the same write clobbers the
// three bytes after it, which corrupted heap memory under -race.
func TestIsProcessInJob_ResultIsAFourByteBOOL(t *testing.T) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	var s struct {
		result int32
		canary [4]byte
	}
	s.canary = [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	if r, _, err := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), uintptr(job), uintptr(unsafe.Pointer(&s.result))); r == 0 {
		t.Fatal(err)
	}
	if s.canary != [4]byte{0xAA, 0xBB, 0xCC, 0xDD} {
		t.Fatalf("IsProcessInJob wrote past its four-byte result: canary = % x", s.canary)
	}
	if found, err := isProcessInJob(windows.CurrentProcess(), job); err != nil || found {
		t.Fatalf("isProcessInJob(self, fresh job) = %v, %v; want false", found, err)
	}
}

func benchmarkAttributeWindows(b *testing.B, foreign bool) {
	addr := freeAddr(b)
	pg := startServeGroup(b, addr)
	want := ipc.OwnerOwned
	if foreign {
		pg = newProcessGroup()
		defer pg.Close()
		want = ipc.OwnerForeign
	}
	c := dialConn(b, addr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := attributeSocket(c, pg); got.Result != want {
			b.Fatalf("attribution = %+v, want %s", got, want)
		}
	}
}

// BenchmarkAttributeWindows_Owned measures one attribution of a socket held
// by a process in the job: one TCP table read and one process open.
func BenchmarkAttributeWindows_Owned(b *testing.B) { benchmarkAttributeWindows(b, false) }

// BenchmarkAttributeWindows_Foreign is the same against an empty job.
func BenchmarkAttributeWindows_Foreign(b *testing.B) { benchmarkAttributeWindows(b, true) }
