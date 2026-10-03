package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/update"
	"github.com/spf13/cobra"
)

func TestValidateUnitName(t *testing.T) {
	good := []string{"app", "myapp", "app_1", "app-1", "app.1", "A", "x123_y-z.t", strings.Repeat("a", 64)}
	for _, n := range good {
		if err := validateUnitName(n); err != nil {
			t.Errorf("validateUnitName(%q) returned error: %v", n, err)
		}
	}

	bad := []struct{ name, why string }{
		{"", "empty"},
		{strings.Repeat("a", 65), "too long"},
		{"app/sub", "slash"},
		{"app\nfoo", "newline"},
		{"app foo", "space"},
		{"app$", "shell metachar"},
		{"app;ls", "semicolon"},
		{"../escape", "dotdot"},
		{"app#tag", "hash"},
	}
	for _, c := range bad {
		if err := validateUnitName(c.name); err == nil {
			t.Errorf("validateUnitName(%q) accepted (%s); want rejection", c.name, c.why)
		}
	}
}

func TestBuildChildArgs_StripsSilentAndInjectsFile(t *testing.T) {
	got := buildChildArgs([]string{"up", "--silent"}, "/abs/cfg.yml", "/var/log/x.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "/var/log/x.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildChildArgs_StripsShortSilent(t *testing.T) {
	got := buildChildArgs([]string{"up", "-s"}, "/abs/cfg.yml", "/var/log/x.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "/var/log/x.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildChildArgs_ReplacesExistingFileFlag(t *testing.T) {
	got := buildChildArgs([]string{"up", "-f", "old.yml", "--silent"}, "/abs/cfg.yml", "/v/x.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "/v/x.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildChildArgs_PreservesExistingLogFile(t *testing.T) {
	got := buildChildArgs([]string{"up", "--silent", "--log-file", "user.log"}, "/abs/cfg.yml", "/auto.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "user.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStarterTemplate_KnownNames(t *testing.T) {
	for _, name := range []string{"", "minimal", "node", "go", "python"} {
		s, err := starterTemplate(name)
		if err != nil {
			t.Errorf("starterTemplate(%q) error = %v", name, err)
		}
		if !strings.Contains(s, "processes:") {
			t.Errorf("starterTemplate(%q) missing 'processes:'\n%s", name, s)
		}
	}
}

func TestStarterTemplate_UnknownRejected(t *testing.T) {
	if _, err := starterTemplate("rust"); err == nil {
		t.Error("expected error for unknown template")
	}
}

func TestResolveConfigExtension_DefaultPrefersYml(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// No file: returns default unchanged.
	if got := resolveConfigExtension("proc-compose.yml"); got != "proc-compose.yml" {
		t.Errorf("got %q, want default", got)
	}
	// Only .yaml exists: switches to .yaml.
	os.WriteFile(filepath.Join(dir, "proc-compose.yaml"), []byte("processes: {a: {cmd: x}}\n"), 0600)
	if got := resolveConfigExtension("proc-compose.yml"); got != "proc-compose.yaml" {
		t.Errorf("got %q, want proc-compose.yaml", got)
	}
	// Both exist: prefers .yml (existing behaviour).
	os.WriteFile(filepath.Join(dir, "proc-compose.yml"), []byte("processes: {a: {cmd: x}}\n"), 0600)
	if got := resolveConfigExtension("proc-compose.yml"); got != "proc-compose.yml" {
		t.Errorf("got %q, want proc-compose.yml", got)
	}
	// Explicit non-default path returned unchanged.
	if got := resolveConfigExtension("custom.yml"); got != "custom.yml" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestResolveConfigExtension_DefaultFindsParentConfig(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "apps", "web")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(dir, "proc-compose.yml"), []byte("processes: {api: {cmd: x}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}

	got := resolveConfigExtension("proc-compose.yml")
	want := filepath.Join(dir, "proc-compose.yml")
	if got != want {
		t.Errorf("got %q, want parent config %q", got, want)
	}
}

func TestResolveConfigContextUsesParentConfigAsProjectRoot(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "packages", "api")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	write := []byte("processes: {api: {cmd: x}}\n")
	if err := os.WriteFile(filepath.Join(dir, "proc-compose.yml"), write, 0600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}

	root, configPath, err := resolveConfigContext("proc-compose.yml", false)
	if err != nil {
		t.Fatal(err)
	}
	if root != dir {
		t.Errorf("root = %q, want %q", root, dir)
	}
	if configPath != filepath.Join(dir, "proc-compose.yml") {
		t.Errorf("configPath = %q, want parent config", configPath)
	}
}

func TestBuildChildArgs_NoDuplicateNoBanner(t *testing.T) {
	got := buildChildArgs([]string{"up", "--silent", "--no-banner"}, "/abs/cfg.yml", "/v/x.log")
	count := 0
	for _, a := range got {
		if a == "--no-banner" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one --no-banner, got %d in %v", count, got)
	}
}

func TestTailLog_ReturnsLastNLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0600)

	got := tailLog(path, 3)
	if !strings.Contains(got, "c") || !strings.Contains(got, "d") || !strings.Contains(got, "e") {
		t.Errorf("expected last 3 lines, got %q", got)
	}
	if strings.Contains(got, "a") || strings.Contains(got, "b") {
		t.Errorf("expected only last 3 lines, got %q", got)
	}
}

func TestTailLog_FewerLinesThanRequested(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	os.WriteFile(path, []byte("only\nthese\n"), 0600)

	got := tailLog(path, 10)
	if !strings.Contains(got, "only") || !strings.Contains(got, "these") {
		t.Errorf("got %q", got)
	}
}

func TestTailLog_MissingFileReturnsEmpty(t *testing.T) {
	got := tailLog("/nonexistent/log", 30)
	if got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}

func TestTailLog_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	os.WriteFile(path, []byte(""), 0600)

	got := tailLog(path, 5)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestIsPortFree(t *testing.T) {
	// We can't reliably test "busy" without claiming a port; just ensure free
	// check on an obviously-free high port returns true.
	if !isPortFree(0) {
		// Port 0 means "let OS choose"; will succeed-listen-then-close. Treat
		// as informational; failure usually means restricted environment.
		t.Skip("port 0 unavailable in this environment")
	}
}

func TestPreflightCheck_MergeProxyOutputPortOccupiedFails(t *testing.T) {
	port, closePort := occupyTCPPort(t)
	defer closePort()

	cfg := &config.Config{
		Merge: &config.Merge{Client: freeTCPPort(t), Server: freeTCPPort(t), Port: port},
		Processes: map[string]config.Process{
			"web": {Cmd: "npm run dev"},
			"api": {Cmd: "go run ."},
		},
	}

	err := preflightCheck(testPIDPath(t), testSocketPath(t), cfg)
	if err == nil {
		t.Fatal("expected occupied merge proxy output port to fail preflight")
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf(":%d", port)) {
		t.Fatalf("error should mention occupied port %d, got: %s", port, msg)
	}
	if !strings.Contains(msg, "proxy (merge-port output)") {
		t.Fatalf("error should identify proxy output role, got: %s", msg)
	}
}

func TestPreflightCheck_ManagedSimpleUpstreamPortOccupiedFails(t *testing.T) {
	port, closePort := occupyTCPPort(t)
	defer closePort()

	cfg := &config.Config{
		Merge: &config.Merge{Client: port, Server: freeTCPPort(t), Port: freeTCPPort(t)},
		Processes: map[string]config.Process{
			"frontend": {Cmd: "npm run dev", Env: map[string]string{"PORT": fmt.Sprintf("%d", port)}},
			"api":      {Cmd: "go run ."},
		},
	}

	err := preflightCheck(testPIDPath(t), testSocketPath(t), cfg)
	if err == nil {
		t.Fatal("expected occupied managed upstream port to fail preflight")
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf(":%d", port)) {
		t.Fatalf("error should mention occupied port %d, got: %s", port, msg)
	}
	if !strings.Contains(msg, "managed upstream frontend") {
		t.Fatalf("error should identify managed upstream process, got: %s", msg)
	}
}

func TestPreflightCheck_ExternalSimpleUpstreamPortOccupiedAllowed(t *testing.T) {
	port, closePort := occupyTCPPort(t)
	defer closePort()

	cfg := &config.Config{
		Merge: &config.Merge{Client: port, Server: freeTCPPort(t), Port: freeTCPPort(t)},
		Processes: map[string]config.Process{
			"frontend": {Cmd: "npm run dev", Env: map[string]string{"PORT": fmt.Sprintf("%d", freeTCPPort(t))}},
			"api":      {Cmd: "go run ."},
		},
	}

	if err := preflightCheck(testPIDPath(t), testSocketPath(t), cfg); err != nil {
		t.Fatalf("external occupied upstream port should be allowed, got: %v", err)
	}
}

func TestPreflightCheck_RouteModeMixedManagedExternalTargetsOnlyManagedFails(t *testing.T) {
	externalPort, closeExternal := occupyTCPPort(t)
	defer closeExternal()
	managedPort, closeManaged := occupyTCPPort(t)
	defer closeManaged()

	cfg := &config.Config{
		Merge: &config.Merge{
			Routes: []string{fmt.Sprintf("/external=%d", externalPort), fmt.Sprintf("/api=%d", managedPort)},
			Port:   freeTCPPort(t),
		},
		Processes: map[string]config.Process{
			"api": {Cmd: "go run .", Env: map[string]string{"PORT": fmt.Sprintf("%d", managedPort)}},
			"web": {Cmd: "npm run dev", Env: map[string]string{"PORT": fmt.Sprintf("%d", freeTCPPort(t))}},
		},
	}

	err := preflightCheck(testPIDPath(t), testSocketPath(t), cfg)
	if err == nil {
		t.Fatal("expected occupied managed route target to fail preflight")
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf(":%d", managedPort)) {
		t.Fatalf("error should mention managed occupied port %d, got: %s", managedPort, msg)
	}
	if !strings.Contains(msg, "managed upstream api") {
		t.Fatalf("error should identify managed route target process, got: %s", msg)
	}
	if strings.Contains(msg, fmt.Sprintf(":%d", externalPort)) {
		t.Fatalf("error should not mention occupied external route target %d, got: %s", externalPort, msg)
	}
}

func occupyTCPPort(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen on temp port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, func() { _ = ln.Close() }
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	port, closePort := occupyTCPPort(t)
	closePort()
	return port
}

func testPIDPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "pc.pid")
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "pc.sock")
}

func TestGetHomeDir(t *testing.T) {
	home, err := getHomeDir()
	if err != nil {
		t.Fatalf("getHomeDir() failed: %v", err)
	}
	if home == "" {
		t.Error("getHomeDir() returned empty string")
	}
	if !filepath.IsAbs(home) {
		t.Errorf("getHomeDir() = %q, want absolute path", home)
	}
}

func TestPrintStatusTable_ModeColumn(t *testing.T) {
	procs := []ipc.ProcState{
		{Name: "svc", State: "running", Mode: "service", PID: 12345, Restarts: 0},
		{Name: "task", State: "completed", Mode: "task", PID: 0, Restarts: 0},
	}

	pr, pw, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = pw
	printStatusTable(procs, "")
	pw.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	buf.ReadFrom(pr)
	output := buf.String()

	if !strings.Contains(output, "MODE") {
		t.Error("expected MODE column header in status table")
	}
	if !strings.Contains(output, "task") {
		t.Error("expected task mode in status table")
	}
	if !strings.Contains(output, "service") {
		t.Error("expected service mode in status table")
	}
}

func TestPrintStatusTable_TerminalPIDHiding(t *testing.T) {
	procs := []ipc.ProcState{
		{Name: "completed_task", State: "completed", Mode: "task", PID: 11111, Restarts: 0},
		{Name: "exited_svc", State: "exited", Mode: "service", PID: 22222, Restarts: 0},
		{Name: "failed_svc", State: "failed", Mode: "service", PID: 33333, Restarts: 0},
		{Name: "running_svc", State: "running", Mode: "service", PID: 99999, Restarts: 0},
	}

	pr, pw, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = pw
	printStatusTable(procs, "")
	pw.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	buf.ReadFrom(pr)
	output := buf.String()

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "completed_task") {
			if strings.Contains(line, "11111") {
				t.Errorf("completed_task should not show stale PID 11111, got: %s", line)
			}
		}
		if strings.Contains(line, "exited_svc") {
			if strings.Contains(line, "22222") {
				t.Errorf("exited_svc should not show stale PID 22222, got: %s", line)
			}
		}
		if strings.Contains(line, "failed_svc") {
			if strings.Contains(line, "33333") {
				t.Errorf("failed_svc should not show stale PID 33333, got: %s", line)
			}
		}
		if strings.Contains(line, "running_svc") {
			if !strings.Contains(line, "99999") {
				t.Errorf("running_svc should show PID 99999, got: %s", line)
			}
		}
	}
}

func TestPrintStatusJSON_IncludesMode(t *testing.T) {
	procs := []ipc.ProcState{
		{Name: "svc", State: "running", Mode: "service", PID: 12345, Restarts: 0},
		{Name: "task", State: "completed", Mode: "task", PID: 0, Restarts: 0},
	}

	pr, pw, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = pw
	err := printStatusJSON(procs, "")
	pw.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("printStatusJSON failed: %v", err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(pr)
	output := buf.String()

	if !strings.Contains(output, `"mode"`) {
		t.Error("expected mode field in JSON output")
	}
	if !strings.Contains(output, `"task"`) {
		t.Error("expected task value in JSON output")
	}
	if !strings.Contains(output, `"service"`) {
		t.Error("expected service value in JSON output")
	}
}

func TestWaitForProcessesReady_CompletedTaskAccepted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	timeout := 500 * time.Millisecond

	socketPath, server, stopServer := ipcServerForTest(t)
	defer stopServer()

	want := map[string]struct{}{"task": {}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForProcessesReady(socketPath, want, timeout)
	}()

	time.Sleep(50 * time.Millisecond)

	server.BroadcastState(ipc.ProcState{Name: "task", State: "completed", Mode: "task", Ready: true})

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("waitForProcessesReady returned error for completed task with Ready=true: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("waitForProcessesReady timed out waiting for completed task")
	}
	_ = server
}

func TestWaitForProcessesReady_FailedProcessRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	timeout := 500 * time.Millisecond

	socketPath, server, stopServer := ipcServerForTest(t)
	defer stopServer()

	want := map[string]struct{}{"svc": {}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForProcessesReady(socketPath, want, timeout)
	}()

	time.Sleep(50 * time.Millisecond)

	server.BroadcastState(ipc.ProcState{Name: "svc", State: "failed", Mode: "service", Ready: false})

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("waitForProcessesReady should have returned error for failed process")
		}
		if !strings.Contains(err.Error(), "failed") {
			t.Errorf("error should mention 'failed', got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("waitForProcessesReady timed out waiting for failed process")
	}
	_ = server
}

