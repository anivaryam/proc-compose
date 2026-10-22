package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const maxHistory = 500

// maxCommandFrameBytes is the largest inbound command frame the server
// accepts, its newline included. A longer frame is discarded up to its
// newline and answered with an error acknowledgment; the connection stays
// open and the frames after it are still read. It is deliberately far below
// MaxEventBytes, which bounds the events the daemon sends out, not the
// commands it accepts.
const maxCommandFrameBytes = 1 << 16

// runnerReplyTimeout bounds how long the server waits for the runner's
// verdict on a command before acknowledging it with a timeout error — 30s is
// well over the typical reload time, and comfortably over a shutdown
// (per-process shutdown_timeout plus the final sweep).
const runnerReplyTimeout = 30 * time.Second

// snapshotWriteTimeout bounds the write of the snapshot every connection
// starts with. A client that has not read it by then is served without it,
// or dropped if part of it went out.
const snapshotWriteTimeout = 5 * time.Second

// stderrSink is where this package's warnings go. It is guarded by
// stderrMu so that a test can capture the warnings without swapping
// os.Stderr underneath a server goroutine that may be writing to it.
var (
	stderrMu   sync.Mutex
	stderrSink io.Writer = os.Stderr
)

// warnf writes one warning line to the package's stderr sink.
func warnf(format string, args ...any) {
	stderrMu.Lock()
	defer stderrMu.Unlock()
	fmt.Fprintf(stderrSink, format, args...)
}

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
	project   *Project

	cmdCh chan Command // received commands forwarded to runner

	// replyTimeout is runnerReplyTimeout, except in tests that need the
	// timeout path without waiting for it.
	replyTimeout time.Duration
}

type serverClient struct {
	conn      net.Conn
	send      chan []byte // buffered; closed on disconnect
	closeOnce sync.Once
	sendMu    sync.Mutex // guards send-channel close vs concurrent sends
	closed    bool       // set under sendMu when send has been closed

	// snapshotDone is closed once the snapshot write has finished. framed is
	// set before it is closed: false when the write was cut short and the
	// connection dropped, so a command read meanwhile must not be acted on.
	snapshotDone chan struct{}
	framed       bool
}

// awaitSnapshot blocks until the snapshot write has finished, and reports
// whether the connection is still served. The write is bounded by
// snapshotWriteTimeout, so the wait is too.
func (c *serverClient) awaitSnapshot() bool {
	<-c.snapshotDone
	return c.framed
}

// NewServer creates a Server for the given socket path.
func NewServer(socketPath string) *Server {
	return &Server{
		socketPath:   socketPath,
		clients:      make(map[*serverClient]struct{}),
		history:      newLogRing(maxHistory),
		states:       make(map[string]*ProcState),
		cmdCh:        make(chan Command, 16),
		replyTimeout: runnerReplyTimeout,
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
		c.sendMu.Lock()
		c.closed = true
		close(c.send)
		c.sendMu.Unlock()
	})
}

// trySend delivers data to the client without panicking if the send
// channel has already been closed by shutdown. Returns true if the
// data was queued, false if the client is gone or the buffer is full.
func (c *serverClient) trySend(data []byte) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

// SetState seeds the initial process state (called before Listen).
func (s *Server) SetState(st ProcState) {
	s.mu.Lock()
	s.states[st.Name] = &st
	s.mu.Unlock()
}

func (s *Server) SetProject(project *Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project = project
}

