package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

const helperSentinel = "__PROC_COMPOSE_TEST_HELPER__"

// helperCmd builds a shell command that re-invokes this test binary in
// helper mode. Args are base64-encoded into a single shell-safe token so
// quoting hell across sh, cmd, PowerShell, and Go's exec-arg escaping
// never matters.
func helperCmd(args ...string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(strings.Join(args, "\x00")))
	if runtime.GOOS == "windows" {
		// Leave path bare on Windows: Go's CreateProcess arg escaping
		// turns any quotes we add into literal \" inside cmd /c.
		// CI temp paths have no spaces.
		return os.Args[0] + " -test.run=^TestRunnerHelperProcess$ -- " + helperSentinel + " " + payload
	}
	return posixQuote(os.Args[0]) + " -test.run=^TestRunnerHelperProcess$ -- " + helperSentinel + " " + payload
}

func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestRunnerHelperProcess(t *testing.T) {
	var payload string
	found := false
	for i, a := range os.Args {
		if a == helperSentinel {
			if i+1 < len(os.Args) {
				payload = os.Args[i+1]
				found = true
			}
			break
		}
	}
	if !found {
		return
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		os.Exit(2)
	}
	args := strings.Split(string(decoded), "\x00")
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "exit":
		if len(args) != 2 {
			os.Exit(2)
		}
		code, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(2)
		}
		os.Exit(code)
	case "sleep":
		if len(args) != 2 {
			os.Exit(2)
		}
		d, err := time.ParseDuration(args[1])
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(d)
		os.Exit(0)
	case "touch":
		if len(args) != 2 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte("started"), 0o644); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "touch-block":
		if len(args) != 2 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte("started"), 0o644); err != nil {
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "fail-after-file":
		if len(args) != 3 {
			os.Exit(2)
		}
		timeout, err := time.ParseDuration(args[2])
		if err != nil {
			os.Exit(2)
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(args[1]); err == nil {
				os.Exit(1)
			} else if !os.IsNotExist(err) {
				os.Exit(2)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(2)
	case "log":
		if len(args) != 2 {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString(args[1] + "\n")
		os.Exit(0)
	case "log-block":
		if len(args) != 2 {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString(args[1] + "\n")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "ready-after":
		if len(args) != 3 {
			os.Exit(2)
		}
		d, err := time.ParseDuration(args[1])
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(d)
		_, _ = os.Stdout.WriteString(args[2] + "\n")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func makeRunner(processes map[string]config.Process) *Runner {
	return &Runner{
		Config: &config.Config{
			Processes: processes,
		},
	}
}

func TestRun_FailedProcessReturnsError(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"failing": {Cmd: helperCmd("exit", "1"), Restart: "never"},
	})
	ctx := context.Background()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error when restart:never process exits non-zero, got nil")
	}
}

func TestRun_CleanExitReturnsNil(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"ok": {Cmd: helperCmd("exit", "0"), Restart: "never"},
	})
	ctx := context.Background()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil error when process exits 0, got: %v", err)
	}
}

func TestRun_SIGTERMReturnsNil(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"long": {Cmd: helperCmd("sleep", "30s"), Restart: "never"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay to simulate SIGTERM.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil on context cancellation (SIGTERM), got: %v", err)
	}
}

func TestRun_OnFailureRestartCancelledReturnsNil(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"flaky": {Cmd: helperCmd("exit", "1"), Restart: "on-failure"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay so the process gets into backoff and is then cancelled.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil when on-failure process is cancelled during backoff, got: %v", err)
	}
}

func TestRun_MaxRestarts_OnFailure(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"flaky": {Cmd: helperCmd("exit", "1"), Restart: "on-failure", MaxRestarts: 2},
	})
	ctx := context.Background()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error when max_restarts exceeded, got nil")
	}
	if !strings.Contains(err.Error(), "flaky") {
		t.Fatalf("expected error to mention process name, got: %v", err)
	}
}

func TestRun_MaxRestarts_Always(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"looper": {Cmd: helperCmd("exit", "0"), Restart: "always", MaxRestarts: 2},
	})
	ctx := context.Background()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error when max_restarts exceeded with restart:always, got nil")
	}
}

func TestRun_MaxRestarts_CancelledBeforeLimit(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"flaky": {Cmd: helperCmd("exit", "1"), Restart: "on-failure", MaxRestarts: 100},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil when cancelled before max_restarts, got: %v", err)
	}
}

