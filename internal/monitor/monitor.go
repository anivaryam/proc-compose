// Package monitor provides an interactive TUI for monitoring running proc-compose daemons.
package monitor

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anivaryam/proc-compose/internal/ansi"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"golang.org/x/term"
)

const maxLogs = 5000

// Run connects to the daemon at socketPath and displays an interactive TUI.
// Returns when the user presses q or the daemon disconnects.
func Run(socketPath string) error {
	// Retry briefly — the daemon may still be starting up when the user
	// runs monitor immediately after `proc-compose up --silent`.
	var client *ipc.Client
	var err error
	var waiting bool
	for i := 0; i < 10; i++ {
		client, err = ipc.Dial(socketPath)
		if err == nil {
			break
		}
		if !waiting {
			fmt.Print("waiting for daemon.")
			waiting = true
		} else {
			fmt.Print(".")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if waiting {
		fmt.Println()
	}
	if err != nil {
		return err
	}
	defer client.Close()

	m := &monitor{
		client:    client,
		procOrder: []string{},
		procs:     map[string]*ipc.ProcState{},
	}
	return m.run()
}

// ─── Monitor state ────────────────────────────────────────────────────────────

type monitor struct {
	client *ipc.Client

	// Process table state.
	procOrder []string
	procs     map[string]*ipc.ProcState
	selected  int // index into procOrder

	// Log buffer: bounded ring of the most recent maxLogs entries. Stored
	// as a fixed-cap slice + head index so push is O(1); previous
	// implementation used `logs = logs[1:]` which memmoved the whole slice
	// every line of every process.
	logs      []ipc.LogEntry
	logHead   int    // index of next write
	logCount  int    // number of valid entries (≤ maxLogs)
	filter    string // "" = all; process name = filtered
	logOffset int    // scroll offset; 0 = tail (newest at bottom)

	// Terminal dimensions.
	width  int
	height int

	// Tunnel URL reported by the managed tunnel process; empty when not tunnelling.
	tunnelURL string

	// Help overlay visibility.
	showHelp bool
}

// pushLog appends one entry to the bounded log ring. Capacity is fixed at
// maxLogs; once full, the oldest entry is overwritten.
func (m *monitor) pushLog(e ipc.LogEntry) {
	if m.logs == nil {
		m.logs = make([]ipc.LogEntry, 0, maxLogs)
	}
	if m.logCount < maxLogs {
		m.logs = append(m.logs, e)
		m.logCount++
		m.logHead = m.logCount % maxLogs
		return
	}
	m.logs[m.logHead] = e
	m.logHead = (m.logHead + 1) % maxLogs
}

// pushLogs appends a batch (used for snapshot replay).
func (m *monitor) pushLogs(es []ipc.LogEntry) {
	for _, e := range es {
		m.pushLog(e)
	}
}

// orderedLogs returns the buffered log entries oldest-first.
func (m *monitor) orderedLogs() []ipc.LogEntry {
	if m.logCount < maxLogs {
		return m.logs[:m.logCount]
	}
	out := make([]ipc.LogEntry, maxLogs)
	tail := maxLogs - m.logHead
	copy(out, m.logs[m.logHead:])
	copy(out[tail:], m.logs[:m.logHead])
	return out
}

func (m *monitor) run() error {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return m.plainStream()
	}

	// Enter raw mode.
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("cannot enter raw mode: %w", err)
	}
	defer term.Restore(fd, old)

	// Alternate screen + hide cursor.
	fmt.Print("\033[?1049h\033[?25l")
	defer fmt.Print("\033[?1049l\033[?25h")

	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		m.width, m.height = w, h
	} else {
		fmt.Fprintf(os.Stderr, "WARN: failed to get terminal size: %v\n", err)
	}

	// Channels.
	events := make(chan ipc.Event, 64)
	keys := make(chan string, 16)
	resize := make(chan struct{}, 1)

	// IPC reader goroutine.
	go func() {
		for {
			ev, err := m.client.Recv()
			if err != nil {
				close(events)
				return
			}
			events <- ev
		}
	}()

	// Keyboard reader goroutine.
	go readKeys(os.Stdin, keys)

	// SIGWINCH handler (Unix only; no-op on Windows).
	setupResizeSignal(resize)

	ticker := time.NewTicker(500 * time.Millisecond) // uptime refresh
	defer ticker.Stop()

	m.render()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				m.renderMessage("daemon disconnected — press any key to exit")
				// Wait for any key, then exit. readKeys closes the channel
				// when stdin EOFs (e.g. terminal closed) so this drains
				// cleanly in either direction.
				if k, ok := <-keys; ok {
					_ = k
				}
				return nil
			}
			m.applyEvent(ev)
			m.render()

		case key := <-keys:
			if done := m.handleKey(key); done {
				return nil
			}
			m.render()

		case <-resize:
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				m.width, m.height = w, h
			} else {
				fmt.Fprintf(os.Stderr, "WARN: failed to get terminal size: %v\n", err)
			}
			m.render()

		case <-ticker.C:
			m.render()
		}
	}
}

