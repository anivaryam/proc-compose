package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/logrotate"
	"github.com/anivaryam/proc-compose/internal/logs"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/runner"
	"github.com/anivaryam/proc-compose/internal/session"
)

// Policy defaults (unmeasured) and bounds.
const (
	DefaultStartupTimeout = 120 * time.Second
	DefaultCheckTimeout   = 10 * time.Minute
	DefaultCleanupTimeout = 30 * time.Second
	MinStartupTimeout     = time.Second
	MaxStartupTimeout     = 10 * time.Minute
	MinCheckTimeout       = time.Second
	MaxCheckTimeout       = time.Hour
	MinCleanupTimeout     = 30 * time.Second
	MaxCleanupTimeout     = 2 * time.Minute
	// sealReserve is the final part of the cleanup budget reserved for
	// synchronized signalling-authority sealing.
	sealReserve = 5 * time.Second
	// maxGraceSeconds caps configured shutdown_timeout during verification.
	maxGraceSeconds = 5
	// CheckProcessName names the check in the journal and live status.
	CheckProcessName = "verify:check"
)

// Options is one verification attempt's validated input.
type Options struct {
	ConfigPath     string // absolute config path
	Session        string // session name; empty is the default session
	Hash           string // runtime hash for (config, session)
	Selectors      []string
	Argv           []string
	Dir            string // raw --dir value; empty is the config directory
	StartupTimeout time.Duration
	CheckTimeout   time.Duration
	CleanupTimeout time.Duration
	ToolVersion    string
	Log            io.Writer // verify's own diagnostics; never stdout in JSON mode
	// ProcessOutput receives streamed process and check output. nil discards
	// it; the journal and the result still keep the evidence.
	ProcessOutput io.Writer
	ExtraEnv      map[string]string // extra check variables; generated session variables win
	// RequireEnvironment fails the attempt unless the check reports an
	// eligible environment through PROC_COMPOSE_ENVIRONMENT_REPORT.
	RequireEnvironment bool
	Now                func() time.Time

	// writeReceipt is a test seam; nil uses WriteReceipt.
	writeReceipt func(path string, result *Result) error
	// afterStartup is a test seam invoked with the live runner after readiness.
	afterStartup func(*runner.Runner)
	// cleanupBudget is a test seam that replaces the validated CleanupTimeout,
	// so budget enforcement can be exercised without waiting the 30s minimum.
	cleanupBudget time.Duration
}

// sealReserve is the final part of the cleanup budget kept for sealing. It
// is the fixed reserve for every user-reachable budget (30s and up).
func (o Options) sealReserve() time.Duration {
	return min(sealReserve, o.CleanupTimeout/6)
}

// ValidateDuration enforces a phase budget's documented range.
func ValidateDuration(name string, value, minimum, maximum time.Duration) error {
	if value < minimum || value > maximum {
		return fmt.Errorf("--%s must be between %s and %s", name, minimum, maximum)
	}
	return nil
}

