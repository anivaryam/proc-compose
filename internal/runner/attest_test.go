package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// TestAnsweringSockets_OnlyProvableAnswers pins which socket answered a
// connection, read right after it was dialed: the server's own row when it
// has an identity; the listener only while the row is still queued for
// accept; nothing for a row the server already closed (its listener may
// belong to a successor by now) or for a connection with no server row here
// (a remote or container address, where a local listener on the same port
// is unrelated).
func TestAnsweringSockets_OnlyProvableAnswers(t *testing.T) {
	c := probeConn{local: ap("127.0.0.1:50000"), remote: ap("127.0.0.1:3000")}
	listeners := []socketEntry{
		{local: ap("127.0.0.1:3000"), remote: ap("0.0.0.0:0"), state: tcpListen, uid: 5, inode: 10},
		{local: ap("127.0.0.1:3000"), remote: ap("127.0.0.1:50001"), state: tcpEstablished, inode: 20}, // another client
	}
	row := func(state byte, inode uint64) []socketEntry {
		return append([]socketEntry{{local: ap("127.0.0.1:3000"), remote: ap("127.0.0.1:50000"), state: state, uid: 7, inode: inode}}, listeners...)
	}
	for _, tc := range []struct {
		name   string
		table  []socketEntry
		want   []uint64
		uid    int
		reason string
	}{
		{"accepted", row(tcpEstablished, 21), []uint64{21}, 7, ""},
		{"accepted and closed by the client", row(0x08 /* CLOSE_WAIT */, 21), []uint64{21}, 7, ""},
		{"queued for accept", row(tcpEstablished, 0), []uint64{10}, 5, ""},
		{"handshake in progress", row(tcpSynRecv, 0), []uint64{10}, 5, ""},
		{"closed by the server", row(0x05 /* FIN_WAIT2 */, 0), nil, 0, "already closed"},
		{"time wait", row(0x06, 0), nil, 0, "already closed"},
		{"no server row here", listeners, nil, 0, "does not end in this network namespace"},
		{"queued with no listener", []socketEntry{{local: ap("127.0.0.1:3000"), remote: ap("127.0.0.1:50000"), state: tcpEstablished}}, nil, 0, "no listener"},
	} {
		got, uid, reason := answeringSockets(tc.table, c)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) || uid != tc.uid || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: answeringSockets = %v, uid %d, %q; want %v, uid %d, reason containing %q", tc.name, got, uid, reason, tc.want, tc.uid, tc.reason)
		}
	}
	// A remote server (a container address) answered; a local process
	// listening on the same port on every address must not be named.
	remote := probeConn{local: ap("172.17.0.1:50000"), remote: ap("172.17.0.2:3000")}
	wildcard := []socketEntry{
		{local: ap("0.0.0.0:3000"), state: tcpListen, inode: 30},
		{local: ap("172.17.0.1:50000"), remote: ap("172.17.0.2:3000"), state: tcpEstablished, inode: 31}, // the probe's own side
	}
	if got, _, reason := answeringSockets(wildcard, remote); got != nil || !strings.Contains(reason, "does not end in this network namespace") {
		t.Fatalf("remote answer attributed to %v (%q), want no local socket", got, reason)
	}
}

