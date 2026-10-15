package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

type processRunResult struct {
	failed   bool
	failFast bool
	quiesced bool
}

// runProcess runs the lifecycle loop for a single process.
// Returns failure metadata when the process ended in the "failed" state.
var errCleanupHandoff = errors.New("runner cleanup handoff")

func (r *Runner) runProcess(ctx context.Context, p procInfo, maxName int, store *stateStore, handoff <-chan struct{}) processRunResult {
	st := store.get(p.name)
	backoff := time.Second

	// Wait for all dependencies to become ready before starting.
	for _, dep := range p.proc.DependsOn {
		depSt := store.get(dep)
		if depSt == nil {
			continue // validated at config load; defensive guard
		}
		r.processLog(p, "info", "dependency.wait", fmt.Sprintf("waiting for %s...", dep))
		select {
		case <-handoff:
			return processRunResult{quiesced: true}
		case <-ctx.Done():
			depSt.mu.Lock()
			depFailed := depSt.readyClosed && !depSt.readyOK
			depSt.mu.Unlock()
			if depFailed {
				msg := fmt.Sprintf("dependency %q failed", dep)
				r.processLog(p, "error", "dependency.failed", msg)
				st.mu.Lock()
				st.state = "failed"
				st.lastError = msg
				st.mu.Unlock()
				r.broadcastState(p, st)
				st.markReady(false)
				return processRunResult{failed: true}
			}
			return processRunResult{}
		case <-depSt.readyChannel():
			depSt.mu.Lock()
			depOK := depSt.readyOK
			depSt.mu.Unlock()
			if !depOK {
				msg := fmt.Sprintf("dependency %q failed", dep)
				r.processLog(p, "error", "dependency.failed", msg)
				st.mu.Lock()
				st.state = "failed"
				st.lastError = msg
				st.mu.Unlock()
				r.broadcastState(p, st)
				st.markReady(false)
				return processRunResult{failed: true}
			}
		}
	}

	if p.proc.EffectiveMode() == config.ProcessModeTask {
		return r.runTaskProcess(ctx, p, maxName, st, handoff)
	}

	for {
		// Readiness settings belong to an incarnation, including after reload.
		r.refreshProc(&p)
		var logRe *regexp.Regexp
		if p.proc.ReadyWhen != nil && p.proc.ReadyWhen.Log != "" {
			logRe = regexp.MustCompile(p.proc.ReadyWhen.Log)
		}
		readyTimeout := resolveReadyTimeout(p.proc.ReadyTimeout)
		st.mu.Lock()
		st.startedAt = time.Now()
		st.state = "starting"
		st.pid = 0
		st.generation++
		gen := st.generation
		st.lastError = ""
		st.exitCode = nil
		st.probeFailed = false
		st.readinessOwner, st.healthOwner = nil, nil
		st.health, st.healthFailures, st.healthError = "", 0, ""
		if p.proc.HealthWhen != nil {
			st.health = healthUnknown
		}
		st.mu.Unlock()
		r.broadcastState(p, st)

		// Create a per-iteration context so restartCh and probe failures can
		// kill just this run without affecting the parent runner.
		procCtx, procCancel := context.WithCancel(ctx)
		restartTriggered := make(chan struct{})
		go func() {
			select {
			case <-st.restartCh:
				close(restartTriggered)
				procCancel()
			case <-procCtx.Done():
			}
		}()

		// A cmd probe runs processes, so it is joined on every exit from this
		// incarnation: none of its events may outlive the incarnation or Run.
		// The health monitor is joined the same way.
		var probes sync.WaitGroup
		if p.proc.HealthWhen != nil {
			probes.Add(1)
			go func() {
				defer probes.Done()
				r.monitorHealth(procCtx, procCancel, p, st, gen, handoff)
			}()
		}
		onStart, onLine := r.buildReadinessCallbacks(p, maxName, st, gen, logRe, readyTimeout, procCtx, procCancel, handoff, &probes, func(msg string) {
			r.processLog(p, "debug", "readiness.progress", msg)
		})

		startTime := time.Now()
		err := r.exec(procCtx, p, maxName, st, onStart, onLine, handoff)
		if errors.Is(err, errCleanupHandoff) {
			procCancel()
			probes.Wait()
			return processRunResult{quiesced: true}
		}
		procCancel() // clean up the restartCh goroutine
		probes.Wait()

		// Reset backoff if the process ran successfully for a meaningful duration.
		if time.Since(startTime) > backoff {
			backoff = time.Second
		}
		// Ensure readyCh is closed so depends_on waiters never deadlock.
		// No-op if markReady was already called by a successful probe.
		st.markReady(false)

		// Check if parent context was cancelled (SIGTERM/SIGINT shutdown).
		if ctx.Err() != nil || r.stopping.Load() {
			st.mu.Lock()
			st.state = "exited"
			st.mu.Unlock()
			r.broadcastState(p, st)
			r.processLog(p, "info", "process.stopped", "stopped")
			return processRunResult{}
		}

		// Check if this was a requested restart (not a natural exit).
		wasRestart := false
		select {
		case <-restartTriggered:
			wasRestart = true
		default:
		}

		if wasRestart {
			// Pick up any config changes applied by Reload().
			r.refreshProc(&p)
			st.resetReady()
			r.processLog(p, "info", "process.restart_requested", "restarting (requested)...")
			st.mu.Lock()
			st.restarts++
			st.state = "restarting"
			st.mu.Unlock()
			r.broadcastState(p, st)
			backoff = time.Second
			continue
		}

		// A readiness failure cancelled this incarnation. The reap can still
		// report a clean exit: os/exec returns the context error only when its
		// own cancel hook signalled the group, and when the group was already
		// withdrawn a command that exited 0 on SIGTERM is reaped as status 0.
		// The probe's verdict decides instead: this is a failure for restart,
		// max_restarts and the run's result.
		st.mu.Lock()
		probeFailed, probeError := st.probeFailed, st.lastError
		st.mu.Unlock()
		if err == nil && probeFailed {
			err = errors.New(probeError)
		}

		if err == nil {
			st.mu.Lock()
			st.state = "exited"
			st.mu.Unlock()
			r.broadcastState(p, st)

			if p.proc.Restart == "always" {
				st.mu.Lock()
				next := st.restarts + 1
				st.mu.Unlock()
				if p.proc.MaxRestarts > 0 && next > p.proc.MaxRestarts {
					return processRunResult{failed: r.giveUp(p, maxName, st)}
				}
				r.processLog(p, "info", "process.restarting", "exited, restarting...")
				// A fresh cycle means a fresh readiness latch: dependents must
				// wait for the new incarnation's probe, not the dead one's.
				st.resetReady()
				st.mu.Lock()
				st.restarts++
				st.state = "restarting"
				st.mu.Unlock()
				r.broadcastState(p, st)
				if waitForCleanupHandoff(backoff, handoff) {
					return processRunResult{quiesced: true}
				}
				continue
			}
			r.processLog(p, "info", "process.exited", "exited")
			return processRunResult{}
		}

		r.processLog(p, "error", "process.failed", fmt.Sprintf("exited: %v", err))
		st.mu.Lock()
		if st.lastError == "" {
			st.lastError = err.Error()
		}
		st.mu.Unlock()

		if p.proc.Restart == "never" {
			st.mu.Lock()
			st.state = "failed"
			st.mu.Unlock()
			r.broadcastState(p, st)
			return processRunResult{failed: true}
		}

		st.mu.Lock()
		next := st.restarts + 1
		st.mu.Unlock()
		if p.proc.MaxRestarts > 0 && next > p.proc.MaxRestarts {
			return processRunResult{failed: r.giveUp(p, maxName, st)}
		}

		// Fresh cycle, fresh readiness latch — same reasoning as the
		// automatic-restart branch above.
		st.resetReady()
		st.mu.Lock()
		st.restarts++
		st.state = "restarting"
		st.mu.Unlock()
		r.broadcastState(p, st)
		r.processLog(p, "info", "process.restarting", fmt.Sprintf("restarting in %s...", backoff))

		select {
		case <-ctx.Done():
			return processRunResult{}
		case <-st.restartCh:
			// Restart requested during backoff — skip the wait. Readiness
			// was already reset above, before the wait started.
			backoff = time.Second
			continue
		case <-time.After(backoff):
		}

		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (r *Runner) runTaskProcess(ctx context.Context, p procInfo, maxName int, st *procState, handoff <-chan struct{}) processRunResult {
	st.mu.Lock()
	st.startedAt = time.Now()
	st.state = "starting"
	st.pid = 0
	st.generation++
	st.mu.Unlock()
	r.broadcastState(p, st)

	err := r.exec(ctx, p, maxName, st, nil, nil, handoff)
	if errors.Is(err, errCleanupHandoff) {
		return processRunResult{quiesced: true}
	}

	st.mu.Lock()
	st.pid = 0
	st.mu.Unlock()

	if ctx.Err() != nil {
		st.mu.Lock()
		st.state = "exited"
		st.mu.Unlock()
		r.broadcastState(p, st)
		r.processLog(p, "info", "process.stopped", "stopped")
		return processRunResult{}
	}

	if err == nil {
		st.mu.Lock()
		st.state = "completed"
		st.mu.Unlock()
		st.markReady(true)
		r.broadcastState(p, st)
		r.processLog(p, "info", "task.completed", "completed")
		return processRunResult{}
	}

	msg := fmt.Sprintf("task failed: %v", err)
	r.processLog(p, "error", "task.failed", msg)
	st.mu.Lock()
	st.state = "failed"
	st.lastError = msg
	st.mu.Unlock()
	st.markReady(false)
	r.broadcastState(p, st)
	return processRunResult{failed: true, failFast: true}
}

// buildReadinessCallbacks builds onStart and onLine hooks bound to the
// current run iteration's context. Each ready_when probe wires its own pair.
// progressFn is called periodically during probe to show waiting progress.
// A cmd probe runs under handoff and is counted in probes, which the caller
// joins before the incarnation ends.
func (r *Runner) buildReadinessCallbacks(
	p procInfo,
	maxName int,
	st *procState,
	gen uint64,
	logRe *regexp.Regexp,
	readyTimeout time.Duration,
	procCtx context.Context,
	procCancel context.CancelFunc,
	handoff <-chan struct{},
	probes *sync.WaitGroup,
	progressFn func(string),
) (onStart func(), onLine func(string)) {
	switch {
	case p.proc.ReadyWhen == nil:
		onStart = func() {
			if !st.markReadyFor(procCtx, gen, true, nil) {
				return
			}
			r.broadcastState(p, st)
			r.processLog(p, "info", "process.ready", "ready")
		}
	case p.proc.ReadyWhen.HTTP != "":
		probeURL := p.proc.ReadyWhen.HTTP
		onStart = func() {
			go func() {
				check := r.newOwnerCheck(p, st, gen, "http")
				ok, rejected := probeHTTP(procCtx, probeURL, readyTimeout, func(msg string) {
					r.processLog(p, "debug", "readiness.progress", msg)
				}, func(msg string) {
					r.processLog(p, "debug", "readiness.progress", msg)
				}, check.attribute, check.accept)
				r.handleProbeResult(p, st, gen, "http", ok, rejected, check.owner, procCtx, procCancel)
			}()
		}
	case p.proc.ReadyWhen.TCP != "":
		probeAddr := p.proc.ReadyWhen.TCP
		onStart = func() {
			go func() {
				check := r.newOwnerCheck(p, st, gen, "tcp")
				ok, rejected := probeTCP(procCtx, probeAddr, readyTimeout, func(msg string) {
					r.processLog(p, "debug", "readiness.progress", msg)
				}, func(msg string) {
					r.processLog(p, "debug", "readiness.progress", msg)
				}, check.attribute, check.accept)
				r.handleProbeResult(p, st, gen, "tcp", ok, rejected, check.owner, procCtx, procCancel)
			}()
		}
	case p.proc.ReadyWhen.Cmd != "":
		onStart = func() {
			probes.Add(1)
			go func() {
				defer probes.Done()
				ok, detail := r.probeCmd(procCtx, p, handoff, readyTimeout, func(msg string) {
					r.processLog(p, "debug", "readiness.progress", msg)
				}, progressFn)
				r.handleProbeResult(p, st, gen, "cmd", ok, detail, nil, procCtx, procCancel)
			}()
		}
	case logRe != nil:
		if readyTimeout > 0 {
			go func() {
				select {
				case <-procCtx.Done():
				case <-time.After(readyTimeout):
					msg := fmt.Sprintf("readiness probe (log) timed out after %s", readyTimeout)
					if st.failReadinessFor(procCtx, gen, msg) {
						r.processLog(p, "error", "readiness.failed", msg)
						procCancel()
					}
				}
			}()
		}
		onLine = func(line string) {
			if logRe.MatchString(line) && st.markReadyFor(procCtx, gen, true, nil) {
				r.broadcastState(p, st)
				r.processLog(p, "info", "process.ready", "ready (log match)")
			}
		}
	}
	return onStart, onLine
}

// exec runs a single invocation of the process command.
// onStart is called once after cmd.Start() succeeds (may be nil).
// onLine is called for each stdout/stderr line (may be nil).
func (r *Runner) exec(ctx context.Context, p procInfo, maxName int, st *procState, onStart func(), onLine func(string), handoff <-chan struct{}) error {
	var cmd *exec.Cmd
	if len(p.argv) > 0 {
		cmd = exec.CommandContext(ctx, p.argv[0], p.argv[1:]...)
	} else {
		shell, args := shellCommand(p.proc.Cmd)
		cmd = exec.CommandContext(ctx, shell, args...)
	}

	// Per-process shutdown_timeout is the window between the graceful group
	// signal and the forced group kill. Setting it on the group (not just on
	// cmd.WaitDelay) is what makes a SIGTERM-ignoring command's descendants
	// get killed too.
	grace := shutdownGrace(p.proc.ShutdownTimeout)

	pg := r.createProcessGroup()
	pg.Setup(cmd)
	if p.proc.ShutdownTimeout > 0 {
		cmd.WaitDelay = grace
	}

	if p.proc.Dir != "" {
		cmd.Dir = p.proc.Dir
	}

	cmd.Env = os.Environ()
	for k, v := range p.proc.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	// Admission, start, tracking, and registry insertion are one transition with
	// shutdown. If cancellation races Start, the group is still registered before
	// shutdown can take its sealing snapshot; if admission already closed, nothing
	// is launched.
	r.lifecycleMu.Lock()
	if (r.stopping.Load() && !(p.teardown && r.teardownOpen)) || ctx.Err() != nil {
		r.lifecycleMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = pg.Close()
		return context.Canceled
	}
	// Record the launch before it happens. A crash after Start must find the
	// group in the record, so a launch that cannot be recorded is refused
	// (unless the PID file itself is gone; see recordPending).
	entry, err := r.recordPending(p.name, pg)
	if err != nil {
		r.lifecycleMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = pg.Close()
		return fmt.Errorf("record process group: %w", err)
	}
	if err := cmd.Start(); err != nil {
		r.recordDrop(entry)
		r.lifecycleMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = pg.Close()
		return fmt.Errorf("start: %w", err)
	}
	if err := pg.Track(cmd); err != nil {
		if cmd.Process != nil {
			if killErr := cmd.Process.Kill(); killErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to kill stray process: %v\n", killErr)
			}
		}
		observation, sealErr := pg.Seal()
		if observation.State == GroupEmpty && sealErr == nil {
			r.recordDrop(entry)
		}
		reap := &singleReap{}
		r.startReap(p.name, observation.Identity, cmd, reap)
		r.lifecycleMu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		if observation.State != GroupEmpty || sealErr != nil {
			r.recordUnverified(p.name, observation.Identity, fmt.Sprintf("%s: failed to establish process-group tracking; seal state=%s identity=%s error=%v", p.name, observation.State, observation.Identity, sealErr))
		}
		if handoff != nil {
			select {
			case <-reap.done:
			case <-handoff:
				return errCleanupHandoff
			}
		} else {
			_ = reap.recv()
		}
		return fmt.Errorf("track: %w", err)
	}
	// Register before releasing lifecycleMu so shutdown's snapshot cannot miss
	// a successfully started process.
	groupIdentity := pg.Identify()
	r.trackGroup(p.name, pg, grace)
	// The leader is this runner's unreaped child here, so its pid and start
	// time still describe it.
	r.recordTracked(entry, cmd.Process.Pid)
	r.lifecycleMu.Unlock()

	st.mu.Lock()
	st.pid = cmd.Process.Pid
	st.state = "running"
	st.mu.Unlock()
	r.broadcastState(p, st)

	// Observe the command leader's exit without reaping it.
	//
	// This is what keeps the group's identity ours. While the leader is
	// unreaped the kernel keeps its process-table slot reserved, and because
	// Setpgid made its PID equal the process-group ID, that numeric identifier
	// cannot be handed to an unrelated group. Reaping is therefore deferred to
	// the very end, after every signal the runner intends to send.
	//
	// Setup must not block: a long-running service would otherwise delay its own
	// start. The observer is built once and reports whether the leader has
	// already exited.
	var observation exitObservation
	observer, obsErr := observeLeaderExit(cmd.Process.Pid)
	if obsErr != nil {
		// The leader's waitable state could not be established, so its
		// identifier can no longer be proven to be ours. Authority is withdrawn
		// BEFORE anything else so that no group signal can be emitted on the
		// strength of an identifier we cannot vouch for.
		pg.Withdraw()
		// The command is still running and this runner started it, so
		// reclaiming it is cleanup the runner owns rather than a group signal:
		// cmd.Process is this process's own handle, not a stale numeric ID.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		reap := &singleReap{}
		r.startReap(p.name, groupIdentity, cmd, reap)
		if handoff != nil {
			select {
			case <-reap.done:
			case <-handoff:
				_ = stdout.Close()
				_ = stderr.Close()
				return errCleanupHandoff
			}
		} else {
			_ = reap.recv()
		}
		return fmt.Errorf("observe leader exit: %w", obsErr)
	}

	// Releases the log reader if something outside the group still holds the
	// pipe after the group has been killed.
	var escaped atomic.Bool
	closeStdout := sync.OnceFunc(func() { _ = stdout.Close(); _ = stderr.Close() })

	if onStart != nil {
		onStart()
	}

	// The log reader runs on its own goroutine so that reading output is
	// independent of observing the exit. A service that closes or redirects its
	// own output reaches end-of-stream while still working, so end-of-stream is
	// an observation about the output and never a reason to terminate anything.
	scanDone := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); r.readOutput(p, "stdout", stdout, onLine) }()
	go func() { defer readers.Done(); r.readOutput(p, "stderr", stderr, onLine) }()
	go func() { readers.Wait(); close(scanDone) }()

	// The exit observer runs for the lifetime of the command. It never reaps:
	// WNOWAIT on Linux, and an EVFILT_PROC knote on macOS, both report the exit
	// while leaving the leader a zombie, and therefore still holding its
	// process-table slot. It is joined before the reap so that nothing can
	// collect the leader out from under the runner.
	observeCh := make(chan exitObservation, 1)
	obsStop := make(chan struct{})
	stopObserver := sync.OnceFunc(func() { close(obsStop) })
	var observeDone chan struct{}
	// reap reports the command's exit status exactly once, whenever cmd.Wait is
	// run on this goroutine's behalf.
	var reap singleReap
	if observer != nil {
		observeDone = make(chan struct{})
		go func() {
			defer close(observeDone)
			observeCh <- observer.Observe(obsStop)
		}()
	} else {
		// No reap-free observation: the exit can only be seen by reaping.
		//
		// Where that lands depends on the containment. A durable platform keeps
		// its authority across the reap and is torn down all the same; elsewhere
		// the reap is what releases the leader's reservation, so a natural
		// completion can no longer signal anything.
		r.startReap(p.name, groupIdentity, cmd, &reap)
		if !durableContainment {
			observation = exitUnsupported
		}
	}
	finishHandoff := func() error {
		closeStdout()
		if observer != nil {
			stopObserver()
			<-observeDone
		}
		<-scanDone
		if !reap.started() {
			r.startReap(p.name, groupIdentity, cmd, &reap)
		}
		return errCleanupHandoff
	}

	cancelled := false
	switch {
	case observer != nil:
		select {
		case <-handoff:
			return finishHandoff()
		case <-ctx.Done():
			cancelled = true
		case observation = <-observeCh:
		}
	case durableContainment:
		select {
		case <-handoff:
			return finishHandoff()
		case <-ctx.Done():
			cancelled = true
		case <-reap.done:
			// The leader is collected, but this platform's containment is a
			// durable kernel handle, so authority is unaffected and the group
			// is still torn down normally.
			observation = exitObserved
		}
	default:
		select {
		case <-handoff:
			return finishHandoff()
		case <-ctx.Done():
			cancelled = true
		case <-reap.done:
			// The leader has been collected, so its identifier may already
			// have been released. Nothing below may signal the group.
			observation = exitOwnershipLost
		}
	}

	// TEARDOWN. Both triggers arrive here — a cancelled incarnation and a leader
	// that exited on its own — because a command that backgrounds work and then
	// exits still owns that work, and it has to be terminated and verified like
	// any other managed tree rather than left running and merely reported.
	//
	// Every signal here is sent while the leader is unreaped.
	if signallingEstablished(observation, cancelled) {
		terminateGroup(pg, grace)
	} else {
		r.recordUnverified(p.name, groupIdentity, fmt.Sprintf(
			"%s: the command leader's identity could not be established (%s), so no process "+
				"group signal was sent; its descendants could not be terminated by proc-compose",
			p.name, observation))
	}

	// Join the observer before the reap. Its blocking call ends when the leader
	// the teardown just killed is gone, so this normally returns immediately; the
	// budget only exists so that a leader which outlives a group-wide kill cannot
	// hold the run open any longer than cmd.Wait already would.
	//
	// This is guarded on there being an observer. Where exit observation is not
	// used, observeCh was never written to, and receiving from it would block on a
	// nil channel until the budget expired — adding the whole grace to every
	// shutdown on that platform.
	if observer != nil && observation == exitPending {
		stopObserver()
		// Only the fact that the observer finished matters here: teardown has
		// already run, and every later step reads the process and the group
		// rather than this result.
		select {
		case <-observeCh:
			<-observeDone
		case <-handoff:
			return finishHandoff()
		case <-time.After(grace + killSettle):
			// The leader outlived a group-wide SIGKILL, which means it is
			// unkillable rather than still ours to wait for. cmd.Wait below
			// blocks on the same process, so nothing is gained by waiting.
		}
	}

	// A reader still blocked once the group has drained is holding the pipe from
	// outside the group: a descendant that called setsid, or otherwise left.
	// Release our end so the run can finish, and record that its termination
	// could not be verified.
	//
	// The settle window matters. Killing the leader closes the last writer, but
	// the reader is on another goroutine and may not have been scheduled to
	// observe the resulting EOF yet, so checking immediately would report an
	// escape for a run that terminated cleanly.
	select {
	case <-scanDone:
	default:
		select {
		case <-scanDone:
		case <-handoff:
			return finishHandoff()
		case <-time.After(killSettle):
			escaped.Store(true)
			closeStdout()
			<-scanDone
		}
	}

	// Withdraw, then reap. This ordering is the whole ownership protocol:
	//
	//   Withdraw() and every signal take the same lock, so once Withdraw returns
	//   no signal can still be in flight — from the graceful escalation, from a
	//   forced shutdown, or from the stack sweep. Until Withdraw is called the
	//   leader is unreaped, and an unreaped leader holds its process-table slot,
	//   so every signal sent before this point provably reached only this
	//   command's own descendants.
	//
	//   cmd.Wait() is what reaps, and reaping releases that reservation. It is
	//   therefore the last step that touches the command.
	pg.Withdraw()
	// Receive the exit status here, on every path. recv returns the same
	// published result to every caller, and it must not be skipped because the
	// result is already published: on the reap-free observer path nothing has
	// received it yet, and an exited leader is often collected before this
	// point.
	//
	// When no reap is in flight — the reap-free observer path — this is the only
	// reap, so it happens here, last.
	if !reap.started() {
		r.startReap(p.name, groupIdentity, cmd, &reap)
	}
	if handoff != nil {
		select {
		case <-reap.done:
			err = reap.recv()
		case <-handoff:
			return errCleanupHandoff
		}
	} else {
		err = reap.recv()
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		code = -1
	}
	st.mu.Lock()
	st.exitCode = &code
	st.mu.Unlock()

	// Verify now that every signal has been sent and the leader is collected.
	r.lifecycleMu.Lock()
	// The terminal sweep took the registry, so this group is its to report.
	// A teardown group was registered after that sweep and is verified here.
	if handoff != nil && r.sealing.Load() && !p.teardown {
		r.lifecycleMu.Unlock()
		return errCleanupHandoff
	}
	if state := pg.State(); state != GroupEmpty {
		// Something is still running. SIGKILL cannot be caught, so a surviving
		// member is either wedged in uninterruptible sleep or escaped the group
		// entirely; either way it is not ours to signal any more. Report it.
		r.recordSurvivor(p.name, groupIdentity, survivorsError(groupResult(p.name, pg, state)))
		// Keep the group registered so the stack-level sweep can still name
		// it; Close would discard the only handle the operator could use. Its
		// record entry stays for the same reason.
		r.lifecycleMu.Unlock()
		return err
	}
	if escaped.Load() {
		// Distinct from a group survivor: the evidence we have only ever
		// covered the original group, and something outlived it.
		r.recordUnverified(p.name, groupIdentity, fmt.Sprintf(
			"%s: a descendant still held the command's output after the process group was "+
				"force-killed, so it had left the group and its termination could not be "+
				"verified; check for detached children of %s", p.name, p.name))
	}

	r.untrackGroup(p.name, pg)
	r.recordDropGroup(pg)
	pg.Close()
	r.lifecycleMu.Unlock()
	return err
}

