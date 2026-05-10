package ipc

import (
	"path/filepath"
	"runtime"
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
	return filepath.Join(t.TempDir(), name)
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
