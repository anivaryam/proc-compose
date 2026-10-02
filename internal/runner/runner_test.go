package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
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

// discardOutputScript wraps a command so that the shell running it does not keep
// the runner's log pipe open, while the command inside keeps running. The runner's
// log reader then reaches end-of-stream against a service that is still working,
// which is the shape these tests exist to pin.
//
// The redirect has to apply to the shell itself, not just to the command appended
// to it: the shell forks, so a redirect attached only to the child leaves the
// forked shell holding the pipe and EOF would not arrive until the command had
// already finished.
//
// It is also placed inside a script rather than inline in the command string.
// Windows shell selection routes any command containing a redirection operator to
// PowerShell, where a null redirect is not the same syntax at all; keeping the
// operators out of the configured command means the shell decision is made on the
// command alone, which is what the product does for real configurations.
func discardOutputScript(t *testing.T, command string) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		script := filepath.Join(dir, "quiet.cmd")
		// The redirection applies to the command the script runs, so the script
		// itself never hands the log pipe to anything it starts.
		body := "@echo off\r\n" + command + " 2>nul >nul\r\n"
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
			t.Fatalf("write quiet script: %v", err)
		}
		return script
	}
	script := filepath.Join(dir, "quiet.sh")
	// exec re-points the script's own streams before the command runs, so the
	// shell stops holding the log pipe. The command then runs in the foreground:
	// it is the thing that has to stay alive.
	body := "#!/bin/sh\nexec >/dev/null 2>&1\n" + command + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write quiet script: %v", err)
	}
	return script
}

func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// encodeHelperArgs builds the payload token helperCmd uses, for the rare case a
// fixture has to re-invoke the helper binary itself rather than name it in a
// command string.
func encodeHelperArgs(args ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(args, "\x00")))
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
	case "ready-gate":
		// ready-gate <line> <marker> <gate>: first incarnation prints the
		// readiness line and exits (triggering a restart); every later
		// incarnation waits for <gate> to appear before printing it, so a
		// test can hold a restarted process unready until it chooses.
		if len(args) != 4 {
			os.Exit(2)
		}
		if _, err := os.Stat(args[2]); err != nil {
			if err := os.WriteFile(args[2], []byte("first"), 0o644); err != nil {
				os.Exit(2)
			}
			_, _ = os.Stdout.WriteString(args[1] + "\n")
			os.Exit(0)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(args[3]); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(2)
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = os.Stdout.WriteString(args[1] + "\n")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "quiet-loop":
		// quiet-loop <pidfile> <ready> <release>: record the PID, close both
		// output streams so the runner's log pipe reaches EOF immediately, then
		// signal on a non-output channel that work is under way and exit only
		// when released.
		//
		// This is the deterministic way to model a service whose output ends
		// before it does. A shell script cannot be trusted here: the leader's
		// process-group and pipe behaviour vary with the /bin/sh, and getting it
		// wrong makes the fixture silently test nothing.
		if len(args) != 4 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(2)
		}
		// Close the streams the runner reads. From here on the pipe has no
		// writer, so the runner sees EOF while this process keeps running.
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		if err := os.WriteFile(args[2], []byte("working"), 0o644); err != nil {
			os.Exit(2)
		}
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(args[3]); err == nil {
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
		os.Exit(7)
	case "quiet-stubborn":
		// quiet-stubborn <pidfile>: record the PID, close both output streams so
		// the runner's log pipe reaches EOF immediately, then ignore SIGTERM and
		// keep working. Only a group-wide SIGKILL can end it.
		//
		// This is the combination that broke the old design: end-of-stream made
		// the runner withdraw signalling authority while the service was still
		// running, so the escalation could no longer reach it.
		if len(args) != 2 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(2)
		}
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		signal.Ignore(syscall.SIGTERM)
		signal.Ignore(syscall.SIGINT)
		signal.Ignore(syscall.SIGHUP)
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		os.Exit(7)
	case "spawn-job-child":
		// spawn-job-child <pidfile>: start a detached long-lived child, record the
		// CHILD's pid, then exit immediately.
		//
		// The child stays inside the command's Job Object, so terminating the job
		// has to reach it even though the leader that started it has already been
		// collected. That is the property Windows has and Unix does not.
		//
		// The recorded pid must be the child's, not this process's. This process is
		// the job leader and it exits at once, so a test that read its own pid would
		// assert against an already-dead parent and prove nothing about whether the
		// job reached a surviving descendant.
		if len(args) != 2 {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestRunnerHelperProcess$",
			"--", helperSentinel, encodeHelperArgs("sleep", "120s"))
		// Detach from this process's streams so the runner's log reader cannot
		// be kept alive by a descendant that was never meant to be waited on.
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		child.Stdin, child.Stdout, child.Stderr = devnull, devnull, devnull
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		// Record only after a successful start, so the file never names a pid that
		// was never created.
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
			os.Exit(2)
		}
		// The child is deliberately not waited on: this process exits at once and
		// the child is left running inside the job.
		os.Exit(0)
	case "record-and-exit":
		// record-and-exit <pidfile>: record this PID and exit 0 immediately.
		// A shutdown that arrives afterwards must treat it as a non-event
		// rather than an error, and must not signal anything on its behalf.
		if len(args) != 2 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