// groupResult describes a group for an operator-facing report.
func groupResult(name string, pg ProcessGroup, state GroupState) []GroupResult {
	return []GroupResult{{
		Name:     name,
		PGID:     groupPGID(pg),
		Identity: pg.Identify(),
		State:    state,
	}}
}

func sealedGroupResult(name string, observation SealObservation) []GroupResult {
	return []GroupResult{{Name: name, Identity: observation.Identity, State: observation.State}}
}

// ── process-group registry ───────────────────────────────────────────────────
//
// A managed command's process group is the handle proc-compose uses to prove
// it stopped everything it started. These helpers keep every live incarnation
// reachable from the shutdown path and are the single place where "which
// processes are still running" is answered.

// trackGroup registers a run iteration's process group under its process name.
//
// A restart installs a new incarnation, and the previous one must not simply be
// forgotten: its descendants can still be running in the original group, so
// dropping the handle is exactly how a restart used to leak them. The previous
// incarnation is terminated first, while it is still owned.
//
// Behaviour change worth stating plainly: a command that backgrounds work and
// then exits no longer leaves that work running across a restart or a shutdown.
// It is now killed and verified, like any other managed process. That pattern
// was never actually supported — without a stdout redirect the runner's log
// scanner blocks on the inherited pipe until this same teardown runs.
func (r *Runner) trackGroup(name string, pg ProcessGroup, _ time.Duration) {
	r.groupsMu.Lock()
	if r.groups == nil {
		r.groups = make(map[string]ProcessGroup)
	}
	prev := r.groups[name]
	r.groups[name] = pg
	r.groupsMu.Unlock()

	if prev == nil || prev == pg {
		return
	}
	// The previous incarnation tore its group down and reaped it before
	// returning, so its identifier is no longer ours to signal. Whatever it left
	// behind was already reported then; report it again if it is still there, so a
	// restart can never silently forget a live process from an older group.
	if state := prev.State(); state != GroupEmpty {
		r.recordSurvivor(name, prev.Identify(), fmt.Errorf("previous incarnation: %w",
			survivorsError(groupResult(name, prev, state))))
	} else {
		r.recordDropGroup(prev)
	}
	prev.Close()
}

