package runner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

// runProcess runs the lifecycle loop for a single process.
// Returns true if the process ended in the "failed" state (restart:never + non-zero exit).
func (r *Runner) runProcess(ctx context.Context, p procInfo, maxName int, store *stateStore) bool {
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
			return false
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
				return true
			}
		}
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
			return false
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
					return r.giveUp(p, maxName, st)
				}
				PrintProcessEvent(p.name, maxName, p.colorIndex, "exited, restarting...")
				r.logEvent(p, "exited, restarting...")
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
			return false
		}

		PrintProcessError(p.name, maxName, p.colorIndex, fmt.Sprintf("exited: %v", err))
		r.logEvent(p, fmt.Sprintf("exited: %v", err))

		if p.proc.Restart == "never" {
			st.mu.Lock()
			st.state = "failed"
			st.mu.Unlock()
			r.broadcastState(p, st)
			return true
		}

		st.mu.Lock()
		next := st.restarts + 1
		st.mu.Unlock()
		if p.proc.MaxRestarts > 0 && next > p.proc.MaxRestarts {
			return r.giveUp(p, maxName, st)
		}

		st.mu.Lock()
		st.restarts++
		st.state = "restarting"
		st.mu.Unlock()
		r.broadcastState(p, st)
		PrintProcessEvent(p.name, maxName, p.colorIndex, fmt.Sprintf("restarting in %s...", backoff))
		r.logEvent(p, fmt.Sprintf("restarting in %s...", backoff))

		select {
		case <-ctx.Done():
			return false
		case <-st.restartCh:
			// Restart requested during backoff — skip the wait.
			backoff = time.Second
			st.resetReady()
			continue
		case <-time.After(backoff):
		}

		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
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

	pg := newProcessGroup()
	pg.Setup(cmd)
	if p.proc.ShutdownTimeout > 0 {
		cmd.WaitDelay = time.Duration(p.proc.ShutdownTimeout) * time.Second
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

	st.mu.Lock()
	st.pid = cmd.Process.Pid
	st.mu.Unlock()

	if onStart != nil {
		onStart()
	}

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

	err = cmd.Wait()
	pg.Close()
	return err
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