// Fixture scripts are written to disk rather than inlined into the config.
// Every fixture self-expires after a fixed lifetime, so no test path — including
// one where the code under test is deliberately broken and never returns — can
// leak a helper. The lifetime is far longer than any legitimate assertion
// window (the slowest shutdown test resolves in seconds), so it never affects a
// passing test; it only bounds the damage of a failing one. It relies on
// date(1), which is why these fixtures are Unix-only anyway.
//
// A Go program cannot model a SIGTERM-ignoring service: the Go runtime installs
// its own SIGTERM/SIGINT handlers at startup, overriding a SIG_IGN disposition
// inherited across exec. A `trap` in a wrapper shell therefore does not make a
// test-binary descendant stubborn — it still dies on the first SIGTERM, which
// would make a shutdown test pass for the wrong reason. Real shell scripts
// ignore SIGTERM reliably, so the fixtures use them.
//
// Scripts also mirror how a config refers to a service that is a script, and
// they avoid nesting `sh -c` inside the command proc-compose already runs
// through `sh -c`.
const (
	// stubbornScript records its PID and then blocks, ignoring every catchable
	// termination signal. Usage: stubborn.sh <pidfile>
	stubbornScript = `#!/bin/sh
trap '' TERM INT HUP
echo $$ > "$1"
FIXTURE_TTL_SECONDS=90
deadline=$(($(date +%s) + FIXTURE_TTL_SECONDS))
while [ "$(date +%s)" -lt "$deadline" ]; do sleep 1; done
`
	// stayScript is the "leader and descendant both ignore SIGTERM" shape:
	// neither the group leader nor its descendant can be stopped politely, so
	// the whole group needs a forced kill.
	// Usage: stay.sh <stubbornScriptPath> <pidfile>
	stayScript = `#!/bin/sh
trap '' TERM INT HUP
sh "$1" "$2" >/dev/null 2>&1 &
trap '' TERM INT HUP
FIXTURE_TTL_SECONDS=90
deadline=$(($(date +%s) + FIXTURE_TTL_SECONDS))
while [ "$(date +%s)" -lt "$deadline" ]; do sleep 1; done
`
	// incarnationScript models one run of a restarting command. The first
	// incarnation backgrounds a SIGTERM-ignoring descendant and exits; every
	// later incarnation blocks. A state file is what distinguishes them, so a
	// test can tell the replacement process apart from the original
	// incarnation's surviving descendant.
	//
	// The descendant's output is detached so the runner's log scanner reaches
	// EOF when the leader exits — otherwise the descendant would hold the pipe
	// open and the test would be exercising the escaped-pipe path instead of the
	// restart path.
	// Usage: incarnation.sh <leaderPidFile> <stateFile> <stubbornScript> <descendantPidFile>
	incarnationScript = `#!/bin/sh
echo $$ > "$1"
if [ -e "$2" ]; then
  FIXTURE_TTL_SECONDS=90
  deadline=$(($(date +%s) + FIXTURE_TTL_SECONDS))
  while [ "$(date +%s)" -lt "$deadline" ]; do sleep 1; done
  exit 0
fi
: > "$2"
sh "$3" "$4" >/dev/null 2>&1 &
# Wait for the descendant to record its pid before exiting. A natural exit now
# initiates teardown immediately, so exiting as soon as the fork returned could
# have the whole group killed before the descendant started, and the restart test
# would then wait on a pidfile nothing was going to write.
i=0
while [ "$i" -lt 400 ]; do
  [ -s "$4" ] && break
  sleep 0.05
  i=$((i + 1))
done
exit 0
`

	// escapeScript starts a descendant in a brand-new session, so it leaves the
	// process group entirely and can no longer be reached by any group-wide
	// signal. It deliberately keeps the inherited stdout: that is what makes the
	// runner have to bound its log read and report the escape as unverified.
	//
	// The leader waits for the descendant to record its PID before exiting. That
	// is not incidental: a descendant is only out of the group once setsid has
	// run, and between fork and setsid it is still a member, so a leader that
	// exited sooner would let the runner's teardown legitimately catch and kill
	// it. Waiting makes the escape provably complete first, so the test measures
	// a descendant that really escaped containment rather than one that was
	// merely caught mid-escape.
	// Usage: escape.sh <stubbornScriptPath> <pidfile>
	escapeScript = `#!/bin/sh
setsid sh "$1" "$2" &
i=0
while [ "$i" -lt 400 ]; do
  [ -s "$2" ] && break
  sleep 0.05
  i=$((i + 1))
done
exit 0
`

	// spawnScript is the "parent exits first" shape: the group leader starts a
	// descendant that ignores SIGTERM and then exits immediately. The
	// descendant holds no pipe on stdout, so the runner's log scanner reaches
	// EOF and cmd.Wait returns while the descendant is still running — which is
	// exactly when a leader-only shutdown stops caring.
	//
	// The descendant is a fresh `sh` rather than a shell subshell on purpose:
	// POSIX `$$` keeps the parent's PID inside a subshell, so a subshell would
	// record the leader's already-exited PID and the test would assert on the
	// wrong process.
	//
	// The leader waits for the descendant to record its pid before exiting. A
	// natural exit now initiates teardown immediately, so a leader that exited as
	// soon as it forked could have its whole group killed before the descendant
	// had even started — and the test would then fail waiting for a pidfile that
	// nothing was ever going to write. Waiting makes the survivor provably
	// established before the leader goes, which is the situation these tests claim
	// to be exercising.
	// Usage: spawn.sh <stubbornScriptPath> <pidfile>
	spawnScript = `#!/bin/sh
trap '' TERM INT HUP
sh "$1" "$2" >/dev/null 2>&1 &
i=0
while [ "$i" -lt 400 ]; do
  [ -s "$2" ] && break
  sleep 0.05
  i=$((i + 1))
done
exit 0
`
)

// ignoresTermination reports whether this platform can model a
// SIGTERM-ignoring process. Windows has no SIGTERM for console apps started in
// a new process group, so the signal-shaped shutdown tests are Unix-only;
// Windows termination is Job Object based and covered by the shared
// survivors/State contract in processgroup.go.
func ignoresTermination() bool { return runtime.GOOS != "windows" }

// writeScript writes a fixture script into dir and returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fixture script %s: %v", name, err)
	}
	return path
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

// TestRun_ReadyServiceThenExitsIsNotReady is the lifecycle-level regression for
// stale readiness: a service that passed its log probe and then exited must
// end up exited and not ready, so --wait-ready can't be satisfied by a process
// that is already gone.
func TestRun_ReadyServiceThenExitsIsNotReady(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"server": {
			Cmd:     helperCmd("log", "listening on port 3000"),
			Restart: "never",
			ReadyWhen: &config.ReadyWhen{
				Log: "listening on port",
			},
		},
	})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected nil after clean exit, got: %v", err)
	}

	states := r.store.Load().snapshot()
	if len(states) != 1 {
		t.Fatalf("expected one state, got %d", len(states))
	}
	if states[0].State != "exited" {
		t.Fatalf("service state = %q, want exited", states[0].State)
	}
	if states[0].Ready {
		t.Fatal("exited service Ready = true, want false")
	}
}

// TestRun_ReadyServiceThenFailsIsNotReady covers the failure variant: a process
// that was marked ready on start and then died with a non-zero status must end
// up failed and not ready — an earlier ready=true must not survive the failure.
func TestRun_ReadyServiceThenFailsIsNotReady(t *testing.T) {
	// No ready_when probe: readiness latches on start, then the command exits 1.
	r := makeRunner(map[string]config.Process{
		"server": {Cmd: helperCmd("exit", "1"), Restart: "never"},
	})

	if err := r.Run(context.Background()); err == nil {
		t.Fatal("expected non-nil error when the process exits non-zero")
	}

	states := r.store.Load().snapshot()
	if len(states) != 1 {
		t.Fatalf("expected one state, got %d", len(states))
	}
	if states[0].State != "failed" {
		t.Fatalf("service state = %q, want failed", states[0].State)
	}
	if states[0].Ready {
		t.Fatal("failed service Ready = true, want false")
	}
}