// plainStream is the fallback for non-TTY environments (pipes/CI).
func (m *monitor) plainStream() error {
	for {
		ev, err := m.client.Recv()
		if err != nil {
			return nil
		}
		switch ev.Type {
		case ipc.TypeLog:
			if ev.Log != nil {
				fmt.Printf("[%s] %s\n", ev.Log.Process, ev.Log.Line)
			}
		case ipc.TypeState:
			if ev.Proc != nil {
				fmt.Printf("[%s] state=%s restarts=%d\n", ev.Proc.Name, ev.Proc.State, ev.Proc.Restarts)
			}
		case ipc.TypeTunnel:
			if ev.TunnelURL != "" {
				fmt.Printf("[tunnel] public: %s\n", ev.TunnelURL)
			}
		}
	}
}

// ─── Event handling ───────────────────────────────────────────────────────────

func (m *monitor) applyEvent(ev ipc.Event) {
	switch ev.Type {
	case ipc.TypeSnapshot:
		for _, p := range ev.Processes {
			cp := p
			m.procs[p.Name] = &cp
		}
		m.rebuildOrder()
		m.pushLogs(ev.RecentLogs)
		if ev.TunnelURL != "" {
			m.tunnelURL = ev.TunnelURL
		}

	case ipc.TypeLog:
		if ev.Log != nil {
			m.pushLog(*ev.Log)
			if m.logOffset > 0 {
				m.logOffset++ // keep scroll position pointing at same content
			}
		}

	case ipc.TypeState:
		if ev.Proc != nil {
			cp := *ev.Proc
			m.procs[cp.Name] = &cp
			m.rebuildOrder()
		}

	case ipc.TypeTunnel:
		m.tunnelURL = ev.TunnelURL
	}
}

func (m *monitor) rebuildOrder() {
	names := make([]string, 0, len(m.procs))
	for n := range m.procs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		left := m.procs[names[i]]
		right := m.procs[names[j]]
		if leftRank, rightRank := healthRank(left), healthRank(right); leftRank != rightRank {
			return leftRank < rightRank
		}
		return names[i] < names[j]
	})
	m.procOrder = names
	if m.selected >= len(m.procOrder) {
		m.selected = max(0, len(m.procOrder)-1)
	}
}

func healthRank(st *ipc.ProcState) int {
	if st == nil {
		return 4
	}
	switch st.State {
	case "failed", "restarting":
		return 0
	case "running":
		if st.Ready {
			return 2
		}
		return 1
	default:
		return 3
	}
}

// ─── Keyboard ─────────────────────────────────────────────────────────────────

