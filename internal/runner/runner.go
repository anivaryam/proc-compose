// Package runner manages the lifecycle of configured processes: starting, restarting, and stopping.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anivaryam/proc-compose/internal/ansi"
	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// Runner starts and manages all configured processes.
type Runner struct {
	Config     *config.Config
	ConfigPath string // absolute path to config file (used for reload)
	Filter     []string
	LogFile    io.Writer   // if non-nil, all output is also written here (no ANSI)
	IPC        *ipc.Server // if non-nil, state + log events are broadcast to monitors
	NoColor    bool        // disable ANSI color output (also set via NO_COLOR env var)
	Verbose    bool        // enable verbose debug output
	LogFormat  string      // output format: "text" (default) or "json"
	HealthPort int         // if > 0, HTTP server listens on this port for /health
	Silent     bool        // suppress startup banner (daemon child sets this)
	Output     io.Writer   // nil uses stdout; daemon children use their rotating output file
	Journal    io.Writer   // canonical JSON-lines events, independently of human output
	// CleanupHandoffTimeout opts into sealing groups and quiescing lifecycle
	// workers after this independent shutdown budget. Zero preserves legacy Run
	// behavior and waits for every lifecycle goroutine.
	CleanupHandoffTimeout time.Duration
	Project               *ipc.Project
	// AttemptID stamps every journal event of this run with the owning
	// verification attempt, so diagnosis can scope evidence to one attempt.
	AttemptID     string
	ResolveConfig func(*config.Config) (*config.Config, error)
	RecordPath    string // daemon PID file that records launched process groups (Unix); empty disables recording
	// TeardownDeadline, when set, returns a time the teardown phase must end
	// by, earlier than its fixed budget would; verify passes its cleanup
	// deadline minus the seal reserve.
	TeardownDeadline func() time.Time
	logMu            sync.Mutex
	sequence         uint64
	maxName          int
	logErr           error
	lastStates       map[string]ipc.ProcState

	store atomic.Pointer[stateStore] // set during Run for command dispatch

	// cfgMu guards concurrent access to Config.Processes between Reload (writer)
	// and the per-process restart loop (reader). Run-time startup and
	// ListProcesses are single-threaded relative to Reload and need no lock.
	cfgMu sync.RWMutex

	// groups holds the live process group of every running managed process.
	// It exists so shutdown can act on all of them, not only the ones whose
	// goroutine is still parked in cmd.Wait: a descendant that escapes the
	// log pipe can keep a command alive long after its goroutine would
	// otherwise return. Entries live only for the duration of one run
	// iteration and are dropped as soon as the group is torn down. The map
	// itself is not persisted; the on-disk record of launched groups is kept
	// separately in records.
	groupsMu sync.Mutex
	groups   map[string]ProcessGroup
	// records mirrors, in RecordPath, every launched group that has not been
	// proven empty. It is a superset of the live groups: an entry is written
	// before the command starts and leaves only once its group is proven
	// empty, so a crash at any point leaves a record that covers whatever is
	// still running. recordMu is separate from groupsMu so that the synced
	// write never holds the registry lock. recordOn is false when RecordPath
	// is empty, on Windows, after the run's initial record failed, or once
	// the PID file has disappeared or come to name another daemon mid-run.
	recordMu sync.Mutex
	records  []*recEntry
	recordOn bool
	// processGroupFactory defaults to the platform implementation. Tests can
	// wrap the existing group to hold the start-to-track admission boundary.
	processGroupFactory func() ProcessGroup
	waitProcess         func(*exec.Cmd) error // optional controlled wait seam; defaults to cmd.Wait
	reapStarted         func(*singleReap)     // optional test seam, called after a reap starts
	reapersMu           sync.Mutex
	reapers             []*trackedReap
	checksMu            sync.Mutex
	checks              map[string]*procState // one-shot checks run against this stack
	// Check admission and quiescence for the current run, guarded by
	// lifecycleMu. Checks are runner-owned like managed processes: Run cancels
	// them when the stack stops and returns only after each has finished.
	activeChecks int
	checksClosed bool
	checkWG      *sync.WaitGroup
	checkCtx     context.Context // cancelled when the run stops admitting checks
	checkHandoff <-chan struct{} // the run's cleanup handoff; nil without a handoff budget

	// stopping is set once the run context is cancelled, so every process
	// goroutine learns that shutdown is in progress and treats leftovers in
	// its group as orphans to terminate rather than as backgrounded work.
	stopping atomic.Bool
	sealing  atomic.Bool // terminal cleanup has begun taking/sealing the group snapshot

	// stopCancel cancels the run context. It is stored (rather than captured)
	// because the shutdown request arrives on the IPC command goroutine.
	stopMu       sync.Mutex
	stopCancel   context.CancelFunc
	stopped      chan struct{}
	stopVerdict  chan error // buffered; carries the verdict to a waiting client
	stopProblems []string
	unresolved   []UnresolvedProcess
	// teardownResults is the teardown phase's outcome for the current run,
	// one entry per teardown process in run order.
	teardownResults []TeardownResult
	// teardownOpen admits teardown launches during the teardown phase. It is
	// guarded by lifecycleMu, like the admission it overrides.
	teardownOpen bool
	// teardownBudget replaces the fixed teardown budget when non-zero (test
	// seam).
	teardownBudget time.Duration

	// stopAck keeps the daemon alive until an in-flight "shutdown" command has
	// had its verdict written to the client. Run's exit seals and drains it.
	stopAck              ackBarrier
	lifecycleMu          sync.Mutex      // serializes launch/restart admission with shutdown
	runCtx               context.Context // guarded by lifecycleMu; detects cancellation before watcher closes admission
	admissionWatcherDone chan struct{}
}