// Usage builds the structured pre-acquisition refusal for a semantic usage
// error detected by the caller (for example, a malformed duration).
func Usage(opts Options, code string) *Result {
	result := newResult(opts, opts.now())
	result.Startup.Status = StatusNotRun
	failPreAcquisition(result, code, opts.now())
	return result
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func newResult(opts Options, started time.Time) *Result {
	name := opts.Session
	if name == "" {
		name = "default"
	}
	result := &Result{
		SchemaVersion:     SchemaVersion,
		Kind:              KindResponse,
		State:             StateCompleted,
		Status:            StatusFailed,
		Phase:             PhaseValidation,
		Session:           name,
		ConfigPath:        opts.ConfigPath,
		Workdir:           filepath.Dir(opts.ConfigPath),
		Source:            Source{Limitations: []string{"revision_unavailable", "dirty_state_unavailable", "observation_unavailable"}},
		ToolVersion:       opts.ToolVersion,
		SelectedProcesses: []ProcessGeneration{},
		Endpoints:         map[string]Endpoint{},
		Startup:           Startup{Status: StatusNotRun, Validation: StatusNotRun, ReadinessObservations: []ReadinessObservation{}, UnprobedProcesses: []string{}, Errors: []ErrorRecord{}},
		Check:             Check{Status: StatusNotRun, Argv: []string{}},
		Environment:       Environment{Required: opts.RequireEnvironment, Status: StatusNotRun, Observations: []Observation{}},
		Cleanup:           Cleanup{Status: StatusNotNeeded, SignalingAuthority: "none", UnresolvedProcesses: []UnresolvedProcess{}, Errors: []ErrorRecord{}, Teardown: []TeardownRecord{}},
		Evidence:          Evidence{ExcerptStatus: "not_collected", Events: []EvidenceEvent{}},
		Errors:            []ErrorRecord{},
		Retention:         "no receipt created",
		StartedAt:         *timestamp(started),
	}
	if len(opts.Argv) > 0 {
		result.Check.Argv = append([]string(nil), opts.Argv...)
		sum, size := ArgvDigest(opts.Argv)
		result.Check.ArgvSHA256, result.Check.ArgvBytes = &sum, size
	}
	return result
}

func failPreAcquisition(result *Result, code string, now time.Time) {
	result.Errors = []ErrorRecord{{Code: code, Count: 1}}
	if code == CodeInvalidConfig {
		result.Startup.Validation = StatusFailed
		result.Startup.Status = StatusFailed
		result.Startup.Errors = []ErrorRecord{{Code: code, Count: 1}}
	}
	result.Status, result.ExitCode, result.Precedence = StatusFailed, ExitUsage, PrecedencePreAcquisition
	result.FinishedAt = timestamp(now)
}

// Run performs one attempt. Cancelling ctx is a catchable interruption: work
// stops and cleanup still runs on its own independent budget.
func Run(ctx context.Context, opts Options) *Result {
	started := opts.now()
	result := newResult(opts, started)
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.ProcessOutput == nil {
		opts.ProcessOutput = io.Discard
	}
	if opts.writeReceipt == nil {
		opts.writeReceipt = WriteReceipt
	}

	// ── validation (no ownership, no receipt) ───────────────────────────────
	if len(opts.Argv) == 0 || opts.Argv[0] == "" {
		failPreAcquisition(result, CodeUsage, opts.now())
		return result
	}
	if result.Check.ArgvBytes > MaxArgvBytes {
		failPreAcquisition(result, CodeResultSizeLimit, opts.now())
		return result
	}
	for _, d := range []struct {
		name          string
		value, lo, hi time.Duration
	}{
		{"startup-timeout", opts.StartupTimeout, MinStartupTimeout, MaxStartupTimeout},
		{"check-timeout", opts.CheckTimeout, MinCheckTimeout, MaxCheckTimeout},
		{"cleanup-timeout", opts.CleanupTimeout, MinCleanupTimeout, MaxCleanupTimeout},
	} {
		if err := ValidateDuration(d.name, d.value, d.lo, d.hi); err != nil {
			fmt.Fprintf(opts.Log, "verify: %v\n", err)
			failPreAcquisition(result, CodeUsage, opts.now())
			return result
		}
	}
	if opts.cleanupBudget > 0 {
		opts.CleanupTimeout = opts.cleanupBudget
	}
	raw, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		failPreAcquisition(result, CodeInvalidConfig, opts.now())
		return result
	}
	result.ConfigSHA256 = digestPtr(raw)
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(opts.Log, "verify: %v\n", err)
		failPreAcquisition(result, CodeInvalidConfig, opts.now())
		return result
	}
	for _, name := range opts.Selectors {
		if err := config.CheckSelector(cfg, name); err != nil {
			fmt.Fprintf(opts.Log, "verify: %v\n", err)
			failPreAcquisition(result, CodeUsage, opts.now())
			return result
		}
	}
	selected := config.SelectedNames(cfg, opts.Selectors)
	hasService := false
	for _, name := range selected {
		if cfg.Processes[name].EffectiveMode() != config.ProcessModeTask {
			hasService = true
		}
	}
	if !hasService {
		fmt.Fprintln(opts.Log, "verify: the selection must include at least one service")
		failPreAcquisition(result, CodeUsage, opts.now())
		return result
	}
	if err := endpointNameCollisions(cfg, selected); err != nil {
		fmt.Fprintf(opts.Log, "verify: %v\n", err)
		failPreAcquisition(result, CodeUsage, opts.now())
		return result
	}
	workdir := filepath.Dir(opts.ConfigPath)
	if opts.Dir != "" {
		workdir = opts.Dir
		if !filepath.IsAbs(workdir) {
			workdir = filepath.Join(filepath.Dir(opts.ConfigPath), workdir)
		}
	}
	if info, statErr := os.Stat(workdir); statErr != nil || !info.IsDir() {
		fmt.Fprintf(opts.Log, "verify: --dir %s is not a directory\n", workdir)
		failPreAcquisition(result, CodeUsage, opts.now())
		return result
	}
	result.Workdir = workdir
	result.Check.Cwd = &workdir
	result.Startup.Validation = StatusPassed
	sourceBefore := observeSource(workdir, opts.now)
	result.Source = sourceBefore.source

	// ── acquisition ─────────────────────────────────────────────────────────
	result.Phase = PhaseAcquiring
	plan, err := session.Acquire(cfg, opts.ConfigPath, opts.Hash, opts.Session, opts.Selectors)
	if err != nil {
		fmt.Fprintf(opts.Log, "verify: %v\n", err)
		var lease *session.LeaseError
		switch {
		case errors.As(err, &lease) && lease.Instance():
			failPreAcquisition(result, CodeSessionOccupied, opts.now())
		case errors.As(err, &lease):
			failPreAcquisition(result, CodeResourceUnavailable, opts.now())
		default:
			failPreAcquisition(result, CodeInvalidConfig, opts.now())
		}
		result.Endpoints = map[string]Endpoint{}
		result.EndpointsNote = ptr("acquisition_incomplete")
		return result
	}
	// verify runs no preflight, so it gates on a crashed run here, under the
	// lease, before it would overwrite that run's PID file record.
	if err := runner.ResolveCrashedRun(paths.PID(opts.Hash), paths.Socket(opts.Hash)); err != nil {
		plan.Close()
		fmt.Fprintf(opts.Log, "verify: %v\n", err)
		failPreAcquisition(result, CodeSessionOccupied, opts.now())
		result.Endpoints = map[string]Endpoint{}
		result.EndpointsNote = ptr("acquisition_incomplete")
		return result
	}
	att := &attempt{opts: opts, result: result, plan: plan, selected: selected, errs: Errors{}, source: sourceBefore}
	return att.run(ctx)
}