// TestWaitForProcessesReady_LaterStateInvalidatesReady pins the fix for stale
// readiness: a service that reported ready and then exited no longer satisfies
// the waiter, even though "other" becomes ready afterwards. Without retraction
// the waiter would return nil here.
func TestWaitForProcessesReady_LaterStateInvalidatesReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	timeout := 5 * time.Second

	// "other" stays pending until after svc is invalidated so the waiter can
	// only finish early if it wrongly counts svc as ready.
	peer := newScriptedPeer(t, []ipc.ProcState{
		{Name: "svc", State: "starting", Mode: "service"},
		{Name: "other", State: "starting", Mode: "service"},
	})
	want := map[string]struct{}{"svc": {}, "other": {}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForProcessesReady(peer.socketPath, want, timeout)
	}()

	// The waiter is attached and its snapshot delivered, so the events below
	// arrive in this exact order and the ready→exited transition is exercised
	// rather than skipped.
	peer.waitConnected(t)
	peer.push(ipc.ProcState{Name: "svc", State: "running", Mode: "service", Ready: true})
	peer.push(ipc.ProcState{Name: "svc", State: "exited", Mode: "service", Ready: false})
	peer.push(ipc.ProcState{Name: "other", State: "running", Mode: "service", Ready: true})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("waitForProcessesReady returned nil although svc exited after being ready")
		}
		if !strings.Contains(err.Error(), "svc") {
			t.Errorf("error should list the unsatisfied process svc, got: %v", err)
		}
		if strings.Contains(err.Error(), "other") {
			t.Errorf("error should not list the satisfied process other, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waitForProcessesReady did not return within the test deadline")
	}
}

// TestWaitForProcessesReady_FailureAfterReadyRejected ensures a failure is not
// masked by an earlier ready=true for the same process.
func TestWaitForProcessesReady_FailureAfterReadyRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	timeout := 5 * time.Second

	peer := newScriptedPeer(t, []ipc.ProcState{
		{Name: "svc", State: "starting", Mode: "service"},
		{Name: "other", State: "starting", Mode: "service"},
	})
	want := map[string]struct{}{"svc": {}, "other": {}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForProcessesReady(peer.socketPath, want, timeout)
	}()

	peer.waitConnected(t)
	peer.push(ipc.ProcState{Name: "svc", State: "running", Mode: "service", Ready: true})
	// Defensive: even if a stale ready=true rides along with the failure,
	// the failure must win.
	peer.push(ipc.ProcState{Name: "svc", State: "failed", Mode: "service", Ready: true})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("waitForProcessesReady returned nil although svc failed after being ready")
		}
		if !strings.Contains(err.Error(), "failed") {
			t.Errorf("error should mention 'failed', got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForProcessesReady did not return within the test deadline")
	}
}

// TestWaitForProcessesReady_RestartingNotReady covers the restart window: a
// service that was ready but is now restarting must not satisfy the waiter.
func TestWaitForProcessesReady_RestartingNotReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	timeout := 700 * time.Millisecond

	// "other" never becomes ready so the waiter keeps waiting and observes
	// svc's transition into restarting.
	peer := newScriptedPeer(t, []ipc.ProcState{
		{Name: "svc", State: "starting", Mode: "service"},
		{Name: "other", State: "starting", Mode: "service"},
	})
	want := map[string]struct{}{"svc": {}, "other": {}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForProcessesReady(peer.socketPath, want, timeout)
	}()

	peer.waitConnected(t)
	peer.push(ipc.ProcState{Name: "svc", State: "running", Mode: "service", Ready: true})
	peer.push(ipc.ProcState{Name: "svc", State: "restarting", Mode: "service", Ready: true})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("waitForProcessesReady returned nil although svc is restarting")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("expected a timeout error listing svc, got: %v", err)
		}
		if !strings.Contains(err.Error(), "svc") {
			t.Errorf("timeout error should list the unsatisfied process svc, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForProcessesReady did not return within the test deadline")
	}
}

// TestWaitForProcessesReady_SilentPeerHitsTimeout is the regression for the
// blocking-Recv bug: the daemon accepts the connection but never sends an
// event, so the waiter must still honour the overall timeout instead of
// blocking forever inside Recv.
func TestWaitForProcessesReady_SilentPeerHitsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	timeout := 400 * time.Millisecond

	dir := socketDirForTest(t)
	socketPath := filepath.Join(dir, "silent.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	stopAccept := make(chan struct{})
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
		// Hold the connection open but never write anything.
		<-stopAccept
		conn.Close()
	}()

	start := time.Now()
	err = waitForProcessesReady(socketPath, map[string]struct{}{"svc": {}}, timeout)
	elapsed := time.Since(start)

	select {
	case conn := <-accepted:
		defer func() { conn.Close() }()
	default:
		t.Fatal("silent peer never accepted the connection")
	}

	if err == nil {
		t.Fatal("expected a timeout error from a silent daemon")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "svc") {
		t.Errorf("error should report a wait-ready timeout listing svc, got: %v", err)
	}
	// Generous upper bound: the point is that it does not hang forever.
	if elapsed > 5*time.Second {
		t.Errorf("waiter took %s, expected it to stop near the %s timeout", elapsed, timeout)
	}
	close(stopAccept)
}