// TestRun_RestartResetsReadiness checks that an automatic restart starts a new
// readiness cycle: the dead incarnation's latch must not carry over into the
// replacement. It watches the *running* second incarnation before its probe is
// released, so removing the resetReady() calls would make it see Ready=true
// where it requires false.
func TestRun_RestartResetsReadiness(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "first-incarnation")
	gate := filepath.Join(dir, "probe-released")

	r := makeRunner(map[string]config.Process{
		"flappy": {
			Cmd:          helperCmd("ready-gate", "READY", marker, gate),
			Restart:      "always",
			ReadyTimeout: -1, // no probe deadline; the test controls readiness
			ReadyWhen:    &config.ReadyWhen{Log: "READY"},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Second incarnation is running, but its probe is gated. If readiness
	// had carried over from the first incarnation this would read true.
	if !waitForProcState(t, r, "flappy", func(st ipc.ProcState) bool {
		return st.State == "running" && st.Restarts >= 1
	}, 15*time.Second) {
		t.Fatalf("second incarnation never reached running with a restart recorded: %+v", snapshotOf(r, "flappy"))
	}
	if got := snapshotOf(r, "flappy"); got.Ready {
		t.Fatalf("restarted service Ready = true before its probe ran, want false (state %q)", got.State)
	}

	// Release the probe and the same process must become ready on its own.
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	if !waitForProcState(t, r, "flappy", func(st ipc.ProcState) bool {
		return st.State == "running" && st.Ready
	}, 10*time.Second) {
		t.Fatalf("restarted service never became ready after its probe ran: %+v", snapshotOf(r, "flappy"))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runner did not stop after cancel")
	}
}

// snapshotOf returns the current state for name, or a zero value if the store
// is not published yet or the name is absent.
func snapshotOf(r *Runner, name string) ipc.ProcState {
	store := r.store.Load()
	if store == nil {
		return ipc.ProcState{Name: name}
	}
	for _, st := range store.snapshot() {
		if st.Name == name {
			return st
		}
	}
	return ipc.ProcState{Name: name}
}

// waitForProcState polls the runner's state store until pred holds for name,
// returning false if it does not within timeout. Polling the store keeps the
// assertion independent of broadcast timing.
func waitForProcState(t *testing.T, r *Runner, name string, pred func(ipc.ProcState) bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if pred(snapshotOf(r, name)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
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
	case <-time.After(10 * time.Second):
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
	states := r.store.Load().snapshot()
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
	states := r.store.Load().snapshot()
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

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not finish in time")
	}

	go r.serveHealth(ln, r.store.Load())

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
	r.store.Store(store)

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

	go r.serveHealth(ln, r.store.Load())

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
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
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
	case <-time.After(10 * time.Second):
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
	r.store.Store(store)

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

// ── process-tree shutdown ───────────────────────────────────────────────────
//
// Every test below asserts on the managed *process*, never on the runner having
// returned, a state field having been cleared, or a port having been released.
// All of those are consistent with a managed child still running.

const shutdownGraceForTest = 1 // seconds; keeps the escalation path quick

// pidIsRunning reports whether pid is still executing. A terminated but
// unreaped process counts as gone: it cannot run code, hold a port or hold a
// file, so treating it as alive would make these assertions flaky wherever init
// reaps slowly.
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
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
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

// trackProcessForCleanup guarantees the fixture is gone even when an assertion
// fails. It signals only this exact PID — never a name pattern or a whole
// process group — so a failing test cannot take down unrelated processes.
//
// The kill is attempted unconditionally rather than behind a liveness check:
// while a failing test is unwinding, the fixture can be in any state between
// "running" and "reaped", and a check that misreads it would leak the process.
func trackProcessForCleanup(t *testing.T, pid int, what string) {
	t.Helper()
	if pid <= 0 {
		return
	}
	t.Cleanup(func() {
		proc, err := os.FindProcess(pid)
		if err != nil {
			t.Logf("cleanup: cannot find %s (PID %d): %v", what, pid, err)
			return
		}
		if err := proc.Kill(); err != nil {
			// Already gone: the assertion under test terminated it, which is the
			// expected outcome and not worth reporting as a cleanup problem.
			if !errors.Is(err, os.ErrProcessDone) {
				t.Logf("cleanup: could not kill %s (PID %d): %v", what, pid, err)
			}
			return
		}
		t.Logf("cleanup: killed leftover %s (PID %d)", what, pid)
	})
}

// waitForPIDFile blocks until the fixture has written its PID file, then
// returns the recorded PID. Polling a file the fixture writes is deterministic
// synchronisation: a fixed sleep would race on a loaded machine.
func waitForPIDFile(t *testing.T, path string, timeout time.Duration) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid, true
			}
		}
		if !time.Now().Before(deadline) {
			return 0, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertTerminated fails unless pid has stopped executing within timeout.
func assertTerminated(t *testing.T, pid int, what string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for pidIsRunning(pid) {
		if !time.Now().Before(deadline) {
			t.Fatalf("%s (PID %d) is still running %s after shutdown", what, pid, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRun_ShutdownKillsGroupWhenDescendantIgnoresSIGTERM covers the escalation
// path: neither the group leader nor its descendant can be stopped politely, so
// the shutdown_timeout escalation has to reach the whole group. exec.Cmd's
// WaitDelay only ever kills the leader, which is the bug this pins.
func TestRun_ShutdownKillsGroupWhenDescendantIgnoresSIGTERM(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("Windows has no SIGTERM for console apps; termination is Job Object based")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)

	r := makeRunner(map[string]config.Process{
		"api": {
			Cmd:             stay + " " + stubborn + " " + pidFile,
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	descendant, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("descendant never recorded its PID")
	}
	trackProcessForCleanup(t, descendant, "descendant")

	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("api never reported a leader PID")
	}
	leader := snapshotOf(r, "api").PID
	trackProcessForCleanup(t, leader, "group leader")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown reported failure: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}

	assertTerminated(t, descendant, "descendant that ignored SIGTERM", 15*time.Second)
	assertTerminated(t, leader, "group leader that ignored SIGTERM", 15*time.Second)
}

// TestRun_RestartAccountsForPreviousIncarnation covers the restart contract: a
// restart must not forget the previous incarnation's group, whose descendants can
// still be running. Asserting only on the new incarnation would pass while the
// original's child leaked.
func TestRun_RestartAccountsForPreviousIncarnation(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("Windows has no SIGTERM for console apps; termination is Job Object based")
	}
	dir := t.TempDir()
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	incarnation := writeScript(t, dir, "incarnation.sh", incarnationScript)

	leaderFile := filepath.Join(dir, "leader.pid")
	state := filepath.Join(dir, "restarted")
	descendant := filepath.Join(dir, "descendant.pid")

	r := makeRunner(map[string]config.Process{
		"worker": {
			// First incarnation backgrounds a stubborn descendant and exits; the
			// replacement then blocks, so the two can be told apart.
			Cmd:             incarnation + " " + leaderFile + " " + state + " " + stubborn + " " + descendant,
			Restart:         "always",
			ShutdownTimeout: shutdownGraceForTest,
		},
		"keeper": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	firstLeader, ok := waitForPIDFile(t, leaderFile, 20*time.Second)
	if !ok {
		t.Fatal("first incarnation never recorded its leader PID")
	}
	trackProcessForCleanup(t, firstLeader, "first incarnation leader")

	original, ok := waitForPIDFile(t, descendant, 20*time.Second)
	if !ok {
		t.Fatal("first incarnation never recorded its descendant PID")
	}
	trackProcessForCleanup(t, original, "first incarnation descendant")

	// Wait for the replacement incarnation to be running, so the restart path is
	// definitely exercised rather than merely possible. Each incarnation rewrites
	// the same leader file, so a changed PID is the signal that a new one started.
	var replacement int
	deadline := time.Now().Add(20 * time.Second)
	for {
		replacement, _ = waitForPIDFile(t, leaderFile, time.Second)
		if replacement != 0 && replacement != firstLeader && pidIsRunning(replacement) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("replacement incarnation never started (restarts=%d, leader=%d, first=%d)",
				snapshotOf(r, "worker").Restarts, replacement, firstLeader)
		}
		time.Sleep(50 * time.Millisecond)
	}
	trackProcessForCleanup(t, replacement, "replacement incarnation leader")
	if replacement == original {
		t.Fatalf("replacement leader (%d) collided with the original descendant PID", replacement)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}

	// The first incarnation's descendant ignored SIGTERM and its leader had
	// already exited, so only the group SIGKILL proves the restart did not lose
	// it. This is the acceptance condition that reporting a survivor used to
	// satisfy instead of terminating it.
	assertTerminated(t, original, "first incarnation's descendant across a restart", 15*time.Second)
	if verdict := r.ShutdownResult(); verdict != nil {
		t.Fatalf("restart and stop reported %v although the first incarnation's descendant "+
			"(PID %d) was terminated", verdict, original)
	}
}

// TestRun_ShutdownReportsNoSurvivorWhenProcessesExitCleanly guards the opposite
// direction: a clean stack must not be reported as a failed termination.
func TestRun_ShutdownReportsNoSurvivorWhenProcessesExitCleanly(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	if !waitForProcState(t, r, "svc", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("svc never started")
	}
	pid := snapshotOf(r, "svc").PID
	trackProcessForCleanup(t, pid, "svc")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown reported failure: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}
	assertTerminated(t, pid, "svc", 15*time.Second)
}

// TestRun_ShutdownWithAlreadyExitedProcess pins already-exited handling: a
// process that finished before the shutdown arrives is a non-event. It must not
// turn the stop into a failure, and it must not cause anything to be signalled
// on its behalf.
func TestRun_ShutdownWithAlreadyExitedProcess(t *testing.T) {
	dir := t.TempDir()
	gonePIDFile := filepath.Join(dir, "gone.pid")

	r := makeRunner(map[string]config.Process{
		"gone": {
			Cmd:     helperCmd("record-and-exit", gonePIDFile),
			Restart: "never",
		},
		"keeper": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	gonePID, ok := waitForPIDFile(t, gonePIDFile, 20*time.Second)
	if !ok {
		t.Fatal("process never recorded its PID")
	}
	if !waitForProcState(t, r, "gone", func(s ipc.ProcState) bool { return s.State == "exited" }, 20*time.Second) {
		t.Fatal("gone never reported itself exited")
	}
	if !waitForProcState(t, r, "keeper", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("keeper never started")
	}
	keeperPID := snapshotOf(r, "keeper").PID
	trackProcessForCleanup(t, keeperPID, "keeper")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown of an already-exited process reported failure: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}
	assertTerminated(t, keeperPID, "keeper", 15*time.Second)
	t.Logf("already-exited PID %d handled without error or stray signals", gonePID)
}

// TestProcessGroup_ClosedGroupIsNeverSignalled pins the identity discipline for
// the zero-handle cases. On Unix a zero or negative PID is not "no group", it
// means "my own process group" or "every process" to kill(2), so a handle that
// was never claimed — or has been closed — must refuse to signal rather than
// fall back to a stale identifier.
func TestProcessGroup_ClosedGroupIsNeverSignalled(t *testing.T) {
	// What a closed group reports differs by platform, and both answers are
	// honest. Unix still holds a numeric identifier it can consult and finds
	// nothing; Windows has released its only handle, so nothing can be queried
	// and it says so rather than claiming a verification it cannot make.
	closedGroupState := GroupEmpty
	if runtime.GOOS == "windows" {
		closedGroupState = GroupUnavailable
	}

	t.Run("never tracked", func(t *testing.T) {
		pg := newProcessGroup()
		if got := pg.State(); got != GroupEmpty {
			t.Fatalf("State() on an untracked group = %v, want %v", got, GroupEmpty)
		}
		if err := pg.Terminate(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("Terminate() on an untracked group = %v, want os.ErrProcessDone", err)
		}
		if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("Kill() on an untracked group = %v, want os.ErrProcessDone", err)
		}
		// Nothing to report: with no identifier there is nothing that could
		// still be running under our management.
		if err := survivorsError(groupResult("untracked", pg, GroupEmpty)); err != nil {
			t.Fatalf("survivorsError() on an untracked group = %v, want nil", err)
		}
	})

	t.Run("after close", func(t *testing.T) {
		// A command that exits immediately: by the time the group is closed,
		// the group it identified is genuinely gone. CommandContext is required
		// because Setup assigns cmd.Cancel.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunnerHelperProcess$")
		cmd.Args = strings.Split(helperCmd("exit", "0"), " ")
		pg := newProcessGroup()
		pg.Setup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := pg.Track(cmd); err != nil {
			t.Fatalf("track: %v", err)
		}
		_ = cmd.Wait()

		if err := pg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Close is idempotent so a double teardown cannot fail a stop.
		if err := pg.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}

		if got := pg.State(); got != closedGroupState {
			t.Fatalf("State() after Close = %v, want %v", got, closedGroupState)
		}
		if err := pg.Terminate(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("Terminate() after Close = %v, want os.ErrProcessDone", err)
		}
		if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("Kill() after Close = %v, want os.ErrProcessDone", err)
		}
		// What survivorsError makes of a closed group differs too, and both are
		// honest. Unix has no identifier left, so there is nothing to report.
		// Windows released its only handle, so it cannot verify anything and says
		// so — which is the point of the design: never claim a verification that
		// cannot be made.
		switch closedGroupState {
		case GroupUnavailable:
			err := survivorsError(groupResult("closed", pg, closedGroupState))
			if err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("survivorsError() after Close = %v, want an unverifiable error", err)
			}
		default:
			if err := survivorsError(groupResult("closed", pg, closedGroupState)); err != nil {
				t.Fatalf("survivorsError() after Close = %v, want nil", err)
			}
		}
	})
}

// TestSurvivorsError_UnavailableContainmentIsNotSuccess covers the Windows-shaped
// failure on any platform: when no containment handle exists, nothing can be
// verified, and that must never read as "everything terminated".
func TestSurvivorsError_UnavailableContainmentIsNotSuccess(t *testing.T) {
	pg := &uncontainedProcessGroup{}

	if got := pg.State(); got != GroupUnavailable {
		t.Fatalf("State() = %v, want %v", got, GroupUnavailable)
	}
	err := survivorsError(groupResult("wedged", pg, pg.State()))
	if err == nil {
		t.Fatal("survivorsError() with no containment = nil, want an unverifiable error")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %q, want it to say containment was unavailable", err)
	}
}

type uncontainedProcessGroup struct{}

func (g *uncontainedProcessGroup) Setup(_ *exec.Cmd)       {}
func (g *uncontainedProcessGroup) Track(_ *exec.Cmd) error { return nil }
func (g *uncontainedProcessGroup) Terminate() error        { return os.ErrProcessDone }
func (g *uncontainedProcessGroup) Kill() error             { return os.ErrProcessDone }
func (g *uncontainedProcessGroup) State() GroupState       { return GroupUnavailable }
func (g *uncontainedProcessGroup) Identify() string        { return "unavailable" }
func (g *uncontainedProcessGroup) Withdraw()               {}
func (g *uncontainedProcessGroup) Close() error            { return nil }

// startTrackedGroup starts cmdStr in its own process group and returns the
// concrete group plus the Cmd, so a test can drive the group's own mutex. The
// Cmd's process is registered for cleanup immediately after creation, before any
// assertion, so no path leaks it.
func startTrackedGroup(t *testing.T, cmdStr string) (ProcessGroup, *exec.Cmd) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	pg := newProcessGroup()
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	if err := pg.Track(cmd); err != nil {
		cancel()
		t.Fatalf("track: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			// Reap it. A zombie leader keeps its process-group ID allocated,
			// which both leaks the fixture and makes the group probe keep
			// succeeding for a group nothing is running in.
			_, _ = cmd.Process.Wait()
		}
		cancel()
		_ = pg.Close()
	})
	return pg, cmd
}

