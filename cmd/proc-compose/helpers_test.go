package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

func ipcServerForTest(t *testing.T) (socketPath string, server *ipc.Server, stop func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc-test-*")
	if err != nil {
		t.Fatalf("temp socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath = filepath.Join(dir, "pc.sock")

	server = ipc.NewServer(socketPath)
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	stop = func() { server.Shutdown() }
	return
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