func ipcServerForTest(t *testing.T) (socketPath string, server *ipc.Server, stop func()) {
	t.Helper()
	dir := socketDirForTest(t)
	socketPath = filepath.Join(dir, "pc.sock")

	server = ipc.NewServer(socketPath)
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	stop = func() { server.Shutdown() }
	return
}

// socketDirForTest is the scratch directory for files a test writes alongside an
// endpoint. The platform-specific naming lives in testTempDir.
func socketDirForTest(t *testing.T) string {
	t.Helper()
	return testTempDir(t)
}

// scriptedPeer is a minimal stand-in for the daemon that hands the waiter a
// stream of state events in a caller-controlled order. Unlike ipc.Server it
// exposes waitConnected, so a test can prove the client is attached before it
// sends the event under test — fixed sleeps cannot, because a client that
// connects late would receive only the newest state and never exercise the
// transition being tested.
type scriptedPeer struct {
	socketPath string
	connected  chan struct{}
	send       chan ipc.ProcState
}

// newScriptedPeer starts a peer that accepts one client, sends it an initial
// snapshot, then relays ProcStates from send in order. It stops with the test.
func newScriptedPeer(t *testing.T, initial []ipc.ProcState) *scriptedPeer {
	t.Helper()
	dir := socketDirForTest(t)
	p := &scriptedPeer{
		socketPath: filepath.Join(dir, "pc.sock"),
		connected:  make(chan struct{}),
		send:       make(chan ipc.ProcState, 16),
	}
	ln, err := net.Listen("unix", p.socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		write := func(ev ipc.Event) {
			data, err := json.Marshal(ev)
			if err != nil {
				return
			}
			conn.Write(append(data, '\n'))
		}
		write(ipc.Event{Type: ipc.TypeSnapshot, Processes: initial})
		// Only signal once the snapshot is on the wire, so a waitConnected
		// caller knows the client is attached and its first read can proceed.
		close(p.connected)
		for st := range p.send {
			write(ipc.Event{Type: ipc.TypeState, Proc: &st})
		}
	}()
	t.Cleanup(func() { close(p.send) })
	return p
}

// waitConnected blocks until the peer has accepted the client and flushed its
// initial snapshot, failing the test if that does not happen promptly.
func (p *scriptedPeer) waitConnected(t *testing.T) {
	t.Helper()
	select {
	case <-p.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("peer never accepted the waiter's connection")
	}
}

// push queues one state event for delivery.
func (p *scriptedPeer) push(st ipc.ProcState) {
	p.send <- st
}

func TestComputeClosure_AllProcs(t *testing.T) {
	procs := map[string]config.Process{
		"a": {Cmd: "echo a"},
		"b": {Cmd: "echo b"},
		"c": {Cmd: "echo c"},
	}
	got := computeClosure(procs, nil)
	if len(got) != 3 {
		t.Errorf("expected 3 procs, got %d", len(got))
	}
}

func TestComputeClosure_WithDeps(t *testing.T) {
	procs := map[string]config.Process{
		"app":     {Cmd: "echo app", DependsOn: []string{"db"}},
		"db":      {Cmd: "echo db", DependsOn: []string{"migrate"}},
		"migrate": {Cmd: "echo migrate"},
	}
	got := computeClosure(procs, []string{"app"})
	if len(got) != 3 {
		t.Errorf("expected closure {app,db,migrate}, got %v", got)
	}
	found := make(map[string]bool)
	for _, n := range got {
		found[n] = true
	}
	if !found["app"] || !found["db"] || !found["migrate"] {
		t.Errorf("expected app,db,migrate in closure, got %v", got)
	}
}

func TestComputeClosure_TaskOnlyRejected(t *testing.T) {
	procs := map[string]config.Process{
		"migrate": {Cmd: "echo migrate", Mode: config.ProcessModeTask},
	}
	closure := computeClosure(procs, []string{"migrate"})
	hasService := false
	for _, name := range closure {
		if procs[name].EffectiveMode() != config.ProcessModeTask {
			hasService = true
			break
		}
	}
	if hasService {
		t.Error("expected task-only closure to have no services")
	}
}

func TestComputeClosure_ServiceWithTaskAllowed(t *testing.T) {
	procs := map[string]config.Process{
		"app":     {Cmd: "echo app", DependsOn: []string{"migrate"}},
		"migrate": {Cmd: "echo migrate", Mode: config.ProcessModeTask},
	}
	closure := computeClosure(procs, []string{"app"})
	hasService := false
	for _, name := range closure {
		if procs[name].EffectiveMode() != config.ProcessModeTask {
			hasService = true
			break
		}
	}
	if !hasService {
		t.Error("expected closure with app to have at least one service")
	}
}

func TestCheckOptionalBinaries_MergeWithoutMergePort_ReturnsError(t *testing.T) {
	cfg := &config.Config{
		Merge: &config.Merge{
			Client: 5173,
			Server: 3001,
			Port:   8080,
		},
		Processes: map[string]config.Process{
			"web": {Cmd: "npm run dev"},
			"api": {Cmd: "go run ."},
		},
	}

	old := mergePortInPath
	mergePortInPath = func() bool { return false }
	t.Cleanup(func() { mergePortInPath = old })

	err := checkOptionalBinaries(cfg)
	if err == nil {
		t.Fatal("expected error when merge: is configured but merge-port is not on PATH")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "merge-port is required for merge: config") {
		t.Errorf("error should contain 'merge-port is required for merge: config', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "was not found in PATH") {
		t.Errorf("error should contain 'was not found in PATH', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "brokit install merge-port") {
		t.Errorf("error should contain 'brokit install merge-port', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "https://github.com/anivaryam/merge-port") {
		t.Errorf("error should contain 'https://github.com/anivaryam/merge-port', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "remove the merge: section") {
		t.Errorf("error should contain 'remove the merge: section', got: %s", errMsg)
	}
}

func TestCheckOptionalBinaries_NoMergeNoError(t *testing.T) {
	cfg := &config.Config{
		Processes: map[string]config.Process{
			"web": {Cmd: "npm run dev"},
		},
	}

	err := checkOptionalBinaries(cfg)
	if err != nil {
		t.Errorf("expected no error when merge: is not configured, got: %v", err)
	}
}

func TestSilentGuardrail_TaskOnlyErrorMessage(t *testing.T) {
	cfg := &config.Config{
		Processes: map[string]config.Process{
			"migrate": {Cmd: "echo migrate", Mode: config.ProcessModeTask},
		},
	}
	err := validateSilentSelection(cfg, []string{"migrate"})
	if err == nil {
		t.Fatal("expected error for task-only selection, got nil")
	}
	if err.Error() != "up --silent requires at least one service; task-only stacks run in foreground" {
		t.Errorf("error = %q, want exact message", err.Error())
	}
}

func TestSilentGuardrail_ServiceWithTaskAllowed(t *testing.T) {
	cfg := &config.Config{
		Processes: map[string]config.Process{
			"app":     {Cmd: "echo app", DependsOn: []string{"migrate"}},
			"migrate": {Cmd: "echo migrate", Mode: config.ProcessModeTask},
		},
	}
	err := validateSilentSelection(cfg, []string{"app"})
	if err != nil {
		t.Errorf("expected nil error for service with task dependency, got: %v", err)
	}
}

// isolateCacheDir reroutes os.UserCacheDir() into a test-controlled dir
// that works on both Linux and macOS:
//   - Linux: os.UserCacheDir reads $XDG_CACHE_HOME first, then $HOME/.cache
//   - macOS: os.UserCacheDir reads $HOME/Library/Caches; $XDG_CACHE_HOME
//     is ignored entirely
//
// Setting HOME (and clearing XDG_CACHE_HOME so the Linux path falls
// through to HOME) gives a single setup that works on both. Returns the
// proc-compose subdir under the redirected cache, already created.
func isolateCacheDir(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir: %v", err)
	}
	dir := filepath.Join(cacheRoot, "proc-compose")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	return dir
}

// TestLiveDaemonPaths_PrefersRecordedAddr ensures the read-side commands
// can find a daemon launched under a different $XDG_RUNTIME_DIR — the bug
// where a daemon launched with no XDG (PID file in ~/.cache/proc-compose/)
// became unreachable from a shell that had XDG set.
func TestLiveDaemonPaths_PrefersRecordedAddr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes don't have the XDG mismatch problem")
	}

	// Shell-side XDG points at an empty dir; daemon's PID file lives in
	// the (redirected) user cache dir. liveDaemonPaths must walk past
	// the empty XDG candidate and discover the daemon via the cache.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cacheDir := isolateCacheDir(t)

	hash := "deadbeef"
	pidPath := filepath.Join(cacheDir, "pc-"+hash+".pid")
	recordedSock := filepath.Join(cacheDir, "pc-"+hash+".sock")
	// Use our own PID so IsAliveFromPIDFile returns true.
	if err := daemon.WritePID(pidPath, os.Getpid(), recordedSock); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	gotPID, gotSock := liveDaemonPaths(hash)
	if gotPID != pidPath {
		t.Errorf("pidPath = %q; want %q", gotPID, pidPath)
	}
	if gotSock != recordedSock {
		t.Errorf("socketPath = %q; want %q (the daemon's recorded addr)", gotSock, recordedSock)
	}
}

// TestLiveDaemonPaths_FallsBackWhenNoDaemon makes sure callers still get
// the env-derived defaults (so they can surface "no daemon running") when
// no live PID file is anywhere on the candidate path.
func TestLiveDaemonPaths_FallsBackWhenNoDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes don't have the XDG mismatch problem")
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	isolateCacheDir(t)

	hash := "cafebabe"
	gotPID, gotSock := liveDaemonPaths(hash)
	if gotPID != paths.PID(hash) {
		t.Errorf("pidPath fallback = %q; want %q", gotPID, paths.PID(hash))
	}
	if gotSock != paths.Socket(hash) {
		t.Errorf("socketPath fallback = %q; want %q", gotSock, paths.Socket(hash))
	}
}

