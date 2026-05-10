package runner

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// broadcastState pushes the current state to IPC (no-op if IPC is nil).
func (r *Runner) broadcastState(p procInfo, st *procState) {
	if r.IPC == nil {
		return
	}
	st.mu.Lock()
	snap := ipc.ProcState{
		Name:       p.name,
		State:      st.state,
		Ready:      st.readyClosed && st.readyOK,
		PID:        st.pid,
		Restarts:   st.restarts,
		StartedAt:  st.startedAt,
		ColorIndex: p.colorIndex,
		CPUPercent: st.cpuPercent,
		MemoryMB:   st.memoryMB,
	}
	st.mu.Unlock()
	r.IPC.BroadcastState(snap)
}

// logEvent writes a plain lifecycle line to LogFile (no-op if nil).
func (r *Runner) logEvent(p procInfo, event string) {
	if r.LogFile != nil {
		fmt.Fprintf(r.LogFile, "%s | %s\n", p.name, event)
	}
	if r.IPC != nil {
		r.IPC.BroadcastLog(ipc.LogEntry{Process: p.name, Line: event, IsEvent: true})
	}
}

// systemEvent writes a runner-level message to stderr, the optional log file,
// and any connected monitors. isErr=true tags the entry red in the TUI.
func (r *Runner) systemEvent(msg string, isErr bool) {
	fmt.Fprintln(os.Stderr, msg)
	if r.LogFile != nil {
		fmt.Fprintln(r.LogFile, msg)
	}
	if r.IPC != nil {
		r.IPC.BroadcastLog(ipc.LogEntry{
			Process: "proc-compose",
			Line:    msg,
			IsEvent: !isErr,
			IsError: isErr,
		})
	}
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