// TestListenerSockets_MostSpecificListenerAnswers pins the kernel's choice:
// an exact address beats its family's wildcard, which beats a dual-stack
// IPv6 wildcard; listeners on one level (SO_REUSEPORT) are all candidates.
func TestListenerSockets_MostSpecificListenerAnswers(t *testing.T) {
	c := probeConn{local: ap("127.0.0.1:50000"), remote: ap("127.0.0.1:3000")}
	dual := socketEntry{local: ap("[::]:3000"), state: tcpListen, inode: 3}
	any4 := socketEntry{local: ap("0.0.0.0:3000"), state: tcpListen, inode: 2}
	exact := socketEntry{local: ap("127.0.0.1:3000"), state: tcpListen, inode: 1}
	exact2 := socketEntry{local: ap("127.0.0.1:3000"), state: tcpListen, inode: 4}
	other := []socketEntry{
		{local: ap("127.0.0.1:3001"), state: tcpListen, inode: 9}, // another port
		{local: ap("10.0.0.5:3000"), state: tcpListen, inode: 8},  // another address
	}
	for _, tc := range []struct {
		name  string
		table []socketEntry
		want  []uint64
	}{
		{"dual-stack only", append([]socketEntry{dual}, other...), []uint64{3}},
		{"ipv4 wildcard over dual-stack", []socketEntry{dual, any4}, []uint64{2}},
		{"exact over wildcards", []socketEntry{dual, any4, exact}, []uint64{1}},
		{"reuseport", []socketEntry{exact, any4, exact2}, []uint64{1, 4}},
		{"none", other, nil},
	} {
		got, _, ok := listenerSockets(tc.table, c)
		if ok != (tc.want != nil) || len(got) != len(tc.want) {
			t.Fatalf("%s: listenerSockets = %v, %v; want %v", tc.name, got, ok, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: listenerSockets = %v; want %v", tc.name, got, tc.want)
			}
		}
	}
	// An IPv6 probe is never answered by an IPv4 wildcard.
	c6 := probeConn{local: ap("[::1]:50000"), remote: ap("[::1]:3000")}
	if got, _, _ := listenerSockets([]socketEntry{any4, dual}, c6); len(got) != 1 || got[0] != 3 {
		t.Fatalf("IPv6 probe: listenerSockets = %v; want [3]", got)
	}
}

// TestClassifyHolders_IsAsymmetric pins the claim rule: owned and foreign
// need a positively identified holder on that side for every candidate
// socket; anything less is unattributable.
func TestClassifyHolders_IsAsymmetric(t *testing.T) {
	member := socketHolder{pid: 10, command: "node", member: true, sameUser: true}
	outsider := socketHolder{pid: 20, command: "node", sameUser: true}
	otherUser := socketHolder{pid: 30}
	for _, tc := range []struct {
		name    string
		inodes  []uint64
		holders map[uint64][]socketHolder
		want    string
		pid     int
	}{
		{"member", []uint64{1}, map[uint64][]socketHolder{1: {member}}, ipc.OwnerOwned, 10},
		{"outsider", []uint64{1}, map[uint64][]socketHolder{1: {outsider}}, ipc.OwnerForeign, 20},
		{"shared descriptor counts as ours", []uint64{1}, map[uint64][]socketHolder{1: {outsider, member}}, ipc.OwnerOwned, 10},
		{"nobody visible", []uint64{1}, map[uint64][]socketHolder{}, ipc.OwnerUnattributable, 0},
		{"reuseport all ours", []uint64{1, 2}, map[uint64][]socketHolder{1: {member}, 2: {member}}, ipc.OwnerOwned, 10},
		{"reuseport all theirs", []uint64{1, 2}, map[uint64][]socketHolder{1: {outsider}, 2: {outsider}}, ipc.OwnerForeign, 20},
		{"reuseport split", []uint64{1, 2}, map[uint64][]socketHolder{1: {member}, 2: {outsider}}, ipc.OwnerUnattributable, 0},
		{"reuseport partly hidden", []uint64{1, 2}, map[uint64][]socketHolder{1: {member}}, ipc.OwnerUnattributable, 0},
		{"no socket", nil, nil, ipc.OwnerUnattributable, 0},
		{"another user's process is not named", []uint64{1}, map[uint64][]socketHolder{1: {otherUser}}, ipc.OwnerUnattributable, 0},
		{"a member sharing it with another user is ours", []uint64{1}, map[uint64][]socketHolder{1: {otherUser, member}}, ipc.OwnerOwned, 10},
		{"reuseport, one side another user's", []uint64{1, 2}, map[uint64][]socketHolder{1: {outsider}, 2: {otherUser}}, ipc.OwnerUnattributable, 0},
	} {
		got := classifyHolders(tc.inodes, tc.holders, "no visible holder")
		if got.Result != tc.want || got.PID != tc.pid || (got.Result == ipc.OwnerUnattributable) != (got.Reason != "") ||
			(got.Result == ipc.OwnerUnattributable && (got.Command != "" || strings.Contains(got.Reason, "PID") || strings.Contains(got.Reason, "30"))) {
			t.Errorf("%s: classifyHolders = %+v, want %s by PID %d", tc.name, got, tc.want, tc.pid)
		}
	}
}

