package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// ErrRecvTimeout is returned by RecvTimeout when no event arrived within
// the requested duration.
var ErrRecvTimeout = errors.New("ipc: timed out waiting for event")

// Client connects to a running daemon's socket/pipe and streams events.
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

// sanitizeSocketPath replaces /run/user/<uid>/ and similar prefixes with ~
// to avoid exposing system internals in user-facing error messages.
func sanitizeSocketPath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(path, home) {
		return strings.Replace(path, home, "~", 1)
	}
	// Handle /run/user/<uid>/ paths by extracting uid from path
	if strings.HasPrefix(path, "/run/user/") {
		rest := strings.TrimPrefix(path, "/run/user/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			uid := rest[:idx]
			return strings.Replace(path, "/run/user/"+uid, "~/.proc-compose", 1)
		}
	}
	return path
}

// Dial connects to the daemon at socketPath.
func Dial(socketPath string) (*Client, error) {
	return DialTimeout(socketPath, 2*time.Second)
}

// DialTimeout connects to the daemon at socketPath, giving up after d.
//
// Callers that share one overall budget across several phases need the
// connection attempt to draw from it rather than impose a fixed cost of their
// own; that is the difference between a bounded stop and an unbounded one.
func DialTimeout(socketPath string, d time.Duration) (*Client, error) {
	conn, err := dialAddr(socketPath, d)
	if err != nil {
		return nil, fmt.Errorf("no proc-compose daemon running (socket: %s)", sanitizeSocketPath(socketPath))
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &Client{conn: conn, scanner: sc}, nil
}

// Recv reads the next event from the daemon. Blocks until an event arrives
// or the connection is closed (returns io.EOF-wrapped error).
func (c *Client) Recv() (Event, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return Event{}, err
		}
		return Event{}, fmt.Errorf("daemon disconnected")
	}
	var ev Event
	if err := json.Unmarshal(c.scanner.Bytes(), &ev); err != nil {
		return Event{}, fmt.Errorf("bad event: %w", err)
	}
	return ev, nil
}

// RecvTimeout reads the next event, giving up after d. Unlike Recv it
// bounds the blocking read with a connection deadline, so a connected but
// silent daemon cannot pin the caller past its own budget.
//
// A receive timeout is terminal for this Client. bufio.Scanner latches the
// read error and any partially buffered line is lost, so subsequent Recv or
// RecvTimeout calls fail immediately with the same error; clearing the
// connection deadline does not revive the stream. Callers that hit
// ErrRecvTimeout must close the Client and stop reading.
func (c *Client) RecvTimeout(d time.Duration) (Event, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return Event{}, fmt.Errorf("set read deadline: %w", err)
	}
	ev, err := c.Recv()
	if err != nil {
		// A timeout left the scanner terminal, so there is nothing to keep
		// alive for a later call — drop the deadline and report.
		_ = c.conn.SetReadDeadline(time.Time{})
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return Event{}, ErrRecvTimeout
		}
		return Event{}, err
	}
	// Success: clear the deadline so plain Recv keeps its blocking behaviour.
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		return Event{}, fmt.Errorf("clear read deadline: %w", err)
	}
	return ev, nil
}

// Send sends a command to the daemon.
func (c *Client) Send(cmd Command) error {
	_, err := c.send(cmd, 0)
	return err
}

// SendTimeout sends a command, giving up if the write has not completed within d.
//
// A plain conn.Write on a Unix socket can block indefinitely once the peer's
// receive buffer is full and it has stopped reading — a daemon that is wedged
// mid-shutdown, for instance. A caller that has given itself an overall budget
// needs the request itself to draw from it, which is the difference between a
// bounded command and one that hangs.
//
// As with RecvTimeout, a zero d means no deadline, so Send's behaviour is
// unchanged for existing callers.
// SendTimeout writes one command under a write deadline.
//
// A non-positive budget is rejected rather than treated as "no deadline": a
// caller that computed a budget from an overall stop deadline must not be able
// to spend an exhausted budget on an unbounded write. Callers that genuinely
// want no deadline use Send.
func (c *Client) SendTimeout(cmd Command, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("send: write budget of %s is exhausted; refusing to write without a deadline", d)
	}
	_, err := c.send(cmd, d)
	return err
}

// send writes one command. d <= 0 leaves the connection's write deadline unset,
// which is only reachable through Send.
func (c *Client) send(cmd Command, d time.Duration) (int, error) {
	ev := Event{Type: TypeCommand, Cmd: &cmd}
	data, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	data = append(data, '\n')
	if d > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(d)); err != nil {
			return 0, fmt.Errorf("set write deadline: %w", err)
		}
	}
	n, err := c.conn.Write(data)
	if d > 0 {
		if derr := c.conn.SetWriteDeadline(time.Time{}); derr != nil && err == nil {
			err = fmt.Errorf("clear write deadline: %w", derr)
		}
	}
	return n, err
}

// Close closes the connection.
func (c *Client) Close() {
	c.conn.Close()
}