// untrackGroup forgets a process's group. The identity check keeps a late
// return from an older run iteration from dropping the group that a restart
// has already installed in its place.
func (r *Runner) untrackGroup(name string, pg ProcessGroup) {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	if r.groups[name] == pg {
		delete(r.groups, name)
	}
}

// snapshotGroups returns the live groups without unregistering them, so a
// caller can signal a group and still leave verification to the sweep.
func (r *Runner) snapshotGroups() []ProcessGroup {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	out := make([]ProcessGroup, 0, len(r.groups))
	for _, pg := range r.groups {
		out = append(out, pg)
	}
	return out
}

// takeGroups removes and returns every tracked group. Removing first means a
// group that arrives later (a restart racing the sweep) is swept by the next
// call rather than silently skipped.
func (r *Runner) takeGroups() map[string]ProcessGroup {
	r.groupsMu.Lock()
	defer r.groupsMu.Unlock()
	groups := r.groups
	r.groups = nil
	return groups
}

// ── process-group record ─────────────────────────────────────────────────────
//
// The record is the on-disk counterpart of the registry above, written to
// RecordPath (the daemon's PID file) so that groups outlive a crashed daemon
// in a form a later stop can check. Every change rewrites the whole list while
// recordMu is held, so writes land in mutation order. A failed write leaves the
// previous record, which still covers every group that may be running: entries
// are added before a launch and removed only after their group is proven empty.