// TestAttributeProbe_UnrecordedConnectionIsUnattributable covers a passing
// probe whose connection was not captured: nothing is claimed.
func TestAttributeProbe_UnrecordedConnectionIsUnattributable(t *testing.T) {
	if got := attributeProbe(probeConn{}, nil); got.Result != ipc.OwnerUnattributable || got.Reason == "" {
		t.Fatalf("attributeProbe(no connection) = %+v", got)
	}
}

// attributedHere reports whether this platform attributes socket owners
// (procfs on Linux, the TCP table and Job Objects on Windows); elsewhere
// every answer is unattributable.
func attributedHere() bool { return runtime.GOOS == "linux" || runtime.GOOS == "windows" }

// TestHTTPOnce_CancellationDuringAttribution is the regression for a
// request cancelled while its connection is still being attributed: the
// transport's dial goroutine finishes the attribution after httpOnce has
// returned, and must write nothing httpOnce shares (run with -race). The
// test deliberately does not synchronise with the end of the attribution
// (it only sleeps past it), since any such synchronisation would order the
// late write after httpOnce's return and hide the race. The cancelled
// attempt also fails promptly instead of waiting for the attribution.
func TestHTTPOnce_CancellationDuringAttribution(t *testing.T) {
	const attribution = 150 * time.Millisecond
	addr := foreignServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	type result struct {
		owner *ipc.EndpointOwner
		err   error
	}
	done := make(chan result, 1)
	go func() {
		owner, err := httpOnce(ctx, "http://"+addr+"/", func(probeConn) *ipc.EndpointOwner {
			close(entered)
			time.Sleep(attribution)
			return &ipc.EndpointOwner{Result: ipc.OwnerOwned}
		})
		done <- result{owner, err}
	}()
	<-entered
	cancelled := time.Now()
	cancel()
	r := <-done
	if r.err == nil || r.owner != nil {
		t.Fatalf("cancelled attempt = %+v, %v; want an error and no owner", r.owner, r.err)
	}
	if waited := time.Since(cancelled); waited >= attribution {
		t.Fatalf("httpOnce returned %s after cancellation, waiting for the attribution", waited)
	}
	time.Sleep(2 * attribution) // let the dial goroutine finish, unsynchronised
}

// TestHTTPOnce_OwnerIsTheAnsweringConnections pins that the owner httpOnce
// returns is the attribution of the connection that carried the answer.
func TestHTTPOnce_OwnerIsTheAnsweringConnections(t *testing.T) {
	addr := foreignServer(t)
	var dialed []probeConn
	owner, err := httpOnce(context.Background(), "http://"+addr+"/", func(c probeConn) *ipc.EndpointOwner {
		dialed = append(dialed, c)
		return &ipc.EndpointOwner{Result: ipc.OwnerForeign, Address: c.remote.String(), PID: len(dialed)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialed) != 1 || owner == nil || owner.PID != 1 || owner.Address != addr {
		t.Fatalf("owner = %+v after %d dials, want the single dial's attribution for %s", owner, len(dialed), addr)
	}
}

// TestConnOwner_SeesThroughTLS pins that an https probe's attribution is
// found under the TLS layer the transport puts over the dialed connection.
func TestConnOwner_SeesThroughTLS(t *testing.T) {
	want := &ipc.EndpointOwner{Result: ipc.OwnerOwned}
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	dialed := &attributedConn{Conn: raw, owner: want}
	if got := connOwner(tls.Client(dialed, &tls.Config{ServerName: "probe"})); got != want {
		t.Fatalf("connOwner(tls over dialed) = %+v, want %+v", got, want)
	}
	if got := connOwner(dialed); got != want {
		t.Fatalf("connOwner(dialed) = %+v", got)
	}
	if got := connOwner(raw); got != nil {
		t.Fatalf("connOwner(unattributed) = %+v, want nil", got)
	}
}

// foreignServer runs an HTTP server in the test process, outside every
// managed process tree, and returns its address.
func foreignServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// freeAddr returns a loopback address with a port nothing listens on now.
func freeAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// servedPID asks the serve-http helper at addr for its PID.
func servedPID(t *testing.T, addr string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatalf("serve-http answered %q", body)
	}
	return pid
}

// runHandle is a Runner running in the background, with its journal.
type runHandle struct {
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
	err     error
	journal *bytes.Buffer
}

// runUntil runs r in the background until stop or wait; cleanup stops it.
func runUntil(t *testing.T, r *Runner) *runHandle {
	t.Helper()
	h := &runHandle{done: make(chan error, 1), journal: &bytes.Buffer{}}
	r.Journal, r.Output, r.Silent = h.journal, io.Discard, true
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- r.Run(ctx) }()
	t.Cleanup(func() { h.stop() })
	return h
}