// ackBarrier holds Run's exit until every in-flight shutdown command has
// finished delivering its verdict.
//
// A sync.WaitGroup cannot express this safely: Add may land concurrently with
// Wait, and Wait is then free to return before the holder it was meant to wait
// for has even registered. This barrier seals instead — once sealed no new
// holder can enter — and then drains to zero, which cannot race.
type ackBarrier struct {
	mu      sync.Mutex
	holders int
	sealed  bool
	drained chan struct{}
}

// enter registers a holder. It fails once the barrier is sealed, which is how a
// late-arriving command learns it must not hold up an already-finishing run.
func (b *ackBarrier) enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sealed {
		return false
	}
	if b.holders == 0 && b.drained == nil {
		b.drained = make(chan struct{})
	}
	b.holders++
	return true
}

// leave releases a holder, satisfying a pending seal once the last one exits.
// Every enter must be matched by exactly one leave, otherwise the seal waits for
// a holder that no longer exists.
func (b *ackBarrier) leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holders--
	if b.sealed && b.holders == 0 && b.drained != nil {
		close(b.drained)
		b.drained = nil
	}
}

// sealAndWait blocks new holders and waits for the current ones to finish. The
// timeout is a liveness backstop only: a holder always releases within its own
// bound, so this cannot release early with a verdict still undelivered.
func (b *ackBarrier) sealAndWait(timeout time.Duration) {
	b.mu.Lock()
	b.sealed = true
	if b.holders == 0 {
		if b.drained != nil {
			close(b.drained)
			b.drained = nil
		}
		b.mu.Unlock()
		return
	}
	ch := b.drained
	b.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}

// getProc returns a snapshot of a process definition under read lock.
func (r *Runner) getProc(name string) (config.Process, bool) {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	p, ok := r.Config.Processes[name]
	return p, ok
}

// refreshProc loads the current definition of p, which a reload may have
// replaced, for its next incarnation and records it in the state store, so the
// store snapshot and broadcastState describe the same definition.
func (r *Runner) refreshProc(p *procInfo) {
	updated, ok := r.getProc(p.name)
	if !ok {
		return
	}
	p.proc = updated
	if store := r.store.Load(); store != nil {
		store.setProc(p.name, updated)
	}
}

