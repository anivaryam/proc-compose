package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

type processRunResult struct {
	failed   bool
	failFast bool
}

// runProcess runs the lifecycle loop for a single process.
// Returns failure metadata when the process ended in the "failed" state.
func (r *Runner) runProcess(ctx context.Context, p procInfo, maxName int, store *stateStore) processRunResult {
	st := store.get(p.name)
	backoff := time.Second

	// Wait for all dependencies to become ready before starting.
	for _, dep := range p.proc.DependsOn {
		depSt := store.get(dep)
		if depSt == nil {
			continue // validated at config load; defensive guard
		}
		PrintProcessEvent(p.name, maxName, p.colorIndex, fmt.Sprintf("waiting for %s...", dep))
		r.logEvent(p, fmt.Sprintf("waiting for %s...", dep))
		select {
		case <-ctx.Done():
			depSt.mu.Lock()
			depFailed := depSt.readyClosed && !depSt.readyOK
			depSt.mu.Unlock()
			if depFailed {
				msg := fmt.Sprintf("dependency %q failed", dep)
				PrintProcessError(p.name, maxName, p.colorIndex, msg)
				r.logEvent(p, msg)
				st.mu.Lock()
				st.state = "failed"
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
				PrintProcessError(p.name, maxName, p.colorIndex, msg)
				r.logEvent(p, msg)
				st.mu.Lock()
				st.state = "failed"
				st.mu.Unlock()
				r.broadcastState(p, st)
				st.markReady(false)
				return processRunResult{failed: true}
			}
		}
	}

	if p.proc.EffectiveMode() == config.ProcessModeTask {
		return r.runTaskProcess(ctx, p, maxName, st)
	}

	// Pre-compile log probe regex once (validated at config load).
	var logRe *regexp.Regexp
	if p.proc.ReadyWhen != nil && p.proc.ReadyWhen.Log != "" {
		logRe = regexp.MustCompile(p.proc.ReadyWhen.Log)
	}
	readyTimeout := resolveReadyTimeout(p.proc.ReadyTimeout)

	for {
		st.mu.Lock()
		st.startedAt = time.Now()
		st.state = "running"
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

		onStart, onLine := r.buildReadinessCallbacks(p, maxName, st, logRe, readyTimeout, procCtx, procCancel, func(msg string) {
			PrintProcessDebug(p.name, maxName, p.colorIndex, msg)
			r.logEvent(p, msg)
		})

		startTime := time.Now()
		err := r.exec(procCtx, p, maxName, st, onStart, onLine)
		procCancel() // clean up the restartCh goroutine

		// Reset backoff if the process ran successfully for a meaningful duration.
		if time.Since(startTime) > backoff {
			backoff = time.Second
		}
		// Ensure readyCh is closed so depends_on waiters never deadlock.
		// No-op if markReady was already called by a successful probe.
		st.markReady(false)

		// Check if parent context was cancelled (SIGTERM/SIGINT shutdown).
		select {
		case <-ctx.Done():
			st.mu.Lock()
			st.state = "exited"
			st.mu.Unlock()
			r.broadcastState(p, st)
			PrintProcessEvent(p.name, maxName, p.colorIndex, "stopped")
			return processRunResult{}
		default:
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
			if updated, ok := r.getProc(p.name); ok {
				p.proc = updated
			}
			st.resetReady()
			PrintProcessEvent(p.name, maxName, p.colorIndex, "restarting (requested)...")
			r.logEvent(p, "restarting (requested)...")
			st.mu.Lock()
			st.restarts++
			st.state = "restarting"
			st.mu.Unlock()
			r.broadcastState(p, st)
			backoff = time.Second
			continue
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
				PrintProcessEvent(p.name, maxName, p.colorIndex, "exited, restarting...")
				r.logEvent(p, "exited, restarting...")
				// A fresh cycle means a fresh readiness latch: dependents must
				// wait for the new incarnation's probe, not the dead one's.
				st.resetReady()
				st.mu.Lock()
				st.restarts++
				st.state = "restarting"
				st.mu.Unlock()
				r.broadcastState(p, st)
				time.Sleep(backoff)
				continue
			}
			PrintProcessEvent(p.name, maxName, p.colorIndex, "exited")
			r.logEvent(p, "exited")
			return processRunResult{}
		}

		PrintProcessError(p.name, maxName, p.colorIndex, fmt.Sprintf("exited: %v", err))
		r.logEvent(p, fmt.Sprintf("exited: %v", err))

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
		PrintProcessEvent(p.name, maxName, p.colorIndex, fmt.Sprintf("restarting in %s...", backoff))
		r.logEvent(p, fmt.Sprintf("restarting in %s...", backoff))

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

func (r *Runner) runTaskProcess(ctx context.Context, p procInfo, maxName int, st *procState) processRunResult {
	st.mu.Lock()
	st.startedAt = time.Now()
	st.state = "running"
	st.mu.Unlock()
	r.broadcastState(p, st)

	err := r.exec(ctx, p, maxName, st, nil, nil)

	st.mu.Lock()
	st.pid = 0
	st.mu.Unlock()

	if ctx.Err() != nil {
		st.mu.Lock()
		st.state = "exited"
		st.mu.Unlock()
		r.broadcastState(p, st)
		PrintProcessEvent(p.name, maxName, p.colorIndex, "stopped")
		return processRunResult{}
	}

	if err == nil {
		st.mu.Lock()
		st.state = "completed"
		st.mu.Unlock()
		st.markReady(true)
		r.broadcastState(p, st)
		PrintProcessEvent(p.name, maxName, p.colorIndex, "completed")
		r.logEvent(p, "completed")
		return processRunResult{}
	}

	msg := fmt.Sprintf("task failed: %v", err)
	PrintProcessError(p.name, maxName, p.colorIndex, msg)
	r.logEvent(p, msg)
	st.mu.Lock()
	st.state = "failed"
	st.mu.Unlock()
	st.markReady(false)
	r.broadcastState(p, st)
	return processRunResult{failed: true, failFast: true}
}

// buildReadinessCallbacks builds onStart and onLine hooks bound to the
// current run iteration's context. Each ready_when probe wires its own pair.
// progressFn is called periodically during probe to show waiting progress.
func (r *Runner) buildReadinessCallbacks(
	p procInfo,
	maxName int,
	st *procState,
	logRe *regexp.Regexp,
	readyTimeout time.Duration,
	procCtx context.Context,
	procCancel context.CancelFunc,
	progressFn func(string),
) (onStart func(), onLine func(string)) {
	switch {
	case p.proc.ReadyWhen == nil:
		onStart = func() {
			st.markReady(true)
			r.broadcastState(p, st)
			PrintProcessEvent(p.name, maxName, p.colorIndex, "ready")
			r.logEvent(p, "ready")
		}
	case p.proc.ReadyWhen.HTTP != "":
		probeURL := p.proc.ReadyWhen.HTTP
		onStart = func() {
			go func() {
				ok := probeHTTP(procCtx, probeURL, readyTimeout, func(msg string) {
					PrintProcessDebug(p.name, maxName, p.colorIndex, msg)
					r.logEvent(p, msg)
				}, func(msg string) {
					PrintProcessDebug(p.name, maxName, p.colorIndex, msg)
					r.logEvent(p, msg)
				})
				r.handleProbeResult(p, maxName, st, "http", ok, procCtx, procCancel)
			}()
		}
	case p.proc.ReadyWhen.TCP != "":
		probeAddr := p.proc.ReadyWhen.TCP
		onStart = func() {
			go func() {
				ok := probeTCP(procCtx, probeAddr, readyTimeout, func(msg string) {
					PrintProcessDebug(p.name, maxName, p.colorIndex, msg)
					r.logEvent(p, msg)
				}, func(msg string) {
					PrintProcessDebug(p.name, maxName, p.colorIndex, msg)
					r.logEvent(p, msg)
				})
				r.handleProbeResult(p, maxName, st, "tcp", ok, procCtx, procCancel)
			}()
		}
	case logRe != nil:
		if readyTimeout > 0 {
			go func() {
				select {
				case <-procCtx.Done():
				case <-time.After(readyTimeout):
					st.mu.Lock()
					alreadyReady := st.readyClosed && st.readyOK
					st.mu.Unlock()
					if !alreadyReady && procCtx.Err() == nil {
						msg := fmt.Sprintf("readiness probe (log) timed out after %s", readyTimeout)
						PrintProcessError(p.name, maxName, p.colorIndex, msg)
						r.logEvent(p, msg)
						procCancel()
					}
				}
			}()
		}
		onLine = func(line string) {
			if logRe.MatchString(line) {
				st.markReady(true)
				r.broadcastState(p, st)
				PrintProcessEvent(p.name, maxName, p.colorIndex, "ready (log match)")
				r.logEvent(p, "ready (log match)")
			}
		}
	}
	return onStart, onLine
}

// exec runs a single invocation of the process command.
// onStart is called once after cmd.Start() succeeds (may be nil).
// onLine is called for each stdout/stderr line (may be nil).
func (r *Runner) exec(ctx context.Context, p procInfo, maxName int, st *procState, onStart func(), onLine func(string)) error {
	shell, args := shellCommand(p.proc.Cmd)
	cmd := exec.CommandContext(ctx, shell, args...)

	// Per-process shutdown_timeout is the window between the graceful group
	// signal and the forced group kill. Setting it on the group (not just on
	// cmd.WaitDelay) is what makes a SIGTERM-ignoring command's descendants
	// get killed too.
	grace := shutdownGrace(p.proc.ShutdownTimeout)

	pg := newProcessGroup()
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
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	if err := pg.Track(cmd); err != nil {
		if cmd.Process != nil {
			if killErr := cmd.Process.Kill(); killErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to kill stray process: %v\n", killErr)
			}
		}
		pg.Close()
		return fmt.Errorf("track: %w", err)
	}

	// Register the group so shutdown can verify and escalate against it even
	// if this goroutine is parked below in the log scanner.
	r.trackGroup(p.name, pg, grace)

	st.mu.Lock()
	st.pid = cmd.Process.Pid
	st.mu.Unlock()

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
		_ = cmd.Wait()
		pg.Close()
		return fmt.Errorf("observe leader exit: %w", obsErr)
	}

	// Releases the log reader if something outside the group still holds the
	// pipe after the group has been killed.
	var escaped atomic.Bool
	closeStdout := sync.OnceFunc(func() { _ = stdout.Close() })

	if onStart != nil {
		onStart()
	}

	// The log reader runs on its own goroutine so that reading output is
	// independent of observing the exit. A service that closes or redirects its
	// own output reaches end-of-stream while still working, so end-of-stream is
	// an observation about the output and never a reason to terminate anything.
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		// Default 64 KiB buffer truncates long log lines (minified JSON, stack
		// traces) silently. 1 MiB matches the IPC client's bound and is plenty
		// for normal process output.
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			raw := scanner.Text()
			fmt.Println(FormatLine(p.name, maxName, p.colorIndex, raw))
			if r.LogFile != nil {
				fmt.Fprintf(r.LogFile, "%s | %s\n", p.name, raw)
			}
			if r.IPC != nil {
				r.IPC.BroadcastLog(ipc.LogEntry{Process: p.name, Line: raw})
				if p.name == "tunnel" {
					if url := extractTunnelURL(raw); url != "" {
						r.IPC.BroadcastTunnelURL(url)
					}
				}
			}
			if onLine != nil {
				onLine(raw)
			}
		}
	}()

	// The exit observer runs for the lifetime of the command. It never reaps:
	// WNOWAIT on Linux, and an EVFILT_PROC knote on macOS, both report the exit
	// while leaving the leader a zombie, and therefore still holding its
	// process-table slot. It is joined before the reap so that nothing can
	// collect the leader out from under the runner.
	observeCh := make(chan exitObservation, 1)
	obsStop := make(chan struct{})
	// reap reports the command's exit status exactly once, whenever cmd.Wait is
	// run on this goroutine's behalf.
	var reap singleReap
	if observer != nil {
		go func() { observeCh <- observer.Observe(obsStop) }()
	} else {
		// No reap-free observation: the exit can only be seen by reaping.
		//
		// Where that lands depends on the containment. A durable platform keeps
		// its authority across the reap and is torn down all the same; elsewhere
		// the reap is what releases the leader's reservation, so a natural
		// completion can no longer signal anything.
		reap.start(cmd.Wait)
		if !durableContainment {
			observation = exitUnsupported
		}
	}

	cancelled := false
	switch {
	case observer != nil:
		select {
		case <-ctx.Done():
			cancelled = true
		case observation = <-observeCh:
		}
	case durableContainment:
		select {
		case <-ctx.Done():
			cancelled = true
		case <-reap.done:
			// The leader is collected, but this platform's containment is a
			// durable kernel handle, so authority is unaffected and the group
			// is still torn down normally.
			err = reap.recv()
			observation = exitObserved
		}
	default:
		select {
		case <-ctx.Done():
			cancelled = true
		case <-reap.done:
			// The leader has been collected, so its identifier may already
			// have been released. Nothing below may signal the group.
			err = reap.recv()
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
		r.recordUnverified(fmt.Sprintf(
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
		close(obsStop)
		// Only the fact that the observer finished matters here: teardown has
		// already run, and every later step reads the process and the group
		// rather than this result.
		select {
		case <-observeCh:
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
	// Receive the exit status exactly once. It may already have been taken above
	// by a natural completion, in which case there is nothing left to receive and
	// a second read would block forever.
	//
	// When no reap is in flight — the reap-free observer path — this is the only
	// reap, so it happens here, last.
	if reap.started() {
		if !reap.taken() {
			err = reap.recv()
		}
	} else {
		err = cmd.Wait()
	}

	// Verify now that every signal has been sent and the leader is collected.
	if state := pg.State(); state != GroupEmpty {
		// Something is still running. SIGKILL cannot be caught, so a surviving
		// member is either wedged in uninterruptible sleep or escaped the group
		// entirely; either way it is not ours to signal any more. Report it.
		r.recordSurvivor(p.name, survivorsError(groupResult(p.name, pg, state)))
		// Keep the group registered so the stack-level sweep can still name
		// it; Close would discard the only handle the operator could use.
		return err
	}
	if escaped.Load() {
		// Distinct from a group survivor: the evidence we have only ever
		// covered the original group, and something outlived it.
		r.recordUnverified(fmt.Sprintf(
			"%s: a descendant still held the command's output after the process group was "+
				"force-killed, so it had left the group and its termination could not be "+
				"verified; check for detached children of %s", p.name, p.name))
	}

	r.untrackGroup(p.name, pg)
	pg.Close()
	return err
}

// groupResult describes a group for an operator-facing report.
func groupResult(name string, pg ProcessGroup, state GroupState) []GroupResult {
	return []GroupResult{{
		Name:  name,
		PGID:  groupPGID(pg),
		State: state,
	}}
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
		r.recordSurvivor(name, fmt.Errorf("previous incarnation: %w",
			survivorsError(groupResult(name, prev, state))))
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

// recordSurvivor notes a managed process that could not be terminated, so the
// stop reports it instead of reporting success.
func (r *Runner) recordSurvivor(name string, err error) {
	r.recordShutdownProblem(fmt.Sprintf("could not terminate %s: %v", name, err))
}

// recordUnverified notes a managed process whose termination could not be
// established at all — for example a descendant that left the process group.
// It is deliberately a separate concept from a group member that survived
// SIGKILL: the difference is whether proc-compose had any means to reach it.
func (r *Runner) recordUnverified(msg string) {
	r.recordShutdownProblem(msg)
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
	r.stopping.Store(true)

	groups := r.takeGroups()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		pg := groups[name]
		// Every group reaching here was torn down and reaped by its own exec, so
		// it can no longer be signalled: its identifier is not provably ours.
		// Judging it is still worthwhile — it is how a survivor that outlived the
		// stack-wide cancellation gets into the verdict at all.
		if state := pg.State(); state != GroupEmpty {
			r.recordSurvivor(name, survivorsError(groupResult(name, pg, state)))
		}
		pg.Close()
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
	if force {
		r.stopping.Store(true)
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

// handleProbeResult finalises a HTTP/TCP probe outcome. On success it marks
// the process ready; on a non-cancellation failure (e.g. ready_timeout) it
// cancels the per-iteration context so the running command is terminated.
func (r *Runner) handleProbeResult(p procInfo, maxName int, st *procState, kind string, ok bool, procCtx context.Context, procCancel context.CancelFunc) {
	if procCtx.Err() != nil {
		return // shutdown already in progress
	}
	if ok {
		st.markReady(true)
		r.broadcastState(p, st)
		PrintProcessEvent(p.name, maxName, p.colorIndex, "ready ("+kind+" probe)")
		r.logEvent(p, "ready ("+kind+" probe)")
		return
	}
	msg := "readiness probe (" + kind + ") timed out"
	PrintProcessError(p.name, maxName, p.colorIndex, msg)
	r.logEvent(p, msg)
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
	PrintProcessError(p.name, maxName, p.colorIndex, msg)
	r.logEvent(p, msg)
	st.mu.Lock()
	st.state = "failed"
	st.mu.Unlock()
	r.broadcastState(p, st)
	return true
}
