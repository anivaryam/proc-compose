package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

func TestRestart_RecoversFailedServiceInLiveDaemon(t *testing.T) {
	trigger := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(trigger, []byte("fail"), 0600); err != nil {
		t.Fatal(err)
	}
	r := makeRunner(map[string]config.Process{
		"worker": {Cmd: helperCmd("fail-after-file", trigger, "30s"), Restart: "never"},
		"api":    {Cmd: helperCmd("sleep", "30s"), Restart: "never"},
	})
	socketPath, server, stopServer := ipcServerForTest(t)
	defer stopServer()
	r.IPC = server
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.State == "failed" }, 5*time.Second) {
		t.Fatal("worker never failed")
	}
	if err := os.Remove(trigger); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Send(ipc.Command{Action: "restart", Process: "worker"}); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := client.RecvTimeout(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == ipc.TypeAck {
			if event.Ack != "ok" || event.AckGeneration != 2 {
				t.Fatalf("restart verdict: %+v", event)
			}
			break
		}
	}
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.Generation == 2 && s.State == "running" && s.Ready }, 5*time.Second) {
		t.Fatalf("worker did not recover: %+v", snapshotOf(r, "worker"))
	}
}

// TestRun_FastFailureKeepsExitStatus pins the exit status of a command that
// exits on its own. Where the exit is observed without reaping, the reap starts
// only at the end of exec, and its result used to be read only if it had not
// been published yet. A leader that is already a zombie is collected at once,
// so the result could be published before that check and the status dropped: a
// command that exited 1 was reported as a clean exit instead of failed. The
// seam holds exec until the reap has published, which makes that order certain.
func TestRun_FastFailureKeepsExitStatus(t *testing.T) {
	r := makeRunner(map[string]config.Process{"worker": {Cmd: serviceHelperCmd("exit", "1"), Restart: "never"}})
	// A hold that times out no longer forces the ordering under test, so it is
	// recorded and fails the test rather than silently letting exec continue.
	var holdTimedOut atomic.Bool
	r.reapStarted = func(reap *singleReap) {
		select {
		case <-reap.done:
		case <-time.After(10 * time.Second):
			holdTimedOut.Store(true)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop after cancellation")
		}
	})
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return for a command that exits at once")
	}
	if holdTimedOut.Load() {
		t.Fatal("reap was not published within the hold; the publish-before-read ordering was not forced")
	}
	got := snapshotOf(r, "worker")
	if got.State != "failed" || got.ExitCode == nil || *got.ExitCode != 1 {
		code := "<nil>"
		if got.ExitCode != nil {
			code = strconv.Itoa(*got.ExitCode)
		}
		t.Fatalf("worker state=%s exit=%s, want failed with exit 1", got.State, code)
	}
}

func TestReload_RebuildsReadinessForReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	write := func(command, probe string, timeout int) {
		t.Helper()
		body := fmt.Sprintf("processes:\n  server:\n    cmd: %q\n    ready_timeout: %d\n    ready_when:\n      log: %q\n", command, timeout, probe)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(helperCmd("log-block", "OLD"), "^OLD$", 10)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: cfg, ConfigPath: path, NoColor: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()
	if !waitForProcState(t, r, "server", func(s ipc.ProcState) bool { return s.Ready }, 5*time.Second) {
		t.Fatal("initial readiness failed")
	}
	write(helperCmd("log-block", "NEW"), "^NEW$", 1)
	if result := r.Reload(); result.Status != "ok" {
		t.Fatalf("reload: %+v", result)
	}
	if !waitForProcState(t, r, "server", func(s ipc.ProcState) bool { return s.Generation == 2 && s.Ready }, 3*time.Second) {
		t.Fatalf("replacement did not use new probe: %+v", snapshotOf(r, "server"))
	}
	write(helperCmd("ready-after", "2s", "NEW"), "^NEW$", 1)
	if result := r.Reload(); result.Status != "ok" {
		t.Fatalf("reload: %+v", result)
	}
	if !waitForProcState(t, r, "server", func(s ipc.ProcState) bool {
		return s.Generation == 3 && s.State == "failed" && strings.Contains(s.LastError, "1s")
	}, 3*time.Second) {
		t.Fatalf("replacement did not use new timeout: %+v", snapshotOf(r, "server"))
	}
}

// TestStoreSnapshot_MatchesBroadcastAfterReload pins the single ProcState
// builder. status, /ready and /health read the store snapshot, and the event
// stream reads broadcastState. After a reload replaces a definition, both must
// describe the incarnation that is running, not the definition loaded at
// startup. Reload refuses mode and depends_on changes, so the stale startup
// definition is caught on the store itself rather than on a ProcState field.
func TestStoreSnapshot_MatchesBroadcastAfterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	write := func(marker string) {
		t.Helper()
		body := fmt.Sprintf("processes:\n  api:\n    cmd: %q\n  server:\n    cmd: %q\n    depends_on: [api]\n    ready_when:\n      log: \"^(OLD|NEW)$\"\n",
			serviceHelperCmd("sleep", "30s"), serviceHelperCmd("log-block", marker))
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("OLD")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: cfg, ConfigPath: path, NoColor: true, Output: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()
	if !waitForProcState(t, r, "server", func(s ipc.ProcState) bool { return s.Ready }, 5*time.Second) {
		t.Fatalf("initial readiness failed: %+v", snapshotOf(r, "server"))
	}
	write("NEW")
	if result := r.Reload(); result.Status != "ok" {
		t.Fatalf("reload: %+v", result)
	}
	if !waitForProcState(t, r, "server", func(s ipc.ProcState) bool { return s.Generation == 2 && s.Ready }, 5*time.Second) {
		t.Fatalf("replacement never became ready: %+v", snapshotOf(r, "server"))
	}

	want, _ := r.getProc("server")
	info, ok := r.store.Load().info("server")
	if !ok {
		t.Fatal("store has no definition for server")
	}
	if !reflect.DeepEqual(info.proc, want) {
		t.Fatalf("snapshot reads a stale definition after reload: cmd %q, want %q", info.proc.Cmd, want.Cmd)
	}

	comparable := func(s ipc.ProcState) ipc.ProcState {
		s.CPUPercent, s.MemoryMB = 0, 0 // the metrics sampler updates these between broadcasts
		return s
	}
	var snap, last ipc.ProcState
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap = comparable(snapshotOf(r, "server"))
		r.logMu.Lock()
		last = comparable(r.lastStates["server"])
		r.logMu.Unlock()
		if reflect.DeepEqual(snap, last) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !reflect.DeepEqual(snap, last) {
		t.Fatalf("snapshot and last broadcast disagree after reload:\nsnapshot:  %+v\nbroadcast: %+v", snap, last)
	}
	if !slices.Equal(snap.DependsOn, []string{"api"}) || snap.Mode != config.ProcessModeService {
		t.Fatalf("snapshot lost the definition: mode=%q depends_on=%v", snap.Mode, snap.DependsOn)
	}
}

