package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

func newTestMonitor() *monitor {
	return &monitor{
		procOrder: []string{},
		procs:     map[string]*ipc.ProcState{},
	}
}

func TestApplyEvent_Snapshot(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{
		Type: ipc.TypeSnapshot,
		Processes: []ipc.ProcState{
			{Name: "backend", State: "running", ColorIndex: 0},
			{Name: "frontend", State: "running", ColorIndex: 1},
		},
		RecentLogs: []ipc.LogEntry{
			{Process: "backend", Line: "started"},
		},
		TunnelURL: "https://abc.tunnel/",
	})
	if len(m.procs) != 2 {
		t.Fatalf("procs len = %d, want 2", len(m.procs))
	}
	if got := m.procOrder; len(got) != 2 || got[0] != "backend" || got[1] != "frontend" {
		t.Errorf("procOrder = %v, want [backend frontend]", got)
	}
	if len(m.logs) != 1 {
		t.Errorf("logs len = %d, want 1", len(m.logs))
	}
	if m.tunnelURL != "https://abc.tunnel/" {
		t.Errorf("tunnelURL = %q", m.tunnelURL)
	}
}

func TestApplyEvent_LogAppend(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{Type: ipc.TypeLog, Log: &ipc.LogEntry{Process: "x", Line: "hello"}})
	if len(m.logs) != 1 || m.logs[0].Line != "hello" {
		t.Fatalf("logs = %v", m.logs)
	}
}

func TestApplyEvent_LogRingBuffer(t *testing.T) {
	m := newTestMonitor()
	for i := 0; i < maxLogs+50; i++ {
		m.applyEvent(ipc.Event{Type: ipc.TypeLog, Log: &ipc.LogEntry{Process: "x", Line: "line"}})
	}
	if len(m.logs) > maxLogs {
		t.Errorf("logs len %d exceeded maxLogs %d", len(m.logs), maxLogs)
	}
}

func TestApplyEvent_StateUpdate(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{
		Type: ipc.TypeState,
		Proc: &ipc.ProcState{Name: "svc", State: "failed", PID: 99},
	})
	st := m.procs["svc"]
	if st == nil {
		t.Fatal("expected state for 'svc'")
	}
	if st.State != "failed" || st.PID != 99 {
		t.Errorf("state = %+v", st)
	}
}

func TestApplyEvent_StateUpdateRebuildsHealthOrder(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{Type: ipc.TypeSnapshot, Processes: []ipc.ProcState{
		{Name: "api", State: "running", Ready: true},
		{Name: "worker", State: "running", Ready: true},
	}})

	m.applyEvent(ipc.Event{Type: ipc.TypeState, Proc: &ipc.ProcState{Name: "worker", State: "failed"}})

	if got := strings.Join(m.procOrder, ","); got != "worker,api" {
		t.Errorf("procOrder = %q, want worker,api", got)
	}
}

func TestApplyEvent_TunnelURL(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{Type: ipc.TypeTunnel, TunnelURL: "https://new.tunnel/"})
	if m.tunnelURL != "https://new.tunnel/" {
		t.Errorf("tunnelURL = %q", m.tunnelURL)
	}
}

func TestRebuildOrder_Sorted(t *testing.T) {
	m := newTestMonitor()
	m.procs["zzz"] = &ipc.ProcState{Name: "zzz", State: "running", Ready: true}
	m.procs["aaa"] = &ipc.ProcState{Name: "aaa", State: "running", Ready: true}
	m.procs["mmm"] = &ipc.ProcState{Name: "mmm", State: "running", Ready: true}
	m.rebuildOrder()
	want := []string{"aaa", "mmm", "zzz"}
	for i, n := range want {
		if m.procOrder[i] != n {
			t.Errorf("procOrder[%d] = %q, want %q", i, m.procOrder[i], n)
		}
	}
}

func TestRebuildOrder_SortsByHealthThenName(t *testing.T) {
	m := newTestMonitor()
	m.procs["ready-api"] = &ipc.ProcState{Name: "ready-api", State: "running", Ready: true}
	m.procs["starting-web"] = &ipc.ProcState{Name: "starting-web", State: "running"}
	m.procs["failed-worker"] = &ipc.ProcState{Name: "failed-worker", State: "failed"}
	m.procs["restarting-db"] = &ipc.ProcState{Name: "restarting-db", State: "restarting"}
	m.procs["ready-web"] = &ipc.ProcState{Name: "ready-web", State: "running", Ready: true}

	m.rebuildOrder()

	want := []string{"failed-worker", "restarting-db", "starting-web", "ready-api", "ready-web"}
	if got := strings.Join(m.procOrder, ","); got != strings.Join(want, ",") {
		t.Errorf("procOrder = %q, want %q", got, strings.Join(want, ","))
	}
}