// recEntry is one launched process group. pg identifies it in memory; on disk
// it is identified by name and pgid.
type recEntry struct {
	pg    ProcessGroup
	group daemon.Group
}

// beginRecord starts the run's record with an empty list. If that cannot be
// written, recording is disabled for the run with one warning: the PID file
// then carries no record, which a later stop reports as unverifiable.
func (r *Runner) beginRecord() {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	r.records = nil
	r.recordOn = r.RecordPath != "" && runtime.GOOS != "windows"
	if !r.recordOn {
		return
	}
	if err := r.writeRecordLocked(); err != nil {
		r.recordOn = false
		r.systemEvent(fmt.Sprintf("warning: process groups will not be recorded for crash recovery: %v", err), true)
	}
}

// recordPending adds a launch that has not started yet. An error means the
// entry could not be persisted and the launch must not proceed. With recording
// disabled it returns a nil entry, which every other record helper ignores.
//
// A PID file that has disappeared mid-run (a removed runtime or temp
// directory), or that another daemon has since recreated as its own, disables
// recording with one warning, as beginRecord does, instead of refusing: no
// record of this run is left on disk to keep complete, and refusing would stop
// every later restart for the rest of the run.
func (r *Runner) recordPending(name string, pg ProcessGroup) (*recEntry, error) {
	entry, lost, err := r.recordPendingLocked(name, pg)
	if lost != nil {
		r.systemEvent(fmt.Sprintf("warning: process groups will no longer be recorded for crash recovery: %v", lost), true)
	}
	return entry, err
}