type procInfo struct {
	name       string
	proc       config.Process
	colorIndex int
	// argv, when set, is executed directly without shell parsing. Only one-shot
	// checks use it; configured processes keep their shell-string semantics.
	argv []string
	// capture, when set, receives every complete output line with its stream.
	capture func(stream, line string)
	// teardown marks a mode: teardown process run by the teardown phase. Such
	// a launch is admitted while teardownOpen although shutdown has closed
	// admission, and the terminal sweep's sealing flag does not hand it off.
	teardown bool
	// quiet marks a runner-internal execution, a ready_when.cmd or
	// health_when.cmd probe, that is not a managed process: emit drops every
	// event for it (output, lifecycle, errors) and its state is never
	// broadcast, so it stays out of the event stream, the IPC state cache and
	// status. Its process group is still tracked, recorded and sealed, and an
	// escape from it is still reported. It is recorded for crash recovery like
	// any launch, because a probe group can outlive a crashed daemon; for a
	// health_when.cmd that costs three PID-file writes per check (pending,
	// tracked, dropped), at most three a second at the 1s interval floor.
	quiet bool
}

func (r *Runner) Run(ctx context.Context) (result error) {
	if r.NoColor {
		ansi.SetDisabled(true)
	}

	procs := r.resolve()
	if len(procs) == 0 {
		return fmt.Errorf("no matching processes to run")
	}

	maxName := 0
	names := make([]string, 0, len(procs))
	for _, p := range procs {
		if len(p.name) > maxName {
			maxName = len(p.name)
		}
		names = append(names, p.name)
	}

	PrintBanner(names, maxName, BannerOptions{Silent: r.Silent || r.LogFormat == "json"})
	r.maxName = maxName

	// Seed IPC with initial state before any process starts.
	store := newStateStore(procs)
	r.store.Store(store)
	if r.Project == nil {
		r.Project = &ipc.Project{RunID: ipc.NewRunID(), ConfigPath: r.ConfigPath}
	}
	r.systemLog("info", "session.started", "session started")

	// Start health server if configured. Bind synchronously so a failure
	// surfaces to the caller (and aborts daemon startup) instead of silently
	// disappearing into a goroutine.
	if r.HealthPort > 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.HealthPort))
		if err != nil {
			return fmt.Errorf("health server bind failed on port %d: %w", r.HealthPort, err)
		}
		go r.serveHealth(ln, store)
		defer ln.Close()
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// IPC commands are served until Run returns, not only until the run is
	// cancelled: a client that asks during teardown, including a stop, must get
	// the daemon's answer rather than the server's 30s timeout. Registered
	// before endRun's defer, so it runs after endRun.
	commandsCtx, stopCommands := context.WithCancel(context.Background())
	defer stopCommands()
	// The shutdown handle must exist before any command can be dispatched:
	// handleCommands is the only caller of Shutdown, and a command that arrived
	// first would otherwise see a half-initialised runner.
	r.beginRun(runCtx, cancelRun)
	defer func() {
		r.endRun()
		r.logMu.Lock()
		logErr := r.logErr
		r.logMu.Unlock()
		if logErr != nil {
			result = errors.Join(result, fmt.Errorf("event journal: %w", logErr))
		}
	}()

	var metricsDone chan struct{}
	if r.IPC != nil {
		for _, st := range store.snapshot() {
			r.IPC.SetState(st)
		}
		go r.handleCommands(commandsCtx, store)
		metricsDone = make(chan struct{})
		go func() {
			defer close(metricsDone)
			r.collectMetrics(runCtx, store)
		}()
	}

	handoff := make(chan struct{})
	if r.CleanupHandoffTimeout <= 0 {
		handoff = nil
	}
	checkCtx, cancelChecks := context.WithCancel(runCtx)
	defer cancelChecks()
	checkWG := &sync.WaitGroup{}
	r.lifecycleMu.Lock()
	r.checkCtx, r.checkHandoff, r.checkWG, r.checksClosed = checkCtx, handoff, checkWG, false
	r.lifecycleMu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func(p procInfo) {
			defer wg.Done()
			for {
				result := r.runProcess(runCtx, p, maxName, store, handoff)
				if result.quiesced {
					return
				}
				if result.failFast {
					cancelRun()
				}
				st := store.get(p.name)
				r.lifecycleMu.Lock()
				st.mu.Lock()
				st.active = len(st.restartCh) > 0 && runCtx.Err() == nil
				st.mu.Unlock()
				// A running check keeps the stack open: its owner ends the run
				// explicitly, so the check is never sealed while still running.
				if r.IPC != nil && !store.anyActive() && r.activeChecks == 0 {
					r.stopping.Store(true)
					cancelRun()
				}
				// That holds without IPC as well. Returning here would let the
				// last worker finish, and finishing workers cancel running checks.
				checkRunning := r.activeChecks > 0
				r.lifecycleMu.Unlock()
				if (r.IPC == nil && !checkRunning) || p.proc.EffectiveMode() == config.ProcessModeTask || runCtx.Err() != nil {
					return
				}
				// A terminal service remains controllable while another process
				// keeps the daemon alive. Tasks retain one-shot semantics.
				select {
				case <-runCtx.Done():
					return
				case <-st.restartCh:
					r.refreshProc(&p)
					st.resetReady()
					st.mu.Lock()
					st.restarts++
					st.active = true
					st.mu.Unlock()
				}
			}
		}(p)
	}

	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		// Checks join the same quiescence: once no worker remains, refuse new
		// checks, cancel running ones, and wait until each has finished its
		// teardown and published its final state. Every Add happened under
		// lifecycleMu before checksClosed, so this Wait cannot race an Add.
		r.lifecycleMu.Lock()
		r.checksClosed = true
		r.lifecycleMu.Unlock()
		cancelChecks()
		checkWG.Wait()
		close(workersDone)
	}()
	handedOff := false
	if handoff == nil {
		<-workersDone
	} else {
		select {
		case <-workersDone:
		case <-runCtx.Done():
			timer := time.NewTimer(r.CleanupHandoffTimeout)
			select {
			case <-workersDone:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				// Seal every group before releasing workers to the handoff path.
				r.sweepGroups(runCtx)
				close(handoff)
				handedOff = true
				<-workersDone // return only after every runner worker is quiescent
			}
		}
	}

	// Every per-process goroutine has returned, but that only proves each
	// command's group *leader* exited. During shutdown, sweep whatever is still
	// tracked so the daemon never exits — and never lets `stop` report success
	// — while a managed process is still running.
	if !handedOff {
		r.sweepGroups(runCtx)
	}
	r.reportPendingReaps()
	// Teardown runs once every managed process is verified gone, while the
	// session's leases and ports are still held.
	r.runTeardown(handedOff, len(procs))
	// Stop and join runner-owned auxiliary workers before computing the final
	// result, so none can mutate state or emit events after Run returns.
	cancelRun()
	r.lifecycleMu.Lock()
	watcherDone := r.admissionWatcherDone
	r.lifecycleMu.Unlock()
	if watcherDone != nil {
		<-watcherDone
	}
	if metricsDone != nil {
		<-metricsDone
	}

	// ctx.Err() != nil means a signal triggered shutdown — not a failure.
	var runErr error
	var failedNames []string
	for _, st := range store.snapshot() {
		if st.State == "failed" {
			failedNames = append(failedNames, st.Name)
		}
	}
	sort.Strings(failedNames)
	if ctx.Err() == nil && len(failedNames) > 0 {
		runErr = fmt.Errorf("processes failed: %s", strings.Join(failedNames, ", "))
	}
	if stopErr := r.ShutdownResult(); stopErr != nil {
		if runErr != nil {
			runErr = fmt.Errorf("%v; %w", runErr, stopErr)
		} else {
			runErr = stopErr
		}
	}
	return errors.Join(runErr, r.TeardownError())
}