// startGroupWithSurvivor starts cmdStr in its own process group and returns the
// group, the Cmd, and the descendant's PID. It blocks until the descendant has
// recorded its PID, so the caller never observes the group mid-fork.
//
// The Cmd's process is registered for cleanup immediately after creation, before
// the wait, so no path leaks it.
func startGroupWithSurvivor(t *testing.T, cmdStr, pidFile string) (ProcessGroup, *exec.Cmd, int) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	pg := newProcessGroup()
	pg.Setup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	if err := pg.Track(cmd); err != nil {
		cancel()
		t.Fatalf("track: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		cancel()
		_ = pg.Close()
	})

	descendant, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("descendant never recorded its PID; the fixture proves nothing")
	}
	return pg, cmd, descendant
}

// TestShutdown_ForceKillsManagedProcessGroupWithoutGrace proves the forced path
// skips the graceful wait and still verifies termination: the group is killed up
// front, but it stays registered so the final sweep can confirm the kill.
func TestShutdown_ForceKillsManagedProcessGroupWithoutGrace(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("Windows has no SIGTERM for console apps; termination is Job Object based")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	stay := writeScript(t, dir, "stay.sh", stayScript)

	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd: stay + " " + stubborn + " " + pidFile,
			// Long enough that only the forced kill can end this in time.
			ShutdownTimeout: 60,
			Restart:         "never",
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	descendant, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("descendant never recorded its PID")
	}
	trackProcessForCleanup(t, descendant, "descendant")
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("worker never reported a leader PID")
	}
	leader := snapshotOf(r, "worker").PID
	trackProcessForCleanup(t, leader, "group leader")

	stopped, _, err := r.Shutdown(true)
	if err != nil {
		t.Fatalf("Shutdown(force): %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(40 * time.Second):
		t.Fatal("forced shutdown did not complete")
	}

	if err := r.ShutdownResult(); err != nil {
		t.Fatalf("forced shutdown reported survivors: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run reported failure after a forced shutdown: %v", err)
	}

	assertTerminated(t, descendant, "descendant", 15*time.Second)
	assertTerminated(t, leader, "group leader", 15*time.Second)
}