func TestRebuildOrder_ClampSelected(t *testing.T) {
	m := newTestMonitor()
	m.procs["a"] = &ipc.ProcState{Name: "a"}
	m.procs["b"] = &ipc.ProcState{Name: "b"}
	m.selected = 5
	m.rebuildOrder()
	if m.selected != 1 {
		t.Errorf("selected = %d, want 1 (clamped)", m.selected)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{499 * time.Millisecond, "0s"},
		{5 * time.Second, "5s"},
		{61 * time.Second, "1m1s"},
		{2*time.Minute + 30*time.Second, "2m30s"},
		{3*time.Hour + 5*time.Minute + 12*time.Second, "3h5m12s"},
	}
	for _, c := range cases {
		got := formatDuration(c.d)
		if got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		s      string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hell…"},
		{"abc", 3, "abc"},
		{"abcd", 3, "ab…"},
		{"", 5, ""},
		{"any", 0, ""},
		{"any", -1, ""},
	}
	for _, c := range cases {
		got := truncate(c.s, c.maxLen)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.s, c.maxLen, got, c.want)
		}
	}
}

func TestStateDisplay(t *testing.T) {
	cases := []struct{ state, wantSym string }{
		{"running", "● "},
		{"restarting", "↺ "},
		{"failed", "✗ "},
		{"exited", "■ "},
		{"unknown", "■ "},
	}
	for _, c := range cases {
		_, sym := stateDisplay(c.state)
		if sym != c.wantSym {
			t.Errorf("stateDisplay(%q) sym = %q, want %q", c.state, sym, c.wantSym)
		}
	}
}

func TestProcColorIdx_Stable(t *testing.T) {
	m := newTestMonitor()
	m.procs["x"] = &ipc.ProcState{Name: "x", ColorIndex: 3}
	if got := m.procColorIdx("x"); got != 3 {
		t.Errorf("procColorIdx(known) = %d, want 3", got)
	}
	// Unknown name: stable hash; just ensure non-negative.
	if got := m.procColorIdx("unknown"); got < 0 {
		t.Errorf("procColorIdx(unknown) = %d, want non-negative", got)
	}
}

func TestApplyEvent_LogScrollOffsetIncrements(t *testing.T) {
	m := newTestMonitor()
	m.pushLog(ipc.LogEntry{Line: "old"})
	m.logOffset = 1
	m.applyEvent(ipc.Event{Type: ipc.TypeLog, Log: &ipc.LogEntry{Line: "new"}})
	if m.logOffset != 2 {
		t.Errorf("logOffset = %d, want 2 (preserves scroll position)", m.logOffset)
	}
}

func TestFilteredLogs(t *testing.T) {
	m := newTestMonitor()
	m.pushLogs([]ipc.LogEntry{
		{Process: "a", Line: "1"},
		{Process: "b", Line: "2"},
		{Process: "a", Line: "3"},
	})
	m.filter = ""
	if got := m.filteredLogs(); len(got) != 3 {
		t.Errorf("all filter: len = %d, want 3", len(got))
	}
	m.filter = "a"
	got := m.filteredLogs()
	if len(got) != 2 {
		t.Fatalf("filter=a len = %d, want 2", len(got))
	}
	for _, l := range got {
		if l.Process != "a" {
			t.Errorf("filtered entry has process %q", l.Process)
		}
	}
}

func TestApplyEvent_SnapshotMultipleProcsAreSorted(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{
		Type: ipc.TypeSnapshot,
		Processes: []ipc.ProcState{
			{Name: "zeta", State: "running", Ready: true},
			{Name: "alpha", State: "running", Ready: true},
			{Name: "mu", State: "running", Ready: true},
		},
	})
	got := strings.Join(m.procOrder, ",")
	if got != "alpha,mu,zeta" {
		t.Errorf("procOrder = %q, want alpha,mu,zeta", got)
	}
}

func TestHandleKeyEnterTogglesSelectedProcessFilter(t *testing.T) {
	m := newTestMonitor()
	m.procOrder = []string{"api", "web"}
	m.selected = 1
	m.filter = "web"
	m.logOffset = 3

	m.handleKey("enter")

	if m.filter != "" {
		t.Errorf("filter = %q, want cleared", m.filter)
	}
	if m.logOffset != 0 {
		t.Errorf("logOffset = %d, want 0", m.logOffset)
	}

	m.handleKey("enter")
	if m.filter != "web" {
		t.Errorf("filter = %q, want web", m.filter)
	}
}

func TestRenderFrameShowsReadinessSummaryAndFilterState(t *testing.T) {
	m := newTestMonitor()
	m.width = 120
	m.height = 20
	m.filter = "api"
	m.applyEvent(ipc.Event{
		Type: ipc.TypeSnapshot,
		Processes: []ipc.ProcState{
			{Name: "api", State: "running", Ready: true},
			{Name: "web", State: "running"},
			{Name: "worker", State: "failed"},
		},
		RecentLogs: []ipc.LogEntry{{Process: "api", Line: "listening"}},
	})

	out := string(stripColor(m.renderFrame()))
	for _, want := range []string{"READY", "ready 1/3", "failed 1", "Logs: filtered to api", "ready", "waiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("render output missing %q:\n%s", want, out)
		}
	}
}

