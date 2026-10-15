//go:build linux

package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

const attestHelperEnv = "PROC_COMPOSE_ATTEST_HELPER"

// TestAttestHelperProcess is the listener the attribution tests run inside a
// process group: serve <addr> <portfile> [reuseport|noaccept|close]. It
// answers every request with its own PID and exits on its own after a minute,
// so a failing test cannot leak it.
func TestAttestHelperProcess(t *testing.T) {
	if os.Getenv(attestHelperEnv) != "serve" {
		return
	}
	args := os.Args[len(os.Args)-3:]
	addr, portFile, mode := args[0], args[1], args[2]
	time.AfterFunc(time.Minute, func() { os.Exit(3) })
	ln, err := listenForAttest(addr, mode == "reuseport")
	if err != nil {
		os.Exit(2)
	}
	if mode != "noaccept" {
		go serveOwnPID(ln, mode == "close")
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(portFile+".tmp", []byte(strconv.Itoa(port)), 0o644); err != nil {
		os.Exit(2)
	}
	if err := os.Rename(portFile+".tmp", portFile); err != nil {
		os.Exit(2)
	}
	select {}
}

func listenForAttest(addr string, reusePort bool) (net.Listener, error) {
	lc := net.ListenConfig{}
	if reusePort {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1) }); err != nil {
				return err
			}
			return serr
		}
	}
	return lc.Listen(context.Background(), "tcp", addr)
}

func serveOwnPID(ln net.Listener, closeAfter bool) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if closeAfter {
			w.Header().Set("Connection", "close")
		}
		fmt.Fprint(w, os.Getpid())
	})}
	_ = srv.Serve(ln)
}

// attestCommand is the shell command that runs the helper as a grandchild:
// neither shell execs, so the listener is two levels below the group leader.
func attestCommand(addr, portFile, mode string) string {
	inner := posixQuote(os.Args[0]) + " -test.run=^TestAttestHelperProcess$ -- " + posixQuote(addr) + " " + posixQuote(portFile) + " " + mode
	return "sh -c " + posixQuote(inner+"; :") + "; :"
}

