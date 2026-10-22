package ipc

import (
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

	// The barrier is released once the ack has been written.
	select {
	case cmd := <-commands:
		select {
		case <-cmd.flushed:
		case <-time.After(2 * time.Second):
			t.Fatal("the ack barrier was not released")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never received the command")
	}
}

// TestServer_ReportsOversizedFrame pins the daemon-side diagnostic for an
// inbound frame over the cap.
//
// The cap stays at 64 KiB: the frame is still dropped and the connection is
// still closed. What is added is the daemon saying so, instead of an
// oversized frame being indistinguishable from a client that disconnected.
func TestServer_ReportsOversizedFrame(t *testing.T) {
	s := NewServer(testSocketPath(t, "pc-oversized-test"))
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Shutdown()

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

	// One byte over the cap.
	written := make(chan error, 1)
	go func() {
		_, err := c.conn.Write(commandFrame(maxCommandFrameBytes + 1))
		written <- err
	}()
	select {
	case <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("the oversized frame was never written")
	}

	if !waitForWarning(&warnings, "command frame too long") {
		t.Fatalf("no diagnostic for the oversized frame; warnings: %q", warningText(&warnings))
	}
	msg := warningText(&warnings)
	t.Logf("daemon diagnostic: %s", msg)

	// The diagnostic names the cap.
	if !strings.Contains(msg, strconv.Itoa(maxCommandFrameBytes)) {
		t.Fatalf("the diagnostic does not name the %d byte cap: %q", maxCommandFrameBytes, msg)
	}

	// The diagnostic names the peer.
	idx := strings.Index(msg, "peer ")
	if idx < 0 {
		t.Fatalf("the diagnostic does not name the peer: %q", msg)
	}
	rest := msg[idx+len("peer "):]
	if rest == "" || rest[0] == ' ' {
		t.Fatalf("the diagnostic names an empty peer: %q", msg)
	}

	// The frame is still dropped: no command reaches the runner.
	select {
	case cmd := <-s.cmdCh:
		t.Fatalf("the oversized frame was processed: %+v", cmd)
	case <-time.After(200 * time.Millisecond):
	}

	// The connection is still closed.
	if _, err := c.RecvTimeout(5 * time.Second); err == nil {
		t.Fatal("the connection stayed open after the oversized frame")
	} else if errors.Is(err, ErrRecvTimeout) {
		t.Fatalf("the connection stayed open after the oversized frame: %v", err)
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

// TestServer_AckWriteFailureIsReported pins the daemon-side diagnostic for a
// failed acknowledgment write, and the barrier that must still be released.
//
// The barrier release is unconditional: a peer that has gone away must not
// wedge the daemon for the whole shutdown budget.
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

	// The barrier is released anyway.
	select {
	case <-cmd.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("the ack barrier was not released after the failed write")
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
