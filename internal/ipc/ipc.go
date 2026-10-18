// Package ipc provides inter-process communication via Unix domain sockets or Windows named pipes.
package ipc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Includes worst-case JSON escaping and the legacy IPC line alias.
const MaxEventBytes = 16 << 20

// TeardownPrefix starts every line of a shutdown verdict that reports a
// teardown process rather than a managed process that could not be stopped.
// A verdict whose every line carries it means termination was verified and
// only teardown failed.
const TeardownPrefix = "teardown: "

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
	Generation uint64    `json:"generation,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	DependsOn  []string  `json:"depends_on,omitempty"`
	// Health is the health_when verdict for the current incarnation: unknown,
	// healthy or unhealthy; empty when the process has no health_when. It is
	// unknown whenever the process is not running. It is separate from Ready,
	// which latches once per incarnation.
	Health      string `json:"health,omitempty"`
	HealthError string `json:"health_error,omitempty"` // the last failed health check, while it is failing
	// ReadinessOwner is who answered the http or tcp readiness probe that
	// made the current incarnation ready. HealthOwner is the same for its
	// last passing health_when check. Both are nil for other probe kinds and
	// before the first pass. They are observations only: no verdict depends
	// on them.
	ReadinessOwner *EndpointOwner `json:"readiness_owner,omitempty"`
	HealthOwner    *EndpointOwner `json:"health_owner,omitempty"`
}

// Endpoint owner results.
const (
	// OwnerOwned means a process in the incarnation's own process tree holds
	// the server socket that answered.
	OwnerOwned = "owned"
	// OwnerForeign means a process outside that tree, identified by PID,
	// holds it.
	OwnerForeign = "foreign"
	// OwnerUnattributable means the holder could not be identified; Reason
	// says why. It is never evidence either way.
	OwnerUnattributable = "unattributable"
)

// Scopes of a foreign answer, placed against the live-session registry.
const (
	// ScopeCrossSession means the answer belongs to another live
	// proc-compose session in this runtime directory; Session names it.
	ScopeCrossSession = "cross_session"
	// ScopeOutsideSession means no live session is known to hold the
	// answer: a program proc-compose did not start, a session in another
	// runtime directory, or a session that is no longer live.
	ScopeOutsideSession = "outside_session"
)

// Evidence that a cross_session answer belongs to the other session.
const (
	// EvidenceHolder means the holder is a member of a process group the
	// other session recorded, proven by the group leader's start time.
	EvidenceHolder = "holder"
	// EvidencePortLease means the answered port is one the other session
	// was assigned and leases for its lifetime; the holder itself was not
	// proven to be one of its processes.
	EvidencePortLease = "port_lease"
)

// EndpointOwner records which process held the server side of the connection
// a passing http or tcp probe used, as the OS socket table reports it.
type EndpointOwner struct {
	Result  string `json:"result"`            // owned | foreign | unattributable
	Address string `json:"address,omitempty"` // the server address that answered, host:port
	PID     int    `json:"pid,omitempty"`     // the holder, when identified
	Command string `json:"command,omitempty"` // the holder's process name
	Reason  string `json:"reason,omitempty"`  // why the holder is unattributable
	// Scope refines a foreign result: cross_session or outside_session. It
	// is empty for owned and unattributable answers, for a foreign answer
	// that this session's own record places (another of its processes, by
	// its recorded process group on Linux, or a port it leases), if listing
	// the registry fails, and in records written before it existed. Another
	// process of this session that its record does not place (on Windows a
	// hardcoded port it does not lease; on Linux a process outside its
	// recorded groups) is outside_session.
	Scope string `json:"scope,omitempty"`
	// Session names the other session of a cross_session answer.
	Session *OwnerSession `json:"session,omitempty"`
}

// OwnerSession names the live session a cross_session answer belongs to and
// the evidence for it.
type OwnerSession struct {
	Name       string `json:"name"` // the session name, "default" for the unnamed one
	RunID      string `json:"run_id,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	Worktree   string `json:"worktree,omitempty"` // where its owner started it, when known
	// Process is the holder's recorded group for holder evidence, otherwise
	// the process the answered port is assigned to; Port is that named port.
	Process  string `json:"process,omitempty"`
	Port     string `json:"port,omitempty"`
	Evidence string `json:"evidence"` // holder | port_lease
}