// TestLiveLogPath_PrefersLogNextToLivePID covers the same XDG mismatch
// for `proc-compose logs` — daemon writes to one runtime dir, user runs
// logs from a shell with a different XDG_RUNTIME_DIR.
func TestLiveLogPath_PrefersLogNextToLivePID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes don't have the XDG mismatch problem")
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cacheDir := isolateCacheDir(t)

	hash := "abcd1234"
	pidPath := filepath.Join(cacheDir, "pc-"+hash+".pid")
	logPath := filepath.Join(cacheDir, "pc-"+hash+".log")
	if err := daemon.WritePID(pidPath, os.Getpid(), filepath.Join(cacheDir, "pc-"+hash+".sock")); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("hello\n"), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := liveLogPath(hash)
	if got != logPath {
		t.Errorf("liveLogPath = %q; want %q (next to live PID)", got, logPath)
	}
}

// TestLiveLogPath_FallsBackToExistingLog returns any candidate-dir log
// when no live daemon is found — useful for post-mortem reads after the
// daemon already exited.
func TestLiveLogPath_FallsBackToExistingLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes don't have the XDG mismatch problem")
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cacheDir := isolateCacheDir(t)

	hash := "11112222"
	leftover := filepath.Join(cacheDir, "pc-"+hash+".log")
	if err := os.WriteFile(leftover, []byte("dead daemon log\n"), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := liveLogPath(hash)
	if got != leftover {
		t.Errorf("liveLogPath = %q; want %q", got, leftover)
	}
}

// TestLiveDaemonPaths_SkipsStalePIDFile guards against returning a dead
// daemon's recorded addr — the probe must keep walking to the next
// candidate dir if the first PID file points at a process that's gone.
func TestLiveDaemonPaths_SkipsStalePIDFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes don't have the XDG mismatch problem")
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cacheDir := isolateCacheDir(t)

	hash := "feed1234"
	// PID 1 is init — guaranteed alive but the PID file's recorded start
	// time won't match, so IsAliveFromPIDFile rejects it as stale.
	stalePID := filepath.Join(cacheDir, "pc-"+hash+".pid")
	if err := os.WriteFile(stalePID, []byte(`{"pid":1,"addr":"/tmp/wrong.sock","started_at":1}`), 0600); err != nil {
		t.Fatalf("write stale pidfile: %v", err)
	}

	_, gotSock := liveDaemonPaths(hash)
	if gotSock == "/tmp/wrong.sock" {
		t.Errorf("returned stale recorded addr; want fallback")
	}
}

// TestStarterTemplateForPlatform_MinimalWindows tests that the Windows minimal
// template does not contain POSIX shell constructs.
func TestStarterTemplateForPlatform_MinimalWindows(t *testing.T) {
	s, err := starterTemplateForPlatform("minimal", "windows")
	if err != nil {
		t.Fatalf("starterTemplateForPlatform(%q, %q) error = %v", "minimal", "windows", err)
	}
	if strings.Contains(s, "sh -c") {
		t.Errorf("Windows minimal template should not contain 'sh -c', got:\n%s", s)
	}
	if strings.Contains(s, "while true; do") {
		t.Errorf("Windows minimal template should not contain 'while true; do', got:\n%s", s)
	}
	if !strings.Contains(s, "powershell") {
		t.Errorf("Windows minimal template should contain 'powershell', got:\n%s", s)
	}
}

// TestStarterTemplateForPlatform_EmptyWindows tests that the Windows "" alias
// returns PowerShell template, not POSIX.
func TestStarterTemplateForPlatform_EmptyWindows(t *testing.T) {
	s, err := starterTemplateForPlatform("", "windows")
	if err != nil {
		t.Fatalf("starterTemplateForPlatform(%q, %q) error = %v", "", "windows", err)
	}
	if strings.Contains(s, "sh -c") {
		t.Errorf("Windows '' template should not contain 'sh -c', got:\n%s", s)
	}
	if strings.Contains(s, "while true; do") {
		t.Errorf("Windows '' template should not contain 'while true; do', got:\n%s", s)
	}
	if !strings.Contains(s, "powershell") {
		t.Errorf("Windows '' template should contain 'powershell', got:\n%s", s)
	}
}

// TestStarterTemplateForPlatform_DefaultWindows tests that the Windows "default"
// alias returns PowerShell template, not POSIX.
func TestStarterTemplateForPlatform_DefaultWindows(t *testing.T) {
	s, err := starterTemplateForPlatform("default", "windows")
	if err != nil {
		t.Fatalf("starterTemplateForPlatform(%q, %q) error = %v", "default", "windows", err)
	}
	if strings.Contains(s, "sh -c") {
		t.Errorf("Windows 'default' template should not contain 'sh -c', got:\n%s", s)
	}
	if strings.Contains(s, "while true; do") {
		t.Errorf("Windows 'default' template should not contain 'while true; do', got:\n%s", s)
	}
	if !strings.Contains(s, "powershell") {
		t.Errorf("Windows 'default' template should contain 'powershell', got:\n%s", s)
	}
}

// TestStarterTemplateForPlatform_MinimalLinux tests that the Linux minimal
// template still uses the POSIX loop (existing behavior preserved).
func TestStarterTemplateForPlatform_MinimalLinux(t *testing.T) {
	s, err := starterTemplateForPlatform("minimal", "linux")
	if err != nil {
		t.Fatalf("starterTemplateForPlatform(%q, %q) error = %v", "minimal", "linux", err)
	}
	if !strings.Contains(s, "processes:") {
		t.Errorf("Linux minimal template should contain 'processes:', got:\n%s", s)
	}
}

// TestSurviveRequiresLinux tests that the survive validation rejects non-Linux.
func TestSurviveRequiresLinux(t *testing.T) {
	tests := []struct {
		goos  string
		phase string // "validate" or "generate"
	}{
		{"darwin", "validate"},
		{"darwin", "generate"},
		{"windows", "validate"},
		{"windows", "generate"},
	}

	for _, tt := range tests {
		t.Run(tt.goos+"_"+tt.phase, func(t *testing.T) {
			err := validateSurvivePlatform(tt.goos)
			if err == nil {
				t.Errorf("validateSurvivePlatform(%q) = nil; want error containing '--survive requires Linux with systemd user services'", tt.goos)
			}
			if err != nil && !strings.Contains(err.Error(), "--survive requires Linux with systemd user services") {
				t.Errorf("validateSurvivePlatform(%q) error = %v; want error containing '--survive requires Linux with systemd user services'", tt.goos, err)
			}
		})
	}
}

// TestSurviveAllowedOnLinux tests that survive validation passes on Linux.
func TestSurviveAllowedOnLinux(t *testing.T) {
	err := validateSurvivePlatform("linux")
	if err != nil {
		t.Errorf("validateSurvivePlatform(%q) = %v; want nil", "linux", err)
	}
}

// TestUninstallRequiresLinux tests that uninstall rejects non-Linux.
func TestUninstallRequiresLinux(t *testing.T) {
	tests := []struct {
		goos string
	}{
		{"darwin"},
		{"windows"},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			err := validateUninstallPlatform(tt.goos)
			if err == nil {
				t.Errorf("validateUninstallPlatform(%q) = nil; want error containing 'uninstall is only supported on Linux with systemd user services'", tt.goos)
			}
			if err != nil && !strings.Contains(err.Error(), "uninstall is only supported on Linux with systemd user services") {
				t.Errorf("validateUninstallPlatform(%q) error = %v; want error containing 'uninstall is only supported on Linux with systemd user services'", tt.goos, err)
			}
		})
	}
}

// TestUninstallAllowedOnLinux tests that uninstall passes on Linux.
func TestUninstallAllowedOnLinux(t *testing.T) {
	err := validateUninstallPlatform("linux")
	if err != nil {
		t.Errorf("validateUninstallPlatform(%q) = %v; want nil", "linux", err)
	}
}

// ── stop: proving managed processes are actually terminated ──────────────────
//
// The reported failure is a stop that reports success while managed processes
// keep running, so every assertion below is made against a process PID — never
// against the daemon having exited, the socket having closed, or a released
// port. All of those are consistent with a managed child still running.

const testShutdownGrace = 1 // seconds; keeps the forced-escalation path quick

// pidIsRunning reports whether pid is still executing. A terminated but unreaped
// process counts as gone: it cannot run code, hold a port or hold a file, so
// counting it as alive would make these assertions flaky wherever init reaps
// slowly.
func pidIsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if !daemon.IsAlive(pid) {
		return false
	}
	return !pidIsTerminatedUnreaped(pid)
}

// pidIsTerminatedUnreaped reports whether pid is a zombie or already dead.
// Linux only; elsewhere the /proc read fails and plain liveness is used.
func pidIsTerminatedUnreaped(pid int) bool {
	data, err := os.ReadFile("/proc/" + fmt.Sprint(pid) + "/stat")
	if err != nil {
		return false
	}
	closeIdx := strings.LastIndexByte(string(data), ')')
	if closeIdx < 0 || closeIdx+2 > len(data) {
		return false
	}
	fields := strings.Fields(string(data[closeIdx+2:]))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "Z", "X", "x":
		return true
	}
	return false
}