// publishSweepVerdict stands in for the tail of Runner.Run: on cancellation it
// sweeps the still-tracked groups and then publishes the verdict, which is the
// ordering the IPC shutdown handler depends on. It lets the survivor-reporting
// tests drive the real code path without starting real processes. The order
// matters and matches endRun: verdict first, then the completion signal.
func publishSweepVerdict(r *Runner, runCtx context.Context) {
	<-runCtx.Done()
	r.sweepGroups(runCtx)
	verdict := r.ShutdownResult()

	r.stopMu.Lock()
	stopped := r.stopped
	stopVerdict := r.stopVerdict
	r.stopped = nil
	r.stopVerdict = nil
	r.stopMu.Unlock()

	if stopVerdict != nil {
		select {
		case stopVerdict <- verdict:
		default:
		}
	}
	if stopped != nil {
		close(stopped)
	}
}

// unstoppableProcessGroup always reports itself as alive, modelling a managed
// process that survives both the graceful signal and the forced kill.
type unstoppableProcessGroup struct{}

func (g *unstoppableProcessGroup) Setup(_ *exec.Cmd)       {}
func (g *unstoppableProcessGroup) Track(_ *exec.Cmd) error { return nil }
func (g *unstoppableProcessGroup) Terminate() error        { return nil }
func (g *unstoppableProcessGroup) Kill() error             { return nil }
func (g *unstoppableProcessGroup) State() GroupState       { return GroupOwned }
func (g *unstoppableProcessGroup) Identify() string        { return "41234" }
func (g *unstoppableProcessGroup) Withdraw()               {}
func (g *unstoppableProcessGroup) Close() error            { return nil }