// beginRun publishes the cancellation handle and the completion channel that
// Shutdown uses. It must run before the IPC command loop starts, so a
// "shutdown" command can never observe a half-initialised runner.
func (r *Runner) beginRun(runCtx context.Context, cancel context.CancelFunc) {
	r.lifecycleMu.Lock()
	r.stopping.Store(false)
	r.sealing.Store(false)
	r.runCtx = runCtx
	r.lifecycleMu.Unlock()
	r.stopMu.Lock()
	r.stopCancel = cancel
	r.stopped = make(chan struct{})
	// Buffered so a verdict published by endRun never blocks, even when no
	// client is waiting for it.
	r.stopVerdict = make(chan error, 1)
	r.stopProblems = nil
	r.unresolved = nil
	r.teardownResults = nil
	r.stopMu.Unlock()

	// Mark shutdown as in progress the moment the run context is cancelled.
	// A signal (Ctrl+C, `stop`) cancels the parent context directly, so this
	// is the only place that observes it.
	done := make(chan struct{})
	r.lifecycleMu.Lock()
	r.admissionWatcherDone = done
	r.lifecycleMu.Unlock()
	go func() {
		defer close(done)
		<-runCtx.Done()
		r.closeAdmission()
	}()
	r.beginRecord()
}

// closeAdmission prevents new process starts and replacement requests. A
// launcher holds lifecycleMu from cmd.Start through group registration, so
// after this transition every successfully started process is already visible
// to cleanup.
func (r *Runner) closeAdmission() {
	r.lifecycleMu.Lock()
	r.stopping.Store(true)
	r.lifecycleMu.Unlock()
}