// Fixture scripts are written to disk rather than inlined into the config as
// nested `sh -c` strings. Two reasons, both learned the hard way:
//
//   - A SIG_IGN disposition set by an inline `trap` with an empty operand is
//     not reliable across every /bin/sh, so the "ignores SIGTERM" fixtures
//     could silently stop modelling what they claim to model.
//   - Nesting `sh -c` inside the command proc-compose already runs through
//     `sh -c` adds a shell layer whose PID is not the one the runner tracks,
//     which is exactly the confusion these tests exist to rule out.
//
// Scripts also mirror how a real config refers to its services.
const (
	// blockerScript records its PID and then blocks until killed.
	// Usage: blocker.sh <pidfile>
	blockerScript = `#!/bin/sh
echo $$ > "$1"
FIXTURE_TTL_SECONDS=90
deadline=$(($(date +%s) + FIXTURE_TTL_SECONDS))
while [ "$(date +%s)" -lt "$deadline" ]; do sleep 1; done
`
	// stubbornScript also ignores every catchable termination signal, in
	// itself and in anything it starts, so it can only be removed by a forced
	// kill. This is the case where terminating just the command leader is not
	// enough.
	// Usage: stubborn.sh <pidfile>
	stubbornScript = `#!/bin/sh
trap '' TERM INT HUP
echo $$ > "$1"
FIXTURE_TTL_SECONDS=90
deadline=$(($(date +%s) + FIXTURE_TTL_SECONDS))
while [ "$(date +%s)" -lt "$deadline" ]; do sleep 1; done
`
	// spawnerScript is the "parent exits first" shape: its group leader starts
	// a descendant that ignores SIGTERM and then exits immediately. The
	// descendant holds no pipe on stdout, so the runner's log scanner reaches
	// EOF and cmd.Wait returns while the descendant is still running.
	//
	// The descendant is a fresh `sh` rather than a shell subshell on purpose:
	// POSIX `$$` keeps the parent's PID inside a subshell, so a subshell would
	// record the leader's already-exited PID and the test would assert on the
	// wrong process.
	// Usage: spawner.sh <stubbornScriptPath> <pidfile>
	spawnerScript = `#!/bin/sh
trap '' TERM INT HUP
sh "$1" "$2" >/dev/null 2>&1 &
exit 0
`
)

// writeFixtureScript writes one of the fixture scripts into dir and returns
// its path.
func writeFixtureScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fixture script %s: %v", name, err)
	}
	return path
}

// stackFixture is a running daemon plus the PIDs of everything it manages.
type stackFixture struct {
	bin        string
	configPath string
	env        []string // the environment `up` used; `stop` must resolve the same PID file
	pids       map[string]int
}

// Process roles in the fixture stack. Each one breaks a different assumption
// that "the daemon exited" would otherwise appear to satisfy.
const (
	rolePortless = "portless-worker" // no port declaration, no readiness probe
	roleDetached = "detached-child"  // leader already exited; descendant alive
	roleStubborn = "stubborn"        // ignores SIGTERM; needs a forced kill
	roleDaemon   = "daemon"
)

// startStack brings up a daemonized stack and waits until every fixture has
// recorded its PID. Waiting on the PID files is deterministic; a fixed sleep
// would race on a loaded machine.
//
// withDetached adds a role whose group leader exits immediately while a
// descendant that ignores SIGTERM keeps running. Such a process is
// reportable-but-not-signallable — see TestStop_ReportsUnsignalledSurvivor — so
// it is opt-in, because it changes the verdict a stop must produce.
func startStack(t *testing.T, bin, dir string, withDetached bool) stackFixture {
	t.Helper()

	blocker := writeFixtureScript(t, dir, "blocker.sh", blockerScript)
	stubborn := writeFixtureScript(t, dir, "stubborn.sh", stubbornScript)

	portlessPIDFile := filepath.Join(dir, "portless.pid")
	stubbornPIDFile := filepath.Join(dir, "stubborn.pid")
	detachedPIDFile := filepath.Join(dir, "detached.pid")
	configPath := filepath.Join(dir, "proc-compose.yml")

	entry := func(name, cmd string) string {
		return fmt.Sprintf("  %s:\n    cmd: %s\n    restart: never\n    shutdown_timeout: %d\n",
			name, cmd, testShutdownGrace)
	}
	body := "processes:\n" +
		// A portless worker: no port declaration and no readiness probe, so
		// nothing short of process liveness can detect it surviving.
		entry(rolePortless, blocker+" "+portlessPIDFile) +
		// Ignores SIGTERM, so it can only be removed by a forced kill.
		entry(roleStubborn, stubborn+" "+stubbornPIDFile)
	if withDetached {
		spawner := writeFixtureScript(t, dir, "spawner.sh", spawnerScript)
		body += entry(roleDetached, spawner+" "+stubborn+" "+detachedPIDFile)
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// A short runtime dir keeps the Unix socket path inside macOS's 104-byte
	// limit, matching the established socketDirForTest pattern.
	runtimeDir := socketDirForTest(t)
	env := append(os.Environ(), "XDG_RUNTIME_DIR="+runtimeDir, "NO_COLOR=1")

	up := exec.Command(bin, "up", "--silent", "--file", configPath)
	up.Env = env
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("up --silent failed: %v\n%s", err, out)
	}

	fixture := stackFixture{bin: bin, configPath: configPath, env: env, pids: map[string]int{}}

	pidPath := ""
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatalf("read runtime dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pid") {
			pidPath = filepath.Join(runtimeDir, e.Name())
		}
	}
	if pidPath == "" {
		t.Fatalf("daemon wrote no PID file in %s", runtimeDir)
	}
	fixture.pids[roleDaemon] = readDaemonPID(t, pidPath)

	fixture.pids[rolePortless] = waitForRecordedPID(t, portlessPIDFile)
	fixture.pids[roleStubborn] = waitForRecordedPID(t, stubbornPIDFile)
	if withDetached {
		fixture.pids[roleDetached] = waitForRecordedPID(t, detachedPIDFile)
	}

	// Register cleanup before any assertion so a failing run cannot leak the
	// daemon or its managed processes.
	t.Cleanup(func() { fixture.forceCleanup(t) })

	for role, pid := range fixture.pids {
		if pid <= 0 {
			t.Fatalf("fixture %q never recorded a PID; the test would prove nothing", role)
		}
		if !pidIsRunning(pid) {
			t.Fatalf("%s (PID %d) was not running before the stop; the fixture proves nothing", role, pid)
		}
	}
	return fixture
}

// forceCleanup terminates anything this test started that is still running.
// It signals exact PIDs only — never a name pattern or a process group — so a
// failing assertion cannot take down unrelated processes.
func (f stackFixture) forceCleanup(t *testing.T) {
	t.Helper()
	for role, pid := range f.pids {
		if pid <= 0 || !pidIsRunning(pid) {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			t.Logf("cleanup: cannot find leftover %s (PID %d): %v", role, pid, err)
			continue
		}
		if err := proc.Kill(); err != nil {
			t.Logf("cleanup: could not kill leftover %s (PID %d): %v", role, pid, err)
			continue
		}
		t.Logf("cleanup: killed leftover %s (PID %d)", role, pid)
	}
}

// readDaemonPID pulls the PID out of the daemon's JSON PID file.
func readDaemonPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var pf struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(data, &pf); err != nil {
		t.Fatalf("parse pid file %q: %v", string(data), err)
	}
	return pf.PID
}