func (m *monitor) handleKey(key string) (quit bool) {
	// Help overlay swallows every key except quit. Any keypress dismisses it
	// so Esc / Enter / Space / any letter all work instead of leaving the user
	// wondering why the TUI is frozen.
	if m.showHelp {
		switch key {
		case "q", "ctrl-c":
			return true
		default:
			m.showHelp = false
		}
		return false
	}

	switch key {
	case "q", "ctrl-c":
		return true
	case "?", "h":
		m.showHelp = !m.showHelp
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < len(m.procOrder)-1 {
			m.selected++
		}
	case "enter":
		if len(m.procOrder) > 0 {
			selected := m.procOrder[m.selected]
			if m.filter == selected {
				m.filter = ""
			} else {
				m.filter = selected
			}
			m.logOffset = 0
		}
	case "a":
		m.filter = ""
		m.logOffset = 0
	case "g":
		// Jump to top of log buffer.
		m.logOffset = len(m.filteredLogs())
		if m.logOffset < 0 {
			m.logOffset = 0
		}
	case "G":
		// Jump to bottom (live tail).
		m.logOffset = 0
	case "pgup":
		m.logOffset += m.logAreaRows()
	case "pgdn":
		m.logOffset -= m.logAreaRows()
		if m.logOffset < 0 {
			m.logOffset = 0
		}
	}
	return false
}

func readKeys(r *os.File, ch chan<- string) {
	buf := make([]byte, 16)
	for {
		n, err := r.Read(buf)
		if err != nil || n == 0 {
			close(ch)
			return
		}
		b := buf[:n]
		switch {
		case n == 1 && b[0] == 'q':
			ch <- "q"
		case n == 1 && b[0] == 'a':
			ch <- "a"
		case n == 1 && b[0] == 'h':
			ch <- "h"
		case n == 1 && b[0] == 'j':
			ch <- "j"
		case n == 1 && b[0] == 'k':
			ch <- "k"
		case n == 1 && b[0] == 'g':
			ch <- "g"
		case n == 1 && b[0] == 'G':
			ch <- "G"
		case n == 1 && b[0] == '?':
			ch <- "?"
		case n == 1 && b[0] == '\r', n == 1 && b[0] == '\n':
			ch <- "enter"
		case n == 1 && b[0] == ' ':
			ch <- "space"
		case n == 1 && b[0] == 0x1b: // bare ESC
			ch <- "esc"
		case n == 1 && b[0] == 3: // ctrl-c
			ch <- "ctrl-c"
		case n >= 3 && b[0] == 0x1b && b[1] == '[':
			switch b[2] {
			case 'A':
				ch <- "up"
			case 'B':
				ch <- "down"
			case '5':
				ch <- "pgup"
			case '6':
				ch <- "pgdn"
			}
		}
	}
}

// ─── Rendering ────────────────────────────────────────────────────────────────

const (
	ansiReset   = ansi.Reset
	ansiBold    = ansi.Bold
	ansiDim     = ansi.Dim
	ansiRed     = ansi.Red
	ansiGreen   = ansi.Green
	ansiYellow  = ansi.Yellow
	ansiCyan    = ansi.Cyan
	ansiReverse = ansi.Reverse
)

// colorFor returns the per-process colour or "" when colour is disabled.
// ansi.ProcessColor already honours $NO_COLOR / --no-color (via
// ansi.SetDisabled), so the monitor doesn't need to branch at callsites.
func colorFor(idx int) string {
	return ansi.ProcessColor(idx)
}

// ansiSGRRe matches only SGR (colour/style) sequences, which all end in 'm'.
// Cursor-positioning sequences (\x1b[<n>;<n>H, \x1b[2K, \x1b[?25h, …) are
// preserved — stripping those would obliterate the TUI's layout.
var ansiSGRRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripColor removes every SGR sequence from b. Called once per frame when
// colour is disabled so the constants above can stay valid in concatenation
// expressions throughout the file.
func stripColor(b []byte) []byte { return ansiSGRRe.ReplaceAll(b, nil) }

func (m *monitor) render() {
	m.write(m.renderActiveFrame())
}

func (m *monitor) renderActiveFrame() []byte {
	if m.showHelp && m.width >= 40 && m.height >= 8 {
		return m.renderHelpFrame()
	}
	return m.renderFrame()
}