func (r *Runner) recordPendingLocked(name string, pg ProcessGroup) (entry *recEntry, lost, err error) {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if !r.recordOn {
		return nil, nil, nil
	}
	entry = &recEntry{pg: pg, group: daemon.Group{Name: name}}
	r.records = append(r.records, entry)
	if err := r.writeRecordLocked(); err != nil {
		r.records = r.records[:len(r.records)-1]
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, daemon.ErrNotOwner) {
			r.recordOn = false
			clear(r.records)
			r.records = nil
			return nil, err, nil
		}
		return nil, nil, err
	}
	return entry, nil, nil
}

// recordTracked fills in a started launch's leader. pid must be the command's
// own pid, not the group's identifier, which a wrapping group may not expose.
// On a failed write the entry stays pending on disk, which reads as unknown.
func (r *Runner) recordTracked(entry *recEntry, pid int) {
	if entry == nil {
		return
	}
	var start uint64
	if info, err := daemon.ProcStat(pid); err == nil {
		start = info.StartTime
	}
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if !r.recordOn {
		return
	}
	entry.group.PGID = pid
	entry.group.LeaderStart = start
	_ = r.writeRecordLocked()
}

// recordDrop removes one entry. The caller must have proven its group empty or
// never started.
func (r *Runner) recordDrop(entry *recEntry) {
	if entry == nil {
		return
	}
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	r.removeRecordsLocked(func(e *recEntry) bool { return e == entry })
}