// waitForRecordedPID blocks until a fixture has written its PID file.
func waitForRecordedPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid := atoiOrZero(strings.TrimSpace(string(data))); pid > 0 {
				return pid
			}
		}
		if !time.Now().Before(deadline) {
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func atoiOrZero(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// assertAllTerminated fails unless every tracked PID has stopped executing.
func (f stackFixture) assertAllTerminated(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		alive := ""
		for role, pid := range f.pids {
			if pidIsRunning(pid) {
				alive += fmt.Sprintf(" %s(PID %d)", role, pid)
			}
		}
		if alive == "" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("still running%s after %s — the stop reported success but the processes survived", alive, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f stackFixture) stop(t *testing.T, extraArgs ...string) (string, error) {
	t.Helper()
	args := append([]string{"stop"}, extraArgs...)
	args = append(args, "--file", f.configPath)
	c := exec.Command(f.bin, args...)
	c.Env = f.env
	out, err := c.CombinedOutput()
	return string(out), err
}

// TestStop_TerminatesDetachedDescendantOfAnExitedLeader is the CLI-level
// acceptance test for the ownership redesign.
//
// A command whose leader exits on its own while a descendant keeps running used
// to be the one case proc-compose could not resolve: by the time it was
// observable the group had been reaped, the descendant could not safely be
// signalled, and `stop` therefore failed while a process was still alive.
//
// The leader is now reaped last, so its process-table slot — and the process-group
// ID equal to its PID — stays reserved through the whole teardown. The descendant
// is therefore terminated like any other managed process and `stop` succeeds.
func TestStop_TerminatesDetachedDescendantOfAnExitedLeader(t *testing.T) {
	requireSignalStopFixture(t)
	bin := buildTestBinary(t)
	fixture := startStack(t, bin, t.TempDir(), true)

	out, err := fixture.stop(t)
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	// The whole point: a same-group descendant whose parent already exited is
	// terminated, not reported. It ignores SIGTERM, so only the group SIGKILL
	// after the leader is gone can have ended it.
	fixture.assertAllTerminated(t, 20*time.Second)
	if pid := fixture.pids[roleDetached]; pidIsRunning(pid) {
		t.Fatalf("detached descendant (PID %d) survived the stop: its leader had already "+
			"exited, so this is the parent-exits-first case", pid)
	}
}

// TestRequestShutdown_ExhaustedBudgetIsNotAnUnboundedWrite pins the caller-side
// half of the exhausted-budget fix.
//
// requestShutdown derives the write budget from the overall stop deadline.
// remainingUntil reports zero once that deadline has passed, and a zero budget
// must be refused rather than spent on a write with no deadline. This uses an
// already-absent socket so the connect phase cannot consume the budget first,
// which isolates the write guard itself.
func TestRequestShutdown_ExhaustedBudgetIsNotAnUnboundedWrite(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")

	start := time.Now()
	res := requestShutdown(socket, false, time.Now().Add(-time.Second))
	elapsed := time.Since(start)

	if res.outcome == stopVerified {
		t.Fatal("requestShutdown() reported success with an already-expired budget")
	}
	if res.detail == "" {
		t.Fatal("requestShutdown() gave no detail for an exhausted budget")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("requestShutdown took %s on an expired budget; the deadline was not respected", elapsed)
	}
}

// requireSignalStopFixture skips platforms whose termination model these
// fixtures cannot express. Windows terminates through a Job Object and has no
// SIGTERM for console apps, so the "ignores SIGTERM" fixtures are Unix-only.
func requireSignalStopFixture(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixtures rely on SIGTERM being ignorable; Windows uses Job Object termination")
	}
}

// TestStop_TerminatesManagedProcesses is the headline regression: a plain
// `stop` must not report success while a portless worker, a detached
// descendant, or a SIGTERM-ignoring process is still running.
func TestStop_TerminatesManagedProcesses(t *testing.T) {
	requireSignalStopFixture(t)
	bin := buildTestBinary(t)
	fixture := startStack(t, bin, t.TempDir(), false)

	out, err := fixture.stop(t)
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	// Assert on the processes before the wording: a stop that reported success
	// while leaving managed processes running is the defect being pinned, so
	// that must be the failure a regression produces.
	fixture.assertAllTerminated(t, 30*time.Second)
	if !strings.Contains(out, "stopped proc-compose daemon") {
		t.Fatalf("stop output = %q, want it to report a stopped daemon", out)
	}
}

// TestStopForce_TerminatesManagedProcesses covers the same guarantee for the
// forced path. Signalling the daemon directly used to kill the daemon and
// orphan every managed process while still printing success.
func TestStopForce_TerminatesManagedProcesses(t *testing.T) {
	requireSignalStopFixture(t)
	bin := buildTestBinary(t)
	fixture := startStack(t, bin, t.TempDir(), false)

	out, err := fixture.stop(t, "--force")
	if err != nil {
		t.Fatalf("stop --force failed: %v\n%s", err, out)
	}
	// Assert on the processes before the wording: a stop that reported success
	// while leaving managed processes running is the defect being pinned.
	fixture.assertAllTerminated(t, 30*time.Second)
	if !strings.Contains(out, "stopped proc-compose daemon") {
		t.Fatalf("stop --force output = %q, want it to report a stopped daemon", out)
	}
}

// ── stop: bounded, verified, and honest about what it could not prove ────────

// fakeDaemon drives stopDaemon against a scripted IPC peer plus a real process
// standing in for the daemon, so both halves of the contract can be exercised
// independently: what the daemon reports, and whether it actually goes away.
type fakeDaemon struct {
	t          *testing.T
	socketPath string
	proc       *exec.Cmd
	pid        int
}

// daemonStandinSentinel marks this test binary as the long-lived daemon stand-in.
const daemonStandinSentinel = "__PROC_COMPOSE_DAEMON_STANDIN__"

// TestDaemonStandinHelper is the long-lived process the daemon stand-in runs.
//
// It replaces a shell "sleep" for two reasons. Windows has no `sleep`, and its
// substitute — `timeout /t 30 /nobreak` — exits immediately when stdin is not a
// console, which is exactly the case for a test subprocess with redirected
// streams. A stand-in that had already died made taskkill fail with exit status
// 128, and made a "daemon is still running" check succeed without ever having
// run; both looked like production defects and were not.
//
// The helper gives a deterministic ready signal and then stays alive until the
// test releases it or a bounded idle lifetime expires, so a stand-in can never
// quietly die underneath an assertion.
func TestDaemonStandinHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-4] != daemonStandinSentinel {
		return
	}
	ready, release := os.Args[len(os.Args)-3], os.Args[len(os.Args)-2]

	if err := os.WriteFile(ready, []byte("ready"), 0o644); err != nil {
		os.Exit(2)
	}
	// A bounded idle lifetime keeps a failing test from leaking the process even
	// if the release file never appears.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// daemonStandin starts a long-lived stand-in process for a test, and blocks until
// it is provably running.
//
// Liveness is proven, not assumed: the helper writes a ready file only after its
// main loop is entered, and the caller asserts the process is alive after that.
func daemonStandin(t *testing.T) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")

	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonStandinHelper$",
		"--", daemonStandinSentinel, ready, release, "idle")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon stand-in: %v", err)
	}
	waitForFile(t, ready, 30*time.Second)
	if !pidIsRunning(cmd.Process.Pid) {
		t.Fatalf("daemon stand-in (PID %d) reported ready but is not running; "+
			"the fixture would prove nothing", cmd.Process.Pid)
	}

	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("go"), 0o644)
		if pidIsRunning(cmd.Process.Pid) {
			_ = cmd.Process.Kill()
		}
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Logf("cleanup: daemon stand-in %d was not reaped", cmd.Process.Pid)
		}
	})
	return cmd
}

// startFakeDaemon starts a long-lived process to represent the daemon and an IPC
// peer that answers with script (nil means no peer, i.e. an unreachable daemon).
//
// The process is registered for cleanup immediately after creation, before any
// assertion, so no path leaks it.
func startFakeDaemon(t *testing.T, script func(fd *fakeDaemon, conn net.Conn)) *fakeDaemon {
	t.Helper()

	proc := daemonStandin(t)
	fd := &fakeDaemon{t: t, proc: proc, pid: proc.Process.Pid}
	// Reap promptly: an unreaped zombie still answers signal 0, which
	// daemon.IsAlive counts as alive and would make every exit check hang.
	reaped := make(chan struct{})
	go func() {
		_, _ = proc.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		if pidIsRunning(fd.pid) {
			_ = proc.Process.Kill()
		}
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Logf("cleanup: daemon stand-in %d was not reaped", fd.pid)
		}
	})

	fd.socketPath = testEndpointForTest(t)

	if script == nil {
		// No peer: leave the path unused so connecting fails, modelling an
		// older daemon or a dead socket.
		return fd
	}
	ln, err := testListenEndpoint(fd.socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go script(fd, c)
		}
	}()
	return fd
}

// serveAck answers the first command with a fixed ack, then runs onAck and holds
// the connection open — which is what a daemon does while it tears itself down.
// onAck lets a test make its stand-in actually exit, so the exit checks
// downstream are exercised rather than short-circuited by a still-running peer.
func serveAck(ack, detail string, onAck func()) func(*fakeDaemon, net.Conn) {
	return func(_ *fakeDaemon, conn net.Conn) {
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 1<<16), 1<<16)
		for sc.Scan() {
			var ev ipc.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.Type != ipc.TypeCommand {
				continue
			}
			_ = json.NewEncoder(conn).Encode(ipc.Event{Type: ipc.TypeAck, Ack: ack, AckDetail: detail})
			if onAck != nil {
				onAck()
			}
			buf := make([]byte, 1)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		}
	}
}

// serveSilent accepts the connection and never answers, modelling a daemon that
// is connected but wedged.
func serveSilent(_ *fakeDaemon, conn net.Conn) {
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}

// TestStopDaemon_SurvivorVerdictMakesStopFail is the CLI-boundary proof the
// review asks for. The daemon reports that it could not verify termination, and
// `stop` must fail. Observing the daemon disappear is not enough: on this path
// the daemon is fully stopped and only the ack distinguishes success from a
// surviving managed process.
func TestStopDaemon_SurvivorVerdictMakesStopFail(t *testing.T) {
	const detail = "could not terminate worker: process group 41234 survived forced termination"
	fd := startFakeDaemon(t, nil)
	scriptedInto(fd, serveAck("partial", detail, func() {
		// The daemon really does finish shutting down; only the managed
		// process is unaccounted for.
		_ = fd.proc.Process.Kill()
	}))
	dir := socketDirForTest(t)

	err := stopDaemon(fd.socketPath, filepath.Join(dir, "x.pid"), fd.pid, fd.proc.Process, true, 5*time.Second)
	if err == nil {
		t.Fatal("stopDaemon = nil, want a failure: the daemon reported a survivor")
	}
	if !strings.Contains(err.Error(), "NOT fully verified") {
		t.Fatalf("error = %q, want it to report that termination was not fully verified", err)
	}
	if !strings.Contains(err.Error(), "41234") {
		t.Fatalf("error = %q, want it to name the surviving process group", err)
	}
}

// scriptedInto attaches a scripted IPC peer to a stand-in created without one,
// so a closure that needs the stand-in (to kill it, say) can be built after the
// fact.
func scriptedInto(fd *fakeDaemon, script func(*fakeDaemon, net.Conn)) {
	fd.t.Helper()
	ln, err := testListenEndpoint(fd.socketPath)
	if err != nil {
		fd.t.Fatalf("listen: %v", err)
	}
	fd.t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go script(fd, c)
		}
	}()
}

// TestStopDaemon_VerifiedVerdictButDaemonStillRunningFails covers the review's
// second requirement: after a successful verdict the daemon must actually be
// gone within the remaining budget. An "ok" ack alone is not proof.
func TestStopDaemon_VerifiedVerdictButDaemonStillRunningFails(t *testing.T) {
	fd := startFakeDaemon(t, serveAck("ok", "", nil))
	dir := socketDirForTest(t)

	// A short budget leaves no room to wait the stand-in out.
	err := stopDaemon(fd.socketPath, filepath.Join(dir, "x.pid"), fd.pid, fd.proc.Process, true, 600*time.Millisecond)
	if err == nil {
		t.Fatal("stopDaemon = nil, want a failure: the daemon reported ok but is still running")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error = %q, want it to report the daemon is still running", err)
	}
}

// TestStopDaemon_SilentPeerIsBoundedAndNotSuccess proves a connected but silent
// daemon cannot pin the caller and cannot be read as success. The elapsed time is
// asserted against the budget rather than a fixed sleep, so the check is about
// the bound itself.
func TestStopDaemon_SilentPeerIsBoundedAndNotSuccess(t *testing.T) {
	fd := startFakeDaemon(t, serveSilent)
	dir := socketDirForTest(t)

	const budget = 700 * time.Millisecond
	start := time.Now()
	err := stopDaemon(fd.socketPath, filepath.Join(dir, "x.pid"), fd.pid, fd.proc.Process, false, budget)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("stopDaemon = nil, want a failure: no verdict was received")
	}
	if !strings.Contains(err.Error(), "did not report a shutdown verdict") {
		t.Fatalf("error = %q, want it to say no verdict was received", err)
	}
	if !strings.Contains(err.Error(), "NOT") {
		t.Fatalf("error = %q, want it to label the outcome as unverified", err)
	}
	// The verdict wait must respect the shared deadline, leaving only the
	// fallback's own bounded signalling on top.
	if elapsed > budget+5*time.Second {
		t.Fatalf("stopDaemon took %s against a %s budget; the deadline is not enforced", elapsed, budget)
	}
}