type attempt struct {
	opts      Options
	result    *Result
	plan      *session.Plan
	selected  []string
	errs      Errors
	runner    *runner.Runner
	runDone   chan error
	interrupt bool
	source    sourceObservation

	cancelRun    func()
	closeRuntime func()

	// The cleanup budget starts once, at the first trigger, and every later
	// teardown step spends what remains of it. A check cancellation can set it
	// from the runner's goroutine, hence the lock.
	cleanupMu       sync.Mutex
	cleanupStart    time.Time
	cleanupDeadline time.Time
}

func (a *attempt) run(ctx context.Context) *Result {
	res, plan, opts := a.result, a.plan, a.opts
	attemptID, err := NewAttemptID(opts.now())
	if err != nil {
		plan.Close()
		failPreAcquisition(res, CodeReceiptPersistence, opts.now())
		return res
	}
	hostname, _ := os.Hostname()
	res.AttemptID = &attemptID
	res.RunID = &plan.Project.RunID
	res.Owner = &Owner{PID: os.Getpid(), Hostname: hostname}
	res.Endpoints = endpointsOf(plan.Project)
	for _, name := range a.selected {
		res.SelectedProcesses = append(res.SelectedProcesses, ProcessGeneration{Name: name})
	}
	receiptPath := filepath.Join(AttemptsDir(plan.Project.Directory), attemptID+".json")
	res.ReceiptPath = &receiptPath
	res.Retention = Retention
	journalPath := plan.Project.JournalPath
	res.Evidence.Journal = &journalPath

	// Active receipt before any process is launched.
	res.Kind, res.State, res.Phase = KindReceipt, StateActive, PhaseStarting
	res.Cleanup.Status, res.Cleanup.SignalingAuthority = StatusNotRun, "held"
	if err := a.opts.writeReceipt(receiptPath, res); err != nil {
		fmt.Fprintf(opts.Log, "verify: active receipt: %v\n", err)
		plan.Close()
		res.Kind, res.State = KindResponse, StateCompleted
		res.ReceiptPath = nil
		res.Retention = "no receipt created"
		res.Cleanup.Status, res.Cleanup.SignalingAuthority = StatusNotNeeded, "none"
		a.errs.Add(CodeReceiptPersistence)
		a.finishVerdict()
		return res
	}

	a.startAndCheck(ctx)
	a.cleanup()
	a.collectEvidence()
	sourceAfter := observeSource(res.Workdir, opts.now)
	if a.source.ok && sourceAfter.ok && !sourceAfter.equal(a.source) {
		res.Source.Limitations = appendUnique(res.Source.Limitations, "source_changed_during_attempt")
	}

	// Terminal receipt, then pruning, then lease release, then response.
	res.Phase = PhaseCompleted
	res.State = StateCompleted
	a.finishVerdict()
	res.ReceiptPersisted = true
	if err := a.persistTerminal(receiptPath); err != nil {
		fmt.Fprintf(opts.Log, "verify: terminal receipt: %v\n", err)
		res.ReceiptPersisted = false
		a.errs.Add(CodeReceiptPersistence)
		a.finishVerdict()
	} else if err := Prune(plan.Project.Directory, receiptPath); err != nil {
		fmt.Fprintf(opts.Log, "verify: prune receipts: %v\n", err)
	}
	if res.Cleanup.SignalingAuthority == "withdrawn" {
		plan.Close()
	}
	// Otherwise the leases stay held until this process exits: authority was not
	// proven withdrawn, so releasing early could hand the session to a new owner
	// while a signal is still possible.
	res.Kind = KindResponse
	return res
}

// persistTerminal writes the terminal receipt, falling back to a smaller copy
// without optional excerpts when the full one exceeds the size limit.
func (a *attempt) persistTerminal(path string) error {
	_, compact, err := Encode(a.result)
	if err != nil {
		return err
	}
	if compact {
		return fmt.Errorf("result exceeds %d bytes", MaxResultBytes)
	}
	return a.opts.writeReceipt(path, a.result)
}