// Describe says, for operators, how an answer relates to the session: of
// session "x" (api, in /w/x) for a holder in its process group, or on a port
// leased by session "x" (api.http, in /w/x).
func (s *OwnerSession) Describe() string {
	what := s.Process
	if s.Port != "" {
		what += "." + s.Port
	}
	var detail []string
	if what != "" {
		detail = append(detail, what)
	}
	if s.Worktree != "" {
		detail = append(detail, "in "+s.Worktree)
	}
	desc := fmt.Sprintf("session %q", s.Name)
	if len(detail) > 0 {
		desc += " (" + strings.Join(detail, ", ") + ")"
	}
	if s.Evidence == EvidenceHolder {
		return "of " + desc
	}
	return "on a port leased by " + desc
}

// LogEntry is a single log line emitted by a managed process.
type LogEntry struct {
	Version       int        `json:"schema_version,omitempty"`
	ID            string     `json:"id,omitempty"`
	RunID         string     `json:"run_id,omitempty"`
	AttemptID     string     `json:"attempt_id,omitempty"` // verification attempt that owned this run, if any
	Sequence      uint64     `json:"sequence,omitempty"`
	Session       string     `json:"session,omitempty"`
	Time          time.Time  `json:"time,omitempty"`
	Process       string     `json:"process"`
	Line          string     `json:"message"`
	Kind          string     `json:"kind,omitempty"`
	Level         string     `json:"level,omitempty"`
	Code          string     `json:"code,omitempty"`
	Stream        string     `json:"stream,omitempty"`
	PID           int        `json:"pid,omitempty"`
	Generation    uint64     `json:"generation,omitempty"`
	State         *ProcState `json:"state,omitempty"`
	Truncated     bool       `json:"truncated,omitempty"`
	OriginalBytes int        `json:"original_bytes,omitempty"`
	IsEvent       bool       `json:"is_event,omitempty"` // process lifecycle event (yellow)
	IsError       bool       `json:"is_error,omitempty"` // error-level line (red)
}