// startAttestGroup runs the helper in a new process group and returns the
// group ID and the port it listens on. The group is killed at cleanup.
func startAttestGroup(t *testing.T, addr, mode string) (pgid, port int) {
	t.Helper()
	portFile := filepath.Join(t.TempDir(), "port")
	cmd := exec.Command("sh", "-c", attestCommand(addr, portFile, mode))
	cmd.Env = append(os.Environ(), attestHelperEnv+"=serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid = cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(portFile); err == nil {
			port, _ = strconv.Atoi(string(b))
			return pgid, port
		}
		if time.Now().After(deadline) {
			t.Fatal("attest helper never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startIdleGroup runs a process group that holds no socket, for probes whose
// answer must be foreign to it.
func startIdleGroup(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// requestPID sends one keep-alive GET over conn and returns the PID the
// server reported, leaving conn open.
func requestPID(t *testing.T, conn net.Conn) int {
	t.Helper()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: probe\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatalf("server answered %q", body)
	}
	return pid
}

func dialProbe(t *testing.T, addr string) (net.Conn, probeConn) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c, ok := probeConnOf(conn)
	if !ok {
		t.Fatal("not a TCP connection")
	}
	return conn, c
}

func requireOwner(t *testing.T, got ipc.EndpointOwner, result string, pid int) {
	t.Helper()
	if got.Result != result || (pid != 0 && got.PID != pid) {
		t.Fatalf("attribution = %+v, want %s by PID %d", got, result, pid)
	}
}

// TestParseSocketLine reads both table formats, including the IPv4-mapped
// form a dual-stack socket uses, and skips the header and malformed rows.
func TestParseSocketLine(t *testing.T) {
	v4 := "   1: 0100007F:0BB8 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 4242 1 0000000000000000 20 4 30 10 -1"
	v6 := "   0: 0000000000000000FFFF00000100007F:0BB8 0000000000000000FFFF00000100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 4343 1 0000000000000000 20 4 0 10 -1"
	any6 := "   0: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4444 1 0000000000000000 100 0 0 10 0"
	for line, want := range map[string]socketEntry{
		v4:   {local: ap("127.0.0.1:3000"), remote: ap("127.0.0.1:50000"), state: 1, uid: 1000, inode: 4242},
		v6:   {local: ap("127.0.0.1:3000"), remote: ap("127.0.0.1:50000"), state: 1, uid: 1000, inode: 4343},
		any6: {local: ap("[::]:3000"), remote: ap("[::]:0"), state: tcpListen, uid: 1000, inode: 4444},
	} {
		if got, ok := parseSocketLine(line); !ok || got != want {
			t.Errorf("parseSocketLine(%q) = %+v, %v; want %+v", line, got, ok, want)
		}
	}
	for _, bad := range []string{"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode", "   1: 0100007F 0100007F:C350 01"} {
		if _, ok := parseSocketLine(bad); ok {
			t.Errorf("parseSocketLine(%q) accepted a malformed row", bad)
		}
	}
}

// TestAttribute_GrandchildListenerIsOwned covers the common tree shape: the
// listener is a descendant of the group leader, not the leader itself.
func TestAttribute_GrandchildListenerIsOwned(t *testing.T) {
	pgid, port := startAttestGroup(t, "127.0.0.1:0", "serve")
	conn, c := dialProbe(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	server := requestPID(t, conn)
	if server == pgid {
		t.Fatal("the listener is the group leader; the fixture proves nothing about descendants")
	}
	requireOwner(t, attributePGID(c, pgid), ipc.OwnerOwned, server)
}

// TestAttribute_ForeignListenerNamesItsHolder is the false-readiness case: a
// process outside the group answers, and it is named.
func TestAttribute_ForeignListenerNamesItsHolder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveOwnPID(ln, false)
	pgid := startIdleGroup(t)
	conn, c := dialProbe(t, ln.Addr().String())
	requestPID(t, conn)
	got := attributePGID(c, pgid)
	requireOwner(t, got, ipc.OwnerForeign, os.Getpid())
	if got.Command == "" {
		t.Fatalf("foreign holder has no process name: %+v", got)
	}
}

// TestAttribute_DualStackWildcardThroughIPv4 pins the IPv4-mapped form: a
// [::] listener accepts 127.0.0.1 and lists the connection in tcp6.
func TestAttribute_DualStackWildcardThroughIPv4(t *testing.T) {
	pgid, port := startAttestGroup(t, "[::]:0", "serve")
	conn, c := dialProbe(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	server := requestPID(t, conn)
	requireOwner(t, attributePGID(c, pgid), ipc.OwnerOwned, server)
}

// TestAttribute_QueuedConnectionUsesTheListener covers a connection the
// server has not accepted yet (a tcp probe connects and leaves at once): the
// accepted socket has no inode, so the listener holding the queue answers.
func TestAttribute_QueuedConnectionUsesTheListener(t *testing.T) {
	pgid, port := startAttestGroup(t, "127.0.0.1:0", "noaccept")
	_, c := dialProbe(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	requireOwner(t, attributePGID(c, pgid), ipc.OwnerOwned, 0)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, c = dialProbe(t, ln.Addr().String())
	requireOwner(t, attributePGID(c, pgid), ipc.OwnerForeign, os.Getpid())
}

// TestAttribute_ServerClosedConnectionIsUnattributable pins why attribution
// runs right after the dial: once the server has answered and closed its
// side, the row is past its life, and the listener now on the port proves
// nothing about who answered (it may be a successor that took the port
// over). So it is unattributable, never the current listener's.
func TestAttribute_ServerClosedConnectionIsUnattributable(t *testing.T) {
	pgid, port := startAttestGroup(t, "127.0.0.1:0", "close")
	conn, c := dialProbe(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	requestPID(t, conn)
	// Wait for the server's close to arrive, so its side is closing.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("server did not close: %v", err)
	}
	got := attributePGID(c, pgid)
	if got.Result != ipc.OwnerUnattributable || !strings.Contains(got.Reason, "already closed") {
		t.Fatalf("attribution after the server closed = %+v, want unattributable", got)
	}
	// Read before the request, the same connection is owned.
	_, c = dialProbe(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	requireOwner(t, attributePGID(c, pgid), ipc.OwnerOwned, 0)
}

// TestAttribute_SameUserRuleAsRoot exercises the same-user rule against a
// real process of another user. It runs only as root, where every fd table
// is readable, so the rule, not file permissions, decides: a listener held by
// another user's process (nobody, or PROC_COMPOSE_TEST_OTHER_UID) is
// unattributable and not named, although root can see who holds it, while
// this test's own listener (root, the same user) is foreign and named. As
// any other user the test skips, since another user's fd table is unreadable
// there and the hidden-holder path, not the rule, would decide.
func TestAttribute_SameUserRuleAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: only root can read another user's fd table, which is what the rule must hold back")
	}
	uid := 65534 // nobody
	if v := os.Getenv("PROC_COMPOSE_TEST_OTHER_UID"); v != "" {
		var err error
		if uid, err = strconv.Atoi(v); err != nil {
			t.Fatal(err)
		}
	}
	// The helper runs as uid, so the test binary and the port file's
	// directory must be reachable for it.
	dir, err := os.MkdirTemp("", "pc-sameuser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	portFile := filepath.Join(dir, "port")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttestHelperProcess$", "--", "127.0.0.1:0", portFile, "serve")
	cmd.Env = append(os.Environ(), attestHelperEnv+"=serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper as uid %d: %v", uid, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	var port int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(portFile); err == nil {
			port, _ = strconv.Atoi(string(b))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper running as uid %d never listened", uid)
		}
	}
	if got := processEUID(cmd.Process.Pid); got != uid {
		t.Fatalf("helper euid = %d, want %d", got, uid)
	}
	pgid := startIdleGroup(t)

	conn, c := dialProbe(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	got := attributePGID(c, pgid)
	if got.Result != ipc.OwnerUnattributable || got.PID != 0 || got.Command != "" || !strings.Contains(got.Reason, "another user") {
		t.Fatalf("listener of uid %d: attribution = %+v, want unattributable and unnamed", uid, got)
	}
	server := requestPID(t, conn)
	t.Logf("uid %d listener (served by PID %d, visible to root): %+v", uid, server, got)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveOwnPID(ln, false)
	_, c = dialProbe(t, ln.Addr().String())
	own := attributePGID(c, pgid)
	if own.Result != ipc.OwnerForeign || own.PID != os.Getpid() || own.Command == "" {
		t.Fatalf("root's own listener: attribution = %+v, want foreign and named (PID %d)", own, os.Getpid())
	}
	t.Logf("same-user (root) listener: %+v", own)
}

// TestProcessEUID reads this process's effective uid the way the same-user
// rule reads a holder's.
func TestProcessEUID(t *testing.T) {
	if got := processEUID(os.Getpid()); got != os.Geteuid() {
		t.Fatalf("processEUID(self) = %d, want %d", got, os.Geteuid())
	}
	if got := processEUID(1 << 30); got != -1 {
		t.Fatalf("processEUID(no such pid) = %d, want -1", got)
	}
}

// TestHTTPOnce_CancellationDuringRealAttribution is the procfs variant of
// TestHTTPOnce_CancellationDuringAttribution: the request is cancelled while
// the real attribution reads the socket table and fd lists (run with -race).
func TestHTTPOnce_CancellationDuringRealAttribution(t *testing.T) {
	addr := foreignServer(t)
	pgid := startIdleGroup(t)
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		entered, done := make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := httpOnce(ctx, "http://"+addr+"/", func(c probeConn) *ipc.EndpointOwner {
				close(entered)
				owner := attributePGID(c, pgid)
				return &owner
			})
			done <- err
		}()
		<-entered
		cancel()
		<-done
	}
	// Let the last dial goroutines finish, without synchronising with them.
	time.Sleep(200 * time.Millisecond)
}

// TestAttribute_ReusePortFollowsTheAcceptedSocket covers two processes on one
// port: each connection is attributed to the process that accepted it, not
// to "whoever listens on the port".
func TestAttribute_ReusePortFollowsTheAcceptedSocket(t *testing.T) {
	pgid, port := startAttestGroup(t, "127.0.0.1:0", "reuseport")
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := listenForAttest(addr, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveOwnPID(ln, false)
	seen := map[string]int{}
	for i := 0; i < 32; i++ {
		conn, c := dialProbe(t, addr)
		server := requestPID(t, conn)
		got := attributePGID(c, pgid)
		want := ipc.OwnerOwned
		if server == os.Getpid() {
			want = ipc.OwnerForeign
		}
		requireOwner(t, got, want, server)
		seen[want]++
	}
	t.Logf("connections by holder: %v", seen)
}

// TestAttribute_UnreadableHolderIsUnattributable uses a listener another user
// holds (a system resolver or daemon), whose fd table this process cannot
// read: the holder is unknown, which must never read as foreign.
func TestAttribute_UnreadableHolderIsUnattributable(t *testing.T) {
	var candidates []string
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		entries, _ := readSocketTable(path)
		for _, e := range entries {
			if e.state != tcpListen || e.uid == os.Getuid() || e.inode == 0 {
				continue
			}
			switch addr := e.local.Addr(); {
			case addr.IsLoopback():
				candidates = append(candidates, e.local.String())
			case addr.IsUnspecified():
				candidates = append(candidates, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(e.local.Port()))))
			}
		}
	}
	var conn net.Conn
	var target string
	for _, candidate := range candidates {
		if c, err := net.DialTimeout("tcp", candidate, 2*time.Second); err == nil {
			conn, target = c, candidate
			break
		}
	}
	if conn == nil {
		t.Skipf("no reachable loopback listener owned by another user on this host (candidates %v)", candidates)
	}
	defer conn.Close()
	c, _ := probeConnOf(conn)
	got := attributePGID(c, startIdleGroup(t))
	if os.Getuid() == 0 {
		t.Skip("running as root: every holder is visible")
	}
	if got.Result != ipc.OwnerUnattributable {
		t.Fatalf("attribution of %s = %+v, want unattributable", target, got)
	}
	t.Logf("%s: %s", target, got.Reason)
}