func (a *attempt) startAndCheck(ctx context.Context) {
	res, plan, opts := a.result, a.plan, a.opts
	startupStart := opts.now()
	startupDeadline := startupStart.Add(opts.StartupTimeout)
	res.Startup.StartedAt, res.Startup.DeadlineAt = timestamp(startupStart), timestamp(startupDeadline)

	cfg := clampGrace(plan.Config)
	plan.Project.LogPath = paths.Log(opts.Hash)
	plan.Project.Owner = session.NewOwner("verify", opts.ConfigPath)
	if err := session.Save(paths.Metadata(opts.Hash), plan.Project); err != nil {
		fmt.Fprintf(opts.Log, "verify: session metadata: %v\n", err)
	}
	journal, err := logrotate.New(plan.Project.JournalPath, 10<<20, 3)
	if err != nil {
		fmt.Fprintf(opts.Log, "verify: event journal: %v\n", err)
		a.failStartup(CodeStartupFailed, StatusFailed)
		a.beginCleanup()
		return
	}
	socketPath, pidPath := paths.Socket(opts.Hash), paths.PID(opts.Hash)
	if err := daemon.WritePID(pidPath, os.Getpid(), socketPath); err != nil {
		fmt.Fprintf(opts.Log, "verify: pid file: %v\n", err)
	}
	server := ipc.NewServer(socketPath)
	server.SetProject(plan.Project)
	if err := server.Listen(); err != nil {
		fmt.Fprintf(opts.Log, "verify: IPC bind: %v\n", err)
		server = nil
	}

	r := &runner.Runner{
		Config:                cfg,
		ConfigPath:            opts.ConfigPath,
		Filter:                opts.Selectors,
		NoColor:               true,
		Silent:                true,
		Output:                opts.ProcessOutput,
		Journal:               journal,
		Project:               plan.Project,
		ResolveConfig:         plan.Resolve,
		CleanupHandoffTimeout: opts.CleanupTimeout - opts.sealReserve(),
		// Stack teardown gets what remains of the one cleanup budget minus the
		// seal reserve, from whichever trigger started it: verify's cleanup,
		// or the stack stopping on its own (a required process failing). A
		// check teardown that already spent the budget leaves only sealing.
		CleanupHandoffDeadline: func() time.Time { return a.beginCleanup().Add(-opts.sealReserve()) },
		// Teardown spends the same cleanup budget and leaves the seal reserve.
		TeardownDeadline: func() time.Time { return a.beginCleanup().Add(-opts.sealReserve()) },
		AttemptID:        *res.AttemptID,
		RecordPath:       pidPath,
	}
	if server != nil {
		r.IPC = server
	}
	a.runner = r
	runCtx, cancelRun := context.WithCancel(context.Background())
	a.runDone = make(chan error, 1)
	go func() { a.runDone <- r.Run(runCtx) }()
	a.cancelRun = func() {
		cancelRun()
	}
	a.closeRuntime = func() {
		if server != nil {
			server.Shutdown()
		}
		daemon.Cleanup(pidPath, socketPath)
		_ = journal.Close()
	}

	res.Phase = PhaseWaiting
	if !a.waitReady(ctx, startupDeadline) {
		a.beginCleanup()
		return
	}
	if opts.afterStartup != nil {
		opts.afterStartup(r)
	}

	// ── check ───────────────────────────────────────────────────────────────
	res.Phase = PhaseChecking
	checkStart := opts.now()
	checkDeadline := checkStart.Add(opts.CheckTimeout)
	res.Check.DeadlineAt = timestamp(checkDeadline)
	checkCtx, cancelCheck := context.WithDeadline(ctx, checkDeadline)
	defer cancelCheck()
	baseline := generations(r.Snapshot())
	monitorStop := make(chan struct{})
	monitorDone := make(chan map[string]bool, 1)
	go func() { monitorDone <- a.monitorRequired(baseline, monitorStop) }()
	check := r.RunCheck(checkCtx, runner.CheckSpec{
		Name: CheckProcessName,
		Argv: opts.Argv,
		Dir:  res.Workdir,
		Env:  a.checkEnv(),
		// Cancellation is the first cleanup trigger. The check's teardown
		// spends the shared budget but leaves one reserve for stack teardown
		// (grace is clamped to fit it) and one for sealing.
		CleanupDeadline: func() time.Time { return a.beginCleanup().Add(-2 * opts.sealReserve()) },
		MaxOutputBytes:  MaxOutputBytes,
	})
	close(monitorStop)
	failedRequired := <-monitorDone
	a.recordCheck(check, ctx, checkCtx)
	a.recordEnvironment(check.Started)
	for range failedRequired {
		a.errs.Add(CodeRequiredFailed)
	}
	a.recordGenerations(r.Snapshot())
	a.beginCleanup()
}

func (a *attempt) failStartup(code, status string) {
	a.result.Startup.Status = status
	a.result.Startup.FinishedAt = timestamp(a.opts.now())
	a.result.Startup.Errors = []ErrorRecord{{Code: code, Count: 1}}
	a.errs.Add(code)
}