func TestReload_RejectsModeChangeBeforeMutatingRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	if err := os.WriteFile(path, []byte("processes:\n  worker:\n    cmd: changed\n    mode: task\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := makeRunner(map[string]config.Process{"worker": {Cmd: "original", Restart: "never"}})
	r.ConfigPath = path
	if result := r.Reload(); result.Status != "error" {
		t.Fatalf("accepted unsupported transition: %+v", result)
	}
	if r.Config.Processes["worker"].Cmd != "original" {
		t.Fatal("rejected reload mutated live configuration")
	}
}

func TestReload_DetectsEmptyEnvironmentKeyReplacement(t *testing.T) {
	a := config.Process{Cmd: "app", Env: map[string]string{"OLD": ""}}
	b := config.Process{Cmd: "app", Env: map[string]string{"NEW": ""}}
	if !procChanged(a, b) {
		t.Fatal("environment key replacement was ignored")
	}
}

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

// serviceHelperCmd is helperCmd for a long-running service whose leader must be
// the helper itself. On Unix the shell execs it, so a test that kills or waits
// the leader through its owned handle leaves no helper behind. Windows cmd has
// no exec; there the helper stays a child of cmd, inside the same Job Object,
// which the runner's seal closes with kill-on-close.
func serviceHelperCmd(args ...string) string {
	if runtime.GOOS == "windows" {
		return helperCmd(args...)
	}
	return "exec " + helperCmd(args...)
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
	case "serve-http":
		// serve-http <addr>: answer every request on addr with this
		// process's PID, so a test can say which process served a probe.
		// It exits on its own after a minute, so a failing test cannot leak it.
		if len(args) != 2 {
			os.Exit(2)
		}
		ln, err := net.Listen("tcp", args[1])
		if err != nil {
			os.Exit(2)
		}
		time.AfterFunc(time.Minute, func() { os.Exit(3) })
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, os.Getpid()) }))
		os.Exit(0)
	case "serve-http-retry":
		// serve-http-retry <addr>: serve-http, but keep retrying the bind
		// while another process holds addr, so it takes the port over the
		// moment that process lets go.
		if len(args) != 2 {
			os.Exit(2)
		}
		time.AfterFunc(time.Minute, func() { os.Exit(3) })
		for {
			ln, err := net.Listen("tcp", args[1])
			if err == nil {
				_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, os.Getpid()) }))
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
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
		if len(args) < 2 || len(args) > 3 {
			os.Exit(2)
		}
		output := os.Stdout
		if len(args) == 3 && args[2] == "stderr" {
			output = os.Stderr
		}
		_, _ = output.WriteString(args[1] + "\n")
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
	case "term-exit0":
		// term-exit0: run until SIGTERM, then exit 0. Models a service that
		// shuts down gracefully when proc-compose cancels it, so the cancel
		// itself is the only evidence that the incarnation failed.
		if len(args) != 1 {
			os.Exit(2)
		}
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		select {
		case <-term:
			os.Exit(0)
		case <-time.After(60 * time.Second):
			os.Exit(7)
		}
	case "probe-file":
		// probe-file <path>: a ready_when.cmd probe that talks on both streams
		// and passes only once path exists.
		if len(args) != 2 {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("probe-chatter-stdout\n")
		_, _ = os.Stderr.WriteString("probe-chatter-stderr\n")
		if _, err := os.Stat(args[1]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "probe-env":
		// probe-env <key> <value> <dir>: passes only when it runs with the
		// process's environment and working directory.
		if len(args) != 4 {
			os.Exit(2)
		}
		wd, err := os.Getwd()
		if err != nil {
			os.Exit(2)
		}
		want, err := filepath.EvalSymlinks(args[3])
		if err != nil {
			os.Exit(2)
		}
		if got, err := filepath.EvalSymlinks(wd); err != nil || got != want || os.Getenv(args[1]) != args[2] {
			os.Exit(1)
		}
		os.Exit(0)
	case "pid-block":
		// pid-block <pidfile>: record this PID, then hang.
		if len(args) != 2 {
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(2)
		}
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "teardown-probe":
		// teardown-probe <pidfile> <out> <code>: write to out whether the PID in
		// pidfile is still running ("alive" or "gone"), then exit with code.
		if len(args) != 4 {
			os.Exit(2)
		}
		code, err := strconv.Atoi(args[3])
		if err != nil {
			os.Exit(2)
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			os.Exit(2)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			os.Exit(2)
		}
		verdict := "gone"
		if pidIsRunning(pid) {
			verdict = "alive"
		}
		if err := os.WriteFile(args[2], []byte(verdict), 0o644); err != nil {
			os.Exit(2)
		}
		os.Exit(code)
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

func TestRun_JSONEventsHaveOneIdentityAndPreserveStderr(t *testing.T) {
	var output, journal bytes.Buffer
	r := makeRunner(map[string]config.Process{
		"job": {Cmd: helperCmd("log", `{"level":"error","message":"application failure"}`, "stderr"), Mode: config.ProcessModeTask, Restart: "never"},
	})
	r.Output, r.Journal, r.LogFormat, r.Verbose, r.Silent = &output, &journal, "json", true, true
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output.String() != journal.String() {
		t.Fatal("console and journal diverged")
	}
	decoder := json.NewDecoder(&journal)
	var sequence uint64
	outputs := 0
	for {
		var event ipc.LogEntry
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event.Version != 1 || event.ID == "" || event.RunID == "" || event.Time.IsZero() || event.Sequence != sequence+1 {
			t.Fatalf("invalid event identity: %+v", event)
		}
		sequence = event.Sequence
		if event.Kind == "output" {
			outputs++
			if event.Stream != "stderr" || event.Level != "error" || event.Generation != 1 || event.PID <= 0 || event.Code != "process.output" {
				t.Fatalf("lost output provenance: %+v", event)
			}
		}
	}
	if outputs != 1 {
		t.Fatalf("application message emitted %d times", outputs)
	}
}

func TestReadOutput_OversizedLineDoesNotLoseFollowingRecords(t *testing.T) {
	var journal bytes.Buffer
	r := &Runner{Output: io.Discard, Journal: &journal, Project: &ipc.Project{RunID: "test"}}
	long := strings.Repeat("x", (1<<20)+17)
	r.readOutput(procInfo{name: "worker"}, "stdout", strings.NewReader(long+"\nnext\n"), nil)
	decoder := json.NewDecoder(&journal)
	var first, next ipc.LogEntry
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&next); err != nil {
		t.Fatal(err)
	}
	if !first.Truncated || first.OriginalBytes != len(long) || len(first.Line) != 1<<20 || next.Line != "next" {
		t.Fatalf("oversized line contract lost: truncated=%v original=%d retained=%d next=%q", first.Truncated, first.OriginalBytes, len(first.Line), next.Line)
	}
}

func TestRun_JournalFailureIsReported(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "closed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	r := makeRunner(map[string]config.Process{"job": {Cmd: helperCmd("exit", "0"), Mode: config.ProcessModeTask}})
	r.Journal, r.Output, r.Silent = file, io.Discard, true
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "event journal") {
		t.Fatalf("lost journal failure: %v", err)
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

// TestRun_ReadyWhen_CmdBecomesReady pins the cmd probe kind: the service is
// running but not ready, and its dependent waits, until the probe command
// exits 0.
func TestRun_ReadyWhen_CmdBecomesReady(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "ready")
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			ReadyTimeout: 20,
			ReadyWhen:    &config.ReadyWhen{Cmd: helperCmd("probe-file", flag)},
		},
		"app": {Cmd: serviceHelperCmd("sleep", "30s"), DependsOn: []string{"worker"}},
	})
	r.Output = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()

	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.State == "running" }, 5*time.Second) {
		t.Fatalf("worker never started: %+v", snapshotOf(r, "worker"))
	}
	// Long enough for more than one failing attempt.
	time.Sleep(1500 * time.Millisecond)
	if got := snapshotOf(r, "worker"); got.Ready {
		t.Fatalf("worker ready before its probe passed: %+v", got)
	}
	if got := snapshotOf(r, "app"); got.PID != 0 {
		t.Fatalf("dependent started before the probe passed: %+v", got)
	}
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.State == "running" && s.Ready }, 10*time.Second) {
		t.Fatalf("worker never became ready after the probe passed: %+v", snapshotOf(r, "worker"))
	}
	if !waitForProcState(t, r, "app", func(s ipc.ProcState) bool { return s.State == "running" }, 10*time.Second) {
		t.Fatalf("dependent never started: %+v", snapshotOf(r, "app"))
	}
	if got := snapshotOf(r, "worker"); got.Generation != 1 || got.Restarts != 0 {
		t.Fatalf("probe disturbed the incarnation: %+v", got)
	}
}

// TestRun_ReadyWhen_CmdTimeoutFailsProcess pins that a cmd probe that never
// passes fails the service at ready_timeout, like every other probe kind, and
// that the timeout message itself (the error-level readiness.failed event and
// LastError, visible without --verbose) names the last attempt's exit status
// and stderr. With --verbose, failed attempts after the fifth are
// readiness.progress lines with the same detail.
func TestRun_ReadyWhen_CmdTimeoutFailsProcess(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:     serviceHelperCmd("sleep", "30s"),
			Restart: "never",
			// Long enough for more than five failed attempts.
			ReadyTimeout: 8,
			ReadyWhen:    &config.ReadyWhen{Cmd: helperCmd("probe-file", missing)},
		},
	})
	var journal bytes.Buffer
	r.Output, r.Journal, r.Verbose = io.Discard, &journal, true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("Run = %v, want the worker reported failed", err)
	}
	const want = "readiness probe (cmd) timed out (last attempt: exit 1: probe-chatter-stderr)"

	if got := snapshotOf(r, "worker"); got.State != "failed" || got.LastError != want {
		t.Fatalf("worker = %+v, want failed with LastError %q", got, want)
	}
	failed, progress := 0, 0
	decoder := json.NewDecoder(&journal)
	for {
		var event ipc.LogEntry
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		switch {
		case event.Code == "readiness.failed":
			failed++
			if event.Level != "error" || event.Line != want {
				t.Fatalf("readiness.failed = %+v, want error-level %q", event, want)
			}
		case event.Code == "readiness.progress" && strings.HasPrefix(event.Line, "probe cmd:"):
			progress++
			if event.Line != "probe cmd: exit 1: probe-chatter-stderr" {
				t.Fatalf("progress line = %q, want the attempt's exit status and stderr", event.Line)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("readiness.failed events = %d, want 1", failed)
	}
	if progress == 0 {
		t.Fatal("no probe cmd progress line after more than five failed attempts")
	}
}

// TestRun_ReadyWhen_CmdUsesProcessEnv pins that the probe runs with the
// process's own environment and working directory.
func TestRun_ReadyWhen_CmdUsesProcessEnv(t *testing.T) {
	dir := t.TempDir()
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			Dir:          dir,
			Env:          map[string]string{"PROBE_TOKEN": "from-process-env"},
			ReadyTimeout: 10,
			ReadyWhen:    &config.ReadyWhen{Cmd: helperCmd("probe-env", "PROBE_TOKEN", "from-process-env", dir)},
		},
	})
	r.Output = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.Ready || s.State == "failed" }, 12*time.Second) {
		t.Fatalf("worker never settled: %+v", snapshotOf(r, "worker"))
	}
	if got := snapshotOf(r, "worker"); !got.Ready {
		t.Fatalf("probe did not see the process env and dir: %+v", got)
	}
}

// TestRun_ReadyWhen_CmdProbeIsQuiet pins that probe processes are not managed
// processes on any surface: no <name>:probe state in the store snapshot, the
// IPC snapshot or the journal, and none of the probe's output emitted.
func TestRun_ReadyWhen_CmdProbeIsQuiet(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "ready")
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			ReadyTimeout: 20,
			ReadyWhen:    &config.ReadyWhen{Cmd: helperCmd("probe-file", flag)},
		},
	})
	var output, journal bytes.Buffer
	r.Output, r.Journal, r.Verbose = &output, &journal, true
	socketPath, server, stopServer := ipcServerForTest(t)
	defer stopServer()
	r.IPC = server
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stopped := false
	defer func() {
		if !stopped {
			cancel()
			<-done
		}
	}()

	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.State == "running" }, 5*time.Second) {
		t.Fatalf("worker never started: %+v", snapshotOf(r, "worker"))
	}
	time.Sleep(1500 * time.Millisecond) // at least one failing attempt
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.Ready }, 10*time.Second) {
		t.Fatalf("worker never became ready: %+v", snapshotOf(r, "worker"))
	}

	for _, st := range r.Snapshot() {
		if st.Name != "worker" {
			t.Fatalf("store snapshot has a non-managed process: %+v", st)
		}
	}
	client, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.RecvTimeout(5 * time.Second)
	client.Close()
	if err != nil || snapshot.Type != ipc.TypeSnapshot {
		t.Fatalf("IPC snapshot = %+v, %v", snapshot, err)
	}
	for _, st := range snapshot.Processes {
		if st.Name != "worker" {
			t.Fatalf("IPC snapshot has a non-managed process: %+v", st)
		}
	}
	for _, entry := range snapshot.RecentLogs {
		if strings.HasSuffix(entry.Process, ":probe") {
			t.Fatalf("IPC log history has a probe event: %+v", entry)
		}
	}

	cancel()
	stopped = true
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.logMu.Lock()
	for name := range r.lastStates {
		if name != "worker" {
			t.Errorf("broadcast state cached for %q", name)
		}
	}
	r.logMu.Unlock()
	decoder := json.NewDecoder(&journal)
	for {
		var event ipc.LogEntry
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(event.Process, ":probe") || strings.Contains(event.Line, "probe-chatter") {
			t.Fatalf("journal has a probe event: %+v", event)
		}
	}
	if strings.Contains(output.String(), "probe-chatter") {
		t.Fatalf("probe output reached the console: %q", output.String())
	}
}