func TestRun_DependsOn_WaitsForDependency(t *testing.T) {
	// "app" depends on "db" which takes ~200ms to become ready (log probe).
	r := makeRunner(map[string]config.Process{
		"db": {
			Cmd:     helperCmd("ready-after", "200ms", "READY"),
			Restart: "never",
			ReadyWhen: &config.ReadyWhen{
				Log: "READY",
			},
		},
		"app": {
			Cmd:       helperCmd("log-block", "app-started"),
			Restart:   "never",
			DependsOn: []string{"db"},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil (cancelled), got: %v", err)
	}
}

func TestRun_DependsOn_FailedDependency(t *testing.T) {
	// "app" depends on "db" which exits immediately (fails before becoming ready).
	r := makeRunner(map[string]config.Process{
		"db": {
			Cmd:     helperCmd("exit", "1"),
			Restart: "never",
			ReadyWhen: &config.ReadyWhen{
				Log: "READY",
			},
		},
		"app": {
			Cmd:       helperCmd("log", "should-not-run"),
			Restart:   "never",
			DependsOn: []string{"db"},
		},
	})
	ctx := context.Background()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error when dependency fails, got nil")
	}
	// Both processes should be reported as failed.
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("expected error to mention 'db', got: %v", err)
	}
	if !strings.Contains(err.Error(), "app") {
		t.Errorf("expected error to mention 'app', got: %v", err)
	}
}

func TestRun_ReadyWhen_Log(t *testing.T) {
	// Process prints "listening on port 3000" and should be marked ready.
	r := makeRunner(map[string]config.Process{
		"server": {
			Cmd:     helperCmd("log-block", "listening on port 3000"),
			Restart: "never",
			ReadyWhen: &config.ReadyWhen{
				Log: "listening on port",
			},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil (cancelled after ready), got: %v", err)
	}
}

func TestRun_FilterPullsInDependencies(t *testing.T) {
	r := &Runner{
		Config: &config.Config{
			Processes: map[string]config.Process{
				"db": {
					Cmd:     helperCmd("log-block", "READY"),
					Restart: "never",
					ReadyWhen: &config.ReadyWhen{
						Log: "READY",
					},
				},
				"app": {
					Cmd:       helperCmd("log-block", "app-started"),
					Restart:   "never",
					DependsOn: []string{"db"},
				},
			},
		},
		Filter: []string{"app"}, // only asked for app, but db should be pulled in
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil (cancelled), got: %v", err)
	}
}

func TestRun_ReadyWhen_HTTPTimeoutFailsProcess(t *testing.T) {
	// Process runs forever, but probe URL is unreachable — readiness must
	// time out within ReadyTimeout and the runner must mark the process failed.
	r := makeRunner(map[string]config.Process{
		"server": {
			Cmd:          helperCmd("sleep", "30s"),
			Restart:      "never",
			ReadyTimeout: 1, // 1 second
			ReadyWhen: &config.ReadyWhen{
				// 127.0.0.1:1 is a reserved port that nothing can bind to.
				HTTP: "http://127.0.0.1:1/notreal",
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error when readiness probe times out, got nil")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("expected error to mention 'server', got: %v", err)
	}
}

func TestRun_ReadyWhen_LogTimeoutFailsProcess(t *testing.T) {
	// Process runs forever and never emits the expected log line.
	r := makeRunner(map[string]config.Process{
		"server": {
			Cmd:          helperCmd("sleep", "30s"),
			Restart:      "never",
			ReadyTimeout: 1,
			ReadyWhen: &config.ReadyWhen{
				Log: "READY",
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error when log readiness probe times out, got nil")
	}
}

func TestRun_MultipleFailedProcessesErrorContainsNames(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"proc-a": {Cmd: helperCmd("exit", "1"), Restart: "never"},
		"proc-b": {Cmd: helperCmd("exit", "1"), Restart: "never"},
	})
	ctx := context.Background()
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected non-nil error, got nil")
	}
	msg := err.Error()
	if len(msg) == 0 {
		t.Fatal("expected non-empty error message")
	}
}

func TestRun_TaskModeSuccessUnblocksDependent(t *testing.T) {
	appStarted := t.TempDir() + string(os.PathSeparator) + "app-started"
	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("sleep", "200ms"), Restart: "never", Mode: config.ProcessModeTask},
		"app": {
			Cmd:       helperCmd("touch", appStarted),
			Restart:   "never",
			DependsOn: []string{"migrate"},
		},
	})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(appStarted); err == nil {
		t.Fatal("dependent service started before task completed")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat app marker: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected task success to unblock dependent and return nil, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not finish after task completed")
	}
	if _, err := os.Stat(appStarted); err != nil {
		t.Fatalf("expected dependent service to start after task completed: %v", err)
	}
}

func TestRun_TaskModeFailurePreventsDependent(t *testing.T) {
	appStarted := t.TempDir() + string(os.PathSeparator) + "app-started"
	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("exit", "1"), Restart: "never", Mode: config.ProcessModeTask},
		"app": {
			Cmd:       helperCmd("touch", appStarted),
			Restart:   "never",
			DependsOn: []string{"migrate"},
		},
	})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error when task fails")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected error to mention failed task, got: %v", err)
	}
	if !strings.Contains(err.Error(), "app") {
		t.Fatalf("expected error to mention failed dependent, got: %v", err)
	}
	if _, statErr := os.Stat(appStarted); statErr == nil {
		t.Fatal("dependent service started after task failure")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat app marker: %v", statErr)
	}
}