// TestShutdown_ReportsSurvivorItCannotTerminate covers the reporting
// requirement from the daemon's side: a managed process that provably cannot be
// killed must be named in the verdict rather than silently reported as a clean
// stop.
func TestShutdown_ReportsSurvivorItCannotTerminate(t *testing.T) {
	r := &Runner{}
	r.trackGroup("wedged", &unstoppableProcessGroup{}, shutdownGraceForTest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.beginRun(ctx, cancel)
	go publishSweepVerdict(r, ctx)

	stopped, _, err := r.Shutdown(false)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	<-stopped

	if got := r.ShutdownResult(); got == nil {
		t.Fatal("ShutdownResult() = nil, want an error naming the survivor")
	} else if !strings.Contains(got.Error(), "wedged") {
		t.Fatalf("ShutdownResult() = %q, want it to name the surviving process", got)
	}
}

// TestHandleShutdown_ReportsPartialWithSurvivors proves the "stopped, but not
// everything died" verdict is distinct from both ok and error, so a caller
// cannot mistake it for a clean stop.
func TestHandleShutdown_ReportsPartialWithSurvivors(t *testing.T) {
	r := &Runner{}
	r.trackGroup("wedged", &unstoppableProcessGroup{}, shutdownGraceForTest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.beginRun(ctx, cancel)
	go publishSweepVerdict(r, ctx)

	res := r.handleShutdown(ipc.Command{Action: "shutdown"})
	if res.Status != "partial" {
		t.Fatalf("handleShutdown status = %q (%s), want partial", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "wedged") {
		t.Fatalf("handleShutdown message = %q, want it to name the survivor", res.Message)
	}
}

// TestHandleShutdown_ReportsOKAfterCleanStop drives the IPC-facing handler that
// `proc-compose stop` and `stop --force` both depend on.
func TestHandleShutdown_ReportsOKAfterCleanStop(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	if !waitForProcState(t, r, "svc", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("svc never started")
	}
	pid := snapshotOf(r, "svc").PID
	trackProcessForCleanup(t, pid, "svc")

	res := r.handleShutdown(ipc.Command{Action: "shutdown"})
	if res.Status != "ok" {
		t.Fatalf("handleShutdown status = %q (%s), want ok", res.Status, res.Message)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run reported failure: %v", err)
	}
	assertTerminated(t, pid, "svc", 15*time.Second)
}

// TestHandleShutdown_RejectsWhenNothingIsRunning keeps a stale `stop` from
// reporting a clean shutdown for a stack that is not there.
func TestHandleShutdown_RejectsWhenNothingIsRunning(t *testing.T) {
	r := &Runner{}
	res := r.handleShutdown(ipc.Command{Action: "shutdown"})
	if res.Status != "error" {
		t.Fatalf("handleShutdown status = %q, want error", res.Status)
	}
	if !strings.Contains(res.Message, "no stack is running") {
		t.Fatalf("handleShutdown message = %q, want 'no stack is running'", res.Message)
	}
}

// TestRun_EscapedDescendantIsReportedAsUnverified covers the review's setsid
// case. A descendant that calls setsid leaves the process group, so no group-wide
// signal can reach it. Closing the inherited log pipe lets the runner finish, but
// that is not evidence the descendant is gone — so the shutdown must be reported
// as unverified rather than clean.
//
// Without this, the group is legitimately empty, the sweep finds nothing, and the
// stop would report success over a process that is still running.
func TestRun_EscapedDescendantIsReportedAsUnverified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid is a Unix facility")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not available on this host")
	}
	if !ignoresTermination() {
		t.Skip("Windows has no SIGTERM for console apps; termination is Job Object based")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escapee.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	escape := writeScript(t, dir, "escape.sh", escapeScript)

	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:             escape + " " + stubborn + " " + pidFile,
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
		"keeper": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	escapee, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("escaped descendant never recorded its PID")
	}
	// Registered immediately, before any assertion, so a failure cannot leak it.
	trackProcessForCleanup(t, escapee, "escaped descendant")

	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}

	verdict := r.ShutdownResult()
	if verdict == nil {
		t.Fatalf("shutdown reported success while descendant (PID %d) had escaped the process group", escapee)
	}
	// The message must distinguish "left the group, could not be verified" from
	// "survived inside the group", because the two need different responses.
	if !strings.Contains(verdict.Error(), "left the group") &&
		!strings.Contains(verdict.Error(), "could not be verified") {
		t.Fatalf("verdict = %q, want it to report that the descendant left the group", verdict)
	}
	if !pidIsRunning(escapee) {
		t.Logf("escaped descendant %d also terminated; verdict was still reported as unverified", escapee)
	}
}

// TestShutdown_VerdictIsDeliveredBeforeRunReturns covers the review's ack-flush
// barrier. It pauses the point at which the verdict would reach the client and
// asserts that Run cannot finish first: without the barrier, main would tear down
// the IPC server and exit while the caller was still waiting to be told what
// happened.
func TestShutdown_VerdictIsDeliveredBeforeRunReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix sockets for the IPC server")
	}

	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	socketPath, server, stopServer := ipcServerForTest(t)
	defer stopServer()
	r.IPC = server

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	if !waitForProcState(t, r, "svc", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("svc never started")
	}
	pid := snapshotOf(r, "svc").PID
	trackProcessForCleanup(t, pid, "svc")

	client, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.Send(ipc.Command{Action: "shutdown", Force: true}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Consume events until the ack arrives; this is what a real `stop` does.
	var ack ipc.Event
	for {
		ev, recvErr := client.Recv()
		if recvErr != nil {
			t.Fatalf("recv: %v", recvErr)
		}
		if ev.Type == ipc.TypeAck {
			ack = ev
			break
		}
	}

	// The verdict must be in the ack, not merely inferred from Run returning.
	if ack.Ack == "ok" {
		if msg := ack.AckDetail; msg != "" {
			t.Fatalf("ack ok carries a complaint: %q", msg)
		}
	}

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not exit after a verified shutdown")
	}
}

// TestAckBarrier_SealStopsLateHolders proves the barrier cannot be entered after
// it is sealed. This is what removes the WaitGroup misuse: a WaitGroup's Wait may
// return before a concurrent Add, so a late shutdown command could have held up
// — or raced past — an already-finishing run.
func TestAckBarrier_SealStopsLateHolders(t *testing.T) {
	var b ackBarrier

	if !b.enter() {
		t.Fatal("enter() on an open barrier = false, want true")
	}
	// A holder is present, so sealing must wait for it.
	sealed := make(chan struct{})
	go func() {
		b.sealAndWait(2 * time.Second)
		close(sealed)
	}()

	// Once sealed, no new holder may enter.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !b.enter() {
			break
		}
		b.leave()
		if time.Now().After(deadline) {
			t.Fatal("barrier never sealed while a holder was present")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Releasing the last holder must satisfy the seal.
	b.leave()
	select {
	case <-sealed:
	case <-time.After(3 * time.Second):
		t.Fatal("sealAndWait did not return after the last holder left")
	}

	// And it must be idempotent.
	b.sealAndWait(time.Millisecond)
}

