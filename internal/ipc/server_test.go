package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	stopRunner := make(chan struct{})
	go func() {
		for {
			select {
			case cmd := <-s.cmdCh:
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
			return
		}
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