// waitReady polls runner state until every selected process is ready, one
// fails, the deadline passes, or the caller interrupts.
func (a *attempt) waitReady(ctx context.Context, deadline time.Time) bool {
	res, r := a.result, a.runner
	cfg := r.Config
	observed := make(map[string]time.Time)
	owners := make(map[string]*ipc.EndpointOwner)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	outcome := ""
	failed := map[string]bool{}
	var last []ipc.ProcState
	for outcome == "" {
		last = r.Snapshot()
		byName := map[string]ipc.ProcState{}
		for _, st := range last {
			byName[st.Name] = st
		}
		allReady := len(byName) > 0
		for _, name := range a.selected {
			st, ok := byName[name]
			if !ok {
				allReady = false
				continue
			}
			isTask := cfg.Processes[name].EffectiveMode() == config.ProcessModeTask
			switch {
			case st.State == "failed" || (!isTask && (st.State == "exited" || st.State == "restarting")) || (isTask && st.State == "exited"):
				failed[name] = true
			case st.Ready:
				if _, seen := observed[name]; !seen {
					observed[name] = a.opts.now()
					owners[name] = st.ReadinessOwner
				}
			default:
				allReady = false
			}
		}
		switch {
		case len(failed) > 0:
			outcome = StatusFailed
		case allReady:
			outcome = StatusPassed
		default:
			select {
			case <-ctx.Done():
				outcome = StatusInterrupted
			case <-timer.C:
				outcome = StatusTimedOut
			case <-a.runDone:
				a.runDone = closedWith(nil)
				outcome = StatusFailed
			case <-ticker.C:
			}
		}
	}
	a.recordGenerations(last)
	byName := map[string]ipc.ProcState{}
	for _, st := range last {
		byName[st.Name] = st
	}
	// The loop can end on runDone with a snapshot taken before the runner's
	// last state change, so the owner an unready process's readiness refused
	// is read from a snapshot taken now.
	final := map[string]ipc.ProcState{}
	for _, st := range r.Snapshot() {
		final[st.Name] = st
	}
	for _, name := range a.selected {
		proc := cfg.Processes[name]
		obs := ReadinessObservation{Process: name, Generation: byName[name].Generation, Probe: probeKind(proc), Status: StatusNotRun}
		// A process that never became ready keeps the last answer its
		// readiness refused (foreign_listener: reject), if any.
		obs.Owner = final[name].ReadinessOwner
		if at, ok := observed[name]; ok {
			obs.ObservedAt = timestamp(at)
			obs.Owner = owners[name]
			obs.Status = StatusPassed
			if obs.Probe == nil {
				obs.Status = StatusNotConfig
			}
		} else if failed[name] {
			obs.Status = StatusFailed
		} else if outcome == StatusTimedOut && byName[name].Generation > 0 {
			obs.Status = StatusTimedOut
		}
		if obs.Probe == nil {
			res.Startup.UnprobedProcesses = append(res.Startup.UnprobedProcesses, name)
			if obs.Status == StatusPassed {
				obs.Status = StatusNotConfig
			}
		}
		res.Startup.ReadinessObservations = append(res.Startup.ReadinessObservations, obs)
	}
	switch outcome {
	case StatusPassed:
		res.Startup.Status = StatusPassed
		res.Startup.FinishedAt = timestamp(a.opts.now())
		return true
	case StatusInterrupted:
		a.interrupt = true
		res.Startup.Status = StatusInterrupted
		res.Startup.FinishedAt = timestamp(a.opts.now())
		a.errs.Add(CodeInterrupted)
	case StatusTimedOut:
		a.failStartup(CodeStartupTimeout, StatusTimedOut)
	default:
		a.failStartup(CodeStartupFailed, StatusFailed)
	}
	return false
}

func closedWith(err error) chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

func probeKind(proc config.Process) *string {
	if proc.EffectiveMode() == config.ProcessModeTask {
		return ptr("task_exit")
	}
	if proc.ReadyWhen == nil {
		return nil
	}
	switch {
	case proc.ReadyWhen.HTTP != "":
		return ptr("http")
	case proc.ReadyWhen.TCP != "":
		return ptr("tcp")
	case proc.ReadyWhen.Cmd != "":
		return ptr("cmd")
	case proc.ReadyWhen.Log != "":
		return ptr("log")
	}
	return nil
}

// monitorRequired records selected services that stop running, change
// generation, or become unhealthy (health_when) while the check runs; a zero
// check exit cannot hide them.
func (a *attempt) monitorRequired(baseline map[string]uint64, stop <-chan struct{}) map[string]bool {
	failed := map[string]bool{}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	inspect := func() {
		for _, st := range a.runner.Snapshot() {
			if st.Mode == config.ProcessModeTask {
				continue
			}
			if st.State != "running" || st.Generation != baseline[st.Name] || st.Health == "unhealthy" {
				failed[st.Name] = true
			}
		}
	}
	for {
		select {
		case <-stop:
			inspect()
			return failed
		case <-ticker.C:
			inspect()
		}
	}
}