func TestSummaryLineCountsOnlyRunningReadyProcesses(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{Type: ipc.TypeSnapshot, Processes: []ipc.ProcState{
		{Name: "api", State: "running", Ready: true},
		{Name: "worker", State: "failed", Ready: true},
	}})

	if got := m.summaryLine(); !strings.Contains(got, "ready 1/2") {
		t.Errorf("summaryLine = %q, want ready 1/2", got)
	}
}

func TestRenderFrameShowsEmptyLogStates(t *testing.T) {
	m := newTestMonitor()
	m.width = 100
	m.height = 16
	m.applyEvent(ipc.Event{Type: ipc.TypeSnapshot, Processes: []ipc.ProcState{{Name: "api", State: "running", Ready: true}}})

	out := string(stripColor(m.renderFrame()))
	if !strings.Contains(out, "No logs yet") {
		t.Errorf("render output missing empty log message:\n%s", out)
	}

	m.filter = "api"
	m.pushLog(ipc.LogEntry{Process: "web", Line: "hello"})
	out = string(stripColor(m.renderFrame()))
	if !strings.Contains(out, "No logs for api yet") {
		t.Errorf("render output missing filtered empty message:\n%s", out)
	}
}

func TestRenderHelpMentionsDismissKeysAndQuit(t *testing.T) {
	m := newTestMonitor()
	m.width = 80
	m.height = 24

	out := string(stripColor(m.renderHelpFrame()))
	for _, want := range []string{"Esc", "Space", "press q to quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q:\n%s", want, out)
		}
	}
}

func TestSmallTerminalFallbackIncludesSummary(t *testing.T) {
	m := newTestMonitor()
	m.width = 30
	m.height = 6
	m.applyEvent(ipc.Event{Type: ipc.TypeSnapshot, Processes: []ipc.ProcState{{Name: "api", State: "running", Ready: true}}})

	out := string(stripColor(m.renderFrame()))
	if !strings.Contains(out, "ready 1/1") {
		t.Errorf("small terminal output missing summary:\n%s", out)
	}
}

func TestActiveFrameUsesSmallTerminalFallbackBeforeHelpOverlay(t *testing.T) {
	m := newTestMonitor()
	m.width = 30
	m.height = 6
	m.showHelp = true

	out := string(stripColor(m.renderActiveFrame()))
	if !strings.Contains(out, "terminal too small") {
		t.Errorf("active frame should show small terminal fallback, got:\n%s", out)
	}
}

func TestApplyEvent_SnapshotAddsNewProc(t *testing.T) {
	m := newTestMonitor()
	m.procs["old"] = &ipc.ProcState{Name: "old"}
	m.procOrder = []string{"old"}
	m.applyEvent(ipc.Event{
		Type: ipc.TypeSnapshot,
		Processes: []ipc.ProcState{
			{Name: "new"},
		},
	})
	// Snapshot adds to existing procs map (doesn't clear it)
	if _, ok := m.procs["old"]; !ok {
		t.Error("old proc should still be present")
	}
	if _, ok := m.procs["new"]; !ok {
		t.Error("new proc should be present")
	}
}

func TestFilteredLogs_EmptyFilter(t *testing.T) {
	m := newTestMonitor()
	m.pushLogs([]ipc.LogEntry{
		{Process: "a", Line: "1"},
		{Process: "b", Line: "2"},
	})
	m.filter = ""
	got := m.filteredLogs()
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestFilteredLogs_NoMatch(t *testing.T) {
	m := newTestMonitor()
	m.pushLogs([]ipc.LogEntry{
		{Process: "a", Line: "1"},
		{Process: "b", Line: "2"},
	})
	m.filter = "nonexistent"
	got := m.filteredLogs()
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestMaxProcNameLen_ReturnsMinWidth(t *testing.T) {
	m := newTestMonitor()
	// maxProcNameLen defaults to 7 (len of "PROCESS")
	if got := m.maxProcNameLen(); got != 7 {
		t.Errorf("maxProcNameLen = %d, want 7 (minimum width)", got)
	}
}

func TestMaxProcNameLen_WithLongerNames(t *testing.T) {
	m := newTestMonitor()
	m.applyEvent(ipc.Event{
		Type: ipc.TypeSnapshot,
		Processes: []ipc.ProcState{
			{Name: "a"},
			{Name: "longname"}, // 8 chars
		},
	})
	if got := m.maxProcNameLen(); got != 8 {
		t.Errorf("maxProcNameLen = %d, want 8", got)
	}
}

func TestLogAreaRows_Defaults(t *testing.T) {
	m := newTestMonitor()
	m.height = 80
	if got := m.logAreaRows(); got <= 0 {
		t.Errorf("logAreaRows = %d, want positive", got)
	}
}
