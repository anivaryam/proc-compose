package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

const maxHistory = 500

// Server is a Unix-socket IPC server that broadcasts events to all connected
// monitors. It is safe for concurrent use.
type Server struct {
	socketPath string
	ln         net.Listener

	mu        sync.RWMutex
	clients   map[*serverClient]struct{}
	history   *logRing // bounded buffer of recent log lines
	states    map[string]*ProcState
	tunnelURL string // last public URL reported by the tunnel process

	cmdCh chan Command // received commands forwarded to runner
}

type serverClient struct {
	conn      net.Conn
	send      chan []byte // buffered; closed on disconnect
	closeOnce sync.Once
}

// NewServer creates a Server for the given socket path.
func NewServer(socketPath string) *Server {
	return &Server{
		socketPath: socketPath,
		clients:    make(map[*serverClient]struct{}),
		history:    newLogRing(maxHistory),
		states:     make(map[string]*ProcState),
		cmdCh:      make(chan Command, 16),
	}
}

// Commands returns a read-only channel of commands received from clients.
func (s *Server) Commands() <-chan Command { return s.cmdCh }

// Listen creates the socket/pipe and starts accepting connections.
func (s *Server) Listen() error {
	ln, err := newServerListener(s.socketPath)
	if err != nil {
		return err
	}
	s.ln = ln
	go s.accept()
	return nil
}

// Shutdown closes the listener and all active connections.
func (s *Server) Shutdown() {
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.shutdown()
	}
}

// shutdown safely closes the client's send channel exactly once.
// The writeLoop goroutine is responsible for closing the net.Conn
// after the send channel is drained.
func (c *serverClient) shutdown() {
	c.closeOnce.Do(func() {
		close(c.send)
	})
}

// SetState seeds the initial process state (called before Listen).
func (s *Server) SetState(st ProcState) {
	s.mu.Lock()
	s.states[st.Name] = &st
	s.mu.Unlock()
}

// BroadcastLog adds the entry to the ring buffer and fans it out to monitors.
func (s *Server) BroadcastLog(entry LogEntry) {
	ev, err := json.Marshal(Event{Type: TypeLog, Log: &entry})
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to marshal log event: %v\n", err)
		return
	}
	ev = append(ev, '\n')

	s.mu.Lock()
	s.history.push(entry)
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

// BroadcastTunnelURL stores the public tunnel URL and fans it out to monitors.
// Called by the runner when the tunnel process reports its forwarding address.
func (s *Server) BroadcastTunnelURL(url string) {
	ev, err := json.Marshal(Event{Type: TypeTunnel, TunnelURL: url})
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to marshal tunnel URL event: %v\n", err)
		return
	}
	ev = append(ev, '\n')

	s.mu.Lock()
	s.tunnelURL = url
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

// BroadcastState updates the state map and fans the change out to monitors.
func (s *Server) BroadcastState(st ProcState) {
	ev, err := json.Marshal(Event{Type: TypeState, Proc: &st})
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to marshal state event: %v\n", err)
		return
	}
	ev = append(ev, '\n')

	s.mu.Lock()
	s.states[st.Name] = &st
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	c := &serverClient{conn: conn, send: make(chan []byte, 64)}

	// Snapshot: register client and capture current state atomically.
	s.mu.Lock()
	s.clients[c] = struct{}{}
	snap := s.snapshot()
	s.mu.Unlock()

	// Send snapshot as first message.
	data, err := json.Marshal(snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to marshal snapshot: %v\n", err)
	} else {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.Write(append(data, '\n'))
		conn.SetWriteDeadline(time.Time{})
	}

	// Start write loop.
	go c.writeLoop()

	// Read commands from the client (monitors send nothing; CLI commands do).
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<16), 1<<16)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == TypeCommand && ev.Cmd != nil {
			cmd := *ev.Cmd
			// Attach a buffered reply channel so the runner can hand back
			// a real verdict (ok / partial / error) instead of the client
			// having to assume the command succeeded the moment it was
			// queued. Buffer size 1 means the runner never blocks even if
			// the client disconnects before reading the ack.
			cmd.Reply = make(chan CommandResult, 1)

			ack := Event{Type: TypeAck, Ack: "ok"}
			select {
			case s.cmdCh <- cmd:
				// Wait for the runner's verdict. Bound the wait so a
				// runaway runner can't pin a connection forever — 30s is
				// well over the typical reload time.
				select {
				case res := <-cmd.Reply:
					ack.Ack = res.Status
					ack.AckDetail = res.Message
				case <-time.After(30 * time.Second):
					ack.Ack = "error"
					ack.AckDetail = "timed out waiting for runner reply"
				}
			default:
				// Channel full — surface the drop so callers can retry.
				ack.Ack = "busy"
			}

			data, err := json.Marshal(ack)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARN: failed to marshal ack: %v\n", err)
				continue
			}
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			conn.Write(append(data, '\n'))
			conn.SetWriteDeadline(time.Time{})
		}
	}

	// Clean up.
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.shutdown()
}

func (c *serverClient) writeLoop() {
	defer c.conn.Close()
	for data := range c.send {
		c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.conn.Write(data); err != nil {
			c.shutdown()
			// Drain remaining messages so shutdown()/close doesn't block.
			for range c.send {
			}
			return
		}
	}
}

func (s *Server) fanOut(clients []*serverClient, data []byte) {
	for _, c := range clients {
		select {
		case c.send <- data:
		default:
			// Channel full — shut down this slow client.
			c.shutdown()
		}
	}
}

func (s *Server) clientSlice() []*serverClient {
	out := make([]*serverClient, 0, len(s.clients))
	for c := range s.clients {
		out = append(out, c)
	}
	return out
}

func (s *Server) snapshot() Event {
	procs := make([]ProcState, 0, len(s.states))
	for _, st := range s.states {
		procs = append(procs, *st)
	}
	hist := s.history.snapshot()
	return Event{Type: TypeSnapshot, Processes: procs, RecentLogs: hist, TunnelURL: s.tunnelURL}
}