// launchAdmissionClosedLocked reports explicit shutdown or parent cancellation.
// The caller must hold lifecycleMu; checking the context closes the small window
// between cancellation and the watcher acquiring the lock.
func (r *Runner) launchAdmissionClosedLocked() bool {
	return r.stopping.Load() || (r.runCtx != nil && r.runCtx.Err() != nil)
}

// endRun publishes the shutdown verdict and unblocks anyone waiting on it.
//
// Ordering matters. `stopped` is closed first so a waiting "shutdown" command
// can send its verdict; Run then waits for that command to finish flushing the
// ack. The daemon's caller is still reading that ack and it carries the list
// of anything that could not be terminated, so it must be on the wire before
// main tears the IPC server down.
func (r *Runner) endRun() {
	termination := r.ShutdownResult()
	if termination == nil {
		r.systemLog("info", "shutdown.verified", "managed process cleanup verified")
	} else {
		r.systemLog("error", "shutdown.unverified", termination.Error())
	}
	// The published verdict also carries teardown failures, each line marked
	// with ipc.TeardownPrefix; termination alone stays in ShutdownResult.
	verdict := errors.Join(termination, r.TeardownError())

	// Publish first, drain second.
	//
	// A waiting shutdown command releases the ack barrier only after its verdict
	// has been written to the client, so draining before publishing would
	// deadlock against the very handler this run is waiting for. Publishing
	// first unblocks it; draining afterwards is what keeps the daemon alive
	// until the verdict has actually been written.
	r.stopMu.Lock()
	stopped := r.stopped
	stopVerdict := r.stopVerdict
	r.stopCancel = nil
	r.stopped = nil
	r.stopVerdict = nil
	r.stopMu.Unlock()

	if stopVerdict != nil {
		// Non-blocking: the channel is buffered and this is the only sender.
		select {
		case stopVerdict <- verdict:
		default:
		}
	}
	if stopped != nil {
		close(stopped)
	}

	r.stopAck.sealAndWait(shutdownFlushTimeout)
}

// shutdownFlushTimeout bounds how long Run's exit waits for an in-flight
// shutdown command to finish delivering its verdict. It is a liveness backstop
// only — the holder releases as soon as its own write attempt completes — so it
// never releases early with a verdict still undelivered.
const shutdownFlushTimeout = 10 * time.Second

func (r *Runner) resolve() []procInfo {
	var procs []procInfo

	colorIdx := 0
	for _, name := range config.SelectedNames(r.Config, r.Filter) {
		procs = append(procs, procInfo{
			name:       name,
			proc:       r.Config.Processes[name],
			colorIndex: colorIdx,
		})
		colorIdx++
	}
	return procs
}