func generations(states []ipc.ProcState) map[string]uint64 {
	out := map[string]uint64{}
	for _, st := range states {
		out[st.Name] = st.Generation
	}
	return out
}

func (a *attempt) recordGenerations(states []ipc.ProcState) {
	byName := generations(states)
	for i := range a.result.SelectedProcesses {
		if g, ok := byName[a.result.SelectedProcesses[i].Name]; ok && g > a.result.SelectedProcesses[i].Generation {
			a.result.SelectedProcesses[i].Generation = g
		}
	}
}

func (a *attempt) recordCheck(check runner.CheckResult, parent, checkCtx context.Context) {
	c := &a.result.Check
	if check.Started {
		g := check.Generation
		c.Generation = &g
		c.StartedAt, c.FinishedAt = timestamp(check.StartedAt), timestamp(check.FinishedAt)
	}
	c.ExitCode, c.Signal = check.ExitCode, check.Signal
	c.Stdout, c.Stderr, c.OutputTruncated = check.Stdout, check.Stderr, check.OutputTruncated
	interrupted := parent.Err() != nil
	timedOut := !interrupted && errors.Is(checkCtx.Err(), context.DeadlineExceeded)
	switch {
	case !check.Started && interrupted && errors.Is(check.Err, context.Canceled):
		// The interruption landed after readiness and refused the check before
		// it started. That is the interruption itself, not a start failure.
		a.interrupt = true
		c.Status = StatusInterrupted
		a.errs.Add(CodeInterrupted)
	case !check.Started:
		c.Status = StatusFailed
		a.errs.Add(CodeCheckStartFailed)
		if check.Err != nil {
			fmt.Fprintf(a.opts.Log, "verify: check did not start: %v\n", check.Err)
		}
		if interrupted {
			// A start failure coinciding with an interruption keeps both facts;
			// interruption still sets the overall precedence.
			a.interrupt = true
			a.errs.Add(CodeInterrupted)
		}
	case interrupted:
		a.interrupt = true
		c.Status = StatusInterrupted
		a.errs.Add(CodeInterrupted)
	case timedOut:
		c.TimedOut = true
		c.Status = StatusTimedOut
		a.errs.Add(CodeCheckTimeout)
	case check.ExitCode != nil && *check.ExitCode == 0:
		c.Status = StatusPassed
	default:
		c.Status = StatusFailed
		a.errs.Add(CodeCheckExitNonzero)
	}
	if check.HandedOff {
		c.Status = StatusUnverified
		if !timedOut && !interrupted {
			a.errs.Add(CodeCleanupUnverified)
		}
	}
}

// recordEnvironment reads the check's environment report and applies its
// verdict. A mismatch or an invalid report always fails the attempt; a missing
// or empty report fails it only when the environment is required. A check
// that never started leaves the environment not_run: its own failure is
// already recorded.
func (a *attempt) recordEnvironment(started bool) {
	env := &a.result.Environment
	if !started {
		return
	}
	path := environmentReportPath(a.plan.Project.Directory, *a.result.AttemptID)
	observations, err := readEnvironmentReport(path)
	// The receipt now carries the report; the file would only accumulate.
	_ = os.Remove(path)
	unknown := func(reason string) {
		env.Status, env.Reason = EnvironmentUnknown, ptr(reason)
		if env.Required || reason == ReasonReportInvalid {
			a.errs.Add(CodeEnvironmentUnverified)
		}
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		unknown(ReasonReportMissing)
	case err != nil:
		fmt.Fprintf(a.opts.Log, "verify: environment report: %v\n", err)
		unknown(ReasonReportInvalid)
	case len(observations) == 0:
		unknown(ReasonNoObservations)
	default:
		env.Observations, env.Status = observations, EnvironmentEligible
		for _, o := range observations {
			if o.Status != ObservationMatched {
				env.Status = EnvironmentIneligible
			}
		}
		if env.Status == EnvironmentIneligible {
			a.errs.Add(CodeEnvironmentIneligible)
		}
	}
}

// beginCleanup marks the first cleanup trigger, which starts the independent
// cleanup budget, and returns its absolute deadline. Later calls keep the
// first trigger's deadline.
func (a *attempt) beginCleanup() time.Time {
	a.cleanupMu.Lock()
	defer a.cleanupMu.Unlock()
	if a.cleanupStart.IsZero() {
		a.cleanupStart = a.opts.now()
		a.cleanupDeadline = a.cleanupStart.Add(a.opts.CleanupTimeout)
	}
	return a.cleanupDeadline
}