func (m *monitor) renderFrame() []byte {
	if m.width < 40 || m.height < 8 {
		// Don't try to draw the table — the layout collapses below this.
		// Show a single hint line instead so the user understands why the
		// TUI looks empty rather than wondering if it has frozen.
		var buf bytes.Buffer
		lines := []string{
			fmt.Sprintf("terminal too small %dx%d", m.width, m.height),
			m.summaryLine(),
			"press q to quit",
			"resize to continue",
		}
		for i, line := range lines {
			if i >= m.height {
				break
			}
			fmt.Fprintf(&buf, "\033[%d;1H\033[K%s", i+1, truncate(line, m.width-1))
		}
		buf.WriteString("\033[J")
		return buf.Bytes()
	}
	var buf bytes.Buffer

	// Cursor to top-left only — no leading clear. Each row erases its
	// own tail with \033[K, and a single \033[J at the end wipes any
	// leftover rows. This "overwrite, don't clear" pattern avoids the
	// blank-screen flash that would otherwise flicker on terminals that
	// don't honour DEC mode 2026 (synchronized output).
	buf.WriteString("\033[H")

	row := 1

	// ── Header ────────────────────────────────────────────────────────────────
	title := "proc-compose monitor"
	hints := "q=quit  ?=help  ↑↓/jk=nav  ⏎=filter  a=all  PgUp/Dn=scroll"
	gap := m.width - utf8.RuneCountInString(title) - utf8.RuneCountInString(hints) - 2
	if gap < 1 {
		gap = 1
	}
	fmt.Fprintf(&buf, "\033[%d;1H\033[K%s%s%s%s%s%s%s",
		row,
		ansiBold+ansiCyan, title, ansiReset,
		strings.Repeat(" ", gap),
		ansiDim, hints, ansiReset)
	row++

	// ── Summary ───────────────────────────────────────────────────────────────
	fmt.Fprintf(&buf, "\033[%d;1H\033[K  %s%s%s",
		row, ansiBold, m.summaryLine(), ansiReset)
	row++

	// ── Tunnel URL (only shown when --tunnel is active) ───────────────────────
	if m.tunnelURL != "" {
		label := "  public: "
		urlPart := m.tunnelURL
		maxURLLen := m.width - utf8.RuneCountInString(label) - 2
		if maxURLLen > 10 && utf8.RuneCountInString(urlPart) > maxURLLen {
			urlPart = string([]rune(urlPart)[:maxURLLen-1]) + "…"
		}
		fmt.Fprintf(&buf, "\033[%d;1H\033[K%s%s%s%s%s",
			row, ansiDim, label, ansiBold+ansiGreen, urlPart, ansiReset)
		row++
	}

	// ── Process table header ──────────────────────────────────────────────────
	fmt.Fprintf(&buf, "\033[%d;1H\033[K%s  %-*s  %-11s  %-8s  %-6s  %-7s  %-8s  %s%s",
		row, ansiDim,
		m.maxProcNameLen(), "PROCESS", "STATUS", "RESTARTS", "CPU%", "MEM", "READY", "UPTIME", ansiReset)
	row++

	// ── Process rows ──────────────────────────────────────────────────────────
	for i, name := range m.procOrder {
		st := m.procs[name]
		cursor := "  "
		prefix := ""
		suffix := ""
		if i == m.selected {
			cursor = ansiReverse + "▶ " + ansiReset
			prefix = ansiReverse
			suffix = ansiReset
		}

		stateColor, stateSym := stateDisplay(st.State)
		uptime := "-"
		if !st.StartedAt.IsZero() && (st.State == "running" || st.State == "restarting") {
			uptime = formatDuration(time.Since(st.StartedAt))
		}

		restarts := fmt.Sprintf("%d", st.Restarts)
		c := colorFor(st.ColorIndex)
		cpuStr := formatCPU(st.CPUPercent)
		memStr := formatMemory(st.MemoryMB)
		readyStr := readinessDisplay(st)

		fmt.Fprintf(&buf, "\033[%d;1H\033[K%s%s%s%-*s%s  %s%s%-11s%s  %-8s  %-6s  %-7s  %-8s  %s%s",
			row,
			cursor, prefix,
			ansiBold+c, m.maxProcNameLen(), padRight(name, m.maxProcNameLen()), ansiReset,
			stateColor, stateSym, st.State, ansiReset,
			restarts,
			cpuStr,
			memStr,
			readyStr,
			uptime, suffix)
		row++
	}

	// ── Log area header ───────────────────────────────────────────────────────
	filterLabel := "Logs: all"
	if m.filter != "" {
		filterLabel = "Logs: filtered to " + m.filter
	}
	scrollHint := ""
	if m.logOffset > 0 {
		scrollHint = fmt.Sprintf("  %s[+%d lines]%s", ansiYellow, m.logOffset, ansiReset)
	}
	div := strings.Repeat("─", m.width)
	fmt.Fprintf(&buf, "\033[%d;1H\033[K%s", row, div)
	row++
	fmt.Fprintf(&buf, "\033[%d;1H\033[K%s%s%s%s",
		row, ansiBold, filterLabel, ansiReset, scrollHint)
	row++

	// ── Log lines ─────────────────────────────────────────────────────────────
	logRows := m.height - row
	if logRows < 1 {
		return buf.Bytes()
	}

	visible := m.filteredLogs()
	total := len(visible)

	// Apply scroll offset from the bottom.
	end := total - m.logOffset
	if end < 0 {
		end = 0
	}
	start := end - logRows
	if start < 0 {
		start = 0
	}
	display := visible[start:end]
	if len(display) == 0 {
		message := "No logs yet"
		if m.filter != "" {
			message = fmt.Sprintf("No logs for %s yet", m.filter)
		}
		fmt.Fprintf(&buf, "\033[%d;1H\033[K%s%s%s", row, ansiDim, message, ansiReset)
		row++
	}

	for _, entry := range display {
		c := colorFor(m.procColorIdx(entry.Process))
		lineColor := ansiReset
		if entry.IsError {
			lineColor = ansiRed
		} else if entry.IsEvent {
			lineColor = ansiYellow
		}

		line := truncate(entry.Line, m.width-m.maxProcNameLen()-4)
		fmt.Fprintf(&buf, "\033[%d;1H\033[K%s%s%-*s%s %s│%s %s%s%s",
			row,
			ansiBold+c, "", m.maxProcNameLen(), entry.Process, ansiReset,
			ansiDim, ansiReset,
			lineColor, line, ansiReset)
		row++
	}

	// Single clear-to-end-of-screen wipes any leftover rows below the
	// last log line. Cheaper than per-row \033[2K and produces no
	// flash because content above is already painted.
	if row <= m.height {
		fmt.Fprintf(&buf, "\033[%d;1H\033[J", row)
	}

	return buf.Bytes()
}