// handleCommands reads IPC commands and dispatches them to process
// goroutines, replying via cmd.Reply so the IPC server can ack the client
// with a real verdict. Every code path must reply (or send a default error
// on unknown actions); otherwise the server's 30s wait timer fires.
func (r *Runner) handleCommands(ctx context.Context, store *stateStore) {
	commands := r.IPC.Commands()
	for {
		var cmd ipc.Command
		select {
		case <-ctx.Done():
			return
		case next, ok := <-commands:
			if !ok {
				return
			}
			cmd = next
		}
		var res ipc.CommandResult
		// holdsBarrier is the single source of truth for acquire/release pairing
		// here. Anything that acquires the barrier in one place and releases it
		// in another can drift out of balance — and an unbalanced barrier either
		// stalls Run's exit or unblocks it early.
		holdsBarrier := false
		switch cmd.Action {
		case "restart":
			if cmd.Process == "" {
				res = ipc.CommandResult{Status: "error", Message: "restart requires a process name"}
				break
			}
			r.cfgMu.RLock()
			proc, known := r.Config.Processes[cmd.Process]
			r.cfgMu.RUnlock()
			if !known {
				res = ipc.CommandResult{Status: "error", Message: fmt.Sprintf("unknown process %q", cmd.Process)}
				break
			}
			st := store.get(cmd.Process)
			if st == nil {
				res = ipc.CommandResult{Status: "error", Message: fmt.Sprintf("process %q is not currently managed", cmd.Process)}
				break
			}
			if proc.EffectiveMode() == config.ProcessModeTask {
				res = ipc.CommandResult{Status: "error", Message: fmt.Sprintf("process %q is a task and cannot be restarted; restart the stack to rerun tasks", cmd.Process)}
				break
			}
			res = r.requestRestart(st)
		case "reload":
			res = r.Reload()
		case "shutdown":
			// Take the barrier before requesting the shutdown, so Run cannot
			// finish — and tear down the IPC server — while this verdict is
			// still undelivered.
			holdsBarrier = r.stopAck.enter()
			if holdsBarrier {
				res = r.handleShutdown(cmd)
			} else {
				res = ipc.CommandResult{
					Status:  "error",
					Message: "daemon is already shutting down and cannot accept another stop request",
				}
			}
		default:
			res = ipc.CommandResult{Status: "error", Message: "unknown action: " + cmd.Action}
		}
		if cmd.Reply != nil {
			cmd.Reply <- res
		}
		if !holdsBarrier {
			continue
		}
		// The verdict is not delivered until the server has written the
		// acknowledgment carrying it, and only this goroutine can observe both
		// the reply and the flush. WaitFlushed returns on disconnect, write
		// failure and timeout alike, so a client that goes away cannot wedge
		// the daemon — and the barrier is released on every path.
		cmd.WaitFlushed(shutdownFlushTimeout)
		r.stopAck.leave()
	}
}

func (r *Runner) requestRestart(st *procState) ipc.CommandResult {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	return r.requestRestartLocked(st)
}

// requestRestartLocked is requestRestart with lifecycleMu already held. Reload
// uses it to make config replacement and restart admission one transition.
func (r *Runner) requestRestartLocked(st *procState) ipc.CommandResult {
	if r.launchAdmissionClosedLocked() {
		return ipc.CommandResult{Status: "error", Message: "daemon is shutting down"}
	}
	st.mu.Lock()
	st.active = true
	generation := st.generation + 1
	st.mu.Unlock()
	st.requestRestart()
	return ipc.CommandResult{Status: "ok", Generation: generation}
}

// shutdownAckTimeout bounds how long a "shutdown" command waits for the stack
// to finish tearing itself down. It sits below the IPC server's 30s ack
// timeout so a client gets the daemon's real verdict rather than the server's
// generic "timed out waiting for runner reply".
const shutdownAckTimeout = 25 * time.Second