func (a *attempt) cleanup() {
	res := a.result
	deadline := a.beginCleanup()
	a.cleanupMu.Lock()
	cleanupStart := a.cleanupStart
	a.cleanupMu.Unlock()
	res.Phase = PhaseCleaningUp
	res.Cleanup.Attempted = true
	res.Cleanup.StartedAt, res.Cleanup.DeadlineAt = timestamp(cleanupStart), timestamp(deadline)
	if a.runner == nil {
		if a.closeRuntime != nil {
			a.closeRuntime()
		}
		res.Cleanup.Status, res.Cleanup.SignalingAuthority = StatusPassed, "withdrawn"
		res.Cleanup.TerminationVerified = ptr(true)
		res.Cleanup.FinishedAt = timestamp(a.opts.now())
		return
	}
	// Run reads its handoff budget through CleanupHandoffDeadline when it
	// observes this cancellation, or an earlier one of its own.
	a.cancelRun()
	returned := false
	timer := time.NewTimer(time.Until(deadline))
	select {
	case <-a.runDone:
		returned = true
	case <-timer.C:
	}
	timer.Stop()
	if a.closeRuntime != nil && returned {
		a.closeRuntime()
	}
	res.Cleanup.FinishedAt = timestamp(a.opts.now())
	a.recordTeardown()
	generation := generations(a.runner.Snapshot())
	if !returned {
		// Signalling authority was not proven sealed inside the budget.
		res.Cleanup.Status, res.Cleanup.SignalingAuthority = StatusUnverified, "unknown"
		res.Cleanup.TerminationVerified = ptr(false)
		for _, name := range a.selected {
			g := generation[name]
			res.Cleanup.UnresolvedProcesses = append(res.Cleanup.UnresolvedProcesses, UnresolvedProcess{Name: name, Generation: &g, Identity: "unavailable"})
		}
		res.Cleanup.Errors = []ErrorRecord{{Code: CodeCleanupUnverified, Count: 1}}
		a.errs.Add(CodeCleanupUnverified)
		return
	}
	res.Cleanup.SignalingAuthority = "withdrawn"
	unresolved := a.runner.Unresolved()
	if a.runner.ShutdownResult() == nil && len(unresolved) == 0 {
		res.Cleanup.Status = StatusPassed
		res.Cleanup.TerminationVerified = ptr(true)
		return
	}
	res.Cleanup.Status = StatusUnverified
	res.Cleanup.TerminationVerified = ptr(false)
	for _, u := range unresolved {
		entry := UnresolvedProcess{Name: u.Name, Identity: u.Identity}
		if u.Name == CheckProcessName {
			entry.Generation = ptr(uint64(1))
		} else if g, ok := generation[u.Name]; ok {
			entry.Generation = &g
		}
		res.Cleanup.UnresolvedProcesses = append(res.Cleanup.UnresolvedProcesses, entry)
	}
	res.Cleanup.Errors = []ErrorRecord{{Code: CodeCleanupUnverified, Count: 1}}
	a.errs.Add(CodeCleanupUnverified)
}

// recordTeardown copies the runner's teardown outcomes into the receipt and
// fails the attempt with teardown_failed when any did not pass. When Run did
// not return in time, only the outcomes recorded so far are present.
func (a *attempt) recordTeardown() {
	failed := false
	for _, t := range a.runner.TeardownResults() {
		a.result.Cleanup.Teardown = append(a.result.Cleanup.Teardown, TeardownRecord{
			Name: t.Name, Status: t.Status, ExitCode: t.ExitCode,
			StartedAt: timestamp(t.StartedAt), FinishedAt: timestamp(t.FinishedAt), Error: t.Error,
		})
		failed = failed || t.Status != runner.TeardownPassed
	}
	if failed {
		a.errs.Add(CodeTeardownFailed)
	}
}