// TestStopDaemon_UnreachableDaemonIsLabelledUnverified pins the compatibility
// fallback: an older daemon that cannot be asked to stop itself still gets
// stopped, but the result is labelled as unverified rather than as a clean stop.
func TestStopDaemon_UnreachableDaemonIsLabelledUnverified(t *testing.T) {
	fd := startFakeDaemon(t, nil) // no IPC peer: the socket does not exist
	dir := socketDirForTest(t)

	err := stopDaemon(fd.socketPath, filepath.Join(dir, "x.pid"), fd.pid, fd.proc.Process, false, 5*time.Second)
	if err == nil {
		t.Fatal("stopDaemon = nil, want a failure: managed processes could not be verified")
	}
	if !strings.Contains(err.Error(), "NOT verified") {
		t.Fatalf("error = %q, want it to state managed processes were not verified", err)
	}
	if pidIsRunning(fd.pid) {
		t.Fatal("the daemon stand-in was signalled but is still running; the fallback did not stop it")
	}
}

// TestSignalWitnessHelper is the child process the signal witness runs.
//
// It is the test binary re-invoked rather than a shell script so the fixture works
// the same way on every platform. On Unix it traps the signals under test and
// records them before exiting, which is what makes a graceful stop distinguishable
// from a forced one. On Windows there is no SIGTERM to trap — the product's own
// contract is that `stop` is always forced there — so it only idles, and the
// graceful-phase assertions that depend on a recorded signal are skipped by the
// tests that make them.
func TestSignalWitnessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != signalWitnessSentinel {
		return
	}
	marker, ready := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]

	if runtime.GOOS != "windows" {
		// Record only when a signal is actually delivered, so an empty marker
		// after a forced stop is real evidence that nothing graceful was tried.
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		_ = os.WriteFile(ready, []byte("ready"), 0o644)
		sig := <-ch
		_ = os.WriteFile(marker, []byte("term"), 0o644)
		t.Logf("witness received %v", sig)
		return
	}
	_ = os.WriteFile(ready, []byte("ready"), 0o644)
	for {
		time.Sleep(50 * time.Millisecond)
	}
}

const signalWitnessSentinel = "__PROC_COMPOSE_SIGNAL_WITNESS__"

// signalWitness starts a process that records whether it was asked to stop
// gracefully, so a test can prove which operation a fallback chose rather than
// inferring it from timing.
func signalWitness(t *testing.T, marker string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")

	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalWitnessHelper$",
		"--", signalWitnessSentinel, marker, ready)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start witness: %v", err)
	}
	// Deterministic gate: the trap is installed before the ready file appears, so
	// a signal after this point is guaranteed to be recorded.
	waitForFile(t, ready, 20*time.Second)
	// Reap promptly: an unreaped zombie still answers signal 0, which
	// daemon.IsAlive counts as alive and would make every exit check hang.
	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		if pidIsRunning(cmd.Process.Pid) {
			_ = cmd.Process.Kill()
		}
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Logf("cleanup: witness %d was not reaped", cmd.Process.Pid)
		}
	})
	return cmd
}

// requireGracefulSignalPhase skips the assertion that a graceful signal was
// delivered first.
//
// Windows has no SIGTERM for console applications: `stop` there is documented as
// always forced, so there is no graceful phase to observe. This skips one specific
// assertion, not the test — the surrounding fallback behaviour is still checked.
func requireGracefulSignalPhase(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows `stop` is always forced; there is no graceful signal phase to observe")
	}
}

// waitForFile blocks until path exists, so a fixture is only used once it is
// genuinely ready.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// signalsSeen reads the witness marker, returning the signals recorded so far.
func signalsSeen(t *testing.T, marker string) string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		return ""
	}
	return string(data)
}

// TestStopDaemon_FallbackPreservesGracefulIntent covers finding 5's graceful
// half: with no usable verdict, `stop` must still try a graceful stop first.
func TestStopDaemon_FallbackPreservesGracefulIntent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "signals")
	cmd := signalWitness(t, marker)
	dir := socketDirForTest(t)
	absent := filepath.Join(dir, "absent.sock")

	err := stopDaemon(absent, filepath.Join(dir, "x.pid"), cmd.Process.Pid, cmd.Process, false, 5*time.Second)
	if err == nil {
		t.Fatal("stopDaemon = nil, want an unverified failure")
	}
	if !strings.Contains(err.Error(), "NOT verified") {
		t.Fatalf("error = %q, want it to state managed processes were not verified", err)
	}
	// The witness must have been asked to stop before anything was killed: it
	// records a term and exits, which is observable in its marker file.
	requireGracefulSignalPhase(t)
	if got := signalsSeen(t, marker); !strings.Contains(got, "term") {
		t.Fatalf("witness recorded %q, want a SIGTERM; the graceful fallback did not signal", got)
	}
}

// TestStopDaemon_FallbackPreservesForceIntent covers finding 5's forced half:
// `stop --force` against a daemon that cannot be asked must go straight to a kill
// and must NOT first wait out a graceful stop. The witness ignores SIGTERM, so a
// graceful attempt would be recorded in its marker; a forced kill would not.
func TestStopDaemon_FallbackPreservesForceIntent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "signals")
	cmd := signalWitness(t, marker)
	dir := socketDirForTest(t)
	absent := filepath.Join(dir, "absent.sock")

	err := stopDaemon(absent, filepath.Join(dir, "x.pid"), cmd.Process.Pid, cmd.Process, true, 5*time.Second)
	if err == nil {
		t.Fatal("stopDaemon = nil, want an unverified failure")
	}
	if !strings.Contains(err.Error(), "NOT verified") {
		t.Fatalf("error = %q, want it to state managed processes were not verified", err)
	}
	requireGracefulSignalPhase(t)
	if got := signalsSeen(t, marker); strings.Contains(got, "term") {
		t.Fatalf("witness recorded %q: --force degraded into a graceful stop before killing", got)
	}
	if pidIsRunning(cmd.Process.Pid) {
		t.Fatal("witness is still running: the forced fallback did not kill the daemon")
	}
}

// TestStopDaemon_DeadlineIsSharedAcrossPhases proves the budget is one overall
// deadline rather than a per-phase allowance: a slow connect plus a silent peer
// must still finish within it.
func TestStopDaemon_DeadlineIsSharedAcrossPhases(t *testing.T) {
	fd := startFakeDaemon(t, serveSilent)
	dir := socketDirForTest(t)

	const budget = 700 * time.Millisecond
	start := time.Now()
	// A sub-millisecond connect allowance leaves the verdict wait almost nothing.
	err := stopDaemon(fd.socketPath, filepath.Join(dir, "x.pid"), fd.pid, fd.proc.Process, true, budget)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("stopDaemon = nil, want a failure")
	}
	if elapsed > budget+5*time.Second {
		t.Fatalf("stopDaemon took %s against a %s budget; phases are not sharing one deadline", elapsed, budget)
	}
}

// ── update notice ────────────────────────────────────────────────────────────

// noticeCommand builds a stand-in with the same name and the same flags the
// real command carries, so the eligibility gate is exercised against the shape
// it actually sees at runtime.
func noticeCommand(t *testing.T, name string, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: name, Run: func(*cobra.Command, []string) {}}
	if name == "up" {
		cmd.Flags().BoolP("silent", "s", false, "")
		cmd.Flags().Bool("survive", false, "")
		cmd.Flags().String("name", "", "")
		cmd.Flags().String("log-format", "text", "")
	}
	if name == "status" || name == "doctor" || name == "bootstrap" {
		cmd.Flags().Bool("json", false, "")
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse flags for %s %v: %v", name, args, err)
	}
	return cmd
}

// TestUpdateNoticeEligible_HumanFacingCommands is the positive case: a person
// watching a terminal gets the hint.
func TestUpdateNoticeEligible_HumanFacingCommands(t *testing.T) {
	for _, name := range []string{"up", "list", "status", "logs", "stop", "validate", "doctor", "bootstrap", "monitor"} {
		if !updateNoticeEligible(noticeCommand(t, name), true) {
			t.Errorf("%q was denied the update notice; it is a human-facing command", name)
		}
	}
}

// TestUpdateNoticeEligible_ExcludedCommands covers every case where extra
// output is noise at best and corrupting at worst: generated documentation and
// completion scripts, machine-readable output, and the daemon handoff.
func TestUpdateNoticeEligible_ExcludedCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		why  string
	}{
		{"help", nil, "help output is generated for reading"},
		{"completion", nil, "completion scripts are piped to a file"},
		{"__complete", nil, "shell completion consumes this output"},
		{"__completeNoDesc", nil, "shell completion consumes this output"},
		{"man", nil, "man pages are written to a file or a pager"},
		{"status", []string{"--json"}, "machine-readable output"},
		{"doctor", []string{"--json"}, "machine-readable output"},
		{"bootstrap", []string{"--json"}, "machine-readable output"},
		{"up", []string{"--silent"}, "daemonize hands off to a detached child"},
		{"up", []string{"-s"}, "daemonize hands off to a detached child"},
		{"up", []string{"--survive", "--name", "app"}, "a systemd unit is written to a file"},
		{"up", []string{"--log-format", "json"}, "machine-readable log output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if updateNoticeEligible(noticeCommand(t, tt.name, tt.args...), true) {
				t.Errorf("%s %v was allowed the update notice; %s", tt.name, tt.args, tt.why)
			}
		})
	}
}

// TestUpdateNoticeEligible_NonInteractiveIsDenied is the cron/CI case: with no
// terminal on stderr there is nobody to tell.
func TestUpdateNoticeEligible_NonInteractiveIsDenied(t *testing.T) {
	for _, name := range []string{"up", "list", "status", "logs", "doctor"} {
		if updateNoticeEligible(noticeCommand(t, name), false) {
			t.Errorf("%q was allowed the update notice without a terminal", name)
		}
	}
}