// recordDropGroup removes the entry for a group the caller has proven empty.
func (r *Runner) recordDropGroup(pg ProcessGroup) {
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	r.removeRecordsLocked(func(e *recEntry) bool { return e.pg == pg })
}

func (r *Runner) removeRecordsLocked(match func(*recEntry) bool) {
	if !r.recordOn {
		return
	}
	kept := r.records[:0]
	for _, e := range r.records {
		if !match(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(r.records) {
		return
	}
	clear(r.records[len(kept):])
	r.records = kept
	// A failed write leaves the removed entry on disk, a superset of the truth.
	_ = r.writeRecordLocked()
}

func (r *Runner) writeRecordLocked() error {
	groups := make([]daemon.Group, 0, len(r.records))
	for _, e := range r.records {
		groups = append(groups, e.group)
	}
	// The runner lives in the daemon, which wrote the PID file under its own
	// pid; a file naming anyone else is not this run's record.
	return daemon.WriteGroups(r.RecordPath, os.Getpid(), groups)
}

// recordSurvivor notes a managed process that could not be terminated, so the
// stop reports it instead of reporting success.
func (r *Runner) recordSurvivor(name, identity string, err error) {
	r.recordUnresolved(name, identity)
	r.recordShutdownProblem(fmt.Sprintf("could not terminate %s: %v", name, err))
}

// recordUnverified notes a managed process whose termination could not be
// established at all — for example a descendant that left the process group.
// It is deliberately a separate concept from a group member that survived
// SIGKILL: the difference is whether proc-compose had any means to reach it.
func (r *Runner) recordUnverified(name, identity, msg string) {
	r.recordUnresolved(name, identity)
	r.recordShutdownProblem(msg)
}

// UnresolvedProcess names a managed process whose termination or reaping could
// not be established. Identity is the platform identifier captured while it was
// still owned, or "unavailable".
type UnresolvedProcess struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
}

func (r *Runner) recordUnresolved(name, identity string) {
	if identity == "" || identity == "unknown" {
		identity = "unavailable"
	}
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	for _, existing := range r.unresolved {
		if existing.Name == name && existing.Identity == identity {
			return
		}
	}
	r.unresolved = append(r.unresolved, UnresolvedProcess{Name: name, Identity: identity})
}

// Unresolved returns every process recorded as surviving or unverified by the
// most recent run, in recording order.
func (r *Runner) Unresolved() []UnresolvedProcess {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	return append([]UnresolvedProcess(nil), r.unresolved...)
}

// recordShutdownProblem appends to the shutdown verdict and surfaces it on the
// console and to monitors. Every problem is kept, so a caller sees all
// survivors rather than only the first.
func (r *Runner) recordShutdownProblem(msg string) {
	r.systemEvent(msg, true)

	r.stopMu.Lock()
	r.stopProblems = append(r.stopProblems, msg)
	r.stopMu.Unlock()
}

// sweepGroups terminates every still-tracked process group and reports the
// ones that survived. It is the enforcement point for the invariant that a
// stopped runner leaves no managed process behind.
//
// It is a no-op outside shutdown: a stack that ends because every process
// finished is not a stop, and must not start killing processes the runner was
// never asked to stop.
func (r *Runner) sweepGroups(runCtx context.Context) {
	if runCtx.Err() == nil {
		return
	}
	r.sealGroups()
}

// sealGroups closes admission, takes every tracked group, seals it, and
// reports the ones that are not empty. sweepGroups runs it at shutdown; the
// teardown phase runs it again for the groups its processes registered.
func (r *Runner) sealGroups() {
	// Close admission before atomically taking the registry. Launchers hold the
	// same lock through Start, Track, and registration.
	r.lifecycleMu.Lock()
	r.stopping.Store(true)
	r.sealing.Store(true)
	groups := r.takeGroups()
	r.lifecycleMu.Unlock()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		pg := groups[name]
		// Seal remaining authority before Run returns. On Windows, this preserves
		// the final membership observation before kill-on-close invalidates it.
		observation, sealErr := pg.Seal()
		if observation.State == GroupEmpty && sealErr == nil {
			r.recordDropGroup(pg)
		}
		if observation.State != GroupEmpty {
			r.recordSurvivor(name, observation.Identity, survivorsError(sealedGroupResult(name, observation)))
		}
		if sealErr != nil {
			r.recordUnverified(name, observation.Identity, fmt.Sprintf("%s: process-group seal failed: %v", name, sealErr))
		}
	}
}

