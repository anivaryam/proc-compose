package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// teardownCodes returns the journal's teardown event codes in order.
func teardownCodes(t *testing.T, journal *bytes.Buffer) []string {
	t.Helper()
	var codes []string
	for _, event := range journalEvents(t, journal) {
		if strings.HasPrefix(event.Code, "teardown.") {
			codes = append(codes, event.Process+":"+event.Code)
		}
	}
	return codes
}

// TestRun_TeardownRunsAfterVerifiedStop pins the core contract: a teardown
// process runs after a stop, only once the service it cleans up after is gone,
// and its success leaves both the termination verdict and Run's result clean.
func TestRun_TeardownRunsAfterVerifiedStop(t *testing.T) {
	dir := t.TempDir()
	apiPID, seen := filepath.Join(dir, "api.pid"), filepath.Join(dir, "seen")
	r := makeRunner(map[string]config.Process{
		"api":     {Cmd: serviceHelperCmd("pid-block", apiPID), ShutdownTimeout: shutdownGraceForTest},
		"cleanup": {Cmd: helperCmd("teardown-probe", apiPID, seen, "0"), Mode: config.ProcessModeTeardown},
	})
	var journal bytes.Buffer
	r.Output, r.Journal = io.Discard, &journal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	pid, ok := waitForPIDFile(t, apiPID, 10*time.Second)
	if !ok {
		t.Fatal("api never started")
	}
	trackProcessForCleanup(t, pid, "api")
	if got := r.Snapshot(); len(got) != 1 || got[0].Name != "api" {
		t.Fatalf("snapshot = %+v, want only the service: teardown never starts with the stack", got)
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not stop")
	}
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if data, readErr := os.ReadFile(seen); readErr != nil || string(data) != "gone" {
		t.Fatalf("teardown saw the service %q (%v), want gone", data, readErr)
	}
	if verdict := r.ShutdownResult(); verdict != nil {
		t.Fatalf("ShutdownResult = %v", verdict)
	}
	results := r.TeardownResults()
	if len(results) != 1 || results[0].Name != "cleanup" || results[0].Status != TeardownPassed || results[0].ExitCode == nil || *results[0].ExitCode != 0 {
		t.Fatalf("teardown results = %+v, want cleanup passed", results)
	}
	if want := []string{"cleanup:teardown.started", "cleanup:teardown.completed"}; !slices.Equal(teardownCodes(t, &journal), want) {
		t.Fatalf("teardown events = %v, want %v", teardownCodes(t, &journal), want)
	}
}

// TestRun_TeardownRunsOnNormalExit: a foreground stack that ends on its own,
// with no IPC and no stop, still runs teardown, also when a filter started
// only part of the stack.
func TestRun_TeardownRunsOnNormalExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "torn-down")
	r := makeRunner(map[string]config.Process{
		"job":     {Cmd: helperCmd("exit", "0"), Mode: config.ProcessModeTask},
		"other":   {Cmd: helperCmd("exit", "1"), Mode: config.ProcessModeTask},
		"cleanup": {Cmd: helperCmd("touch", marker), Mode: config.ProcessModeTeardown},
	})
	r.Filter = []string{"job"}
	r.Output = io.Discard
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("teardown did not run: %v", err)
	}
	if results := r.TeardownResults(); len(results) != 1 || results[0].Status != TeardownPassed {
		t.Fatalf("teardown results = %+v", results)
	}
}

// TestRun_TeardownRunsAfterFailFastTask: a failed setup task stops the stack,
// and teardown still runs to release whatever setup had taken.
func TestRun_TeardownRunsAfterFailFastTask(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "torn-down")
	r := makeRunner(map[string]config.Process{
		"seed":    {Cmd: helperCmd("exit", "1"), Mode: config.ProcessModeTask},
		"api":     {Cmd: serviceHelperCmd("sleep", "60s"), DependsOn: []string{"seed"}},
		"cleanup": {Cmd: helperCmd("touch", marker), Mode: config.ProcessModeTeardown},
	})
	r.Output = io.Discard
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "processes failed") {
		t.Fatalf("Run = %v, want the failed task reported", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("teardown did not run after the failed task: %v", statErr)
	}
	if strings.Contains(err.Error(), ipc.TeardownPrefix) {
		t.Fatalf("Run = %v: a passing teardown must not be reported", err)
	}
}