// write emits the assembled frame, stripping SGR escape sequences when
// colour is disabled so $NO_COLOR / --no-color produce a clean monochrome
// TUI without obliterating cursor-positioning sequences.
//
// Wraps the frame in DEC mode 2026 (synchronized output): supporting
// terminals buffer the entire repaint and flip atomically, eliminating
// flicker from the clear-screen + redraw sequence. Terminals that don't
// understand 2026 ignore the bracket sequences.
func (m *monitor) write(b []byte) {
	if ansi.Disabled() {
		b = stripColor(b)
	}
	var out bytes.Buffer
	out.Grow(len(b) + 16)
	out.WriteString("\033[?2026h")
	out.Write(b)
	out.WriteString("\033[?2026l")
	os.Stdout.Write(out.Bytes())
}

func (m *monitor) renderMessage(msg string) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\033[H\033[J\033[1;1H%s\n", msg)
	m.write(buf.Bytes())
}

// renderHelp displays a centered help overlay with all keyboard shortcuts.
func (m *monitor) renderHelp() {
	m.write(m.renderHelpFrame())
}

func (m *monitor) renderHelpFrame() []byte {
	var buf bytes.Buffer
	buf.WriteString("\033[H\033[J")

	lines := []string{
		"Keyboard Shortcuts",
		"",
		"  q, Ctrl+C        Quit",
		"  ?, h             Toggle this help",
		"  Esc, Space       Close this help",
		"  ↑/↓ or k/j       Navigate processes",
		"  Enter            Toggle filter for selected process",
		"  a                Show all logs (unfilter)",
		"  PgUp/PgDn        Scroll log area",
		"  G                Jump to live tail",
		"  g                Jump to top of buffer",
		"",
		"Press Esc, Space, or any key to close; press q to quit",
	}

	// Calculate centered box dimensions.
	boxWidth := 0
	for _, line := range lines {
		if w := utf8.RuneCountInString(line) + 4; w > boxWidth {
			boxWidth = w
		}
	}
	if boxWidth < 40 {
		boxWidth = 40
	}
	if m.width > 0 && boxWidth > m.width {
		boxWidth = m.width
	}
	boxHeight := len(lines) + 2 // +2 for border
	startRow := (m.height - boxHeight) / 2
	if startRow < 1 {
		startRow = 1
	}
	startCol := (m.width - boxWidth) / 2
	if startCol < 1 {
		startCol = 1
	}

	row := startRow
	// Top border
	fmt.Fprintf(&buf, "\033[%d;%dH%s%s%s%s",
		row, startCol, ansiBold, ansiCyan, strings.Repeat("─", boxWidth-2), ansiReset)
	row++

	for _, line := range lines {
		leftPad := (boxWidth - 2 - utf8.RuneCountInString(line)) / 2
		if leftPad < 0 {
			leftPad = 0
		}
		fmt.Fprintf(&buf, "\033[%d;%dH%s│%s %s %s%s%s",
			row, startCol, ansiCyan,
			ansiReset, strings.Repeat(" ", leftPad), ansiBold+ansiCyan, line, ansiReset)
		row++
	}

	// Bottom border
	fmt.Fprintf(&buf, "\033[%d;%dH%s%s%s",
		row, startCol, ansiBold+ansiCyan, strings.Repeat("─", boxWidth-2), ansiReset)

	return buf.Bytes()
}

func (m *monitor) filteredLogs() []ipc.LogEntry {
	logs := m.orderedLogs()
	if m.filter == "" {
		return logs
	}
	out := make([]ipc.LogEntry, 0, len(logs))
	for _, l := range logs {
		if l.Process == m.filter {
			out = append(out, l)
		}
	}
	return out
}

func (m *monitor) logAreaRows() int {
	extra := 0
	if m.tunnelURL != "" {
		extra = 1
	}
	rows := m.height - 4 - len(m.procOrder) - 2 - extra
	if rows < 1 {
		return 1
	}
	return rows
}

func (m *monitor) maxProcNameLen() int {
	n := 7 // minimum "PROCESS"
	for _, name := range m.procOrder {
		if len(name) > n {
			n = len(name)
		}
	}
	return n
}

func (m *monitor) procColorIdx(name string) int {
	if st, ok := m.procs[name]; ok {
		return st.ColorIndex
	}
	// Stable fallback: hash the name.
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}

func (m *monitor) summaryLine() string {
	total := len(m.procOrder)
	ready := 0
	failed := 0
	restarting := 0
	for _, name := range m.procOrder {
		st := m.procs[name]
		if st == nil {
			continue
		}
		if st.State == "running" && st.Ready {
			ready++
		}
		switch st.State {
		case "failed":
			failed++
		case "restarting":
			restarting++
		}
	}
	return fmt.Sprintf("ready %d/%d  failed %d  restarting %d", ready, total, failed, restarting)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func readinessDisplay(st *ipc.ProcState) string {
	if st == nil {
		return "-"
	}
	if st.State != "running" {
		return "-"
	}
	if st.Ready {
		return "ready"
	}
	return "waiting"
}

func stateDisplay(state string) (color, sym string) {
	switch state {
	case "running":
		return ansiGreen, "● "
	case "restarting":
		return ansiYellow, "↺ "
	case "failed":
		return ansiRed, "✗ "
	default:
		return ansiDim, "■ "
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func formatCPU(cpu float64) string {
	if cpu <= 0 {
		return "   -"
	}
	return fmt.Sprintf("%5.1f%%", cpu)
}

func formatMemory(memMB float64) string {
	if memMB <= 0 {
		return "     -"
	}
	return fmt.Sprintf("%6.1fMB", memMB)
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen-1]) + "…"
}

func padRight(s string, width int) string {
	remain := width - utf8.RuneCountInString(s)
	if remain <= 0 {
		return s
	}
	return s + strings.Repeat(" ", remain)
}