// TestRun_ReadyWhen_CmdProbeEscapeStaysQuiet pins that a probe stays out of
// the event stream on its error paths too. A probe whose descendant leaves the
// group still holding the output pipe makes the runner close the pipe under
// the blocked readers, so they fail with a read error instead of EOF; that
// read error must not surface as a "<name>:probe" event. The escape itself is
// still reported, because the descendant may outlive the stack.
func TestRun_ReadyWhen_CmdProbeEscapeStaysQuiet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid is a Unix facility")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not available on this host")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escapee.pid")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	escape := writeScript(t, dir, "escape.sh", escapeScript)
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			ReadyTimeout: 20,
			ReadyWhen:    &config.ReadyWhen{Cmd: escape + " " + stubborn + " " + pidFile},
		},
	})
	var journal bytes.Buffer
	r.Output, r.Journal, r.Verbose = io.Discard, &journal, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	escapee, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		cancel()
		<-done
		t.Fatal("escaped probe descendant never recorded its PID")
	}
	trackProcessForCleanup(t, escapee, "escaped probe descendant")
	if !waitForProcState(t, r, "worker", func(s ipc.ProcState) bool { return s.Ready }, 20*time.Second) {
		cancel()
		<-done
		t.Fatalf("worker never became ready: %+v", snapshotOf(r, "worker"))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("runner did not finish shutting down")
	}

	if verdict := r.ShutdownResult(); verdict == nil || !strings.Contains(verdict.Error(), "worker:probe") {
		t.Fatalf("verdict = %v, want the escaped probe descendant reported", verdict)
	}
	decoder := json.NewDecoder(&journal)
	for {
		var event ipc.LogEntry
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(event.Process, ":probe") {
			t.Fatalf("journal has a probe event: %+v", event)
		}
	}
}

// TestRun_ReadyWhen_CmdProbeEndsWithIncarnation pins that a probe still
// running when the stack stops is terminated and its group released before Run
// returns. TestRun_ReadyWhen_CmdProbeIsJoinedBeforeRunReturns pins the join.
func TestRun_ReadyWhen_CmdProbeEndsWithIncarnation(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "probe.pid")
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			ReadyTimeout: -1,
			ReadyWhen:    &config.ReadyWhen{Cmd: serviceHelperCmd("pid-block", pidFile)},
		},
	})
	r.Output = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	pid, ok := waitForPIDFile(t, pidFile, 10*time.Second)
	if !ok {
		cancel()
		<-done
		t.Fatal("probe never started")
	}
	trackProcessForCleanup(t, pid, "probe")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return with a probe running")
	}
	assertTerminated(t, pid, "probe", 2*time.Second)
	if groups := r.snapshotGroups(); len(groups) != 0 {
		t.Fatalf("groups left registered: %d", len(groups))
	}
}

// TestRun_ReadyWhen_CmdProbeReapsDoNotAccumulate pins that a probe retried
// without limit (ready_timeout -1) does not grow the reap registry: each
// completed attempt's wait, and with it the attempt's exec.Cmd, is dropped
// once a later reap registers, rather than kept until Run returns.
func TestRun_ReadyWhen_CmdProbeReapsDoNotAccumulate(t *testing.T) {
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			ReadyTimeout: -1,
			ReadyWhen:    &config.ReadyWhen{Cmd: helperCmd("exit", "1")},
		},
	})
	r.Output = io.Discard
	var started atomic.Int32
	r.reapStarted = func(*singleReap) { started.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()

	// The worker's own reap plus four probe attempts.
	for deadline := time.Now().Add(15 * time.Second); started.Load() < 5; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d reaps started", started.Load())
		}
	}
	r.reapersMu.Lock()
	total, probes := len(r.reapers), 0
	for _, tracked := range r.reapers {
		if tracked.process == "worker:probe" {
			probes++
		}
	}
	r.reapersMu.Unlock()
	// At most the worker's pending reap and the latest probe attempt.
	if probes > 1 || total > 2 {
		t.Fatalf("reap registry kept %d entries (%d probe attempts) after %d reaps started", total, probes, started.Load())
	}
}

// TestRun_ReadyWhen_CmdProbeIsJoinedBeforeRunReturns pins that the probe
// goroutine itself, not just its process, has finished when Run returns. The
// probe's reap is held, so its attempt cannot finish until the stack hands off
// cleanup; a runner that stopped joining the probe would let the incarnation,
// and then Run, return with that goroutine still inside probeCmd.
func TestRun_ReadyWhen_CmdProbeIsJoinedBeforeRunReturns(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "probe.pid")
	probeCmd := serviceHelperCmd("pid-block", pidFile)
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:          serviceHelperCmd("sleep", "30s"),
			ReadyTimeout: -1,
			ReadyWhen:    &config.ReadyWhen{Cmd: probeCmd},
		},
	})
	r.Output = io.Discard
	r.CleanupHandoffTimeout = 250 * time.Millisecond
	var probeReapHeld atomic.Bool
	reapRelease := make(chan struct{})
	releaseReap := sync.OnceFunc(func() { close(reapRelease) })
	t.Cleanup(releaseReap)
	r.waitProcess = func(cmd *exec.Cmd) error {
		if strings.Contains(strings.Join(cmd.Args, " "), probeCmd) {
			probeReapHeld.Store(true)
			<-reapRelease
		}
		return cmd.Wait()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	pid, ok := waitForPIDFile(t, pidFile, 10*time.Second)
	if !ok {
		cancel()
		releaseReap()
		<-done
		t.Fatal("probe never started")
	}
	trackProcessForCleanup(t, pid, "probe")
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		releaseReap()
		t.Fatal("Run did not return with a probe running")
	}
	if !probeReapHeld.Load() {
		t.Fatal("the probe's reap was never held, so the join was not exercised")
	}
	buf := make([]byte, 1<<20)
	stacks := string(buf[:runtime.Stack(buf, true)])
	if strings.Contains(stacks, ".(*Runner).probeCmd") {
		t.Fatalf("probe goroutine still running after Run returned:\n%s", stacks)
	}
	assertTerminated(t, pid, "probe", 2*time.Second)
	// Let the held reap collect the probe's zombie before cleanup runs.
	releaseReap()
	for deadline := time.Now().Add(2 * time.Second); daemon.IsAlive(pid) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
}