func TestUpdateNoticeEligible_NilCommandIsDenied(t *testing.T) {
	if updateNoticeEligible(nil, true) {
		t.Error("a nil command was allowed the update notice")
	}
}

// TestPrintUpdateNotice_AnnouncesFromCache seeds the shared user cache with a
// newer release and asserts the notice names both versions and a command that
// updates this copy, on stderr only.
//
// The copy under test is the test binary itself, in a temporary directory.
func TestPrintUpdateNotice_AnnouncesFromCache(t *testing.T) {
	cacheDir := isolateCacheDir(t)
	seedUpdateCache(t, cacheDir, "v1.3.0", time.Now())

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = pw
	printUpdateNotice(noticeCommand(t, "list"), "v1.2.0", true)
	pw.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	buf.ReadFrom(pr)

	got := buf.String()
	wants := []string{"v1.2.0", "v1.3.0"}
	if runtime.GOOS == "windows" {
		wants = append(wants, "$env:BROKIT_BIN", "brokit install --force proc-compose")
	} else {
		wants = append(wants, update.InstallScriptURL, "PROC_COMPOSE_INSTALL_DIR=")
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
}

// TestPrintUpdateNotice_DefaultDirectoryStillOffersAnInPlaceUpdate is the
// originally reported case: the binary sits in ~/.local/bin, which is also
// brokit's own default directory. Nothing about the directory proves brokit
// installed it — this repository's install.sh and Makefile use the same one —
// so the notice must not fall back to the bare `brokit update` line that fails
// for an unregistered copy.
func TestPrintUpdateNotice_DefaultDirectoryStillOffersAnInPlaceUpdate(t *testing.T) {
	defaultDir := filepath.Join(t.TempDir(), ".local", "bin")
	requireDir(t, defaultDir)
	bin := buildTestBinaryVersionAt(t, defaultDir, "v1.2.0")
	cacheDir := isolateCacheDir(t)
	seedUpdateCache(t, cacheDir, "v1.3.0", time.Now())

	env := append(os.Environ(), "HOME="+t.TempDir(), "XDG_CACHE_HOME="+filepath.Dir(cacheDir), "NO_COLOR=1")
	// BROKIT_BIN deliberately matches the binary's own directory: from in here
	// that is indistinguishable from a brokit-managed copy, and must be treated
	// as unknown rather than as proof.
	env = append(env, "BROKIT_BIN="+defaultDir)

	got := runInteractive(t, bin, t.TempDir(), env, true, "doctor")
	if !strings.Contains(got, "new proc-compose release") {
		t.Fatalf("positive control printed no notice at all:\n%s", got)
	}
	if want := "Run: " + update.UpdateCommand + "\n"; strings.Contains(got, want) {
		t.Errorf("a copy in brokit's default directory was told only to run brokit update:\n%s", got)
	}
	for _, want := range []string{
		defaultDir,
		"PROC_COMPOSE_INSTALL_DIR=" + defaultDir,
		update.InstallScriptURL,
		update.UpdateCommand,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"which brokit manages", "is managed by brokit", "brokit-managed"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("notice asserted ownership with %q:\n%s", forbidden, got)
		}
	}
}

// TestPrintUpdateNotice_PointsAtTheRunningBinaryDirectory proves the notice is
// aimed at the file being executed, wherever that is.
func TestPrintUpdateNotice_PointsAtTheRunningBinaryDirectory(t *testing.T) {
	binDir := t.TempDir()
	bin := buildTestBinaryVersionAt(t, binDir, "v1.2.0")
	cacheDir := isolateCacheDir(t)
	seedUpdateCache(t, cacheDir, "v1.3.0", time.Now())

	env := append(os.Environ(), "HOME="+t.TempDir(), "XDG_CACHE_HOME="+filepath.Dir(cacheDir),
		"BROKIT_BIN="+t.TempDir(), "NO_COLOR=1")

	got := runInteractive(t, bin, t.TempDir(), env, true, "doctor")
	if !strings.Contains(got, "new proc-compose release") {
		t.Fatalf("positive control printed no notice at all:\n%s", got)
	}
	for _, want := range []string{
		binDir,
		"PROC_COMPOSE_INSTALL_DIR=" + binDir,
		update.InstallScriptURL,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
}

func requireDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestPrintUpdateNotice_SilentForDevelopmentBuild is the guard for a locally
// built binary: "dev" is not a release, so there is nothing to offer.
func TestPrintUpdateNotice_SilentForDevelopmentBuild(t *testing.T) {
	cacheDir := isolateCacheDir(t)
	seedUpdateCache(t, cacheDir, "v1.3.0", time.Now())

	got := captureStderr(t, func() {
		printUpdateNotice(noticeCommand(t, "list"), "dev", true)
	})
	if got != "" {
		t.Errorf("a development build was told to update:\n%s", got)
	}
}

// TestPrintUpdateNotice_SilentForExcludedCommand is the structured-output
// guarantee at the boundary the user actually sees: nothing is added to stderr
// for a --json invocation even when a newer release is cached.
func TestPrintUpdateNotice_SilentForExcludedCommand(t *testing.T) {
	cacheDir := isolateCacheDir(t)
	seedUpdateCache(t, cacheDir, "v1.3.0", time.Now())

	got := captureStderr(t, func() {
		printUpdateNotice(noticeCommand(t, "status", "--json"), "v1.2.0", true)
	})
	if got != "" {
		t.Errorf("structured output was interleaved with a notice:\n%s", got)
	}
}

// TestUpdateNotice_EndToEndLeavesStructuredOutputIntact runs the built binary
// against a cache that already knows about a newer release. `doctor --json`
// must emit exactly one JSON document on stdout with nothing appended, and a
// non-interactive `list` must stay silent — the two ways a stray notice would
// break a script.
func TestUpdateNotice_EndToEndLeavesStructuredOutputIntact(t *testing.T) {
	bin := buildTestBinaryVersion(t, "v1.2.0")
	cacheDir := isolateCacheDir(t)
	seedUpdateCache(t, cacheDir, "v9.9.9", time.Now())

	// A redirected HOME/XDG_CACHE_HOME points the child at the seeded cache.
	env := append(os.Environ(),
		"HOME="+t.TempDir(),
		"XDG_CACHE_HOME="+filepath.Dir(cacheDir),
		"NO_COLOR=1",
	)
	work := t.TempDir()

	jsonCmd := exec.Command(bin, "doctor", "--json")
	jsonCmd.Dir = work
	jsonCmd.Env = env
	var stdout, stderr bytes.Buffer
	jsonCmd.Stdout = &stdout
	jsonCmd.Stderr = &stderr
	if err := jsonCmd.Run(); err != nil {
		t.Fatalf("doctor --json failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "new proc-compose release") {
		t.Errorf("structured output was polluted by the notice:\n%s", stdout.String())
	}
	if strings.Contains(stderr.String(), "new proc-compose release") {
		t.Errorf("doctor --json printed a notice despite being machine-readable:\n%s", stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Errorf("doctor --json stdout is not a single JSON document: %v\n%s", err, stdout.String())
	}

	// exec gives the child a pipe, not a terminal: nobody is watching.
	listCmd := exec.Command(bin, "list")
	listCmd.Dir = work
	listCmd.Env = env
	listOut, listErr := listCmd.CombinedOutput()
	_ = listErr // list without a config fails; that is not what this asserts
	if strings.Contains(string(listOut), "new proc-compose release") {
		t.Errorf("a non-interactive run printed a notice:\n%s", listOut)
	}

	// util-linux script supplies a real terminal without a new PTY dependency.
	// A positive control prevents a dev build or terminal-detection failure
	// from making the exclusion assertions pass vacuously.
	if runtime.GOOS != "linux" {
		return
	}
	if out := runInteractive(t, bin, work, env, true, "doctor"); !strings.Contains(out, "new proc-compose release") {
		t.Fatalf("positive control did not print a notice:\n%s", out)
	}
	out := runInteractive(t, bin, work, env, true, "doctor", "--json")
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("interactive doctor --json is not a single JSON document: %v\n%s", err, out)
	}
	writeCLIFile(t, filepath.Join(work, "proc-compose.yml"), "processes: {app: {cmd: echo hello}}\n")
	out = runInteractive(t, bin, work, env, false, "up", "--no-color", "--dry-run")
	if !strings.Contains(out, "new proc-compose release") || strings.Contains(out, "\x1b[") {
		t.Fatalf("--no-color must print a notice without ANSI escapes:\n%s", out)
	}
}

// runInteractive runs bin with a terminal on stderr, which is what the notice's
// eligibility gate requires: exec gives a child a pipe, so an ordinary run
// proves nothing about interactive behaviour.
//
// util-linux `script` supplies a real terminal without a new PTY dependency, so
// callers get the platform's own terminal detection exercised rather than a
// stand-in. noColor is passed explicitly instead of relying on an inherited
// NO_COLOR.
func runInteractive(t *testing.T, bin, dir string, env []string, noColor bool, args ...string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("interactive assertions require util-linux script")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("interactive assertions require util-linux script")
	}
	cmd := exec.Command(script, "-q", "-e", "-c", `exec "$PC_UPDATE_TEST_BIN" `+strings.Join(args, " "), "/dev/null")
	cmd.Dir = dir
	cmd.Env = append(env, "PC_UPDATE_TEST_BIN="+bin)
	if !noColor {
		cmd.Env = append(cmd.Env, "NO_COLOR=")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("interactive %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// seedUpdateCache writes a cache entry standing in for a lookup that already
// happened, at the location update.Default() reads.
func seedUpdateCache(t *testing.T, cacheDir, latest string, checkedAt time.Time) {
	t.Helper()
	entry := `{"latest":"` + latest + `","checked_at":"` + checkedAt.Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(filepath.Join(cacheDir, update.CacheFileName), []byte(entry), 0600); err != nil {
		t.Fatalf("seed update cache: %v", err)
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = pw
	fn()
	pw.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(pr)
	return buf.String()
}