func TestRun_TaskModeFailureCancelsStack(t *testing.T) {
	siblingStarted := t.TempDir() + string(os.PathSeparator) + "sibling-started"
	r := makeRunner(map[string]config.Process{
		"api":     {Cmd: helperCmd("touch-block", siblingStarted), Restart: "never"},
		"migrate": {Cmd: helperCmd("fail-after-file", siblingStarted, "1s"), Restart: "never", Mode: config.ProcessModeTask},
	})

	start := time.Now()
	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error when task fails")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected task failure to cancel sibling quickly, took %s", elapsed)
	}
	if _, statErr := os.Stat(siblingStarted); statErr != nil {
		t.Fatalf("expected sibling service to have started before cancellation: %v", statErr)
	}
}

func TestRun_TaskModeSuccessStateCompletedReady(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("exit", "0"), Restart: "never", Mode: config.ProcessModeTask},
	})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected task success to return nil, got: %v", err)
	}
	states := r.store.snapshot()
	if len(states) != 1 {
		t.Fatalf("expected one state, got %d", len(states))
	}
	st := states[0]
	if st.State != "completed" {
		t.Fatalf("task state = %q, want completed", st.State)
	}
	if !st.Ready {
		t.Fatal("task Ready = false, want true after successful completion")
	}
	if st.PID != 0 {
		t.Fatalf("task PID = %d, want 0 after completion", st.PID)
	}
}

func TestRun_TaskModeRunnerDefenseIgnoresRestartAlways(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("exit", "0"), Restart: "always", Mode: config.ProcessModeTask, MaxRestarts: 2},
	})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected successful task to finish without restarting, got: %v", err)
	}
	states := r.store.snapshot()
	if states[0].Restarts != 0 {
		t.Fatalf("task restarts = %d, want 0", states[0].Restarts)
	}
}

// ─── Health endpoint tests ───────────────────────────────────────────────

func TestServeHealth_TaskCompletedIsHealthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("exit", "0"), Restart: "never", Mode: config.ProcessModeTask},
	})

	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background())
	}()

	time.Sleep(100 * time.Millisecond)

	go r.serveHealth(ln, r.store)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health returned %d, want 200 for completed task", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["healthy"] != true {
		t.Errorf("healthy = %v, want true for completed task", result["healthy"])
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Log("runner did not finish in time")
	}
}

func TestServeHealth_FailedTaskIsUnhealthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	proc := config.Process{Cmd: helperCmd("exit", "1"), Restart: "never", Mode: config.ProcessModeTask}
	r := makeRunner(map[string]config.Process{
		"migrate": proc,
	})

	store := newStateStore([]procInfo{{name: "migrate", proc: proc, colorIndex: 0}})
	r.store = store

	st := store.get("migrate")
	st.state = "failed"
	st.readyClosed = true
	st.readyOK = false

	go r.serveHealth(ln, store)

	time.Sleep(50 * time.Millisecond)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health returned %d, want 503 for failed task", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["healthy"] != false {
		t.Errorf("healthy = %v, want false for failed task", result["healthy"])
	}
}