// journalEvents decodes a journal written by a finished Run.
func journalEvents(t *testing.T, journal *bytes.Buffer) []ipc.LogEntry {
	t.Helper()
	var events []ipc.LogEntry
	decoder := json.NewDecoder(journal)
	for {
		var event ipc.LogEntry
		if err := decoder.Decode(&event); err == io.EOF {
			return events
		} else if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

// TestRun_HealthReportMarksUnhealthyAndRecovers pins health_action: report.
// The service keeps running and stays ready while its health_when probe fails;
// it becomes unhealthy after healthThreshold consecutive failures, recovers on
// the next pass, and each transition is journalled exactly once. Health probe
// runs stay out of the state store and the journal, like readiness probes.
func TestRun_HealthReportMarksUnhealthyAndRecovers(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "healthy")
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:            serviceHelperCmd("sleep", "60s"),
			HealthWhen:     &config.ReadyWhen{Cmd: helperCmd("probe-file", flag)},
			HealthInterval: 1,
			// The probe inherits the process env. A -race helper otherwise
			// sleeps 1s at exit (the race runtime's atexit_sleep_ms), which
			// is the whole 1s check limit of this interval.
			Env: map[string]string{"GORACE": "atexit_sleep_ms=0"},
		},
	})
	var journal bytes.Buffer
	r.Output, r.Journal = io.Discard, &journal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			t.Fatal("runner did not stop")
			return nil
		}
	}

	health := func(want string) func(ipc.ProcState) bool {
		return func(s ipc.ProcState) bool { return s.Health == want }
	}
	if !waitForProcState(t, r, "worker", health("healthy"), 10*time.Second) {
		_ = stop()
		t.Fatalf("worker never became healthy: %+v", snapshotOf(r, "worker"))
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	if !waitForProcState(t, r, "worker", health("unhealthy"), 15*time.Second) {
		_ = stop()
		t.Fatalf("worker never became unhealthy: %+v", snapshotOf(r, "worker"))
	}
	got := snapshotOf(r, "worker")
	if got.State != "running" || !got.Ready || got.Generation != 1 || got.Restarts != 0 {
		t.Fatalf("report action disturbed the incarnation: %+v", got)
	}
	if !strings.Contains(got.HealthError, "exit 1") || !strings.Contains(got.HealthError, "probe-chatter-stderr") {
		t.Fatalf("health_error = %q, want the exit status and stderr head", got.HealthError)
	}
	for _, st := range r.Snapshot() {
		if st.Name != "worker" {
			t.Fatalf("store snapshot has a non-managed process: %+v", st)
		}
	}
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitForProcState(t, r, "worker", health("healthy"), 10*time.Second) {
		_ = stop()
		t.Fatalf("worker never recovered: %+v", snapshotOf(r, "worker"))
	}
	if got := snapshotOf(r, "worker"); got.HealthError != "" || got.Generation != 1 {
		t.Fatalf("recovered state = %+v", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	counts := map[string]int{}
	var healthStates []string
	for _, event := range journalEvents(t, &journal) {
		// The stderr head reaches health events as the failure detail; the
		// probe's own output never becomes an output event.
		if strings.HasSuffix(event.Process, ":probe") || (event.Kind == "output" && strings.Contains(event.Line, "probe-chatter")) {
			t.Fatalf("journal has a probe event: %+v", event)
		}
		counts[event.Code]++
		if strings.HasPrefix(event.Code, "health.") && event.Kind != "probe" {
			t.Fatalf("health event kind = %q, want probe: %+v", event.Kind, event)
		}
		if event.Code == "process.state" && event.State != nil && event.State.Health != "" {
			if n := len(healthStates); n == 0 || healthStates[n-1] != event.State.Health {
				healthStates = append(healthStates, event.State.Health)
			}
		}
	}
	if counts["health.unhealthy"] != 1 || counts["health.recovered"] != 1 || counts["health.check_failed"] < healthThreshold {
		t.Fatalf("health events = %v, want one unhealthy, one recovered, at least %d check_failed", counts, healthThreshold)
	}
	// The final unknown is the stop: a process that is not running has no
	// current verdict.
	if want := []string{"unknown", "healthy", "unhealthy", "healthy", "unknown"}; !slices.Equal(healthStates, want) {
		t.Fatalf("journalled health transitions = %v, want %v", healthStates, want)
	}
}

// TestRun_HealthProbeGroupSealedOnShutdown pins that a health_when.cmd run is
// owned by its incarnation: one still running when the stack stops is bounded
// by the cleanup handoff, sealed with the stack, and its goroutine is joined
// before Run returns.
func TestRun_HealthProbeGroupSealedOnShutdown(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "probe.pid")
	probeCmd := serviceHelperCmd("pid-block", pidFile)
	r := makeRunner(map[string]config.Process{
		"worker": {
			Cmd:            serviceHelperCmd("sleep", "30s"),
			HealthWhen:     &config.ReadyWhen{Cmd: probeCmd},
			HealthInterval: 3,
		},
	})
	r.Output = io.Discard
	r.CleanupHandoffTimeout = 250 * time.Millisecond
	var probeReapHeld atomic.Bool
	reapRelease := make(chan struct{})
	releaseReap := sync.OnceFunc(func() { close(reapRelease) })
	t.Cleanup(releaseReap)
	r.waitProcess = func(cmd *exec.Cmd) error {
		if strings.Contains(strings.Join(cmd.Args, " "), probeCmd) {
			probeReapHeld.Store(true)
			<-reapRelease
		}
		return cmd.Wait()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	pid, ok := waitForPIDFile(t, pidFile, 10*time.Second)
	if !ok {
		cancel()
		releaseReap()
		<-done
		t.Fatal("health probe never started")
	}
	trackProcessForCleanup(t, pid, "health probe")
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		releaseReap()
		t.Fatal("Run did not return with a health probe running")
	}
	if !probeReapHeld.Load() {
		t.Fatal("the probe's reap was never held, so the handoff was not exercised")
	}
	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, ".(*Runner).monitorHealth") {
		t.Fatalf("health goroutine still running after Run returned:\n%s", stacks)
	}
	assertTerminated(t, pid, "health probe", 2*time.Second)
	releaseReap()
	for deadline := time.Now().Add(2 * time.Second); daemon.IsAlive(pid) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
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

// TestServeHealth_UnhealthyServiceIs503 pins the /health rule: a running
// service whose health_when probe has failed is unhealthy, while /ready keeps
// reporting the startup latch.
func TestServeHealth_UnhealthyServiceIs503(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	r := makeRunner(map[string]config.Process{
		// 127.0.0.1:1 is a reserved port that nothing listens on.
		"svc": {Cmd: serviceHelperCmd("sleep", "60s"), HealthWhen: &config.ReadyWhen{TCP: "127.0.0.1:1"}, HealthInterval: 1},
	})
	r.Output = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	if !waitForProcState(t, r, "svc", func(s ipc.ProcState) bool { return s.Health == "unhealthy" }, 15*time.Second) {
		t.Fatalf("svc never became unhealthy: %+v", snapshotOf(r, "svc"))
	}
	go r.serveHealth(ln, r.store.Load())

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	var body struct {
		Healthy   bool `json:"healthy"`
		Processes []struct {
			Name   string `json:"name"`
			State  string `json:"state"`
			Health string `json:"health"`
		} `json:"processes"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || body.Healthy || len(body.Processes) != 1 ||
		body.Processes[0].State != "running" || body.Processes[0].Health != "unhealthy" {
		t.Fatalf("/health = %d %+v, want 503 naming the unhealthy running service", resp.StatusCode, body)
	}
	ready, err := client.Get("http://" + ln.Addr().String() + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	ready.Body.Close()
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("/ready = %d, want 200: health does not change readiness", ready.StatusCode)
	}
}

// ─── Restart guardrail tests ────────────────────────────────────────────

func TestHandleRestart_RejectsTaskMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket IPC not used on Windows")
	}
	// The service keeps the daemon alive after the task completes, so the
	// request is answered by a live stack rather than racing its shutdown.
	r := makeRunner(map[string]config.Process{
		"migrate": {Cmd: helperCmd("exit", "0"), Restart: "never", Mode: config.ProcessModeTask},
		"keeper":  {Cmd: helperCmd("sleep", "60s"), Restart: "never"},
	})

	socketPath, ipcServer, stopServer := ipcServerForTest(t)
	defer stopServer()

	r.IPC = ipcServer
	r.ConfigPath = t.TempDir() + "/proc-compose.yml"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	}()

	if !waitForProcState(t, r, "migrate", func(s ipc.ProcState) bool { return s.State == "completed" }, 10*time.Second) {
		t.Fatalf("task never completed: %+v", snapshotOf(r, "migrate"))
	}

	ipcClient, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer ipcClient.Close()

	if err := ipcClient.Send(ipc.Command{Action: "restart", Process: "migrate"}); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	for {
		ev, err := ipcClient.RecvTimeout(5 * time.Second)
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
}

// TestHandleCommands_AnswersDuringTeardown pins that IPC commands are answered
// while the stack tears down. The command loop used to stop as soon as the run
// was cancelled, so a client asking during teardown, including a stop, waited
// for the server's 30s timeout instead of getting the daemon's answer.
func TestHandleCommands_AnswersDuringTeardown(t *testing.T) {
	r := makeRunner(map[string]config.Process{"api": {Cmd: serviceHelperCmd("sleep", "60s"), Restart: "never"}})
	// The reap is the last step of teardown, so holding it keeps the run
	// cancelled but not yet returned on every platform.
	waitStarted, reapRelease := make(chan struct{}), make(chan struct{})
	waitStartedOnce := sync.OnceFunc(func() { close(waitStarted) })
	r.waitProcess = func(cmd *exec.Cmd) error {
		waitStartedOnce()
		<-reapRelease
		return cmd.Wait()
	}
	socketPath, ipcServer, stopServer := ipcServerForTest(t)
	defer stopServer()
	r.IPC = ipcServer

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(runDone)
	}()
	releaseReap := sync.OnceFunc(func() { close(reapRelease) })
	t.Cleanup(func() {
		releaseReap()
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop after the reap was released")
		}
	})

	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.State == "running" }, 10*time.Second) {
		t.Fatal("service never started")
	}
	cancel()
	select {
	case <-waitStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("teardown never reached the reap")
	}
	select {
	case <-runDone:
		t.Fatal("Run returned while its reap was held; teardown is not in progress")
	default:
	}

	client, err := ipc.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.Send(ipc.Command{Action: "restart", Process: "api"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for {
		ev, err := client.RecvTimeout(5 * time.Second)
		if err != nil {
			t.Fatalf("no answer to a command sent during teardown: %v", err)
		}
		if ev.Type != ipc.TypeAck {
			continue
		}
		if ev.Ack != "error" || ev.AckDetail != "daemon is shutting down" {
			t.Fatalf("ack during teardown = %s %q, want error %q", ev.Ack, ev.AckDetail, "daemon is shutting down")
		}
		break
	}
	releaseReap()
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the reap was released")
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
	if runtime.GOOS == "windows" {
		socketPath = `\\.\pipe\` + filepath.Base(dir)
	}

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
		Cmd:       "echo",
		ReadyWhen: &config.ReadyWhen{HTTP: "http://localhost:3000/health"},
	}
	b := config.Process{
		Cmd:       "echo",
		ReadyWhen: &config.ReadyWhen{HTTP: "http://localhost:4000/health"},
	}
	if !procChanged(a, b) {
		t.Error("procChanged should detect ReadyWhen change")
	}
}

func TestReload_DetectsReadyWhenCmdChange(t *testing.T) {
	a := config.Process{Cmd: "worker", ReadyWhen: &config.ReadyWhen{Cmd: "check --ready"}}
	b := config.Process{Cmd: "worker", ReadyWhen: &config.ReadyWhen{Cmd: "check --live"}}
	if !procChanged(a, b) {
		t.Error("procChanged should detect a ready_when.cmd change")
	}
}

func TestReload_DetectsHealthWhenChange(t *testing.T) {
	base := config.Process{Cmd: "worker", Restart: "always", HealthWhen: &config.ReadyWhen{Cmd: "check --live"}}
	for name, change := range map[string]func(*config.Process){
		"probe":    func(p *config.Process) { p.HealthWhen = &config.ReadyWhen{Cmd: "check --deep"} },
		"removed":  func(p *config.Process) { p.HealthWhen = nil },
		"interval": func(p *config.Process) { p.HealthInterval = 30 },
		"action":   func(p *config.Process) { p.HealthAction = config.HealthActionRestart },
	} {
		changed := base
		change(&changed)
		if !procChanged(base, changed) {
			t.Errorf("procChanged missed a health_* %s change", name)
		}
	}
	if procChanged(base, base) {
		t.Error("procChanged reported an unchanged health probe")
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
	return !pidIsTerminatedUnreaped(pid) && !pidHasExited(pid)
}

// pidIsTerminatedUnreaped reports whether pid is a zombie or already dead.
// Linux only; elsewhere daemon.ProcStat is unsupported and plain liveness is used.
func pidIsTerminatedUnreaped(pid int) bool {
	info, err := daemon.ProcStat(pid)
	if err != nil {
		return false
	}
	switch info.State {
	case 'Z', 'X', 'x':
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
func (g *uncontainedProcessGroup) Seal() (SealObservation, error) {
	return SealObservation{State: g.State(), Identity: g.Identify()}, nil
}
func (g *uncontainedProcessGroup) Close() error { return nil }

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
func (g *unstoppableProcessGroup) Seal() (SealObservation, error) {
	return SealObservation{State: g.State(), Identity: g.Identify()}, nil
}
func (g *unstoppableProcessGroup) Close() error { return nil }

type gatedSealGroup struct {
	mu          sync.Mutex
	signalStart chan struct{}
	signalGate  chan struct{}
	sealed      bool
	closed      bool
}

func (g *gatedSealGroup) Setup(_ *exec.Cmd)       {}
func (g *gatedSealGroup) Track(_ *exec.Cmd) error { return nil }
func (g *gatedSealGroup) Terminate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return os.ErrProcessDone
	}
	close(g.signalStart)
	<-g.signalGate
	return nil
}
func (g *gatedSealGroup) Kill() error       { return g.Terminate() }
func (g *gatedSealGroup) State() GroupState { return GroupOwned }
func (g *gatedSealGroup) Identify() string  { return "gated" }
func (g *gatedSealGroup) Withdraw() {
	g.mu.Lock()
	g.sealed = true
	g.mu.Unlock()
}
func (g *gatedSealGroup) Seal() (SealObservation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sealed = true
	g.closed = true
	return SealObservation{State: GroupOwned, Identity: "gated"}, nil
}
func (g *gatedSealGroup) Close() error { _, err := g.Seal(); return err }

// TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult pins the
// shared-runner boundary: a signal already holding the group lock completes
// before sealing returns, and sealing an owned group remains a failed verdict
// even while its independent reap is still blocked.
func TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult(t *testing.T) {
	r := &Runner{}
	pg := &gatedSealGroup{signalStart: make(chan struct{}), signalGate: make(chan struct{})}
	r.trackGroup("gated", pg, 0)
	signalRelease := sync.OnceFunc(func() { close(pg.signalGate) })
	defer signalRelease()

	signalDone := make(chan error, 1)
	go func() { signalDone <- pg.Terminate() }()
	<-pg.signalStart

	reapGate := make(chan struct{})
	reapRelease := sync.OnceFunc(func() { close(reapGate) })
	defer reapRelease()
	go func() { <-reapGate }() // model a cmd.Wait that has not completed

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweepDone := make(chan struct{})
	go func() { r.sweepGroups(ctx); close(sweepDone) }()
	select {
	case <-sweepDone:
		t.Fatal("seal returned while the in-flight signal was blocked")
	case <-time.After(20 * time.Millisecond):
	}

	signalRelease()
	if err := <-signalDone; err != nil {
		t.Fatalf("in-flight signal: %v", err)
	}
	select {
	case <-sweepDone:
	case <-time.After(time.Second):
		t.Fatal("seal did not finish after the in-flight signal completed")
	}
	if err := r.ShutdownResult(); err == nil || !strings.Contains(err.Error(), "could not terminate") {
		t.Fatalf("shutdown verdict = %v, want owned group reported unverified", err)
	}
	if err := r.ShutdownResult(); !strings.Contains(err.Error(), "gated") {
		t.Fatalf("shutdown verdict = %v, want sealed identity retained", err)
	}
	if err := pg.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("signal after seal = %v, want os.ErrProcessDone", err)
	}
	if !pg.closed {
		t.Fatal("seal did not close the group")
	}
	reapRelease()
}

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

	r := makeRunner(map[string]config.Process{
		"svc": {
			Cmd:     serviceHelperCmd("record-and-exit", pidFile),
			Restart: "never",
			Env:     map[string]string{"GORACE": "atexit_sleep_ms=0"},
		},
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
	pid, ok := waitForPIDFile(t, pidFile, 2*time.Second)
	if !ok {
		t.Fatal("service never recorded its PID")
	}
	if pid > 0 && pidIsRunning(pid) {
		t.Fatalf("service (PID %d) outlived its own exit", pid)
	}
}

// helperArgv is helperCmd as a direct argv, for checks that bypass the shell.
func helperArgv(args ...string) []string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(strings.Join(args, "\x00")))
	return []string{os.Args[0], "-test.run=^TestRunnerHelperProcess$", "--", helperSentinel, payload}
}

func startCheckStack(t *testing.T) (*Runner, context.CancelFunc, func() error) {
	t.Helper()
	return startCheckStackWith(t, config.Process{Cmd: helperCmd("sleep", "60s"), Restart: "never"})
}

// startCheckStackWith runs a one-service stack with no IPC server, as verify does
// when its socket cannot be bound, and waits for the service to be ready.
func startCheckStackWith(t *testing.T, api config.Process) (*Runner, context.CancelFunc, func() error) {
	t.Helper()
	r := makeRunner(map[string]config.Process{"api": api})
	r.NoColor, r.Silent, r.Output = true, true, io.Discard
	r.CleanupHandoffTimeout = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() { runErr = r.Run(ctx); close(done) }()
	wait := func() error {
		select {
		case <-done:
			return runErr
		case <-time.After(20 * time.Second):
			return errors.New("runner did not stop")
		}
	}
	t.Cleanup(func() {
		cancel()
		if err := wait(); err != nil && err.Error() == "runner did not stop" {
			t.Error(err)
		}
	})
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Ready }, 10*time.Second) {
		t.Fatal("stack did not become ready")
	}
	return r, cancel, wait
}

func TestRunCheck_DirectArgvAndOutputCapture(t *testing.T) {
	r, cancel, wait := startCheckStack(t)
	literal := `a b "quoted" $HOME; echo injected`
	result := r.RunCheck(context.Background(), CheckSpec{Name: "verify:check", Argv: helperArgv("log", literal), Cleanup: 5 * time.Second, MaxOutputBytes: 4096})
	if result.Err != nil || !result.Started || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("check result = %+v", result)
	}
	if result.Stdout != literal+"\n" {
		t.Fatalf("stdout = %q, want literal argv %q", result.Stdout, literal)
	}
	if result.Generation != 1 || result.HandedOff {
		t.Fatalf("generation/handoff = %d/%t", result.Generation, result.HandedOff)
	}
	if again := r.RunCheck(context.Background(), CheckSpec{Name: "verify:check", Argv: helperArgv("exit", "0"), Cleanup: time.Second}); again.Err == nil {
		t.Fatal("a check name was reused within one stack")
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("stack cleanup after check = %v", err)
	}
}

// TestRunCheck_ServiceFailureDoesNotCancelRunningCheckWithoutIPC pins that a
// running check keeps the stack open with or without an IPC server. Without one,
// the failed service's worker used to return, the last worker finishing cancelled
// running checks, and verify reported a check it had killed as failed. macOS CI
// hit this because the test socket path exceeded the platform limit, so verify
// ran without IPC.
func TestRunCheck_ServiceFailureDoesNotCancelRunningCheckWithoutIPC(t *testing.T) {
	trigger := filepath.Join(t.TempDir(), "fail")
	r, cancel, wait := startCheckStackWith(t, config.Process{Cmd: serviceHelperCmd("fail-after-file", trigger, "60s"), Restart: "never"})
	if r.IPC != nil {
		t.Fatal("fixture has an IPC server; the IPC-less path would not be exercised")
	}
	results := make(chan CheckResult, 1)
	go func() {
		results <- r.RunCheck(context.Background(), CheckSpec{Name: "verify:check", Argv: helperArgv("sleep", "1500ms"), Cleanup: 5 * time.Second})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.lifecycleMu.Lock()
		active := r.activeChecks
		r.lifecycleMu.Unlock()
		if active > 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("check was never admitted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(trigger, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.State == "failed" }, 10*time.Second) {
		t.Fatalf("service never failed: %+v", snapshotOf(r, "api"))
	}
	select {
	case result := <-results:
		t.Fatalf("check ended before its own completion once the service failed: %+v", result)
	default:
	}
	var result CheckResult
	select {
	case result = <-results:
	case <-time.After(20 * time.Second):
		t.Fatal("check did not finish")
	}
	if result.Err != nil || !result.Started || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("check result after the service failed = %+v, want its own clean exit", result)
	}
	cancel()
	if err := wait(); err != nil && err.Error() == "runner did not stop" {
		t.Fatal(err)
	}
}

func TestRunCheck_NonzeroExitAndOutputCap(t *testing.T) {
	r, _, _ := startCheckStack(t)
	result := r.RunCheck(context.Background(), CheckSpec{Name: "verify:check", Argv: helperArgv("log", strings.Repeat("x", 100), "stderr"), Cleanup: 5 * time.Second, MaxOutputBytes: 10})
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("exit = %v", result.ExitCode)
	}
	if !result.OutputTruncated || len(result.Stderr) != 10 || result.Stdout != "" {
		t.Fatalf("capture = stdout %q stderr %q truncated %t", result.Stdout, result.Stderr, result.OutputTruncated)
	}
	failed := r.RunCheck(context.Background(), CheckSpec{Name: "verify:check2", Argv: helperArgv("exit", "3"), Cleanup: 5 * time.Second})
	if failed.ExitCode == nil || *failed.ExitCode != 3 || failed.Err != nil {
		t.Fatalf("nonzero check = %+v", failed)
	}
}

func TestRunCheck_DeadlineTerminatesCheck(t *testing.T) {
	r, _, _ := startCheckStack(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := r.RunCheck(ctx, CheckSpec{Name: "verify:check", Argv: helperArgv("touch-block", pidFile), Cleanup: 10 * time.Second})
	if !result.Started || result.HandedOff {
		t.Fatalf("timed-out check = %+v", result)
	}
	if result.ExitCode != nil && *result.ExitCode == 0 {
		t.Fatalf("timed-out check reported success: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 9*time.Second {
		t.Fatalf("check teardown took %s", elapsed)
	}
	assertTerminated(t, result.PID, "timed-out check", 5*time.Second)
	if unresolved := r.Unresolved(); len(unresolved) != 0 {
		t.Fatalf("unresolved after clean check teardown: %+v", unresolved)
	}
}

func TestRunCheck_RefusedWithoutRunningStack(t *testing.T) {
	r := makeRunner(map[string]config.Process{})
	if result := r.RunCheck(context.Background(), CheckSpec{Name: "c", Argv: helperArgv("exit", "0")}); !result.AdmissionRefused || result.Started {
		t.Fatalf("check without stack = %+v", result)
	}
	if result := r.RunCheck(context.Background(), CheckSpec{Name: "c"}); result.Err == nil {
		t.Fatal("empty argv accepted")
	}
}

type admissionGateProcessGroup struct {
	ProcessGroup
	trackStarted     chan struct{}
	trackRelease     chan struct{}
	terminateStarted chan struct{}
	terminateRelease chan struct{}
	closed           chan struct{}
	terminateOnce    sync.Once
	closeOnce        sync.Once
	pid              int
	cmd              *exec.Cmd
}

func (g *admissionGateProcessGroup) Track(cmd *exec.Cmd) error {
	g.pid = cmd.Process.Pid
	g.cmd = cmd
	close(g.trackStarted)
	<-g.trackRelease
	return g.ProcessGroup.Track(cmd)
}

// Terminate and Kill share one gate on the first teardown signal: Unix teardown
// starts with Terminate, while Windows has no graceful phase and starts with Kill.
func (g *admissionGateProcessGroup) Terminate() error {
	g.holdTeardown()
	return g.ProcessGroup.Terminate()
}

func (g *admissionGateProcessGroup) Kill() error {
	g.holdTeardown()
	return g.ProcessGroup.Kill()
}

func (g *admissionGateProcessGroup) holdTeardown() {
	g.terminateOnce.Do(func() {
		close(g.terminateStarted)
		<-g.terminateRelease
	})
}

func (g *admissionGateProcessGroup) Close() error {
	err := g.ProcessGroup.Close()
	g.closeOnce.Do(func() { close(g.closed) })
	return err
}

type signalSealGateProcessGroup struct {
	ProcessGroup
	mu              sync.Mutex
	signalStarted   chan struct{}
	signalRelease   chan struct{}
	sealStarted     chan struct{}
	sealReturned    chan struct{}
	signalOnce      sync.Once
	sealOnce        sync.Once
	sealed          bool
	cmd             *exec.Cmd
	observation     SealObservation
	sealErr         error
	delegateSignals int
}

func (g *signalSealGateProcessGroup) Setup(cmd *exec.Cmd) {
	g.ProcessGroup.Setup(cmd)
	cmd.Cancel = func() error { return g.Terminate() }
}

func (g *signalSealGateProcessGroup) Track(cmd *exec.Cmd) error {
	g.cmd = cmd
	return g.ProcessGroup.Track(cmd)
}

func (g *signalSealGateProcessGroup) signal(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return os.ErrProcessDone
	}
	g.signalOnce.Do(func() {
		close(g.signalStarted)
		<-g.signalRelease
	})
	g.delegateSignals++
	return fn()
}

func (g *signalSealGateProcessGroup) Terminate() error {
	return g.signal(g.ProcessGroup.Terminate)
}

func (g *signalSealGateProcessGroup) Kill() error {
	return g.signal(g.ProcessGroup.Kill)
}

func (g *signalSealGateProcessGroup) Seal() (SealObservation, error) {
	g.sealOnce.Do(func() { close(g.sealStarted) })
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return g.observation, g.sealErr
	}
	g.observation, g.sealErr = g.ProcessGroup.Seal()
	g.sealed = true
	close(g.sealReturned)
	return g.observation, g.sealErr
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func trackedReaper(t *testing.T, r *Runner, process string) *trackedReap {
	t.Helper()
	r.reapersMu.Lock()
	defer r.reapersMu.Unlock()
	for _, tracked := range r.reapers {
		if tracked.process == process {
			return tracked
		}
	}
	t.Fatalf("no reaper registered for %q", process)
	return nil
}

func assertRunnerQuiescent(t *testing.T, r *Runner, state []ipc.ProcState, process config.Process, verdict, output, journal string, sequence uint64) {
	t.Helper()
	if got := r.store.Load().snapshot(); !reflect.DeepEqual(got, state) {
		t.Fatalf("runner state changed after handoff: before=%+v after=%+v", state, got)
	}
	if got := r.Config.Processes["api"]; !reflect.DeepEqual(got, process) {
		t.Fatalf("config changed after handoff: before=%+v after=%+v", process, got)
	}
	if got := r.ShutdownResult(); (got == nil) != (verdict == "") || (got != nil && got.Error() != verdict) {
		t.Fatalf("shutdown verdict changed after handoff: before=%q after=%v", verdict, got)
	}
	if got := r.Output.(*synchronizedBuffer).String(); got != output {
		t.Fatalf("runner output changed after handoff: before=%q after=%q", output, got)
	}
	if got := r.Journal.(*synchronizedBuffer).String(); got != journal {
		t.Fatalf("runner journal changed after handoff: before=%q after=%q", journal, got)
	}
	r.logMu.Lock()
	gotSequence := r.sequence
	r.logMu.Unlock()
	if gotSequence != sequence {
		t.Fatalf("runner event sequence changed after handoff: before=%d after=%d", sequence, gotSequence)
	}
}

// TestRunnerCleanupHandoff_DeadlineReadWhenStackStopsItself pins that
// CleanupHandoffDeadline is read when Run observes cancellation, including one
// the stack caused itself (a failed task), so a caller such as verify never
// writes the handoff budget while Run may be reading it. The deadline, not the
// much longer CleanupHandoffTimeout, bounds the handoff.
func TestRunnerCleanupHandoff_DeadlineReadWhenStackStopsItself(t *testing.T) {
	dir := t.TempDir()
	apiPID, trigger := filepath.Join(dir, "api.pid"), filepath.Join(dir, "fail")
	apiCmd := serviceHelperCmd("pid-block", apiPID)
	r := makeRunner(map[string]config.Process{
		"api":  {Cmd: apiCmd, ShutdownTimeout: shutdownGraceForTest},
		"seed": {Cmd: helperCmd("fail-after-file", trigger, "30s"), Mode: config.ProcessModeTask},
	})
	r.Output = io.Discard
	r.CleanupHandoffTimeout = 30 * time.Second
	var calls atomic.Int32
	r.CleanupHandoffDeadline = func() time.Time {
		calls.Add(1)
		return time.Now().Add(300 * time.Millisecond)
	}
	reapRelease := make(chan struct{})
	releaseReap := sync.OnceFunc(func() { close(reapRelease) })
	t.Cleanup(releaseReap)
	r.waitProcess = func(cmd *exec.Cmd) error {
		if strings.Contains(strings.Join(cmd.Args, " "), apiCmd) {
			<-reapRelease
		}
		return cmd.Wait()
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	pid, ok := waitForPIDFile(t, apiPID, 10*time.Second)
	if !ok {
		releaseReap()
		t.Fatal("api never started")
	}
	trackProcessForCleanup(t, pid, "api")
	if err := os.WriteFile(trigger, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		releaseReap()
		t.Fatal("Run waited out CleanupHandoffTimeout instead of the handoff deadline")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("handoff took %s, want it bounded by the deadline", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("CleanupHandoffDeadline called %d times, want once when cancellation was observed", got)
	}
	// The seal ended api although its reap is still held.
	assertTerminated(t, pid, "api", 2*time.Second)
	releaseReap()
}

func TestRunnerCleanupHandoff_SealsBeforeReturningAndQuiescesWorker(t *testing.T) {
	command := serviceHelperCmd("sleep", "120s")
	configPath := filepath.Join(t.TempDir(), "proc-compose.yml")
	writeConfig := func(command string) {
		t.Helper()
		body := "processes:\n  api:\n    cmd: " + strconv.Quote(command) + "\n    restart: never\n"
		if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(command)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	output, journal := &synchronizedBuffer{}, &synchronizedBuffer{}
	r := &Runner{Config: cfg, ConfigPath: configPath}
	r.NoColor = true
	r.Silent = true
	r.Output = output
	r.Journal = journal
	r.CleanupHandoffTimeout = 250 * time.Millisecond
	group := &signalSealGateProcessGroup{
		ProcessGroup:  newProcessGroup(),
		signalStarted: make(chan struct{}),
		signalRelease: make(chan struct{}),
		sealStarted:   make(chan struct{}),
		sealReturned:  make(chan struct{}),
	}
	r.processGroupFactory = func() ProcessGroup { return group }
	waitStarted, reapRelease := make(chan struct{}), make(chan struct{})
	r.waitProcess = func(cmd *exec.Cmd) error {
		close(waitStarted)
		<-reapRelease
		return cmd.Wait()
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = r.Run(ctx)
		close(runDone)
	}()
	signalReleased := sync.OnceFunc(func() { close(group.signalRelease) })
	reapReleased := sync.OnceFunc(func() { close(reapRelease) })
	t.Cleanup(func() {
		signalReleased()
		reapReleased()
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			if group.cmd != nil && group.cmd.Process != nil {
				_ = group.cmd.Process.Kill()
			}
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Error("runner did not quiesce after emergency owned-handle kill")
			}
		}
		if group.cmd != nil && group.cmd.Process != nil && pidIsRunning(group.cmd.Process.Pid) {
			t.Errorf("handoff fixture process %d survived cleanup", group.cmd.Process.Pid)
		}
	})

	if !waitForProcState(t, r, "api", func(state ipc.ProcState) bool { return state.State == "running" && state.PID > 0 }, 10*time.Second) {
		t.Fatal("production process did not reach running state")
	}
	pid := snapshotOf(r, "api").PID
	cancel()
	select {
	case <-group.signalStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("production cancellation did not enter the delayed signal")
	}
	select {
	case <-group.sealStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded production Run did not reach group sealing")
	}
	select {
	case <-group.sealReturned:
		t.Fatal("seal returned before the delayed in-flight signal completed")
	default:
	}
	signalReleased()
	select {
	case <-group.sealReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("seal did not complete after the delayed signal was released")
	}
	select {
	case <-waitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("production lifecycle did not enter its tracked reaper")
	}
	if result := r.requestRestart(r.store.Load().get("api")); result.Status != "error" {
		t.Fatalf("restart after seal = %+v, want admission refusal", result)
	}
	select {
	case <-runDone:
		if runErr == nil || !strings.Contains(runErr.Error(), "not reaped") {
			t.Fatalf("Runner.Run result = %v, want unverified cleanup failure", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runner.Run remained blocked by the production reaper")
	}
	tracked := trackedReaper(t, r, "api")
	if tracked.reap.taken() {
		t.Fatal("reaper unexpectedly completed before its gate was released")
	}
	if err := group.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("signal after seal = %v, want os.ErrProcessDone", err)
	}
	group.mu.Lock()
	if !group.sealed || group.delegateSignals == 0 {
		group.mu.Unlock()
		t.Fatalf("signal/seal observation: sealed=%t delegated=%d", group.sealed, group.delegateSignals)
	}
	group.mu.Unlock()

	stateBefore := r.store.Load().snapshot()
	processBefore := r.Config.Processes["api"]
	verdictBefore := r.ShutdownResult().Error()
	outputBefore, journalBefore := output.String(), journal.String()
	r.logMu.Lock()
	sequenceBefore := r.sequence
	r.logMu.Unlock()
	reapReleased()
	select {
	case <-tracked.reap.done:
	case <-time.After(5 * time.Second):
		t.Fatal("released production reaper did not finish and get joined")
	}
	assertTerminated(t, pid, "handoff fixture leader", 5*time.Second)
	assertRunnerQuiescent(t, r, stateBefore, processBefore, verdictBefore, outputBefore, journalBefore, sequenceBefore)
	writeConfig(serviceHelperCmd("sleep", "121s"))
	if result := r.Reload(); result.Status != "error" {
		t.Fatalf("reload after bounded Run = %+v, want refusal", result)
	}
	assertRunnerQuiescent(t, r, stateBefore, processBefore, verdictBefore, outputBefore, journalBefore, sequenceBefore)
}

// TestRunCheck_StackShutdownQuiescesActiveCheck is the independent-review
// reproduction: stopping the stack while a check runs must cancel and reap the
// check before Run returns, and nothing may be emitted after Run returns.
func TestRunCheck_StackShutdownQuiescesActiveCheck(t *testing.T) {
	r := makeRunner(map[string]config.Process{"api": {Cmd: helperCmd("sleep", "60s"), Restart: "never"}})
	r.Output = io.Discard
	r.NoColor, r.Silent = true, true
	r.CleanupHandoffTimeout = 250 * time.Millisecond
	trackRelease, terminateRelease := make(chan struct{}), make(chan struct{})
	close(trackRelease)
	close(terminateRelease)
	checkGroup := &admissionGateProcessGroup{
		ProcessGroup: newProcessGroup(), trackStarted: make(chan struct{}), trackRelease: trackRelease,
		terminateStarted: make(chan struct{}), terminateRelease: terminateRelease, closed: make(chan struct{}),
	}
	created := 0
	r.processGroupFactory = func() ProcessGroup {
		created++
		if created == 2 {
			return checkGroup
		}
		return newProcessGroup()
	}
	stackCtx, cancelStack := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = r.Run(stackCtx); close(runDone) }()
	checkCtx, cancelCheck := context.WithCancel(context.Background())
	checkDone := make(chan struct{})
	checkLaunched := false
	t.Cleanup(func() {
		cancelCheck()
		select {
		case <-checkGroup.trackStarted:
			if checkGroup.cmd != nil && checkGroup.cmd.Process != nil {
				_ = checkGroup.cmd.Process.Kill()
			}
		default:
		}
		if checkLaunched {
			select {
			case <-checkDone:
			case <-time.After(10 * time.Second):
				t.Error("check cleanup did not finish")
			}
		}
		cancelStack()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("stack cleanup did not finish")
		}
		_ = checkGroup.Close()
	})
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Ready }, 10*time.Second) {
		t.Fatal("stack did not become ready")
	}
	checkLaunched = true
	var result CheckResult
	go func() {
		result = r.RunCheck(checkCtx, CheckSpec{Name: "verify:check", Argv: helperArgv("sleep", "60s"), Cleanup: time.Second, MaxOutputBytes: 4096})
		close(checkDone)
	}()
	select {
	case <-checkGroup.trackStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("check did not start")
	}
	cancelStack()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("stack shutdown did not return")
	}
	select {
	case <-checkDone:
	default:
		t.Fatal("Runner.Run returned before its active check finished")
	}
	if pidIsRunning(checkGroup.cmd.Process.Pid) {
		t.Errorf("Runner.Run returned with its owned check still alive (PID %d)", checkGroup.cmd.Process.Pid)
	}
	if !result.Started || (result.ExitCode != nil && *result.ExitCode == 0) {
		t.Fatalf("check cancelled by stack shutdown = %+v", result)
	}
	r.logMu.Lock()
	before := r.sequence
	r.logMu.Unlock()
	time.Sleep(100 * time.Millisecond)
	r.logMu.Lock()
	after := r.sequence
	r.logMu.Unlock()
	if after != before {
		t.Errorf("runner emitted events after Run returned: sequence %d -> %d", before, after)
	}
	if late := r.RunCheck(context.Background(), CheckSpec{Name: "verify:late", Argv: helperArgv("exit", "0")}); !late.AdmissionRefused || late.Started {
		t.Fatalf("check admitted after Run returned: %+v", late)
	}
	assertTerminated(t, checkGroup.cmd.Process.Pid, "check", 5*time.Second)
}

// TestRunCheck_StackHandoffBoundsBlockedCheckReap holds the check's reap so
// teardown cannot finish on its own. The stack's handoff budget, not the
// check's own (much longer) deadline, must end the wait: Run returns within
// that one budget and reports the unreaped check instead of success.
func TestRunCheck_StackHandoffBoundsBlockedCheckReap(t *testing.T) {
	r := makeRunner(map[string]config.Process{"api": {Cmd: helperCmd("sleep", "60s"), Restart: "never"}})
	r.Output = io.Discard
	r.NoColor, r.Silent = true, true
	r.CleanupHandoffTimeout = 300 * time.Millisecond
	reapStarted, reapRelease := make(chan struct{}), make(chan struct{})
	startedOnce, releaseOnce := sync.OnceFunc(func() { close(reapStarted) }), sync.OnceFunc(func() { close(reapRelease) })
	r.waitProcess = func(cmd *exec.Cmd) error {
		if len(cmd.Args) > 0 && cmd.Args[0] == os.Args[0] {
			startedOnce()
			<-reapRelease
		}
		return cmd.Wait()
	}
	stackCtx, cancelStack := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = r.Run(stackCtx); close(runDone) }()
	t.Cleanup(func() {
		releaseOnce()
		cancelStack()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("stack cleanup did not finish")
		}
	})
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.Ready }, 10*time.Second) {
		t.Fatal("stack did not become ready")
	}
	var deadlineCalls atomic.Int32
	checkDone := make(chan CheckResult, 1)
	go func() {
		checkDone <- r.RunCheck(context.Background(), CheckSpec{Name: "verify:check", Argv: helperArgv("sleep", "60s"),
			CleanupDeadline: func() time.Time { deadlineCalls.Add(1); return time.Now().Add(time.Minute) }})
	}()
	if !waitForCheckRunning(r, "verify:check", 10*time.Second) {
		t.Fatal("check did not start")
	}
	started := time.Now()
	cancelStack()
	select {
	case <-reapStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("check teardown did not reach its reap")
	}
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stack handoff did not end the blocked check wait")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("blocked check held Run for %s despite a 300ms handoff", elapsed)
	}
	result := <-checkDone
	if !result.HandedOff || result.ExitCode != nil {
		t.Fatalf("blocked check = %+v, want handoff without exit status", result)
	}
	if deadlineCalls.Load() != 1 {
		t.Fatalf("CleanupDeadline called %d times, want once at the trigger", deadlineCalls.Load())
	}
	found := false
	for _, u := range r.Unresolved() {
		found = found || u.Name == "verify:check"
	}
	if !found || r.ShutdownResult() == nil {
		t.Fatalf("unreaped check not reported: unresolved=%+v verdict=%v", r.Unresolved(), r.ShutdownResult())
	}
	releaseOnce()
	assertTerminated(t, result.PID, "handed-off check", 5*time.Second)
}

func waitForCheckRunning(r *Runner, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := r.stateFor(name); st != nil {
			st.mu.Lock()
			running := st.pid != 0
			st.mu.Unlock()
			if running {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

type failedTrackProcessGroup struct {
	ProcessGroup
	cmd    *exec.Cmd
	sealed chan struct{}
}

func (g *failedTrackProcessGroup) Track(cmd *exec.Cmd) error {
	g.cmd = cmd
	return errors.New("controlled Track failure")
}

func (g *failedTrackProcessGroup) Seal() (SealObservation, error) {
	observation, err := g.ProcessGroup.Seal()
	close(g.sealed)
	return observation, err
}

// The admission tests below hold or fail Track after cmd.Start. They run on every
// platform: on Windows the command is created suspended and runs only once Track
// has placed it in its Job Object, so nothing it spawns can escape while Track is
// held or after it fails.
func TestRunnerAdmission_TrackFailureKillsAndReapsStartedProcess(t *testing.T) {
	command := serviceHelperCmd("sleep", "120s")
	r := makeRunner(map[string]config.Process{"api": {Cmd: command, Restart: "never"}})
	group := &failedTrackProcessGroup{ProcessGroup: newProcessGroup(), sealed: make(chan struct{})}
	r.processGroupFactory = func() ProcessGroup { return group }
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = r.Run(context.Background())
		close(runDone)
	}()
	t.Cleanup(func() {
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			if group.cmd != nil && group.cmd.Process != nil {
				_ = group.cmd.Process.Kill()
			}
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Error("runner remained blocked after killing the owned process handle")
			}
		}
		if group.cmd != nil && group.cmd.Process != nil && pidIsRunning(group.cmd.Process.Pid) {
			t.Errorf("failed-admission process %d survived test cleanup", group.cmd.Process.Pid)
		}
	})

	select {
	case <-runDone:
		if runErr == nil || !strings.Contains(runErr.Error(), "api") {
			t.Fatalf("Runner.Run result = %v, want failed process result", runErr)
		}
	case <-time.After(10 * time.Second):
		if group.cmd != nil && group.cmd.Process != nil {
			_ = group.cmd.Process.Kill()
		}
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Runner.Run did not finish cleanup after Track failure")
		}
		t.Fatalf("Runner.Run exceeded cleanup wait after Track failure: %v", runErr)
	}
	if group.cmd == nil || group.cmd.Process == nil {
		t.Fatal("Track failure did not observe the already-started process")
	}
	select {
	case <-group.sealed:
	case <-time.After(time.Second):
		t.Fatal("process-group resources were not sealed after Track failure")
	}
	assertTerminated(t, group.cmd.Process.Pid, "failed-admission process", 5*time.Second)
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	if len(r.groups) != 0 {
		t.Fatalf("failed admission left registered groups: %v", r.groups)
	}
}

func TestRunnerAdmission_CancelBetweenStartAndRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	command := serviceHelperCmd("sleep", "120s")
	writeConfig := func(command string) {
		t.Helper()
		body := "processes:\n  api:\n    cmd: " + strconv.Quote(command) + "\n    restart: never\n"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(command)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: cfg, ConfigPath: path, NoColor: true, Silent: true}
	group := &admissionGateProcessGroup{
		trackStarted:     make(chan struct{}),
		trackRelease:     make(chan struct{}),
		terminateStarted: make(chan struct{}),
		terminateRelease: make(chan struct{}),
		closed:           make(chan struct{}),
	}
	r.processGroupFactory = func() ProcessGroup {
		group.ProcessGroup = newProcessGroup()
		return group
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = r.Run(ctx)
		close(runDone)
	}()
	trackReleased := sync.OnceFunc(func() { close(group.trackRelease) })
	terminateReleased := sync.OnceFunc(func() { close(group.terminateRelease) })
	t.Cleanup(func() {
		trackReleased()
		terminateReleased()
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("runner did not finish after releasing admission gates; killing owned process handle")
			if group.cmd != nil && group.cmd.Process != nil {
				_ = group.cmd.Process.Kill()
			}
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Error("runner remained blocked after killing its owned process handle")
			}
		}
		if group.pid > 0 && pidIsRunning(group.pid) {
			t.Errorf("admission fixture process %d survived test cleanup", group.pid)
		}
	})

	select {
	case <-group.trackStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("production Runner.Run did not reach process-group tracking")
	}
	pid := group.pid
	cancel() // context cancellation lands after cmd.Start and before Track.
	select {
	case <-runDone:
		t.Fatalf("Runner.Run returned before the started process was registered: %v", runErr)
	case <-time.After(25 * time.Millisecond):
	}
	// Queue both replacement paths while the launch still holds lifecycleMu.
	// Once Track is released, either request may acquire the lock before the
	// cancellation watcher; runCtx.Err must still refuse it without mutation.
	writeConfig(serviceHelperCmd("sleep", "121s"))
	restartResult := make(chan ipc.CommandResult, 1)
	go func() { restartResult <- r.requestRestart(r.store.Load().get("api")) }()
	reloadResult := make(chan ipc.CommandResult, 1)
	go func() { reloadResult <- r.Reload() }()
	trackReleased()

	select {
	case <-group.terminateStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("registered process did not enter production teardown")
	}
	r.groupsMu.Lock()
	registered := r.groups["api"] == group
	r.groupsMu.Unlock()
	if !registered {
		t.Fatal("started process was not visible in the cleanup registry before teardown")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !r.stopping.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.stopping.Load() {
		t.Fatal("cancellation did not close launch admission")
	}
	select {
	case result := <-restartResult:
		if result.Status != "error" {
			t.Fatalf("restart racing cancellation = %+v, want refusal", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart request remained blocked after launch admission ended")
	}
	select {
	case result := <-reloadResult:
		if result.Status != "error" {
			t.Fatalf("reload racing cancellation = %+v, want refusal", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload remained blocked after launch admission ended")
	}
	if got := r.Config.Processes["api"].Cmd; got != command {
		t.Fatalf("reload changed command after admission closed: %q", got)
	}

	stopped, _, err := r.Shutdown(false)
	if err != nil {
		t.Fatalf("Shutdown after parent cancellation: %v", err)
	}
	terminateReleased()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown did not observe production cleanup completion")
	}
	select {
	case <-runDone:
		if runErr != nil {
			t.Fatalf("Runner.Run after cancellation: %v", runErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Runner.Run did not finish after production teardown was released")
	}
	select {
	case <-group.closed:
	case <-time.After(time.Second):
		t.Fatal("registered process group was not closed by production cleanup")
	}
	assertTerminated(t, pid, "admitted process", 5*time.Second)
}

func TestRunnerCleanupHandoff_FailedTrackDoesNotHoldAdmissionOrRun(t *testing.T) {
	proc := config.Process{Cmd: serviceHelperCmd("sleep", "120s"), Restart: "never"}
	output, journal := &synchronizedBuffer{}, &synchronizedBuffer{}
	r := makeRunner(map[string]config.Process{"api": proc})
	r.NoColor = true
	r.Silent = true
	r.Output = output
	r.Journal = journal
	r.CleanupHandoffTimeout = 200 * time.Millisecond
	group := &failedTrackProcessGroup{ProcessGroup: newProcessGroup(), sealed: make(chan struct{})}
	r.processGroupFactory = func() ProcessGroup { return group }
	waitStarted, reapRelease := make(chan struct{}), make(chan struct{})
	r.waitProcess = func(cmd *exec.Cmd) error {
		close(waitStarted)
		<-reapRelease
		return cmd.Wait()
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = r.Run(ctx)
		close(runDone)
	}()
	reapReleased := sync.OnceFunc(func() { close(reapRelease) })
	t.Cleanup(func() {
		reapReleased()
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			if group.cmd != nil && group.cmd.Process != nil {
				_ = group.cmd.Process.Kill()
			}
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Error("runner did not finish after emergency owned-handle kill")
			}
		}
		if group.cmd != nil && group.cmd.Process != nil && pidIsRunning(group.cmd.Process.Pid) {
			t.Errorf("failed-track fixture process %d survived cleanup", group.cmd.Process.Pid)
		}
	})

	select {
	case <-waitStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("failed Track did not start its tracked reap")
	}
	select {
	case <-group.sealed:
	case <-time.After(time.Second):
		t.Fatal("failed Track did not seal its unregistered group")
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for !r.stopping.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.stopping.Load() {
		t.Fatal("admission remained blocked behind the unresolved failed-track reap")
	}
	select {
	case <-runDone:
		if runErr == nil || !strings.Contains(runErr.Error(), "not reaped") {
			t.Fatalf("Runner.Run result = %v, want unverified failed-track reap", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runner.Run remained blocked by failed-track cleanup wait")
	}
	if result := r.requestRestart(r.store.Load().get("api")); result.Status != "error" {
		t.Fatalf("restart after failed-track handoff = %+v, want refusal", result)
	}
	tracked := trackedReaper(t, r, "api")
	if tracked.reap.taken() {
		t.Fatal("failed-track reaper unexpectedly completed before release")
	}
	stateBefore := r.store.Load().snapshot()
	processBefore := r.Config.Processes["api"]
	verdictBefore := r.ShutdownResult().Error()
	outputBefore, journalBefore := output.String(), journal.String()
	r.logMu.Lock()
	sequenceBefore := r.sequence
	r.logMu.Unlock()
	reapReleased()
	select {
	case <-tracked.reap.done:
	case <-time.After(5 * time.Second):
		t.Fatal("failed-track reaper did not finish after its gate was released")
	}
	if group.cmd == nil || group.cmd.Process == nil {
		t.Fatal("failed Track did not retain the started process handle for cleanup proof")
	}
	assertTerminated(t, group.cmd.Process.Pid, "failed-track leader", 5*time.Second)
	assertRunnerQuiescent(t, r, stateBefore, processBefore, verdictBefore, outputBefore, journalBefore, sequenceBefore)
}

// TestFormatRecovery_StableEvidenceLines pins the evidence shape stop and up
// print for a crashed run: one key=value line per group, sorted by name and
// then pgid, with empty fields left out.
func TestFormatRecovery_StableEvidenceLines(t *testing.T) {
	groups := []RecoveredGroup{
		{GroupResult: GroupResult{Name: "web", PGID: 912}, LeaderStart: 51301, Outcome: OutcomeTerminated, Reason: "leader_proven", Signals: []string{"SIGTERM"}, PIDs: []int{912, 915}},
		{GroupResult: GroupResult{Name: "detached-child", PGID: 944}, Outcome: OutcomeUnknown, Reason: "leader_gone", LiveMembers: 1, PIDs: []int{950}},
		{GroupResult: GroupResult{Name: "api", PGID: 0}, Outcome: OutcomeUnknown, Reason: "launch_pending"},
		{GroupResult: GroupResult{Name: "web", PGID: 13}, Outcome: OutcomeEmpty, Reason: "no_members"},
		{GroupResult: GroupResult{Name: "stubborn", PGID: 930}, LeaderStart: 51340, Outcome: OutcomeTerminated, Reason: "leader_proven", Signals: []string{"SIGTERM", "SIGKILL"}, PIDs: []int{930, 931}},
		{GroupResult: GroupResult{Name: "worker", PGID: 960}, LeaderStart: 51400, Outcome: OutcomeUnknown, Reason: "boot_unknown"},
	}
	want := "recovered group=api pgid=0 outcome=unknown reason=launch_pending\n" +
		"recovered group=detached-child pgid=944 outcome=unknown reason=leader_gone live_members=1 members=950\n" +
		"recovered group=stubborn pgid=930 leader_start=51340 outcome=terminated reason=leader_proven signals=SIGTERM,SIGKILL members=930,931\n" +
		"recovered group=web pgid=13 outcome=empty reason=no_members\n" +
		"recovered group=web pgid=912 leader_start=51301 outcome=terminated reason=leader_proven signals=SIGTERM members=912,915\n" +
		"recovered group=worker pgid=960 leader_start=51400 outcome=unknown reason=boot_unknown"
	if got := FormatRecovery(groups); got != want {
		t.Fatalf("FormatRecovery =\n%s\nwant\n%s", got, want)
	}
	if got := FormatRecovery([]RecoveredGroup{{GroupResult: GroupResult{Name: "my api", PGID: 7}, Outcome: OutcomeEmpty, Reason: "no_members"}}); got != `recovered group="my api" pgid=7 outcome=empty reason=no_members` {
		t.Fatalf("a name with a space must stay one field: %s", got)
	}
}