// BroadcastLog adds the entry to the ring buffer and fans it out to monitors.
func (s *Server) BroadcastLog(entry LogEntry) {
	ev, err := json.Marshal(Event{Type: TypeLog, Log: &entry})
	if err != nil {
		warnf("WARN: failed to marshal log event: %v\n", err)
		return
	}
	ev = append(ev, '\n')

	s.mu.Lock()
	s.history.push(entry, len(ev))
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

// BroadcastTunnelURL stores the public tunnel URL and fans it out to monitors.
// Called by the runner when the tunnel process reports its forwarding address.
func (s *Server) BroadcastTunnelURL(url string) {
	ev, err := json.Marshal(Event{Type: TypeTunnel, TunnelURL: url})
	if err != nil {
		warnf("WARN: failed to marshal tunnel URL event: %v\n", err)
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
		warnf("WARN: failed to marshal state event: %v\n", err)
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
	c := &serverClient{conn: conn, send: make(chan []byte, 64), snapshotDone: make(chan struct{})}

	// Snapshot: register client and capture current state atomically.
	s.mu.Lock()
	s.clients[c] = struct{}{}
	snap := s.snapshot()
	s.mu.Unlock()

	// The snapshot is written while commands are read. A client may write
	// its command before it reads the snapshot, and on a transport that does
	// not buffer (a Windows named pipe) neither write completes until the
	// other side reads: writing the snapshot before reading anything held
	// both until the snapshot's deadline. Commands are still acted on only
	// once the snapshot is out, so the client reads the snapshot first, then
	// events and acknowledgments.
	go s.serveWrites(c, snap)

	// Read commands from the client (monitors send nothing; CLI commands do).
	s.readCommands(c)

	s.drop(c)
}

// serveWrites writes the snapshot, releases the commands waiting on it, and
// then runs the write loop, unless the snapshot write dropped the connection.
func (s *Server) serveWrites(c *serverClient, snap Event) {
	c.framed = s.writeSnapshot(c, snap)
	close(c.snapshotDone)
	if c.framed {
		c.writeLoop()
	}
}

// writeSnapshot writes snap as the first frame of the connection, and reports
// whether the stream is still well framed.
func (s *Server) writeSnapshot(c *serverClient, snap Event) bool {
	data, err := json.Marshal(snap)
	if err != nil {
		warnf("WARN: failed to marshal snapshot: %v\n", err)
		return true
	}
	c.conn.SetWriteDeadline(time.Now().Add(snapshotWriteTimeout))
	n, werr := c.conn.Write(append(data, '\n'))
	c.conn.SetWriteDeadline(time.Time{})
	switch {
	case werr == nil:
		return true
	case n > 0:
		// Part of the snapshot is on the wire: the client would read
		// everything after it as the tail of that broken line. Drop the
		// connection rather than serve a stream it cannot parse; closing it
		// also ends the read of its commands.
		warnf("WARN: failed to write snapshot to client after %d of %d bytes, dropping the connection: %v\n", n, len(data)+1, werr)
		s.drop(c)
		c.conn.Close() // the write loop that would close it never starts
		return false
	default:
		// Nothing was written, so the stream is still well framed and the
		// client is served without its snapshot: it did not read for the
		// whole deadline, or it has gone away.
		warnf("WARN: failed to write snapshot to client, serving it without one: %v\n", werr)
		return true
	}
}

// drop unregisters c and closes its send channel.
func (s *Server) drop(c *serverClient) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.shutdown()
}

// readCommands reads newline-delimited command frames from c until the
// stream ends.
//
// A frame over maxCommandFrameBytes is discarded up to its newline and
// answered with an error acknowledgment, and the frames after it are still
// read: one oversized frame must not take the commands queued behind it down
// with it. bufio.Scanner cannot do this — once a line outgrows its buffer it
// latches bufio.ErrTooLong and stops for good — so this reads with
// bufio.Reader, whose ReadSlice reports a full buffer and can carry on.
//
// Each frame is read in full before it waits for the snapshot write, so a
// client blocked writing its command is released, and only then acted on.
// If the snapshot write dropped the connection, no command read meanwhile
// reaches the runner.
func (s *Server) readCommands(c *serverClient) {
	r := bufio.NewReaderSize(c.conn, maxCommandFrameBytes)
	for {
		frame, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			err = discardLine(r)
			if !c.awaitSnapshot() {
				return
			}
			s.rejectOversizedFrame(c.conn)
		} else if len(frame) > 0 {
			var ev Event
			if json.Unmarshal(frame, &ev) == nil && ev.Type == TypeCommand && ev.Cmd != nil {
				if !c.awaitSnapshot() {
					return
				}
				s.handleCommand(c.conn, ev.Cmd)
			}
		}
		if err != nil {
			return // the client disconnected, or the connection failed
		}
	}
}