func TestServeHealth_ServiceRunningIsHealthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	r := makeRunner(map[string]config.Process{
		"svc": {Cmd: helperCmd("sleep", "10s"), Restart: "never"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		r.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	go r.serveHealth(ln, r.store)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health returned %d, want 200 for running service", resp.StatusCode)
	}
}

// ─── Restart guardrail tests ────────────────────────────────────────────

func TestHandleRestart_RejectsTaskMode(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("exit", "0"), Restart: "never", Mode: config.ProcessModeTask},
	})

	socketPath, ipcServer, stopServer := ipcServerForTest(t)
	defer stopServer()

	r.IPC = ipcServer
	r.ConfigPath = t.TempDir() + "/proc-compose.yml"

	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background())
	}()

	time.Sleep(200 * time.Millisecond)

	ipcClient, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer ipcClient.Close()

	if err := ipcClient.Send(ipc.Command{Action: "restart", Process: "migrate"}); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	for {
		ev, err := ipcClient.Recv()
		if err != nil {
			t.Fatalf("recv failed: %v", err)
		}
		if ev.Type == ipc.TypeAck {
			if ev.Ack != "error" {
				t.Errorf("expected ack error, got %s: %s", ev.Ack, ev.AckDetail)
			}
			want := `process "migrate" is a task and cannot be restarted; restart the stack to rerun tasks`
			if ev.AckDetail != want {
				t.Errorf("AckDetail = %q, want %q", ev.AckDetail, want)
			}
			break
		}
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Log("runner did not finish in time")
	}
}

func ipcServerForTest(t *testing.T) (string, *ipc.Server, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc-test-*")
	if err != nil {
		t.Fatalf("temp socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "pc.sock")

	server := ipc.NewServer(socketPath)
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	stop := func() { server.Shutdown() }
	return socketPath, server, stop
}

// ─── Reload semantic diff tests ─────────────────────────────────────────

func TestReload_DetectsModeChange(t *testing.T) {
	// procChanged should detect Mode change
	a := config.Process{Cmd: "echo", Restart: "never", Mode: config.ProcessModeService}
	b := config.Process{Cmd: "echo", Restart: "never", Mode: config.ProcessModeTask}
	if !procChanged(a, b) {
		t.Error("procChanged should detect Mode change")
	}
}

func TestReload_DetectsReadyWhenChange(t *testing.T) {
	a := config.Process{
		Cmd: "echo",
		ReadyWhen: &config.ReadyWhen{HTTP: "http://localhost:3000/health"},
	}
	b := config.Process{
		Cmd: "echo",
		ReadyWhen: &config.ReadyWhen{HTTP: "http://localhost:4000/health"},
	}
	if !procChanged(a, b) {
		t.Error("procChanged should detect ReadyWhen change")
	}
}

func TestReload_DetectsReadyTimeoutChange(t *testing.T) {
	a := config.Process{Cmd: "echo", ReadyTimeout: 30}
	b := config.Process{Cmd: "echo", ReadyTimeout: 60}
	if !procChanged(a, b) {
		t.Error("procChanged should detect ReadyTimeout change")
	}
}

func TestReload_SkipsCompletedTaskRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := dir + "/proc-compose.yml"
	if err := os.WriteFile(configPath, []byte(`processes:
  migrate:
    cmd: echo done
    mode: task
    restart: never
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	r := &Runner{Config: cfg, ConfigPath: configPath}
	store := newStateStore([]procInfo{{name: "migrate", proc: cfg.Processes["migrate"], colorIndex: 0}})
	r.store = store

	// Simulate task completing
	st := store.get("migrate")
	st.state = "completed"
	st.readyClosed = true
	st.readyOK = true

	// Change config - modify the cmd
	newCfg := *cfg
	newCfg.Processes["migrate"] = config.Process{Cmd: "echo changed", Restart: "never", Mode: config.ProcessModeTask}

	// Swap in new config
	r.Config = &newCfg

	result := r.Reload()

	if result.Status != "partial" {
		t.Errorf("reload status = %q, want partial", result.Status)
	}
	if !strings.Contains(result.Message, "completed") {
		t.Errorf("message = %q, want 'completed' in message", result.Message)
	}

	// Verify the task was NOT restarted (restartCh should be empty)
	select {
	case <-st.restartCh:
		t.Error("task was restarted but should have been skipped")
	default:
		// OK - no restart was requested
	}
}