func (r *Runner) createProcessGroup() ProcessGroup {
	if r.processGroupFactory != nil {
		return r.processGroupFactory()
	}
	return newProcessGroup()
}

func (r *Runner) startReap(name, identity string, cmd *exec.Cmd, reap *singleReap) {
	if reap.started() {
		return
	}
	wait := cmd.Wait
	if waitProcess := r.waitProcess; waitProcess != nil {
		wait = func() error { return waitProcess(cmd) }
	}
	tracked := &trackedReap{process: name, identity: identity, reap: reap}
	r.reapersMu.Lock()
	// Prune before appending: launchers that never stop, such as a
	// ready_when.cmd probe retried every second under ready_timeout -1, would
	// otherwise keep every completed attempt's exec.Cmd alive until Run
	// returns. Unresolved waits stay for reportPendingReaps. The reap starts
	// under the lock because pruning reads every registered reap's state from
	// other launchers' goroutines.
	r.pruneTakenReapsLocked()
	reap.start(wait)
	r.reapers = append(r.reapers, tracked)
	r.reapersMu.Unlock()
	if r.reapStarted != nil {
		r.reapStarted(reap)
	}
}

// reportPendingReaps records every wait that remained unresolved at the
// operation boundary. Reaper goroutines publish only to their own singleReap;
// they never update this registry or Runner state after Run returns.
func (r *Runner) reportPendingReaps() {
	r.reapersMu.Lock()
	r.pruneTakenReapsLocked()
	pending := slices.Clone(r.reapers)
	r.reapersMu.Unlock()
	for _, tracked := range pending {
		r.recordUnverified(tracked.process, tracked.identity, fmt.Sprintf("%s: process leader %s was not reaped before cleanup handoff", tracked.process, tracked.identity))
	}
}

