package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

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
	conn, err := dialAddr(socketPath, 2*time.Second)
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

// Send sends a command to the daemon.
func (c *Client) Send(cmd Command) error {
	ev := Event{Type: TypeCommand, Cmd: &cmd}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.conn.Write(data)
	return err
}

// Close closes the connection.
func (c *Client) Close() {
	c.conn.Close()
}
