package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/runner"
	"github.com/anivaryam/proc-compose/internal/session"
)

const helperEnv = "PROC_COMPOSE_VERIFY_HELPER"

// TestMain doubles as the service and check fixture so tests run real
// processes without depending on tools installed on the host.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helper(mode, os.Args[len(os.Args)-1]))
	}
	os.Exit(m.Run())
}

func helper(mode, arg string) int {
	switch mode {
	case "serve":
		ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		_ = os.WriteFile(arg, []byte(strconv.Itoa(os.Getpid())), 0600)
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
		return 0
	case "get":
		resp, err := http.Get(os.Getenv("PROC_COMPOSE_URL_API_HTTP"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 4
		}
		resp.Body.Close()
		fmt.Printf("argv=%s port=%s run=%s\n", arg, os.Getenv("PROC_COMPOSE_PORT_API_HTTP"), os.Getenv("PROC_COMPOSE_RUN_ID"))
		return 0
	case "exit":
		code, _ := strconv.Atoi(arg)
		fmt.Println("exiting", code)
		return code
	case "sleep":
		d, _ := time.ParseDuration(arg)
		time.Sleep(d)
		return 0
	case "report":
		if err := os.WriteFile(os.Getenv("PROC_COMPOSE_ENVIRONMENT_REPORT"), []byte(arg), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 5
		}
		return 0
	}
	return 99
}

type fixture struct {
	dir, config, pidFile string
}

func newFixture(t *testing.T, body string) fixture {
	t.Helper()
	// The session socket lives in the runtime dir. t.TempDir embeds the test
	// name, and on macOS that path exceeds the 104-byte socket limit, so verify
	// would run without IPC there. A short directory keeps every platform on
	// the same path; the IPC-less mode is covered by the runner's own tests.
	runtimeDir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	dir := t.TempDir()
	f := fixture{dir: dir, config: filepath.Join(dir, "proc-compose.yml"), pidFile: filepath.Join(dir, "api.pid")}
	body = strings.ReplaceAll(body, "{{BIN}}", os.Args[0])
	body = strings.ReplaceAll(body, "{{PID}}", f.pidFile)
	if err := os.WriteFile(f.config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

const serviceConfig = `processes:
  api:
    cmd: {{BIN}} {{PID}}
    env:
      ` + helperEnv + `: serve
    ports:
      http: 0
    ready_when:
      http: ${urls.api.http}/
`

func (f fixture) options(t *testing.T, sessionName string, argv ...string) Options {
	t.Helper()
	hash, err := paths.FromConfig(f.config, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		ConfigPath: f.config, Session: sessionName, Hash: hash, Argv: argv, ToolVersion: "test",
		StartupTimeout: 20 * time.Second, CheckTimeout: 20 * time.Second, CleanupTimeout: 30 * time.Second,
	}
}

func checkArgv(mode, arg string) []string {
	return []string{os.Args[0], "-test.run=^$", arg}
}

func withHelper(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(helperEnv, mode)
}

func assertComplete(t *testing.T, result *Result) map[string]any {
	t.Helper()
	data, compact, err := Encode(result)
	if err != nil || compact {
		t.Fatalf("encode: compact=%t err=%v", compact, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "kind", "state", "attempt_id", "status", "phase", "session", "run_id", "config_path", "config_sha256", "workdir", "source", "tool_version", "selected_processes", "endpoints", "startup", "check", "environment", "cleanup", "evidence", "errors", "exit_code", "precedence", "receipt_path", "receipt_persisted", "retention", "started_at", "finished_at"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("result omits %q", key)
		}
	}
	return fields
}

func pidFromFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("service pid: %v", err)
	}
	pid, _ := strconv.Atoi(string(data))
	return pid
}

// recordedGroupFor pins verify's wiring of Runner.RecordPath: the attempt's
// PID file must record a tracked process group for name. On Linux the recorded
// pgid must be the group of the pid the service wrote to servicePIDFile. It
// returns an error instead of failing because the afterStartup seam is not
// guaranteed to run on the test goroutine. Windows records no groups.
func recordedGroupFor(pidPath, name, servicePIDFile string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := daemon.ReadRecord(pidPath)
		if err != nil {
			return err
		}
		var tracked *daemon.Group
		if rec.Groups != nil {
			for i, g := range *rec.Groups {
				if g.Name == name && g.PGID > 0 {
					tracked = &(*rec.Groups)[i]
				}
			}
		}
		if tracked != nil {
			if runtime.GOOS != "linux" {
				return nil
			}
			data, err := os.ReadFile(servicePIDFile)
			if err != nil {
				return err
			}
			pid, _ := strconv.Atoi(string(data))
			info, err := daemon.ProcStat(pid)
			if err != nil {
				return fmt.Errorf("ProcStat(%d): %w", pid, err)
			}
			if tracked.PGID != info.PGID || tracked.LeaderStart == 0 {
				return fmt.Errorf("recorded %+v, want pgid %d with a leader start time", *tracked, info.PGID)
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s records no tracked group %q: %+v (groups %v)", pidPath, name, rec, rec.Groups)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for daemon.IsAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if daemon.IsAlive(pid) {
		t.Fatalf("process %d survived verified cleanup", pid)
	}
}

func TestRun_HappyPathLiteralArgvEndpointsAndReceipt(t *testing.T) {
	f := newFixture(t, serviceConfig)
	t.Setenv(helperEnv, "get")
	literal := `two words "quoted" $HOME;|&`
	opts := f.options(t, "happy", os.Args[0], "-test.run=^$", literal)
	var recordErr error
	opts.afterStartup = func(*runner.Runner) { recordErr = recordedGroupFor(paths.PID(opts.Hash), "api", f.pidFile) }
	// The service is told its mode through config env; the check inherits the
	// caller's environment, which carries the check mode.
	result := Run(context.Background(), opts)
	if recordErr != nil {
		t.Fatalf("process-group record: %v", recordErr)
	}
	if result.Status != StatusPassed || result.ExitCode != 0 {
		t.Fatalf("result = %s/%d errors=%v stderr=%q", result.Status, result.ExitCode, result.Errors, result.Check.Stderr)
	}
	assertComplete(t, result)
	endpoint := result.Endpoints["api.http"]
	want := fmt.Sprintf("argv=%s port=%d run=%s\n", literal, endpoint.Port, *result.RunID)
	if result.Check.Stdout != want {
		t.Fatalf("check stdout = %q, want %q", result.Check.Stdout, want)
	}
	if result.Kind != KindResponse || !result.ReceiptPersisted || result.Cleanup.Status != StatusPassed || result.Cleanup.SignalingAuthority != "withdrawn" {
		t.Fatalf("result envelope = %+v", result)
	}
	if obs := result.Startup.ReadinessObservations; len(obs) != 1 || obs[0].Status != StatusPassed || *obs[0].Probe != "http" || obs[0].Generation != 1 {
		t.Fatalf("readiness = %+v", obs)
	}
	receipt, err := ReadReceipt(*result.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != StateCompleted || receipt.Status != StatusPassed || receipt.Check.Stdout != want {
		t.Fatalf("receipt = %+v", receipt)
	}
	if result.Evidence.FirstCursor == nil || !strings.HasPrefix(*result.Evidence.FirstCursor, *result.RunID+":") {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	assertGone(t, pidFromFile(t, f.pidFile))
	// The lease was released: the same session can be acquired again.
	held, err := session.InstanceHeld(opts.Hash)
	if err != nil || held {
		t.Fatalf("instance lease after attempt held=%t err=%v", held, err)
	}
}

// TestProbeKind_NamesEveryReadinessKind keeps the receipt's probe field in step
// with the probe kinds config accepts: a cmd probe is a probe, not unprobed.
func TestProbeKind_NamesEveryReadinessKind(t *testing.T) {
	for want, rw := range map[string]*config.ReadyWhen{
		"http": {HTTP: "http://localhost:1/"},
		"tcp":  {TCP: "localhost:1"},
		"cmd":  {Cmd: "check"},
		"log":  {Log: "ready"},
	} {
		if got := probeKind(config.Process{Cmd: "serve", ReadyWhen: rw}); got == nil || *got != want {
			t.Errorf("probeKind(%+v) = %v, want %q", rw, got, want)
		}
	}
}

func TestRun_UnprobedServiceIsDistinguishedFromProbeReadiness(t *testing.T) {
	f := newFixture(t, `processes:
  api:
    cmd: {{BIN}} {{PID}}
    env:
      `+helperEnv+`: serve
    ports:
      http: 0
`)
	withHelper(t, "exit")
	result := Run(context.Background(), f.options(t, "", checkArgv("exit", "0")...))
	if result.Status != StatusPassed {
		t.Fatalf("status = %s errors=%v", result.Status, result.Errors)
	}
	obs := result.Startup.ReadinessObservations
	if len(obs) != 1 || obs[0].Probe != nil || obs[0].Status != StatusNotConfig || len(result.Startup.UnprobedProcesses) != 1 {
		t.Fatalf("unprobed readiness = %+v / %v", obs, result.Startup.UnprobedProcesses)
	}
}

func TestRun_PreAcquisitionFailuresAreStructuredWithoutReceipts(t *testing.T) {
	f := newFixture(t, "processes:\n  api:\n    depends_on: [missing]\n")
	result := Run(context.Background(), f.options(t, "bad", "true"))
	fields := assertComplete(t, result)
	if result.ExitCode != ExitUsage || result.Phase != PhaseValidation || result.Startup.Validation != StatusFailed || result.Errors[0].Code != CodeInvalidConfig {
		t.Fatalf("invalid config = %+v", result)
	}
	if fields["attempt_id"] != nil || fields["run_id"] != nil || result.ReceiptPath != nil || result.Check.Status != StatusNotRun || result.Cleanup.Status != StatusNotNeeded {
		t.Fatalf("pre-acquisition identity/phases = %v", fields)
	}
	if result.Check.ArgvSHA256 == nil || result.Check.ArgvBytes != len(`["true"]`) {
		t.Fatalf("accepted argv digest = %v/%d", result.Check.ArgvSHA256, result.Check.ArgvBytes)
	}
	if _, err := os.Stat(paths.SessionDir(f.options(t, "bad").Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-acquisition failure created session state: %v", err)
	}

	good := newFixture(t, serviceConfig)
	for name, opts := range map[string]Options{
		"empty argv":     good.options(t, "x"),
		"unknown select": func() Options { o := good.options(t, "x", "true"); o.Selectors = []string{"nope"}; return o }(),
		"bad dir":        func() Options { o := good.options(t, "x", "true"); o.Dir = "missing"; return o }(),
		"short cleanup":  func() Options { o := good.options(t, "x", "true"); o.CleanupTimeout = time.Second; return o }(),
		"huge argv":      good.options(t, "x", strings.Repeat("\x01", MaxArgvBytes)),
	} {
		result := Run(context.Background(), opts)
		if result.ExitCode != ExitUsage || result.RunID != nil || result.Status != StatusFailed {
			t.Errorf("%s: %+v", name, result)
		}
	}
}

func TestRun_OccupiedSessionIsRefusedWithoutMutation(t *testing.T) {
	f := newFixture(t, serviceConfig)
	opts := f.options(t, "busy", "true")
	cfg, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := session.Acquire(cfg, f.config, opts.Hash, "busy", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := session.Save(paths.Metadata(opts.Hash), holder.Project); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(paths.Metadata(opts.Hash))
	result := Run(context.Background(), opts)
	assertComplete(t, result)
	if result.ExitCode != ExitUsage || result.Phase != PhaseAcquiring || result.Errors[0].Code != CodeSessionOccupied || result.Startup.Validation != StatusPassed || result.ReceiptPath != nil {
		t.Fatalf("occupied = %+v", result)
	}
	after, _ := os.ReadFile(paths.Metadata(opts.Hash))
	if string(before) != string(after) {
		t.Fatal("refusal rewrote the occupant's session metadata")
	}
	if held, _ := session.InstanceHeld(opts.Hash); !held {
		t.Fatal("refusal released the occupant's lease")
	}
}

func TestRun_StartupTimeoutSkipsCheckAndCleansUp(t *testing.T) {
	f := newFixture(t, `processes:
  api:
    cmd: {{BIN}} {{PID}}
    env:
      `+helperEnv+`: serve
    ports:
      http: 0
    ready_when:
      tcp: 127.0.0.1:1
    ready_timeout: -1
`)
	opts := f.options(t, "", "true")
	opts.StartupTimeout = 2 * time.Second
	result := Run(context.Background(), opts)
	if result.Startup.Status != StatusTimedOut || result.Check.Status != StatusNotRun || result.ExitCode != ExitFailed {
		t.Fatalf("startup timeout = startup %s check %s exit %d errors %v", result.Startup.Status, result.Check.Status, result.ExitCode, result.Errors)
	}
	if result.Cleanup.Status != StatusPassed {
		t.Fatalf("cleanup = %+v", result.Cleanup)
	}
	assertGone(t, pidFromFile(t, f.pidFile))

	// A check that never ran reports no environment, and a required one adds
	// no error beyond the startup failure itself.
	opts.RequireEnvironment = true
	result = Run(context.Background(), opts)
	if result.Environment.Status != StatusNotRun || !result.Environment.Required || len(result.Errors) != 1 || result.Errors[0].Code != CodeStartupTimeout {
		t.Fatalf("environment after startup timeout = %+v errors %v", result.Environment, result.Errors)
	}
}

func TestRun_CheckFailuresAndTimeoutKeepPhaseFacts(t *testing.T) {
	f := newFixture(t, serviceConfig)
	withHelper(t, "exit")
	failed := Run(context.Background(), f.options(t, "", checkArgv("exit", "7")...))
	if failed.Check.Status != StatusFailed || *failed.Check.ExitCode != 7 || failed.ExitCode != ExitFailed || failed.Startup.Status != StatusPassed {
		t.Fatalf("nonzero check = %+v", failed.Check)
	}
	if !strings.Contains(failed.Check.Stdout, "exiting 7") || failed.Errors[0].Code != CodeCheckExitNonzero {
		t.Fatalf("nonzero evidence = %q %v", failed.Check.Stdout, failed.Errors)
	}

	withHelper(t, "sleep")
	opts := f.options(t, "", checkArgv("sleep", "60s")...)
	opts.CheckTimeout = time.Second
	started := time.Now()
	timedOut := Run(context.Background(), opts)
	if !timedOut.Check.TimedOut || timedOut.Check.Status != StatusTimedOut || timedOut.ExitCode != ExitFailed || timedOut.Cleanup.Status != StatusPassed {
		t.Fatalf("timeout = check %+v cleanup %+v", timedOut.Check, timedOut.Cleanup)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("timed-out attempt took %s", elapsed)
	}
}

func TestRun_RequiredServiceFailureCannotBeMaskedByCheckSuccess(t *testing.T) {
	f := newFixture(t, `processes:
  api:
    cmd: {{BIN}} 1
    restart: never
    env:
      `+helperEnv+`: exit
`)
	withHelper(t, "sleep")
	result := Run(context.Background(), f.options(t, "", checkArgv("sleep", "1500ms")...))
	if result.Check.Status != StatusPassed || result.Status != StatusFailed {
		// Startup can also catch the failure first; both must fail the attempt.
		if result.Status != StatusFailed {
			t.Fatalf("required failure masked: %+v", result)
		}
	}
	found := false
	for _, e := range result.Errors {
		found = found || e.Code == CodeRequiredFailed || e.Code == CodeStartupFailed
	}
	if !found {
		t.Fatalf("errors = %v", result.Errors)
	}
}

// TestRun_UnhealthyRequiredServiceFailsCheck pins that a required service
// that becomes unhealthy during the check fails the attempt as
// required_failed even though it keeps running and the check passes.
func TestRun_UnhealthyRequiredServiceFailsCheck(t *testing.T) {
	f := newFixture(t, serviceConfig+`    health_when:
      tcp: 127.0.0.1:1
    health_interval: 1
`)
	withHelper(t, "sleep")
	result := Run(context.Background(), f.options(t, "", checkArgv("sleep", "6s")...))
	if result.Check.Status != StatusPassed || result.Status != StatusFailed {
		t.Fatalf("check %s overall %s errors=%v", result.Check.Status, result.Status, result.Errors)
	}
	if len(result.Errors) == 0 || result.Errors[0].Code != CodeRequiredFailed {
		t.Fatalf("errors = %v, want %s", result.Errors, CodeRequiredFailed)
	}
	if result.Cleanup.Status != StatusPassed {
		t.Fatalf("cleanup = %+v", result.Cleanup)
	}
}

func TestRun_RequiredServiceExitDuringCheck(t *testing.T) {
	f := newFixture(t, serviceConfig)
	withHelper(t, "sleep")
	opts := f.options(t, "", checkArgv("sleep", "1s")...)
	opts.afterStartup = func(r *runner.Runner) {
		// Kill the service out from under the stack once the check is admitted.
		go func() {
			time.Sleep(200 * time.Millisecond)
			if proc, err := os.FindProcess(pidFromFile(t, f.pidFile)); err == nil {
				_ = proc.Kill()
			}
		}()
	}
	result := Run(context.Background(), opts)
	if result.Check.Status != StatusPassed || result.Status != StatusFailed {
		t.Fatalf("check %s overall %s", result.Check.Status, result.Status)
	}
	if result.Errors[0].Code != CodeRequiredFailed {
		t.Fatalf("errors = %v", result.Errors)
	}
}

func TestRun_InterruptionDuringCheck(t *testing.T) {
	f := newFixture(t, serviceConfig)
	withHelper(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	opts := f.options(t, "", checkArgv("sleep", "60s")...)
	opts.afterStartup = func(*runner.Runner) { time.AfterFunc(300*time.Millisecond, cancel) }
	result := Run(ctx, opts)
	if result.Status != StatusInterrupted || result.ExitCode != ExitInterrupted || result.Check.Status != StatusInterrupted || result.Precedence != PrecedenceInterrupted {
		t.Fatalf("interrupted = %s/%d check %s", result.Status, result.ExitCode, result.Check.Status)
	}
	if result.Cleanup.Status != StatusPassed || !result.ReceiptPersisted {
		t.Fatalf("cleanup after interrupt = %+v persisted %t", result.Cleanup, result.ReceiptPersisted)
	}
}

func TestRun_TerminalReceiptFailureIsReportedAndSkipsPrune(t *testing.T) {
	f := newFixture(t, serviceConfig)
	// Without a helper mode the test binary runs no tests and exits 0: a
	// portable "true" that does not depend on POSIX tools being on PATH.
	opts := f.options(t, "persist", checkArgv("", "")...)
	writes := 0
	opts.writeReceipt = func(path string, result *Result) error {
		writes++
		if result.State == StateActive {
			return WriteReceipt(path, result)
		}
		return errors.New("disk full")
	}
	result := Run(context.Background(), opts)
	if result.ReceiptPersisted || result.Status != StatusFailed || result.ExitCode != ExitFailed {
		t.Fatalf("persistence failure = %+v", result)
	}
	if result.Errors[0].Code != CodeReceiptPersistence || result.Cleanup.Status != StatusPassed {
		t.Fatalf("errors=%v cleanup=%s", result.Errors, result.Cleanup.Status)
	}
	// The earlier active receipt stays as written; it is not claimed terminal.
	active, err := ReadReceipt(*result.ReceiptPath)
	if err != nil || active.State != StateActive {
		t.Fatalf("active receipt = %+v err %v", active, err)
	}
	if held, _ := session.InstanceHeld(opts.Hash); held {
		t.Fatal("lease was not released after authority withdrawal")
	}
	// Owner is this live process but no longer runs the session: indeterminate,
	// never active, abandoned, or passed.
	state := Classify(active, OwnerEvidence{
		OwnerAlive: func(o Owner) (bool, bool) { return daemon.IsAlive(o.PID), true },
		OwnerRunning: func(o Owner) bool {
			alive, pid := daemon.IsAliveFromPIDFile(paths.PID(opts.Hash))
			return alive && pid == o.PID
		},
		LeaseHeld: func() (bool, error) { return session.InstanceHeld(opts.Hash) },
	})
	if state != StateIndeterminate {
		t.Fatalf("classification = %s", state)
	}
}

// startupBarrier holds verify attempts after readiness, through the afterStartup
// seam, until the controller releases them. If the controller has not released
// them within timeout, the barrier cancels the context shared by every attempt,
// so none of them can pass, and records the timeout for the test to report.
type startupBarrier struct {
	ctx        context.Context
	abort      context.CancelFunc
	timeout    time.Duration
	arrived    chan time.Time
	left       chan time.Time
	release    chan struct{}
	releaseNow func()
	releasedAt atomic.Int64
	timedOut   atomic.Bool
}

func newStartupBarrier(t *testing.T, parties int, timeout time.Duration) *startupBarrier {
	ctx, abort := context.WithCancel(context.Background())
	b := &startupBarrier{ctx: ctx, abort: abort, timeout: timeout,
		arrived: make(chan time.Time, parties), left: make(chan time.Time, parties), release: make(chan struct{})}
	b.releaseNow = sync.OnceFunc(func() {
		b.releasedAt.Store(time.Now().UnixNano())
		close(b.release)
	})
	// Cancelling, not releasing, on cleanup: a held attempt that outlives the
	// test is interrupted rather than let through to its check.
	t.Cleanup(abort)
	return b
}

// hold is the afterStartup seam.
func (b *startupBarrier) hold(*runner.Runner) {
	b.arrived <- time.Now()
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case <-b.release:
	case <-b.ctx.Done():
	case <-timer.C:
		b.timedOut.Store(true)
		b.abort()
	}
	b.left <- time.Now()
}

// collectResults receives up to n results within timeout. On timeout it calls
// abort and returns the results that did arrive, with ok false, so the caller
// fails with partial diagnostics instead of waiting on attempts that may never
// return.
func collectResults(results <-chan *Result, n int, timeout time.Duration, abort func()) ([]*Result, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	got := make([]*Result, 0, n)
	for len(got) < n {
		select {
		case r := <-results:
			got = append(got, r)
		case <-timer.C:
			abort()
			return got, false
		}
	}
	return got, true
}

func TestCollectResults_TimeoutReturnsPartialResults(t *testing.T) {
	for _, arrived := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(arrived), func(t *testing.T) {
			results := make(chan *Result, 2)
			for i := range arrived {
				results <- &Result{Session: strconv.Itoa(i)}
			}
			aborted := 0
			done := make(chan struct{})
			var got []*Result
			var ok bool
			go func() {
				defer close(done)
				got, ok = collectResults(results, 2, 50*time.Millisecond, func() { aborted++ })
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("collectResults did not return with %d of 2 results", arrived)
			}
			complete := arrived == 2
			if ok != complete || len(got) != arrived {
				t.Fatalf("collectResults = %d results, ok %t; want %d, ok %t", len(got), ok, arrived, complete)
			}
			if wantAborts := map[bool]int{true: 0, false: 1}[complete]; aborted != wantAborts {
				t.Fatalf("abort called %d times, want %d", aborted, wantAborts)
			}
		})
	}
}

// TestStartupBarrier_TimeoutCancelsTheAttempt exercises the barrier's timeout
// path through a real attempt: never released, it must be interrupted rather
// than proceed to a passing check.
func TestStartupBarrier_TimeoutCancelsTheAttempt(t *testing.T) {
	f := newFixture(t, serviceConfig)
	withHelper(t, "sleep")
	b := newStartupBarrier(t, 1, 300*time.Millisecond)
	opts := f.options(t, "held", checkArgv("sleep", "10ms")...)
	opts.afterStartup = b.hold
	result := Run(b.ctx, opts)
	if !b.timedOut.Load() {
		t.Fatal("barrier did not record its timeout")
	}
	if result.Status == StatusPassed || result.Check.Status == StatusPassed {
		t.Fatalf("attempt passed after its barrier timed out: status=%s check=%s", result.Status, result.Check.Status)
	}
	if result.Status != StatusInterrupted || result.Cleanup.Status != StatusPassed {
		t.Fatalf("timed-out attempt = %s precedence=%s check=%s/%v errors=%v, cleanup %+v; want interrupted with clean cleanup", result.Status, result.Precedence, result.Check.Status, result.Check.ExitCode, result.Errors, result.Cleanup)
	}
}

func TestRun_DistinctSessionsRunConcurrently(t *testing.T) {
	f := newFixture(t, serviceConfig)
	withHelper(t, "sleep")
	// Both attempts must be past readiness, holding their leases with their
	// stacks running, before either may run its check. Without this barrier one
	// attempt can finish before the other acquires, and reusing its released
	// port would then be legitimate.
	const barrierTimeout = 15 * time.Second
	b := newStartupBarrier(t, 2, barrierTimeout)
	results := make(chan *Result, 2)
	for _, name := range []string{"one", "two"} {
		opts := f.options(t, name, checkArgv("sleep", "500ms")...)
		opts.afterStartup = b.hold
		go func() { results <- Run(b.ctx, opts) }()
	}
	value := func(s *string) string {
		if s == nil {
			return "<nil>"
		}
		return *s
	}
	describe := func(r *Result) string {
		if r == nil {
			return "<no result>"
		}
		return fmt.Sprintf("session=%s status=%s run_id=%s endpoints=%v started=%s ready=%s check=%s..%s finished=%s errors=%v",
			r.Session, r.Status, value(r.RunID), r.Endpoints, r.StartedAt, value(r.Startup.FinishedAt),
			value(r.Check.StartedAt), value(r.Check.FinishedAt), value(r.FinishedAt), r.Errors)
	}
	// collect is bounded: attempts that do not finish fail the test with
	// whatever did arrive instead of hanging it, and cancellation is the
	// cleanup for the rest.
	collect := func() (*Result, *Result, bool) {
		got, ok := collectResults(results, 2, 2*time.Minute, b.abort)
		got = append(got, nil, nil)
		return got[0], got[1], ok
	}

	arrivals := 0
	for arrivals < 2 {
		select {
		case <-b.arrived:
			arrivals++
		case <-time.After(barrierTimeout):
			b.abort()
			a, c, finished := collect()
			t.Fatalf("attempts did not overlap (%d of 2 reached readiness; all results collected: %t):\n  %s\n  %s", arrivals, finished, describe(a), describe(c))
		}
	}
	// Both are past readiness. Neither may leave the barrier, or finish, until
	// the controller releases them.
	select {
	case at := <-b.left:
		b.abort()
		a, c, finished := collect()
		t.Fatalf("an attempt left the barrier at %s before release (all results collected: %t):\n  %s\n  %s", at, finished, describe(a), describe(c))
	case r := <-results:
		b.abort()
		t.Fatalf("an attempt finished before release:\n  %s", describe(r))
	case <-time.After(200 * time.Millisecond):
	}
	b.releaseNow()
	a, c, finished := collect()
	if !finished {
		t.Fatalf("attempts did not finish within 2m of release:\n  %s\n  %s", describe(a), describe(c))
	}
	if b.timedOut.Load() {
		t.Fatalf("barrier timed out instead of being released:\n  %s\n  %s", describe(a), describe(c))
	}
	released := time.Unix(0, b.releasedAt.Load())
	for i := range 2 {
		select {
		case at := <-b.left:
			if at.Before(released) {
				t.Fatalf("an attempt left the barrier at %s, before release at %s", at, released)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 attempts recorded leaving the barrier after both finished", i)
		}
	}
	if a == nil || c == nil || a.Status != StatusPassed || c.Status != StatusPassed {
		t.Fatalf("concurrent attempts did not both pass:\n  %s\n  %s", describe(a), describe(c))
	}
	if a.Endpoints["api.http"].Port == c.Endpoints["api.http"].Port {
		t.Errorf("overlapping sessions shared endpoint api.http:\n  %s\n  %s", describe(a), describe(c))
	}
	if a.RunID == nil || c.RunID == nil || *a.RunID == *c.RunID {
		t.Errorf("overlapping sessions shared a run identity:\n  %s\n  %s", describe(a), describe(c))
	}
}

func TestEncode_CompactErrorNeverReportsSuccess(t *testing.T) {
	result := newResult(Options{ConfigPath: "/x/proc-compose.yml", Argv: []string{"true"}}, time.Now())
	result.Status, result.RunID = StatusPassed, ptr("run")
	for i := 0; i < 2000; i++ {
		result.SelectedProcesses = append(result.SelectedProcesses, ProcessGeneration{Name: strings.Repeat("p", 64) + strconv.Itoa(i), Generation: 1})
	}
	data, compact, err := Encode(result)
	if err != nil || !compact || len(data) > MaxResultBytes {
		t.Fatalf("compact=%t size=%d err=%v", compact, len(data), err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["kind"] != KindCompactError || out["status"] != StatusFailed || out["exit_code"] != float64(ExitFailed) || out["attempt_state"] != StateIndeterminate || out["terminal_receipt_persisted"] != false {
		t.Fatalf("compact = %v", out)
	}
	if phases := out["phase_statuses"].(map[string]any); phases["environment"] != StatusNotRun {
		t.Fatalf("phase_statuses = %v", phases)
	}
	identity := out["identity"].(map[string]any)
	if identity["complete"] != false || len(identity["omitted_fields"].([]any)) == 0 || !strings.HasPrefix(identity["session_sha256"].(string), "sha256:") {
		t.Fatalf("identity = %v", identity)
	}
}

func TestClassify_RequiresPositiveOwnerLossAndReleasedLease(t *testing.T) {
	active := &Result{State: StateActive, Owner: &Owner{PID: 1}}
	cases := []struct {
		alive, known, running, held bool
		want                        string
	}{
		{true, true, true, true, StateActive},
		{false, true, false, false, StateAbandoned},
		{false, true, false, true, StateIndeterminate},
		// A live owner that no longer runs the session outlived its attempt.
		{true, true, false, false, StateIndeterminate},
		{true, true, false, true, StateIndeterminate},
		{false, false, false, false, StateIndeterminate},
	}
	for _, c := range cases {
		probed := false
		got := Classify(active, OwnerEvidence{
			OwnerAlive:   func(Owner) (bool, bool) { return c.alive, c.known },
			OwnerRunning: func(Owner) bool { return c.running },
			LeaseHeld:    func() (bool, error) { probed = true; return c.held, nil },
		})
		if got != c.want {
			t.Errorf("alive=%t known=%t running=%t held=%t: %s, want %s", c.alive, c.known, c.running, c.held, got, c.want)
		}
		// The probe may take a free lease, so only positive owner loss reaches it.
		if probed != (c.known && !c.alive) {
			t.Errorf("alive=%t known=%t: lease probed=%t", c.alive, c.known, probed)
		}
	}
	gone := OwnerEvidence{OwnerAlive: func(Owner) (bool, bool) { return false, true }}
	if got := Classify(active, gone); got != StateIndeterminate {
		t.Errorf("gone owner without a lease probe = %s, want indeterminate", got)
	}
	gone.LeaseHeld = func() (bool, error) { return false, errors.New("probe failed") }
	if got := Classify(active, gone); got != StateIndeterminate {
		t.Errorf("failed lease probe = %s, want indeterminate", got)
	}
	if Classify(&Result{State: StateActive}, OwnerEvidence{}) != StateIndeterminate {
		t.Error("missing owner metadata was not indeterminate")
	}
	if Classify(&Result{State: StateCompleted}, OwnerEvidence{}) != StateCompleted {
		t.Error("terminal receipt reclassified")
	}
}

func TestPrune_KeepsRecentTerminalAndAllNonterminal(t *testing.T) {
	dir := t.TempDir()
	write := func(i int, state string) string {
		id := fmt.Sprintf("01J%023d", i)
		path := filepath.Join(AttemptsDir(dir), id+".json")
		r := newResult(Options{ConfigPath: "/x/c.yml"}, time.Now())
		r.State = state
		if err := WriteReceipt(path, r); err != nil {
			t.Fatal(err)
		}
		return path
	}
	activeOld := write(0, StateActive)
	var last string
	for i := 1; i <= RetainedTerminalReceipts+5; i++ {
		last = write(i, StateCompleted)
	}
	if err := Prune(dir, last); err != nil {
		t.Fatal(err)
	}
	paths, _ := ListReceipts(dir)
	if len(paths) != RetainedTerminalReceipts+1 {
		t.Fatalf("retained %d receipts", len(paths))
	}
	if _, err := os.Stat(activeOld); err != nil {
		t.Fatal("active receipt was pruned")
	}
}

func TestNewAttemptID_SortableULID(t *testing.T) {
	a, _ := NewAttemptID(time.UnixMilli(1000))
	b, _ := NewAttemptID(time.UnixMilli(2000))
	if len(a) != 26 || a >= b {
		t.Fatalf("ids %s %s", a, b)
	}
}

func TestEnvName_Normalizes(t *testing.T) {
	if got := EnvName("URL", "web-app", "http.v2"); got != "PROC_COMPOSE_URL_WEB_APP_HTTP_V2" {
		t.Fatal(got)
	}
	cfg := &config.Config{Processes: map[string]config.Process{"a-b": {Ports: map[string]int{"x": 1}}, "a_b": {Ports: map[string]int{"x": 2}}}}
	if endpointNameCollisions(cfg, []string{"a-b", "a_b"}) == nil {
		t.Fatal("collision accepted")
	}
}

func TestDiagnose_ScopesToAttemptAndGroupsOutput(t *testing.T) {
	f := newFixture(t, serviceConfig)
	opts := f.options(t, "diag", "/bin/sh", "-c", "echo boom; echo boom; echo boom; exit 2")
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("POSIX shell required for this fixture")
	}
	first := Run(context.Background(), opts)
	second := Run(context.Background(), f.options(t, "diag", "/bin/sh", "-c", "echo other"))
	if first.Status != StatusFailed || second.Status != StatusPassed {
		t.Fatalf("fixture attempts: %s / %s", first.Status, second.Status)
	}
	receipt, err := ReadReceipt(*first.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	d := Diagnose(receipt, StateCompleted, nil)
	if d.Boundary != "attempt_id" || d.HistoryMissing || d.EventsScanned == 0 {
		t.Fatalf("diagnosis = %+v", d)
	}
	var check *ProcessHistory
	for i := range d.Processes {
		if d.Processes[i].Name == CheckProcessName {
			check = &d.Processes[i]
		}
		for _, group := range d.Processes[i].Output {
			if strings.Contains(group.Message, "other") {
				t.Fatal("diagnosis included output from a different attempt")
			}
		}
	}
	if check == nil || len(check.Output) != 1 || check.Output[0].Count != 3 || check.Output[0].FirstID == check.Output[0].LastID {
		t.Fatalf("check history = %+v", check)
	}
	filtered := Diagnose(receipt, StateCompleted, []string{"api"})
	if len(filtered.Processes) != 1 || filtered.Processes[0].Name != "api" {
		t.Fatalf("filtered = %+v", filtered.Processes)
	}

	if err := os.Remove(receipt.Journal()); err != nil {
		t.Fatal(err)
	}
	missing := Diagnose(receipt, StateCompleted, nil)
	if !missing.HistoryMissing || missing.Boundary != "unavailable" || len(missing.Processes) != 0 {
		t.Fatalf("missing history = %+v", missing)
	}
}

func TestCompare_ReportsDifferencesWithoutAttribution(t *testing.T) {
	base := newFixture(t, serviceConfig)
	cand := newFixture(t, serviceConfig)
	withHelper(t, "exit")
	b, c := base.options(t, "cmp-baseline", checkArgv("exit", "0")...), cand.options(t, "cmp-candidate", checkArgv("exit", "0")...)
	same := Compare(context.Background(), b, c, 2)
	if same.Status != "comparable" || same.ExitCode != 0 || same.OutcomesEqual == nil || !*same.OutcomesEqual || len(same.Order) != 4 || same.Order[2] != "candidate" {
		t.Fatalf("equal comparison = %+v", same)
	}
	c.Argv = checkArgv("exit", "5")
	different := Compare(context.Background(), b, c, 1)
	if different.Status != "comparable" || *different.OutcomesEqual || len(different.Differences) == 0 {
		t.Fatalf("different comparison = %+v", different)
	}
	found := false
	for _, limitation := range different.Limitations {
		found = found || limitation == "differences_not_attributed_to_code_change"
	}
	if !found {
		t.Fatal("comparison omitted its attribution limitation")
	}
	if bad := Compare(context.Background(), b, c, 0); bad.ExitCode != ExitUsage {
		t.Fatalf("repeat 0 = %+v", bad)
	}
}

const (
	eligibleReport   = `{"observations":[{"name":"api.build","status":"matched","expected":"b1","observed":"b1"},{"name":"db.fixture","status":"matched"}]}`
	ineligibleReport = `{"observations":[{"name":"api.build","status":"mismatched","expected":"b2","observed":"b1"},{"name":"db.fixture","status":"unobserved","expected":"run-1","observed":null}]}`
)

// TestRun_EnvironmentReportVerdicts pins how the check's environment report
// changes the attempt: a mismatch or invalid report always fails it, a
// missing or empty one only when required, and the semantic check status is
// kept as its own fact.
func TestRun_EnvironmentReportVerdicts(t *testing.T) {
	f := newFixture(t, serviceConfig)
	cases := []struct {
		name, mode, arg string
		required        bool
		status, reason  string
		errs            []string
	}{
		{"eligible", "report", eligibleReport, true, EnvironmentEligible, "", nil},
		{"ineligible", "report", ineligibleReport, false, EnvironmentIneligible, "", []string{CodeEnvironmentIneligible}},
		{"missing optional", "exit", "0", false, EnvironmentUnknown, ReasonReportMissing, nil},
		{"missing required", "exit", "0", true, EnvironmentUnknown, ReasonReportMissing, []string{CodeEnvironmentUnverified}},
		{"empty required", "report", `{"observations":[]}`, true, EnvironmentUnknown, ReasonNoObservations, []string{CodeEnvironmentUnverified}},
		{"invalid optional", "report", `{"eligible":true,"observations":[]}`, false, EnvironmentUnknown, ReasonReportInvalid, []string{CodeEnvironmentUnverified}},
		{"null optional", "report", `null`, false, EnvironmentUnknown, ReasonReportInvalid, []string{CodeEnvironmentUnverified}},
		{"repeated status required", "report", `{"observations":[{"name":"a","status":"mismatched","status":"matched"}]}`, true, EnvironmentUnknown, ReasonReportInvalid, []string{CodeEnvironmentUnverified}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withHelper(t, tc.mode)
			opts := f.options(t, "env", checkArgv(tc.mode, tc.arg)...)
			opts.RequireEnvironment = tc.required
			result := Run(context.Background(), opts)
			assertComplete(t, result)
			env := result.Environment
			reason := ""
			if env.Reason != nil {
				reason = *env.Reason
			}
			if env.Status != tc.status || reason != tc.reason || env.Required != tc.required {
				t.Fatalf("environment = %+v reason %q", env, reason)
			}
			var codes []string
			for _, record := range result.Errors {
				codes = append(codes, record.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tc.errs, ",") {
				t.Fatalf("errors = %v, want %v", codes, tc.errs)
			}
			wantStatus, wantExit := StatusPassed, ExitPassed
			if len(tc.errs) > 0 {
				wantStatus, wantExit = StatusFailed, ExitFailed
			}
			if result.Status != wantStatus || result.ExitCode != wantExit || result.Check.Status != StatusPassed {
				t.Fatalf("status %s exit %d check %s", result.Status, result.ExitCode, result.Check.Status)
			}
			receipt, err := ReadReceipt(*result.ReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Environment.Status != tc.status || len(receipt.Environment.Observations) != len(env.Observations) {
				t.Fatalf("receipt environment = %+v", receipt.Environment)
			}
			if tc.mode == "report" && tc.status != EnvironmentUnknown && (len(env.Observations) != 2 || env.Observations[0].Name != "api.build") {
				t.Fatalf("observations = %+v", env.Observations)
			}
			// The receipt carries the report; the file itself is removed.
			if _, err := os.Stat(environmentReportPath(filepath.Dir(filepath.Dir(*result.ReceiptPath)), *result.AttemptID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("report file left behind: %v", err)
			}
		})
	}
}

func TestReadEnvironmentReport_RejectsMalformedEvidence(t *testing.T) {
	dir := t.TempDir()
	many := make([]string, MaxObservations+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"name":"o%d","status":"matched"}`, i)
	}
	for name, body := range map[string]string{
		"not json":           `observations`,
		"top-level null":     `null`,
		"top-level array":    `[{"observations":[]}]`,
		"repeated status":    `{"observations":[{"name":"a","status":"mismatched","status":"matched"}]}`,
		"case-folded status": `{"observations":[{"name":"a","status":"mismatched","Status":"matched"}]}`,
		"repeated list":      `{"observations":[{"name":"a","status":"mismatched"}],"observations":[{"name":"a","status":"matched"}]}`,
		"unknown field":      `{"observations":[],"eligible":true}`,
		"unknown obs field":  `{"observations":[{"name":"a","status":"matched","note":"x"}]}`,
		"trailing value":     `{"observations":[]} {}`,
		"number value":       `{"observations":[{"name":"a","status":"matched","expected":1}]}`,
		"empty name":         `{"observations":[{"name":"","status":"matched"}]}`,
		"duplicate name":     `{"observations":[{"name":"a","status":"matched"},{"name":"a","status":"matched"}]}`,
		"unknown status":     `{"observations":[{"name":"a","status":"ok"}]}`,
		"too many":           `{"observations":[` + strings.Join(many, ",") + `]}`,
		"oversized file":     `{"observations":[]}` + strings.Repeat(" ", MaxEnvironmentBytes),
		"oversized encoding": `{"observations":[{"name":"a","status":"matched","expected":"` + strings.Repeat("<", MaxEnvironmentBytes/2) + `"}]}`,
	} {
		path := filepath.Join(dir, "report")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readEnvironmentReport(path); err == nil {
			t.Errorf("%s: accepted %.80q", name, body)
		}
	}
	if _, err := readEnvironmentReport(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing report error = %v", err)
	}
	path := filepath.Join(dir, "report")
	if err := os.WriteFile(path, []byte(ineligibleReport+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	observations, err := readEnvironmentReport(path)
	if err != nil || len(observations) != 2 || observations[1].Observed != nil || *observations[0].Expected != "b2" {
		t.Fatalf("valid report = %+v err %v", observations, err)
	}
}

// TestCompare_EnvironmentMismatchIsNotComparable: a side whose check reports
// the wrong environment cannot be compared, even though both checks exit 0.
func TestCompare_EnvironmentMismatchIsNotComparable(t *testing.T) {
	base := newFixture(t, serviceConfig)
	cand := newFixture(t, serviceConfig)
	withHelper(t, "report")
	b, c := base.options(t, "env-baseline", checkArgv("report", eligibleReport)...), cand.options(t, "env-candidate", checkArgv("report", ineligibleReport)...)
	result := Compare(context.Background(), b, c, 1)
	if result.Status != "not_comparable" || result.ExitCode != ExitFailed || result.OutcomesEqual != nil {
		t.Fatalf("comparison = %+v", result)
	}
	if len(result.Errors) != 1 || result.Errors[0].Code != CodeEnvironmentIneligible {
		t.Fatalf("errors = %v", result.Errors)
	}
	if got := result.Sides["candidate"].Attempts[0].EnvironmentStatus; got != EnvironmentIneligible {
		t.Fatalf("candidate environment_status = %q", got)
	}
	if got := result.Sides["baseline"].Attempts[0].EnvironmentStatus; got != EnvironmentEligible {
		t.Fatalf("baseline environment_status = %q", got)
	}

	// Required: a side that reports nothing is not comparable either.
	withHelper(t, "exit")
	b.Argv, c.Argv = checkArgv("exit", "0"), checkArgv("exit", "0")
	b.RequireEnvironment, c.RequireEnvironment = true, true
	if silent := Compare(context.Background(), b, c, 1); silent.Status != "not_comparable" || len(silent.Errors) != 1 || silent.Errors[0].Code != CodeEnvironmentUnverified {
		t.Fatalf("required but unreported comparison = %+v", silent)
	}
}

// TestRun_BlockedCheckReapSpendsOneCleanupBudget holds the check's reap so its
// teardown cannot finish. The check deadline is the first cleanup trigger:
// check teardown, stack teardown, and sealing must all fit inside the one
// budget that starts there, instead of the check spending part of it and the
// stack then starting a fresh one.
func TestRun_BlockedCheckReapSpendsOneCleanupBudget(t *testing.T) {
	f := newFixture(t, serviceConfig)
	withHelper(t, "sleep")
	opts := f.options(t, "", checkArgv("sleep", "60s")...)
	opts.CheckTimeout = time.Second
	opts.cleanupBudget = 6 * time.Second
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var checkPID atomic.Int64
	opts.afterStartup = func(r *runner.Runner) {
		r.SetProcessWaiter(func(cmd *exec.Cmd) error {
			if len(cmd.Args) > 0 && cmd.Args[0] == os.Args[0] {
				checkPID.Store(int64(cmd.Process.Pid))
				<-release
			}
			return cmd.Wait()
		})
	}
	result := Run(context.Background(), opts)
	returned := time.Now()
	parse := func(name string, value *string) time.Time {
		t.Helper()
		if value == nil {
			t.Fatalf("%s missing", name)
		}
		at, err := time.Parse(time.RFC3339Nano, *value)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	checkDeadline := parse("check.deadline_at", result.Check.DeadlineAt)
	cleanupStart := parse("cleanup.started_at", result.Cleanup.StartedAt)
	cleanupDeadline := parse("cleanup.deadline_at", result.Cleanup.DeadlineAt)
	if gap := cleanupStart.Sub(checkDeadline); gap < -100*time.Millisecond || gap > time.Second {
		t.Errorf("cleanup started %s after the check deadline; the trigger must start the budget", gap)
	}
	if budget := cleanupDeadline.Sub(cleanupStart); budget != 6*time.Second {
		t.Errorf("cleanup budget = %s, want 6s", budget)
	}
	if over := returned.Sub(cleanupDeadline); over > time.Second {
		t.Errorf("verify returned %s after the single cleanup deadline", over)
	}
	if result.Status == StatusPassed || result.Check.Status != StatusUnverified || result.Cleanup.Status != StatusUnverified {
		t.Fatalf("blocked reap = status %s check %s cleanup %+v", result.Status, result.Check.Status, result.Cleanup)
	}
	found := false
	for _, u := range result.Cleanup.UnresolvedProcesses {
		found = found || u.Name == CheckProcessName
	}
	if !found {
		t.Errorf("unreaped check missing from unresolved: %+v", result.Cleanup.UnresolvedProcesses)
	}
	releaseOnce()
	if pid := int(checkPID.Load()); pid != 0 {
		assertGone(t, pid)
	}
	assertGone(t, pidFromFile(t, f.pidFile))
}

// teardownConfig adds a teardown process to serviceConfig, run as the helper
// in mode {{TEARDOWN_MODE}} with argument {{TEARDOWN_ARG}}.
const teardownConfig = serviceConfig + `  cleanup:
    mode: teardown
    cmd: {{BIN}} {{TEARDOWN_ARG}}
    shutdown_timeout: 1
    env:
      ` + helperEnv + `: {{TEARDOWN_MODE}}
`

// TestRun_TeardownRecordedInReceipt pins the receipt's teardown section: it is
// always present, records each teardown process's outcome, and a failed
// teardown fails the attempt with teardown_failed while cleanup.status and
// termination_verified keep describing termination only.
func TestRun_TeardownRecordedInReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		wantStatus string
	}{
		{"passed", "0", runner.TeardownPassed},
		{"failed", "3", runner.TeardownFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReplacer("{{TEARDOWN_MODE}}", "exit", "{{TEARDOWN_ARG}}", tc.code).Replace(teardownConfig)
			f := newFixture(t, body)
			withHelper(t, "exit")
			result := Run(context.Background(), f.options(t, "", checkArgv("exit", "0")...))
			if result.Cleanup.Status != StatusPassed || result.Cleanup.TerminationVerified == nil || !*result.Cleanup.TerminationVerified {
				t.Fatalf("cleanup = %+v, want termination verified", result.Cleanup)
			}
			got := result.Cleanup.Teardown
			if len(got) != 1 || got[0].Name != "cleanup" || got[0].Status != tc.wantStatus || got[0].ExitCode == nil || strconv.Itoa(*got[0].ExitCode) != tc.code {
				t.Fatalf("cleanup.teardown = %+v, want cleanup %s with exit %s", got, tc.wantStatus, tc.code)
			}
			hasCode := false
			for _, e := range result.Errors {
				hasCode = hasCode || e.Code == CodeTeardownFailed
			}
			if tc.wantStatus == runner.TeardownPassed {
				if result.Status != StatusPassed || hasCode || got[0].Error != "" {
					t.Fatalf("result = %s errors=%v teardown=%+v, want passed", result.Status, result.Errors, got)
				}
			} else if result.Status != StatusFailed || result.ExitCode != ExitFailed || !hasCode || !strings.Contains(got[0].Error, "exit 3") {
				t.Fatalf("result = %s/%d errors=%v teardown=%+v, want failed with %s", result.Status, result.ExitCode, result.Errors, got, CodeTeardownFailed)
			}
			receipt, err := ReadReceipt(*result.ReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(receipt.Cleanup.Teardown) != 1 || receipt.Cleanup.Teardown[0].Status != tc.wantStatus {
				t.Fatalf("receipt teardown = %+v", receipt.Cleanup.Teardown)
			}
		})
	}

	// Without teardown processes the section is an empty list, never absent.
	f := newFixture(t, serviceConfig)
	withHelper(t, "exit")
	result := Run(context.Background(), f.options(t, "", checkArgv("exit", "0")...))
	data, err := json.Marshal(result.Cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"teardown":[]`) {
		t.Fatalf("cleanup = %s, want an empty teardown list", data)
	}
}

// TestRun_TeardownFitsCleanupBudget: teardown spends the attempt's one cleanup
// budget. A hung teardown is cut off before the seal reserve, so verify still
// returns at the cleanup deadline with termination verified and the teardown
// reported as timed out.
func TestRun_TeardownFitsCleanupBudget(t *testing.T) {
	body := strings.NewReplacer("{{TEARDOWN_MODE}}", "sleep", "{{TEARDOWN_ARG}}", "60s").Replace(teardownConfig)
	f := newFixture(t, body)
	withHelper(t, "exit")
	opts := f.options(t, "", checkArgv("exit", "0")...)
	opts.cleanupBudget = 8 * time.Second
	result := Run(context.Background(), opts)
	returned := time.Now()
	deadline, err := time.Parse(time.RFC3339Nano, *result.Cleanup.DeadlineAt)
	if err != nil {
		t.Fatal(err)
	}
	if over := returned.Sub(deadline); over > time.Second {
		t.Fatalf("verify returned %s after the cleanup deadline", over)
	}
	if result.Cleanup.Status != StatusPassed {
		t.Fatalf("cleanup = %+v, want the hung teardown terminated and sealed", result.Cleanup)
	}
	if got := result.Cleanup.Teardown; len(got) != 1 || got[0].Status != runner.TeardownTimedOut {
		t.Fatalf("cleanup.teardown = %+v, want cleanup timed_out", got)
	}
	if result.Status != StatusFailed {
		t.Fatalf("result = %s errors=%v, want failed", result.Status, result.Errors)
	}
	assertGone(t, pidFromFile(t, f.pidFile))
}