// pruneTakenReapsLocked drops every registered wait whose result has been
// published, keeping only the unresolved ones. Callers hold reapersMu.
func (r *Runner) pruneTakenReapsLocked() {
	keep := r.reapers[:0]
	for _, tracked := range r.reapers {
		if !tracked.reap.taken() {
			keep = append(keep, tracked)
		}
	}
	clear(r.reapers[len(keep):])
	r.reapers = keep
}

func waitForCleanupHandoff(delay time.Duration, handoff <-chan struct{}) bool {
	if handoff == nil {
		time.Sleep(delay)
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-handoff:
		return true
	case <-timer.C:
		return false
	}
}

// shutdownProblems returns the accumulated verdict problems.
func (r *Runner) shutdownProblems() []string {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	return append([]string(nil), r.stopProblems...)
}

// ── shutdown request ─────────────────────────────────────────────────────────

// Shutdown asks the runner to stop the whole stack. With force set, managed
// process groups are killed outright instead of being asked to exit first.
//
// It returns a channel closed once Run has finished tearing the stack down and
// verifying what it could, plus a channel carrying that verdict. A caller must
// therefore never infer termination from the daemon merely being gone: the
// verdict is the proof, and its absence is reported as "not verified" rather
// than as success.
func (r *Runner) Shutdown(force bool) (<-chan struct{}, <-chan error, error) {
	r.stopMu.Lock()
	cancel, stopped, verdict := r.stopCancel, r.stopped, r.stopVerdict
	r.stopMu.Unlock()

	if cancel == nil || stopped == nil || verdict == nil {
		return nil, nil, fmt.Errorf("no stack is running")
	}
	// Close launch and replacement admission before any force signal or context
	// cancellation. The same lock spans every Start-to-registry transition.
	r.closeAdmission()
	if force {
		// Kill every live group now so nothing has to wait out its own
		// shutdown_timeout. The groups stay registered: the sweep still has to
		// verify that the kill worked, and an unverified kill is exactly the
		// failure mode this change exists to remove.
		for _, pg := range r.snapshotGroups() {
			_ = pg.Kill()
		}
	}
	cancel()
	return stopped, verdict, nil
}

// ShutdownResult reports the verified outcome of a completed shutdown: nil only
// when every managed process group was confirmed empty and no descendant escaped
// verification, otherwise an error naming everything that could not be stopped.
func (r *Runner) ShutdownResult() error {
	problems := r.shutdownProblems()
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "\n"))
}

// handleProbeResult finalises a HTTP/TCP/cmd probe outcome for incarnation gen.
// On success it marks the process ready; on a non-cancellation failure (e.g.
// ready_timeout) it records the failure and cancels the per-iteration context
// so the running command is terminated. Both are decided under st.mu against
// procCtx, so a result that arrives after its incarnation ended is dropped.
// A non-empty detail (the cmd probe's last attempt, or an http or tcp
// answer foreign_listener: reject refused) is appended to the failure
// message. owner is who answered a passing http or tcp probe; it is recorded
// with the readiness and reported when foreign.
func (r *Runner) handleProbeResult(p procInfo, st *procState, gen uint64, kind string, ok bool, detail string, owner *ipc.EndpointOwner, procCtx context.Context, procCancel context.CancelFunc) {
	if ok {
		if st.markReadyFor(procCtx, gen, true, owner) {
			r.broadcastState(p, st)
			r.processLog(p, "info", "process.ready", "ready ("+kind+" probe)")
			if owner != nil && owner.Result == ipc.OwnerForeign {
				r.processLog(p, "warn", "readiness.foreign_listener", foreignListenerMessage(p.name, "readiness probe ("+kind+")", owner, false))
			}
		}
		return
	}
	msg := "readiness probe (" + kind + ") timed out"
	if detail != "" {
		msg += " (last attempt: " + detail + ")"
	}
	if !st.failReadinessFor(procCtx, gen, msg) {
		return // shutdown, restart, or a later incarnation already took over
	}
	r.processLog(p, "error", "readiness.failed", msg)
	procCancel()
}

// resolveReadyTimeout maps the ReadyTimeout config value to a duration.
// 0 means "use default (60s)"; a negative value disables the limit.
func resolveReadyTimeout(seconds int) time.Duration {
	switch {
	case seconds < 0:
		return 0
	case seconds == 0:
		return 60 * time.Second
	default:
		return time.Duration(seconds) * time.Second
	}
}

// giveUp transitions the process to failed state and returns true.
// Extracted from runProcess to avoid duplication.
func (r *Runner) giveUp(p procInfo, maxName int, st *procState) bool {
	msg := fmt.Sprintf("max restarts (%d) reached, giving up", p.proc.MaxRestarts)
	r.processLog(p, "error", "process.restart_limit", msg)
	st.mu.Lock()
	st.state = "failed"
	st.lastError = msg
	st.mu.Unlock()
	r.broadcastState(p, st)
	return true
}