// stop cancels the run and returns Run's error and the journal.
func (h *runHandle) stop() (error, string) {
	h.cancel()
	h.once.Do(func() { h.err = <-h.done })
	return h.err, h.journal.String()
}

// wait returns once Run returns on its own, without cancelling it, so a
// failure it reports is never masked by the cancellation stop would cause.
func (h *runHandle) wait(t *testing.T, timeout time.Duration) (error, string) {
	t.Helper()
	select {
	case err := <-h.done:
		h.once.Do(func() { h.err = err })
	case <-time.After(timeout):
		t.Fatalf("Run did not return on its own within %s", timeout)
	}
	return h.err, h.journal.String()
}

// TestRun_ForeignListenerReadinessIsAttributed is the false-readiness
// regression: a process that never binds becomes ready on the first attempt
// because another process answers its probe address. With the default
// foreign_listener: allow, attribution reports it and does not change the
// verdict: foreign, naming this test process, where sockets are attributed,
// and unattributable with a reason elsewhere.
func TestRun_ForeignListenerReadinessIsAttributed(t *testing.T) {
	addr := foreignServer(t)
	for kind, rw := range map[string]*config.ReadyWhen{"tcp": {TCP: addr}, "http": {HTTP: "http://" + addr + "/"}} {
		t.Run(kind, func(t *testing.T) {
			r := makeRunner(map[string]config.Process{
				"api": {Cmd: helperCmd("sleep", "30s"), Restart: "never", ReadyTimeout: 5, ReadyWhen: rw},
			})
			run := runUntil(t, r)
			if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Ready }, 3*time.Second) {
				t.Fatal("readiness did not pass against the foreign listener")
			}
			got := snapshotOf(r, "api").ReadinessOwner
			if got == nil || got.Address != addr {
				t.Fatalf("readiness_owner = %+v, want one at %s", got, addr)
			}
			if attributedHere() {
				if got.Result != ipc.OwnerForeign || got.PID != os.Getpid() || got.Command == "" {
					t.Fatalf("readiness_owner = %+v, want foreign PID %d with its name", got, os.Getpid())
				}
				if _, journal := run.stop(); !strings.Contains(journal, `"code":"readiness.foreign_listener"`) {
					t.Fatal("no readiness.foreign_listener event")
				}
			} else if got.Result != ipc.OwnerUnattributable || !strings.Contains(got.Reason, runtime.GOOS) {
				t.Fatalf("readiness_owner = %+v, want unattributable on %s", got, runtime.GOOS)
			}
		})
	}
}