// TestEndRun_CannotOvertakeAnInFlightVerdict is the deterministic half of the
// ack-flush proof. A real client cannot be made to pause mid-write, so the
// barrier is exercised directly: while a holder is still inside its flush window,
// endRun must not complete. If it did, main would tear the IPC server down while
// the caller was still waiting to be told what happened — the exact ordering bug
// the review describes.
func TestEndRun_CannotOvertakeAnInFlightVerdict(t *testing.T) {
	r := &Runner{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.beginRun(ctx, cancel)

	if !r.stopAck.enter() {
		t.Fatal("could not enter the ack barrier")
	}

	endDone := make(chan struct{})
	go func() {
		r.endRun()
		close(endDone)
	}()

	// The holder is still "delivering"; teardown must block on it.
	select {
	case <-endDone:
		t.Fatal("endRun completed while a shutdown verdict was still undelivered")
	case <-time.After(250 * time.Millisecond):
	}

	// Once the holder reports delivery, teardown proceeds.
	r.stopAck.leave()
	select {
	case <-endDone:
	case <-time.After(10 * time.Second):
		t.Fatal("endRun did not complete after the verdict was delivered")
	}
}

// TestEndRun_PublishesVerdictBeforeSignallingCompletion pins the ordering the
// shutdown handler depends on: the verdict must be available on the channel
// before the completion signal fires, otherwise a waiting handler would deadlock
// or fall back to "no verdict".
func TestEndRun_PublishesVerdictBeforeSignallingCompletion(t *testing.T) {
	r := &Runner{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.beginRun(ctx, cancel)

	stopped, verdictCh, err := r.Shutdown(false)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Shutdown cancelled the run context, so endRun runs on its own.
	endDone := make(chan struct{})
	go func() {
		r.endRun()
		close(endDone)
	}()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never signalled completion")
	}

	// Reading the verdict must not block: it is published with the completion
	// signal, never after it.
	select {
	case verdict := <-verdictCh:
		if verdict != nil {
			t.Fatalf("clean stack published verdict %v, want nil", verdict)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verdict was not available when completion was signalled")
	}

	select {
	case <-endDone:
	case <-time.After(10 * time.Second):
		t.Fatal("endRun did not complete")
	}
}

// TestShutdown_IPCPathReleasesTheAckBarrier pins barrier ownership on the path
// that actually holds it. Only handleCommands can see both the reply and the
// flush, so it — not handleShutdown — must acquire and release the barrier. If
// the release is left to the wrong frame, Run's exit stalls for the whole flush
// timeout after every stop.
//
// The command therefore has to travel through a real IPC server: a direct
// handleShutdown call cannot reach the defect. The bound is on elapsed time,
// which is what distinguishes a stall from a prompt exit; a fixed sleep would
// not.
func TestShutdown_IPCPathReleasesTheAckBarrier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix sockets for the IPC server")
	}

	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})
	socketPath, server, stopServer := ipcServerForTest(t)
	defer stopServer()
	r.IPC = server

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	if !waitForProcState(t, r, "svc", func(s ipc.ProcState) bool { return s.PID > 0 }, 20*time.Second) {
		t.Fatal("svc never started")
	}
	pid := snapshotOf(r, "svc").PID
	trackProcessForCleanup(t, pid, "svc")

	client, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.Send(ipc.Command{Action: "shutdown", Force: true}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for {
		ev, recvErr := client.Recv()
		if recvErr != nil {
			t.Fatalf("recv: %v", recvErr)
		}
		if ev.Type == ipc.TypeAck {
			if ev.Ack != "ok" {
				t.Fatalf("ack = %q (%s), want ok", ev.Ack, ev.AckDetail)
			}
			break
		}
	}

	start := time.Now()
	select {
	case <-runDone:
	case <-time.After(shutdownFlushTimeout + 5*time.Second):
		t.Fatal("Run did not exit after a shutdown delivered over IPC")
	}
	if elapsed := time.Since(start); elapsed > shutdownFlushTimeout/2 {
		t.Fatalf("Run took %s to exit after the verdict was delivered; the ack barrier looks "+
			"held (flush timeout is %s)", elapsed, shutdownFlushTimeout)
	}
	assertTerminated(t, pid, "svc", 15*time.Second)
}

// ── output EOF is not command exit ───────────────────────────────────────────
//
// A managed command's log stream can finish long before the command does: a
// config that redirects or closes its own output leaves the runner's pipe with
// no writer at all. Reaching the end of the stream is therefore not evidence of
// exit, and must never terminate anything.

// TestRun_EOFDoesNotTerminateAHealthyService is the finding-1 regression.
//
// The helper closes both output streams immediately, so the runner's log pipe
// reaches EOF while the service is still working. It then proves continued work
// on a deterministic non-output channel (the ready file) and exits only when
// released. Reaching the end of the output stream must not terminate it.
func TestRun_EOFDoesNotTerminateAHealthyService(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "svc.pid")
	working := filepath.Join(dir, "working")
	release := filepath.Join(dir, "release")

	cmd := discardOutputScript(t, helperCmd("quiet-loop", pidFile, working, release))

	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:             cmd,
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	pid, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("service never recorded its PID")
	}
	trackProcessForCleanup(t, pid, "service")

	// The ready file is the non-output channel: it exists only once the streams
	// are closed, so the service is provably past EOF by the time it appears.
	waitForFilePresent(t, working, 20*time.Second)

	// Give any (incorrect) EOF-driven teardown ample time to act.
	time.Sleep(2 * time.Second)

	if !pidIsRunning(pid) {
		t.Fatalf("service (PID %d) was terminated after its output ended; EOF is not "+
			"evidence that the command exited", pid)
	}
	select {
	case err := <-done:
		t.Fatalf("runner returned (%v) while the service was still running", err)
	default:
	}

	// Now actually stop it, and require that this — not EOF — is what ends it.
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown reported failure: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not stop after the service was released")
	}
	assertTerminated(t, pid, "service", 15*time.Second)
	if err := r.ShutdownResult(); err != nil {
		t.Fatalf("a cleanly stopped service was reported as a problem: %v", err)
	}
}