// TestRun_ReadinessOwnerIsRecorded runs the full path through the runner: a
// foreign listener still makes the process ready (attribution reports, it
// does not decide), but the state and the journal say who answered; a
// process that serves its own probe is owned.
func TestRun_ReadinessOwnerIsRecorded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveOwnPID(ln, false)
	foreign := ln.Addr().String()

	dir := t.TempDir()
	portFile := filepath.Join(dir, "port")
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	own := free.Addr().String()
	free.Close()
	r := makeRunner(map[string]config.Process{
		"impostor": {Cmd: helperCmd("sleep", "30s"), Restart: "never", ReadyWhen: &config.ReadyWhen{HTTP: "http://" + foreign + "/"}},
		"tcp":      {Cmd: helperCmd("sleep", "30s"), Restart: "never", ReadyWhen: &config.ReadyWhen{TCP: foreign}},
		"server": {
			Cmd: attestCommand(own, portFile, "serve"), Restart: "never",
			Env:        map[string]string{attestHelperEnv: "serve"},
			ReadyWhen:  &config.ReadyWhen{HTTP: "http://" + own + "/"},
			HealthWhen: &config.ReadyWhen{TCP: own}, HealthInterval: 1,
		},
	})
	// emit serialises journal writes, and the journal is read after Run.
	var journal bytes.Buffer
	r.Journal, r.Output, r.Silent = &journal, io.Discard, true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stop := sync.OnceFunc(func() { cancel(); <-done })
	defer stop()

	for _, name := range []string{"impostor", "tcp"} {
		if !waitForProcState(t, r, name, func(s ipc.ProcState) bool { return s.Ready && s.ReadinessOwner != nil }, 5*time.Second) {
			t.Fatalf("%s: no attributed readiness: %+v", name, snapshotOf(r, name))
		}
		got := snapshotOf(r, name).ReadinessOwner
		if got.Result != ipc.OwnerForeign || got.PID != os.Getpid() || got.Address != foreign {
			t.Fatalf("%s: readiness_owner = %+v, want foreign PID %d at %s", name, got, os.Getpid(), foreign)
		}
	}

	served := func(s ipc.ProcState) bool {
		return s.Ready && s.ReadinessOwner != nil && s.HealthOwner != nil
	}
	if !waitForProcState(t, r, "server", served, 10*time.Second) {
		t.Fatalf("server: no attributed readiness and health: %+v", snapshotOf(r, "server"))
	}
	conn, _ := dialProbe(t, own)
	server := requestPID(t, conn)
	st := snapshotOf(r, "server")
	for what, got := range map[string]*ipc.EndpointOwner{"readiness_owner": st.ReadinessOwner, "health_owner": st.HealthOwner} {
		if got.Result != ipc.OwnerOwned || got.PID != server || got.Address != own {
			t.Fatalf("server: %s = %+v, want owned by PID %d at %s", what, got, server, own)
		}
	}

	stop()
	if !strings.Contains(journal.String(), `"code":"readiness.foreign_listener"`) {
		t.Fatalf("journal has no readiness.foreign_listener event:\n%s", journal.String())
	}
}

func benchmarkAttribute(b *testing.B, foreign bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	go serveOwnPID(ln, false)
	addr := ln.Addr().String()
	pgid := syscall.Getpgrp()
	if foreign {
		cmd := exec.Command("sleep", "60")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			b.Fatal(err)
		}
		defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
		pgid = cmd.Process.Pid
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	c, _ := probeConnOf(conn)
	want := ipc.OwnerOwned
	if foreign {
		want = ipc.OwnerForeign
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := attributePGID(c, pgid); got.Result != want {
			b.Fatalf("attribution = %+v, want %s", got, want)
		}
	}
}

// BenchmarkAttribute_Owned measures the common path: the answering socket is
// held by a member of the probed group (here, the benchmark's own group).
func BenchmarkAttribute_Owned(b *testing.B) { benchmarkAttribute(b, false) }

// BenchmarkAttribute_Foreign measures the walk of every visible process that
// a socket no member holds requires.
func BenchmarkAttribute_Foreign(b *testing.B) { benchmarkAttribute(b, true) }