// TestRun_OwnListenerIsOwnedEvenUnderReject pins the other side: a process
// that serves its own probe address is owned, by the server itself (a child
// of the shell on Windows), for readiness and health, and reject never
// refuses it.
func TestRun_OwnListenerIsOwnedEvenUnderReject(t *testing.T) {
	addr := freeAddr(t)
	r := makeRunner(map[string]config.Process{
		"api": {
			Cmd: helperCmd("serve-http", addr), Restart: "never", ReadyTimeout: 10,
			ReadyWhen:       &config.ReadyWhen{HTTP: "http://" + addr + "/"},
			HealthWhen:      &config.ReadyWhen{TCP: addr},
			HealthInterval:  1,
			ForeignListener: config.ForeignListenerReject,
		},
	})
	runUntil(t, r)
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Ready && s.HealthOwner != nil }, 10*time.Second) {
		t.Fatalf("api never became ready and checked: %+v", snapshotOf(r, "api"))
	}
	server := servedPID(t, addr)
	st := snapshotOf(r, "api")
	if st.Health != healthHealthy {
		t.Fatalf("health = %q (%s), want healthy", st.Health, st.HealthError)
	}
	for what, got := range map[string]*ipc.EndpointOwner{"readiness_owner": st.ReadinessOwner, "health_owner": st.HealthOwner} {
		switch {
		case got.Address != addr:
			t.Fatalf("%s = %+v, want address %s", what, got, addr)
		case attributedHere() && (got.Result != ipc.OwnerOwned || got.PID != server):
			t.Fatalf("%s = %+v, want owned by the server, PID %d", what, got, server)
		case !attributedHere() && got.Result != ipc.OwnerUnattributable:
			t.Fatalf("%s = %+v, want unattributable on %s", what, got, runtime.GOOS)
		}
	}
}

// TestRun_ForeignListenerRejectHoldsReadiness is the enforcement contract:
// with foreign_listener: reject, a foreign answer does not make the process
// ready; readiness times out and the error names the holder.
func TestRun_ForeignListenerRejectHoldsReadiness(t *testing.T) {
	if !attributedHere() {
		t.Skip("socket owners are not attributed on " + runtime.GOOS + "; reject never refuses an unattributable answer")
	}
	addr := foreignServer(t)
	for kind, rw := range map[string]*config.ReadyWhen{"tcp": {TCP: addr}, "http": {HTTP: "http://" + addr + "/"}} {
		t.Run(kind, func(t *testing.T) {
			r := makeRunner(map[string]config.Process{
				"api": {Cmd: helperCmd("sleep", "30s"), Restart: "never", ReadyTimeout: 2, ReadyWhen: rw, ForeignListener: config.ForeignListenerReject},
			})
			run := runUntil(t, r)
			// restart: never, so the refused process fails and Run returns
			// its error by itself; cancelling could mask it.
			err, journal := run.wait(t, 15*time.Second)
			st := snapshotOf(r, "api")
			want := fmt.Sprintf("answered at %s by PID %d", addr, os.Getpid())
			if st.Ready || !strings.Contains(st.LastError, "timed out") || !strings.Contains(st.LastError, want) {
				t.Fatalf("ready=%v last_error=%q, want a timeout naming %q", st.Ready, st.LastError, want)
			}
			if o := st.ReadinessOwner; o == nil || o.Result != ipc.OwnerForeign || o.PID != os.Getpid() {
				t.Fatalf("readiness_owner after the refusal = %+v, want the refused foreign answer", o)
			}
			if err == nil || strings.Count(journal, `"code":"readiness.foreign_listener"`) != 1 || !strings.Contains(journal, "foreign_listener: reject") {
				t.Fatalf("Run err = %v; want a failure and exactly one reject warning in the journal", err)
			}
		})
	}
}

