package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// readOutput consumes oversized lines without blocking the child or silently
// ending the stream. The retained prefix and original size are explicit.
func (r *Runner) readOutput(p procInfo, stream string, input io.Reader, onLine func(string)) {
	const maxLine = 1 << 20
	reader := bufio.NewReaderSize(input, 64*1024)
	var retained []byte
	total := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		complete := err != bufio.ErrBufferFull
		hasLine := len(fragment) > 0 || total > 0
		if complete {
			fragment = bytes.TrimSuffix(bytes.TrimSuffix(fragment, []byte("\n")), []byte("\r"))
		}
		total += len(fragment)
		if available := maxLine - len(retained); available > 0 {
			if len(fragment) > available {
				fragment = fragment[:available]
			}
			retained = append(retained, fragment...)
		}
		if complete && hasLine {
			raw := string(retained)
			level := "info"
			var fields struct {
				Level    string `json:"level"`
				Severity string `json:"severity"`
			}
			if json.Unmarshal(retained, &fields) == nil {
				candidate := strings.ToLower(fields.Level)
				if candidate == "" {
					candidate = strings.ToLower(fields.Severity)
				}
				switch candidate {
				case "debug", "info", "warn", "error", "fatal":
					level = candidate
				}
			}
			event := ipc.LogEntry{Kind: "output", Level: level, Code: "process.output", Stream: stream, Line: raw, IsError: level == "error" || level == "fatal"}
			if total > maxLine {
				event.Truncated, event.OriginalBytes = true, total
			}
			r.emit(p, event)
			if r.IPC != nil && p.name == "tunnel" {
				if url := extractTunnelURL(raw); url != "" {
					r.IPC.BroadcastTunnelURL(url)
				}
			}
			if onLine != nil {
				onLine(raw)
			}
			if p.capture != nil {
				p.capture(stream, raw)
			}
			retained, total = retained[:0], 0
		}
		if err != nil && err != bufio.ErrBufferFull {
			if err != io.EOF {
				r.processLog(p, "error", "output.read_failed", err.Error())
			}
			return
		}
	}
}

// broadcastState pushes the current state to IPC (no-op if IPC is nil). A
// quiet execution is never broadcast.
func (r *Runner) broadcastState(p procInfo, st *procState) {
	if p.quiet {
		return
	}
	st.mu.Lock()
	snap := st.ipcStateLocked(p.name, p.proc)
	st.mu.Unlock()
	if r.IPC != nil {
		r.IPC.BroadcastState(snap)
	}
	r.logMu.Lock()
	previous, exists := r.lastStates[p.name]
	changed := !exists || previous.State != snap.State || previous.Generation != snap.Generation || previous.PID != snap.PID || previous.Ready != snap.Ready || previous.LastError != snap.LastError || previous.Health != snap.Health
	if r.lastStates == nil {
		r.lastStates = make(map[string]ipc.ProcState)
	}
	r.lastStates[p.name] = snap
	r.logMu.Unlock()
	if changed {
		r.emit(p, ipc.LogEntry{Kind: "state", Level: "debug", Code: "process.state", Line: snap.State, State: &snap, IsEvent: true})
	}
}

func (r *Runner) processLog(p procInfo, level, code, message string) {
	kind := "lifecycle"
	if strings.HasPrefix(code, "readiness.") || strings.HasPrefix(code, "health.") {
		kind = "probe"
	}
	r.emit(p, ipc.LogEntry{Kind: kind, Level: level, Code: code, Stream: "supervisor", Line: message, IsEvent: true, IsError: level == "error"})
}

func (r *Runner) systemLog(level, code, message string) {
	r.emit(procInfo{name: "proc-compose"}, ipc.LogEntry{Kind: "session", Level: level, Code: code, Stream: "supervisor", Line: message, IsEvent: true, IsError: level == "error"})
}

// emit is the sole event-ordering and output boundary. A message is rendered
// once per sink; files, agents, and the monitor observe the same identity.
// Nothing from a quiet execution passes it: output, lifecycle, and error
// events alike are dropped here so no call site has to remember the check.
func (r *Runner) emit(p procInfo, entry ipc.LogEntry) {
	if p.quiet {
		return
	}
	entry.Process = p.name
	if st := r.stateFor(p.name); st != nil {
		st.mu.Lock()
		entry.PID, entry.Generation = st.pid, st.generation
		st.mu.Unlock()
	}
	if entry.State != nil {
		entry.PID, entry.Generation = entry.State.PID, entry.State.Generation
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.sequence++
	entry.Version, entry.Sequence, entry.Time = 1, r.sequence, time.Now().UTC()
	if r.Project != nil {
		entry.RunID, entry.Session = r.Project.RunID, r.Project.Session
	}
	entry.AttemptID = r.AttemptID
	entry.ID = fmt.Sprintf("%s:%d", entry.RunID, entry.Sequence)
	data, err := json.Marshal(entry)
	if err != nil {
		r.logErr = err
		return
	}
	if r.Journal != nil {
		if _, err := r.Journal.Write(append(data, '\n')); err != nil {
			r.logErr = err
		}
	}
	if entry.Level != "debug" || r.Verbose {
		output := r.Output
		if output == nil {
			output = os.Stdout
		}
		line := string(data)
		if r.LogFormat != "json" {
			line = FormatLine(p.name, r.maxName, p.colorIndex, entry.Line)
		}
		fmt.Fprintln(output, line)
		if r.LogFile != nil { // foreground tee; daemon output is already the file sink
			fileLine := string(data)
			if r.LogFormat != "json" {
				fileLine = fmt.Sprintf("%s | %s", p.name, entry.Line)
			}
			fmt.Fprintln(r.LogFile, fileLine)
		}
	}
	if r.IPC != nil {
		r.IPC.BroadcastLog(entry)
	}
}

// systemEvent writes a runner-level message to stderr, the optional log file,
// and any connected monitors. isErr=true tags the entry red in the TUI.
func (r *Runner) systemEvent(msg string, isErr bool) {
	level := "info"
	if isErr {
		level = "error"
	}
	r.systemLog(level, "session.event", msg)
}

var (
	ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	tunnelURLRe  = regexp.MustCompile(`https?://[^\s→]+`)
)

// extractTunnelURL parses the public URL from a line of tunnel output.
// It matches both the initial "Forwarding:" banner and "Reconnected:" lines.
// Returns "" if the line is not a tunnel announcement.
func extractTunnelURL(line string) string {
	clean := ansiEscapeRe.ReplaceAllString(line, "")
	if !strings.Contains(clean, "Forwarding:") && !strings.Contains(clean, "Reconnected:") {
		return ""
	}
	for _, m := range tunnelURLRe.FindAllString(clean, -1) {
		if !strings.Contains(m, "localhost") {
			return strings.TrimRight(m, ".")
		}
	}
	return ""
}
