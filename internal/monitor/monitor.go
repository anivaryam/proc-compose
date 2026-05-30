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
	"unicode"

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

	// Windows conhost ignores ANSI escapes unless ENABLE_VIRTUAL_TERMINAL_PROCESSING
	// is set on stdout. Without this, cursor positioning is dropped and every log
	// row overwrites the same line. No-op on Unix.
	if restore, err := enableVTOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to enable VT output: %v\n", err)
	} else {
		defer restore()
	}

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
		return 5
	}
	switch st.State {
	case "failed":
		return 0
	case "restarting":
		return 1
	case "running":
		if st.Ready {
			return 3
		}
		return 2
	case "completed":
		return 4
	default:
		return 5
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
	// own tail with \033[K while immediately redrawing the full bordered
	// row. This "overwrite, don't clear" pattern avoids the blank-screen
	// flash that would otherwise flicker on terminals that don't honour
	// DEC mode 2026 (synchronized output).
	buf.WriteString("\033[H")

	innerW := m.width - 2
	contentLimit := innerW - 1
	frameContent := func(content string) string {
		contentW := visibleWidth(content)
		if contentW > contentLimit {
			content = truncate(string(stripColor([]byte(content))), contentLimit)
			contentW = visibleWidth(content)
		}
		return content + strings.Repeat(" ", contentLimit-contentW)
	}
	writeBorderedRow := func(row int, content string) {
		if row <= 1 || row >= m.height {
			return
		}
		fmt.Fprintf(&buf, "\033[%d;1H\033[K%s│%s%s",
			row, ansiCyan, ansiReset, frameContent(content))
		fmt.Fprintf(&buf, "\033[%d;%dH%s│%s", row, m.width, ansiCyan, ansiReset)
	}
	writeBlankRows := func(from int) {
		for r := from; r < m.height; r++ {
			writeBorderedRow(r, "")
		}
	}

	fmt.Fprintf(&buf, "\033[1;1H\033[K%s┌%s┐%s",
		ansiBold+ansiCyan, strings.Repeat("─", innerW), ansiReset)

	row := 2

	// ── Header ────────────────────────────────────────────────────────────────
	title := "proc-compose monitor"
	hints := "q=quit  ?=help  ↑↓/jk=nav  ⏎=filter  a=all  PgUp/Dn=scroll"
	gap := innerW - cellWidthString(title) - cellWidthString(hints) - 2
	if gap < 1 {
		gap = 1
	}
	writeBorderedRow(row, fmt.Sprintf("%s%s%s%s%s%s%s",
		ansiBold+ansiCyan, title, ansiReset,
		strings.Repeat(" ", gap),
		ansiDim, hints, ansiReset))
	row++

	// ── Summary ───────────────────────────────────────────────────────────────
	writeBorderedRow(row, fmt.Sprintf("  %s%s%s", ansiBold, m.summaryLine(), ansiReset))
	row++

	// ── Tunnel URL (only shown when --tunnel is active) ───────────────────────
	if m.tunnelURL != "" {
		label := "  public: "
		urlPart := m.tunnelURL
		maxURLLen := innerW - cellWidthString(label) - 2
		if maxURLLen > 10 && cellWidthString(urlPart) > maxURLLen {
			urlPart = truncate(urlPart, maxURLLen)
		}
		writeBorderedRow(row, fmt.Sprintf("%s%s%s%s%s", ansiDim, label, ansiBold+ansiGreen, urlPart, ansiReset))
		row++
	}

	// ── Process table header ──────────────────────────────────────────────────
	writeBorderedRow(row, fmt.Sprintf("%s  %s  %-13s  %-8s  %-6s  %-8s  %-8s  %s%s",
		ansiDim,
		padRight("PROCESS", m.maxProcNameLen()), "STATUS", "RESTARTS", "CPU%", "MEM", "READY", "UPTIME", ansiReset))
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

		writeBorderedRow(row, fmt.Sprintf("%s%s%s%s%s  %s%s%-11s%s  %-8s  %-6s  %-8s  %-8s  %s%s",
			cursor, prefix,
			ansiBold+c, padRight(name, m.maxProcNameLen()), ansiReset,
			stateColor, stateSym, st.State, ansiReset,
			restarts,
			cpuStr,
			memStr,
			readyStr,
			uptime, suffix))
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
	div := strings.Repeat("─", innerW)
	writeBorderedRow(row, div)
	row++
	writeBorderedRow(row, fmt.Sprintf("%s%s%s%s", ansiBold, filterLabel, ansiReset, scrollHint))
	row++

	// ── Log lines ─────────────────────────────────────────────────────────────
	logRows := m.height - row
	if logRows < 1 {
		writeBlankRows(row)
		fmt.Fprintf(&buf, "\033[%d;1H\033[K%s└%s┘%s",
			m.height, ansiBold+ansiCyan, strings.Repeat("─", innerW), ansiReset)
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
		writeBorderedRow(row, fmt.Sprintf("%s%s%s", ansiDim, message, ansiReset))
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

		lineWidth := innerW - m.maxProcNameLen() - 4
		line := truncate(sanitizeLogLine(entry.Line), lineWidth)
		writeBorderedRow(row, fmt.Sprintf("%s%s%s %s│%s %s%s%s",
			ansiBold+c, padRight(entry.Process, m.maxProcNameLen()), ansiReset,
			ansiDim, ansiReset,
			lineColor, line, ansiReset))
		row++
	}

	writeBlankRows(row)
	fmt.Fprintf(&buf, "\033[%d;1H\033[K%s└%s┘%s",
		m.height, ansiBold+ansiCyan, strings.Repeat("─", innerW), ansiReset)

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

func (m *monitor) renderHelpFrame() []byte {
	var buf bytes.Buffer
	buf.WriteString("\033[H\033[J")

	type helpRow struct {
		key, desc string
	}
	shortcuts := []helpRow{
		{"q, Ctrl+C", "Quit"},
		{"?, h", "Toggle this help"},
		{"Esc, Space", "Close this help"},
		{"↑/↓ or k/j", "Navigate processes"},
		{"Enter", "Toggle filter for selected process"},
		{"a", "Show all logs (unfilter)"},
		{"PgUp/PgDn", "Scroll log area"},
		{"G", "Jump to live tail"},
		{"g", "Jump to top of buffer"},
	}
	header := "Keyboard Shortcuts"
	footer := "Press Esc, Space, or any key to close; press q to quit"

	// Width of the key column (max key width).
	keyW := 0
	for _, s := range shortcuts {
		if w := cellWidthString(s.key); w > keyW {
			keyW = w
		}
	}
	const gap = 3 // spaces between key and description
	// Width of shortcut content (key column + gap + longest description).
	descW := 0
	for _, s := range shortcuts {
		if w := cellWidthString(s.desc); w > descW {
			descW = w
		}
	}
	shortcutW := keyW + gap + descW

	// Box must fit the widest of header/footer/shortcut block plus padding.
	contentW := shortcutW
	if w := cellWidthString(header); w > contentW {
		contentW = w
	}
	if w := cellWidthString(footer); w > contentW {
		contentW = w
	}
	boxWidth := contentW + 4 // 1 border + 1 pad each side
	if boxWidth < 40 {
		boxWidth = 40
	}
	if m.width > 0 && boxWidth > m.width {
		boxWidth = m.width
	}

	// Interior width between the side borders (excluding borders, including pad).
	innerW := boxWidth - 2
	// Left offset of shortcut block within interior, centered.
	shortcutIndent := (innerW - shortcutW) / 2
	if shortcutIndent < 1 {
		shortcutIndent = 1
	}

	boxHeight := len(shortcuts) + 5 // header, blank, shortcuts, blank, footer + top/bottom border
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
	fmt.Fprintf(&buf, "\033[%d;%dH%s┌%s┐%s",
		row, startCol, ansiBold+ansiCyan, strings.Repeat("─", innerW), ansiReset)
	row++
	fitText := func(text string, width int) string {
		if width <= 0 {
			return ""
		}
		if cellWidthString(text) > width {
			return truncate(text, width)
		}
		return text
	}

	writeCentered := func(text string) {
		text = fitText(text, innerW)
		textW := cellWidthString(text)
		pad := (innerW - textW) / 2
		if pad < 0 {
			pad = 0
		}
		trail := innerW - pad - textW
		if trail < 0 {
			trail = 0
		}
		fmt.Fprintf(&buf, "\033[%d;%dH%s│%s%s%s%s%s%s%s│%s",
			row, startCol,
			ansiCyan, ansiReset,
			strings.Repeat(" ", pad), ansiBold+ansiCyan, text, ansiReset,
			strings.Repeat(" ", trail), ansiCyan, ansiReset)
		row++
	}
	writeBlank := func() {
		fmt.Fprintf(&buf, "\033[%d;%dH%s│%s%s%s│%s",
			row, startCol, ansiCyan, ansiReset, strings.Repeat(" ", innerW), ansiCyan, ansiReset)
		row++
	}

	writeCentered(header)
	writeBlank()
	for _, s := range shortcuts {
		desc := s.desc
		keyPad := keyW - cellWidthString(s.key)
		descW := innerW - shortcutIndent - keyPad - cellWidthString(s.key) - gap
		desc = fitText(desc, descW)
		rowW := shortcutIndent + keyPad + cellWidthString(s.key) + gap + cellWidthString(desc)
		trail := innerW - rowW
		if trail < 0 {
			trail = 0
		}
		fmt.Fprintf(&buf, "\033[%d;%dH%s│%s%s%s%s%s%s%s%s%s%s│%s",
			row, startCol, ansiCyan, ansiReset,
			strings.Repeat(" ", shortcutIndent),
			strings.Repeat(" ", keyPad),
			ansiBold+ansiCyan, s.key, ansiReset,
			strings.Repeat(" ", gap),
			desc,
			strings.Repeat(" ", trail), ansiCyan, ansiReset)
		row++
	}
	writeBlank()
	writeCentered(footer)

	// Bottom border
	fmt.Fprintf(&buf, "\033[%d;%dH%s└%s┘%s",
		row, startCol, ansiBold+ansiCyan, strings.Repeat("─", innerW), ansiReset)

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
	n := cellWidthString("PROCESS")
	for _, name := range m.procOrder {
		if w := cellWidthString(name); w > n {
			n = w
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
	completedTasks := 0
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
		if isCompletedTask(st) {
			completedTasks++
		}
		switch st.State {
		case "failed":
			failed++
		case "restarting":
			restarting++
		}
	}
	return fmt.Sprintf("ready %d/%d  tasks %d done  failed %d  restarting %d", ready, total, completedTasks, failed, restarting)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func readinessDisplay(st *ipc.ProcState) string {
	if st == nil {
		return "-"
	}
	if st.State == "completed" && st.Ready {
		return "done"
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
	case "completed":
		return ansiGreen, "✓ "
	default:
		return ansiDim, "■ "
	}
}

func isCompletedTask(st *ipc.ProcState) bool {
	return st != nil && st.State == "completed" && st.Ready && st.Mode == "task"
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
		return "-"
	}
	return fmt.Sprintf("%.1f%%", cpu)
}

func formatMemory(memMB float64) string {
	if memMB <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fMB", memMB)
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if cellWidthString(s) <= maxLen {
		return s
	}
	if maxLen == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := cellWidthRune(r)
		if used+w > maxLen-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + "…"
}

func padRight(s string, width int) string {
	remain := width - cellWidthString(s)
	if remain <= 0 {
		return s
	}
	return s + strings.Repeat(" ", remain)
}

func sanitizeLogLine(s string) string {
	s = string(stripColor([]byte(s)))
	var b strings.Builder
	changed := false
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
			changed = true
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			b.WriteRune(' ')
			changed = true
		default:
			b.WriteRune(r)
		}
	}
	if !changed {
		return s
	}
	return b.String()
}

func visibleWidth(s string) int {
	return cellWidthString(string(stripColor([]byte(s))))
}

func cellWidthString(s string) int {
	width := 0
	for _, r := range s {
		width += cellWidthRune(r)
	}
	return width
}

func cellWidthRune(r rune) int {
	switch {
	case r == '\t':
		return 4
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case unicode.Is(unicode.Mn, r):
		return 0
	case isWideRune(r):
		return 2
	default:
		return 1
	}
}

func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115f) ||
		(r >= 0x2300 && r <= 0x23ff) ||
		(r >= 0x2329 && r <= 0x232a) ||
		(r >= 0x2600 && r <= 0x27bf) ||
		(r >= 0x2e80 && r <= 0xa4cf) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1faff)
}