// discardLine consumes r up to and including the next newline. It returns
// nil once the line has ended, or the read error that ended the stream first.
func discardLine(r *bufio.Reader) error {
	for {
		if _, err := r.ReadSlice('\n'); !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}

// rejectOversizedFrame answers a discarded oversized frame with an error
// acknowledgment the client can show, and reports it on the daemon side too.
func (s *Server) rejectOversizedFrame(conn net.Conn) {
	warnf("WARN: command frame too long: discarded a frame over the %d byte inbound limit and answered it with an error acknowledgment\n", maxCommandFrameBytes)
	_ = writeAck(conn, Event{
		Type:      TypeAck,
		Ack:       "error",
		AckDetail: fmt.Sprintf("command frame too long: the daemon accepts command frames of at most %d bytes; the frame was discarded", maxCommandFrameBytes),
	})
}

// handleCommand forwards one command to the runner and writes its
// acknowledgment.
//
// cmd.flushed reports what became of that acknowledgment, so a handler that
// must not be orphaned by a server shutdown (the "shutdown" command, whose
// verdict is the only report of what could not be terminated) can wait for
// its own answer and learn whether it was delivered. It is released as soon
// as the write attempt completes, whatever its result: a peer that has gone
// away must not wedge the daemon.
func (s *Server) handleCommand(conn net.Conn, in *Command) {
	// Attach a buffered reply channel so the runner can hand back a real
	// verdict (ok / partial / error) instead of the client having to assume
	// the command succeeded the moment it was queued. Buffer size 1 means the
	// runner never blocks even if the client disconnects before reading the
	// ack.
	cmd := *in
	cmd.Reply = make(chan CommandResult, 1)
	cmd.flushed = &ackDelivery{done: make(chan struct{})}
	// undelivered stays nil only when the ack carrying the runner's verdict
	// is written in full.
	var undelivered error
	defer func() { cmd.flushed.finish(undelivered) }()

	ack := Event{Type: TypeAck, Ack: "ok"}
	select {
	case s.cmdCh <- cmd:
		// Wait for the runner's verdict. Bound the wait so a runaway runner
		// can't pin a connection forever.
		timer := time.NewTimer(s.replyTimeout)
		defer timer.Stop()
		select {
		case res := <-cmd.Reply:
			ack.Ack = res.Status
			ack.AckDetail = res.Message
			ack.AckGeneration = res.Generation
		case <-timer.C:
			ack.Ack = "error"
			ack.AckDetail = "timed out waiting for runner reply"
			// A verdict the runner hands back after this is never written.
			undelivered = fmt.Errorf("the client was answered with a timeout after %s, before the runner's verdict arrived", s.replyTimeout)
		}
	default:
		// Channel full — surface the drop so callers can retry.
		ack.Ack = "busy"
	}

	if err := writeAck(conn, ack); err != nil && undelivered == nil {
		undelivered = err
	}
}

// writeAck writes one acknowledgment to conn under a write deadline. A
// failure is reported on stderr, because the client cannot be told, and
// returned.
func writeAck(conn net.Conn, ack Event) error {
	data, err := json.Marshal(ack)
	if err != nil {
		warnf("WARN: failed to marshal ack: %v\n", err)
		return fmt.Errorf("marshal acknowledgment: %w", err)
	}
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write(append(data, '\n'))
	conn.SetWriteDeadline(time.Time{})
	if err != nil {
		warnf("WARN: failed to write ack to client: %v\n", err)
		return fmt.Errorf("write acknowledgment: %w", err)
	}
	return nil
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
		if !c.trySend(data) {
			// Channel full or already closed — shut down this slow client.
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
	return Event{Type: TypeSnapshot, Processes: procs, RecentLogs: hist, TunnelURL: s.tunnelURL, Project: s.project}
}