// Older daemons used "line". Keep their events readable by upgraded clients.
func (e *LogEntry) UnmarshalJSON(data []byte) error {
	type plain LogEntry
	var wire struct {
		plain
		LegacyLine *string `json:"line"`
		Message    *string `json:"message"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = LogEntry(wire.plain)
	if wire.Message != nil {
		e.Line = *wire.Message
	} else if wire.LegacyLine != nil {
		e.Line = *wire.LegacyLine
	}
	return nil
}

// lastRunNano is the timestamp part of the last run ID issued by this process.
var lastRunNano atomic.Int64

// NewRunID returns a run identifier, "<pid>-<unix nanoseconds>", that is unique
// within this process. The wall clock cannot guarantee that alone: on Windows it
// advances only on the clock interrupt, so two runs acquired in the same tick
// would read the same nanoseconds. A reading at or before the last one issued
// is moved just past it.
func NewRunID() string { return newRunID(time.Now().UnixNano()) }

func newRunID(now int64) string {
	for {
		last := lastRunNano.Load()
		next := max(now, last+1)
		if lastRunNano.CompareAndSwap(last, next) {
			return fmt.Sprintf("%d-%d", os.Getpid(), next)
		}
	}
}

type Project struct {
	Session     string                       `json:"session,omitempty"`
	RunID       string                       `json:"run_id"`
	ConfigPath  string                       `json:"config_path"`
	Directory   string                       `json:"directory"`
	Ports       map[string]map[string]int    `json:"ports,omitempty"`
	URLs        map[string]map[string]string `json:"urls,omitempty"`
	Resources   []string                     `json:"resources,omitempty"`
	LogPath     string                       `json:"log_path,omitempty"`
	JournalPath string                       `json:"journal_path"`
	Owner       *Owner                       `json:"owner,omitempty"`
	// Teardown names the run's teardown processes, so stop can allow for
	// their budget and report that they did not run after a crash.
	Teardown []string `json:"teardown,omitempty"`
}

// Owner describes who created a session, for human/agent handoff. It is
// descriptive metadata, never cleanup authority: only the process holding the
// session lease may stop what it started.
type Owner struct {
	PID       int       `json:"pid"`
	Command   string    `json:"command"`            // "up" or "verify"
	Label     string    `json:"label,omitempty"`    // PROC_COMPOSE_OWNER, e.g. an agent/task name
	Worktree  string    `json:"worktree,omitempty"` // nearest ancestor containing .git, if any
	StartedAt time.Time `json:"started_at"`
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
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
}

// Command is a message sent from a CLI client to the daemon. The Reply
// channel is in-memory only (the JSON tag suppresses it on the wire); the
// server attaches it before pushing to the runner so that the runner can
// surface a real verdict back to the waiting client instead of forcing the
// client to assume success the moment the command is queued.
type Command struct {
	Action  string             `json:"action"`            // "restart", "reload", "shutdown"
	Process string             `json:"process,omitempty"` // target process name (for restart)
	Force   bool               `json:"force,omitempty"`   // "shutdown": kill managed processes instead of asking them to exit
	Reply   chan CommandResult `json:"-"`
	// flushed is closed by the server once the acknowledgment carrying Reply
	// has been written to the client. A "shutdown" handler waits on it so the
	// daemon does not start tearing its IPC server down while the caller is
	// still reading the verdict — which is the only place that reports what
	// could not be terminated. Like Reply it never goes on the wire.
	flushed chan struct{}
}

// WaitFlushed blocks until the acknowledgment carrying this command's Reply
// has been written to the client, or until timeout elapses. It returns
// immediately for a command that did not come from a Server.
//
// Use it only for a verdict that must survive the server going away. A
// "shutdown" verdict qualifies: it is the caller's only report of which
// managed processes could not be terminated.
func (c *Command) WaitFlushed(timeout time.Duration) {
	if c.flushed == nil {
		return
	}
	select {
	case <-c.flushed:
	case <-time.After(timeout):
	}
}

// Event is the envelope for all messages sent over the socket.
// Only the fields matching Type are populated.
type Event struct {
	Type string `json:"type"`

	// TypeSnapshot — sent once to each new monitor on connect.
	Processes  []ProcState `json:"processes,omitempty"`
	RecentLogs []LogEntry  `json:"recent_logs,omitempty"`
	TunnelURL  string      `json:"tunnel_url,omitempty"` // non-empty when a tunnel is active
	Project    *Project    `json:"project,omitempty"`

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
	Ack           string `json:"ack,omitempty"`
	AckDetail     string `json:"ack_detail,omitempty"`
	AckGeneration uint64 `json:"ack_generation,omitempty"`
}

// Keep the historical IPC "line" field for already-running older monitors.
// The journal and agent pages use LogEntry directly and retain one "message"
// field, so compatibility does not double agent context or persisted storage.
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	type legacyLog struct {
		LogEntry
		LegacyLine string `json:"line"`
	}
	var log *legacyLog
	if e.Log != nil {
		log = &legacyLog{LogEntry: *e.Log, LegacyLine: e.Log.Line}
	}
	var recent []legacyLog
	for _, entry := range e.RecentLogs {
		recent = append(recent, legacyLog{LogEntry: entry, LegacyLine: entry.Line})
	}
	return json.Marshal(struct {
		plain
		Log        *legacyLog  `json:"log,omitempty"`
		RecentLogs []legacyLog `json:"recent_logs,omitempty"`
	}{plain: plain(e), Log: log, RecentLogs: recent})
}