// TestRun_StopTerminatesAHealthyServiceThatClosedItsOutput is the other half of
// the end-of-stream contract.
//
// The companion test proves that EOF does not terminate a healthy service. This
// one proves the converse, which is what the old design got wrong: reaching EOF
// must not withdraw shutdown authority, so an ordinary stop still escalates all
// the way to the group SIGKILL and the service really dies.
//
// The fixture closes its streams and then ignores SIGTERM, so the only thing
// that can end it is a group-wide kill after the grace period.
func TestRun_StopTerminatesAHealthyServiceThatClosedItsOutput(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("Windows terminates through the Job Object; there is no SIGTERM to ignore")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "svc.pid")

	cmd := discardOutputScript(t, helperCmd("quiet-stubborn", pidFile))

	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:             cmd,
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	pid, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("service never recorded its PID")
	}
	trackProcessForCleanup(t, pid, "service")

	// Give the log reader time to see EOF, so the stop below provably happens
	// after the output has already ended.
	time.Sleep(2 * time.Second)
	if !pidIsRunning(pid) {
		t.Fatalf("service (PID %d) was terminated before any stop was requested", pid)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown reported failure: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runner did not stop")
	}

	assertTerminated(t, pid, "service that closed its output and ignored SIGTERM", 15*time.Second)
	if verdict := r.ShutdownResult(); verdict != nil {
		t.Fatalf("shutdown reported %v although the service was terminated", verdict)
	}
}

// TestSurvivorsError_AlwaysNamesTheGroup guards the diagnostic contract// TestSurvivorsError_AlwaysNamesTheGroup guards the diagnostic contract: a
// survivor report has to identify what could not be terminated even when the
// members could not be enumerated.
func TestSurvivorsError_AlwaysNamesTheGroup(t *testing.T) {
	err := survivorsError([]GroupResult{{
		Name:  "api",
		PGID:  41234,
		State: GroupOwned,
	}})
	if err == nil {
		t.Fatal("survivorsError() on an owned group with no enumerated members = nil, want an error")
	}
	msg := err.Error()
	for _, want := range []string{"api", "41234", "could not terminate"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "<nil>") {
		t.Fatalf("error = %q, want no unrendered member list", msg)
	}
}

// TestSignallingEstablished_UnsupportedObservationRevokesAuthority pins the
// other-Unix fallback: an exit seen only by reaping is not authority, because
// reaping is what releases the leader's process-table slot.
func TestSignallingEstablished_UnsupportedObservationRevokesAuthority(t *testing.T) {
	if durableContainment {
		t.Skip("this platform's containment survives the leader's exit")
	}
	if signallingEstablished(exitUnsupported, false) {
		t.Fatal("signallingEstablished(exitUnsupported, running) = true, want false: " +
			"reaping released the reservation the identifier depended on")
	}
	if !signallingEstablished(exitUnsupported, true) {
		t.Fatal("signallingEstablished(exitUnsupported, cancelled) = false, want true: " +
			"a cancelled command's unreaped group is still ours to signal")
	}
}

// TestSurvivorsError_UnavailableContainmentNamesTheGroup keeps the unavailable
// case informative.
func TestSurvivorsError_UnavailableContainmentNamesTheGroup(t *testing.T) {
	err := survivorsError([]GroupResult{{Name: "svc", State: GroupUnavailable}})
	if err == nil {
		t.Fatal("survivorsError() on an unavailable group = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %q, want it to say containment was unavailable", err.Error())
	}
	if strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("error = %q, want no unrendered member list", err.Error())
	}
}

func waitForFilePresent(t *testing.T, path string, timeout time.Duration) {
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

// TestRun_NaturalExitWithLiveDescendantIsTerminated is the acceptance test for
// the whole design: a command that backgrounds work and then exits naturally
// still owns that work, and proc-compose must terminate it rather than merely
// report it.
//
// The descendant ignores SIGTERM, so this also proves the escalation reaches the
// whole group after the leader has already gone. That is only sound because the
// leader is reaped last: while it is an unreaped zombie it keeps its
// process-table slot, and its PID — the process-group ID — cannot have been
// handed to anything else.
func TestRun_NaturalExitWithLiveDescendantIsTerminated(t *testing.T) {
	if !ignoresTermination() {
		t.Skip("Windows has no SIGTERM for console apps; termination is Job Object based")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	spawn := writeScript(t, dir, "spawn.sh", spawnScript)

	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:             spawn + " " + stubborn + " " + pidFile,
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
		"keeper": {
			Cmd:             helperCmd("sleep", "120s"),
			Restart:         "never",
			ShutdownTimeout: shutdownGraceForTest,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	descendant, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("worker descendant never recorded its PID")
	}
	trackProcessForCleanup(t, descendant, "worker descendant")

	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.State == "exited" }, 20*time.Second) {
		t.Fatal("worker never reported its leader as exited")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown reported failure: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}

	// The descendant ignored SIGTERM, so only the group SIGKILL after the leader
	// had already exited can have ended it.
	assertTerminated(t, descendant, "same-group descendant of a naturally exited leader", 15*time.Second)
	if verdict := r.ShutdownResult(); verdict != nil {
		t.Fatalf("shutdown reported %v although descendant (PID %d) was terminated", verdict, descendant)
	}
}

// TestRun_CleanServiceExitIsNotReportedAsSurvivor guards the opposite direction:
// the ordinary exit path must not manufacture a survivor. This is the case that
// would break if the group were judged before the reap, where a
// terminated-but-unreaped leader still makes the group look populated.
func TestRun_CleanServiceExitIsNotReportedAsSurvivor(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "svc.pid")
	script := writeScript(t, dir, "quick.sh", quickExitScript)

	r := makeRunner(map[string]config.Process{
		"svc": {Cmd: script + " " + pidFile, Restart: "never"},
	})

	start := time.Now()
	err := r.Run(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("clean exit reported failure: %v", err)
	}
	if err := r.ShutdownResult(); err != nil {
		t.Fatalf("clean exit produced a shutdown verdict: %v", err)
	}
	// A false survivor would have to burn the whole grace and kill-settle budget
	// before being reported, so the elapsed time proves the group was judged
	// immediately rather than after a stall.
	if elapsed > 2*time.Second {
		t.Fatalf("clean exit took %s; the group was probably judged before the leader was reaped", elapsed)
	}
	pid, _ := waitForPIDFile(t, pidFile, 2*time.Second)
	if pid > 0 && pidIsRunning(pid) {
		t.Fatalf("service (PID %d) outlived its own exit", pid)
	}
}

// quickExitScript records its PID and exits immediately, leaving nothing behind.
const quickExitScript = `#!/bin/sh
echo $$ > "$1"
exit 0
`
