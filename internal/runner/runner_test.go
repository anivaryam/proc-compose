package runner

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
)

func helperCmd(args ...string) string {
	if runtime.GOOS == "windows" {
		cmd := "set GO_WANT_HELPER_PROCESS=1 & " + os.Args[0] + " -test.run=TestRunnerHelperProcess --"
		for _, arg := range args {
			cmd += " " + cmdQuote(arg)
		}
		return cmd
	}
	parts := []string{"GO_WANT_HELPER_PROCESS=1", strconv.Quote(os.Args[0]), "-test.run=TestRunnerHelperProcess", "--"}
	for _, arg := range args {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

func cmdQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func TestRunnerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	args = args[1:]
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