// TestRun_TeardownFailureFoldsIntoVerdictNotTermination: a failed teardown is
// reported in the published stop verdict and Run's result, every line marked
// with ipc.TeardownPrefix, while ShutdownResult (termination) stays nil.
func TestRun_TeardownFailureFoldsIntoVerdictNotTermination(t *testing.T) {
	dir := t.TempDir()
	apiPID := filepath.Join(dir, "api.pid")
	r := makeRunner(map[string]config.Process{
		"api":     {Cmd: serviceHelperCmd("pid-block", apiPID), ShutdownTimeout: shutdownGraceForTest},
		"cleanup": {Cmd: helperCmd("log", "bucket locked", "stderr") + " && " + helperCmd("exit", "3"), Mode: config.ProcessModeTeardown},
	})
	if runtime.GOOS == "windows" {
		r.Config.Processes["cleanup"] = config.Process{Cmd: helperCmd("exit", "3"), Mode: config.ProcessModeTeardown}
	}
	r.Output = io.Discard
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	pid, ok := waitForPIDFile(t, apiPID, 10*time.Second)
	if !ok {
		t.Fatal("api never started")
	}
	trackProcessForCleanup(t, pid, "api")
	stopped, verdictCh, err := r.Shutdown(false)
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not stop")
	}
	<-stopped
	verdict := <-verdictCh
	if r.ShutdownResult() != nil {
		t.Fatalf("ShutdownResult = %v, want nil: termination was verified", r.ShutdownResult())
	}
	if verdict == nil {
		t.Fatal("published verdict is nil although teardown failed")
	}
	for _, line := range strings.Split(verdict.Error(), "\n") {
		if !strings.HasPrefix(line, ipc.TeardownPrefix) {
			t.Fatalf("verdict line %q lacks the teardown prefix", line)
		}
	}
	if !strings.Contains(verdict.Error(), "cleanup failed: exit 3") {
		t.Fatalf("verdict = %q, want the teardown's exit status", verdict)
	}
	if runtime.GOOS != "windows" && !strings.Contains(verdict.Error(), "bucket locked") {
		t.Fatalf("verdict = %q, want the teardown's stderr head", verdict)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), ipc.TeardownPrefix+"cleanup failed") {
		t.Fatalf("Run = %v, want the teardown failure", runErr)
	}
}

// TestRun_TeardownSkippedWhenTerminationUnverified: teardown never runs beside
// a process that may still be running. A descendant that escaped its group
// makes termination unverified, so the teardown is recorded as skipped and its
// command never starts.
func TestRun_TeardownSkippedWhenTerminationUnverified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid is a Unix facility")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not available on this host")
	}
	dir := t.TempDir()
	pidFile, marker := filepath.Join(dir, "escapee.pid"), filepath.Join(dir, "torn-down")
	stubborn := writeScript(t, dir, "stubborn.sh", stubbornScript)
	escape := writeScript(t, dir, "escape.sh", escapeScript)
	r := makeRunner(map[string]config.Process{
		"worker":  {Cmd: escape + " " + stubborn + " " + pidFile, ShutdownTimeout: shutdownGraceForTest},
		"keeper":  {Cmd: helperCmd("sleep", "120s"), ShutdownTimeout: shutdownGraceForTest},
		"cleanup": {Cmd: helperCmd("touch", marker), Mode: config.ProcessModeTeardown},
	})
	var journal bytes.Buffer
	r.Output, r.Journal = io.Discard, &journal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	escapee, ok := waitForPIDFile(t, pidFile, 20*time.Second)
	if !ok {
		t.Fatal("escaped descendant never recorded its PID")
	}
	trackProcessForCleanup(t, escapee, "escaped descendant")
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("runner did not stop")
	}
	if r.ShutdownResult() == nil {
		t.Fatal("fixture did not make termination unverified")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("teardown ran beside an unverified termination (stat: %v)", statErr)
	}
	results := r.TeardownResults()
	if len(results) != 1 || results[0].Status != TeardownSkipped || results[0].Error != TeardownSkipUnverified {
		t.Fatalf("teardown results = %+v, want cleanup skipped (%s)", results, TeardownSkipUnverified)
	}
	if err == nil || !strings.Contains(err.Error(), ipc.TeardownPrefix+"cleanup skipped") {
		t.Fatalf("Run = %v, want the skipped teardown reported", err)
	}
	if want := []string{"proc-compose:teardown.skipped"}; !slices.Equal(teardownCodes(t, &journal), want) {
		t.Fatalf("teardown events = %v, want %v", teardownCodes(t, &journal), want)
	}
}

// TestRun_TeardownBudgetKillsHungTaskAndSeals: a teardown process still
// running at its budget is terminated, its group verified empty, and the next
// one, with no budget left, is reported as not run. Restarts stay refused
// while teardown runs.
func TestRun_TeardownBudgetKillsHungTaskAndSeals(t *testing.T) {
	dir := t.TempDir()
	hungPID := filepath.Join(dir, "hung.pid")
	r := makeRunner(map[string]config.Process{
		"api":    {Cmd: serviceHelperCmd("sleep", "60s"), ShutdownTimeout: shutdownGraceForTest},
		"a-hung": {Cmd: serviceHelperCmd("pid-block", hungPID), Mode: config.ProcessModeTeardown, ShutdownTimeout: shutdownGraceForTest},
		"b-late": {Cmd: helperCmd("exit", "0"), Mode: config.ProcessModeTeardown},
	})
	r.teardownBudget = 3 * time.Second
	r.Output = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	if !waitForProcState(t, r, "api", func(s ipc.ProcState) bool { return s.State == "running" }, 10*time.Second) {
		t.Fatal("api never started")
	}
	cancel()
	pid, ok := waitForPIDFile(t, hungPID, 15*time.Second)
	if !ok {
		t.Fatal("hung teardown never started")
	}
	trackProcessForCleanup(t, pid, "hung teardown")
	if res := r.requestRestart(r.store.Load().get("api")); res.Status == "ok" {
		t.Fatalf("restart during teardown = %+v, want it refused", res)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not stop within the teardown budget")
	}
	assertTerminated(t, pid, "hung teardown", 5*time.Second)
	if verdict := r.ShutdownResult(); verdict != nil {
		t.Fatalf("ShutdownResult = %v, want the hung teardown's group sealed clean", verdict)
	}
	results := r.TeardownResults()
	if len(results) != 2 || results[0].Status != TeardownTimedOut || results[1].Status != TeardownNotRun {
		t.Fatalf("teardown results = %+v, want a-hung timed_out and b-late not_run", results)
	}
	if err == nil || !strings.Contains(err.Error(), ipc.TeardownPrefix+"a-hung timed_out") || !strings.Contains(err.Error(), ipc.TeardownPrefix+"b-late not_run") {
		t.Fatalf("Run = %v, want both teardown outcomes", err)
	}
}