// collectEvidence freezes the journal high-water mark for this run.
func (a *attempt) collectEvidence() {
	ev := &a.result.Evidence
	if ev.Journal == nil || a.runner == nil {
		return
	}
	runID := a.plan.Project.RunID
	first, err := logs.Query(*ev.Journal, logs.Options{RunID: runID, Limit: 1, MaxBytes: 1024})
	if err != nil {
		ev.ExcerptStatus = "not_collected"
		a.errs.Add(CodeEvidenceUnavailable)
		return
	}
	last, _ := logs.Query(*ev.Journal, logs.Options{RunID: runID, Limit: 1, MaxBytes: 1 << 20, Tail: true})
	if len(first.Events) > 0 {
		ev.FirstCursor = ptr(first.Events[0].ID)
	}
	if len(last.Events) > 0 {
		ev.LastCursor = ptr(last.Events[len(last.Events)-1].ID)
	}
	ev.Gap = first.Gap || last.Gap
	errorsPage, _ := logs.Query(*ev.Journal, logs.Options{RunID: runID, Level: "error", Limit: 20, MaxBytes: 8 << 10, Tail: true})
	for _, event := range errorsPage.Events {
		ev.Events = append(ev.Events, EvidenceEvent{ID: event.ID, Process: event.Process, Generation: event.Generation, Level: event.Level, Code: event.Code, Message: truncateUTF8(event.Line, 512)})
	}
	ev.Truncated = errorsPage.Truncated || errorsPage.HasMore
	ev.ExcerptStatus = "complete"
	if ev.Truncated {
		ev.ExcerptStatus = "truncated"
	}
	if a.result.Cleanup.Status != StatusPassed {
		ev.Truncated = true
		ev.ExcerptStatus = "incomplete_at_cleanup_deadline"
	}
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// finishVerdict applies the exit precedence without erasing phase facts.
func (a *attempt) finishVerdict() {
	res := a.result
	res.Errors = a.errs.Records()
	res.FinishedAt = timestamp(a.opts.now())
	switch {
	case a.interrupt:
		res.Status, res.ExitCode, res.Precedence = StatusInterrupted, ExitInterrupted, PrecedenceInterrupted
	case len(res.Errors) > 0:
		res.Status, res.ExitCode, res.Precedence = StatusFailed, ExitFailed, PrecedenceFailure
	default:
		res.Status, res.ExitCode, res.Precedence = StatusPassed, ExitPassed, PrecedencePassed
	}
}

func endpointsOf(project *ipc.Project) map[string]Endpoint {
	out := map[string]Endpoint{}
	for process, ports := range project.Ports {
		for name, port := range ports {
			out[process+"."+name] = Endpoint{Port: port, URL: project.URLs[process][name]}
		}
	}
	return out
}

// EnvName normalizes a process/port pair into PROC_COMPOSE_<KIND>_<PROC>_<PORT>.
func EnvName(kind, process, port string) string {
	norm := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c >= 'a' && c <= 'z':
				b.WriteByte(c - 'a' + 'A')
			case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
				b.WriteByte(c)
			default:
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	return "PROC_COMPOSE_" + kind + "_" + norm(process) + "_" + norm(port)
}

func endpointNameCollisions(cfg *config.Config, selected []string) error {
	seen := map[string]string{}
	for _, process := range selected {
		for port := range cfg.Processes[process].Ports {
			key := EnvName("PORT", process, port)
			if other, exists := seen[key]; exists {
				return fmt.Errorf("endpoints %s and %s.%s both map to %s", other, process, port, key)
			}
			seen[key] = process + "." + port
		}
	}
	return nil
}

func (a *attempt) checkEnv() map[string]string {
	project := a.plan.Project
	env := map[string]string{}
	for key, value := range a.opts.ExtraEnv {
		env[key] = value
	}
	for key, value := range map[string]string{
		"PROC_COMPOSE_SESSION":            a.result.Session,
		"PROC_COMPOSE_SESSION_DIR":        project.Directory,
		"PROC_COMPOSE_RUN_ID":             project.RunID,
		"PROC_COMPOSE_ENVIRONMENT_REPORT": environmentReportPath(project.Directory, *a.result.AttemptID),
	} {
		env[key] = value
	}
	for process, ports := range project.Ports {
		for name, port := range ports {
			env[EnvName("PORT", process, name)] = fmt.Sprint(port)
			env[EnvName("URL", process, name)] = project.URLs[process][name]
		}
	}
	return env
}

// clampGrace caps shutdown grace so graceful termination fits the cleanup
// schedule. It copies the config; the session's resolved config is unchanged.
func clampGrace(cfg *config.Config) *config.Config {
	out := *cfg
	out.Processes = make(map[string]config.Process, len(cfg.Processes))
	for name, proc := range cfg.Processes {
		if proc.ShutdownTimeout <= 0 || proc.ShutdownTimeout > maxGraceSeconds {
			proc.ShutdownTimeout = maxGraceSeconds
		}
		out.Processes[name] = proc
	}
	return &out
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// ── source provenance ───────────────────────────────────────────────────────

type sourceObservation struct {
	source   Source
	revision string
	status   string
	ok       bool
}

func (s sourceObservation) equal(other sourceObservation) bool {
	return s.ok && other.ok && s.revision == other.revision && s.status == other.status
}

func observeSource(dir string, now func() time.Time) sourceObservation {
	obs := sourceObservation{source: Source{Limitations: []string{}}}
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	revision, revErr := git("rev-parse", "HEAD")
	status, statusErr := git("status", "--porcelain", "--untracked-files=normal")
	if revErr == nil && revision != "" {
		obs.source.Revision = &revision
	} else {
		obs.source.Limitations = append(obs.source.Limitations, "revision_unavailable")
	}
	if statusErr == nil {
		dirty := status != ""
		obs.source.Dirty = &dirty
	} else {
		obs.source.Limitations = append(obs.source.Limitations, "dirty_state_unavailable")
	}
	if revErr == nil || statusErr == nil {
		obs.source.ObservedAt = timestamp(now())
	} else {
		obs.source.Limitations = append(obs.source.Limitations, "observation_unavailable")
	}
	obs.revision, obs.status, obs.ok = revision, status, revErr == nil && statusErr == nil
	sort.Strings(obs.source.Limitations)
	return obs
}