// TestRun_ForeignListenerRejectFailsHealth covers health_when: under
// reject, a check a foreign process answers is a failed check, so three in
// a row make the service unhealthy with the holder in health_error.
func TestRun_ForeignListenerRejectFailsHealth(t *testing.T) {
	if !attributedHere() {
		t.Skip("socket owners are not attributed on " + runtime.GOOS)
	}
	own, foreign := freeAddr(t), foreignServer(t)
	r := makeRunner(map[string]config.Process{
		"api": {
			Cmd: helperCmd("serve-http", own), Restart: "never", ReadyTimeout: 10,
			ReadyWhen:       &config.ReadyWhen{HTTP: "http://" + own + "/"},
			HealthWhen:      &config.ReadyWhen{TCP: foreign},
			HealthInterval:  1,
			ForeignListener: config.ForeignListenerReject,
		},
	})
	run := runUntil(t, r)
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Health == healthUnhealthy }, 15*time.Second) {
		t.Fatalf("health was not refused: %+v", snapshotOf(r, "api"))
	}
	st := snapshotOf(r, "api")
	if want := fmt.Sprintf("tcp: answered at %s by PID %d", foreign, os.Getpid()); !strings.Contains(st.HealthError, want) {
		t.Fatalf("health_error = %q, want it to contain %q", st.HealthError, want)
	}
	if st.HealthOwner == nil || st.HealthOwner.Result != ipc.OwnerForeign {
		t.Fatalf("health_owner = %+v, want foreign", st.HealthOwner)
	}
	if _, journal := run.stop(); strings.Count(journal, `"code":"health.foreign_listener"`) != 1 {
		t.Fatal("want exactly one health.foreign_listener event for one holder")
	}
}

// handoffResponder is a foreign server that answers one probe only after it
// has given its port away: it accepts the connection, reads the request,
// closes its listener so the managed process can bind the port, waits until
// that successor serves, and only then answers 200. With keepAlive it keeps
// the connection open and answers further requests on it.
func handoffResponder(t *testing.T, keepAlive bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	done := make(chan struct{})
	t.Cleanup(func() { ln.Close(); <-done })
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		buf := make([]byte, 4096)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		ln.Close()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				c.Close()
				break
			}
		}
		header := "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n"
		if !keepAlive {
			header += "Connection: close\r\n"
		}
		for {
			if _, err := io.WriteString(conn, header+"\r\nFOREIGN"); err != nil || !keepAlive {
				return
			}
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	return addr
}

// TestRun_HandoffAnswerBelongsToItsResponder is the regression for a foreign
// server that answers after handing its port to the managed process. The
// answer came from the foreign server, so it must never be credited to the
// successor now holding the port; and under reject, the refused answer's
// connection must not be reused, so the next attempt reaches the healthy
// successor. Both hold whether the foreign server closes the connection or
// keeps it alive.
func TestRun_HandoffAnswerBelongsToItsResponder(t *testing.T) {
	if !attributedHere() {
		t.Skip("socket owners are not attributed on " + runtime.GOOS)
	}
	for _, keepAlive := range []bool{false, true} {
		for _, policy := range []string{config.ForeignListenerAllow, config.ForeignListenerReject} {
			t.Run(fmt.Sprintf("keepalive=%v/%s", keepAlive, policy), func(t *testing.T) {
				addr := handoffResponder(t, keepAlive)
				r := makeRunner(map[string]config.Process{
					"api": {Cmd: helperCmd("serve-http-retry", addr), Restart: "never", ReadyTimeout: 10,
						ReadyWhen: &config.ReadyWhen{HTTP: "http://" + addr + "/"}, ForeignListener: policy},
				})
				runUntil(t, r)
				if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Ready || s.State == "failed" }, 15*time.Second) {
					t.Fatalf("api never settled: %+v", snapshotOf(r, "api"))
				}
				st := snapshotOf(r, "api")
				if !st.Ready || st.ReadinessOwner == nil {
					t.Fatalf("api not ready: state=%s last_error=%q", st.State, st.LastError)
				}
				got := st.ReadinessOwner
				if policy == config.ForeignListenerAllow {
					// The 200 that made it ready was the foreign server's.
					if got.Result != ipc.OwnerForeign || got.PID != os.Getpid() {
						t.Fatalf("readiness_owner = %+v, want foreign PID %d (the responder), not the successor", got, os.Getpid())
					}
					return
				}
				// Under reject the foreign answer was refused and a later
				// attempt, on a new connection, reached the successor.
				if successor := servedPID(t, addr); got.Result != ipc.OwnerOwned || got.PID != successor {
					t.Fatalf("readiness_owner = %+v, want owned by the successor PID %d", got, successor)
				}
			})
		}
	}
}