// handleShutdown stops the whole stack on request and reports the verified
// outcome. It is the path both `proc-compose stop` and `stop --force` use:
// signalling the daemon outright would destroy the only record of which process
// groups it manages, and orphan every one of them.
//
// It does NOT take the ack barrier. The caller owns that, because only the caller
// can also observe the reply and the flush that follow.
func (r *Runner) handleShutdown(cmd ipc.Command) ipc.CommandResult {
	stopped, verdictCh, err := r.Shutdown(cmd.Force)
	if err != nil {
		return ipc.CommandResult{Status: "error", Message: err.Error()}
	}

	select {
	case <-stopped:
	case <-time.After(shutdownAckTimeout):
		return ipc.CommandResult{
			Status:  "error",
			Message: fmt.Sprintf("daemon did not finish shutting down within %s; managed processes were not verified", shutdownAckTimeout),
		}
	}

	// The verdict is published by endRun immediately after the final sweep, so
	// it is available as soon as <-stopped fires. A verdict that never arrives
	// is reported as "not verified" rather than defaulted to success.
	var verdict error
	select {
	case verdict = <-verdictCh:
	case <-time.After(shutdownVerdictTimeout):
		verdict = errors.New("daemon finished shutting down without publishing a verdict")
	}

	if verdict != nil {
		// "partial" rather than "error": the stack is down, but termination
		// could not be fully verified. The caller must not read this as a
		// clean stop.
		return ipc.CommandResult{Status: "partial", Message: verdict.Error()}
	}
	return ipc.CommandResult{Status: "ok"}
}

// shutdownVerdictTimeout bounds the wait for endRun's verdict. It is generous
// because the two are published together, and exists only so a lost verdict
// degrades to "not verified" instead of hanging the daemon.
const shutdownVerdictTimeout = 5 * time.Second

// serveHealth runs the HTTP server on the given pre-bound listener.
// The listener is bound synchronously by Run so bind failures abort startup.
func (r *Runner) serveHealth(ln net.Listener, store *stateStore) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, req *http.Request) {
		ready := true
		states := store.snapshot()
		for _, state := range states {
			if !state.Ready {
				ready = false
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ready": ready, "processes": states})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		r.cfgMu.RLock()
		defer r.cfgMu.RUnlock()
		procs := store.snapshot()
		type procStatus struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			Restarts int    `json:"restarts"`
			Health   string `json:"health,omitempty"`
		}
		status := make([]procStatus, 0, len(procs))
		allHealthy := true
		for _, p := range procs {
			status = append(status, procStatus{Name: p.Name, State: p.State, Restarts: p.Restarts, Health: p.Health})
			healthy := false
			if p.Mode == config.ProcessModeTask {
				healthy = p.State == "completed"
			} else {
				// A service whose health_when is failing is unhealthy even
				// while it runs; /ready keeps reporting the startup latch.
				healthy = (p.State == "running" || p.State == "restarting") && p.Health != "unhealthy"
			}
			if !healthy {
				allHealthy = false
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if allHealthy {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"healthy":   allHealthy,
			"processes": status,
			"timestamp": time.Now().Format(time.RFC3339),
		}); err != nil && r.Verbose {
			fmt.Fprintf(os.Stderr, "health encode: %v\n", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "not found"}); err != nil && r.Verbose {
			fmt.Fprintf(os.Stderr, "health encode: %v\n", err)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		// Always surface errors here — bind already succeeded so anything
		// reaching this branch is a runtime serve failure (clients dropped,
		// listener torn down) that operators want to see.
		fmt.Fprintf(os.Stderr, "health server error: %v\n", err)
	}
}

// collectMetrics periodically gathers CPU and memory metrics for all running
// processes and broadcasts updated state to connected monitors.
func (r *Runner) collectMetrics(ctx context.Context, store *stateStore) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running := store.updateMetrics()
			for _, name := range running {
				// The store holds the definition of the running incarnation,
				// not the one loaded at startup.
				if p, ok := store.info(name); ok {
					st := store.get(name)
					if st != nil {
						r.broadcastState(p, st)
					}
				}
			}
		}
	}
}
