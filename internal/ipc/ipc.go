// Package ipc provides inter-process communication via Unix domain sockets or Windows named pipes.
package ipc

import "time"

// Event types sent over the Unix socket (newline-delimited JSON).
const (
	TypeSnapshot = "snapshot"
	TypeLog      = "log"
	TypeState    = "state"
	TypeTunnel   = "tunnel"  // tunnel URL became known or changed
	TypeCommand  = "command" // client -> server command
	TypeAck      = "ack"     // server -> client command acknowledgment
)

// ProcState is one row in the process table. Ready is broadcast separately
// from State because a process can be "running" but not yet ready (probe
// still polling). Consumers that want a true readiness signal — `up
// --wait-ready`, the monitor's status display — must check Ready, not just
// State == "running".
type ProcState struct {
	Name       string    `json:"name"`
	State      string    `json:"state"` // starting | running | restarting | exited | completed | failed
	Mode       string    `json:"mode"`  // service (default) or task
	Ready      bool      `json:"ready"` // process passed its ready_when probe (or has none)
	PID        int       `json:"pid"`
	Restarts   int       `json:"restarts"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	ColorIndex int       `json:"color_index"`
	CPUPercent float64   `json:"cpu_percent,omitempty"` // CPU usage as percentage
	MemoryMB   float64   `json:"memory_mb,omitempty"`   // memory usage in megabytes
}

// LogEntry is a single log line emitted by a managed process.
type LogEntry struct {
	Process string `json:"process"`
	Line    string `json:"line"`
	IsEvent bool   `json:"is_event,omitempty"` // process lifecycle event (yellow)
	IsError bool   `json:"is_error,omitempty"` // error-level line (red)
}

// CommandResult carries a runner's verdict for a command back to the
// awaiting client. Status is one of:
//
//	"ok"      — the command completed as requested
//	"partial" — the command was accepted but couldn't be fully applied
//	            (e.g. reload skipped processes that were added or removed)
//	"error"   — the command failed
//
// Message holds a human-readable explanation and is empty for plain "ok".
type CommandResult struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Command is a message sent from a CLI client to the daemon. The Reply
// channel is in-memory only (the JSON tag suppresses it on the wire); the
// server attaches it before pushing to the runner so that the runner can
// surface a real verdict back to the waiting client instead of forcing the
// client to assume success the moment the command was queued.
type Command struct {
	Action  string             `json:"action"`            // "restart", "reload"
	Process string             `json:"process,omitempty"` // target process name (for restart)
	Reply   chan CommandResult `json:"-"`
}

// Event is the envelope for all messages sent over the socket.
// Only the fields matching Type are populated.
type Event struct {
	Type string `json:"type"`

	// TypeSnapshot — sent once to each new monitor on connect.
	Processes  []ProcState `json:"processes,omitempty"`
	RecentLogs []LogEntry  `json:"recent_logs,omitempty"`
	TunnelURL  string      `json:"tunnel_url,omitempty"` // non-empty when a tunnel is active

	// TypeLog — a log line from a running process.
	Log *LogEntry `json:"log,omitempty"`

	// TypeState — a process changed state.
	Proc *ProcState `json:"proc,omitempty"`

	// TypeCommand — a command from a client (restart, reload, etc.).
	Cmd *Command `json:"cmd,omitempty"`

	// TypeAck — acknowledgment of a command. Ack is one of "ok",
	// "partial", "error", "busy". AckDetail carries an optional
	// human-readable explanation (e.g. which processes were skipped on a
	// partial reload, or why a command was rejected).
	Ack       string `json:"ack,omitempty"`
	AckDetail string `json:"ack_detail,omitempty"`
}
