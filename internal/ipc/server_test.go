package ipc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestServerClient_DoubleShutdown(t *testing.T) {
	c := &serverClient{
		send: make(chan []byte, 4),
	}
	// First shutdown should succeed without panic.
	c.shutdown()
	// Second shutdown should be a safe no-op.
	c.shutdown()
}

func TestServerClient_ConcurrentShutdown(t *testing.T) {
	c := &serverClient{
		send: make(chan []byte, 4),
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.shutdown()
		}()
	}
	wg.Wait()
}

// testSocketPath returns a temp socket/pipe address valid for the current OS.
func testSocketPath(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\` + name
	}
	dir, err := os.MkdirTemp("", "pc-test-*")
	if err != nil {
		t.Fatalf("temp socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// swapStderrSink points the package's warning sink at w and returns the
// function that restores it.
//
// The package's own mutex makes the swap race-free: a server goroutine
// writing a warning and this test reading the capture are ordered by the
// same lock. Swapping os.Stderr instead would race with a goroutine that
// writes to it.
func swapStderrSink(w io.Writer) func() {
	stderrMu.Lock()
	old := stderrSink
	stderrSink = w
	stderrMu.Unlock()
	return func() {
		stderrMu.Lock()
		stderrSink = old
		stderrMu.Unlock()
	}
}

// warningText returns the warnings captured so far.
func warningText(w *bytes.Buffer) string {
	stderrMu.Lock()
	defer stderrMu.Unlock()
	return w.String()
}

// waitForWarning waits for want to appear in the captured warnings. The
// server's goroutine does not announce the write, so the wait is a bounded
// poll rather than a barrier.
func waitForWarning(w *bytes.Buffer, want string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(warningText(w), want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// commandFrame builds a command frame of exactly size bytes, the newline
// included.
func commandFrame(size int) []byte {
	const prefix = `{"type":"command","cmd":{"action":"restart","process":"`
	const suffix = `"}}` + "\n"
	if size < len(prefix)+len(suffix) {
		return nil
	}
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

// TestDialCause pins the cause extraction used by DialTimeout: the errno
// when the chain holds one, the deepest cause when it does not, and never the
// raw *net.OpError — that error carries the unsanitized socket path.
func TestDialCause(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENOENT}}
	if got := dialCause(opErr); got != syscall.ENOENT {
		t.Fatalf("dialCause = %v, want syscall.ENOENT", got)
	}

	// A chain without an errno — a dial timeout's cause is net's unexported
	// timeout error — yields the deepest cause.
	inner := errors.New("i/o timeout")
	timeout := &net.OpError{Op: "dial", Net: "unix", Err: inner}
	if got := dialCause(timeout); got != inner {
		t.Fatalf("dialCause = %v, want the deepest cause %v", got, inner)
	}
	if _, ok := dialCause(timeout).(*net.OpError); ok {
		t.Fatal("dialCause returned the raw *net.OpError, which carries the unsanitized path")
	}
}

// TestClient_RecvTimeoutSilentServer pins the deadline guarantee: a server
// that accepts the connection and then stays silent must make RecvTimeout
// return ErrRecvTimeout instead of blocking forever.
func TestClient_RecvTimeoutSilentServer(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-silent-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	// Drain snapshot so the client is connected but has nothing left to read.
	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}

	start := time.Now()
	if _, err := c.RecvTimeout(200 * time.Millisecond); !errors.Is(err, ErrRecvTimeout) {
		t.Fatalf("RecvTimeout on a silent server = %v, want ErrRecvTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("RecvTimeout took %s, expected it to give up near 200ms", elapsed)
	}
}

func TestServer_AckSignalsBusyWhenChannelFull(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-ack-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	// Fill the command channel so subsequent sends drop.
	for i := 0; i < cap(s.cmdCh); i++ {
		s.cmdCh <- Command{Action: "restart", Process: "filler"}
	}

	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// Drain snapshot.
	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}

	if err := c.Send(Command{Action: "restart", Process: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, err := c.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == TypeAck {
			if ev.Ack == "ok" {
				t.Fatalf("expected non-ok ack when cmdCh is full, got %q", ev.Ack)
			}
			return // got busy ack — pass
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for ack")
		}
	}
}

func TestServer_AckOkWhenAccepted(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-ack-ok-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	// Drain commands as if we were the runner, replying "ok" so the
	// server can deliver a non-timeout ack to the client. Without this
	// drainer the server's Reply wait would time out at 30s.
	commands := make(chan Command, 1)
	stopRunner := make(chan struct{})
	go func() {
		for {
			select {
			case cmd := <-s.cmdCh:
				commands <- cmd
				if cmd.Reply != nil {
					cmd.Reply <- CommandResult{Status: "ok"}
				}
			case <-stopRunner:
				return
			}
		}
	}()
	defer close(stopRunner)

	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}

	if err := c.Send(Command{Action: "restart", Process: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for {
		ev, err := c.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == TypeAck {
			if ev.Ack != "ok" {
				t.Fatalf("expected ok ack, got %q", ev.Ack)
			}
			break
		}
	}

	// The barrier is released once the ack has been written, and reports it
	// as delivered.
	select {
	case cmd := <-commands:
		if err := cmd.WaitFlushed(2 * time.Second); err != nil {
			t.Fatalf("WaitFlushed after a delivered ack = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never received the command")
	}
}

// TestServer_OversizedFrameIsAnsweredAndTheConnectionKept is the #30
// regression. The 64 KiB inbound cap stays, but a frame over it is answered
// with an error acknowledgment the client can show, and the connection
// survives it: the commands queued behind it on the same connection are
// still processed. bufio.Scanner latched ErrTooLong and tore the connection
// down, silently dropping every one of them.
func TestServer_OversizedFrameIsAnsweredAndTheConnectionKept(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-oversized-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	commands := make(chan Command, 4)
	stopRunner := make(chan struct{})
	defer close(stopRunner)
	go func() {
		for {
			select {
			case cmd := <-s.cmdCh:
				commands <- cmd
				cmd.Reply <- CommandResult{Status: "ok"}
			case <-stopRunner:
				return
			}
		}
	}()

	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}

	var warnings bytes.Buffer
	restore := swapStderrSink(&warnings)
	defer restore()

	// One byte over the cap, then a frame that takes several buffer fills to
	// discard (the issue's 100 KiB case and more), each followed by a valid
	// command on the same connection.
	written := make(chan error, 1)
	go func() {
		for _, frame := range [][]byte{
			commandFrame(maxCommandFrameBytes + 1),
			commandFrame(64),
			commandFrame(4*maxCommandFrameBytes + 7),
			commandFrame(64),
		} {
			if _, err := c.conn.Write(frame); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()

	wantAcks := []string{"error", "ok", "error", "ok"}
	for i, want := range wantAcks {
		var ack Event
		for ack.Type != TypeAck {
			ack, err = c.RecvTimeout(10 * time.Second)
			if err != nil {
				t.Fatalf("ack %d: %v (the connection did not survive the oversized frame)", i, err)
			}
		}
		if ack.Ack != want {
			t.Fatalf("ack %d = %q (%s), want %q", i, ack.Ack, ack.AckDetail, want)
		}
		if want == "error" {
			// The client can show the reason: it names the condition and the
			// limit.
			if !strings.Contains(ack.AckDetail, "command frame too long") ||
				!strings.Contains(ack.AckDetail, strconv.Itoa(maxCommandFrameBytes)) {
				t.Fatalf("error ack detail = %q, want it to name the condition and the %d byte limit",
					ack.AckDetail, maxCommandFrameBytes)
			}
		}
	}
	if err := <-written; err != nil {
		t.Fatalf("write: %v", err)
	}

	// Only the valid commands reached the runner. Each was queued before its
	// reply, so both are buffered by now.
	if got := len(commands); got != 2 {
		t.Fatalf("the runner received %d commands, want the 2 valid ones", got)
	}
	for range 2 {
		if cmd := <-commands; len(cmd.Process) > 64 {
			t.Fatalf("an oversized frame reached the runner: %d byte process name", len(cmd.Process))
		}
	}

	// The daemon reports each discarded frame and names the limit. The peer
	// is not named: a Unix socket's peer address is empty.
	if !waitForWarning(&warnings, "command frame too long") {
		t.Fatalf("no diagnostic for the oversized frame; warnings: %q", warningText(&warnings))
	}
	msg := warningText(&warnings)
	t.Logf("daemon diagnostic: %s", msg)
	if got := strings.Count(msg, "command frame too long"); got != 2 {
		t.Fatalf("%d diagnostics for 2 oversized frames: %q", got, msg)
	}
	if !strings.Contains(msg, strconv.Itoa(maxCommandFrameBytes)) {
		t.Fatalf("the diagnostic does not name the %d byte cap: %q", maxCommandFrameBytes, msg)
	}
	if strings.Contains(msg, "peer") {
		t.Fatalf("the diagnostic names a peer it cannot identify: %q", msg)
	}
}

// TestServer_ProcessesFrameAtTheCap pins the boundary of the inbound cap: a
// frame of exactly the cap is still processed.
func TestServer_ProcessesFrameAtTheCap(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-cap-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	commands := make(chan Command, 1)
	go func() {
		select {
		case cmd := <-s.cmdCh:
			commands <- cmd
			if cmd.Reply != nil {
				cmd.Reply <- CommandResult{Status: "ok"}
			}
		case <-time.After(30 * time.Second):
		}
	}()

	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}

	frame := commandFrame(maxCommandFrameBytes)
	if len(frame) != maxCommandFrameBytes {
		t.Fatalf("frame = %d bytes, want %d", len(frame), maxCommandFrameBytes)
	}
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case cmd := <-commands:
		if cmd.Action != "restart" {
			t.Fatalf("processed command = %+v, want a restart", cmd)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a frame of exactly the cap was not processed")
	}
}

// TestServer_AckWriteFailureIsReported is the #31 regression. A failed
// acknowledgment write is reported on the daemon side and to the command's
// waiter: WaitFlushed says the verdict was not delivered, so the runner can
// act on it. It used to be released exactly as for a delivered verdict.
//
// The barrier is still released at once, not at its timeout: a peer that has
// gone away must not wedge the daemon for the whole shutdown budget.
func TestServer_AckWriteFailureIsReported(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-ack-fail-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	// The runner holds the command until the client is gone, so the ack is
	// written to a peer that has already disconnected.
	commands := make(chan Command, 1)
	release := make(chan struct{})
	go func() {
		cmd := <-s.cmdCh
		commands <- cmd
		<-release
		if cmd.Reply != nil {
			cmd.Reply <- CommandResult{Status: "ok"}
		}
	}()

	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}
	if err := c.Send(Command{Action: "restart", Process: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var cmd Command
	select {
	case cmd = <-commands:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never received the command")
	}

	var warnings bytes.Buffer
	restore := swapStderrSink(&warnings)
	defer restore()

	// The client goes away, then the runner replies: the ack write fails.
	c.Close()
	close(release)

	if !waitForWarning(&warnings, "failed to write ack") {
		t.Fatalf("no diagnostic for the failed ack write; warnings: %q", warningText(&warnings))
	}
	t.Logf("daemon diagnostic: %s", warningText(&warnings))

	// The barrier is released at once, and reports the verdict undelivered.
	const flushTimeout = 10 * time.Second
	start := time.Now()
	err = cmd.WaitFlushed(flushTimeout)
	if err == nil {
		t.Fatal("WaitFlushed after a failed ack write = nil: the verdict is reported delivered")
	}
	if elapsed := time.Since(start); elapsed > flushTimeout/2 {
		t.Fatalf("WaitFlushed took %s after the failed write; the barrier was held to its timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "write acknowledgment") {
		t.Fatalf("WaitFlushed = %v, want the write failure", err)
	}
	t.Logf("undelivered: %v", err)
}

// TestServer_ReplyTimeoutLeavesTheVerdictUndelivered pins the other way a
// verdict misses its client: the server gave up waiting and answered with a
// timeout error, so the verdict the runner hands back afterwards is never
// written. The ack write itself succeeds, but WaitFlushed must not read that
// as the verdict being delivered.
func TestServer_ReplyTimeoutLeavesTheVerdictUndelivered(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-reply-timeout-test"))
	s.replyTimeout = 100 * time.Millisecond
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

	commands := make(chan Command, 1)
	go func() { commands <- <-s.cmdCh }()

	c, err := Dial(s.socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Recv(); err != nil {
		t.Fatalf("snapshot Recv: %v", err)
	}
	if err := c.Send(Command{Action: "shutdown"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var cmd Command
	select {
	case cmd = <-commands:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never received the command")
	}

	ev, err := c.RecvTimeout(10 * time.Second)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Type != TypeAck || ev.Ack != "error" || !strings.Contains(ev.AckDetail, "timed out") {
		t.Fatalf("event = %+v, want the timeout error ack", ev)
	}

	// The runner's late verdict goes nowhere, and WaitFlushed says so.
	cmd.Reply <- CommandResult{Status: "partial", Message: "worker: could not terminate"}
	err = cmd.WaitFlushed(2 * time.Second)
	if err == nil {
		t.Fatal("WaitFlushed after a timeout ack = nil: a verdict the client never saw is reported delivered")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("WaitFlushed = %v, want it to name the timeout", err)
	}
}

// TestCommand_WaitFlushedWithoutAServer pins the two outcomes that involve no
// write: a command that did not come from a Server has no client to deliver
// to, and a server that has not finished within the timeout has not delivered.
func TestCommand_WaitFlushedWithoutAServer(t *testing.T) {
	var direct Command
	if err := direct.WaitFlushed(time.Hour); err != nil {
		t.Fatalf("WaitFlushed on a command with no server = %v, want nil", err)
	}

	pending := Command{flushed: &ackDelivery{done: make(chan struct{})}}
	if err := pending.WaitFlushed(10 * time.Millisecond); err == nil {
		t.Fatal("WaitFlushed past its timeout = nil, want the verdict reported undelivered")
	}
}

// snapshotWriteFailure fails its first write — the snapshot — the way a
// write that runs into its deadline does, reporting written bytes as sent.
// Later writes go through.
type snapshotWriteFailure struct {
	net.Conn
	written int
	failed  atomic.Bool
}

func (c *snapshotWriteFailure) Write(b []byte) (int, error) {
	if c.failed.CompareAndSwap(false, true) {
		return c.written, os.ErrDeadlineExceeded
	}
	return c.Conn.Write(b)
}

// TestServer_DropsAClientWhoseSnapshotWasCutShort pins the snapshot half of
// #31 when part of the snapshot was written. The write error used to be
// discarded and the client served on, although everything after the broken
// line would be read as its tail. The server now reports the failure,
// unregisters the client and closes the connection without reading from it.
func TestServer_DropsAClientWhoseSnapshotWasCutShort(t *testing.T) {
	s := NewServer("unused")
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()

	var warnings bytes.Buffer
	restore := swapStderrSink(&warnings)
	defer restore()

	handled := make(chan struct{})
	go func() {
		s.handle(&snapshotWriteFailure{Conn: serverSide, written: 10})
		close(handled)
	}()
	// Without the fix handle reads commands from the pipe, which nobody
	// writes to, and never returns.
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the server kept serving a client whose snapshot was cut short")
	}

	if msg := warningText(&warnings); !strings.Contains(msg, "failed to write snapshot") || !strings.Contains(msg, "dropping") {
		t.Fatalf("no diagnostic for the dropped connection; warnings: %q", msg)
	}
	s.mu.RLock()
	registered := len(s.clients)
	s.mu.RUnlock()
	if registered != 0 {
		t.Fatalf("%d clients still registered after the snapshot write failed", registered)
	}
	// The connection is closed, so the client sees the end of the stream.
	_ = clientSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := clientSide.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("client read after the dropped connection = %v, want io.EOF", err)
	}
}

// TestServer_ServesAClientWhoseSnapshotWasNotWritten pins the other snapshot
// failure: nothing was written, so the stream is still well framed. That is
// what a Windows named pipe does to a client that writes its command before
// reading — the snapshot write waits out its deadline — and that client must
// still get its acknowledgment. The failure is reported instead of discarded.
func TestServer_ServesAClientWhoseSnapshotWasNotWritten(t *testing.T) {
	s := NewServer("unused")
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()

	var warnings bytes.Buffer
	restore := swapStderrSink(&warnings)
	defer restore()

	go func() {
		cmd := <-s.cmdCh
		cmd.Reply <- CommandResult{Status: "ok"}
	}()
	handled := make(chan struct{})
	go func() {
		s.handle(&snapshotWriteFailure{Conn: serverSide})
		close(handled)
	}()
	defer func() {
		clientSide.Close()
		<-handled
	}()

	sc := bufio.NewScanner(clientSide)
	sc.Buffer(make([]byte, 64<<10), MaxEventBytes)
	c := &Client{conn: clientSide, scanner: sc}
	if err := c.SendTimeout(Command{Action: "restart", Process: "x"}, 5*time.Second); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The acknowledgment is the first event: no snapshot, and no part of one.
	ev, err := c.RecvTimeout(5 * time.Second)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if ev.Type != TypeAck || ev.Ack != "ok" {
		t.Fatalf("first event = %+v, want the ok acknowledgment", ev)
	}

	if msg := warningText(&warnings); !strings.Contains(msg, "failed to write snapshot") {
		t.Fatalf("no diagnostic for the failed snapshot write; warnings: %q", msg)
	}
}

// TestClient_SendTimeoutBoundsABlockedWrite is the finding-4 regression. It uses
// net.Pipe, which is unbuffered and synchronous: a write only completes when the
// other end reads, so with nobody reading the write blocks. That makes the block
// deterministic rather than relying on a Unix socket's buffer happening to fill.
//
// The point is that a caller with an overall budget must be able to bound its
// request, not just the reply it waits for afterwards.
func TestClient_SendTimeoutBoundsABlockedWrite(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	c := &Client{conn: clientConn}

	start := time.Now()
	err := c.SendTimeout(Command{Action: "shutdown", Force: true}, 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SendTimeout() = nil, want a deadline error: nothing was reading the pipe")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("SendTimeout() = %v, want a timeout error", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("SendTimeout took %s; the write deadline was not applied", elapsed)
	}
}

// TestClient_SendTimeoutRejectsAnExhaustedBudget pins the other half of the
// finding-4 fix. A caller that derives its write budget from an overall deadline
// computes zero once that deadline has passed, and a zero used to mean "no
// deadline" — so an exhausted budget silently became an unbounded write against
// exactly the wedged daemon the budget exists to bound.
//
// The write must be refused before anything is sent.
func TestClient_SendTimeoutRejectsAnExhaustedBudget(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	c := &Client{conn: clientConn}

	for _, budget := range []time.Duration{0, -time.Second} {
		err := c.SendTimeout(Command{Action: "shutdown"}, budget)
		if err == nil {
			t.Fatalf("SendTimeout(%s) = nil, want the exhausted budget to be refused", budget)
		}
		if !strings.Contains(err.Error(), "exhausted") {
			t.Fatalf("SendTimeout(%s) = %v, want it to say the budget is exhausted", budget, err)
		}
	}
}

// TestClient_SendWithoutDeadlineIsUnchanged guards the shared path: ordinary
// Send must keep its previous behaviour for every other consumer, i.e. no
// deadline is imposed and a reader on the other end receives the command.
func TestClient_SendWithoutDeadlineIsUnchanged(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	c := &Client{conn: clientConn}

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 512)
		n, err := serverConn.Read(buf)
		if err != nil {
			got <- nil
			return
		}
		got <- buf[:n]
	}()

	if err := c.Send(Command{Action: "restart", Process: "api"}); err != nil {
		t.Fatalf("Send() = %v", err)
	}
	select {
	case data := <-got:
		if len(data) == 0 {
			t.Fatal("Send() delivered nothing")
		}
		if !strings.Contains(string(data), `"restart"`) {
			t.Fatalf("Send() delivered %q, want it to contain the action", string(data))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send() never reached the reader")
	}
}

// TestNewRunID_UniqueWhenTheClockDoesNotAdvance pins run-ID uniqueness within a
// process. Every call reads the same wall-clock value, as two acquisitions in
// one Windows clock tick do; formatting the clock alone gave both the same ID.
func TestNewRunID_UniqueWhenTheClockDoesNotAdvance(t *testing.T) {
	const now = int64(1_791_813_600_000_000_000)
	const workers, perWorker = 8, 64
	ids := make(chan string, workers*perWorker)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				ids <- newRunID(now)
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, workers*perWorker)
	prefix := strconv.Itoa(os.Getpid()) + "-"
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate run ID %q from a clock that did not advance", id)
		}
		seen[id] = true
		nanos, err := strconv.ParseInt(strings.TrimPrefix(id, prefix), 10, 64)
		if !strings.HasPrefix(id, prefix) || err != nil || nanos < now {
			t.Fatalf("run ID %q is not %s<nanoseconds at or after the clock>", id, prefix)
		}
	}
	if earlier := newRunID(now - 1); seen[earlier] {
		t.Fatalf("clock moving backwards reused run ID %q", earlier)
	}
}