// TestRun_TeardownHandoffIsSealedAndReported: a teardown process whose reap
// does not finish within the budget is handed off. The phase then seals its
// group like any managed group, so nothing stays registered after Run
// returns, and the unreaped leader makes termination unverified.
func TestRun_TeardownHandoffIsSealedAndReported(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "held")
	cleanupCmd := helperCmd("touch", marker)
	r := makeRunner(map[string]config.Process{
		"job":     {Cmd: helperCmd("exit", "0"), Mode: config.ProcessModeTask},
		"cleanup": {Cmd: cleanupCmd, Mode: config.ProcessModeTeardown, ShutdownTimeout: shutdownGraceForTest},
	})
	r.teardownBudget = 3 * time.Second
	r.Output = io.Discard
	var held atomic.Bool
	release := make(chan struct{})
	releaseReap := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseReap)
	r.waitProcess = func(cmd *exec.Cmd) error {
		if strings.Contains(strings.Join(cmd.Args, " "), cleanupCmd) {
			held.Store(true)
			<-release
		}
		return cmd.Wait()
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		releaseReap()
		t.Fatal("Run did not return with a teardown reap held")
	}
	if !held.Load() {
		t.Fatal("the teardown's reap was never held")
	}
	results := r.TeardownResults()
	if len(results) != 1 || results[0].Status != TeardownHandedOff {
		t.Fatalf("teardown results = %+v, want cleanup handed_off", results)
	}
	if groups := r.snapshotGroups(); len(groups) != 0 {
		t.Fatalf("%d group(s) still registered after Run: the teardown group was not sealed", len(groups))
	}
	if verdict := r.ShutdownResult(); verdict == nil || !strings.Contains(verdict.Error(), "cleanup") {
		t.Fatalf("ShutdownResult = %v, want the unreaped teardown leader reported", verdict)
	}
	if err == nil || !strings.Contains(err.Error(), ipc.TeardownPrefix+"cleanup handed_off") {
		t.Fatalf("Run = %v, want the handed-off teardown reported", err)
	}
}

// TestReload_UpdatesTeardownDefinition: a changed teardown definition takes
// effect without any restart, because nothing runs for it until the stack
// stops.
func TestReload_UpdatesTeardownDefinition(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "proc-compose.yml")
	write := func(cleanup string) {
		t.Helper()
		body := "processes:\n  api:\n    cmd: serve\n  cleanup:\n    mode: teardown\n    cmd: " + cleanup + "\n"
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("clean-v1")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: cfg, ConfigPath: configPath}
	r.store.Store(newStateStore([]procInfo{{name: "api", proc: cfg.Processes["api"]}}))
	write("clean-v2")
	res := r.Reload()
	if res.Status != "ok" || res.Message != "updated 1 teardown" {
		t.Fatalf("Reload = %+v, want ok with the teardown updated", res)
	}
	if got := r.Config.Processes["cleanup"].Cmd; got != "clean-v2" {
		t.Fatalf("teardown cmd after reload = %q, want clean-v2", got)
	}
}

// TestRun_TeardownRunsAfterForcedStop: --force means "do not wait for the
// services", not "leak what they held": teardown still runs once the forced
// kill is verified.
func TestRun_TeardownRunsAfterForcedStop(t *testing.T) {
	dir := t.TempDir()
	apiPID, marker := filepath.Join(dir, "api.pid"), filepath.Join(dir, "torn-down")
	r := makeRunner(map[string]config.Process{
		"api":     {Cmd: serviceHelperCmd("pid-block", apiPID), ShutdownTimeout: 30},
		"cleanup": {Cmd: helperCmd("touch", marker), Mode: config.ProcessModeTeardown},
	})
	r.Output = io.Discard
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	pid, ok := waitForPIDFile(t, apiPID, 10*time.Second)
	if !ok {
		t.Fatal("api never started")
	}
	trackProcessForCleanup(t, pid, "api")
	start := time.Now()
	if _, _, err := r.Shutdown(true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not stop")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("forced stop took %s: the 30s grace was waited out", elapsed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("teardown did not run after a forced stop: %v", err)
	}
}
